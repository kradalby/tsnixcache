// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/mattn/go-isatty"
	"golang.org/x/term"

	"github.com/kradalby/tsnixcache/upload"
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
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	const age = "--age"

	d := durationFlag(fs, "age", 2*time.Hour, "")

	err := fs.Parse([]string{age, "1d"})
	if err != nil {
		t.Fatalf("parse 1d: %v", err)
	}

	if *d != 24*time.Hour {
		t.Errorf("--age 1d = %s, want 24h", *d)
	}

	err = fs.Parse([]string{age, "0"})
	if err != nil || *d != 0 {
		t.Errorf("--age 0 = %s, %v, want 0, nil", *d, err)
	}

	err = fs.Parse([]string{age, "-90m"})
	if err == nil {
		t.Error("--age -90m was accepted, want a parse error")
	}
}

// TestPathArgsStdin: "-" expands to the paths on stdin so a closure can be piped
// in (nix-store -qR … | tsnixcache push --to … -).
func TestPathArgsStdin(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	go func() {
		fmt.Fprint(pw, "/nix/store/a-one\n\n  /nix/store/b-two  \n")
		pw.Close() // #nosec G104 -- test writer
	}()

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

// testRenderer builds a barRenderer drawing to /dev/null.
func testRenderer(t *testing.T) *barRenderer {
	t.Helper()

	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}

	t.Cleanup(func() { _ = devnull.Close() })

	return newBarRenderer(t.Context(), devnull)
}

// waitFor calls r.wait() under a deadline: a bar that is never completed or
// aborted (an orphan) would otherwise block mpb's Wait forever.
func waitFor(t *testing.T, r *barRenderer) []upload.Stats {
	t.Helper()

	ch := make(chan []upload.Stats, 1)
	go func() { ch <- r.wait() }()

	select {
	case failed := <-ch:
		return failed
	case <-time.After(10 * time.Second):
		t.Fatal("wait() did not return — a bar was never completed or aborted (deadlock?)")

		return nil
	}
}

// TestBarRendererLifecycle drives the real mpb glue for a completed and a failed
// path, asserting wait() returns promptly (no deadlock) with the failure stashed.
func TestBarRendererLifecycle(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer devnull.Close() // #nosec G104 -- test cleanup

	r := newBarRenderer(t.Context(), devnull)

	good := "/nix/store/8kvxvr3pmsypxiypq4g8zy13glnfr7nx-good-1"
	bad := "/nix/store/9kvxvr3pmsypxiypq4g8zy13glnfr7nx-bad-1"

	r.start(good, 1000)
	r.start(bad, 2000)
	r.progress(good, 500, 1e6)
	r.progress(bad, 800, 1e6)
	r.done(upload.Stats{Path: good, NarSize: 1000})
	r.done(upload.Stats{Path: bad, NarSize: 2000, Err: errPushToRequired})

	// Progress/done on an unknown path (e.g. a skipped one) must be a no-op.
	r.done(upload.Stats{Path: "/nix/store/never-started", Skipped: true})

	type res struct{ failed []upload.Stats }

	ch := make(chan res, 1)
	go func() { ch <- res{failed: r.wait()} }()

	select {
	case got := <-ch:
		if len(got.failed) != 1 || got.failed[0].Path != bad {
			t.Errorf("wait() failed = %v, want one entry for %s", got.failed, bad)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait() did not return — bars never completed/aborted (deadlock?)")
	}
}

// TestBarRendererStartIsIdempotent: a duplicate start for a path must keep the
// existing bar. Adding a second one leaves the first orphaned — nothing ever
// completes or aborts it — and mpb's Wait then blocks forever, hanging push.
func TestBarRendererStartIsIdempotent(t *testing.T) {
	r := testRenderer(t)

	path := "/nix/store/8kvxvr3pmsypxiypq4g8zy13glnfr7nx-retried-1"

	r.start(path, 1000)
	r.start(path, 1000)
	r.progress(path, 500, 1e6)
	r.done(upload.Stats{Path: path, NarSize: 1000})

	if failed := waitFor(t, r); len(failed) != 0 {
		t.Errorf("wait() failed = %v, want none", failed)
	}
}

// TestBarRendererStartAfterShutdown: Ctrl-C cancels the context the mpb container
// runs on. A start racing that shutdown must be a no-op, not a panic — mpb's
// AddBar/New answer a shut-down container with a panic rather than an error, and
// that panic would kill push in the middle of tidying up.
func TestBarRendererStartAfterShutdown(t *testing.T) {
	r := testRenderer(t)

	r.wait() // shuts the container down, as a cancelled context does

	path := "/nix/store/8kvxvr3pmsypxiypq4g8zy13glnfr7nx-late-1"

	// Recovered here rather than left to crash the binary: a panic in one test
	// takes the whole package down and buries which case failed.
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("start after shutdown panicked: %v", p)
			}
		}()

		r.start(path, 1000)
	}()

	// Nothing may be recorded either: a bar the dead container never serves
	// would be an orphan, and progress/done would dereference it.
	r.mu.Lock()
	_, tracked := r.bars[path]
	r.mu.Unlock()

	if tracked {
		t.Errorf("start after shutdown recorded a bar for %s", path)
	}

	r.progress(path, 500, 1e6)
	r.done(upload.Stats{Path: path, NarSize: 1000})
}

// TestBarRendererReportsUnstartedFailure: a path skipped because a dependency
// failed never starts a transfer, so it has no bar. It must still be reported, or
// progress mode leaves the user with no idea which paths did not make it.
func TestBarRendererReportsUnstartedFailure(t *testing.T) {
	r := testRenderer(t)

	dep := "/nix/store/8kvxvr3pmsypxiypq4g8zy13glnfr7nx-unmet-dep-1"

	r.done(upload.Stats{Path: dep, NarSize: 10, Err: errPushToRequired})

	failed := waitFor(t, r)
	if len(failed) != 1 || failed[0].Path != dep {
		t.Errorf("wait() failed = %v, want one entry for %s", failed, dep)
	}
}

// TestBarRendererZeroSizeNar: a bar for a zero-byte NAR has its total clamped to
// 1, so completing it with the NAR size would leave it short of its total. mpb
// never marks such a bar done and Wait blocks forever — push would hang after a
// successful transfer, with the summary never logged.
func TestBarRendererZeroSizeNar(t *testing.T) {
	r := testRenderer(t)

	path := "/nix/store/8kvxvr3pmsypxiypq4g8zy13glnfr7nx-empty-1"

	r.start(path, 0)
	r.done(upload.Stats{Path: path, NarSize: 0})

	if failed := waitFor(t, r); len(failed) != 0 {
		t.Errorf("wait() failed = %v, want none", failed)
	}
}
