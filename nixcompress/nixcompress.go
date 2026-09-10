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
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"golang.org/x/sync/errgroup"
)

// Sentinel errors for unknown compression types and missing external binaries.
var (
	errBzip2Required = errors.New("nixcompress: bzip2 encoder requires the bzip2 binary in PATH")
	errXzRequired    = errors.New("nixcompress: xz decoding requires the xz binary in PATH")
	errUnknownCodec  = errors.New("nixcompress: unknown compression")
)

// Decoder ceilings prevent pusher-controlled headers from determining memory use.
// Nix can emit streams above these limits; their producers must lower compression
// settings rather than expanding the server's decode budget.
const (
	xzDecodeMemLimit = "--memlimit-decompress=128MiB"
	zstdMaxWindow    = 8 << 20

	// zstdEncodeWindow is what we compress with, and must stay within
	// zstdMaxWindow so our own output survives a round trip.
	zstdEncodeWindow = 2 << 20
)

// zstdDecodeMemLimit gives both decoder paths the same declared-window ceiling.
// zstd's --memlimit accepts bytes; xz's --memlimit-decompress is a different flag.
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
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	dec, err := decoder(ctx, &contextReader{ctx: ctx, r: r}, compression, useExternal)
	if err != nil {
		return nil, err
	}

	return &contextReadCloser{ReadCloser: dec, ctx: ctx}, nil
}

type contextReader struct {
	ctx context.Context //nolint:containedctx // io.Reader binds cancellation for the stream's lifetime.
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	err := r.ctx.Err()
	if err != nil {
		return 0, err
	}

	// Bound work between cancellation checks even for callers with large buffers.
	n, err := r.r.Read(p[:min(len(p), 32<<10)])

	cause := r.ctx.Err()
	if cause != nil {
		return n, cause
	}

	return n, err
}

type contextReadCloser struct {
	io.ReadCloser

	ctx context.Context //nolint:containedctx // Keep cancellation active between decoded reads.
}

func (r *contextReadCloser) Read(p []byte) (int, error) {
	return (&contextReader{ctx: r.ctx, r: r.ReadCloser}).Read(p)
}

func (r *contextReadCloser) Unwrap() io.ReadCloser { return r.ReadCloser }

func decoder(ctx context.Context, r io.Reader, compression string, useExternal bool) (io.ReadCloser, error) {
	switch compression {
	case compressionNone:
		return io.NopCloser(r), nil

	case compressionXz:
		// xz(1) enforces a memory ceiling; the Go reader's DictCap is a floor.
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
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	enc, err := encoder(ctx, &contextWriter{ctx: ctx, w: w}, compression, useExternal)
	if err != nil {
		return nil, err
	}

	return &contextWriteCloser{Writer: &contextWriter{ctx: ctx, w: enc}, inner: enc}, nil
}

type contextWriteCloser struct {
	io.Writer

	inner io.WriteCloser
}

func (w *contextWriteCloser) Unwrap() io.WriteCloser { return w.inner }

func (w *contextWriteCloser) Close() error { return w.inner.Close() }

type contextWriter struct {
	ctx context.Context //nolint:containedctx // io.Writer binds cancellation for the stream lifetime.
	w   io.Writer
}

func (w *contextWriter) Write(p []byte) (int, error) {
	written := 0

	for len(p) > 0 {
		err := w.ctx.Err()
		if err != nil {
			return written, err
		}

		chunk := p[:min(len(p), 32<<10)]
		n, err := w.w.Write(chunk)

		written += n
		if err != nil {
			return written, err
		}

		if n != len(chunk) {
			return written, io.ErrShortWrite
		}

		p = p[n:]
	}

	return written, w.ctx.Err()
}

func encoder(ctx context.Context, w io.Writer, compression string, useExternal bool) (io.WriteCloser, error) {
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

		// Bound per-request memory and workers independently of the host's CPU count.
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
	done    chan struct{}
	err     error
	workers errgroup.Group
}

func newExternalDecoder(ctx context.Context, r io.Reader, name string, args ...string) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- caller-controlled
	cmd.WaitDelay = 10 * time.Second
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
	d.workers.Go(func() error {
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

		return nil
	})

	return d, nil
}

func (d *externalDecoder) Read(p []byte) (int, error) { return d.pr.Read(p) }

func (d *externalDecoder) Close() error {
	defer func() { _ = d.workers.Wait() }()
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
	workers errgroup.Group
}

func newExternalEncoder(ctx context.Context, w io.Writer, name string, args ...string) (io.WriteCloser, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- caller-controlled
	cmd.Stdout = w
	cmd.WaitDelay = 10 * time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("nixcompress: stdin pipe for %s: %w", name, err)
	}

	e := &externalEncoder{
		cmd:   cmd,
		stdin: stdin,
	}
	cmd.Stderr = &e.stderr

	err = cmd.Start()
	if err != nil {
		_ = stdin.Close()

		return nil, fmt.Errorf("nixcompress: start %s: %w", name, err)
	}

	e.workers.Go(func() error {
		waitErr := cmd.Wait()
		if waitErr != nil {
			waitErr = exitError(name, waitErr, e.stderr.String())
		}

		return waitErr
	})

	return e, nil
}

func (e *externalEncoder) Write(p []byte) (int, error) { return e.stdin.Write(p) }

func (e *externalEncoder) Close() error {
	err := e.stdin.Close()
	if err != nil {
		_ = e.cmd.Process.Kill()
		_ = e.workers.Wait()

		return fmt.Errorf("nixcompress: close stdin: %w", err)
	}

	return e.workers.Wait()
}
