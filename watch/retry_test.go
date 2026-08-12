// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package watch

import (
	"slices"
	"testing"
	"time"
)

var epoch = time.Unix(1_700_000_000, 0)

func TestRetryQueueBackoff(t *testing.T) {
	q := newRetryQueue(time.Second, 8*time.Second, time.Hour, 10)

	// First failure schedules base (1s) out, less the jitter.
	if q.fail("a", 1, epoch) {
		t.Fatal("first failure should not give up")
	}

	if got := q.due(epoch); len(got) != 0 {
		t.Errorf("nothing should be due immediately, got %v", got)
	}

	if got := q.due(epoch.Add(2 * time.Second)); len(got) != 1 {
		t.Errorf("path should be due after 1s ± jitter, got %v", got)
	}

	// Backoff doubles: 1s, 2s, 4s, 8s (cap), 8s..., each spread by ±25%.
	wantSecs := []int{2, 4, 8, 8}
	for i, secs := range wantSecs {
		now := epoch.Add(time.Duration(i+2) * time.Second)
		q.fail("a", 1, now)

		want := time.Duration(secs) * time.Second
		lo, hi := want*3/4, want*5/4

		got := q.items["a"].nextAt.Sub(now)
		if got < lo || got > hi {
			t.Errorf("attempt %d: backoff %v, want %v within ±25%% (%v..%v)", i+2, got, want, lo, hi)
		}
	}
}

// TestRetryQueueBackoffJitter: one failed batch stamps every path with the same
// attempt count at the same instant, so without a spread the whole queue comes
// due in a single poll for the entire outage and lands on a cache that is
// already unhealthy.
func TestRetryQueueBackoffJitter(t *testing.T) {
	q := newRetryQueue(time.Minute, time.Hour, time.Hour, 0)

	seen := make(map[time.Time]bool)

	for i := range 20 {
		path := string(rune('a' + i))
		q.fail(path, int64(i+1), epoch)
		seen[q.items[path].nextAt] = true
	}

	if len(seen) < 2 {
		t.Errorf("20 paths failing together got %d distinct retry times, want them spread", len(seen))
	}
}

func TestRetryQueueGiveUp(t *testing.T) {
	q := newRetryQueue(time.Second, time.Minute, 5*time.Second, 10)

	if q.fail("a", 1, epoch) {
		t.Fatal("first failure (age 0) should not give up")
	}

	if !q.fail("a", 1, epoch.Add(6*time.Second)) {
		t.Error("failure past maxAge should give up")
	}

	if q.len() != 0 {
		t.Errorf("given-up path should be removed, len=%d", q.len())
	}
}

// TestRetryQueueZeroDisables pins what the documentation promises: an operator
// who asks for "never give up" or "never evict" gets it, rather than silently
// getting the defaults back.
func TestRetryQueueZeroDisables(t *testing.T) {
	q := newRetryQueue(time.Second, time.Minute, 0, 0)

	q.fail("a", 1, epoch)

	if q.fail("a", 1, epoch.Add(100*time.Hour)) {
		t.Error("zero maxAge gave up on a path")
	}

	for i := range 100 {
		q.fail(string(rune('b'+i)), int64(i+2), epoch)
	}

	if dropped := q.evictOldest(); dropped != nil {
		t.Errorf("zero cap evicted %v", dropped)
	}
}

// TestRetryQueueEvictOldest checks eviction is ordered by ValidPaths id. Nix
// registers a path only after its references, so dropping the low ids drops
// leaves whose surviving roots pull them back in — and unlike the first-failure
// time, the id is distinct for every path in a batch that failed at one instant.
func TestRetryQueueEvictOldest(t *testing.T) {
	q := newRetryQueue(time.Second, time.Minute, time.Hour, 2)

	// Same instant, ids out of order: only the id can order these.
	q.fail("root", 3, epoch)
	q.fail("leaf", 1, epoch)
	q.fail("mid", 2, epoch)

	dropped := q.evictOldest()
	if len(dropped) != 1 || dropped[0].path != "leaf" || dropped[0].id != 1 {
		t.Errorf("should evict the lowest id ('leaf'), got %v", dropped)
	}

	if q.len() != 2 {
		t.Errorf("len after evict = %d, want 2", q.len())
	}
}

func TestRetryQueueSucceedRemoves(t *testing.T) {
	q := newRetryQueue(time.Second, time.Minute, time.Hour, 10)

	q.fail("a", 1, epoch)
	q.succeed("a")

	if q.len() != 0 {
		t.Errorf("succeed should remove path, len=%d", q.len())
	}
}

// TestRetryQueueAllIgnoresBackoff: the shutdown drain has no later poll to
// honour a backoff with, so waiting one out is just dropping the path.
func TestRetryQueueAllIgnoresBackoff(t *testing.T) {
	q := newRetryQueue(time.Hour, time.Hour, time.Hour, 10)

	q.fail("a", 1, epoch)
	q.fail("b", 2, epoch)

	if due := q.due(epoch); len(due) != 0 {
		t.Fatalf("nothing should be due within the backoff, got %v", due)
	}

	if got := q.all(); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("all() = %v, want every queued path", got)
	}
}
