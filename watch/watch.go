// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package watch polls the Nix database for new paths,
// then uploads them to a remote cache.
package watch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/kradalby/tsnixcache/upload"

	// sqlite driver registration.
	_ "modernc.org/sqlite"
)

// Sentinel errors returned by Watch.
var (
	errDBUnreadable = errors.New("watch: Nix database unreadable")
)

// pathLogLimit caps the per-path failure warnings of a single upload, matching
// the cap samplePaths puts on the batch line.
const pathLogLimit = 8

// uploadBatch bounds nix path-info arguments and per-batch closure decoding.
// Discoveries and due retries can otherwise grow without limit between polls.
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
	// StateDir holds private durable state, defaulting to the user state directory.
	StateDir  string
	DBPath    string
	StoreDir  string
	TargetURL string
	// Ready runs after the source and durable state are usable.
	Ready                         func() error
	notificationFactoryForTesting func() (*fsnotify.Watcher, error)

	// Report records durable progress before drain I/O and before state closes.
	Report func(Progress) error

	// CopyFn uploads paths (and their closure) to the target, returning transfer
	// stats. Defaults to upload.Closure. Overridable for tests.
	CopyFn func(ctx context.Context, targetURL string, paths []string) (upload.Summary, error)

	// PollInterval controls how often the DB is polled even without fsnotify events.
	// Defaults to 30s.
	PollInterval time.Duration

	// DebounceDelay is how long to wait after the last fsnotify event before
	// polling the DB. Defaults to 500ms.
	DebounceDelay time.Duration

	// IdleExit starts a final drain after this long without delivery progress.
	// Incomplete delivery returns an error and remains durable for restart.
	IdleExit time.Duration

	// DrainTimeout bounds the final drain. Zero means no deadline.
	DrainTimeout time.Duration

	// StallTimeout cancels an upload with no progress for this long (default 60s),
	// so a laptop dropping offline fails fast into the retry queue.
	StallTimeout time.Duration

	// Attempts is the per-upload attempt count within a single poll (default 2);
	// the retry queue provides the longer-term, backed-off retry loop.
	Attempts int

	// Backoff and first failure survive restart. Zero RetryMaxAge disables expiry.
	// RetryQueueSize limits discovery and selection batches; zero uses bounded
	// defaults. Pending overflow remains on disk instead of evicting work.
	RetryBackoffBase time.Duration
	RetryBackoffMax  time.Duration
	RetryMaxAge      time.Duration
	RetryQueueSize   int
}

// Watch opens the DB and store watcher, then runs the event loop until ctx is
// cancelled.
func (w *Watcher) Watch(ctx context.Context) (retErr error) { //nolint:cyclop
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

	p, err := newPoller(ctx, w, copyFn, func() {})
	if err != nil {
		return err
	}
	defer func() {
		reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		retErr = errors.Join(retErr, p.report(reportCtx), p.close())
	}()

	cp, err := p.state.Checkpoint(ctx)
	if err != nil {
		return err
	}

	slog.Info("watch: starting", "storeDir", w.StoreDir, "targetURL", w.TargetURL, "maxID", cp.Cursor)

	n := newNotifications(w)
	defer n.close()

	n.ensure(time.Now())

	// idleTimer fires when no new paths have been found for IdleExit duration.
	// idleC is nil (blocks forever) when IdleExit is not set.
	var idleC <-chan time.Time

	var idleTimer *time.Timer

	resetIdle := func() {}

	if w.IdleExit > 0 {
		idleTimer = time.NewTimer(w.IdleExit)
		defer idleTimer.Stop()

		idleC = idleTimer.C
		resetIdle = func() {
			idleTimer.Reset(w.IdleExit)
		}
	}

	p.resetIdle = resetIdle

	if w.Ready != nil {
		err := w.Ready()
		if err != nil {
			return err
		}
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	debounceTimer := time.NewTimer(debounce)
	debounceTimer.Stop()

	defer debounceTimer.Stop()

	work := make(chan struct{}, 1)
	work <- struct{}{}

	for {
		select {
		case <-ctx.Done():
			return p.finish(ctx)
		case <-idleC:
			slog.Info("watch: idle exit", "after", w.IdleExit)

			return p.finish(ctx)
		case event, ok := <-n.events:
			if !ok {
				n.failed(errNotificationClosed, time.Now())
				debounceTimer.Reset(0)
			} else if n.relevant(event) {
				debounceTimer.Reset(debounce)
			}

			continue
		case eventErr, ok := <-n.errors:
			if !ok {
				eventErr = errNotificationClosed
			}

			n.failed(eventErr, time.Now())
			debounceTimer.Reset(0)

			continue
		case <-n.retry.C:
			n.ensure(time.Now())

			continue
		case <-work:
		case <-debounceTimer.C:
			slog.Debug("watch: debounce fired")
		case <-ticker.C:
			slog.Debug("watch: periodic poll")
			n.ensure(time.Now())
		}

		err = p.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return p.finish(ctx)
			}

			return err
		}

		if p.more {
			select {
			case work <- struct{}{}:
			default:
			}
		}
	}
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
	return newValidPaths(ctx, db, sinceID, math.MaxInt64, pollLimit)
}

func newValidPaths(ctx context.Context, db *sql.DB, sinceID, throughID int64, limit int) ([]ValidPath, int64, error) {
	maxID := sinceID

	rows, err := db.QueryContext(
		ctx,
		`SELECT id, path, COALESCE(narSize, 0) FROM ValidPaths WHERE id > ? AND id <= ? ORDER BY id ASC LIMIT ?`,
		sinceID, throughID, limit,
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
