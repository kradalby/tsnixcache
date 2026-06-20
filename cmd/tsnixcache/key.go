package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/peterbourgon/ff/v3/ffcli"

	"github.com/kradalby/tsnixcache/signing"
)

func newKeyCmd() *ffcli.Command {
	return &ffcli.Command{
		Name:        "key",
		ShortUsage:  "tsnixcache key <subcommand>",
		ShortHelp:   "Key management subcommands.",
		Subcommands: []*ffcli.Command{newKeyGenerateCmd()},
		Exec: func(ctx context.Context, args []string) error {
			return flag.ErrHelp
		},
	}
}

func newKeyGenerateCmd() *ffcli.Command {
	fs := flag.NewFlagSet("key generate", flag.ExitOnError)
	name := fs.String("name", "tsnixcache", "key name (used as cache.example.com)")

	return &ffcli.Command{
		Name:       "generate",
		ShortUsage: "tsnixcache key generate [--name <name>]",
		ShortHelp:  "Generate a new ed25519 signing keypair.",
		FlagSet:    fs,
		Exec: func(ctx context.Context, args []string) error {
			sk, pk, err := signing.GenerateKey(*name)
			if err != nil {
				return err
			}
			fmt.Printf("private: %s\n", sk.String())
			fmt.Printf("public:  %s\n", pk.String())
			return nil
		},
	}
}
