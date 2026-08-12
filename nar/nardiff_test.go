// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package nar_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/kradalby/tsnixcache/nar"
)

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

	err := nar.Write(&buf, path)
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

				for _, name := range []string{"ab", "Z", "_under", "a-b", "q", "A", "-dash", "Zz", "a b", "é"} {
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
