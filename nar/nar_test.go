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
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
)

// writeExport wraps WriteExportStream for tests that already hold the whole NAR
// in memory. Production only ever streams, so this convenience lives with the
// tests that want it rather than in the package's API.
const (
	dashName   = "-dash"
	underName  = "_under"
	hyphenName = "a-b"
)

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
// (which folds punctuation away, putting "ab" before hyphenName) or a
// case-insensitive one (which puts "ab" before "Z") produces a different, and
// therefore differently hashed, NAR. want is written out by hand: comparing
// against a re-sort of the same input would only re-state the implementation.
func TestSortedEntries(t *testing.T) {
	names := []string{"ab", "Z", underName, hyphenName, "q", "A", dashName, "Zz"}
	want := []string{dashName, "A", "Z", "Zz", underName, hyphenName, "ab", "q"}

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

// writeFile writes a file with given content under dir.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()

	err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)
	if err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// dumpDir serialises dir as a complete NAR — the magic plus a directory node —
// with the case hack forced either way, so the bytes can be compared with
// `nix-store --dump`. Write cannot stand in: it picks the case hack from GOOS.
func dumpDir(dir string, caseHack bool) ([]byte, error) {
	var buf bytes.Buffer

	bw := bufio.NewWriter(&buf)

	err := writeString(bw, "nix-archive-1")
	if err != nil {
		return nil, err
	}

	err = writeDir(bw, dir, caseHack, 1)
	if err != nil {
		return nil, err
	}

	err = bw.Flush()
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// TestCaseHackStripsSuffix verifies that with the case hack on, a
// "~nix~case~hack~N" suffix is stripped from the NAR name (so the two real names
// both survive), entries sort by the stripped name, and the raw suffix never
// appears in the output — matching nix's serialisation on a case-insensitive store.
func TestCaseHackStripsSuffix(t *testing.T) {
	dir := t.TempDir()
	// On a case-insensitive store nix would store one of these hacked; we create
	// both literally (Linux is case-sensitive) to drive the stripping logic.
	writeFile(t, dir, "README~nix~case~hack~1", "upper")
	writeFile(t, dir, "readme", "lower")

	out, err := dumpDir(dir, true)
	if err != nil {
		t.Fatalf("writeDir(caseHack=true): %v", err)
	}

	if bytes.Contains(out, []byte(caseHackSuffix)) {
		t.Error("case-hack suffix leaked into the NAR; it must be stripped")
	}

	up := bytes.Index(out, []byte("README"))
	lo := bytes.Index(out, []byte("readme"))

	if up < 0 || lo < 0 {
		t.Fatalf("expected both README (%d) and readme (%d) in the NAR", up, lo)
	}

	// nix sorts by the stripped name: "README" (0x52) sorts before "readme" (0x72).
	if up > lo {
		t.Errorf("entries not sorted by stripped name: README at %d, readme at %d", up, lo)
	}
}

// TestCaseHackDisabledKeepsSuffix verifies that off a case-insensitive store the
// suffix is a normal filename byte (nix does not strip it there).
func TestCaseHackDisabledKeepsSuffix(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "README~nix~case~hack~1", "upper")

	out, err := dumpDir(dir, false)
	if err != nil {
		t.Fatalf("writeDir(caseHack=false): %v", err)
	}

	if !bytes.Contains(out, []byte(caseHackSuffix)) {
		t.Error("with the case hack off, the literal name must be emitted verbatim")
	}
}

// nixCaseHackDump returns nix's NAR for dir with the case hack forced on.
// use-case-hack is an ordinary nix option rather than a macOS-only behaviour,
// so the hardest branch in this package can be measured against nix anywhere,
// not just asserted from what we believe nix does.
func nixCaseHackDump(t *testing.T, dir string) []byte {
	t.Helper()

	_, err := exec.LookPath("nix-store")
	if err != nil {
		t.Skip("nix-store not in PATH")
	}

	dump := func(path string) ([]byte, error) {
		// #nosec G204 -- path is a temporary directory this test created.
		return exec.CommandContext(t.Context(),
			"nix-store", "--option", "use-case-hack", "true", "--dump", path).Output()
	}

	// Probe on an empty directory: a nix that cannot serialise here should skip
	// the test rather than fail it. A nix that ignores the option still fails
	// the comparison below, which is the answer we want to hear about.
	probe := filepath.Join(t.TempDir(), "probe")

	err = os.Mkdir(probe, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	_, err = dump(probe)
	if err != nil {
		t.Skipf("nix-store --dump is unusable here: %v", err)
	}

	out, err := dump(dir)
	if err != nil {
		t.Fatalf("nix-store --dump %s: %v", dir, err)
	}

	return out
}

// TestCaseHackMatchesNixDump is the differential test for the case hack: with
// use-case-hack on, our directory serialisation must be the bytes nix writes.
func TestCaseHackMatchesNixDump(t *testing.T) {
	dir := t.TempDir()

	// Both real names survive as distinct NAR entries, sorted by the stripped
	// name — "README" (0x52) before "readme" (0x72).
	writeFile(t, dir, "README~nix~case~hack~1", "upper")
	writeFile(t, dir, "readme", "lower")

	// nix strips from the marker to the end of the name, not just the digits.
	writeFile(t, dir, "trail~nix~case~hack~9leftovers", "trailing")

	// Directories and symlinks carry hacked names too, and stripping happens at
	// every level, not only the root.
	sub := filepath.Join(dir, "Sub~nix~case~hack~2")

	err := os.Mkdir(sub, 0o750) // #nosec G301 -- test directory
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, sub, "X", "upper x")
	writeFile(t, sub, "x~nix~case~hack~3", "lower x")

	err = os.Symlink("readme", filepath.Join(dir, "link~nix~case~hack~4"))
	if err != nil {
		t.Fatal(err)
	}

	theirs := nixCaseHackDump(t, dir)

	ours, err := dumpDir(dir, true)
	if err != nil {
		t.Fatalf("writeDir(caseHack=true): %v", err)
	}

	if bytes.Equal(ours, theirs) {
		return
	}

	off := 0
	for off < len(ours) && off < len(theirs) && ours[off] == theirs[off] {
		off++
	}

	t.Errorf("case-hack NAR mismatch: ours %d bytes, nix %d bytes, first difference at offset %d\n ours: %q\n nix:  %q",
		len(ours), len(theirs), off,
		ours[off:min(off+48, len(ours))], theirs[off:min(off+48, len(theirs))])
}

