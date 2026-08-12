// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/peterbourgon/ff/v3/ffcli"
)

var (
	errWaitForPidFileRequired = errors.New("wait-for: --pid-file is required")
	errWaitForPidFileTimeout  = errors.New("wait-for: timed out waiting for the PID file to hold a PID")
)

// pidFileTimeout bounds the wait for the PID file only, not the watcher's run:
// the watcher writes the file as it starts, so if it has not appeared by now it
// never will (crashed, wrong path, never started) and blocking forever would
// hang the CI job it is supposed to drain.
const pidFileTimeout = 2 * time.Minute

func newWaitForCmd() *ffcli.Command {
	fs := flag.NewFlagSet("wait-for", flag.ExitOnError)
	pidFile := fs.String("pid-file", "", "PID file written by tsnixcache watch --pid-file (required)")
	timeout := durationFlag(fs, "pid-file-timeout", pidFileTimeout,
		"how long to wait for the PID file to appear (0 = forever)")

	return &ffcli.Command{
		Name:       "wait-for",
		ShortUsage: "tsnixcache wait-for --pid-file <path>",
		ShortHelp:  "Wait for a background tsnixcache watch process to exit (reads PID file).",
		FlagSet:    fs,
		Exec: func(ctx context.Context, args []string) error {
			if *pidFile == "" {
				return errWaitForPidFileRequired
			}

			return waitForPidFile(ctx, *pidFile, *timeout)
		},
	}
}

// waitForPidFile blocks until the process named in path exits. It polls for the
// file to appear first (handles the race where the background process hasn't
// written it yet when this step starts), giving up after timeout.
func waitForPidFile(ctx context.Context, path string, timeout time.Duration) error {
	pid, err := readPidFile(ctx, path, timeout)
	if err != nil {
		return err
	}

	for {
		if !processAlive(pid) {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// processAlive reports whether pid still exists. Signal 0 answers EPERM for a
// live process owned by another uid — the watcher under sudo or a systemd unit,
// wait-for in a user shell — and treating that as "exited" would make the drain
// step a silent no-op that tears the runner down mid-upload.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	err = proc.Signal(syscall.Signal(0))

	return err == nil || errors.Is(err, syscall.EPERM)
}

// readPidFile polls until path holds a PID, or timeout elapses (0 = forever).
func readPidFile(ctx context.Context, path string, timeout time.Duration) (int, error) {
	if timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	for {
		data, err := os.ReadFile(path) // #nosec G304 -- path is from a trusted CLI flag
		if err == nil {
			// Unparsable means "not written yet", not "broken": the file only
			// ever holds a pid, and a reader can land between the create and
			// the write of an older watch. Keep polling until the timeout
			// rather than turning a microsecond race into a failed CI step.
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr == nil {
				return pid, nil
			}
		} else if !os.IsNotExist(err) {
			return 0, fmt.Errorf("wait-for: read %s: %w", path, err)
		}

		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return 0, fmt.Errorf("%w: %s (waited %s)", errWaitForPidFileTimeout, path, timeout)
			}

			return 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
