// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package nar

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
)

// writeExport wraps WriteExportStream for tests that already hold the whole NAR
// in memory. Production only ever streams, so this convenience lives with the
// tests that want it rather than in the package's API.
func writeExport(w io.Writer, narBytes []byte, storePath string, refs []string, deriver string) error {
	_, err := WriteExportStream(w, bytes.NewReader(narBytes), storePath, refs, deriver)

	return err
}

// A minimal NAR decoder, written against the format rather than against nar.go,
// so tests can state what the archive should contain instead of grepping the
// byte stream for substrings that also occur in file contents.

// narNode is a decoded NAR node.
type narNode struct {
	typ     string // "regular", "symlink" or "directory"
	exec    bool
	target  string   // symlink only
	content []byte   // regular only
	names   []string // directory entry names, in the order the NAR lists them
	entries map[string]*narNode
}

type narReader struct {
	t   *testing.T
	b   []byte
	off int
}

// blob reads one length-prefixed, 8-byte-aligned field.
func (r *narReader) blob() []byte {
	r.t.Helper()

	if r.off+8 > len(r.b) {
		r.t.Fatalf("nar truncated: want a length at offset %d, have %d bytes", r.off, len(r.b))
	}

	n := int(binary.LittleEndian.Uint64(r.b[r.off:])) // #nosec G115 -- bounded against the buffer below
	r.off += 8

	if n < 0 || n > len(r.b)-r.off {
		r.t.Fatalf("nar field at offset %d claims %d bytes, only %d remain", r.off-8, n, len(r.b)-r.off)
	}

	v := r.b[r.off : r.off+n]
	r.off += n

	for pad := (8 - n%8) % 8; pad > 0; pad-- {
		if r.b[r.off] != 0 {
			r.t.Fatalf("non-zero padding at offset %d", r.off)
		}

		r.off++
	}

	return v
}

func (r *narReader) str() string {
	r.t.Helper()

	return string(r.blob())
}

func (r *narReader) expect(want string) {
	r.t.Helper()

	got := r.str()
	if got != want {
		r.t.Fatalf("nar: got token %q at offset %d, want %q", got, r.off, want)
	}
}

// node decodes one node, leaving off just past its closing ")".
func (r *narReader) node() *narNode {
	r.t.Helper()

	r.expect("(")
	r.expect("type")

	n := &narNode{typ: r.str()}

	switch n.typ {
	case "regular":
		tok := r.str()
		if tok == "executable" {
			r.expect("")

			n.exec = true
			tok = r.str()
		}

		if tok != "contents" {
			r.t.Fatalf("nar: got token %q in regular node, want %q", tok, "contents")
		}

		n.content = r.blob()

	case "symlink":
		r.expect("target")

		n.target = r.str()

	case "directory":
		n.entries = map[string]*narNode{}

		// A directory's entry list is terminated by its own ")", so this loop
		// consumes the closing token itself and returns early.
		for {
			tok := r.str()
			if tok == ")" {
				return n
			}

			if tok != "entry" {
				r.t.Fatalf("nar: got token %q in directory, want %q or %q", tok, "entry", ")")
			}

			r.expect("(")
			r.expect("name")

			name := r.str()

			r.expect("node")

			n.names = append(n.names, name)
			n.entries[name] = r.node()

			r.expect(")")
		}

	default:
		r.t.Fatalf("nar: unknown node type %q", n.typ)
	}

	r.expect(")")

	return n
}

// parseNAR decodes a complete NAR archive and fails on any trailing garbage —
// which is how a length prefix that outruns its payload shows up.
func parseNAR(t *testing.T, b []byte) *narNode {
	t.Helper()

	r := &narReader{t: t, b: b}
	r.expect("nix-archive-1")

	n := r.node()

	if r.off != len(r.b) {
		t.Fatalf("nar: %d trailing bytes after the root node", len(r.b)-r.off)
	}

	return n
}

// dumpNAR serialises path and decodes the result.
func dumpNAR(t *testing.T, path string) *narNode {
	t.Helper()

	var buf bytes.Buffer

	err := Write(&buf, path)
	if err != nil {
		t.Fatalf("Write(%s): %v", path, err)
	}

	return parseNAR(t, buf.Bytes())
}

