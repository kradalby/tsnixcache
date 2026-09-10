// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package watch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/kradalby/tsnixcache/upload"

	_ "modernc.org/sqlite"
)

// openTestDB creates an in-memory SQLite DB with the minimal ValidPaths schema.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}

	_, err = db.ExecContext(context.Background(), `CREATE TABLE ValidPaths (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		path    TEXT NOT NULL UNIQUE,
		narSize INTEGER
	)`)
	if err != nil {
		db.Close() // #nosec G104 -- closing on error path; original error is primary
		t.Fatalf("create schema: %v", err)
	}

	t.Cleanup(func() {
		db.Close() // #nosec G104 -- test cleanup; errors not actionable
	})

	return db
}

// insertPaths inserts path rows into the test DB.
func insertPaths(t *testing.T, db *sql.DB, paths ...string) {
	t.Helper()
	insertPathsContext(t.Context(), t, db, paths...)
}

func insertPathsContext(ctx context.Context, t *testing.T, db *sql.DB, paths ...string) {
	t.Helper()

	// Publish each fixture batch once, without a durable commit for every row.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)

	defer tx.Rollback()

	for _, p := range paths {
		_, err = tx.ExecContext(ctx, `INSERT INTO ValidPaths (path) VALUES (?)`, p)
		require.NoError(t, err, "insert path %q", p)
	}

	require.NoError(t, tx.Commit())
}

// openTestFileDB creates a temp SQLite file DB with the ValidPaths schema.
func openTestFileDB(t *testing.T) (*sql.DB, string) {
	t.Helper()

	return openTestFileDBContext(t.Context(), t)
}

func openTestFileDBContext(ctx context.Context, t *testing.T) (*sql.DB, string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "db.sqlite")

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open file db: %v", err)
	}

	_, err = db.ExecContext(ctx, `CREATE TABLE ValidPaths (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		path    TEXT NOT NULL UNIQUE,
		narSize INTEGER
	)`)
	if err != nil {
		db.Close() // #nosec G104 -- closing on error path; original error is primary
		t.Fatalf("create schema: %v", err)
	}

	t.Cleanup(func() {
		db.Close() // #nosec G104 -- test cleanup; errors not actionable
	})

	return db, path
}

func TestNewValidPaths_Empty(t *testing.T) {
	db := openTestDB(t)

	paths, maxID, err := NewValidPaths(context.Background(), db, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(paths) != 0 {
		t.Errorf("expected 0 paths, got %d: %v", len(paths), paths)
	}

	if maxID != 0 {
		t.Errorf("expected maxID=0, got %d", maxID)
	}
}

func TestNewValidPaths_SinceID(t *testing.T) {
	db := openTestDB(t)
	insertPaths(t, db, "/nix/store/aaa", "/nix/store/bbb", "/nix/store/ccc")

	paths, maxID, err := NewValidPaths(context.Background(), db, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d: %v", len(paths), paths)
	}

	if paths[0].Path != "/nix/store/bbb" || paths[1].Path != "/nix/store/ccc" {
		t.Errorf("unexpected paths: %v", paths)
	}

	if maxID != 3 {
		t.Errorf("expected maxID=3, got %d", maxID)
	}
}

func TestNewValidPaths_NoneNew(t *testing.T) {
	db := openTestDB(t)
	insertPaths(t, db, "/nix/store/aaa", "/nix/store/bbb")

	paths, maxID, err := NewValidPaths(context.Background(), db, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(paths) != 0 {
		t.Errorf("expected 0 paths, got %d: %v", len(paths), paths)
	}

	if maxID != 5 {
		t.Errorf("expected maxID=5 (sinceID), got %d", maxID)
	}
}

// syncBuffer is a log sink safe to read while Watch is still logging from its
// own goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buf.Reset()
}

// captureLogs redirects the default logger into a buffer for the test. It
// mutates the process-wide default logger, so no test using it may be parallel.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()

	buf := &syncBuffer{}
	prev := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return buf
}

