// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"

	"github.com/kradalby/tsnixcache/humanise"
	"github.com/kradalby/tsnixcache/upload"
)

// barRenderer draws one live mpb bar per in-flight upload. Its start/progress/
// done methods satisfy upload.Options' OnStart/OnProgress/OnPath callbacks and
// are safe to call concurrently. Bars render to out (stderr for push), leaving
// stdout clean.
type barRenderer struct {
	p *mpb.Progress

	mu   sync.Mutex
	bars map[string]*barState

	failMu sync.Mutex
	failed []upload.Stats
}

type barState struct {
	bar   *mpb.Bar
	total int64        // the bar's own total, which done must reach to complete it
	bps   atomic.Int64 // last wire rate in bytes/s, read by the rate decorator
}

func newBarRenderer(ctx context.Context, out *os.File) *barRenderer {
	return &barRenderer{
		p:    mpb.NewWithContext(ctx, mpb.WithOutput(out)),
		bars: make(map[string]*barState),
	}
}

// start adds a bar for a path whose transfer is beginning. It is idempotent: a
// path that already has a bar keeps it, because replacing it would orphan the
// first bar — nothing would ever complete or abort it, and wait() would block
// forever.
func (r *barRenderer) start(path string, narSize int64) {
	r.mu.Lock()
	_, ok := r.bars[path]
	r.mu.Unlock()

	if ok {
		return
	}

	st := &barState{total: max(narSize, 1)}

	// Add outside r.mu: it is an unbuffered handover to mpb's serve goroutine,
	// so holding the lock across it would let a slow terminal block every
	// in-flight upload's progress callback.
	//
	// Add, not AddBar: AddBar panics once the container has shut down, which is
	// exactly what Ctrl-C does (it cancels the context the container runs on).
	bar, err := r.p.Add(
		st.total,
		mpb.BarStyle().Build(),
		mpb.PrependDecorators(
			decor.Name(storeName(path), decor.WCSyncSpaceR),
			decor.Percentage(decor.WC{W: 5}),
		),
		mpb.AppendDecorators(
			decor.CountersKibiByte("% .1f / % .1f", decor.WCSyncSpace),
			decor.Any(func(decor.Statistics) string {
				return humanise.Bitrate(float64(st.bps.Load()))
			}, decor.WCSyncSpace),
			decor.Elapsed(decor.ET_STYLE_MMSS, decor.WCSyncSpace),
		),
	)
	if err != nil {
		return // container gone (interrupted): there is nothing left to draw on
	}

	st.bar = bar

	r.mu.Lock()
	defer r.mu.Unlock()

	// Lost a race with another start for the same path: drop this bar (Abort
	// with drop, so it neither shows nor keeps Wait blocked) and keep the first.
	if _, ok := r.bars[path]; ok {
		bar.Abort(true)

		return
	}

	r.bars[path] = st
}

// progress advances a path's bar to the uncompressed bytes handed to the
// compressor, which is all the callback knows; the wire trails it, so a small
// path can show 100% while its last buffers are still draining.
func (r *barRenderer) progress(path string, sent int64, wireBps float64) {
	r.mu.Lock()
	st := r.bars[path]
	r.mu.Unlock()

	if st == nil {
		return
	}

	st.bps.Store(int64(wireBps))
	st.bar.SetCurrent(sent)
}

// done finalises a path's bar: complete on success, aborted (left visible) on
// failure. Skipped paths never started a bar, so they are ignored here and only
// show up in the summary. Failures are stashed to log after rendering stops.
func (r *barRenderer) done(s upload.Stats) {
	r.mu.Lock()
	st := r.bars[s.Path]
	r.mu.Unlock()

	// A path skipped because a dependency failed never started a transfer, so it
	// has no bar — but it must still be reported, or it disappears without a
	// trace in progress mode while the plain-log mode names it.
	if s.Err != nil {
		if st != nil {
			st.bar.Abort(false)
		}

		r.failMu.Lock()
		r.failed = append(r.failed, s)
		r.failMu.Unlock()

		return
	}

	if st == nil {
		return
	}

	// Leave the average on the finished bar: the last sample is usually taken
	// after the final flush and reads 0 bit/s, which looks like a stall.
	st.bps.Store(int64(s.AvgBps))

	// The bar's own total, not s.NarSize: mpb only marks a bar done once current
	// reaches total, and Wait never returns while any bar is unfinished. A
	// zero-size NAR (total clamped to 1) would hang push after a successful
	// transfer.
	st.bar.SetCurrent(st.total)
}

// wait blocks until every bar has completed or aborted, then returns the failed
// paths so the caller can log them below the finished bars (logging mid-render
// would corrupt the display).
func (r *barRenderer) wait() []upload.Stats {
	r.p.Wait()

	r.failMu.Lock()
	defer r.failMu.Unlock()

	return r.failed
}

// storeName is a store path's derivation name — the basename with the 32-char
// hash prefix and its dash removed, used as a compact bar label.
func storeName(path string) string {
	base := filepath.Base(path)
	if len(base) > 33 && base[32] == '-' {
		return base[33:]
	}

	return base
}