// TestGoldenSHA256 is a port of the tailscale nardump golden hash test.
func TestGoldenSHA256(t *testing.T) {
	dir := t.TempDir()

	// sub/dir/ (directory)
	err := os.MkdirAll(filepath.Join(dir, "sub", "dir"), 0o750) // #nosec G301 -- test directory permissions
	if err != nil {
		t.Fatal(err)
	}

	// brokenlink → "brokenfile"
	err = os.Symlink("brokenfile", filepath.Join(dir, "brokenlink"))
	if err != nil {
		t.Fatal(err)
	}

	// dirl → "sub/dir"
	err = os.Symlink("sub/dir", filepath.Join(dir, "dirl"))
	if err != nil {
		t.Fatal(err)
	}

	// dirb → "/abs/nonexistentdir"
	err = os.Symlink("/abs/nonexistentdir", filepath.Join(dir, "dirb"))
	if err != nil {
		t.Fatal(err)
	}

	// sub/dir/file1 → empty regular file
	f1, err := os.Create(filepath.Join(dir, "sub", "dir", "file1")) // #nosec G304 -- test path
	if err != nil {
		t.Fatal(err)
	}

	err = f1.Close()
	if err != nil {
		t.Fatal(err)
	}

	// file2m → 2MB zero file
	f2m, err := os.Create(filepath.Join(dir, "file2m")) // #nosec G304 -- test path
	if err != nil {
		t.Fatal(err)
	}

	err = f2m.Close()
	if err != nil {
		t.Fatal(err)
	}

	err = os.Truncate(filepath.Join(dir, "file2m"), 2*1024*1024)
	if err != nil {
		t.Fatal(err)
	}

	// sub/goodlink → "../file2m"
	err = os.Symlink("../file2m", filepath.Join(dir, "sub", "goodlink"))
	if err != nil {
		t.Fatal(err)
	}

	h := sha256.New()

	err = Write(h, dir)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := hex.EncodeToString(h.Sum(nil))

	want := "727613a36f41030e93a4abf2649c3ec64a2757ccff364e3f6f7d544eb976e442"
	if got != want {
		t.Errorf("sha256 = %s, want %s", got, want)
	}
}

// TestTopLevelRegularFile verifies a single regular file root produces a valid NAR.
func TestTopLevelRegularFile(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "single.txt")

	err := os.WriteFile(path, []byte("hello NAR"), 0o600) // #nosec G306 -- test file
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer

	err = Write(&buf, path)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	b := buf.Bytes()

	if !bytes.Contains(b, []byte("nix-archive-1")) {
		t.Error("missing magic string")
	}

	if !bytes.Contains(b, []byte("regular")) {
		t.Error("missing 'regular' type")
	}
}

// TestTopLevelSymlink verifies a single symlink root produces a valid NAR.
func TestTopLevelSymlink(t *testing.T) {
	dir := t.TempDir()

	link := filepath.Join(dir, "mylink")

	err := os.Symlink("target", link)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer

	err = Write(&buf, link)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	b := buf.Bytes()

	if !bytes.Contains(b, []byte("nix-archive-1")) {
		t.Error("missing magic string")
	}

	if !bytes.Contains(b, []byte("symlink")) {
		t.Error("missing 'symlink' type")
	}
}

// TestExecBit pins NAR executability to nix's rule: the owner execute bit alone
// (S_IXUSR). Testing any execute bit would mark a group- or other-executable
// file executable, changing the NAR hash of a path nix hashes differently.
func TestExecBit(t *testing.T) {
	tests := []struct {
		name string
		mode os.FileMode
		want bool
	}{
		{name: "owner and all", mode: 0o755, want: true},
		{name: "owner only", mode: 0o700, want: true},
		{name: "owner write-protected", mode: 0o544, want: true},
		{name: "not executable", mode: 0o644, want: false},
		{name: "group executable only", mode: 0o454, want: false},
		{name: "group and other executable", mode: 0o411, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "script.sh")

			err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600) // #nosec G306 -- test file
			if err != nil {
				t.Fatal(err)
			}

			// Chmod rather than rely on WriteFile, whose mode the umask trims.
			err = os.Chmod(path, tt.mode)
			if err != nil {
				t.Fatal(err)
			}

			fi, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}

			if fi.Mode().Perm() != tt.mode {
				t.Skipf("filesystem stored mode %o, not %o", fi.Mode().Perm(), tt.mode)
			}

			node := dumpNAR(t, path)
			if node.exec != tt.want {
				t.Errorf("mode %o: executable = %v, want %v", tt.mode, node.exec, tt.want)
			}
		})
	}
}

