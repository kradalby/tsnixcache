// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package db

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"

	"github.com/kradalby/tsnixcache/gen/dbsqlc"
)

const testSource = "source"

var errTestRollback = errors.New("test: rollback")

func openTestState(t *testing.T) *DB {
	t.Helper()
	d, err := Open(t.Context(), filepath.Join(privateDir(t), "watch.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })

	return d
}

func TestOpen(t *testing.T) {
	path := filepath.Join(privateDir(t), "space ? # %", "watch.sqlite")
	d, err := Open(t.Context(), path)
	require.NoError(t, err)
	_, err = Open(t.Context(), path)
	require.ErrorIs(t, err, ErrBusy)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o600), info.Mode().Perm())

	for pragma, want := range map[string]string{"journal_mode": "wal", "synchronous": "2", "auto_vacuum": "2", "foreign_keys": "1"} {
		var got string
		require.NoError(t, d.sql.QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&got))
		require.Equal(t, want, got, pragma)
	}

	require.NoError(t, d.Initialize(t.Context(), dbsqlc.InitializeParams{Cursor: 42, Anchor: "before", Source: testSource}))
	require.NoError(t, d.Close())
	d, err = Open(t.Context(), path)
	require.NoError(t, err)

	defer d.Close()

	cp, err := d.Checkpoint(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 42, cp.Cursor)
}

