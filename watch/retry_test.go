// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package watch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	"github.com/kradalby/tsnixcache/db"
	"github.com/kradalby/tsnixcache/gen/dbsqlc"
	"github.com/kradalby/tsnixcache/upload"
)

const testCacheURL = "http://cache"

var errTestCacheDown = errors.New("test: cache down")

type retryFixture struct {
	p      *poller
	source *sql.DB
	w      *Watcher
}

func newRetryFixture(t *testing.T, copyFn copyFunc) *retryFixture {
	t.Helper()
	source, path := openTestFileDB(t)
	w := &Watcher{DBPath: path, StoreDir: t.TempDir(), StateDir: filepath.Join(t.TempDir(), "state"), TargetURL: testCacheURL, RetryQueueSize: 512, RetryBackoffBase: time.Hour, RetryBackoffMax: time.Hour}
	p, err := newPoller(t.Context(), w, copyFn, func() {})
	require.NoError(t, err)

	f := &retryFixture{p: p, source: source, w: w}

	t.Cleanup(func() {
		if f.p != nil {
			require.NoError(t, f.p.close())
		}
	})

	return f
}

func (f *retryFixture) reopen(t *testing.T) {
	t.Helper()

	copyFn := f.p.copyFn
	require.NoError(t, f.p.close())
	f.p = nil
	p, err := newPoller(t.Context(), f.w, copyFn, func() {})
	require.NoError(t, err)

	f.p = p
}

func pollAll(t *testing.T, p *poller) {
	t.Helper()

	for range 100 {
		require.NoError(t, p.poll(t.Context()))

		if !p.more {
			return
		}
	}

	t.Fatal("poller did not quiesce")
}

func fixturePaths(n int) []string {
	paths := make([]string, n)
	for i := range paths {
		paths[i] = fmt.Sprintf("/nix/store/%032d-path", i)
	}

	return paths
}

func TestDurableOutage(t *testing.T) {
	captureLogs(t)

	attempts := map[string]int{}
	down := true
	f := newRetryFixture(t, func(_ context.Context, _ string, names []string) (upload.Summary, error) {
		require.LessOrEqual(t, len(names), uploadBatch)

		for _, name := range names {
			attempts[name]++
		}

		if down {
			return upload.Summary{Failed: names}, errTestCacheDown
		}

		return upload.Summary{Uploaded: len(names)}, nil
	})
	paths := fixturePaths(1101)
	insertPaths(t, f.source, paths...)
	pollAll(t, f.p)
	require.Len(t, attempts, len(paths))

	for _, n := range attempts {
		require.Equal(t, 1, n)
	}

	before, err := f.p.state.Pending(t.Context(), paths[0])
	require.NoError(t, err)
	cp, err := f.p.state.Checkpoint(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, len(paths), cp.Cursor)
	f.reopen(t)
	pollAll(t, f.p)

	for _, n := range attempts {
		require.Equal(t, 1, n)
	}

	after, err := f.p.state.Pending(t.Context(), paths[0])
	require.NoError(t, err)
	require.Empty(t, cmp.Diff(before, after))

	down = false

	require.NoError(t, f.p.state.Transaction(t.Context(), func(q *dbsqlc.Queries) error {
		for _, path := range paths {
			err := q.Retry(t.Context(), dbsqlc.RetryParams{Path: path, Now: time.Now().UnixMilli(), NextAt: 1})
			if err != nil {
				return err
			}
		}

		return nil
	}))
	pollAll(t, f.p)
	n, err := f.p.state.CountPending(t.Context())
	require.NoError(t, err)
	require.Zero(t, n)

	for _, n := range attempts {
		require.Equal(t, 2, n)
	}
}