// TestCaseHackCollision verifies two entries stripping to the same NAR name are
// rejected, as nix rejects them.
func TestCaseHackCollision(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a", "one")
	writeFile(t, dir, "a~nix~case~hack~1", "two")

	_, err := dumpDir(dir, true)
	if !errors.Is(err, errNameCollision) {
		t.Errorf("expected errNameCollision, got %v", err)
	}
}

// requireNix skips unless nix can serialise a path here. `nix-store --dump` is
// pure local serialisation — it neither contacts the daemon nor touches
// /nix/store — so this runs wherever the nix binary exists, and skips with a
// reason where it does not.
func requireNix(t *testing.T) {
	t.Helper()

	_, err := exec.LookPath("nix-store")
	if err != nil {
		t.Skip("nix-store not in PATH")
	}

	probe := filepath.Join(t.TempDir(), "probe")

	err = os.WriteFile(probe, []byte("probe"), 0o600) // #nosec G306 -- test fixture
	if err != nil {
		t.Fatal(err)
	}

	// #nosec G204 -- probe is a temp file we just created.
	err = exec.CommandContext(t.Context(), "nix-store", "--dump", probe).Run()
	if err != nil {
		t.Skipf("nix-store --dump is unusable here: %v", err)
	}
}

// nixDump returns nix's canonical NAR for path: the ground truth nar.Write must
// reproduce byte for byte, since every consumer hashes it.
func nixDump(t *testing.T, path string) []byte {
	t.Helper()

	// #nosec G204 -- path is a test fixture or a store path.
	out, err := exec.CommandContext(t.Context(), "nix-store", "--dump", path).Output()
	if err != nil {
		t.Fatalf("nix-store --dump %s: %v", path, err)
	}

	return out
}

// ourDump returns nar.Write's NAR for path.
func ourDump(t *testing.T, path string) []byte {
	t.Helper()

	var buf bytes.Buffer

	err := Write(&buf, path)
	if err != nil {
		t.Fatalf("nar.Write %s: %v", path, err)
	}

	return buf.Bytes()
}

// assertMatchesNix compares our NAR for path with nix's, byte for byte.
func assertMatchesNix(t *testing.T, path string) {
	t.Helper()

	ours := ourDump(t, path)
	theirs := nixDump(t, path)

	if bytes.Equal(ours, theirs) {
		return
	}

	off := 0
	for off < len(ours) && off < len(theirs) && ours[off] == theirs[off] {
		off++
	}

	t.Errorf("NAR mismatch for %s: ours %d bytes, nix %d bytes, first difference at offset %d\n ours: %q\n nix:  %q",
		path, len(ours), len(theirs), off,
		ours[off:min(off+48, len(ours))], theirs[off:min(off+48, len(theirs))])
}

// chmod sets an exact mode, which os.WriteFile cannot: the umask trims it.
func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()

	err := os.Chmod(path, mode)
	if err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	if fi.Mode().Perm() != mode {
		t.Skipf("filesystem stored mode %o for %s, not %o", fi.Mode().Perm(), path, mode)
	}
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()

	err := os.WriteFile(path, []byte(content), 0o600) // #nosec G306 -- test fixture
	if err != nil {
		t.Fatal(err)
	}

	chmod(t, path, mode)
}

func mkdir(t *testing.T, path string) {
	t.Helper()

	err := os.MkdirAll(path, 0o755) // #nosec G301 -- test fixture
	if err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, path string) {
	t.Helper()

	err := os.Symlink(target, path)
	if err != nil {
		t.Fatal(err)
	}
}