// fakeNix puts a `nix` ahead of the real one on PATH, answering
// `path-info -r --json` with a fixed closure. It lets a test drive the real
// upload path — and everything Watch wires into it — without a Nix store.
func fakeNix(t *testing.T, paths []string) {
	t.Helper()

	type meta struct {
		Path       string   `json:"path"`
		NarSize    int64    `json:"narSize"`
		References []string `json:"references"`
	}

	closure := make(map[string]meta, len(paths))
	for _, p := range paths {
		closure[p] = meta{Path: p, NarSize: 1, References: []string{}}
	}

	blob, err := json.Marshal(closure)
	require.NoError(t, err)

	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "closure.json")
	require.NoError(t, os.WriteFile(jsonPath, blob, 0o600))

	// Resolve cat here rather than leaving it to the script: an unresolvable cat
	// exits 127, which surfaces as "nix path-info: exit status 127" and blames
	// the watcher's wiring for a missing coreutils.
	cat, err := exec.LookPath("cat")
	require.NoError(t, err)

	script := "#!/bin/sh\nexec " + cat + " " + jsonPath + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nix"), []byte(script), 0o700)) // #nosec G306 -- must be executable

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// findLog returns the first captured log line carrying the given message.
func findLog(out, msg string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, `msg="`+msg+`"`) {
			return line
		}
	}

	return ""
}

// startHandler closes ready when Watch logs that it has started.
type startHandler struct {
	slog.Handler

	once  sync.Once
	ready chan struct{}
}

// Enabled overrides the discarding handler's "nothing is enabled", which would
// stop Handle from ever being called.
func (h *startHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *startHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Message == "watch: starting" {
		h.once.Do(func() { close(h.ready) })
	}

	return h.Handler.Handle(ctx, rec)
}

// watchStarted returns a channel closed once Watch has logged its start line,
// which is the first observable point after it records its baseline maxID.
// Anything inserted after that is guaranteed to be seen by a later poll.
func watchStarted(t *testing.T) <-chan struct{} {
	t.Helper()

	prev := slog.Default()
	ready := make(chan struct{})

	// Wrap a discarding handler, never slog's default one: the default routes
	// records back through the log package, which slog.SetDefault has just
	// pointed at us, and the first record then deadlocks on the log mutex.
	slog.SetDefault(slog.New(&startHandler{Handler: slog.DiscardHandler, ready: ready}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return ready
}

// TestGracefulShutdown checks that a cancelled context is a clean stop: the
// final drain still uploads what is pending (so it needs a live context of its
// own) and Watch returns nil rather than leaving the unit failed.
func TestGracefulShutdown(t *testing.T) {
	db, dbPath := openTestFileDB(t)
	storeDir := t.TempDir()

	ready := watchStarted(t)

	var (
		mu  sync.Mutex
		got []string
	)

	copyFn := func(ctx context.Context, _ string, paths []string) (upload.Summary, error) {
		// The drain must run on a live context, not the cancelled one.
		err := ctx.Err()
		if err != nil {
			return upload.Summary{}, err
		}

		mu.Lock()
		defer mu.Unlock()

		got = append(got, paths...)

		return upload.Summary{Uploaded: len(paths)}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())

	w := &Watcher{
		StateDir:  filepath.Join(t.TempDir(), "state"),
		DBPath:    dbPath,
		StoreDir:  storeDir,
		TargetURL: "file:///tmp/tsnixcache-test-shutdown",
		CopyFn:    copyFn,
		// Long enough that only the shutdown drain can pick the path up.
		PollInterval:  time.Hour,
		DebounceDelay: time.Hour,
	}

	done := make(chan error, 1)

	startTestTask(t, func() { done <- w.Watch(ctx) })

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not start")
	}

	insertPaths(t, db, "/nix/store/aaa-drain")
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Watch returned %v, want nil on a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return after context cancellation")
	}

	mu.Lock()
	defer mu.Unlock()

	if !slices.Contains(got, "/nix/store/aaa-drain") {
		t.Errorf("shutdown drain uploaded %v, want it to include /nix/store/aaa-drain", got)
	}
}

