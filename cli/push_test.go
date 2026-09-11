// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/peterbourgon/ff/v4"
	"github.com/stretchr/testify/require"
	"golang.org/x/term"
)

// TestProgressEnabledZeroWidthTerminal covers the terminal that reports 0
// columns (a pty nobody has sized: "script" without a controlling terminal,
// "ssh -tt" from a non-tty, some CI runners). mpb draws zero-width bars there —
// nothing at all — while progress mode suppresses the per-path logs, so the
// push looks hung. Plain logs must win.
func TestProgressEnabledZeroWidthTerminal(t *testing.T) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}

	defer ptmx.Close() // #nosec G104 -- test cleanup

	fd := ptmx.Fd()
	if !isatty.IsTerminal(fd) {
		t.Skip("pty master is not reported as a terminal here")
	}

	width, _, err := term.GetSize(int(fd))
	if err != nil || width != 0 {
		t.Skipf("fresh pty is not zero-width (width=%d, err=%v)", width, err)
	}

	if progressEnabled(false, fd) {
		t.Error("progressEnabled on a zero-width terminal should be false")
	}

	if progressEnabled(true, fd) {
		t.Error("progressEnabled(noProgress=true) should be false regardless of fd")
	}
}

func TestProgressEnabledNonTerminal(t *testing.T) {
	// A pipe is never a terminal.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	defer pr.Close() // #nosec G104 -- test cleanup
	defer pw.Close() // #nosec G104 -- test cleanup

	if progressEnabled(false, pr.Fd()) {
		t.Error("progressEnabled on a non-terminal fd should be false")
	}
}

func TestCheckCacheURL(t *testing.T) {
	good := []string{"http://tsnixcache", "https://cache.example.com:5000/", "http://127.0.0.1:5000"}
	for _, in := range good {
		err := checkCacheURL(in)
		if err != nil {
			t.Errorf("checkCacheURL(%q) = %v, want nil", in, err)
		}
	}

	// "localhost:5000" parses as scheme "localhost", opaque "5000" — the typo
	// that used to fail once per path after the whole closure was resolved.
	bad := []string{"localhost:5000", "cache.example.com", "ftp://cache", "http://", ""}
	for _, in := range bad {
		err := checkCacheURL(in)
		if !errors.Is(err, errCacheURL) {
			t.Errorf("checkCacheURL(%q) = %v, want errCacheURL", in, err)
		}
	}
}

// TestDayDurationFlag: "1d" must parse for every duration flag, not only the gc
// ones, and 0 must stay expressible where it means "off".
func TestDayDurationFlag(t *testing.T) {
	for _, tc := range []struct {
		input   string
		want    time.Duration
		invalid bool
	}{
		{"1d", 24 * time.Hour, false},
		{"0", 0, false},
		{"-90m", 0, true},
		{"bogus", 0, true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			fs := ff.NewFlagSet("test")
			d := durationFlag(fs, "age", time.Hour, "")

			err := fs.Parse([]string{"--age", tc.input})
			if tc.invalid {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, *d)
		})
	}
}

func TestPathArgsStdin(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	startTestTask(t, func() {
		fmt.Fprint(pw, "/nix/store/a-one\n\n  /nix/store/b-two  \n")
		pw.Close() // #nosec G104 -- test writer
	})

	stdin := os.Stdin
	os.Stdin = pr

	t.Cleanup(func() {
		os.Stdin = stdin

		pr.Close() // #nosec G104 -- test cleanup
	})

	got, err := pathArgs([]string{"/nix/store/c-three", "-"})
	if err != nil {
		t.Fatalf("pathArgs: %v", err)
	}

	want := []string{"/nix/store/c-three", "/nix/store/a-one", "/nix/store/b-two"}
	if !slices.Equal(got, want) {
		t.Errorf("pathArgs = %v, want %v", got, want)
	}
}

// TestPushDashReadsStdin parses a real command line: ff v4.0.0-beta.1 dropped a
// bare "-" before push ever saw it, which TestPathArgsStdin cannot catch.
func TestPushDashReadsStdin(t *testing.T) {
	const (
		stdinPath = "/nix/store/00000000000000000000000000000000-from-stdin"
		closed    = "http://127.0.0.1:1" // nothing listens: no upload can start
	)

	tests := []struct {
		name    string
		args    []string // after "push --to"
		wantErr error    // nil: stdinPath must reach nix path-info
	}{
		{name: "dash operand", args: []string{closed, "-"}},
		{name: "dash after --", args: []string{closed, "--", "-"}},
		{name: "dash as flag value", args: []string{"-", "/nix/store/x"}, wantErr: errCacheURL},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The fake nix records what push resolves, then fails so nothing uploads.
			dir := t.TempDir()
			record := filepath.Join(dir, "args")
			script := "#!/bin/sh\necho \"$@\" > " + record + "\nexit 1\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "nix"), []byte(script), 0o700)) // #nosec G306 -- fake binary
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

			pr, pw, err := os.Pipe()
			require.NoError(t, err)

			startTestTask(t, func() {
				fmt.Fprintln(pw, stdinPath)
				pw.Close() // #nosec G104 -- test writer
			})

			stdin := os.Stdin
			os.Stdin = pr

			t.Cleanup(func() {
				os.Stdin = stdin

				pr.Close() // #nosec G104 -- test cleanup
			})

			err = newRootCmd().ParseAndRun(t.Context(), append([]string{"push", "--to"}, tt.args...))
			require.Error(t, err)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)

				return
			}

			got, err := os.ReadFile(record) // #nosec G304 -- test temp file
			require.NoError(t, err)
			require.Contains(t, string(got), stdinPath)
		})
	}
}
