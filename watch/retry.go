// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package watch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v5"
	"tailscale.com/envknob"

	"github.com/kradalby/tsnixcache/db"
	"github.com/kradalby/tsnixcache/gen/dbsqlc"
	"github.com/kradalby/tsnixcache/store"
	"github.com/kradalby/tsnixcache/upload"
)

// ErrIncomplete means shutdown retained work or recorded terminal failures.
var (
	ErrIncomplete        = errors.New("watch: incomplete drain")
	errMissingCheckpoint = errors.New("watch: existing state has no discovery checkpoint")
)

type copyFunc = func(context.Context, string, []string) (upload.Summary, error)

type poller struct {
	draining      bool
	statePath     string
	w             *Watcher
	db            *sql.DB
	state         *db.DB
	copyFn        copyFunc
	resetIdle     func()
	source        os.FileInfo
	dbFailures    int
	cleanupCursor string
	more          bool
}

func newPoller(ctx context.Context, w *Watcher, copyFn copyFunc, resetIdle func()) (_ *poller, retErr error) {
	path, err := filepath.Abs(w.DBPath)
	if err != nil {
		return nil, err
	}

	storePath, err := filepath.Abs(w.StoreDir)
	if err != nil {
		return nil, err
	}

	stateDir := w.StateDir
	if stateDir == "" {
		stateDir, err = defaultStateDir()
		if err != nil {
			return nil, err
		}
	}

	key := sha256.Sum256([]byte(path + "\x00" + storePath + "\x00" + w.TargetURL))
	p := &poller{w: w, copyFn: copyFn, resetIdle: resetIdle}

	defer func() {
		if retErr != nil {
			_ = p.close()
		}
	}()

	err = p.openSource(ctx)
	if err != nil {
		return nil, err
	}

	baseline, anchor, err := p.latest(ctx)
	if err != nil {
		return nil, err
	}

	p.statePath, err = filepath.Abs(filepath.Join(stateDir, fmt.Sprintf("watch-v1-%x.sqlite", key)))
	if err != nil {
		return nil, err
	}

	state, err := db.Open(ctx, p.statePath)
	if err != nil {
		return nil, err
	}

	p.state = state

	err = p.initializeState(ctx, baseline, anchor)
	if err != nil {
		return nil, err
	}

	return p, nil
}

func defaultStateDir() (string, error) {
	if runtime.GOOS == "darwin" {
		base, err := os.UserConfigDir()

		return filepath.Join(base, "tsnixcache"), err
	}

	if base := envknob.String("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "tsnixcache"), nil
	}

	home, err := os.UserHomeDir()

	return filepath.Join(home, ".local", "state", "tsnixcache"), err
}

func sourceIdentity(info os.FileInfo) string {
	stat := info.Sys().(*syscall.Stat_t)

	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
}

func (p *poller) openSource(ctx context.Context) error {
	info, err := os.Stat(p.w.DBPath)
	if err != nil {
		return fmt.Errorf("%w: stat source: %w", errDBUnreadable, err)
	}

	sourceDB, err := sql.Open("sqlite", store.ReadOnlyDSN(p.w.DBPath))
	if err != nil {
		return fmt.Errorf("%w: open source: %w", errDBUnreadable, err)
	}

	sourceDB.SetMaxOpenConns(1)

	err = sourceDB.PingContext(ctx)
	if err != nil {
		_ = sourceDB.Close()

		return fmt.Errorf("%w: ping source: %w", errDBUnreadable, err)
	}

	if p.db != nil {
		_ = p.db.Close()
	}

	p.db, p.source = sourceDB, info

	return nil
}

func (p *poller) close() error {
	var err error
	if p.db != nil {
		err = p.db.Close()
	}

	if p.state != nil {
		err = errors.Join(err, p.state.Close())
	}

	return err
}

func (p *poller) latest(ctx context.Context) (int64, string, error) {
	var (
		id   int64
		path string
	)

	err := p.db.QueryRowContext(ctx, `SELECT id, path FROM ValidPaths ORDER BY id DESC LIMIT 1`).Scan(&id, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}

	return id, path, err
}

