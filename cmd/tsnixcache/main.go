// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/peterbourgon/ff/v3/ffcli"

	"github.com/kradalby/tsnixcache/cache"
)

var errUnknownSubcommand = errors.New("unknown subcommand")

// newRootCmd builds the command tree. Separate from main so a test can run it
// without exiting the process.
func newRootCmd() *ffcli.Command {
	fs := flag.NewFlagSet("tsnixcache", flag.ExitOnError)
	showVersion := fs.Bool("version", false, "print the version and exit")

	root := &ffcli.Command{
		ShortUsage: "tsnixcache <subcommand>",
		FlagSet:    fs,
		Subcommands: []*ffcli.Command{
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

		return flag.ErrHelp
	}

	return root
}

func newVersionCmd() *ffcli.Command {
	return &ffcli.Command{
		Name:       "version",
		ShortUsage: "tsnixcache version",
		ShortHelp:  "Print the version.",
		Exec: func(_ context.Context, _ []string) error {
			fmt.Fprintln(os.Stdout, cache.Version)

			return nil
		},
	}
}

// Exit codes: 2 for a usage error (ffcli has already printed the usage text),
// 130 for a run cut short by Ctrl-C, 1 for everything else.
const (
	exitUsage     = 2
	exitInterrupt = 130
	exitError     = 1
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	// Uninstall the handler as soon as the first signal lands, so a second
	// Ctrl-C (or SIGKILL-before-timeout from a supervisor) takes the default
	// action and kills a shutdown that has wedged, instead of being swallowed.
	go func() {
		<-ctx.Done()
		stop()
	}()

	err := newRootCmd().ParseAndRun(ctx, os.Args[1:])

	// Read before stop(): stop() cancels the context too, so afterwards there
	// is no telling a signal from an ordinary exit.
	interrupted := ctx.Err() != nil

	stop()

	switch {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
		// ffcli printed the usage text already; the wrapped sentinel would only
		// add "error: flag: help requested".
		os.Exit(exitUsage)
	case interrupted:
		// Whatever the command reported, it reported it because it was cut
		// short — "paths failed: 1 of 4" is a symptom of Ctrl-C, not a result.
		fmt.Fprintln(os.Stderr, "interrupted")
		os.Exit(exitInterrupt)
	default:
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(exitError)
	}
}
