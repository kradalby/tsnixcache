// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package nar

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestCallerBufioWriter pins what Write may do with a *bufio.Writer it is
// handed: write the whole NAR through to the caller's real sink before
// returning, and leave the writer to the caller afterwards. A wrapper that
// buffers on top of the caller's buffer leaves the tail of the NAR behind; one
// that adopts the caller's writer — bufio.NewWriterSize returns w unchanged
// when w is a big enough *bufio.Writer — can go on to reset or recycle a writer
// somebody else still holds. The buffer sizes bracket writeBufSize so both
// paths are covered.
func TestCallerBufioWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hello.txt")

	err := os.WriteFile(path, []byte("hello NAR"), 0o600) // #nosec G306 -- test file
	if err != nil {
		t.Fatal(err)
	}

	var want bytes.Buffer

	err = Write(&want, path)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	for _, size := range []int{4 << 10, writeBufSize, 2 * writeBufSize} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			var sink bytes.Buffer

			caller := bufio.NewWriterSize(&sink, size)

			err := Write(caller, path)
			if err != nil {
				t.Fatalf("Write: %v", err)
			}

			if sink.String() != want.String() {
				t.Errorf("%d of the NAR's %d bytes reached the sink before the caller flushed; Write must flush a *bufio.Writer it was given", sink.Len(), want.Len())
			}

			// Anything the caller writes next must still reach its own sink:
			// Write must not have repointed or recycled the writer.
			sink.Reset()

			_, err = caller.WriteString("AFTER")
			if err != nil {
				t.Fatal(err)
			}

			err = caller.Flush()
			if err != nil {
				t.Fatal(err)
			}

			if sink.String() != "AFTER" {
				t.Errorf("the caller's later writes reached %q, want %q — Write took over its writer", sink.String(), "AFTER")
			}
		})
	}
}

// TestCallerBufioWriterExportStream is the same contract for the other exported
// entry point, which buffers the same way.
func TestCallerBufioWriterExportStream(t *testing.T) {
	var want bytes.Buffer

	err := writeExport(&want, []byte("nar bytes"), "/nix/store/abc-x", nil, "")
	if err != nil {
		t.Fatalf("writeExport: %v", err)
	}

	var sink bytes.Buffer

	caller := bufio.NewWriterSize(&sink, writeBufSize)

	_, err = WriteExportStream(caller, bytes.NewReader([]byte("nar bytes")), "/nix/store/abc-x", nil, "")
	if err != nil {
		t.Fatalf("WriteExportStream: %v", err)
	}

	if sink.String() != want.String() {
		t.Errorf("%d of the export stream's %d bytes reached the sink before the caller flushed", sink.Len(), want.Len())
	}

	sink.Reset()

	_, err = caller.WriteString("AFTER")
	if err != nil {
		t.Fatal(err)
	}

	err = caller.Flush()
	if err != nil {
		t.Fatal(err)
	}

	if sink.String() != "AFTER" {
		t.Errorf("the caller's later writes reached %q, want %q — WriteExportStream took over its writer", sink.String(), "AFTER")
	}
}
