// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package cli implements the tsnixcache command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/ffhelp"
	"golang.org/x/sync/errgroup"

	"github.com/kradalby/tsnixcache/cache"
)

var errUnknownSubcommand = errors.New("unknown subcommand")

func newRootCmd() *ff.Command {
	fs := ff.NewFlagSet("tsnixcache")
	showVersion := fs.BoolLongDefault("version", false, "print the version and exit")

	root := &ff.Command{
		Name:  "tsnixcache",
		Usage: "tsnixcache <subcommand>",
		Flags: fs,
		Subcommands: []*ff.Command{
			newServeCmd(),
			newPushCmd(),
			newWatchCmd(),
			newWaitForCmd(),
			newKeyCmd(),
			newGCCmd(),
			newVersionCmd(),
		},
	}

	root.Exec = func(_ context.Context, args []string) error {
		if *showVersion {
			fmt.Fprintln(os.Stdout, cache.Version)

			return nil
		}

		// Parse only reaches Exec when args[0] matched no subcommand, so name
		// the word: the bare usage dump left the operator to spot their own
		// typo.
		if len(args) > 0 {
			return fmt.Errorf("%w %q", errUnknownSubcommand, args[0])
		}

		return ff.ErrHelp
	}

	return root
}

func newVersionCmd() *ff.Command {
	return &ff.Command{
		Name:      "version",
		Usage:     "tsnixcache version",
		ShortHelp: "Print the version.",
		Exec: func(_ context.Context, _ []string) error {
			fmt.Fprintln(os.Stdout, cache.Version)

			return nil
		},
	}
}

// Main executes the CLI and returns its process exit code.
func Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	var group errgroup.Group
	group.Go(func() error {
		<-ctx.Done()
		stop()

		return nil
	})

	cmd := newRootCmd()
	err := cmd.ParseAndRun(ctx, args)
	interrupted := ctx.Err() != nil

	stop()

	_ = group.Wait()

	switch {
	case err == nil:
		return 0
	case errors.Is(err, ff.ErrHelp):
		_, _ = ffhelp.Command(cmd).WriteTo(os.Stderr)

		return 2
	case interrupted:
		fmt.Fprintln(os.Stderr, "interrupted")

		return 130
	default:
		fmt.Fprintf(os.Stderr, "error: %v\n", err)

		return 1
	}
}
