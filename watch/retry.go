// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package watch

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"strings"
	"time"
)

// retryItem tracks a path awaiting re-upload after a failure. id is the
// ValidPaths row the path was discovered at, kept so that eviction can hand the
// path back to the poller's cursor instead of losing it.
type retryItem struct {
	id        int64
	attempts  int
	firstFail time.Time
	nextAt    time.Time
}

// evicted is a queue entry dropped by the size cap: the path, and the row the
// poller must rewind its cursor below to rediscover it.
type evicted struct {
	path string
	id   int64
}

// retryQueue holds paths whose upload failed and schedules bounded, backed-off
// retries. It is not safe for concurrent use — the watcher drives it from a
// single goroutine. All methods take the current time so the queue is
// deterministic under test. A zero maxAge never gives up; a zero cap never
// evicts.
type retryQueue struct {
	base   time.Duration // first backoff interval
	max    time.Duration // cap on any single backoff interval
	maxAge time.Duration // give up on a path that has been failing for at least this long
	cap    int           // max entries before the oldest are evicted
	items  map[string]*retryItem
}

func newRetryQueue(base, maxInterval, maxAge time.Duration, capacity int) *retryQueue {
	return &retryQueue{
		base:   base,
		max:    maxInterval,
		maxAge: maxAge,
		cap:    capacity,
		items:  make(map[string]*retryItem),
	}
}

func (q *retryQueue) len() int { return len(q.items) }

// due returns the paths whose next retry time has arrived, in path order so a
// batch is chunked the same way twice.
func (q *retryQueue) due(now time.Time) []string {
	var out []string

	for p, it := range q.items {
		if !it.nextAt.After(now) {
			out = append(out, p)
		}
	}

	slices.Sort(out)

	return out
}

// all returns every queued path, backoff or not. The shutdown drain uses it:
// with no later poll to honour it, waiting out a backoff is just dropping the
// path.
func (q *retryQueue) all() []string {
	out := make([]string, 0, len(q.items))
	for p := range q.items {
		out = append(out, p)
	}

	slices.Sort(out)

	return out
}

// fail records a failed attempt for path, scheduling the next retry with
// exponential backoff. id is the ValidPaths row it was discovered at, or 0 if
// this poll did not see it in the DB. It returns true if the path has been
// failing for at least maxAge and was therefore dropped (the caller should warn).
func (q *retryQueue) fail(path string, id int64, now time.Time) bool {
	it := q.items[path]
	if it == nil {
		it = &retryItem{firstFail: now}
		q.items[path] = it
	}

	if id > 0 {
		it.id = id
	}

	it.attempts++

	if q.maxAge > 0 && now.Sub(it.firstFail) >= q.maxAge {
		delete(q.items, path)

		return true
	}

	it.nextAt = now.Add(q.backoff(it.attempts))

	return false
}

// succeed removes a path from the queue.
func (q *retryQueue) succeed(path string) { delete(q.items, path) }

// evictOldest drops entries past the size cap, lowest ValidPaths id first, and
// returns them so the caller can rediscover and log them. Lowest id first is
// deliberate on two counts: a whole batch fails at one timestamp, so the
// first-failure time cannot order it and the survivors would be arbitrary; and
// nix registers a path only after its references, so the low ids are leaves
// whose surviving roots pull them back in.
func (q *retryQueue) evictOldest() []evicted {
	if q.cap <= 0 || len(q.items) <= q.cap {
		return nil
	}

	all := make([]evicted, 0, len(q.items))
	for p, it := range q.items {
		all = append(all, evicted{path: p, id: it.id})
	}

	slices.SortFunc(all, func(a, b evicted) int {
		return cmp.Or(cmp.Compare(a.id, b.id), strings.Compare(a.path, b.path))
	})

	dropped := all[:len(all)-q.cap]
	for _, e := range dropped {
		delete(q.items, e.path)
	}

	return dropped
}

// backoff returns the delay for the nth attempt: base·2^(n-1), capped at max and
// spread by ±25%. The spread matters as much as the curve: one failed batch
// stamps every path with the same attempt count and the same instant, so without
// jitter the entire queue comes due in a single poll for the whole outage and
// lands on a cache that is already unhealthy.
func (q *retryQueue) backoff(attempts int) time.Duration {
	d := q.base

	for i := 1; i < attempts && d < q.max; i++ {
		d *= 2
	}

	if d > q.max {
		d = q.max
	}

	//nolint:gosec // spreading retries, not generating secrets
	return time.Duration(float64(d) * (0.75 + 0.5*rand.Float64()))
}

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}

	return v
}

func orDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}

	return v
}
