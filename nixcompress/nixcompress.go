// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package nixcompress handles compression for the nix binary cache protocol.
//
// When a NAR is served with Compression: none, FileHash==NarHash and FileSize==NarSize.
// When compressed, FileHash/FileSize refer to the compressed bytes; NarHash/NarSize refer
// to the uncompressed NAR.
package nixcompress

import (
	"bytes"
	"compress/bzip2"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Sentinel errors for unknown compression types and missing external binaries.
var (
	errBzip2Required = errors.New("nixcompress: bzip2 encoder requires the bzip2 binary in PATH")
	errXzRequired    = errors.New("nixcompress: xz decoding requires the xz binary in PATH")
	errUnknownCodec  = errors.New("nixcompress: unknown compression")
)

// Ceilings for pusher-controlled decoder memory. A NAR arrives with its
// Compression field chosen by whoever pushed it, so both decoders would
// otherwise size their buffers from attacker-supplied header bytes: an xz block
// header can demand a 4 GiB LZMA2 dictionary and a zstd frame header a 512 MiB
// window, from inputs of 64 and 10 bytes respectively.
//
// 128 MiB covers the largest dictionary any xz preset produces (64 MiB, at -9)
// plus the decoder's own working set. 8 MiB covers the pushes anyone actually
// sends: nix compresses NARs at the libzstd default, which declares a 2 MiB
// window, and `nix copy --to ...?compression-level=19` lands exactly on 8 MiB.
//
// It is a cap, not an upper bound on what nix can emit. nix hands
// compression-level straight to libzstd, so — unlike zstd(1), which gates levels
// above 19 behind --ultra — compression-level=20 and up are reachable and
// declare 32 MiB or more. Those pushes are refused, and the operator has to
// lower the level: following them up would mean sizing a decode buffer from a
// header field the pusher chose, which is what this constant exists to stop.
const (
	xzDecodeMemLimit = "--memlimit-decompress=128MiB"
	zstdMaxWindow    = 8 << 20

	// zstdEncodeWindow is what we compress with, and must stay within
	// zstdMaxWindow so our own output survives a round trip.
	zstdEncodeWindow = 2 << 20
)

// zstdDecodeMemLimit bounds the external zstd decoder to zstdMaxWindow, so the
// ceiling does not move when an operator turns on --external-compression for
// throughput; zstd(1) otherwise defaults to 128 MiB, sixteen times what the
// in-process path allows.
//
// The flag is ZSTD_DCtx_setMaxWindowSize under the covers, so it compares
// against the frame's declared window exactly as WithDecoderMaxWindow does, and
// the two paths accept and refuse the same frames. Two spellings to avoid:
// --memlimit-decompress= is the xz name and zstd 1.5.7 fails to parse it, and a
// bare number is bytes rather than the megabytes the manual claims.
func zstdDecodeMemLimit() string {
	return fmt.Sprintf("--memlimit=%d", zstdMaxWindow)
}

// The compression names this package understands, spelled once.
const (
	compressionNone  = "none"
	compressionXz    = "xz"
	compressionZstd  = "zstd"
	compressionBzip2 = "bzip2"
)

func unknownCompression(compression string) error {
	return fmt.Errorf("%w %q", errUnknownCodec, compression)
}

// Supported reports whether compression names a codec this package handles.
// Nix also accepts gzip, br, lzma and lzip; a server that only discovers the
// codec is unsupported once the NAR has been spooled makes the client re-upload
// it on every retry, so callers should reject unsupported names before reading
// the body.
func Supported(compression string) bool {
	switch compression {
	case compressionNone, compressionXz, compressionZstd, compressionBzip2:
		return true
	default:
		return false
	}
}

// ExternalAvailable reports whether the named binary (e.g. "xz", "zstd") is in PATH.
func ExternalAvailable(name string) bool {
	_, err := exec.LookPath(name)

	return err == nil
}

// Requirement is one external binary this package runs, and what depends on it.
type Requirement struct {
	// Binary is the name looked up in PATH.
	Binary string
	// Codec is the Compression value the binary serves, so a caller can say
	// what breaks rather than only what is missing.
	Codec string
	// Decode and Encode say which directions run the binary. They are not the
	// same: xz decoding always shells out while xz encoding only does on
	// request, and bzip2 is the other way round.
	Decode bool
	Encode bool
	// Optional marks a binary whose absence degrades rather than fails, so a
	// caller can pick its wording — and its exit status — accordingly.
	Optional bool
	// Present reports whether Binary was found in PATH.
	Present bool
}

// Requirements reports the external binaries this package will run under the
// given useExternal setting, and whether each is in PATH.
//
// It exists so a server can refuse to start, or at least complain, rather than
// discovering a missing binary halfway through an import: xz is what `nix copy
// --to http://...` compresses with by default, so a host without it rejects the
// commonest push there is, and only after the client has uploaded the NAR.
func Requirements(useExternal bool) []Requirement {
	reqs := []Requirement{
		// Decoding xz has no in-process fallback at all — ulikunitz/xz cannot be
		// given a memory ceiling — so this one is required whatever useExternal
		// says. Encoding is the ordinary opt-in.
		{Binary: "xz", Codec: compressionXz, Decode: true, Encode: useExternal},
		// compress/bzip2 is decode-only, so encoding always shells out and
		// decoding never does.
		{Binary: "bzip2", Codec: compressionBzip2, Encode: true},
	}

	if useExternal {
		// Both directions fall back to the in-process implementation when the
		// binary is absent, so this is a request that went unhonoured rather
		// than a codec that stopped working.
		reqs = append(reqs, Requirement{
			Binary: "zstd", Codec: compressionZstd,
			Decode: true, Encode: true, Optional: true,
		})
	}

	for i := range reqs {
		reqs[i].Present = ExternalAvailable(reqs[i].Binary)
	}

	return reqs
}

// Decoder returns a ReadCloser that decompresses r using the given compression type.
// compression is one of: "none", "xz", "zstd", "bzip2".
// xz always decodes in the xz binary, because the pure-Go decoder cannot be given
// a memory ceiling (see xzDecodeMemLimit); "none" and bzip2 are always in-process.
// Only zstd honours useExternal, and only when the zstd binary is in PATH.
func Decoder(ctx context.Context, r io.Reader, compression string, useExternal bool) (io.ReadCloser, error) {
	switch compression {
	case compressionNone:
		return io.NopCloser(r), nil

	case compressionXz:
		// ulikunitz/xz reads the dictionary size from the block header and
		// treats ReaderConfig.DictCap as a floor, not a ceiling, so there is no
		// way to bound it from here. xz(1) takes an explicit limit, and on
		// spooled NARs it decodes an order of magnitude faster besides: the
		// pure-Go reader is handed one byte at a time by lzma.breader, which
		// costs a read(2) per compressed byte.
		if !ExternalAvailable("xz") {
			return nil, errXzRequired
		}

		return newExternalDecoder(ctx, r, "xz", "-d", "--stdout", xzDecodeMemLimit)

	case compressionZstd:
		if useExternal && ExternalAvailable("zstd") {
			return newExternalDecoder(ctx, r, "zstd", "-d", "-c", zstdDecodeMemLimit())
		}

		zr, err := zstd.NewReader(r,
			zstd.WithDecoderMaxWindow(zstdMaxWindow),
			zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, fmt.Errorf("nixcompress: zstd reader: %w", err)
		}

		return zr.IOReadCloser(), nil

	case compressionBzip2:
		// No external path for bzip2; stdlib compress/bzip2 is decode-only.
		return io.NopCloser(bzip2.NewReader(r)), nil

	default:
		return nil, unknownCompression(compression)
	}
}

// Encoder returns a WriteCloser that compresses writes to w.
// compression is one of: "none", "xz", "zstd", "bzip2".
// If useExternal is true and the relevant binary is available in PATH, an external
// process is used; otherwise a pure-Go implementation is used. bzip2 ignores
// useExternal: it always shells out, and fails if the binary is missing.
// Note: the nix binary cache server normally only serves "none" or "zstd"; xz and
// bzip2 encoder support is provided for round-trip testing and compatibility.
func Encoder(ctx context.Context, w io.Writer, compression string, useExternal bool) (io.WriteCloser, error) {
	switch compression {
	case compressionNone:
		return nopWriteCloser{w}, nil

	case compressionXz:
		if useExternal && ExternalAvailable("xz") {
			return newExternalEncoder(ctx, w, "xz", "-c", "-")
		}

		xzw, err := xz.NewWriter(w)
		if err != nil {
			return nil, fmt.Errorf("nixcompress: xz writer: %w", err)
		}

		return xzw, nil

	case compressionZstd:
		if useExternal && ExternalAvailable("zstd") {
			return newExternalEncoder(ctx, w, "zstd", "-c", "-")
		}

		// Encoders are created per request and never pooled, so the defaults
		// (GOMAXPROCS workers, 8 MiB window) cost ~18 MiB of live heap each on a
		// 16-core host. A 2 MiB window with two workers is both smaller (~5 MiB)
		// and faster, and gives up 0.1% of ratio.
		zw, err := zstd.NewWriter(w,
			zstd.WithWindowSize(zstdEncodeWindow),
			zstd.WithEncoderConcurrency(2))
		if err != nil {
			return nil, fmt.Errorf("nixcompress: zstd writer: %w", err)
		}

		return zw, nil

	case compressionBzip2:
		// stdlib compress/bzip2 is decode-only; use external binary for encoding.
		// useExternal flag is ignored: always use the bzip2 binary.
		if ExternalAvailable("bzip2") {
			return newExternalEncoder(ctx, w, "bzip2", "-c")
		}

		return nil, errBzip2Required

	default:
		return nil, unknownCompression(compression)
	}
}

// nopWriteCloser wraps an io.Writer with a no-op Close.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// exitError folds a child's stderr into its exit status. An exit status alone
// tells a pusher nothing, and "Frame requires too much memory for decoding" is
// exactly what they need.
func exitError(name string, err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return fmt.Errorf("nixcompress: %s: %w", name, err)
	}

	return fmt.Errorf("nixcompress: %s: %w: %s", name, err, stderr)
}