func TestFreshRetryFairness(t *testing.T) {
	captureLogs(t)

	var batches [][]string

	f := newRetryFixture(t, func(_ context.Context, _ string, paths []string) (upload.Summary, error) {
		batches = append(batches, slices.Clone(paths))

		return upload.Summary{Failed: paths}, errTestCacheDown
	})
	insertPaths(t, f.source, "retry")
	pollAll(t, f.p)
	require.NoError(t, f.p.state.Retry(t.Context(), dbsqlc.RetryParams{Path: "retry", Now: 1, NextAt: 1}))
	insertPaths(t, f.source, fixturePaths(1101)...)

	batches = nil

	require.NoError(t, f.p.poll(t.Context()))
	require.Len(t, batches, 2)
	require.Len(t, batches[0], uploadBatch)
	require.Equal(t, []string{"retry"}, batches[1])
}

func TestDrainEveryPage(t *testing.T) {
	captureLogs(t)

	var (
		f   *retryFixture
		got []string
	)

	paths := fixturePaths(2101)
	f = newRetryFixture(t, func(ctx context.Context, _ string, names []string) (upload.Summary, error) {
		if len(got) == 0 {
			insertPathsContext(ctx, t, f.source, "later")
			_, err := f.source.ExecContext(ctx, `DELETE FROM ValidPaths WHERE path = ?`, paths[len(paths)-1])
			require.NoError(t, err)
		}

		got = append(got, names...)

		return upload.Summary{Uploaded: len(names)}, nil
	})
	insertPaths(t, f.source, paths...)
	require.NoError(t, f.p.finish(t.Context()))
	require.Equal(t, paths[:len(paths)-1], got)
	cp, err := f.p.state.Checkpoint(t.Context())
	require.NoError(t, err)
	require.False(t, cp.Boundary.Valid)
	f.reopen(t)
	pollAll(t, f.p)
	require.Equal(t, "later", got[len(got)-1])
}

func TestIncompleteDrainResumes(t *testing.T) {
	captureLogs(t)

	calls := 0
	down := true
	f := newRetryFixture(t, func(_ context.Context, _ string, names []string) (upload.Summary, error) {
		calls++

		if down {
			return upload.Summary{Failed: names}, errTestCacheDown
		}

		return upload.Summary{Uploaded: len(names)}, nil
	})
	insertPaths(t, f.source, "a")
	require.ErrorIs(t, f.p.finish(t.Context()), ErrIncomplete)
	require.Equal(t, 1, calls)
	before, err := f.p.state.Pending(t.Context(), "a")
	require.NoError(t, err)
	f.reopen(t)
	require.ErrorIs(t, f.p.finish(t.Context()), ErrIncomplete)
	require.Equal(t, 1, calls, "restart must not renew the forced final allowance")
	pollAll(t, f.p)
	require.Equal(t, 1, calls, "ordinary retries must honor the saved backoff")
	after, err := f.p.state.Pending(t.Context(), "a")
	require.NoError(t, err)
	require.Equal(t, before, after)

	down = false

	require.NoError(t, f.p.state.Retry(t.Context(), dbsqlc.RetryParams{Path: "a", Now: 1, NextAt: 1}))
	pollAll(t, f.p)
	cp, err := f.p.state.Checkpoint(t.Context())
	require.NoError(t, err)
	require.False(t, cp.Boundary.Valid)
}

