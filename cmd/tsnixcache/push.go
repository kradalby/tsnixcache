// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/peterbourgon/ff/v3/ffcli"
	"golang.org/x/term"
	"tailscale.com/envknob"

	"github.com/kradalby/tsnixcache/humanise"
	"github.com/kradalby/tsnixcache/upload"
)

var (
	errPushToRequired   = errors.New("push: --to is required")
	errPushPathRequired = errors.New("push: at least one store path is required")
	errPushJobs         = errors.New("push: --jobs must be at least 1")
	errPushAttempts     = errors.New("push: --attempts must be at least 1")
	errPushStallTimeout = errors.New("push: --stall-timeout must be greater than 0")
	errPushTimedOut     = errors.New("push: --timeout expired")
	errCacheURL         = errors.New("--to must be an http:// or https:// URL")
)

func newPushCmd() *ffcli.Command {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	to := fs.String("to", "", "target cache URL (required)")
	attempts := fs.Int("attempts", 5, "max attempts per path before giving up (at least 1)")
	jobs := fs.Int("jobs", 8, "max concurrent path uploads (at least 1)")
	// Not disableable: upload substitutes its own 60s default for a zero, so a
	// "0 = off" here would silently mean 60s.
	stallTimeout := durationFlag(fs, "stall-timeout", 60*time.Second,
		"cancel an upload with no progress for this long (must be greater than 0)")
	// No overall deadline by default: a manual push of a large closure must run
	// to completion, not be killed mid-copy. The post-build-hook sets a bounded
	// best-effort deadline explicitly.
	timeout := durationFlag(fs, "timeout", 0, "overall deadline across all paths (0 = none)")
	noProgress := fs.Bool("no-progress", false, "disable the interactive progress bars even on a terminal")

	return &ffcli.Command{
		Name:       "push",
		ShortUsage: "tsnixcache push --to <url> <path>... (\"-\" reads paths from stdin)",
		ShortHelp:  "Push store paths and their closure to a remote cache.",
		FlagSet:    fs,
		Exec: func(ctx context.Context, args []string) error {
			if *to == "" {
				*to = envknob.String("TSNIXCACHE_URL")
			}

			if *to == "" {
				return errPushToRequired
			}

			err := checkCacheURL(*to)
			if err != nil {
				return fmt.Errorf("push: %w", err)
			}

			if *jobs < 1 {
				return fmt.Errorf("%w, got %d", errPushJobs, *jobs)
			}

			if *attempts < 1 {
				return fmt.Errorf("%w, got %d", errPushAttempts, *attempts)
			}

			if *stallTimeout <= 0 {
				return errPushStallTimeout
			}

			paths, err := pathArgs(args)
			if err != nil {
				return err
			}

			if len(paths) == 0 {
				return errPushPathRequired
			}

			return runPush(ctx, *to, paths, upload.Options{
				Jobs:         *jobs,
				Attempts:     *attempts,
				StallTimeout: *stallTimeout,
			}, *timeout, progressEnabled(*noProgress, os.Stderr.Fd()))
		},
	}
}

// pathArgs returns the store paths to push, expanding the conventional "-"
// argument into the lines on stdin so a closure can be piped in:
//
//	nix-store -qR /run/current-system | tsnixcache push --to http://cache -
func pathArgs(args []string) ([]string, error) {
	paths := make([]string, 0, len(args))

	for _, arg := range args {
		if arg != "-" {
			paths = append(paths, arg)

			continue
		}

		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				paths = append(paths, line)
			}
		}

		err := scanner.Err()
		if err != nil {
			return nil, fmt.Errorf("push: read paths from stdin: %w", err)
		}
	}

	return paths, nil
}

// checkCacheURL rejects a --to that is not an absolute http(s) URL. Without it a
// typo like "localhost:5000" is only caught per path, after the whole closure
// has been resolved — and watch repeats that on every poll, forever.
func checkCacheURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w, got %q: %w", errCacheURL, raw, err)
	}

	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w, got %q", errCacheURL, raw)
	}

	return nil
}

// durationFlag registers a duration flag that also accepts the "d" (days)
// suffix the NixOS module documents, so "--retry-max-age 1d" parses like
// --gc-interval already does. A bare "0" stays legal for the flags where it
// means "off"; everything else must be positive.
func durationFlag(fs *flag.FlagSet, name string, def time.Duration, usage string) *time.Duration {
	d := def
	fs.Var(dayDuration{&d}, name, usage)

	return &d
}

type dayDuration struct{ d *time.Duration }