// TestDebounceCoalesces checks that a burst of fsnotify events produces one
// poll, not one per event. It counts the debounce firings rather than the
// copyFn calls: only the first poll of a burst finds the new row, so a call
// counter sits at 1 however often the debounce fires and passes with the
// debounce set to a nanosecond.
func TestDebounceCoalesces(t *testing.T) {
	logs := captureLogs(t)
	db, dbPath := openTestFileDB(t)
	storeDir := t.TempDir()

	var (
		mu       sync.Mutex
		allPaths []string
	)

	copyFn := func(_ context.Context, _ string, paths []string) (upload.Summary, error) {
		mu.Lock()
		defer mu.Unlock()

		allPaths = append(allPaths, paths...)

		return upload.Summary{Uploaded: len(paths)}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())

	w := &Watcher{
		StateDir:      filepath.Join(t.TempDir(), "state"),
		DBPath:        dbPath,
		StoreDir:      storeDir,
		TargetURL:     "file:///tmp/tsnixcache-test-debounce",
		CopyFn:        copyFn,
		PollInterval:  10 * time.Second, // long; we rely on fsnotify events
		DebounceDelay: 100 * time.Millisecond,
	}

	watchDone := make(chan error, 1)

	startTestTask(t, func() { watchDone <- w.Watch(ctx) })

	t.Cleanup(func() {
		cancel()
		<-watchDone
	})

	// Insert only once Watch has recorded its baseline maxID, or the path is
	// part of the baseline and never uploaded.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.NotEmpty(c, findLog(logs.String(), "watch: starting"))
	}, 10*time.Second, 10*time.Millisecond)

	insertPaths(t, db, "/nix/store/aaa-debounce")

	// Trigger many rapid fsnotify events; one debounce window should swallow them all.
	for range 10 {
		f := dbPath + "-wal"
		_ = os.WriteFile(f, []byte("x"), 0o600)
		_ = os.Remove(f)
	}

	// Observe the complete debounce window. Backlog work uses a separate signal.
	start := time.Now()

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.GreaterOrEqual(c, time.Since(start), 3*w.DebounceDelay)

		n := strings.Count(logs.String(), `msg="watch: debounce fired"`)
		assert.GreaterOrEqual(c, n, 1)
		assert.Equal(c, 1, n)
	}, time.Second, 10*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if !slices.Contains(allPaths, "/nix/store/aaa-debounce") {
		t.Errorf("expected /nix/store/aaa-debounce in copied paths, got %v", allPaths)
	}
}

// TestBaselineSkipsExistingPaths pins the baseline cursor: everything already
// registered when the watcher starts is history. Without it every daemon
// restart re-pushes the whole store, which presents as "the cache is slow"
// rather than as a bug.
func TestBaselineSkipsExistingPaths(t *testing.T) {
	db, dbPath := openTestFileDB(t)
	insertPaths(t, db, "/nix/store/aaa-existing", "/nix/store/bbb-existing")

	ready := watchStarted(t)

	var (
		mu  sync.Mutex
		got []string
	)

	copyFn := func(_ context.Context, _ string, paths []string) (upload.Summary, error) {
		mu.Lock()
		defer mu.Unlock()

		got = append(got, paths...)

		return upload.Summary{Uploaded: len(paths)}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())

	w := &Watcher{
		StateDir:      filepath.Join(t.TempDir(), "state"),
		DBPath:        dbPath,
		StoreDir:      t.TempDir(),
		TargetURL:     "file:///tmp/tsnixcache-test-baseline",
		CopyFn:        copyFn,
		PollInterval:  20 * time.Millisecond,
		DebounceDelay: time.Hour,
	}

	watchDone := make(chan error, 1)

	startTestTask(t, func() { watchDone <- w.Watch(ctx) })

	t.Cleanup(func() {
		cancel()
		<-watchDone
	})

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not start")
	}

	insertPaths(t, db, "/nix/store/ccc-new")

	// Waiting for the new path proves a poll ran, so the pre-existing paths had
	// their chance to be offered too.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		mu.Lock()
		defer mu.Unlock()

		assert.Contains(c, got, "/nix/store/ccc-new")
	}, 2*time.Second, 10*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if !slices.Equal(got, []string{"/nix/store/ccc-new"}) {
		t.Errorf("uploaded %v, want only the path registered after the watcher started", got)
	}
}

