package watch

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openTestDB creates an in-memory SQLite DB with the minimal ValidPaths schema.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE ValidPaths (
		id   INTEGER PRIMARY KEY AUTOINCREMENT,
		path TEXT NOT NULL
	)`)
	if err != nil {
		db.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// insertPaths inserts path rows into the test DB.
func insertPaths(t *testing.T, db *sql.DB, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := db.Exec(`INSERT INTO ValidPaths (path) VALUES (?)`, p); err != nil {
			t.Fatalf("insert path %q: %v", p, err)
		}
	}
}

// openTestFileDB creates a temp SQLite file DB with the ValidPaths schema.
func openTestFileDB(t *testing.T) (db *sql.DB, path string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "db.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open file db: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE ValidPaths (
		id   INTEGER PRIMARY KEY AUTOINCREMENT,
		path TEXT NOT NULL
	)`)
	if err != nil {
		db.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func TestNewValidPaths_Empty(t *testing.T) {
	db := openTestDB(t)
	paths, maxID, err := NewValidPaths(db, 0)
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

	paths, maxID, err := NewValidPaths(db, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d: %v", len(paths), paths)
	}
	if paths[0] != "/nix/store/bbb" || paths[1] != "/nix/store/ccc" {
		t.Errorf("unexpected paths: %v", paths)
	}
	if maxID != 3 {
		t.Errorf("expected maxID=3, got %d", maxID)
	}
}

func TestNewValidPaths_NoneNew(t *testing.T) {
	db := openTestDB(t)
	insertPaths(t, db, "/nix/store/aaa", "/nix/store/bbb")

	paths, maxID, err := NewValidPaths(db, 5)
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

func TestNixCopyCommand(t *testing.T) {
	if _, err := exec.LookPath("nix"); err != nil {
		t.Skip("nix not in PATH")
	}
	// nix copy to a nonsense URL with a nonexistent path should fail with an
	// error — this exercises the code path without actually copying anything.
	err := nixCopy(
		context.Background(),
		"file:///tmp/tsnixcache-test-nonexistent",
		[]string{"/nix/store/nonexistent-aaaaaaaaaaaaaaaaaaaaaaaaaaaa-x"},
	)
	if err == nil {
		t.Fatal("expected nixCopy to return error for nonexistent path, got nil")
	}
}

func TestGracefulShutdown(t *testing.T) {
	_, dbPath := openTestFileDB(t)
	storeDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	w := &Watcher{
		DBPath:        dbPath,
		StoreDir:      storeDir,
		TargetURL:     "file:///tmp/tsnixcache-test-shutdown",
		CopyFn:        func(_ context.Context, _ string, _ []string) error { return nil },
		PollInterval:  100 * time.Millisecond,
		DebounceDelay: 50 * time.Millisecond,
	}

	done := make(chan error, 1)
	go func() { done <- w.Watch(ctx) }()

	select {
	case err := <-done:
		// ctx.Err() or nil are both acceptable returns.
		if err != nil && err != context.Canceled {
			t.Errorf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return after context cancellation")
	}
}

func TestDebounceCoalesces(t *testing.T) {
	db, dbPath := openTestFileDB(t)
	storeDir := t.TempDir()

	var (
		mu    sync.Mutex
		calls [][]string
	)
	copyFn := func(_ context.Context, _ string, paths []string) error {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]string, len(paths))
		copy(cp, paths)
		calls = append(calls, cp)
		return nil
	}

	ctx := t.Context()

	w := &Watcher{
		DBPath:        dbPath,
		StoreDir:      storeDir,
		TargetURL:     "file:///tmp/tsnixcache-test-debounce",
		CopyFn:        copyFn,
		PollInterval:  10 * time.Second, // long; we rely on fsnotify events
		DebounceDelay: 100 * time.Millisecond,
	}

	watchDone := make(chan error, 1)
	go func() { watchDone <- w.Watch(ctx) }()

	// Give Watch time to start and record the baseline maxID.
	time.Sleep(30 * time.Millisecond)

	// Insert a path and trigger many rapid fsnotify events.
	insertPaths(t, db, "/nix/store/aaa-debounce")

	for range 10 {
		f := filepath.Join(storeDir, "tmpfile")
		_ = os.WriteFile(f, []byte("x"), 0o600)
		_ = os.Remove(f)
	}

	// Wait for debounce to settle (200ms > 100ms debounce).
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	n := len(calls)
	var allPaths []string
	for _, c := range calls {
		allPaths = append(allPaths, c...)
	}
	mu.Unlock()

	if n == 0 {
		t.Fatal("copyFn was never called")
	}
	if n > 2 {
		t.Errorf("debounce should have coalesced events into ≤2 calls, got %d", n)
	}
	if !slices.Contains(allPaths, "/nix/store/aaa-debounce") {
		t.Errorf("expected /nix/store/aaa-debounce in copied paths, got %v", allPaths)
	}
}

func TestPollWithoutFsnotify(t *testing.T) {
	db, dbPath := openTestFileDB(t)
	storeDir := t.TempDir()

	var callCount atomic.Int32
	var mu sync.Mutex
	var gotPaths []string

	copyFn := func(_ context.Context, _ string, paths []string) error {
		mu.Lock()
		defer mu.Unlock()
		callCount.Add(1)
		gotPaths = append(gotPaths, paths...)
		return nil
	}

	ctx := t.Context()

	w := &Watcher{
		DBPath:        dbPath,
		StoreDir:      storeDir,
		TargetURL:     "file:///tmp/tsnixcache-test-poll",
		CopyFn:        copyFn,
		PollInterval:  80 * time.Millisecond,
		DebounceDelay: 10 * time.Second, // long; we rely on the ticker
	}

	watchDone := make(chan error, 1)
	go func() { watchDone <- w.Watch(ctx) }()

	// Give Watch time to start and record the baseline maxID.
	time.Sleep(20 * time.Millisecond)

	// Insert a new path — no fsnotify events, only the ticker should pick it up.
	insertPaths(t, db, "/nix/store/bbb-poll")

	// Wait for 2x PollInterval.
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	paths := make([]string, len(gotPaths))
	copy(paths, gotPaths)
	mu.Unlock()

	if !slices.Contains(paths, "/nix/store/bbb-poll") {
		t.Errorf("expected /nix/store/bbb-poll to be picked up by poll, got %v", paths)
	}
}
