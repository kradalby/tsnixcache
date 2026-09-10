// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package watch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/kradalby/tsnixcache/upload"
)

func TestNotifications(t *testing.T) {
	dir := t.TempDir()
	w := &Watcher{DBPath: filepath.Join(dir, "db.sqlite"), StoreDir: t.TempDir()}
	n := newNotifications(w)
	t.Cleanup(n.close)

	now := time.Now()
	n.ensure(now)
	require.NotNil(t, n.watcher)
	require.Equal(t, []string{dir}, n.watcher.WatchList())

	for _, name := range []string{"db.sqlite", "db.sqlite-wal", "db.sqlite-shm", "db.sqlite-journal"} {
		require.True(t, n.relevant(fsnotify.Event{Name: filepath.Join(dir, name)}))
	}

	require.False(t, n.relevant(fsnotify.Event{Name: filepath.Join(dir, "unrelated")}))
	old := n.watcher
	require.NoError(t, old.Close())

	_, open := <-n.events
	require.False(t, open)
	n.failed(errNotificationClosed, now)
	require.Nil(t, n.events)
	require.Nil(t, n.errors)
	n.ensure(now)
	require.Nil(t, n.watcher)
	n.ensure(now.Add(time.Second))
	require.NotNil(t, n.watcher)
	require.NotSame(t, old, n.watcher)
}

func TestNotificationReplacementAndBackoff(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	require.NoError(t, os.Mkdir(dir, 0o700))
	n := newNotifications(&Watcher{DBPath: filepath.Join(dir, "db.sqlite")})
	t.Cleanup(n.close)

	now := time.Now()
	n.ensure(now)
	old := n.watcher

	require.NoError(t, os.Rename(dir, dir+".old"))
	require.NoError(t, os.Mkdir(dir, 0o700))
	n.ensure(now)
	require.NotNil(t, n.watcher)
	require.NotSame(t, old, n.watcher)
	require.NoError(t, os.Remove(dir))
	n.ensure(now)
	require.Nil(t, n.watcher)

	for range 20 {
		now = n.next
		n.ensure(now)
		require.LessOrEqual(t, n.backoff, time.Minute)
	}

	require.NoError(t, os.Mkdir(dir, 0o700))
	n.ensure(n.next)
	require.NotNil(t, n.watcher)
}

func TestNotificationFallback(t *testing.T) {
	db, path := openTestFileDB(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var (
		calls int
		n     *fsnotify.Watcher
	)

	w := &Watcher{DBPath: path, StoreDir: t.TempDir(), StateDir: filepath.Join(t.TempDir(), "state"), TargetURL: testCacheURL, PollInterval: 5 * time.Millisecond}
	w.SetNotificationFactoryForTesting(func() (*fsnotify.Watcher, error) {
		calls++
		if calls == 1 {
			return nil, syscall.EMFILE
		}

		var err error

		n, err = fsnotify.NewWatcher()

		return n, err
	})
	w.Ready = func() error {
		insertPaths(t, db, "/nix/store/fallback-first")

		return nil
	}

	var paths []string

	w.CopyFn = func(_ context.Context, _ string, names []string) (upload.Summary, error) {
		paths = append(paths, names...)
		if len(paths) == 1 {
			// The first delivery proves polling works before event recovery.
			require.Equal(t, 1, calls)
		}

		return upload.Summary{Uploaded: len(names)}, nil
	}
	// Stay idle long enough to exercise the bounded attachment retry.
	w.IdleExit = 1200 * time.Millisecond
	require.NoError(t, w.Watch(ctx))
	require.GreaterOrEqual(t, calls, 2)
	require.NotNil(t, n)
	require.Equal(t, []string{"/nix/store/fallback-first"}, paths)
}

func TestDarwinDescriptorPressure(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires native kqueue backend")
	}

	if os.Getenv("TSNIXCACHE_DESCRIPTOR_CHILD") != "1" {
		// #nosec G204 G702 -- isolated copy of this test executable.
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDarwinDescriptorPressure$", "-test.timeout=30s")

		cmd.Env = append(os.Environ(), "TSNIXCACHE_DESCRIPTOR_CHILD=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)

		return
	}

	db, path := openTestFileDB(t)

	storeDir := t.TempDir()
	for i := range 1024 {
		require.NoError(t, os.WriteFile(filepath.Join(storeDir, strconv.Itoa(i)), nil, 0o600))
	}

	var limit unix.Rlimit
	require.NoError(t, unix.Getrlimit(unix.RLIMIT_NOFILE, &limit))
	limit.Cur = min(limit.Cur, 128)
	require.NoError(t, unix.Setrlimit(unix.RLIMIT_NOFILE, &limit))

	for _, disabled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())

		w := &Watcher{DBPath: path, StoreDir: storeDir, StateDir: filepath.Join(t.TempDir(), "state"), TargetURL: testCacheURL, PollInterval: time.Millisecond}
		if disabled {
			w.SetNotificationFactoryForTesting(func() (*fsnotify.Watcher, error) { return nil, syscall.EMFILE })
		}

		name := fmt.Sprintf("/nix/store/descriptor-%t", disabled)
		w.Ready = func() error {
			insertPaths(t, db, name)

			return nil
		}

		var delivered bool

		w.CopyFn = func(_ context.Context, _ string, paths []string) (upload.Summary, error) {
			delivered = true

			cancel()

			return upload.Summary{Uploaded: len(paths)}, nil
		}
		require.NoError(t, w.Watch(ctx))
		cancel()
		require.True(t, delivered)
	}
}
