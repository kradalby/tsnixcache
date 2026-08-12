// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/peterbourgon/ff/v3/ffcli"
	"tailscale.com/envknob"

	"github.com/kradalby/tsnixcache/watch"
)

var (
	errWatchToRequired   = errors.New("watch: --to is required")
	errWatchAttempts     = errors.New("watch: --attempts must be at least 1")
	errWatchStallTimeout = errors.New("watch: --stall-timeout must be greater than 0")
	errWatchPollInterval = errors.New("watch: --poll-interval must be greater than 0")
)

func newWatchCmd() *ffcli.Command {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	dbPath := fs.String("db", "/nix/var/nix/db/db.sqlite", "path to Nix SQLite database")
	storeDir := fs.String("store-dir", "/nix/store", "path to Nix store directory")
	to := fs.String("to", "", "target cache URL (required)")
	idleExit := durationFlag(fs, "idle-exit", 0,
		"exit after this duration with no new paths (0 = run forever; useful in CI)")
	pollInterval := durationFlag(fs, "poll-interval", 30*time.Second, "DB poll interval (safety net; raise on battery)")
	pidFile := fs.String("pid-file", "", "write PID to file on start and remove on exit (for use with wait-for)")
	verbose := fs.Bool("verbose", false, "log per-path uploads at debug level")
	// Not disableable: upload substitutes its own 60s default for a zero.
	stallTimeout := durationFlag(fs, "stall-timeout", 60*time.Second,
		"cancel an upload with no progress for this long (must be greater than 0)")
	attempts := fs.Int("attempts", 2,
		"per-upload attempts within a poll, at least 1 (the retry queue handles longer-term retries)")
	retryBase := durationFlag(fs, "retry-backoff-base", 30*time.Second, "first retry-queue backoff interval")
	retryMax := durationFlag(fs, "retry-backoff-max", 10*time.Minute, "cap on any retry-queue backoff interval")
	retryMaxAge := durationFlag(fs, "retry-max-age", 2*time.Hour,
		"give up on a path failing for at least this long (0 = never give up)")
	// Eviction is not a drop: the poll cursor rewinds below the evicted ids, so
	// a later poll finds them again.
	retrySize := fs.Int("retry-queue-size", 512,
		"max paths held for retry; past this the lowest store-db id is evicted and rediscovered later (0 = unbounded)")

	return &ffcli.Command{
		Name:       "watch",
		ShortUsage: "tsnixcache watch --to <url> [--db <db>] [--store-dir <dir>]",
		ShortHelp:  "Watch the Nix store for new paths and push them to a remote cache.",
		FlagSet:    fs,
		Exec: func(ctx context.Context, args []string) error {
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

			if *attempts < 1 {
				return fmt.Errorf("%w, got %d", errWatchAttempts, *attempts)
			}

			if *stallTimeout <= 0 {
				return errWatchStallTimeout
			}

			if *pollInterval <= 0 {
				return errWatchPollInterval
			}

			if *pidFile != "" {
				err := writePidFile(*pidFile, os.Getpid())
				if err != nil {
					return fmt.Errorf("watch: write pid file: %w", err)
				}
				defer os.Remove(*pidFile)
			}

			return (&watch.Watcher{
				DBPath:           *dbPath,
				StoreDir:         *storeDir,
				TargetURL:        *to,
				IdleExit:         *idleExit,
				PollInterval:     *pollInterval,
				StallTimeout:     *stallTimeout,
				Attempts:         *attempts,
				RetryBackoffBase: *retryBase,
				RetryBackoffMax:  *retryMax,
				RetryMaxAge:      *retryMaxAge,
				RetryQueueSize:   *retrySize,
			}).Watch(ctx)
		},
	}
}

// writePidFile writes pid to path atomically. wait-for polls the path every
// 100ms, so a plain create-then-write lets it read the file in between and see
// an empty one.
func writePidFile(path string, pid int) error {
	// Same directory, so the rename stays on one filesystem.
	f, err := os.CreateTemp(filepath.Dir(path), ".tsnixcache-pid-*")
	if err != nil {
		return err
	}

	name := f.Name()

	_, err = fmt.Fprintf(f, "%d\n", pid)

	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}

	if err == nil {
		err = os.Rename(name, path)
	}

	if err != nil {
		_ = os.Remove(name)

		return err
	}

	return nil
}
