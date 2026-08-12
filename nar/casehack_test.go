// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package nar

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

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
