// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package watch monitors the Nix store directory and database for new paths,
// then uploads them to a remote cache.
package watch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/kradalby/tsnixcache/humanise"
	"github.com/kradalby/tsnixcache/store"
	"github.com/kradalby/tsnixcache/upload"

	// sqlite driver registration.
	_ "modernc.org/sqlite"
)

// Sentinel errors returned by Watch.
var (
	errEventsClosed = errors.New("watch: fsnotify events channel closed")
	errErrorsClosed = errors.New("watch: fsnotify errors channel closed")
	errDBUnreadable = errors.New("watch: Nix database unreadable")
)

// drainTimeout bounds the final poll after shutdown is requested.
// ponytail: a fixed budget rather than a flag — it has to stay well under
// systemd's stop timeout (90s by default), and nothing has needed to tune it.
const drainTimeout = 30 * time.Second

// pathLogLimit caps the per-path failure warnings of a single upload, matching
// the cap samplePaths puts on the batch line.
const pathLogLimit = 8

// uploadBatch caps how many paths go into one copyFn call. The union of "every
// path registered since the last poll" and "every retry now due" is otherwise
// unbounded: a mass substitution can register tens of thousands of paths inside
// one tick, and upload puts each one on `nix path-info`'s command line
// (~85 bytes a path against a 2 MiB ARG_MAX) before decoding the closure of all
// of them at once. E2BIG or an OOM there fails the whole batch, which is the
// expensive kind of failure.
const uploadBatch = 256

// pollLimit caps the rows one poll reads. The cursor only advances past what was
// returned, so a catch-up (a rewind after eviction, or a store that grew while
// the watcher was down) is spread over consecutive polls instead of arriving as
// one enormous batch.
const pollLimit = 1000

// maxPollFailures is how many consecutive DB read failures Watch tolerates
// before returning an error. A permanently unreadable database (the WAL sidecar
// gone, permissions changed underneath us, corruption) is otherwise invisible:
// the process ticks forever uploading nothing while systemd sees a healthy unit
// and Restart=on-failure never fires. Exiting non-zero reopens the DB.
const maxPollFailures = 5

// pathResultLogger returns an upload.Options.OnPath that logs why individual
// paths failed; successes are covered by the batch summary. It stops after
// pathLogLimit paths: one failing root drags its whole closure down with it
// (thousands of paths for a system closure), and an unreachable cache repeats
// that on every poll and every backoff cycle until RetryMaxAge — easily enough
// to trip journald's rate limiter and have it discard the batch summary an
// operator actually needs. The returned function may be called concurrently.
func pathResultLogger() func(upload.Stats) {
	var failures atomic.Int64

	return func(s upload.Stats) {
		if s.Err == nil {
			return
		}

		switch n := failures.Add(1); {
		case n <= pathLogLimit:
			slog.Warn("watch: upload path", "path", s.Path, "err", s.Err)
		case n == pathLogLimit+1:
			slog.Warn("watch: further path failures not logged", "limit", pathLogLimit)
		}
	}
}

// Watcher holds configuration and state for watching the Nix store.
type Watcher struct {
	DBPath    string
	StoreDir  string
	TargetURL string

	// CopyFn uploads paths (and their closure) to the target, returning transfer
	// stats. Defaults to upload.Closure. Overridable for tests.
	CopyFn func(ctx context.Context, targetURL string, paths []string) (upload.Summary, error)

	// PollInterval controls how often the DB is polled even without fsnotify events.
	// Defaults to 30s.
	PollInterval time.Duration

	// DebounceDelay is how long to wait after the last fsnotify event before
	// polling the DB. Defaults to 500ms.
	DebounceDelay time.Duration

	// IdleExit, if non-zero, causes Watch to return nil after this duration
	// passes with no new store paths. Useful for CI: the watcher exits cleanly
	// once builds are done and the idle window expires.
	IdleExit time.Duration

	// StallTimeout cancels an upload with no progress for this long (default 60s),
	// so a laptop dropping offline fails fast into the retry queue.
	StallTimeout time.Duration

	// Attempts is the per-upload attempt count within a single poll (default 2);
	// the retry queue provides the longer-term, backed-off retry loop.
	Attempts int

	// Retry queue tuning: exponential backoff from RetryBackoffBase (default 30s)
	// to RetryBackoffMax (default 10m). RetryMaxAge is when to give up on a path
	// that keeps failing and RetryQueueSize is how many paths to hold before
	// evicting the lowest-numbered ones; unlike the two backoffs these two mean
	// what they say at zero — never give up, and never evict. The command line
	// supplies the defaults (2h and 512), so a zero here is a deliberate ask.
	RetryBackoffBase time.Duration
	RetryBackoffMax  time.Duration
	RetryMaxAge      time.Duration
	RetryQueueSize   int
}