// TestIdleExit covers --idle-exit, whose whole purpose is the CI recipe: the
// watcher must return nil on its own once the idle window passes.
func TestIdleExit(t *testing.T) {
	_, dbPath := openTestFileDB(t)

	w := &Watcher{
		StateDir:  filepath.Join(t.TempDir(), "state"),
		DBPath:    dbPath,
		StoreDir:  t.TempDir(),
		TargetURL: "file:///tmp/tsnixcache-test-idle",
		CopyFn: func(_ context.Context, _ string, _ []string) (upload.Summary, error) {
			return upload.Summary{}, nil
		},
		IdleExit:      100 * time.Millisecond,
		PollInterval:  20 * time.Millisecond,
		DebounceDelay: time.Hour,
	}

	start := time.Now()
	done := make(chan error, 1)

	startTestTask(t, func() { done <- w.Watch(t.Context()) })

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Watch returned %v, want nil on idle exit", err)
		}

		if elapsed := time.Since(start); elapsed < w.IdleExit {
			t.Errorf("Watch exited after %v, want it to wait out the %v idle window", elapsed, w.IdleExit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not idle-exit")
	}
}

// TestRetryAfterCopyFailure verifies that a path failing to upload (cache
// offline) lands in the retry queue and is re-attempted once its backoff elapses.
func TestRetryAfterCopyFailure(t *testing.T) {
	db, dbPath := openTestFileDB(t)
	storeDir := t.TempDir()

	var mu sync.Mutex

	var gotPaths []string

	var failNext atomic.Bool

	failNext.Store(true) // first upload fails (cache "down")

	copyFn := func(_ context.Context, _ string, paths []string) (upload.Summary, error) {
		if failNext.Swap(false) {
			return upload.Summary{Failed: append([]string(nil), paths...)}, errTestCacheDown
		}

		mu.Lock()
		defer mu.Unlock()

		gotPaths = append(gotPaths, paths...)

		return upload.Summary{Uploaded: len(paths)}, nil
	}

	ready := watchStarted(t)
	ctx, cancel := context.WithCancel(t.Context())

	w := &Watcher{
		StateDir:      filepath.Join(t.TempDir(), "state"),
		DBPath:        dbPath,
		StoreDir:      storeDir,
		TargetURL:     "file:///tmp/tsnixcache-test-retry",
		CopyFn:        copyFn,
		PollInterval:  40 * time.Millisecond,
		DebounceDelay: 10 * time.Second,
		// Short backoff so the retry is due within the test window.
		RetryBackoffBase: 10 * time.Millisecond,
		RetryBackoffMax:  20 * time.Millisecond,
	}

	watchDone := make(chan error, 1)

	startTestTask(t, func() { watchDone <- w.Watch(ctx) })

	// Waiting for the watcher to leak into the next test's log buffer is not
	// hypothetical: it asserts on an empty one.
	t.Cleanup(func() {
		cancel()
		<-watchDone
	})

	// Insert only once Watch has recorded its baseline maxID; the first poll
	// then fails the copy and a later poll retries.
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not start")
	}

	insertPaths(t, db, "/nix/store/aaa-retry")

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		mu.Lock()
		defer mu.Unlock()

		assert.Contains(c, gotPaths, "/nix/store/aaa-retry")
	}, 2*time.Second, 10*time.Millisecond)
}

