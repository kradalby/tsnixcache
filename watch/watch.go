// Package watch monitors the Nix store directory and database for new paths,
// then copies them to a remote cache via `nix copy`.
package watch

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/fsnotify/fsnotify"
	_ "modernc.org/sqlite"
)

// Watcher holds configuration and state for watching the Nix store.
type Watcher struct {
	DBPath    string
	StoreDir  string
	TargetURL string

	// CopyFn is called to copy paths to the target. Defaults to nixCopy.
	CopyFn func(ctx context.Context, targetURL string, paths []string) error

	// PollInterval controls how often the DB is polled even without fsnotify events.
	// Defaults to 30s.
	PollInterval time.Duration

	// DebounceDelay is how long to wait after the last fsnotify event before
	// polling the DB. Defaults to 500ms.
	DebounceDelay time.Duration
}

// Watch starts watching using top-level package defaults.
func Watch(ctx context.Context, dbPath, storeDir, targetURL string) error {
	return (&Watcher{DBPath: dbPath, StoreDir: storeDir, TargetURL: targetURL}).Watch(ctx)
}

// Watch opens the DB and store watcher, then runs the event loop until ctx is
// cancelled.
func (w *Watcher) Watch(ctx context.Context) error {
	copyFn := w.CopyFn
	if copyFn == nil {
		copyFn = nixCopy
	}
	pollInterval := w.PollInterval
	if pollInterval <= 0 {
		pollInterval = 30 * time.Second
	}
	debounce := w.DebounceDelay
	if debounce <= 0 {
		debounce = 500 * time.Millisecond
	}

	dsn := "file:" + w.DBPath + "?mode=ro&_journal_mode=WAL"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("watch: open db %s: %w", w.DBPath, err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("watch: ping db: %w", err)
	}

	// Establish baseline — capture current maxID without copying anything.
	_, maxID, err := NewValidPaths(db, 0)
	if err != nil {
		return fmt.Errorf("watch: initial poll: %w", err)
	}
	slog.Info("watch: starting", "storeDir", w.StoreDir, "targetURL", w.TargetURL, "maxID", maxID)

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watch: fsnotify: %w", err)
	}
	defer fsw.Close()

	if err := fsw.Add(w.StoreDir); err != nil {
		return fmt.Errorf("watch: watch storeDir %s: %w", w.StoreDir, err)
	}

	// poll polls for new paths and copies them.
	poll := func() {
		paths, newMax, err := NewValidPaths(db, maxID)
		if err != nil {
			slog.Error("watch: poll db", "err", err)
			return
		}
		if len(paths) == 0 {
			return
		}
		maxID = newMax
		slog.Info("watch: copying new paths", "count", len(paths))
		if err := copyFn(ctx, w.TargetURL, paths); err != nil {
			slog.Error("watch: nix copy", "err", err)
		}
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// debounceTimer fires after the debounce delay following the last fsnotify event.
	// It starts stopped; we use Reset to arm it.
	debounceTimer := time.NewTimer(debounce)
	if !debounceTimer.Stop() {
		<-debounceTimer.C
	}

	for {
		select {
		case <-ctx.Done():
			// Drain any pending debounce.
			poll()
			return ctx.Err()

		case event, ok := <-fsw.Events:
			if !ok {
				return fmt.Errorf("watch: fsnotify events channel closed")
			}
			slog.Debug("watch: fs event", "event", event)
			// Reset debounce window.
			if !debounceTimer.Stop() {
				select {
				case <-debounceTimer.C:
				default:
				}
			}
			debounceTimer.Reset(debounce)

		case err, ok := <-fsw.Errors:
			if !ok {
				return fmt.Errorf("watch: fsnotify errors channel closed")
			}
			slog.Warn("watch: fsnotify error", "err", err)

		case <-debounceTimer.C:
			slog.Debug("watch: debounce fired")
			poll()

		case <-ticker.C:
			slog.Debug("watch: periodic poll")
			poll()
		}
	}
}

// NewValidPaths queries ValidPaths for rows with id > sinceID, returning the
// paths and the highest id seen (or sinceID if no rows were found).
func NewValidPaths(db *sql.DB, sinceID int64) (paths []string, maxID int64, err error) {
	maxID = sinceID
	rows, err := db.Query(
		`SELECT id, path FROM ValidPaths WHERE id > ? ORDER BY id ASC`,
		sinceID,
	)
	if err != nil {
		return nil, sinceID, fmt.Errorf("watch: query ValidPaths: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			return nil, sinceID, fmt.Errorf("watch: scan ValidPaths: %w", err)
		}
		paths = append(paths, path)
		if id > maxID {
			maxID = id
		}
	}
	if err := rows.Err(); err != nil {
		return nil, sinceID, fmt.Errorf("watch: iterate ValidPaths: %w", err)
	}
	return paths, maxID, nil
}

// nixCopy runs `nix copy --to targetURL <paths...>`.
func nixCopy(ctx context.Context, targetURL string, paths []string) error {
	args := append([]string{"copy", "--to", targetURL}, paths...)
	cmd := exec.CommandContext(ctx, "nix", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("watch: nix copy: %w\n%s", err, out)
	}
	return nil
}