// externalDecoder runs an external command, feeding r to its stdin and
// exposing its stdout as a ReadCloser.
type externalDecoder struct {
	cmd *exec.Cmd
	pr  *io.PipeReader
	pw  *io.PipeWriter
	// stderr is why the command failed; an exit status alone tells a pusher
	// nothing, and "Memory usage limit reached" is exactly what they need.
	stderr bytes.Buffer
	// done is closed once the command has been waited for; err holds its status.
	done chan struct{}
	err  error
}

func newExternalDecoder(ctx context.Context, r io.Reader, name string, args ...string) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- caller-controlled
	cmd.Stdin = r
	cmd.Stdout = pw

	d := &externalDecoder{
		cmd:  cmd,
		pr:   pr,
		pw:   pw,
		done: make(chan struct{}),
	}
	cmd.Stderr = &d.stderr

	err := cmd.Start()
	if err != nil {
		_ = pr.Close()
		_ = pw.Close()

		return nil, fmt.Errorf("nixcompress: start %s: %w", name, err)
	}

	// Publish the exit status before closing the write-end, so a reader that has
	// seen EOF is guaranteed to find done already closed. That ordering is what
	// lets Close tell a finished decode from a caller abandoning the stream.
	go func() {
		waitErr := cmd.Wait()
		if waitErr != nil {
			waitErr = exitError(name, waitErr, d.stderr.String())
		}

		d.err = waitErr

		close(d.done)

		if waitErr != nil {
			pw.CloseWithError(waitErr)
		} else {
			_ = pw.Close()
		}
	}()

	return d, nil
}