func TestTransaction(t *testing.T) {
	d := openTestState(t)
	require.NoError(t, d.Initialize(t.Context(), dbsqlc.InitializeParams{Source: testSource}))

	update := func(q *dbsqlc.Queries) error {
		err := q.Discover(t.Context(), dbsqlc.DiscoverParams{Path: "a", SourceID: 1})
		if err != nil {
			return err
		}

		return q.Advance(t.Context(), dbsqlc.AdvanceParams{Cursor: 1, Anchor: "a", Source: testSource})
	}
	err := d.Transaction(t.Context(), func(q *dbsqlc.Queries) error {
		require.NoError(t, update(q))

		return errTestRollback
	})
	require.ErrorIs(t, err, errTestRollback)
	cp, err := d.Checkpoint(t.Context())
	require.NoError(t, err)
	require.Zero(t, cp.Cursor)
	n, err := d.CountPending(t.Context())
	require.NoError(t, err)
	require.Zero(t, n)
	require.NoError(t, d.Transaction(t.Context(), update))
	cp, err = d.Checkpoint(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, cp.Cursor)
	n, err = d.CountPending(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
}

func TestReplayPreservesRetry(t *testing.T) {
	d := openTestState(t)
	require.NoError(t, d.Discover(t.Context(), dbsqlc.DiscoverParams{Path: "a", SourceID: 1}))
	require.NoError(t, d.Retry(t.Context(), dbsqlc.RetryParams{Path: "a", Now: 100, NextAt: 200}))
	require.NoError(t, d.Discover(t.Context(), dbsqlc.DiscoverParams{Path: "a", SourceID: 99}))
	require.NoError(t, d.Retry(t.Context(), dbsqlc.RetryParams{Path: "a", Now: 300, NextAt: 400}))
	p, err := d.Pending(t.Context(), "a")
	require.NoError(t, err)
	require.EqualValues(t, 100, p.FirstFail)
	require.EqualValues(t, 2, p.Attempts)
	require.EqualValues(t, 400, p.NextAt)
	require.EqualValues(t, 99, p.SourceID)
}

func TestRejectUnknownSchema(t *testing.T) {
	path := filepath.Join(privateDir(t), "watch.sqlite")
	d, err := Open(t.Context(), path)
	require.NoError(t, err)
	_, err = d.sql.ExecContext(t.Context(), `CREATE TABLE future (v TEXT)`)
	require.NoError(t, err)
	require.NoError(t, d.Close())
	_, err = Open(t.Context(), path)
	require.Error(t, err)
	// Failure must release ownership while preserving the unrecognized state.
	_, again := Open(t.Context(), path)
	require.Error(t, again)
	require.NotErrorIs(t, again, ErrBusy)

	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	require.NoError(t, err)

	defer raw.Close()

	var name string
	require.NoError(t, raw.QueryRowContext(t.Context(), `SELECT name FROM sqlite_schema WHERE name = 'future'`).Scan(&name))
	require.Equal(t, "future", name)
}

func TestRejectCorruptState(t *testing.T) {
	path := filepath.Join(privateDir(t), "watch.sqlite")
	data := []byte("broken database")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	_, err := Open(t.Context(), path)
	require.Error(t, err)
	got, err := os.ReadFile(path) // #nosec G304 -- test-owned temporary file.
	require.NoError(t, err)
	require.Equal(t, data, got)
}

func TestStateWriteFailure(t *testing.T) {
	d := openTestState(t)
	require.NoError(t, d.Initialize(t.Context(), dbsqlc.InitializeParams{Source: testSource}))
	_, err := d.sql.ExecContext(t.Context(), `PRAGMA query_only = ON`)
	require.NoError(t, err)
	err = d.Transaction(t.Context(), func(q *dbsqlc.Queries) error {
		return q.Advance(t.Context(), dbsqlc.AdvanceParams{Cursor: 99})
	})
	require.Error(t, err)
	cp, err := d.Checkpoint(t.Context())
	require.NoError(t, err)
	require.Zero(t, cp.Cursor)
}

func TestStateFull(t *testing.T) {
	d := openTestState(t)
	require.NoError(t, d.Initialize(t.Context(), dbsqlc.InitializeParams{Source: testSource}))

	var pages int64
	require.NoError(t, d.sql.QueryRowContext(t.Context(), `PRAGMA page_count`).Scan(&pages))
	_, err := d.sql.ExecContext(t.Context(), fmt.Sprintf(`PRAGMA max_page_count = %d`, pages))
	require.NoError(t, err)
	err = d.Transaction(t.Context(), func(q *dbsqlc.Queries) error {
		err := q.Discover(t.Context(), dbsqlc.DiscoverParams{Path: strings.Repeat("x", 1<<20), SourceID: 1})
		if err != nil {
			return err
		}

		return q.Advance(t.Context(), dbsqlc.AdvanceParams{Cursor: 1})
	})

	var full *sqlite.Error
	require.ErrorAs(t, err, &full)
	require.Equal(t, 13, full.Code()) // SQLITE_FULL.
	cp, err := d.Checkpoint(t.Context())
	require.NoError(t, err)
	require.Zero(t, cp.Cursor)
	n, err := d.CountPending(t.Context())
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestMaintain(t *testing.T) {
	d := openTestState(t)

	paths := make([]string, 400)
	for i := range paths {
		paths[i] = fmt.Sprintf("%04d-%s", i, strings.Repeat("x", 1024))
	}

	require.NoError(t, d.Transaction(t.Context(), func(q *dbsqlc.Queries) error {
		for i, path := range paths {
			err := q.Discover(t.Context(), dbsqlc.DiscoverParams{Path: path, SourceID: int64(i)})
			if err != nil {
				return err
			}
		}

		return nil
	}))

	var before, after int64
	require.NoError(t, d.sql.QueryRowContext(t.Context(), `PRAGMA page_count`).Scan(&before))
	require.NoError(t, d.Transaction(t.Context(), func(q *dbsqlc.Queries) error {
		for _, path := range paths {
			err := q.Retire(t.Context(), path)
			if err != nil {
				return err
			}
		}

		return nil
	}))

	for range 10 {
		require.NoError(t, d.Maintain(t.Context()))
	}

	require.NoError(t, d.sql.QueryRowContext(t.Context(), `PRAGMA page_count`).Scan(&after))
	require.Less(t, after, before)
}

func TestOwnershipProcess(t *testing.T) {
	if path := os.Getenv("TSNIXCACHE_TEST_STATE"); path != "" {
		d, err := Open(t.Context(), path)
		if os.Getenv("TSNIXCACHE_TEST_BUSY") != "" {
			require.ErrorIs(t, err, ErrBusy)

			return
		}

		require.NoError(t, err)

		defer d.Close()

		_, err = fmt.Fprintln(os.Stdout, "ready")
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, os.Stdin)
		require.NoError(t, err)

		return
	}

	for _, killed := range []bool{false, true} {
		t.Run(strconv.FormatBool(killed), func(t *testing.T) {
			path := filepath.Join(privateDir(t), "watch.sqlite")

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			// #nosec G204 G702 -- re-executes this test binary.
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOwnershipProcess$")

			cmd.Env = append(os.Environ(), "TSNIXCACHE_TEST_STATE="+path)
			in, err := cmd.StdinPipe()
			require.NoError(t, err)
			out, err := cmd.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, cmd.Start())
			t.Cleanup(func() { _ = in.Close(); _ = cmd.Wait() })

			line, err := bufio.NewReader(out).ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, "ready\n", line)
			_, err = Open(t.Context(), path)
			require.ErrorIs(t, err, ErrBusy)

			if killed {
				require.NoError(t, cmd.Process.Kill())
			} else {
				require.NoError(t, in.Close())
			}

			err = cmd.Wait()
			if killed {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			d, err := Open(t.Context(), path)
			require.NoError(t, err)
			require.NoError(t, d.Close())
		})
	}
}

func TestTruncatedState(t *testing.T) {
	path := filepath.Join(privateDir(t), "watch.sqlite")
	d, err := Open(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, d.Initialize(t.Context(), dbsqlc.InitializeParams{Cursor: 123}))
	require.NoError(t, d.Discover(t.Context(), dbsqlc.DiscoverParams{Path: "pending", SourceID: 124}))
	require.NoError(t, d.Close())
	require.NoError(t, os.Truncate(path, 0))
	_, err = Open(t.Context(), path)
	require.ErrorIs(t, err, errEmptyState)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Zero(t, info.Size())
}

func TestStateDirectoryPermissions(t *testing.T) {
	dir := privateDir(t)
	require.NoError(t, os.Chmod(dir, 0o755)) // #nosec G302 -- exercises refusal of public state directories.
	_, err := Open(t.Context(), filepath.Join(dir, "watch.sqlite"))
	require.ErrorIs(t, err, errPrivateDirectory)
}

func privateDir(tb testing.TB) string {
	tb.Helper()
	dir := tb.TempDir()
	require.NoError(tb, os.Chmod(dir, 0o700)) // #nosec G302 -- directory needs owner traversal.

	return dir
}
