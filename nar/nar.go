// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause
// Forked from tailscale.com/cmd/nardump/nardump with streaming and root-type extensions.

// Package nar serialises a filesystem tree into nix's NAR format and wraps a
// NAR in a nix-store export stream. Every consumer hashes these bytes, so the
// output is not merely a valid NAR but the same bytes nix would have written;
// the differential tests against `nix-store --dump` are what keep it so.
package nar

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
)

// caseHackSuffix is the marker nix appends to on-disk names that collide only by
// case in a case-insensitive store (e.g. macOS). nix strips everything from this
// marker on when serialising a NAR; we must do the same to match its byte stream.
const caseHackSuffix = "~nix~case~hack~"

// errNameCollision is returned when two directory entries strip to the same NAR
// name — a collision nix itself rejects.
var errNameCollision = errors.New("nar: case-hack name collision")

// errUnsupportedType mirrors nix's dumpPath, which refuses anything that is not
// a regular file, symlink or directory: the format cannot describe a device,
// socket or FIFO, and opening a FIFO with no writer blocks for ever.
var errUnsupportedType = errors.New("nar: file has an unsupported type")

// errMaxDepth is returned for a tree nested deeper than nix will unpack.
var errMaxDepth = errors.New("nar: exceeds maximum NAR directory depth")

// maxDirDepth is nix's limit on directory nesting, enforced when it dumps and
// again when it restores. Serialising past it would produce a NAR no nix can
// consume, so we stop where nix stops. Measured against nix 2.34.7: a root
// directory with 63 directories below it is the deepest one it accepts.
const maxDirDepth = 64

// darwinCaseHack mirrors nix's use-case-hack setting, which defaults on for the
// case-insensitive Apple filesystem and off elsewhere.
const darwinCaseHack = runtime.GOOS == "darwin"

// writeBufSize sizes the buffer in front of the NAR sink. NARs are whole store
// paths — routinely tens of megabytes — and every flush is a write to a zstd
// encoder, an HTTP response or nix-store's stdin, so bufio's 4 KiB default hands
// the sink the stream in 4096-byte pieces on the serve hot path.
const writeBufSize = 256 << 10

// writerPool recycles the buffers. At writeBufSize a fresh buffer per NAR costs
// more than serialising a small store path does, and a nix store is mostly small
// store paths. Get returns nil when the pool is empty, so there is no New.
var writerPool sync.Pool

// getWriter buffers w, unless the caller already handed us a *bufio.Writer, in
// which case we write straight to theirs. Do not wrap a caller's buffer in
// another one: the flush at the end of a write would only move the NAR from our
// buffer into theirs, leaving the tail short of their sink. The bool reports
// whether the writer is ours to recycle.
func getWriter(w io.Writer) (*bufio.Writer, bool) {
	bw, ok := w.(*bufio.Writer)
	if ok {
		return bw, false
	}

	bw, _ = writerPool.Get().(*bufio.Writer)
	if bw == nil {
		return bufio.NewWriterSize(w, writeBufSize), true
	}

	bw.Reset(w)

	return bw, true
}

// putWriter recycles a buffer nar itself created. A writer the caller passed in
// is left alone: resetting it would redirect the writes the caller makes after
// we return, and pooling it would hand a writer somebody else still holds to the
// next NAR. Reset also drops our reference to the sink.
func putWriter(bw *bufio.Writer, ours bool) {
	if !ours {
		return
	}

	bw.Reset(io.Discard)
	writerPool.Put(bw)
}

// Write writes a NAR archive of the filesystem rooted at root to w.
// Streaming: regular file contents are copied straight from the open file.
// If w is a *bufio.Writer, Write writes through it and flushes it before
// returning; the whole NAR has reached w's own sink by then.
func Write(w io.Writer, root string) error {
	bw, ours := getWriter(w)
	defer putWriter(bw, ours)

	err := writeString(bw, "nix-archive-1")
	if err != nil {
		return err
	}

	fi, err := os.Lstat(root)
	if err != nil {
		return err
	}

	err = writeNode(bw, root, fi, darwinCaseHack, 1)
	if err != nil {
		return err
	}

	return bw.Flush()
}

// writeNode writes the node for path, whose lstat is fi. depth is the node's
// directory nesting level, counting the NAR root as 1.
func writeNode(w *bufio.Writer, path string, fi fs.FileInfo, caseHack bool, depth int) error {
	switch {
	case fi.IsDir():
		return writeDir(w, path, caseHack, depth)
	case fi.Mode()&fs.ModeSymlink != 0:
		return writeSymlink(w, path)
	case fi.Mode().IsRegular():
		return writeRegular(w, path, fi)
	default:
		return fmt.Errorf("%w: %s (mode %s)", errUnsupportedType, path, fi.Mode().Type())
	}
}

