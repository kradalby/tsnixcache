// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"tailscale.com/envknob"

	"github.com/peterbourgon/ff/v4"

	"github.com/kradalby/tsnixcache/watch"
)

var (
	errWatchRetryConfig  = errors.New("watch: invalid idle/retry configuration or unexpected arguments")
	errWatchToRequired   = errors.New("watch: --to is required")
	errWatchAttempts     = errors.New("watch: --attempts must be at least 1")
	errWatchStallTimeout = errors.New("watch: --stall-timeout must be greater than 0")
	errWatchPollInterval = errors.New("watch: --poll-interval must be greater than 0")
)

func newWatchCmd() *ff.Command {
	fs := ff.NewFlagSet("watch")
	dbPath := fs.StringLong("db", "/nix/var/nix/db/db.sqlite", "path to Nix SQLite database")
	storeDir := fs.StringLong("store-dir", "/nix/store", "path to Nix store directory")
	to := fs.StringLong("to", "", "target cache URL (required)")
	idleExit := durationFlag(fs, "idle-exit", 0,
		"drain after this long without delivery progress (0 = run until explicitly stopped)")
	// Thirty seconds leaves room in the services' 90-second stop budget.
	drainTimeout := durationFlag(fs, "drain-timeout", 30*time.Second,
		"final drain deadline (0 = no deadline)")
	pollInterval := durationFlag(fs, "poll-interval", 30*time.Second, "DB poll interval (safety net; raise on battery)")
	pidFile := fs.StringLong("pid-file", "",
		"session PID path in a mode 0700 directory (mktemp -d); keeps status for wait-for")
	stateDir := fs.StringLong("state-dir", "",
		"persistent retry state directory, mode 0700 (default: user state directory)")
	verbose := fs.BoolLongDefault("verbose", false, "log per-path uploads at debug level")
	// Not disableable: upload substitutes its own 60s default for a zero.
	stallTimeout := durationFlag(fs, "stall-timeout", 60*time.Second,
		"cancel an upload with no progress for this long (must be greater than 0)")
	attempts := fs.IntLong("attempts", 2,
		"per-upload attempts within a poll, at least 1 (the retry queue handles longer-term retries)")
	retryBase := durationFlag(fs, "retry-backoff-base", 30*time.Second, "first retry-queue backoff interval")
	retryMax := durationFlag(fs, "retry-backoff-max", 10*time.Minute, "cap on any retry-queue backoff interval")
	retryMaxAge := durationFlag(fs, "retry-max-age", 2*time.Hour,
		"give up on a path failing for at least this long (0 = never give up)")
	retrySize := fs.IntLong("retry-queue-size", 512,
		"discovery and retry batch limit; overflow stays on disk (0 = bounded defaults)")

	return &ff.Command{
		Name:      "watch",
		Usage:     "tsnixcache watch --to <url> [--db <db>] [--store-dir <dir>]",
		ShortHelp: "Watch the Nix store for new paths and push them to a remote cache.",
		Flags:     fs,
		Exec: func(ctx context.Context, args []string) (retErr error) {
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			var session *watch.Session

			if *pidFile != "" {
				var err error

				session, err = watch.NewSession(ctx, *pidFile, cancel)
				if err != nil {
					return err
				}

				defer func() { retErr = session.Close(ctx, retErr) }()
			}

			if *verbose {
				slog.SetLogLoggerLevel(slog.LevelDebug)
			}

			if *to == "" {
				*to = envknob.String("TSNIXCACHE_URL")
			}

			if *to == "" {
				return errWatchToRequired
			}

			err := checkCacheURL(*to)
			if err != nil {
				return fmt.Errorf("watch: %w", err)
			}

			w := &watch.Watcher{
				StateDir:         *stateDir,
				DBPath:           *dbPath,
				StoreDir:         *storeDir,
				TargetURL:        *to,
				IdleExit:         *idleExit,
				DrainTimeout:     *drainTimeout,
				PollInterval:     *pollInterval,
				StallTimeout:     *stallTimeout,
				Attempts:         *attempts,
				RetryBackoffBase: *retryBase,
				RetryBackoffMax:  *retryMax,
				RetryMaxAge:      *retryMaxAge,
				RetryQueueSize:   *retrySize,
			}

			err = validateWatchOptions(w, args)
			if err != nil {
				return err
			}

			if session != nil {
				w.Ready, w.Report = session.Ready, session.Report
			}

			return w.Watch(ctx)
		},
	}
}

func validateWatchOptions(w *watch.Watcher, args []string) error {
	if w.Attempts < 1 {
		return errWatchAttempts
	}

	if w.StallTimeout <= 0 {
		return errWatchStallTimeout
	}

	if w.PollInterval <= 0 {
		return errWatchPollInterval
	}

	if w.IdleExit < 0 || w.DrainTimeout < 0 ||
		w.RetryBackoffBase <= 0 || w.RetryBackoffMax < w.RetryBackoffBase ||
		w.RetryMaxAge < 0 || w.RetryQueueSize < 0 || len(args) != 0 {
		return errWatchRetryConfig
	}

	return nil
}
