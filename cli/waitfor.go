// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/peterbourgon/ff/v4"

	"github.com/kradalby/tsnixcache/watch"
)

var errWaitForConfig = errors.New("wait-for: invalid session path, mode, timeout, or arguments")

func newWaitForCmd() *ff.Command {
	fs := ff.NewFlagSet("wait-for")
	path := fs.StringLong("pid-file", "", "session PID path written by watch (required)")
	ready := fs.BoolLong("ready", "wait for readiness and print the session identifier")
	stop := fs.BoolLong("stop", "request a final drain and wait for its result")
	token := fs.StringLong("session", "", "expected session identifier, as printed by --ready")
	startup := durationFlag(fs, "pid-file-timeout", 2*time.Minute, "session startup timeout (must be positive)")
	timeout := durationFlag(fs, "timeout", 2*time.Minute, "overall wait timeout (0 = until interrupted)")

	return &ff.Command{
		Name: "wait-for", Usage: "tsnixcache wait-for --pid-file <path> [--ready | --stop]", Flags: fs,
		ShortHelp: "Wait for verified watcher readiness or final delivery result.",
		Exec: func(ctx context.Context, args []string) error {
			if *path == "" || (*ready && *stop) || *timeout < 0 || *startup <= 0 || len(args) != 0 {
				return errWaitForConfig
			}

			if *timeout > 0 {
				var cancel context.CancelFunc

				ctx, cancel = context.WithTimeout(ctx, *timeout)
				defer cancel()
			}

			status, err := watch.WaitSession(ctx, *path, watch.WaitOptions{
				Ready: *ready, Stop: *stop, Session: *token, StartupTimeout: *startup,
			})
			if err != nil {
				return err
			}

			if *ready {
				fmt.Fprintln(os.Stdout, status.Session)
			}

			return nil
		},
	}
}
