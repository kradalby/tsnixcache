package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/peterbourgon/ff/v3/ffcli"

	"github.com/kradalby/tsnixcache/watch"
)

func newWatchCmd() *ffcli.Command {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	dbPath := fs.String("db", "/nix/var/nix/db/db.sqlite", "path to Nix SQLite database")
	storeDir := fs.String("store-dir", "/nix/store", "path to Nix store directory")
	to := fs.String("to", "", "target cache URL (required)")

	return &ffcli.Command{
		Name:       "watch",
		ShortUsage: "tsnixcache watch --to <url> [--db <db>] [--store-dir <dir>]",
		ShortHelp:  "Watch the Nix store for new paths and push them to a remote cache.",
		FlagSet:    fs,
		Exec: func(ctx context.Context, args []string) error {
			if *to == "" {
				return fmt.Errorf("watch: --to is required")
			}
			return watch.Watch(ctx, *dbPath, *storeDir, *to)
		},
	}
}