// TestEmptyDir verifies an empty directory root produces a valid NAR.
func TestEmptyDir(t *testing.T) {
	dir := t.TempDir()

	emptyDir := filepath.Join(dir, "empty")

	err := os.Mkdir(emptyDir, 0o750) // #nosec G301 -- test directory
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer

	err = Write(&buf, emptyDir)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	b := buf.Bytes()

	if !bytes.Contains(b, []byte("nix-archive-1")) {
		t.Error("missing magic string")
	}

	if !bytes.Contains(b, []byte("directory")) {
		t.Error("missing 'directory' type")
	}
}

// TestDeeplyNested verifies 10 levels of nesting completes without error.
func TestDeeplyNested(t *testing.T) {
	dir := t.TempDir()

	nested := dir

	for range 10 {
		nested = filepath.Join(nested, "dir")

		err := os.Mkdir(nested, 0o750) // #nosec G301 -- test directory
		if err != nil {
			t.Fatal(err)
		}
	}

	err := os.WriteFile(filepath.Join(nested, "file"), []byte("deep"), 0o600) // #nosec G306 -- test file
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer

	err = Write(&buf, dir)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// TestLargeFileStreaming creates a 32MB file and verifies Write succeeds
// and the output is larger than 32MB (contents included).
func TestLargeFileStreaming(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "big.bin")

	f, err := os.Create(path) // #nosec G304 -- test path
	if err != nil {
		t.Fatal(err)
	}

	err = f.Close()
	if err != nil {
		t.Fatal(err)
	}

	const size = 32 * 1024 * 1024

	err = os.Truncate(path, size)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer

	err = Write(&buf, path)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if buf.Len() < size {
		t.Errorf("output size %d < file size %d", buf.Len(), size)
	}
}

// TestZeroByteFile verifies a zero-byte file produces a NAR with contents u64(0).
func TestZeroByteFile(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "empty.txt")

	err := os.WriteFile(path, nil, 0o600) // #nosec G306 -- test file
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer

	err = Write(&buf, path)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	b := buf.Bytes()

	if !bytes.Contains(b, []byte("contents")) {
		t.Error("missing 'contents' string")
	}

	// Find "contents" and check that 8 bytes after the string+padding is u64(0)
	// "contents" = 8 chars, written as: u64(8) + "contents" (no pad needed)
	// so pattern is: [8 0 0 0 0 0 0 0] "contents" [0 0 0 0 0 0 0 0]
	contentsHdr := append([]byte{8, 0, 0, 0, 0, 0, 0, 0}, []byte("contents")...)

	idx := bytes.Index(b, contentsHdr)
	if idx < 0 {
		t.Fatal("cannot find 'contents' header in output")
	}

	sizeOff := idx + len(contentsHdr)
	if sizeOff+8 > len(b) {
		t.Fatal("output too short after 'contents'")
	}

	size := binary.LittleEndian.Uint64(b[sizeOff:])
	if size != 0 {
		t.Errorf("contents size = %d, want 0", size)
	}
}

// TestSortedEntries pins directory entry order to a raw byte comparison, which
// is what nix does. The names are chosen so that a locale-aware collation
// (which folds punctuation away, putting "ab" before "a-b") or a
// case-insensitive one (which puts "ab" before "Z") produces a different, and
// therefore differently hashed, NAR. want is written out by hand: comparing
// against a re-sort of the same input would only re-state the implementation.
func TestSortedEntries(t *testing.T) {
	names := []string{"ab", "Z", "_under", "a-b", "q", "A", "-dash", "Zz"}
	want := []string{"-dash", "A", "Z", "Zz", "_under", "a-b", "ab", "q"}

	dir := t.TempDir()

	for _, name := range names {
		err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600) // #nosec G306 -- test file
		if err != nil {
			t.Fatal(err)
		}
	}

	got := dumpNAR(t, dir).names

	if !slices.Equal(got, want) {
		t.Errorf("entry order:\n got %q\nwant %q", got, want)
	}
}