func (v dayDuration) String() string {
	if v.d == nil || *v.d == 0 {
		return "0"
	}

	return formatDuration(*v.d)
}

func (v dayDuration) Set(s string) error {
	if s == "0" {
		*v.d = 0

		return nil
	}

	d, err := parseDurationString(s)
	if err != nil {
		return err
	}

	*v.d = d

	return nil
}

// progressEnabled reports whether to draw interactive bars: only when not
// disabled and stderr (where bars render) is a terminal of a usable width. A
// terminal reporting 0 columns — "script" without a controlling terminal, some
// pty-allocating CI runners — renders zero-width bars, i.e. nothing at all,
// while the per-path logs stay suppressed, so a long push looks hung.
func progressEnabled(noProgress bool, fd uintptr) bool {
	if noProgress || !isatty.IsTerminal(fd) {
		return false
	}

	width, _, err := term.GetSize(int(fd)) // #nosec G115 -- an fd always fits an int

	return err == nil && width > 0
}

func runPush(
	ctx context.Context,
	targetURL string,
	paths []string,
	opts upload.Options,
	timeout time.Duration,
	progress bool,
) error {
	if timeout > 0 {
		var cancel context.CancelFunc

		// WithTimeoutCause so an expiry reads like its siblings ("upload
		// stalled…", "…within the import deadline") instead of the bare
		// "context deadline exceeded".
		ctx, cancel = context.WithTimeoutCause(ctx, timeout, errPushTimedOut)
		defer cancel()
	}

	if progress {
		return runPushWithBars(ctx, targetURL, paths, opts)
	}

	// Plain mode: slog per-path (concurrency-safe) as each finishes, then summary.
	opts.OnPath = logPath

	_, sum, err := upload.Closure(ctx, targetURL, paths, opts)

	return report(ctx, sum, err)
}

// report logs the batch summary and turns a push failure into the returned
// error. The summary is skipped when nothing ran — a closure that failed to
// resolve would otherwise print "push: done paths=0" right before the error
// explaining why there were none.
func report(ctx context.Context, sum upload.Summary, err error) error {
	if sum.Paths > 0 {
		logSummary(sum)
	}

	if err == nil {
		return nil
	}

	// The deadline names itself: ctx.Err() is the opaque "context deadline
	// exceeded", the cause says which deadline it was.
	cause := context.Cause(ctx)
	if errors.Is(cause, errPushTimedOut) {
		return fmt.Errorf("%w after %s", errPushTimedOut, sum.Wall.Round(time.Millisecond))
	}

	return fmt.Errorf("push: %w", err)
}

// runPushWithBars runs the push with live per-path progress bars, logging the
// summary and any failures only after rendering stops (to avoid corrupting it).
func runPushWithBars(ctx context.Context, targetURL string, paths []string, opts upload.Options) error {
	r := newBarRenderer(ctx, os.Stderr)
	opts.OnStart = r.start
	opts.OnProgress = r.progress
	opts.OnPath = r.done

	_, sum, err := upload.Closure(ctx, targetURL, paths, opts)

	for _, s := range r.wait() {
		slog.Warn("push: failed", "path", s.Path, "err", s.Err)
	}

	return report(ctx, sum, err)
}

// logPath logs one path's transfer stats (failed, already-present, or uploaded).
func logPath(s upload.Stats) {
	switch {
	case s.Err != nil:
		slog.Warn("push: failed", "path", s.Path, "err", s.Err)

		return
	case s.Skipped:
		slog.Info("push: present", "path", s.Path, "size", humanise.Bytes(s.NarSize))

		return
	}

	slog.Info(
		"push: uploaded",
		"path", s.Path,
		"size", humanise.Bytes(s.NarSize),
		"dur", s.Duration.Round(time.Millisecond),
		"avg_upload", humanise.Bitrate(s.AvgBps),
		"peak_upload", humanise.Bitrate(s.PeakBps),
	)
}

// logSummary logs the aggregate line for a push.
func logSummary(sum upload.Summary) {
	avg := 0.0
	if sum.Wall > 0 {
		avg = float64(sum.WireBytes) / sum.Wall.Seconds()
	}

	slog.Info(
		"push: done",
		"paths", sum.Paths,
		"uploaded", sum.Uploaded,
		"skipped", sum.Skipped,
		"bytes", humanise.Bytes(sum.NarBytes),
		"dur", sum.Wall.Round(time.Millisecond),
		"avg_upload", humanise.Bitrate(avg),
		"peak_upload", humanise.Bitrate(sum.PeakBps),
	)
}