func TestExpirySurvivesReplay(t *testing.T) {
	captureLogs(t)

	calls := 0
	f := newRetryFixture(t, func(_ context.Context, _ string, paths []string) (upload.Summary, error) {
		calls++

		return upload.Summary{Failed: paths}, errTestCacheDown
	})
	f.w.RetryMaxAge = time.Hour
	insertPaths(t, f.source, "a", "anchor")
	pollAll(t, f.p)
	// Mark an old failure without waiting for wall time.
	require.NoError(t, f.p.state.Retire(t.Context(), "a"))
	require.NoError(t, f.p.state.Discover(t.Context(), dbsqlc.DiscoverParams{Path: "a", SourceID: 1, Source: sourceIdentity(f.p.source)}))
	require.NoError(t, f.p.state.Retry(t.Context(), dbsqlc.RetryParams{Path: "a", Now: time.Now().Add(-2 * time.Hour).UnixMilli(), NextAt: 1}))
	pollAll(t, f.p)
	it, err := f.p.state.Pending(t.Context(), "a")
	require.NoError(t, err)
	require.EqualValues(t, 1, it.Expired)
	_, err = f.source.ExecContext(t.Context(), `DELETE FROM ValidPaths WHERE path = 'anchor'`)
	require.NoError(t, err)
	f.reopen(t)
	pollAll(t, f.p)
	after, err := f.p.state.Pending(t.Context(), "a")
	require.NoError(t, err)
	require.Equal(t, it, after)
	require.Equal(t, 1, calls)
	require.ErrorIs(t, f.p.finish(t.Context()), ErrIncomplete)
	cp, err := f.p.state.Checkpoint(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, cp.LastExpired)
	require.Zero(t, cp.Expired)
	require.False(t, cp.Boundary.Valid)
	_, err = f.source.ExecContext(t.Context(), `DELETE FROM ValidPaths WHERE path = 'a'`)
	require.NoError(t, err)
	pollAll(t, f.p)
	_, err = f.p.state.Pending(t.Context(), "a")
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestLivePaths(t *testing.T) {
	source := openTestDB(t)
	paths := fixturePaths(600)
	insertPaths(t, source, paths...)
	names := append([]string{"missing", paths[4], "'; DROP TABLE ValidPaths; --", paths[4]}, paths...)
	want := append([]string{paths[4], paths[4]}, paths...)
	require.Equal(t, want, livePaths(t.Context(), source, names))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Equal(t, names, livePaths(ctx, source, names))
	require.NoError(t, source.Close())
	require.Equal(t, names, livePaths(t.Context(), source, names))
}

func TestRetryDelay(t *testing.T) {
	w := &Watcher{RetryBackoffBase: time.Second, RetryBackoffMax: time.Minute}
	for attempts, base := range map[int64]time.Duration{1: time.Second, 2: 2 * time.Second, 100: time.Minute, math.MaxInt64: time.Minute} {
		d := retryDelay(w, attempts)
		require.GreaterOrEqual(t, d, base*3/4)
		require.LessOrEqual(t, d, base*5/4)
	}
}

func TestReplaceSourceDuringDrain(t *testing.T) {
	captureLogs(t)

	var (
		f   *retryFixture
		got []string
	)

	replaced := false
	f = newRetryFixture(t, func(ctx context.Context, _ string, names []string) (upload.Summary, error) {
		got = append(got, names...)

		if !replaced {
			replaced = true
			replacement, path := openTestFileDBContext(ctx, t)
			insertPathsContext(ctx, t, replacement, "replacement")
			require.NoError(t, replacement.Close())
			require.NoError(t, f.source.Close())
			require.NoError(t, os.Rename(path, f.w.DBPath))
		}

		return upload.Summary{Uploaded: len(names)}, nil
	})
	insertPaths(t, f.source, fixturePaths(2101)...)
	require.NoError(t, f.p.finish(t.Context()))
	require.Contains(t, got, "replacement")
	require.Len(t, got, uploadBatch+1)
	cp, err := f.p.state.Checkpoint(t.Context())
	require.NoError(t, err)
	require.False(t, cp.Boundary.Valid)
	require.Equal(t, "replacement", cp.Anchor)
}

func TestSourceSymlinkIdentity(t *testing.T) {
	captureLogs(t)
	f := newRetryFixture(t, func(_ context.Context, _ string, names []string) (upload.Summary, error) {
		return upload.Summary{Failed: names}, errTestCacheDown
	})
	link := filepath.Join(t.TempDir(), "source.sqlite")
	require.NoError(t, os.Symlink(f.w.DBPath, link))
	f.w.DBPath = link
	f.reopen(t)
	insertPaths(t, f.source, "pending")
	pollAll(t, f.p)
	before, err := f.p.state.Pending(t.Context(), "pending")
	require.NoError(t, err)
	replacement, path := openTestFileDB(t)
	insertPaths(t, replacement, "new", "pending")
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(path, link))
	f.reopen(t)
	pollAll(t, f.p)
	after, err := f.p.state.Pending(t.Context(), "pending")
	require.NoError(t, err)
	require.Equal(t, before.FirstFail, after.FirstFail)
	require.Equal(t, before.Attempts, after.Attempts)
	require.EqualValues(t, 2, after.SourceID)
	_, err = f.p.state.Pending(t.Context(), "new")
	require.NoError(t, err)
}