// TestRegularSizeFromOpenFile covers a file changing size between the lstat
// that chose the regular-file branch and the open that reads it. The length
// prefix must come from the opened file, as nix's does, so it describes the
// bytes actually copied; a NAR whose prefix outruns its payload is corrupt, and
// nothing downstream would notice until an import failed.
func TestRegularSizeFromOpenFile(t *testing.T) {
	const stale = 4096

	tests := []struct {
		name   string
		actual int64
	}{
		{name: "unchanged", actual: stale},
		{name: "truncated", actual: 512},
		{name: "truncated to empty", actual: 0},
		{name: "grown", actual: stale * 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "shifting.bin")

			err := os.WriteFile(path, bytes.Repeat([]byte("x"), stale), 0o600) // #nosec G306 -- test file
			if err != nil {
				t.Fatal(err)
			}

			// The stat the directory walk would have taken, before the change.
			fi, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}

			err = os.Truncate(path, tt.actual)
			if err != nil {
				t.Fatal(err)
			}

			var buf bytes.Buffer

			bw := bufio.NewWriter(&buf)

			err = writeRegular(bw, path, fi)
			if err != nil {
				t.Fatalf("writeRegular: %v", err)
			}

			err = bw.Flush()
			if err != nil {
				t.Fatal(err)
			}

			r := &narReader{t: t, b: buf.Bytes()}

			node := r.node()
			if int64(len(node.content)) != tt.actual {
				t.Errorf("contents = %d bytes, want %d — the length came from the stale lstat, not the open file",
					len(node.content), tt.actual)
			}

			if r.off != len(buf.Bytes()) {
				t.Errorf("%d bytes trailing the node — payload outran its length prefix", len(buf.Bytes())-r.off)
			}
		})
	}
}

// TestRegularRefusesSymlink pins the O_NOFOLLOW open: if the path is a symlink
// by the time writeRegular runs — swapped since the lstat that called it a
// regular file — the target must not be serialised under this entry's name.
func TestRegularRefusesSymlink(t *testing.T) {
	dir := t.TempDir()

	target := filepath.Join(dir, "target")

	err := os.WriteFile(target, []byte("secret"), 0o600) // #nosec G306 -- test file
	if err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "link")

	err = os.Symlink(target, link)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer

	bw := bufio.NewWriter(&buf)

	err = writeRegular(bw, link, fi)
	if err == nil {
		t.Fatal("writeRegular followed a symlink; it must open with O_NOFOLLOW")
	}
}

// TestUnsupportedFileTypes covers what nix's dumpPath refuses. A FIFO is the
// one that bites: os.Open on it blocks until a writer appears, which on the
// serve path pins a connection and its goroutine for the process lifetime.
// The test hangs rather than fails if that regresses, which go test's timeout
// turns back into a failure.
func TestUnsupportedFileTypes(t *testing.T) {
	dir := t.TempDir()

	fifo := filepath.Join(dir, "fifo")

	err := syscall.Mkfifo(fifo, 0o600)
	if err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	err = Write(io.Discard, fifo)
	if !errors.Is(err, errUnsupportedType) {
		t.Errorf("Write(fifo) = %v, want errUnsupportedType", err)
	}

	err = Write(io.Discard, dir)
	if !errors.Is(err, errUnsupportedType) {
		t.Errorf("Write(dir containing a fifo) = %v, want errUnsupportedType", err)
	}

	// A character device is well-formed enough to serialise as an empty regular
	// file, which is exactly the silent wrong answer nix refuses to give.
	fi, err := os.Lstat("/dev/null")
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return
	}

	err = Write(io.Discard, "/dev/null")
	if !errors.Is(err, errUnsupportedType) {
		t.Errorf("Write(/dev/null) = %v, want errUnsupportedType", err)
	}
}

// TestMaxDirDepth pins nix's nesting limit. Measured against nix 2.34.7: a root
// directory with 63 directories below it dumps and restores, one more is
// refused by both ends, so a NAR we produced past the limit would be one no nix
// could unpack.
func TestMaxDirDepth(t *testing.T) {
	tests := []struct {
		depth   int
		wantErr bool
	}{
		{depth: maxDirDepth},
		{depth: maxDirDepth + 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.depth), func(t *testing.T) {
			root := t.TempDir()

			nested := root
			for range tt.depth - 1 {
				nested = filepath.Join(nested, "d")
			}

			err := os.MkdirAll(nested, 0o750) // #nosec G301 -- test directory
			if err != nil {
				t.Fatal(err)
			}

			err = Write(io.Discard, root)
			if errors.Is(err, errMaxDepth) != tt.wantErr {
				t.Errorf("Write(%d directories deep) = %v, want errMaxDepth: %v", tt.depth, err, tt.wantErr)
			}
		})
	}
}