// TestPathResultLogger checks the operator gets the reason a path failed, and
// that a closure-wide failure is capped rather than logged path by path.
func TestPathResultLogger(t *testing.T) {
	logs := captureLogs(t)
	onPath := pathResultLogger()

	onPath(upload.Stats{Path: "/nix/store/aaa-why", Err: errTestCacheDown})

	out := logs.String()
	if !strings.Contains(out, "/nix/store/aaa-why") || !strings.Contains(out, errTestCacheDown.Error()) {
		t.Errorf("failed path log %q, want it to name the path and the error", out)
	}

	logs.Reset()

	onPath(upload.Stats{Path: "/nix/store/bbb-ok"})

	if out := logs.String(); out != "" {
		t.Errorf("successful path logged %q, want nothing (the batch summary covers it)", out)
	}

	// One more failure than the cap allows, on top of the one already logged.
	for i := range pathLogLimit {
		onPath(upload.Stats{Path: fmt.Sprintf("/nix/store/ccc-%d", i), Err: errTestCacheDown})
	}

	out = logs.String()
	if got := strings.Count(out, `msg="watch: upload path"`); got != pathLogLimit-1 {
		t.Errorf("logged %d further paths, want the cap of %d to hold across the whole run", got, pathLogLimit-1)
	}

	if !strings.Contains(out, "watch: further path failures not logged") {
		t.Errorf("no suppression notice in %q; a silent cap looks like the reasons were lost", out)
	}
}

// TestWatchLogsPathFailures drives Watch with no CopyFn injected, so the real
// upload path runs. It checks three things the unit test above cannot: that the
// per-path logger is actually wired into upload.Options, that its cap holds when
// a whole closure fails at once, and that the cap is spent per upload rather
// than once for the lifetime of the daemon.
func TestWatchLogsPathFailures(t *testing.T) {
	const (
		root1       = "/nix/store/0000000000000000000000000000000r-root"
		root2       = "/nix/store/0000000000000000000000000000000s-root"
		closureSize = pathLogLimit + 4
	)

	closure := []string{root1, root2}

	for i := range closureSize - len(closure) {
		closure = append(closure, fmt.Sprintf("/nix/store/%032d-dep%d", i, i))
	}

	fakeNix(t, closure)

	logs := captureLogs(t)

	db, dbPath := openTestFileDB(t)

	w := &Watcher{
		StateDir: filepath.Join(t.TempDir(), "state"),
		DBPath:   dbPath,
		StoreDir: t.TempDir(),
		// Port 1 is reserved and never listening, so every path fails.
		TargetURL:     "http://127.0.0.1:1",
		PollInterval:  20 * time.Millisecond,
		DebounceDelay: time.Hour,
		Attempts:      1,
	}

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)

	startTestTask(t, func() { done <- w.Watch(ctx) })

	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Insert only once Watch has recorded its baseline maxID, or the path is part
	// of the baseline and never uploaded.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.NotEmpty(c, findLog(logs.String(), "watch: starting"))
	}, 10*time.Second, 10*time.Millisecond)

	// uploadRoot registers a path and returns the log of the upload it triggers.
	// Each root is uploaded by its own poll: the previous one is in the retry
	// queue under the default 30s backoff, so it is not due again in this window.
	uploadRoot := func(path string) string {
		t.Helper()
		logs.Reset()
		insertPaths(t, db, path)

		require.EventuallyWithT(t, func(c *assert.CollectT) {
			assert.NotEmpty(c, findLog(logs.String(), "watch: uploaded batch"))
		}, 10*time.Second, 20*time.Millisecond)

		return logs.String()
	}

	out := uploadRoot(root1)
	if got := strings.Count(out, `msg="watch: upload path"`); got != pathLogLimit {
		t.Errorf("Watch logged %d per-path failures for a closure of %d, want the cap of %d\n%s",
			got, closureSize, pathLogLimit, out)
	}

	// A fresh cap for the second upload. Built once for the daemon instead, a
	// watcher that saw one big failing closure would never again report why any
	// path failed.
	out = uploadRoot(root2)
	if got := strings.Count(out, `msg="watch: upload path"`); got != pathLogLimit {
		t.Errorf("the second upload logged %d per-path failures, want a fresh cap of %d\n%s",
			got, pathLogLimit, out)
	}
}