func TestStateIdentity(t *testing.T) {
	f := newRetryFixture(t, func(_ context.Context, _ string, names []string) (upload.Summary, error) {
		return upload.Summary{Uploaded: len(names)}, nil
	})
	_, err := newPoller(t.Context(), f.w, f.p.copyFn, func() {})
	require.ErrorIs(t, err, db.ErrBusy)

	other := *f.w
	other.TargetURL = "http://another-cache"
	p, err := newPoller(t.Context(), &other, f.p.copyFn, func() {})
	require.NoError(t, err)

	defer p.close()

	insertPaths(t, f.source, "new")
	pollAll(t, f.p)
	cp, err := p.state.Checkpoint(t.Context())
	require.NoError(t, err)
	require.Zero(t, cp.Cursor)
	pollAll(t, p)
	cp, err = p.state.Checkpoint(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, cp.Cursor)
}

func TestDrainDeadline(t *testing.T) {
	captureLogs(t)
	f := newRetryFixture(t, func(ctx context.Context, _ string, names []string) (upload.Summary, error) {
		<-ctx.Done()

		return upload.Summary{Failed: names}, ctx.Err()
	})
	f.w.SetDrainTimeoutForTesting(50 * time.Millisecond)
	insertPaths(t, f.source, fixturePaths(2101)...)
	require.ErrorIs(t, f.p.finish(t.Context()), ErrIncomplete)
	cp, err := f.p.state.Checkpoint(t.Context())
	require.NoError(t, err)
	require.True(t, cp.Boundary.Valid)
	require.EqualValues(t, 2101, cp.Boundary.Int64)
	require.Less(t, cp.Cursor, cp.Boundary.Int64)

	f.p.copyFn = func(_ context.Context, _ string, names []string) (upload.Summary, error) {
		return upload.Summary{Uploaded: len(names)}, nil
	}
	f.reopen(t)
	pollAll(t, f.p)
	cp, err = f.p.state.Checkpoint(t.Context())
	require.NoError(t, err)
	require.False(t, cp.Boundary.Valid)
}

