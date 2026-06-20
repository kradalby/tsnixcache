// Package nixcompress handles compression for the nix binary cache protocol.
//
// When a NAR is served with Compression: none, FileHash==NarHash and FileSize==NarSize.
// When compressed, FileHash/FileSize refer to the compressed bytes; NarHash/NarSize refer
// to the uncompressed NAR.
package nixcompress

import (
	"compress/bzip2"
	"fmt"
	"io"
	"os/exec"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// ExternalAvailable reports whether the named binary (e.g. "xz", "zstd") is in PATH.
func ExternalAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// Decoder returns a ReadCloser that decompresses r using the given compression type.
// compression is one of: "none", "xz", "zstd", "bzip2".
// If useExternal is true and the relevant binary is available in PATH, an external
// process is used; otherwise a pure-Go implementation is used.
func Decoder(r io.Reader, compression string, useExternal bool) (io.ReadCloser, error) {
	switch compression {
	case "none":
		return io.NopCloser(r), nil

	case "xz":
		if useExternal && ExternalAvailable("xz") {
			return newExternalDecoder(r, "xz", "-d", "--stdout")
		}
		xzr, err := xz.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("nixcompress: xz reader: %w", err)
		}
		return io.NopCloser(xzr), nil

	case "zstd":
		if useExternal && ExternalAvailable("zstd") {
			return newExternalDecoder(r, "zstd", "-d", "-c")
		}
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("nixcompress: zstd reader: %w", err)
		}
		return zr.IOReadCloser(), nil

	case "bzip2":
		// No external path for bzip2; stdlib compress/bzip2 is decode-only.
		return io.NopCloser(bzip2.NewReader(r)), nil

	default:
		return nil, fmt.Errorf("nixcompress: unknown compression %q", compression)
	}
}

// Encoder returns a WriteCloser that compresses writes to w.
// compression is one of: "none", "xz", "zstd", "bzip2".
// If useExternal is true and the relevant binary is available in PATH, an external
// process is used; otherwise a pure-Go implementation is used.
// Note: the nix binary cache server normally only serves "none" or "zstd"; xz and
// bzip2 encoder support is provided for round-trip testing and compatibility.
func Encoder(w io.Writer, compression string, useExternal bool) (io.WriteCloser, error) {
	switch compression {
	case "none":
		return nopWriteCloser{w}, nil

	case "xz":
		if useExternal && ExternalAvailable("xz") {
			return newExternalEncoder(w, "xz", "-c", "-")
		}
		xzw, err := xz.NewWriter(w)
		if err != nil {
			return nil, fmt.Errorf("nixcompress: xz writer: %w", err)
		}
		return xzw, nil

	case "zstd":
		if useExternal && ExternalAvailable("zstd") {
			return newExternalEncoder(w, "zstd", "-c", "-")
		}
		zw, err := zstd.NewWriter(w)
		if err != nil {
			return nil, fmt.Errorf("nixcompress: zstd writer: %w", err)
		}
		return zw, nil

	case "bzip2":
		// stdlib compress/bzip2 is decode-only; use external binary for encoding.
		// useExternal flag is ignored: always use the bzip2 binary.
		if ExternalAvailable("bzip2") {
			return newExternalEncoder(w, "bzip2", "-c")
		}
		return nil, fmt.Errorf("nixcompress: bzip2 encoder requires the bzip2 binary in PATH")

	default:
		return nil, fmt.Errorf("nixcompress: unknown compression %q", compression)
	}
}

// nopWriteCloser wraps an io.Writer with a no-op Close.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// externalDecoder runs an external command, feeding r to its stdin and
// exposing its stdout as a ReadCloser.
type externalDecoder struct {
	cmd *exec.Cmd
	pr  *io.PipeReader
	pw  *io.PipeWriter
	// done is closed once the copy goroutine finishes
	done chan struct{}
	err  error
}

func newExternalDecoder(r io.Reader, name string, args ...string) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	cmd := exec.Command(name, args...)
	cmd.Stdin = r
	cmd.Stdout = pw

	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("nixcompress: start %s: %w", name, err)
	}

	d := &externalDecoder{
		cmd:  cmd,
		pr:   pr,
		pw:   pw,
		done: make(chan struct{}),
	}

	// Wait for the command to finish then close the write-end of the pipe so
	// readers see EOF. Any error from the command is stored.
	go func() {
		defer close(d.done)
		waitErr := cmd.Wait()
		if waitErr != nil {
			pw.CloseWithError(fmt.Errorf("nixcompress: %s: %w", name, waitErr))
		} else {
			pw.Close()
		}
		d.err = waitErr
	}()

	return d, nil
}

func (d *externalDecoder) Read(p []byte) (int, error) { return d.pr.Read(p) }

func (d *externalDecoder) Close() error {
	// Signal the reader side we're done, then wait for the process.
	d.pr.Close()
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
	done  chan error
}

func newExternalEncoder(w io.Writer, name string, args ...string) (io.WriteCloser, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = w

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("nixcompress: stdin pipe for %s: %w", name, err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, fmt.Errorf("nixcompress: start %s: %w", name, err)
	}

	e := &externalEncoder{
		cmd:   cmd,
		stdin: stdin,
		done:  make(chan error, 1),
	}

	go func() {
		e.done <- cmd.Wait()
	}()

	return e, nil
}

func (e *externalEncoder) Write(p []byte) (int, error) { return e.stdin.Write(p) }

func (e *externalEncoder) Close() error {
	if err := e.stdin.Close(); err != nil {
		_ = e.cmd.Process.Kill()
		<-e.done
		return fmt.Errorf("nixcompress: close stdin: %w", err)
	}
	if err := <-e.done; err != nil {
		return fmt.Errorf("nixcompress: encoder process: %w", err)
	}
	return nil
}