func TestPollWithoutFsnotify(t *testing.T) {
	db, dbPath := openTestFileDB(t)
	storeDir := t.TempDir()

	var callCount atomic.Int32

	var mu sync.Mutex

	var gotPaths []string

	copyFn := func(_ context.Context, _ string, paths []string) (upload.Summary, error) {
		mu.Lock()
		defer mu.Unlock()

		callCount.Add(1)

		gotPaths = append(gotPaths, paths...)

		return upload.Summary{}, nil
	}

	ready := watchStarted(t)
	ctx, cancel := context.WithCancel(t.Context())

	w := &Watcher{
		StateDir:      filepath.Join(t.TempDir(), "state"),
		DBPath:        dbPath,
		StoreDir:      storeDir,
		TargetURL:     "file:///tmp/tsnixcache-test-poll",
		CopyFn:        copyFn,
		PollInterval:  80 * time.Millisecond,
		DebounceDelay: 10 * time.Second, // long; we rely on the ticker
	}

	watchDone := make(chan error, 1)

	startTestTask(t, func() { watchDone <- w.Watch(ctx) })

	t.Cleanup(func() {
		cancel()
		<-watchDone
	})

	// Let Watch capture its baseline maxID before inserting.
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not start")
	}

	// Insert a new path — no fsnotify events, only the ticker should pick it up.
	insertPaths(t, db, "/nix/store/bbb-poll")

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		mu.Lock()
		defer mu.Unlock()

		assert.Contains(c, gotPaths, "/nix/store/bbb-poll")
	}, 2*time.Second, 10*time.Millisecond)
}

func startTestTask(tb testing.TB, task func()) {
	tb.Helper()

	var group errgroup.Group
	group.Go(func() error {
		task()

		return nil
	})
	tb.Cleanup(func() { require.NoError(tb, group.Wait()) })
}

func TestCancellationDrainsActiveUpload(t *testing.T) {
	source, path := openTestFileDB(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ready := make(chan struct{})
	started := make(chan struct{})
	done := make(chan error, 1)

	var calls atomic.Int32

	w := &Watcher{
		DBPath: path, StoreDir: t.TempDir(), StateDir: filepath.Join(t.TempDir(), "state"), TargetURL: testCacheURL,
		PollInterval: time.Millisecond, Ready: func() error {
			close(ready)

			return nil
		},
		CopyFn: func(ctx context.Context, _ string, paths []string) (upload.Summary, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-ctx.Done()

				return upload.Summary{Failed: paths}, ctx.Err()
			}

			return upload.Summary{Uploaded: len(paths)}, nil
		},
	}
	startTestTask(t, func() { done <- w.Watch(ctx) })
	<-ready
	insertPaths(t, source, "new")
	<-started
	cancel()
	require.NoError(t, <-done)
	require.EqualValues(t, 2, calls.Load())
}

func TestIdleExitReportsIncomplete(t *testing.T) {
	source, path := openTestFileDB(t)
	w := &Watcher{
		DBPath: path, StoreDir: t.TempDir(), StateDir: filepath.Join(t.TempDir(), "state"), TargetURL: testCacheURL,
		IdleExit: 50 * time.Millisecond, PollInterval: time.Millisecond, RetryBackoffBase: time.Hour, RetryBackoffMax: time.Hour,
		Ready: func() error {
			insertPaths(t, source, "pending")

			return nil
		},
		CopyFn: func(_ context.Context, _ string, paths []string) (upload.Summary, error) {
			return upload.Summary{Failed: paths}, errTestCacheDown
		},
	}
	require.ErrorIs(t, w.Watch(t.Context()), ErrIncomplete)
}