// Watch opens the DB and store watcher, then runs the event loop until ctx is
// cancelled.
func (w *Watcher) Watch(ctx context.Context) error { //nolint:cyclop
	copyFn := w.CopyFn
	if copyFn == nil {
		copyFn = func(ctx context.Context, targetURL string, paths []string) (upload.Summary, error) {
			// A fresh logger per upload, so its cap is per upload rather than
			// spent once for the lifetime of the daemon.
			opts := upload.Options{
				StallTimeout: w.StallTimeout,
				Attempts:     orInt(w.Attempts, 2),
				// The batch summary only says how many paths failed; an operator
				// needs the reason for each one.
				OnPath: pathResultLogger(),
			}

			_, sum, err := upload.Closure(ctx, targetURL, paths, opts)

			return sum, err
		}
	}

	pollInterval := orDuration(w.PollInterval, 30*time.Second)
	debounce := orDuration(w.DebounceDelay, 500*time.Millisecond)

	rq := newRetryQueue(
		orDuration(w.RetryBackoffBase, 30*time.Second),
		orDuration(w.RetryBackoffMax, 10*time.Minute),
		w.RetryMaxAge,
		w.RetryQueueSize,
	)

	// One definition of "read another process's Nix DB", shared with store: the
	// pragma syntax is driver-specific, and getting it wrong silently costs the
	// busy_timeout, so every SQLITE_BUSY would fail a poll outright.
	db, err := sql.Open("sqlite", store.ReadOnlyDSN(w.DBPath))
	if err != nil {
		return fmt.Errorf("watch: open db %s: %w", w.DBPath, err)
	}
	defer db.Close()

	err = db.PingContext(ctx)
	if err != nil {
		return fmt.Errorf("watch: ping db: %w", err)
	}

	// Establish baseline — everything already registered is history, not
	// something to push.
	maxID, err := maxValidPathID(ctx, db)
	if err != nil {
		return fmt.Errorf("watch: initial poll: %w", err)
	}

	slog.Info("watch: starting", "storeDir", w.StoreDir, "targetURL", w.TargetURL, "maxID", maxID)

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watch: fsnotify: %w", err)
	}
	defer fsw.Close()

	err = fsw.Add(w.StoreDir)
	if err != nil {
		return fmt.Errorf("watch: watch storeDir %s: %w", w.StoreDir, err)
	}

	// idleTimer fires when no new paths have been found for IdleExit duration.
	// idleC is nil (blocks forever) when IdleExit is not set.
	var idleC <-chan time.Time

	var idleTimer *time.Timer

	resetIdle := func() {}

	if w.IdleExit > 0 {
		idleTimer = time.NewTimer(w.IdleExit)
		defer idleTimer.Stop()

		idleC = idleTimer.C
		// Reset alone is enough. Since Go 1.23 timer channels are unbuffered and
		// Reset guarantees nothing from the previous setting is received after
		// it returns, the Stop-then-drain dance this used to do was dead
		// code: its drain branch could never be taken.
		resetIdle = func() {
			idleTimer.Reset(w.IdleExit)
		}
	}

	p := &poller{w: w, db: db, copyFn: copyFn, rq: rq, resetIdle: resetIdle, maxID: maxID}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// debounceTimer fires after the debounce delay following the last fsnotify event.
	// It starts stopped; we use Reset to arm it.
	//
	// Stop is deliberately not followed by a receive. Since Go 1.23 timer
	// channels are unbuffered and Stop discards any value the timer has already
	// produced, a freshly created timer always reports true from Stop: the
	// old `if !Stop() { <-C }` guard never ran its drain, and had it run it
	// would have blocked forever rather than collected a stale tick. Go 1.27
	// removed the asynctimerchan GODEBUG that restored the old buffered
	// behaviour, so there is no way back to the semantics that idiom was
	// written for.
	debounceTimer := time.NewTimer(debounce)
	debounceTimer.Stop()

	armDebounce := func() {
		debounceTimer.Reset(debounce)
	}

	for {
		select {
		case <-ctx.Done():
			// The drain needs a context of its own: ctx is already cancelled, so
			// on it every upload fails instantly and the queue dies with us.
			p.finish(context.WithoutCancel(ctx))

			// Cancellation is how SIGTERM reaches us: a clean stop, not a
			// failure. Returning ctx.Err() exited 1 and left the systemd unit
			// failed after every restart.
			return nil

		case <-idleC:
			slog.Info("watch: idle exit", "after", w.IdleExit)
			p.finish(ctx)

			return nil

		case event, ok := <-fsw.Events:
			if !ok {
				return errEventsClosed
			}

			slog.Debug("watch: fs event", "event", event)
			armDebounce()

		case err, ok := <-fsw.Errors:
			if !ok {
				return errErrorsClosed
			}

			slog.Warn("watch: fsnotify error", "err", err)

			// An overflow means the kernel dropped events on the floor, so the
			// paths behind them are exactly the ones now unaccounted for.
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				armDebounce()
			}

		case <-debounceTimer.C:
			slog.Debug("watch: debounce fired")

			err = p.poll(ctx)
			if err != nil {
				return err
			}

		case <-ticker.C:
			slog.Debug("watch: periodic poll")
			w.ensureWatch(fsw)

			err = p.poll(ctx)
			if err != nil {
				return err
			}
		}
	}
}