func TestBatchFailureRetained(t *testing.T) {
	f := newRetryFixture(t, func(context.Context, string, []string) (upload.Summary, error) {
		return upload.Summary{}, errTestCacheDown
	})
	insertPaths(t, f.source, "a", "b")
	pollAll(t, f.p)
	n, err := f.p.state.CountPending(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
}

func TestSourceFailureBudget(t *testing.T) {
	f := newRetryFixture(t, func(context.Context, string, []string) (upload.Summary, error) {
		t.Fatal("unreadable source must not upload")

		return upload.Summary{}, nil
	})
	require.NoError(t, f.p.db.Close())

	for range maxPollFailures - 1 {
		require.NoError(t, f.p.poll(t.Context()))
	}

	require.ErrorIs(t, f.p.poll(t.Context()), errDBUnreadable)
}

func TestStateFailureIsImmediate(t *testing.T) {
	f := newRetryFixture(t, func(context.Context, string, []string) (upload.Summary, error) {
		t.Fatal("unreadable state must not upload")

		return upload.Summary{}, nil
	})
	require.NoError(t, f.p.state.Close())
	err := f.p.poll(t.Context())
	f.p.state = nil

	require.Error(t, err)
	require.Zero(t, f.p.dbFailures)
}

func TestVanishedConsumedFinalIsRetired(t *testing.T) {
	captureLogs(t)

	var f *retryFixture

	replaced := false
	f = newRetryFixture(t, func(ctx context.Context, _ string, names []string) (upload.Summary, error) {
		if !replaced {
			replaced = true
			replacement, path := openTestFileDBContext(ctx, t)
			insertPathsContext(ctx, t, replacement, "replacement")
			require.NoError(t, replacement.Close())
			require.NoError(t, f.source.Close())
			require.NoError(t, os.Rename(path, f.w.DBPath))

			return upload.Summary{Failed: names}, errTestCacheDown
		}

		return upload.Summary{Uploaded: len(names)}, nil
	})
	insertPaths(t, f.source, fixturePaths(600)...)
	err := f.p.finish(t.Context())
	pending, countErr := f.p.state.CountPending(t.Context())
	require.NoError(t, countErr)
	t.Logf("drain error=%v, pending=%d although old source paths all vanished", err, pending)
	require.NoError(t, err)
	require.Zero(t, pending)
}

func TestNewRegistrationReactivatesExpiredPath(t *testing.T) {
	captureLogs(t)

	calls := 0
	f := newRetryFixture(t, func(_ context.Context, _ string, paths []string) (upload.Summary, error) {
		calls++
		if calls == 1 {
			return upload.Summary{Failed: paths}, errTestCacheDown
		}

		return upload.Summary{Uploaded: len(paths)}, nil
	})
	f.w.RetryMaxAge = 1
	insertPaths(t, f.source, "rebuilt")
	pollAll(t, f.p)
	require.ErrorIs(t, f.p.finish(t.Context()), ErrIncomplete)
	_, err := f.source.ExecContext(t.Context(), `DELETE FROM ValidPaths WHERE path = 'rebuilt'`)
	require.NoError(t, err)
	insertPaths(t, f.source, "rebuilt")
	pollAll(t, f.p)
	item, err := f.p.state.Pending(t.Context(), "rebuilt")
	t.Logf("upload calls=%d, new row state=%+v, state error=%v", calls, item, err)
	require.Equal(t, 2, calls, "a new registration should not inherit an expired registration's terminal status")
}

func TestExtremeRetryDelay(t *testing.T) {
	w := &Watcher{RetryBackoffBase: math.MaxInt64, RetryBackoffMax: math.MaxInt64}
	for range 20 {
		delay := retryDelay(w, 1)
		require.Positive(t, delay)

		now := time.Now()
		require.Greater(t, now.Add(delay).UnixMilli(), now.UnixMilli())
	}
}

type crashConfig struct {
	DBPath   string `json:"db_path"`
	StoreDir string `json:"store_dir"`
	StateDir string `json:"state_dir"`
	Stage    string `json:"stage"`
	Remote   string `json:"remote"`
}

func TestCrashRecovery(t *testing.T) {
	const (
		envKey         = "TSNIXCACHE_TEST_WATCH_CRASH"
		crashDiscovery = "discovery"
		crashUpload    = "upload"
		crashFinal     = "final"
		crashAck       = "ack"
		crashExit      = 86
	)
	if input := os.Getenv(envKey); input != "" {
		var cfg crashConfig
		require.NoError(t, json.Unmarshal([]byte(input), &cfg))
		w := &Watcher{DBPath: cfg.DBPath, StoreDir: cfg.StoreDir, StateDir: cfg.StateDir, TargetURL: testCacheURL}
		p, err := newPoller(t.Context(), w, func(_ context.Context, _ string, paths []string) (upload.Summary, error) {
			if cfg.Stage == crashDiscovery || cfg.Stage == crashFinal {
				os.Exit(crashExit)
			}

			require.NoError(t, os.WriteFile(cfg.Remote, []byte("delivered"), 0o600))

			if cfg.Stage == crashUpload {
				os.Exit(crashExit)
			}

			return upload.Summary{Uploaded: len(paths)}, nil
		}, func() {})
		require.NoError(t, err)

		if cfg.Stage == crashFinal {
			require.NoError(t, p.finish(t.Context()))
		} else {
			require.NoError(t, p.poll(t.Context()))
		}

		os.Exit(crashExit)
	}

	for _, stage := range []string{crashDiscovery, crashUpload, crashFinal, crashAck} {
		t.Run(stage, func(t *testing.T) {
			f := newRetryFixture(t, func(context.Context, string, []string) (upload.Summary, error) { return upload.Summary{}, nil })
			insertPaths(t, f.source, "pending")
			require.NoError(t, f.p.close())
			f.p = nil
			cfg := crashConfig{DBPath: f.w.DBPath, StoreDir: f.w.StoreDir, StateDir: f.w.StateDir, Stage: stage, Remote: filepath.Join(t.TempDir(), "remote")}
			data, err := json.Marshal(cfg)
			require.NoError(t, err)

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			// #nosec G204 G702 -- re-executes this test binary.
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashRecovery$")

			cmd.Env = append(os.Environ(), envKey+"="+string(data))
			out, err := cmd.CombinedOutput()

			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "%s", out)
			require.Equal(t, crashExit, exitErr.ExitCode(), "%s", out)

			calls := 0
			p, err := newPoller(t.Context(), f.w, func(_ context.Context, _ string, paths []string) (upload.Summary, error) {
				calls++

				_, statErr := os.Stat(cfg.Remote)
				if statErr == nil {
					return upload.Summary{Skipped: len(paths)}, nil
				}

				return upload.Summary{Uploaded: len(paths)}, nil
			}, func() {})
			require.NoError(t, err)

			f.p = p
			if stage == crashFinal {
				require.ErrorIs(t, p.finish(t.Context()), ErrIncomplete)
				require.Zero(t, calls, "forced allowance survives a crash before network I/O")
			}

			pollAll(t, p)

			if stage == crashAck {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
			}

			n, err := p.state.CountPending(t.Context())
			require.NoError(t, err)
			require.Zero(t, n)
		})
	}
}

