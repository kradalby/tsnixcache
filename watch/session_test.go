// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package watch

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kradalby/tsnixcache/db"
	"github.com/kradalby/tsnixcache/upload"
)

func sessionPath(tb testing.TB) string {
	tb.Helper()
	dir := filepath.Join(tb.TempDir(), strings.Repeat("long-workspace-", 12))
	require.NoError(tb, os.Mkdir(dir, 0o700))

	return filepath.Join(dir, "watch.pid")
}

func TestSessionNamesSharedDirectory(t *testing.T) {
	path := sessionPath(t)
	dir := filepath.Dir(path)
	require.NoError(t, os.Chmod(dir, 0o755)) // #nosec G302 -- exercises refusal of a shared session directory.

	_, err := NewSession(t.Context(), path, func() {})
	require.ErrorIs(t, err, errSessionPrivate)
	require.ErrorContains(t, err, dir+" is mode 0755")

	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o700)) // #nosec G306 -- a private mode must not hide the file

	_, err = NewSession(t.Context(), filepath.Join(file, "watch.pid"), func() {})
	require.ErrorIs(t, err, errSessionPrivate)
	require.ErrorContains(t, err, file+" is not a directory")
}

func TestSessionLifecycle(t *testing.T) {
	path := sessionPath(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	s, err := NewSession(t.Context(), path, cancel)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close(t.Context(), nil)) })

	status, err := ReadStatus(path)
	require.NoError(t, err)
	require.Equal(t, SessionStarting, status.State)
	require.Less(t, len(status.Socket), 104)

	for _, file := range []string{path, path + ".status.json", path + ".lock"} {
		info, err := os.Stat(file)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	_, err = NewSession(t.Context(), path, cancel)
	require.ErrorIs(t, err, ErrSessionBusy)
	require.NoError(t, s.Ready())
	ready, err := WaitSession(t.Context(), path, WaitOptions{Ready: true})
	require.NoError(t, err)
	require.Equal(t, status.Session, ready.Session)

	for range 3 {
		stopped, err := sessionControl(t.Context(), status, true)
		require.NoError(t, err)
		require.Equal(t, SessionDraining, stopped.State)
	}

	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.NoError(t, s.Ready())

	status, err = ReadStatus(path)
	require.NoError(t, err)
	require.Equal(t, SessionDraining, status.State)
	require.NoError(t, s.Close(t.Context(), nil))

	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	terminal, err := WaitSession(t.Context(), path, WaitOptions{Stop: true, Session: status.Session})
	require.NoError(t, err)
	require.Equal(t, SessionCompleted, terminal.State)
	_, err = WaitSession(t.Context(), path, WaitOptions{Ready: true})
	require.ErrorIs(t, err, ErrSessionNotReady)
}

func TestSessionReuseRejectsOldStop(t *testing.T) {
	path := sessionPath(t)
	s, err := NewSession(t.Context(), path, func() {})
	require.NoError(t, err)
	old, err := ReadStatus(path)
	require.NoError(t, err)
	require.NoError(t, s.Close(t.Context(), nil))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	next, err := NewSession(t.Context(), path, cancel)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, next.Close(t.Context(), nil)) })

	current, err := ReadStatus(path)
	require.NoError(t, err)

	old.Socket = current.Socket
	_, err = sessionControl(t.Context(), old, true)
	require.ErrorIs(t, err, ErrSessionReplaced)
	require.NoError(t, ctx.Err())
	_, err = WaitSession(t.Context(), path, WaitOptions{Session: old.Session, Stop: true})
	require.ErrorIs(t, err, ErrSessionReplaced)
	require.NoError(t, ctx.Err())
}

func TestSessionFailureAndMissingStatus(t *testing.T) {
	path := sessionPath(t)
	_, err := WaitSession(t.Context(), path, WaitOptions{StartupTimeout: 10 * time.Millisecond})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	s, err := NewSession(t.Context(), path, func() {})
	require.NoError(t, err)
	require.ErrorIs(t, s.Close(t.Context(), errTestCacheDown), errTestCacheDown)
	status, err := WaitSession(t.Context(), path, WaitOptions{})
	require.ErrorIs(t, err, ErrSessionFailed)
	require.Contains(t, status.Error, errTestCacheDown.Error())

	for _, input := range []string{"", "0", "-1", "broken", `{"version":1,"pid":0}`} {
		require.NoError(t, os.WriteFile(path+".status.json", []byte(input), 0o600))
		_, err = ReadStatus(path)
		require.ErrorIs(t, err, errSessionStatus)
	}
}

func TestSessionWriteFailure(t *testing.T) {
	path := sessionPath(t)
	s, err := NewSession(t.Context(), path, func() {})
	require.NoError(t, err)
	require.NoError(t, os.Remove(path+".status.json"))
	require.NoError(t, os.Mkdir(path+".status.json", 0o700))
	require.Error(t, s.Ready())
	require.Error(t, s.Close(t.Context(), nil))

	lock, err := sessionLock(path)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
}