// ensureWatch re-adds StoreDir if the watch has gone. On IN_IGNORED or
// IN_UNMOUNT — the store unmounted or moved — fsnotify removes the watch
// internally and reports it as an event with no bits set, which its own sender
// then drops: nothing arrives on Events or Errors, and the watcher silently
// degrades to the poll ticker forever.
func (w *Watcher) ensureWatch(fsw *fsnotify.Watcher) {
	if slices.Contains(fsw.WatchList(), w.StoreDir) {
		return
	}

	err := fsw.Add(w.StoreDir)
	if err != nil {
		slog.Warn("watch: store watch lost, re-adding failed", "storeDir", w.StoreDir, "err", err)

		return
	}

	slog.Warn("watch: store watch lost, re-added", "storeDir", w.StoreDir)
}

// copyFunc uploads paths and their closure to a target, returning transfer stats.
type copyFunc = func(ctx context.Context, targetURL string, paths []string) (upload.Summary, error)

// poller holds the mutable state of the poll loop: the DB cursor, the retry
// queue and the consecutive-failure count, driven once per debounce/tick by poll.
type poller struct {
	w          *Watcher
	db         *sql.DB
	copyFn     copyFunc
	rq         *retryQueue
	resetIdle  func()
	maxID      int64
	dbFailures int
}

// poll uploads newly-registered store paths plus the retries whose backoff has
// elapsed.
func (p *poller) poll(ctx context.Context) error {
	return p.run(ctx, p.rq.due(time.Now()))
}

// drain is the last poll before Watch returns, so it ignores backoff: every
// queued path gets one more attempt rather than being scheduled into a future
// that will not happen.
func (p *poller) drain(ctx context.Context) error {
	return p.run(ctx, p.rq.all())
}

// finish makes that last attempt within drainTimeout and then says what is
// still owed. Whatever remains queued is genuinely lost — the queue is
// in-memory and a restart's baseline starts at the current max id — so both
// the journal and CI need to see it.
func (p *poller) finish(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, drainTimeout)
	defer cancel()

	err := p.drain(ctx)
	if err != nil {
		slog.Error("watch: shutdown drain", "err", err)
	}

	if n := p.rq.len(); n > 0 {
		slog.Warn("watch: exiting with paths still queued", "count", n, "paths", samplePaths(p.rq.all()))
	}
}