func TestResumedDrainIncludesNewSessionBuild(t *testing.T) {
	for _, failedOld := range []bool{false, true} {
		t.Run(strconv.FormatBool(failedOld), func(t *testing.T) {
			var (
				uploaded []string
				source   *sql.DB
			)

			f := newRetryFixture(t, func(ctx context.Context, _ string, names []string) (upload.Summary, error) {
				uploaded = append(uploaded, names...)
				if slices.Contains(names, "new-build") {
					insertPathsContext(ctx, t, source, "after-stop")
				}

				if failedOld && slices.Contains(names, "old-build") {
					return upload.Summary{Failed: names}, errTestCacheDown
				}

				return upload.Summary{Uploaded: len(names)}, nil
			})
			source = f.source
			insertPaths(t, source, "old-build")
			require.NoError(t, f.p.beginDrain(t.Context()))
			require.NoError(t, f.p.discover(t.Context()))
			fresh, err := f.p.state.Fresh(t.Context(), 10)
			require.NoError(t, err)
			require.NoError(t, f.p.attempt(t.Context(), fresh, 1))
			require.NoError(t, f.p.discover(t.Context()))
			// Restart before EndDrain: a later build belongs to the new stop request.
			f.reopen(t)
			insertPaths(t, source, "new-build")

			uploaded = nil

			err = f.p.finish(t.Context())
			if failedOld {
				require.ErrorIs(t, err, ErrIncomplete)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, []string{"new-build"}, uploaded)
			cp, err := f.p.state.Checkpoint(t.Context())
			require.NoError(t, err)
			require.EqualValues(t, 1, cp.Generation)
			require.EqualValues(t, 2, cp.Cursor)
		})
	}
}