func (p *poller) validateAnchor(ctx context.Context, cp dbsqlc.Checkpoint) error {
	var path string
	if cp.Cursor > 0 {
		err := p.db.QueryRowContext(ctx, `SELECT path FROM ValidPaths WHERE id = ?`, cp.Cursor).Scan(&path)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: validate source anchor: %w", errDBUnreadable, err)
		}
	}

	if cp.Source == sourceIdentity(p.source) && (cp.Cursor == 0 || cp.Anchor == path && path != "") {
		return nil
	}

	slog.Warn("watch: source changed, replaying registrations", "cursor", cp.Cursor)

	return p.state.Transaction(ctx, func(q *dbsqlc.Queries) error {
		if cp.Source != sourceIdentity(p.source) && cp.Boundary.Valid {
			id, _, err := p.latest(ctx)
			if err != nil {
				return err
			}

			err = q.ReconcileBoundary(ctx, sql.NullInt64{Int64: id, Valid: true})
			if err != nil {
				return err
			}
		}

		return q.Advance(ctx, dbsqlc.AdvanceParams{Source: sourceIdentity(p.source)})
	})
}

func (p *poller) refreshSource(ctx context.Context) error {
	info, err := os.Stat(p.w.DBPath)
	if err != nil {
		return fmt.Errorf("%w: stat source: %w", errDBUnreadable, err)
	}

	if !os.SameFile(info, p.source) {
		err = p.openSource(ctx)
		if err != nil {
			return err
		}
	}

	cp, err := p.state.Checkpoint(ctx)
	if err != nil {
		return err
	}

	return p.validateAnchor(ctx, cp)
}

func (p *poller) discover(ctx context.Context) error {
	cp, err := p.state.Checkpoint(ctx)
	if err != nil {
		return err
	}

	if cp.Boundary.Valid && cp.DrainDiscovered != 0 {
		return nil
	}

	upper := int64(math.MaxInt64)
	if cp.Boundary.Valid {
		upper = cp.Boundary.Int64
	}

	paths, cursor, err := newValidPaths(ctx, p.db, cp.Cursor, upper, min(pollLimit, orInt(p.w.RetryQueueSize, pollLimit)))
	if err != nil {
		return errors.Join(errDBUnreadable, err)
	}

	p.more = len(paths) > 0

	return p.state.Transaction(ctx, func(q *dbsqlc.Queries) error {
		for _, path := range paths {
			err := q.Discover(ctx, dbsqlc.DiscoverParams{Path: path.Path, SourceID: path.ID, Source: sourceIdentity(p.source)})
			if err != nil {
				return err
			}
		}

		if len(paths) > 0 {
			err := q.Advance(ctx, dbsqlc.AdvanceParams{
				Cursor: cursor, Anchor: paths[len(paths)-1].Path, Source: sourceIdentity(p.source),
			})
			if err != nil {
				return err
			}
		}

		if cp.Boundary.Valid && len(paths) == 0 {
			return q.Discovered(ctx)
		}

		return nil
	})
}

func (p *poller) poll(ctx context.Context) error {
	p.more = false

	err := p.refreshSource(ctx)
	if err != nil {
		if errors.Is(err, errDBUnreadable) {
			return p.sourceError(err)
		}

		return err
	}

	err = p.discover(ctx)
	if err != nil {
		if errors.Is(err, errDBUnreadable) {
			return p.sourceError(err)
		}

		return fmt.Errorf("watch: persist discoveries: %w", err)
	}

	p.dbFailures = 0
	limit := int64(min(uploadBatch, orInt(p.w.RetryQueueSize, uploadBatch)))

	fresh, err := p.state.Fresh(ctx, limit)
	if err != nil {
		return err
	}

	err = p.attempt(ctx, fresh, 0)
	if err != nil {
		return err
	}

	freshProgress := len(fresh) > 0

	due, err := p.state.Due(ctx, dbsqlc.DueParams{NextAt: time.Now().UnixMilli(), Limit: limit})
	if err != nil {
		return err
	}

	err = p.attempt(ctx, due, 0)
	if err != nil {
		return err
	}

	p.more = p.more || freshProgress || len(due) > 0

	err = p.cleanup(ctx, false)
	if err != nil {
		return err
	}

	err = p.refreshSource(ctx)
	if err != nil {
		return err
	}

	_, err = p.complete(ctx)

	return err
}

func (p *poller) sourceError(err error) error {
	p.dbFailures++
	slog.Warn("watch: read source", "err", err, "consecutive", p.dbFailures)

	if p.dbFailures >= maxPollFailures {
		return fmt.Errorf("%w: %w", errDBUnreadable, err)
	}

	return nil
}