// run uploads the new paths found since the cursor plus the given retries. The
// cursor advances past every discovered path, so new paths are never blocked by
// a stuck one; failures go to the bounded retry queue instead. It returns an
// error only once the DB has been unreadable for maxPollFailures polls running,
// which is Watch's cue to exit and be restarted.
func (p *poller) run(ctx context.Context, retries []string) error {
	paths, newMax, err := NewValidPaths(ctx, p.db, p.maxID)
	if err != nil {
		p.dbFailures++

		slog.Error("watch: poll db", "err", err, "consecutive", p.dbFailures)

		if p.dbFailures >= maxPollFailures {
			return fmt.Errorf("%w after %d polls: %w", errDBUnreadable, p.dbFailures, err)
		}

		return nil
	}

	p.dbFailures = 0

	// Union of newly-registered paths and the retries handed in, keyed by the
	// ValidPaths id each was discovered at (0 for a retry queued before a
	// rewind, whose id is already recorded in the queue).
	ids := make(map[string]int64, len(paths)+len(retries))
	for _, vp := range paths {
		ids[vp.Path] = vp.ID

		slog.Debug("watch: path", "path", vp.Path, "size", vp.NarSize)
	}

	for _, path := range retries {
		if _, seen := ids[path]; !seen {
			ids[path] = 0
		}
	}

	// Advance past the new paths regardless of upload outcome; failures are
	// tracked in the retry queue, not by holding the cursor. Eviction is the one
	// exception and rewinds it back (see evict).
	p.maxID = newMax

	if len(ids) == 0 {
		return nil
	}

	names := p.live(ctx, slices.Sorted(maps.Keys(ids)))
	if len(names) == 0 {
		return nil
	}

	slog.Info("watch: uploading",
		"count", len(names), "new", len(paths), "retry", len(retries), "paths", samplePaths(names))

	for chunk := range slices.Chunk(names, uploadBatch) {
		sum, err := p.copyFn(ctx, p.w.TargetURL, chunk)
		if err != nil {
			// A batch-level failure (anything that fails before per-path results
			// exist) leaves Failed empty. Without this every path would reconcile as
			// a success, dropping out of the retry queue never to be pushed again.
			if len(sum.Failed) == 0 {
				sum.Failed = chunk
			}

			slog.Warn("watch: upload", "err", err, "failed", len(sum.Failed), "paths", samplePaths(sum.Failed))
		}

		// Back off from when the attempt finished, not when the poll started: a long
		// upload would otherwise eat the backoff it was meant to wait.
		p.reconcile(chunk, ids, sum, time.Now())
	}

	return nil
}

// live drops the paths the Nix DB no longer knows, and clears them out of the
// retry queue. `nix path-info -r --json` exits non-zero and prints nothing when
// any single root is invalid, so one path garbage-collected between polls fails
// the entire batch: every path in it then backs off in lockstep, batches again,
// meets the same dead path again, and nothing is uploaded until it ages out.
func (p *poller) live(ctx context.Context, names []string) []string {
	stmt, err := p.db.PrepareContext(ctx, `SELECT 1 FROM ValidPaths WHERE path = ?`)
	if err != nil {
		// A filter we cannot run is no reason to skip the uploads.
		slog.Warn("watch: check paths", "err", err)

		return names
	}
	defer stmt.Close()

	live := make([]string, 0, len(names))

	var gone []string

	for _, path := range names {
		var one int

		err := stmt.QueryRowContext(ctx, path).Scan(&one)

		switch {
		case err == nil:
			live = append(live, path)
		case errors.Is(err, sql.ErrNoRows):
			gone = append(gone, path)
			p.rq.succeed(path)
		default:
			slog.Warn("watch: check path", "path", path, "err", err)

			live = append(live, path)
		}
	}

	if len(gone) > 0 {
		slog.Info("watch: dropping paths no longer in the store", "count", len(gone), "paths", samplePaths(gone))
	}

	return live
}

