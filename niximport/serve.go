// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package niximport

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/kradalby/tsnixcache/narinfo"
	"github.com/kradalby/tsnixcache/nixbase32"
)

const (
	serveMagicClient   = 0x390c9deb
	serveMagicServer   = 0x5452eecb
	serveVersion       = 0x205
	serveAddToStoreNar = 9
)

var (
	errServeProtocol = errors.New("niximport: unsupported nix-store serve protocol (requires 2.5 or newer)")
	errServeAck      = errors.New("niximport: nix-store did not acknowledge import")
	errServeHash     = errors.New("niximport: serve import requires a SHA-256 NAR hash")
)

// Legacy --import buffers the complete NAR in Nix; AddToStoreNar streams it.
func (imp *Importer) importStream(ctx context.Context, decoded io.Reader, ni *narinfo.NarInfo) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	args := []string{"--serve", "--write"}
	if imp.NixStoreURI != "" && imp.NixStoreURI != "auto" {
		args = append([]string{"--store", imp.NixStoreURI}, args...)
	}

	cmd := exec.CommandContext(runCtx, "nix-store", args...) // #nosec G204 -- operator-selected Nix store, trusted binary.
	cmd.WaitDelay = waitDelay

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("niximport: stdin pipe: %w", err)
	}
	defer stdin.Close()

	output, writer := io.Pipe()
	defer output.Close()
	defer writer.Close()

	stderr := &capWriter{max: stderrCap}
	cmd.Stdout = writer

	cmd.Stderr = stderr

	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("niximport: start nix-store: %w", err)
	}

	// Cancellation must unblock protocol reads even if a descendant holds stdout.
	stop := context.AfterFunc(runCtx, func() {
		_ = stdin.Close()
		_ = output.CloseWithError(runCtx.Err())
	})
	defer stop()

	var group errgroup.Group
	group.Go(func() error {
		waitErr := cmd.Wait()
		_ = writer.CloseWithError(waitErr)

		return waitErr
	})

	protocolErr := serveExchange(stdin, output, decoded, ni)
	closeErr := stdin.Close()
	// WaitDelay cannot release a copy already blocked in io.Pipe.Write.
	_ = output.CloseWithError(protocolErr)

	if protocolErr != nil {
		cancel()
	}

	waitErr := group.Wait()

	cause := ctx.Err()
	if cause != nil {
		return cause
	}

	if waitErr != nil {
		return errors.Join(protocolErr, fmt.Errorf("niximport: nix-store --serve: %w\n%s", waitErr, stderr.String()))
	}

	if protocolErr != nil {
		return fmt.Errorf("niximport: serve protocol: %w", protocolErr)
	}

	if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
		return fmt.Errorf("niximport: close stdin: %w", closeErr)
	}

	return nil
}

func serveExchange(to io.Writer, from, decoded io.Reader, ni *narinfo.NarInfo) error {
	hash, err := narinfo.CanonicalHash(ni.NarHash)
	if err != nil {
		return err
	}

	encoded, ok := strings.CutPrefix(hash, "sha256:")
	if !ok {
		return errServeHash
	}

	digest, err := nixbase32.DecodeString(encoded)
	if err != nil {
		return err
	}

	if ni.NarSize > math.MaxInt64 {
		return errNarTooLarge
	}

	err = serveHandshake(to, from)
	if err != nil {
		return err
	}

	header := binary.LittleEndian.AppendUint64(nil, serveAddToStoreNar)
	header = appendServeString(header, ni.StorePath)
	header = appendServeString(header, ni.Deriver)
	header = appendServeString(header, hex.EncodeToString(digest))
	refs := slices.Clone(ni.References)
	slices.Sort(refs)
	refs = slices.Compact(refs)

	header = binary.LittleEndian.AppendUint64(header, uint64(len(refs)))
	for _, ref := range refs {
		header = appendServeString(header, ref)
	}

	header = binary.LittleEndian.AppendUint64(header, 0) // registration time
	header = binary.LittleEndian.AppendUint64(header, ni.NarSize)
	// Preserve legacy import's trust metadata: no ultimate flag, signatures or CA.
	header = binary.LittleEndian.AppendUint64(header, 0)
	header = binary.LittleEndian.AppendUint64(header, 0)

	header = appendServeString(header, "")

	_, err = io.Copy(to, bytes.NewReader(header))
	if err != nil {
		return err
	}

	// CopyN discards decoder errors accompanying the final requested bytes.
	written, err := io.Copy(to, io.LimitReader(decoded, int64(ni.NarSize))) // #nosec G115 -- checked above.
	if err != nil {
		return err
	}

	if uint64(written) != ni.NarSize { // #nosec G115 -- io.Copy returns a nonnegative byte count.
		return io.ErrUnexpectedEOF
	}

	ack, err := readServeWord(from)
	if err != nil {
		return err
	}

	if ack != 1 {
		return errServeAck
	}

	return nil
}

func appendServeString(dst []byte, value string) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, uint64(len(value)))
	dst = append(dst, value...)

	var padding [8]byte

	return append(dst, padding[:(8-len(value)%8)%8]...)
}

func readServeWord(from io.Reader) (uint64, error) {
	var data [8]byte

	_, err := io.ReadFull(from, data[:])

	return binary.LittleEndian.Uint64(data[:]), err
}

func serveHandshake(to io.Writer, from io.Reader) error {
	hello := binary.LittleEndian.AppendUint64(nil, serveMagicClient)

	hello = binary.LittleEndian.AppendUint64(hello, serveVersion)

	_, err := io.Copy(to, bytes.NewReader(hello))
	if err != nil {
		return err
	}

	magic, err := readServeWord(from)
	if err != nil {
		return err
	}

	if magic != serveMagicServer {
		return errServeProtocol
	}

	version, err := readServeWord(from)
	if err != nil {
		return err
	}

	if version>>8 != 2 || version < serveVersion {
		return errServeProtocol
	}

	return nil
}