func (p *poller) finish(ctx context.Context) error {
	ctx, cancel := p.drainContext(ctx)
	defer cancel()

	err := p.beginDrain(ctx)
	if err != nil {
		return errors.Join(ErrIncomplete, err)
	}

	p.draining = true

	err = p.report(ctx)
	if err != nil {
		return errors.Join(ErrIncomplete, err)
	}

	limit := int64(min(uploadBatch, orInt(p.w.RetryQueueSize, uploadBatch)))
	for {
		err = p.refreshSource(ctx)
		if err != nil {
			return errors.Join(ErrIncomplete, err)
		}

		err = p.discover(ctx)
		if err != nil {
			return errors.Join(ErrIncomplete, err)
		}

		cp, err := p.state.Checkpoint(ctx)
		if err != nil {
			return errors.Join(ErrIncomplete, err)
		}

		items, err := p.state.Final(ctx, dbsqlc.FinalParams{FinalGeneration: cp.Generation, Limit: limit})
		if err != nil {
			return errors.Join(ErrIncomplete, err)
		}

		if len(items) == 0 && cp.DrainDiscovered != 0 {
			ready, err := p.drainReady(ctx)
			if err != nil {
				return errors.Join(ErrIncomplete, err)
			}

			if ready {
				break
			}

			continue
		}

		err = p.attempt(ctx, items, cp.Generation)
		if err != nil {
			return errors.Join(ErrIncomplete, err)
		}
	}

	complete, err := p.complete(ctx)
	if err != nil {
		return errors.Join(ErrIncomplete, err)
	}

	if !complete {
		pending, countErr := p.state.CountPending(ctx)

		return errors.Join(fmt.Errorf("%w: %d pending", ErrIncomplete, pending), countErr)
	}

	return nil
}

func (p *poller) drainContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx = context.WithoutCancel(ctx)

	if p.w.DrainTimeout > 0 {
		return context.WithTimeout(ctx, p.w.DrainTimeout)
	}

	return ctx, func() {}
}

func (p *poller) complete(ctx context.Context) (bool, error) {
	cp, err := p.state.Checkpoint(ctx)
	if err != nil {
		return false, err
	}

	if !cp.Boundary.Valid || cp.DrainDiscovered == 0 {
		return false, nil
	}

	pending, err := p.state.HasPending(ctx)
	if err != nil || pending != 0 {
		return false, err
	}

	err = p.state.EndDrain(ctx)
	if err != nil {
		return false, err
	}

	if cp.Expired > 0 {
		return true, fmt.Errorf("%w: %d expired", ErrIncomplete, cp.Expired)
	}

	return true, p.state.Maintain(ctx)
}