// TestNarMatchesNixDump is the differential test: for each fixture, our NAR must
// be exactly the bytes nix produces. Every other test in this package asserts
// what we believe nix does; this one asks it.
func TestNarMatchesNixDump(t *testing.T) {
	requireNix(t)

	tests := []struct {
		name string
		// build populates dir and returns the path to serialise.
		build func(t *testing.T, dir string) string
	}{
		{
			name: "regular file",
			build: func(t *testing.T, dir string) string {
				t.Helper()

				p := filepath.Join(dir, "plain.txt")
				write(t, p, "hello\n", 0o644)

				return p
			},
		},
		{
			name: "empty file",
			build: func(t *testing.T, dir string) string {
				t.Helper()

				p := filepath.Join(dir, "empty")
				write(t, p, "", 0o644)

				return p
			},
		},
		{
			name: "executable",
			build: func(t *testing.T, dir string) string {
				t.Helper()

				p := filepath.Join(dir, "run.sh")
				write(t, p, "#!/bin/sh\necho hi\n", 0o755)

				return p
			},
		},
		{
			name: "owner execute only",
			build: func(t *testing.T, dir string) string {
				t.Helper()

				p := filepath.Join(dir, "private.sh")
				write(t, p, "#!/bin/sh\n", 0o700)

				return p
			},
		},
		{
			// nix keys executability off the owner bit alone, so this file is a
			// plain regular one to it however executable the group finds it.
			name: "group execute only",
			build: func(t *testing.T, dir string) string {
				t.Helper()

				p := filepath.Join(dir, "grouponly")
				write(t, p, "not executable to nix\n", 0o454)

				return p
			},
		},
		{
			name: "other execute only",
			build: func(t *testing.T, dir string) string {
				t.Helper()

				p := filepath.Join(dir, "otheronly")
				write(t, p, "not executable to nix\n", 0o645)

				return p
			},
		},
		{
			name: "symlink",
			build: func(t *testing.T, dir string) string {
				t.Helper()

				p := filepath.Join(dir, "link")
				symlink(t, "some/relative/target", p)

				return p
			},
		},
		{
			name: "empty directory",
			build: func(t *testing.T, dir string) string {
				t.Helper()

				p := filepath.Join(dir, "empty")
				mkdir(t, p)

				return p
			},
		},
		{
			// Every file length modulo 8, to pin the padding of the contents field.
			name: "padding",
			build: func(t *testing.T, dir string) string {
				t.Helper()

				for n := range 17 {
					write(t, filepath.Join(dir, "f"+strconv.Itoa(n)), string(bytes.Repeat([]byte("x"), n)), 0o644)
				}

				return dir
			},
		},
		{
			// Entry order is a raw byte comparison. A locale-aware collation
			// (folding away the "-") or a case-insensitive one (putting "ab"
			// before "Z") reorders these and changes the NAR hash.
			name: "entry ordering",
			build: func(t *testing.T, dir string) string {
				t.Helper()

				for _, name := range []string{"ab", "Z", underName, hyphenName, "q", "A", dashName, "Zz", "a b", "é"} {
					write(t, filepath.Join(dir, name), name, 0o644)
				}

				return dir
			},
		},
		{
			name: "tree",
			build: func(t *testing.T, dir string) string {
				t.Helper()

				mkdir(t, filepath.Join(dir, "bin"))
				mkdir(t, filepath.Join(dir, "share", "doc"))
				write(t, filepath.Join(dir, "bin", "prog"), "#!/bin/sh\n", 0o755)
				write(t, filepath.Join(dir, "share", "doc", "README"), "docs\n", 0o444)
				symlink(t, "../bin/prog", filepath.Join(dir, "share", "prog"))
				symlink(t, "/nix/store/nonexistent", filepath.Join(dir, "absolute"))
				symlink(t, "nowhere", filepath.Join(dir, "broken"))

				return dir
			},
		},
		{
			// Larger than the sink buffer, so the contents field spans flushes.
			name: "large file",
			build: func(t *testing.T, dir string) string {
				t.Helper()

				buf := make([]byte, 3<<20)
				for i := range buf {
					buf[i] = byte(i * 7)
				}

				p := filepath.Join(dir, "big.bin")

				err := os.WriteFile(p, buf, 0o600) // #nosec G306 -- test fixture
				if err != nil {
					t.Fatal(err)
				}

				return p
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertMatchesNix(t, tt.build(t, t.TempDir()))
		})
	}
}

// TestNarMatchesNixDumpStorePath runs the same comparison over a real store
// path. A system's sw directory is a buildEnv symlink forest — the structure
// that first exposed the macOS case-hack divergence — so it is worth checking
// wherever one exists.
func TestNarMatchesNixDumpStorePath(t *testing.T) {
	requireNix(t)

	sw, err := filepath.EvalSymlinks("/run/current-system/sw")
	if err != nil {
		t.Skipf("no /run/current-system/sw: %v", err)
	}

	assertMatchesNix(t, sw)
}