func (d *externalDecoder) Read(p []byte) (int, error) { return d.pr.Read(p) }

func (d *externalDecoder) Close() error {
	// Signal the reader side we're done, then wait for the process.
	_ = d.pr.Close()

	// The command having already exited means the stream ran to its end, so its
	// status is the decode result and callers should see it. Still running means
	// this caller gave up part-way — the kill below is our doing, not a decode
	// failure, so there is nothing to report.
	select {
	case <-d.done:
		return d.err
	default:
	}

	if d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
	}

	<-d.done

	return nil
}

// externalEncoder runs an external command, exposing its stdin as a WriteCloser
// and directing its stdout to w.
type externalEncoder struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// stderr is why the command failed, captured for the same reason the
	// decoder captures it: "bzip2: I/O or other error" beats "exit status 1".
	stderr bytes.Buffer
	// done carries the exit status, already wrapped.
	done chan error
}

func newExternalEncoder(ctx context.Context, w io.Writer, name string, args ...string) (io.WriteCloser, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- caller-controlled
	cmd.Stdout = w

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("nixcompress: stdin pipe for %s: %w", name, err)
	}

	e := &externalEncoder{
		cmd:   cmd,
		stdin: stdin,
		done:  make(chan error, 1),
	}
	cmd.Stderr = &e.stderr

	err = cmd.Start()
	if err != nil {
		_ = stdin.Close()

		return nil, fmt.Errorf("nixcompress: start %s: %w", name, err)
	}

	go func() {
		waitErr := cmd.Wait()
		if waitErr != nil {
			waitErr = exitError(name, waitErr, e.stderr.String())
		}

		e.done <- waitErr
	}()

	return e, nil
}

func (e *externalEncoder) Write(p []byte) (int, error) { return e.stdin.Write(p) }

func (e *externalEncoder) Close() error {
	err := e.stdin.Close()
	if err != nil {
		_ = e.cmd.Process.Kill()
		<-e.done

		return fmt.Errorf("nixcompress: close stdin: %w", err)
	}

	return <-e.done
}