func (p *poller) attempt(ctx context.Context, items []dbsqlc.Pending, generation int64) error {
	if len(items) == 0 {
		return nil
	}

	active, err := p.prepare(ctx, items, generation)
	if err != nil {
		return err
	}

	if len(active) == 0 {
		return nil
	}

	names := make([]string, 0, len(active))
	for _, it := range active {
		names = append(names, it.Path)
	}

	sum, uploadErr := p.copyFn(ctx, p.w.TargetURL, names)
	if uploadErr != nil && len(sum.Failed) == 0 {
		sum.Failed = names
	}

	failed := make(map[string]bool, len(sum.Failed))
	for _, path := range sum.Failed {
		failed[path] = true
	}

	now := time.Now()

	err = p.state.Transaction(ctx, func(q *dbsqlc.Queries) error {
		for _, it := range active {
			if failed[it.Path] {
				next := now.Add(retryDelay(p.w, it.Attempts+1)).UnixMilli()

				err := q.Retry(ctx, dbsqlc.RetryParams{Path: it.Path, Now: now.UnixMilli(), NextAt: next})
				if err != nil {
					return err
				}
			} else {
				err := q.Retire(ctx, it.Path)
				if err != nil {
					return err
				}
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	slog.Info("watch: uploaded batch",
		"uploaded", sum.Uploaded, "skipped", sum.Skipped,
		"failed", len(sum.Failed), "err", uploadErr, "paths", samplePaths(sum.Failed))

	if len(failed) < len(active) {
		p.resetIdle()
	}

	return p.state.Maintain(ctx)
}

func (p *poller) cleanup(ctx context.Context, all bool) error {
	cursor := p.cleanupCursor
	if all {
		cursor = ""
	}

	limit := int64(min(uploadBatch, orInt(p.w.RetryQueueSize, uploadBatch)))
	for {
		items, err := p.state.PendingPage(ctx, dbsqlc.PendingPageParams{Path: cursor, Limit: limit})
		if err != nil {
			return err
		}

		if len(items) == 0 {
			p.cleanupCursor = ""

			return p.state.Maintain(ctx)
		}

		_, err = p.prepare(ctx, items, 0)
		if err != nil {
			return err
		}

		cursor = items[len(items)-1].Path
		if !all {
			p.cleanupCursor = cursor

			return p.state.Maintain(ctx)
		}
	}
}

// livePaths fails open as a whole on errors, preserving input order and duplicates.
func livePaths(ctx context.Context, source *sql.DB, names []string) []string {
	found := make(map[string]bool, len(names))
	for chunk := range slices.Chunk(names, uploadBatch) {
		paths, err := queryLive(ctx, source, chunk)
		if err != nil {
			slog.Warn("watch: check paths", "err", err)

			return names
		}

		for _, path := range paths {
			found[path] = true
		}
	}

	live := make([]string, 0, len(names))
	for _, name := range names {
		if found[name] {
			live = append(live, name)
		}
	}

	return live
}

func queryLive(ctx context.Context, source *sql.DB, names []string) ([]string, error) {
	args := make([]any, len(names))
	for i, name := range names {
		args[i] = name
	}
	// #nosec G202 -- only placeholder count varies; paths remain parameters.
	query := `SELECT path FROM ValidPaths WHERE path IN (` +
		strings.TrimSuffix(strings.Repeat("?,", len(names)), ",") + `)`

	rows, err := source.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var paths []string

	for rows.Next() {
		var path string

		err = rows.Scan(&path)
		if err != nil {
			return nil, err
		}

		paths = append(paths, path)
	}

	return paths, rows.Err()
}

func retryDelay(w *Watcher, attempts int64) time.Duration {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = orDuration(w.RetryBackoffBase, 30*time.Second)
	b.MaxInterval = orDuration(w.RetryBackoffMax, 10*time.Minute)
	b.InitialInterval = min(b.InitialInterval, b.MaxInterval)
	b.Multiplier = 2
	b.RandomizationFactor = 0.25
	b.Reset()

	var delay time.Duration
	for range min(attempts, 64) {
		delay = b.NextBackOff()
	}

	if delay < 0 {
		return b.MaxInterval
	}

	return delay
}

func orInt(v, fallback int) int {
	if v <= 0 {
		return fallback
	}

	return v
}

func orDuration(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}

	return v
}

func (p *poller) initializeState(ctx context.Context, baseline int64, anchor string) error {
	cp, err := p.state.Checkpoint(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if !p.state.Created {
			return errMissingCheckpoint
		}

		err = p.state.Initialize(ctx, dbsqlc.InitializeParams{
			Cursor: baseline, Anchor: anchor, Source: sourceIdentity(p.source),
		})
		if err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		err = p.validateAnchor(ctx, cp)
		if err != nil {
			return err
		}

		if cp.LastExpired > 0 {
			slog.Warn("watch: previous drain expired paths", "count", cp.LastExpired, "generation", cp.LastGeneration)
		}
	}

	return nil
}

func (p *poller) prepare(ctx context.Context, items []dbsqlc.Pending, generation int64) ([]dbsqlc.Pending, error) {
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.Path
	}

	live := livePaths(ctx, p.db, names)

	liveSet := make(map[string]bool, len(live))
	for _, name := range live {
		liveSet[name] = true
	}

	active := make([]dbsqlc.Pending, 0, len(items))
	now := time.Now()

	err := p.state.Transaction(ctx, func(q *dbsqlc.Queries) error {
		for _, it := range items {
			if !liveSet[it.Path] {
				err := q.Retire(ctx, it.Path)
				if err != nil {
					return err
				}

				continue
			}

			if it.Expired != 0 {
				continue
			}

			if it.FirstFail != 0 && p.w.RetryMaxAge > 0 && now.Sub(time.UnixMilli(it.FirstFail)) >= p.w.RetryMaxAge {
				err := q.MarkExpired(ctx, it.Path)
				if err != nil {
					return err
				}

				err = q.Expire(ctx)
				if err != nil {
					return err
				}

				slog.Warn("watch: giving up on path", "path", it.Path, "after", p.w.RetryMaxAge)

				continue
			}

			if generation > 0 {
				err := q.MarkFinal(ctx, dbsqlc.MarkFinalParams{Path: it.Path, FinalGeneration: generation})
				if err != nil {
					return err
				}
			}

			active = append(active, it)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return active, nil
}

func (p *poller) drainReady(ctx context.Context) (bool, error) {
	err := p.cleanup(ctx, true)
	if err != nil {
		return false, err
	}

	err = p.refreshSource(ctx)
	if err != nil {
		return false, err
	}

	cp, err := p.state.Checkpoint(ctx)
	if err != nil {
		return false, err
	}

	return cp.DrainDiscovered != 0, nil
}

func (p *poller) beginDrain(ctx context.Context) error {
	err := p.refreshSource(ctx)
	if err != nil {
		return err
	}

	id, _, err := p.latest(ctx)
	if err != nil {
		return err
	}

	return p.state.BeginDrain(ctx, id)
}