// reconcile updates the retry queue from an upload result and logs the batch.
func (p *poller) reconcile(names []string, ids map[string]int64, sum upload.Summary, now time.Time) {
	failedSet := make(map[string]struct{}, len(sum.Failed))
	for _, path := range sum.Failed {
		failedSet[path] = struct{}{}
	}

	for _, path := range names {
		if _, bad := failedSet[path]; bad {
			if p.rq.fail(path, ids[path], now) {
				slog.Warn("watch: giving up on path", "path", path, "after", p.rq.maxAge)
			}

			continue
		}

		// Not named in Failed means delivered. That holds because upload records
		// every closure member exactly once and fails a path whose dependency
		// failed, so a requested root never succeeds silently under a broken
		// closure; run covers the batch-level error above.
		p.rq.succeed(path)
	}

	p.evict()

	avg := 0.0
	if sum.Wall > 0 {
		avg = float64(sum.WireBytes) / sum.Wall.Seconds()
	}

	slog.Info(
		"watch: uploaded batch",
		"uploaded", sum.Uploaded,
		"skipped", sum.Skipped,
		"failed", len(sum.Failed),
		"retrying", p.rq.len(),
		"bytes", humanise.Bytes(sum.NarBytes),
		"dur", sum.Wall.Round(time.Millisecond),
		"avg_upload", humanise.Bitrate(avg),
		"peak_upload", humanise.Bitrate(sum.PeakBps),
	)

	// resetIdle fires on progress (anything that wasn't a failure), so a
	// persistently failing batch still idle-exits.
	if sum.Uploaded > 0 || sum.Skipped > 0 {
		p.resetIdle()
	}
}

// evict enforces the queue's size cap. The queue is the only record that a path
// still owes an upload — the cursor has already moved past it — so dropping an
// entry outright loses that path until someone pushes it by hand. Rewinding the
// cursor below the lowest id dropped hands the record back to the database: the
// next poll rediscovers them, and re-uploading anything that did land is cheap
// because upload skips a path already present.
func (p *poller) evict() {
	dropped := p.rq.evictOldest()
	if len(dropped) == 0 {
		return
	}

	rewind := p.maxID
	paths := make([]string, 0, len(dropped))

	for _, ev := range dropped {
		paths = append(paths, ev.path)

		if ev.id > 0 && ev.id-1 < rewind {
			rewind = ev.id - 1
		}
	}

	p.maxID = rewind

	slog.Warn("watch: retry queue full, rediscovering oldest",
		"count", len(dropped), "cursor", rewind, "paths", samplePaths(paths))
}

// ValidPath is a store path and its uncompressed NAR size, as recorded in the
// Nix database, along with the row id it was found at.
type ValidPath struct {
	ID      int64
	Path    string
	NarSize int64
}

// NewValidPaths queries ValidPaths for rows with id > sinceID, returning the
// paths and the highest id seen (or sinceID if no rows were found). At most
// pollLimit rows come back per call; since the caller's cursor advances only
// past what was returned, the next poll continues where this one stopped.
func NewValidPaths(ctx context.Context, db *sql.DB, sinceID int64) ([]ValidPath, int64, error) {
	maxID := sinceID

	rows, err := db.QueryContext(
		ctx,
		`SELECT id, path, COALESCE(narSize, 0) FROM ValidPaths WHERE id > ? ORDER BY id ASC LIMIT ?`,
		sinceID, pollLimit,
	)
	if err != nil {
		return nil, sinceID, fmt.Errorf("watch: query ValidPaths: %w", err)
	}
	defer rows.Close()

	var paths []ValidPath

	for rows.Next() {
		var vp ValidPath

		err = rows.Scan(&vp.ID, &vp.Path, &vp.NarSize)
		if err != nil {
			return nil, sinceID, fmt.Errorf("watch: scan ValidPaths: %w", err)
		}

		paths = append(paths, vp)

		if vp.ID > maxID {
			maxID = vp.ID
		}
	}

	err = rows.Err()
	if err != nil {
		return nil, sinceID, fmt.Errorf("watch: iterate ValidPaths: %w", err)
	}

	return paths, maxID, nil
}

// maxValidPathID returns the highest id in ValidPaths, which is where the
// watcher starts. Reading the one number rather than every row matters: the
// baseline wants nothing but the cursor, and a real store hands back a hundred
// thousand rows to be thrown away.
func maxValidPathID(ctx context.Context, db *sql.DB) (int64, error) {
	var maxID int64

	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM ValidPaths`).Scan(&maxID)
	if err != nil {
		return 0, fmt.Errorf("watch: max ValidPaths id: %w", err)
	}

	return maxID, nil
}

// samplePaths returns up to a cap of full store paths so a batch is identifiable
// in the INFO log without enabling debug. Larger batches are truncated with a
// "+N more" sentinel to keep the log line bounded.
func samplePaths(paths []string) []string {
	const limit = 8

	if len(paths) <= limit {
		return paths
	}

	return append(paths[:limit:limit], fmt.Sprintf("+%d more", len(paths)-limit))
}