// WriteExportStream writes a nix-store export stream for a single path, copying
// the NAR from r instead of buffering it. Returns the number of NAR bytes copied.
// As with Write, a *bufio.Writer is written through and flushed, not wrapped.
func WriteExportStream(w io.Writer, r io.Reader, storePath string, refs []string, deriver string) (int64, error) {
	bw, ours := getWriter(w)
	defer putWriter(bw, ours)

	err := writeUint64(bw, 1)
	if err != nil {
		return 0, err
	}

	n, err := io.Copy(bw, r)
	if err != nil {
		return n, err
	}

	_, err = bw.WriteString("NIXE\x00\x00\x00\x00")
	if err != nil {
		return n, err
	}

	err = writeString(bw, storePath)
	if err != nil {
		return n, err
	}

	err = writeUint64(bw, uint64(len(refs)))
	if err != nil {
		return n, err
	}

	// nix writes info->references, a StorePathSet ordered by base name, so an
	// export stream is only byte-identical to nix's when the references are
	// sorted the same way. Sort a copy — the caller's slice is its own.
	sorted := slices.Clone(refs)
	slices.SortFunc(sorted, func(a, b string) int {
		return strings.Compare(path.Base(a), path.Base(b))
	})

	for _, ref := range sorted {
		err = writeString(bw, ref)
		if err != nil {
			return n, err
		}
	}

	err = writeString(bw, deriver)
	if err != nil {
		return n, err
	}

	err = writeUint64(bw, 0)
	if err != nil {
		return n, err
	}

	err = writeUint64(bw, 0)
	if err != nil {
		return n, err
	}

	return n, bw.Flush()
}

// The writers below take *bufio.Writer rather than io.Writer on purpose: it lets
// them use WriteString/WriteByte, which neither convert nor let a scratch buffer
// escape to the heap. Through an io.Writer interface every string and every
// 8-byte header allocated, which on a serve of a large store path is tens of
// allocations per file entry.
func writeString(w *bufio.Writer, s string) error {
	err := writeUint64(w, uint64(len(s)))
	if err != nil {
		return err
	}

	_, err = w.WriteString(s)
	if err != nil {
		return err
	}

	return writePad(w, len(s))
}

func writePad(w *bufio.Writer, n int) error {
	for range (8 - n%8) % 8 {
		err := w.WriteByte(0)
		if err != nil {
			return err
		}
	}

	return nil
}

// writeUint64 writes v in NAR's little-endian 64-bit encoding.
func writeUint64(w *bufio.Writer, v uint64) error {
	for range 8 {
		err := w.WriteByte(byte(v))
		if err != nil {
			return err
		}

		v >>= 8
	}

	return nil
}

// narEntry pairs a directory entry's NAR name (with any case-hack suffix
// stripped) with its actual on-disk name, which we recurse into.
type narEntry struct {
	narName  string
	diskName string
}

// narEntries maps on-disk directory entries to their NAR names. With caseHack on
// (a case-insensitive store), a "~nix~case~hack~N" suffix is stripped from the
// NAR name — nix does the same so both real names survive in the case-sensitive
// NAR. Two entries stripping to the same name are a collision nix rejects.
func narEntries(path string, dirents []os.DirEntry, caseHack bool) ([]narEntry, error) {
	entries := make([]narEntry, 0, len(dirents))

	if !caseHack {
		for _, e := range dirents {
			entries = append(entries, narEntry{narName: e.Name(), diskName: e.Name()})
		}

		return entries, nil
	}

	seen := make(map[string]string, len(dirents))

	for _, e := range dirents {
		disk := e.Name()

		name := disk
		if before, _, found := strings.Cut(disk, caseHackSuffix); found {
			name = before
		}

		prev, ok := seen[name]
		if ok {
			return nil, fmt.Errorf("%w in %s: %q and %q", errNameCollision, path, prev, disk)
		}

		seen[name] = disk

		entries = append(entries, narEntry{narName: name, diskName: disk})
	}

	return entries, nil
}