// TestExportReferencesSorted pins the reference block to nix's order. nix keeps
// them in a StorePathSet, so an export stream built from an arbitrary slice
// order is a valid import but not the byte stream nix would have written.
func TestExportReferencesSorted(t *testing.T) {
	refs := []string{"/nix/store/ccc-c", "/nix/store/aaa-a", "/nix/store/bbb-b"}
	want := []string{"/nix/store/aaa-a", "/nix/store/bbb-b", "/nix/store/ccc-c"}

	given := slices.Clone(refs)

	var out bytes.Buffer

	err := writeExport(&out, nil, "/nix/store/ddd-d", refs, "")
	if err != nil {
		t.Fatalf("writeExport: %v", err)
	}

	if !slices.Equal(refs, given) {
		t.Errorf("the caller's slice was reordered to %q; it must be left alone", refs)
	}

	// Walk to the reference block. The NAR is empty here, so what precedes it is
	// u64(1) and the eight raw bytes of the NIXE magic — which is not a
	// length-prefixed field — followed by the store path, which is.
	r := &narReader{t: t, b: out.Bytes(), off: 16}

	r.expect("/nix/store/ddd-d")

	n := int(binary.LittleEndian.Uint64(r.b[r.off:])) // #nosec G115 -- we wrote this count ourselves
	r.off += 8

	got := make([]string, 0, n)
	for range n {
		got = append(got, r.str())
	}

	if !slices.Equal(got, want) {
		t.Errorf("references:\n got %q\nwant %q", got, want)
	}
}

// TestSinkWriteSize checks the NAR leaves the process in chunks worth writing.
// The sink is a zstd encoder, an HTTP response or nix-store's stdin, so a
// bufio default of 4 KiB would drip a multi-megabyte NAR out 4096 bytes at a
// time.
func TestSinkWriteSize(t *testing.T) {
	const size = 4 << 20

	path := filepath.Join(t.TempDir(), "big.bin")

	err := os.WriteFile(path, make([]byte, size), 0o600) // #nosec G306 -- test file
	if err != nil {
		t.Fatal(err)
	}

	cw := &countingWriter{}

	err = Write(cw, path)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	const wantMin = 64 << 10
	if cw.max < wantMin {
		t.Errorf("largest write to the sink = %d bytes, want at least %d", cw.max, wantMin)
	}
}

// TestWriteExportRoundTrip decodes the whole export trailer. `nix-store
// --import` is the only reader of this stream, and it accepts one that carries
// no references and no deriver just as happily: the path lands in the store
// believing it depends on nothing, so the next nix-collect-garbage takes its
// closure away underneath it. Nothing short of decoding the fields notices.
func TestWriteExportRoundTrip(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "hello.txt")

	err := os.WriteFile(path, []byte("hello"), 0o600) // #nosec G306 -- test file
	if err != nil {
		t.Fatal(err)
	}

	var narBuf bytes.Buffer

	err = Write(&narBuf, path)
	if err != nil {
		t.Fatalf("Write NAR: %v", err)
	}

	narBytes := narBuf.Bytes()

	storePath := "/nix/store/abc123-hello-1.0"
	refs := []string{"/nix/store/dep1", "/nix/store/dep2"}
	deriver := "/nix/store/abc123-hello-1.0.drv"

	var out bytes.Buffer

	err = writeExport(&out, narBytes, storePath, refs, deriver)
	if err != nil {
		t.Fatalf("writeExport: %v", err)
	}

	b := out.Bytes()

	// First 8 bytes must be u64(1)
	if len(b) < 8 {
		t.Fatal("output too short")
	}

	magic := binary.LittleEndian.Uint64(b[:8])
	if magic != 1 {
		t.Errorf("leading uint64 = %d, want 1", magic)
	}

	// The NAR is copied through untouched, so the trailer starts at a known offset.
	if !bytes.Equal(b[8:8+len(narBytes)], narBytes) {
		t.Fatal("the NAR was not copied verbatim into the export stream")
	}

	off := 8 + len(narBytes)
	if got := string(b[off : off+8]); got != "NIXE\x00\x00\x00\x00" {
		t.Fatalf("token after the NAR = %q, want the NIXE magic", got)
	}

	r := &narReader{t: t, b: b, off: off + 8}

	r.expect(storePath)

	n := int(binary.LittleEndian.Uint64(r.b[r.off:])) // #nosec G115 -- we wrote this count ourselves
	r.off += 8

	got := make([]string, 0, n)
	for range n {
		got = append(got, r.str())
	}

	if !slices.Equal(got, refs) {
		t.Errorf("references in the export:\n got %q\nwant %q", got, refs)
	}

	r.expect(deriver)

	// The two trailing counts nix reads as "no signatures" and "not content
	// addressed", and then the stream is over: a trailer short of them leaves
	// nix-store blocked on a read that never comes.
	for range 2 {
		if v := binary.LittleEndian.Uint64(r.b[r.off:]); v != 0 {
			t.Errorf("trailing count at offset %d = %d, want 0", r.off, v)
		}

		r.off += 8
	}

	if r.off != len(b) {
		t.Errorf("%d bytes left after the trailer, want the stream to end there", len(b)-r.off)
	}
}
