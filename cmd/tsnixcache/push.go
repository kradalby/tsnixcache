package main

import (
	"context"
	"flag"
	"fmt"
	"os/exec"

	"github.com/peterbourgon/ff/v3/ffcli"
)

func newPushCmd() *ffcli.Command {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	to := fs.String("to", "", "target cache URL (required)")

	return &ffcli.Command{
		Name:       "push",
		ShortUsage: "tsnixcache push --to <url> <path>...",
		ShortHelp:  "Push store paths to a remote cache via nix copy.",
		FlagSet:    fs,
		Exec: func(ctx context.Context, args []string) error {
			if *to == "" {
				return fmt.Errorf("push: --to is required")
			}
			if len(args) == 0 {
				return fmt.Errorf("push: at least one store path is required")
			}
			return runPush(ctx, *to, args)
		},
	}
}

func runPush(ctx context.Context, targetURL string, paths []string) error {
	cmdArgs := append([]string{"copy", "--to", targetURL}, paths...)
	cmd := exec.CommandContext(ctx, "nix", cmdArgs...)
	cmd.Stdout = nil
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("push: nix copy: %w\n%s", err, out)
	}
	return nil
}
