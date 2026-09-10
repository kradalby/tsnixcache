// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"os"
	"testing"
	"time"

	"github.com/kradalby/tsnixcache/upload"
)

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

	startTestTask(t, func() { ch <- r.wait() })

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

	startTestTask(t, func() { ch <- res{failed: r.wait()} })

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
