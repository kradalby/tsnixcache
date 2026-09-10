// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/kradalby/tsnixcache/watch"
)

const (
	commandChild   = "TSNIXCACHE_COMMAND_TEST"
	startupFailure = "startup-failure"
)

// Each child executes the production command entrypoint, including signal and exit handling.
func TestWatchCommands(t *testing.T) {
	if args := os.Getenv(commandChild); args != "" {
		var values []string
		require.NoError(t, json.Unmarshal([]byte(args), &values))
		os.Exit(Main(values))
	}

	for _, mode := range []string{"success", startupFailure, "drain-failure"} {
		t.Run(mode, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "session")
			require.NoError(t, os.Mkdir(dir, 0o700))
			pid := filepath.Join(dir, "watch.pid")
			dbPath := filepath.Join(t.TempDir(), "db.sqlite")
			source, err := sql.Open("sqlite", dbPath)
			require.NoError(t, err)

			defer source.Close()

			_, err = source.ExecContext(t.Context(), `CREATE TABLE ValidPaths(id INTEGER PRIMARY KEY AUTOINCREMENT, path TEXT UNIQUE, narSize INTEGER)`)
			require.NoError(t, err)

			if mode == startupFailure {
				dbPath += ".missing"
			}

			args := []string{
				watchCommand, "--to", "http://127.0.0.1:1", flagDB, dbPath,
				flagStoreDir, t.TempDir(), "--state-dir", filepath.Join(t.TempDir(), "state"), flagPIDFile, pid,
				"--attempts", "1", "--poll-interval", "1h",
			}
			cmd := commandProcess(t, args...)

			var logs bytes.Buffer

			cmd.Stdout, cmd.Stderr = &logs, &logs
			require.NoError(t, cmd.Start())

			done := make(chan error, 1)

			startTestTask(t, func() { done <- cmd.Wait() })
			t.Cleanup(func() { _ = cmd.Process.Kill() })

			if mode == startupFailure {
				require.Error(t, <-done, "%s", logs.String())
				out, waitErr := commandProcess(t, "wait-for", flagPIDFile, pid, "--stop").CombinedOutput()
				require.Error(t, waitErr, "%s", out)

				status, statusErr := watch.ReadStatus(pid)
				require.NoError(t, statusErr)
				require.Equal(t, watch.SessionFailed, status.State)

				return
			}

			ready, err := commandProcess(t, "wait-for", flagPIDFile, pid, "--ready").CombinedOutput()
			require.NoError(t, err, "%s", ready)
			token := strings.TrimSpace(string(ready))
			require.Len(t, token, 64)

			if mode == "drain-failure" {
				_, err = source.ExecContext(t.Context(), `INSERT INTO ValidPaths(path) VALUES (?)`, "/nix/store/00000000000000000000000000000000-missing")
				require.NoError(t, err)
			}

			out, stopErr := commandProcess(t, "wait-for", flagPIDFile, pid, "--stop", "--session", token).CombinedOutput()
			watchErr := <-done

			if mode == "success" {
				require.NoError(t, stopErr, "%s", out)
				require.NoError(t, watchErr, "%s", logs.String())
			} else {
				require.Error(t, stopErr, "%s", out)
				require.Error(t, watchErr, "%s", logs.String())
			}
			// The retained result must also work after both processes have exited.
			_, lateErr := commandProcess(t, "wait-for", flagPIDFile, pid, "--session", token).CombinedOutput()
			require.Equal(t, stopErr == nil, lateErr == nil)
		})
	}
}

func commandProcess(tb testing.TB, args ...string) *exec.Cmd {
	tb.Helper()

	data, err := json.Marshal(args)
	require.NoError(tb, err)
	ctx, cancel := context.WithTimeout(tb.Context(), 45*time.Second)
	tb.Cleanup(cancel)
	// #nosec G204 G702 -- isolated command entrypoint in this test executable.
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWatchCommands$", "-test.timeout=40s")

	cmd.Env = append(os.Environ(), commandChild+"="+string(data))
	cmd.WaitDelay = time.Second

	return cmd
}

func startTestTask(tb testing.TB, fn func()) {
	tb.Helper()

	var group errgroup.Group
	group.Go(func() error {
		fn()

		return nil
	})
	tb.Cleanup(func() { require.NoError(tb, group.Wait()) })
}