// writeDirEntries writes the entries of a directory in sorted order. depth is
// the containing directory's nesting level.
func writeDirEntries(w *bufio.Writer, path string, entries []narEntry, caseHack bool, depth int) error {
	for _, entry := range entries {
		err := writeString(w, "entry")
		if err != nil {
			return err
		}

		err = writeString(w, "(")
		if err != nil {
			return err
		}

		err = writeString(w, "name")
		if err != nil {
			return err
		}

		err = writeString(w, entry.narName)
		if err != nil {
			return err
		}

		err = writeString(w, "node")
		if err != nil {
			return err
		}

		childPath := path + "/" + entry.diskName
		// ReadDir already reported the type, but not the mode writeRegular needs
		// for the executable bit, so stat each child — without following it, as a
		// symlink's own mode is what the NAR describes.
		fi, err := os.Lstat(childPath)
		if err != nil {
			return err
		}

		err = writeNode(w, childPath, fi, caseHack, depth+1)
		if err != nil {
			return err
		}

		err = writeString(w, ")")
		if err != nil {
			return err
		}
	}

	return nil
}

func writeDir(w *bufio.Writer, path string, caseHack bool, depth int) error {
	if depth > maxDirDepth {
		return fmt.Errorf("%w of %d: %s", errMaxDepth, maxDirDepth, path)
	}

	err := writeString(w, "(")
	if err != nil {
		return err
	}

	err = writeString(w, "type")
	if err != nil {
		return err
	}

	err = writeString(w, "directory")
	if err != nil {
		return err
	}

	dirents, err := os.ReadDir(path)
	if err != nil {
		return err
	}

	entries, err := narEntries(path, dirents, caseHack)
	if err != nil {
		return err
	}

	// nix orders entries by their NAR (stripped) name, not the on-disk name, and
	// compares raw bytes — never locale-aware or case-insensitive collation.
	slices.SortFunc(entries, func(a, b narEntry) int {
		return strings.Compare(a.narName, b.narName)
	})

	err = writeDirEntries(w, path, entries, caseHack, depth)
	if err != nil {
		return err
	}

	return writeString(w, ")")
}

// writeRegular serialises a regular file. fi is the caller's lstat, used only
// for the executable bit — nix takes that from the lstat and the length from
// the file it opens, so the length always describes the bytes we go on to read.
func writeRegular(w *bufio.Writer, path string, fi fs.FileInfo) error {
	err := writeString(w, "(")
	if err != nil {
		return err
	}

	err = writeString(w, "type")
	if err != nil {
		return err
	}

	err = writeString(w, "regular")
	if err != nil {
		return err
	}

	// nix decides executability from the owner bit alone (S_IXUSR); a file that
	// is group- or other-executable but not owner-executable is a plain regular
	// file in its NAR, so testing all three would change the NAR hash.
	if fi.Mode()&0o100 != 0 {
		err = writeString(w, "executable")
		if err != nil {
			return err
		}

		err = writeString(w, "")
		if err != nil {
			return err
		}
	}

	// O_NOFOLLOW as nix does: if the path was swapped for a symlink since the
	// lstat that chose this branch, refuse it rather than serialise whatever it
	// points at under this entry's name.
	// #nosec G304 -- path comes from trusted filesystem traversal
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	// The length comes from the opened file, not the caller's older lstat, so it
	// describes the file we are about to read even if the path was replaced in
	// between.
	st, err := f.Stat()
	if err != nil {
		return err
	}

	size := st.Size()

	err = writeString(w, "contents")
	if err != nil {
		return err
	}

	err = writeUint64(w, uint64(size)) // #nosec G115 -- size is always non-negative
	if err != nil {
		return err
	}

	// The header above promised exactly size bytes. If the file was truncated
	// between the fstat and now, silently copying fewer would emit a NAR whose
	// length prefix outruns its payload — corrupt, and only detected by whoever
	// tries to import it. CopyN also caps a file that grew, keeping the payload
	// consistent with the header either way.
	n, err := io.CopyN(w, f, size)
	if err != nil {
		return fmt.Errorf("nar: %s: copied %d of %d bytes: %w", path, n, size, err)
	}

	err = writePad(w, int(size))
	if err != nil {
		return err
	}

	return writeString(w, ")")
}

func writeSymlink(w *bufio.Writer, path string) error {
	err := writeString(w, "(")
	if err != nil {
		return err
	}

	err = writeString(w, "type")
	if err != nil {
		return err
	}

	err = writeString(w, "symlink")
	if err != nil {
		return err
	}

	err = writeString(w, "target")
	if err != nil {
		return err
	}

	target, err := os.Readlink(path)
	if err != nil {
		return err
	}

	err = writeString(w, target)
	if err != nil {
		return err
	}

	return writeString(w, ")")
}