func TestSessionStopDuringUpload(t *testing.T) {
	source, dbPath := openTestFileDB(t)
	path := sessionPath(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	s, err := NewSession(t.Context(), path, cancel)
	require.NoError(t, err)

	entered := make(chan struct{})

	var calls int

	w := &Watcher{
		DBPath: dbPath, StoreDir: t.TempDir(), StateDir: filepath.Join(t.TempDir(), "state"), TargetURL: testCacheURL,
		PollInterval: time.Hour, IdleExit: 0, Report: s.Report,
		Ready: func() error {
			insertPaths(t, source, "active")

			return s.Ready()
		},
		CopyFn: func(ctx context.Context, _ string, paths []string) (upload.Summary, error) {
			calls++
			if calls == 1 {
				close(entered)
				<-ctx.Done()

				return upload.Summary{Failed: paths}, ctx.Err()
			}

			return upload.Summary{Uploaded: len(paths)}, nil
		},
	}
	done := make(chan error, 1)

	startTestTask(t, func() { done <- s.Close(ctx, w.Watch(ctx)) })

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("upload did not start")
	}

	waitCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()

	status, err := WaitSession(waitCtx, path, WaitOptions{Stop: true})
	require.NoError(t, err)
	require.Equal(t, SessionCompleted, status.State)
	require.NoError(t, <-done)
	require.Equal(t, 2, calls)
	require.NotNil(t, status.Progress.Boundary)
	require.Zero(t, status.Progress.Pending)
}

func TestSessionCrashAfterDrain(t *testing.T) {
	const (
		envKey        = "TSNIXCACHE_SESSION_CRASH"
		crashComplete = "complete"
	)
	if input := os.Getenv(envKey); input != "" {
		var cfg crashConfig
		require.NoError(t, json.Unmarshal([]byte(input), &cfg))
		s, err := NewSession(t.Context(), cfg.Remote, func() {})
		require.NoError(t, err)

		w := &Watcher{DBPath: cfg.DBPath, StoreDir: cfg.StoreDir, StateDir: cfg.StateDir, TargetURL: testCacheURL, Report: s.Report}
		p, err := newPoller(t.Context(), w, func(_ context.Context, _ string, names []string) (upload.Summary, error) {
			return upload.Summary{Uploaded: len(names)}, nil
		}, func() {})
		require.NoError(t, err)

		if cfg.Stage == crashComplete {
			require.NoError(t, p.finish(t.Context()))
		}

		os.Exit(86)
	}

	for _, stage := range []string{crashComplete, "abandoned"} {
		t.Run(stage, func(t *testing.T) {
			f := newRetryFixture(t, func(context.Context, string, []string) (upload.Summary, error) { return upload.Summary{}, nil })
			insertPaths(t, f.source, "pending")
			require.NoError(t, f.p.close())
			f.p = nil
			path := sessionPath(t)
			cfg := crashConfig{DBPath: f.w.DBPath, StoreDir: f.w.StoreDir, StateDir: f.w.StateDir, Stage: stage, Remote: path}
			data, err := json.Marshal(cfg)
			require.NoError(t, err)
			// #nosec G204 G702 -- isolated copy of this test executable.
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSessionCrashAfterDrain$", "-test.timeout=30s")

			cmd.Env = append(os.Environ(), envKey+"="+string(data))
			out, err := cmd.CombinedOutput()

			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "%s", out)
			require.Equal(t, 86, exitErr.ExitCode(), "%s", out)

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			before, err := ReadStatus(path)
			require.NoError(t, err)

			if stage == crashComplete {
				cancelled, stop := context.WithCancel(t.Context())
				stop()

				_, err = recoverSession(cancelled, path, before)
				require.ErrorIs(t, err, context.Canceled)

				unchanged, readErr := ReadStatus(path)
				require.NoError(t, readErr)
				require.Equal(t, before, unchanged)
				state, openErr := db.Open(t.Context(), before.Progress.StateFile)
				require.NoError(t, openErr)
				_, err = recoverSession(t.Context(), path, before)
				require.ErrorIs(t, err, db.ErrBusy)
				require.NoError(t, state.Close())

				unchanged, readErr = ReadStatus(path)
				require.NoError(t, readErr)
				require.Equal(t, before, unchanged)
			}

			result, err := WaitSession(ctx, path, WaitOptions{})
			if stage == crashComplete {
				require.NoError(t, err)
				require.Equal(t, SessionCompleted, result.State)
				require.NotNil(t, result.Progress.Boundary)
			} else {
				require.ErrorIs(t, err, ErrSessionFailed)
				require.Equal(t, SessionFailed, result.State)
			}

			retained, readErr := ReadStatus(path)
			require.NoError(t, readErr)
			require.Equal(t, result, retained)
		})
	}
}

func TestMissingRecoveryStateIsReported(t *testing.T) {
	path := sessionPath(t)
	status := Status{
		Version: 1, Session: strings.Repeat("a", 64), PID: os.Getpid(), State: SessionDraining,
		Socket: filepath.Join(t.TempDir(), "missing.sock"), Progress: Progress{StateFile: filepath.Join(t.TempDir(), "missing.sqlite"), Generation: 1},
	}
	s := &Session{path: path, status: status}
	require.NoError(t, s.write())
	_, err := WaitSession(t.Context(), path, WaitOptions{Stop: true, StartupTimeout: time.Second})
	require.ErrorIs(t, err, os.ErrNotExist)
	retained, err := ReadStatus(path)
	require.NoError(t, err)
	require.Equal(t, status, retained)
}
