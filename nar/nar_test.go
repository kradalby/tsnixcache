// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package nar

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestGoldenSHA256 is a port of the tailscale nardump golden hash test.
func TestGoldenSHA256(t *testing.T) {
	dir := t.TempDir()

	// sub/dir/ (directory)
	if err := os.MkdirAll(filepath.Join(dir, "sub", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// brokenlink → "brokenfile"
	if err := os.Symlink("brokenfile", filepath.Join(dir, "brokenlink")); err != nil {
		t.Fatal(err)
	}
	// dirl → "sub/dir"
	if err := os.Symlink("sub/dir", filepath.Join(dir, "dirl")); err != nil {
		t.Fatal(err)
	}
	// dirb → "/abs/nonexistentdir"
	if err := os.Symlink("/abs/nonexistentdir", filepath.Join(dir, "dirb")); err != nil {
		t.Fatal(err)
	}
	// sub/dir/file1 → empty regular file
	f1, err := os.Create(filepath.Join(dir, "sub", "dir", "file1"))
	if err != nil {
		t.Fatal(err)
	}
	f1.Close()
	// file2m → 2MB zero file
	f2m, err := os.Create(filepath.Join(dir, "file2m"))
	if err != nil {
		t.Fatal(err)
	}
	f2m.Close()
	if err := os.Truncate(filepath.Join(dir, "file2m"), 2*1024*1024); err != nil {
		t.Fatal(err)
	}
	// sub/goodlink → "../file2m"
	if err := os.Symlink("../file2m", filepath.Join(dir, "sub", "goodlink")); err != nil {
		t.Fatal(err)
	}

	h := sha256.New()
	if err := Write(h, dir); err != nil {
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
	if err := os.WriteFile(path, []byte("hello NAR"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Write(&buf, path); err != nil {
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
	if err := os.Symlink("target", link); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Write(&buf, link); err != nil {
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

// TestExecBit verifies that a file with mode 0755 has "executable" in the NAR.
func TestExecBit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Write(&buf, path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("executable")) {
		t.Error("missing 'executable' string for 0755 file")
	}
}

// TestEmptyDir verifies an empty directory root produces a valid NAR.
func TestEmptyDir(t *testing.T) {
	dir := t.TempDir()
	emptyDir := filepath.Join(dir, "empty")
	if err := os.Mkdir(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Write(&buf, emptyDir); err != nil {
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
	for i := 0; i < 10; i++ {
		nested = filepath.Join(nested, "dir")
		if err := os.Mkdir(nested, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(nested, "file"), []byte("deep"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Write(&buf, dir); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// TestLargeFileStreaming creates a 32MB file and verifies Write succeeds
// and the output is larger than 32MB (contents included).
func TestLargeFileStreaming(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	const size = 32 * 1024 * 1024
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Write(&buf, path); err != nil {
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
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Write(&buf, path); err != nil {
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

// TestSortedEntries verifies NAR contains entry names in lexicographic order.
func TestSortedEntries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"z", "a", "m"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	if err := Write(&buf, dir); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b := buf.Bytes()

	idxA := bytes.Index(b, []byte("a"))
	idxM := bytes.Index(b, []byte("m"))
	idxZ := bytes.Index(b, []byte("z"))

	if idxA >= idxM || idxM >= idxZ {
		t.Errorf("entries not sorted: positions a=%d m=%d z=%d", idxA, idxM, idxZ)
	}
}

// TestWriteExportRoundTrip verifies the export stream format.
func TestWriteExportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	var narBuf bytes.Buffer
	if err := Write(&narBuf, path); err != nil {
		t.Fatalf("Write NAR: %v", err)
	}
	narBytes := narBuf.Bytes()

	storePath := "/nix/store/abc123-hello-1.0"
	refs := []string{"/nix/store/dep1", "/nix/store/dep2"}
	deriver := "/nix/store/abc123-hello-1.0.drv"

	var out bytes.Buffer
	if err := WriteExport(&out, narBytes, storePath, refs, deriver); err != nil {
		t.Fatalf("WriteExport: %v", err)
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

	// Must contain NIXE magic
	if !bytes.Contains(b, []byte("NIXE\x00\x00\x00\x00")) {
		t.Error("missing NIXE magic")
	}

	// Must contain storePath
	if !bytes.Contains(b, []byte(storePath)) {
		t.Error("missing storePath in export")
	}
}
