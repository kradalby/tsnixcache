// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package flakehash maintains the Nix hash of Go's vendor tree.
package flakehash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/peterbourgon/ff/v4"
	"tailscale.com/cmd/nardump/nardump"
)

const hashesFile = "flakehashes.json"

// ErrStale means the recorded hash differs from the current vendor tree.
var ErrStale = errors.New("flakehashes.json is stale: run `go run ./cmd/vendorhash update`")

type flakeHashes struct {
	Vendor vendorBlock `json:"vendor"`
}

type vendorBlock struct {
	SRI string `json:"sri"`
}

// Main executes vendorhash and returns its process exit code.
func Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := Run(ctx, args, os.Stdout, os.Stderr)
	if err == nil {
		return 0
	}

	if errors.Is(err, ff.ErrHelp) || errors.Is(err, errUsage) {
		fmt.Fprintln(os.Stderr, "usage: vendorhash <check|update>")

		return 2
	}

	slog.Error("vendor hash", "err", err)

	return 1
}

var errUsage = errors.New("vendorhash: expected check or update without arguments")

// Run checks or updates flakehashes.json in the current directory.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	command := &ff.Command{
		Name:  "vendorhash",
		Usage: "vendorhash <check|update>",
		Flags: ff.NewFlagSet("vendorhash"),
		Exec: func(_ context.Context, args []string) error {
			if len(args) == 0 {
				return ff.ErrHelp
			}

			return errUsage
		},
	}
	for _, name := range []string{"check", "update"} {
		command.Subcommands = append(command.Subcommands, &ff.Command{
			Name:  name,
			Flags: ff.NewFlagSet(name),
			Exec: func(ctx context.Context, args []string) error {
				if len(args) != 0 {
					return errUsage
				}

				if name == "check" {
					return check(func() (string, error) { return computeVendorSRI(ctx, stderr) })
				}

				return update(ctx, stdout, stderr)
			},
		})
	}

	return command.ParseAndRun(ctx, args)
}

// computeVendorSRI runs `go mod vendor` into a temporary directory and
// returns the Nix SRI hash of the resulting tree. This produces the
// same hash that Nix uses as vendorHash in buildGoModule.
func computeVendorSRI(ctx context.Context, stderr io.Writer) (string, error) {
	out, err := os.MkdirTemp("", "nar-vendor-")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	// `go mod vendor -o` requires the destination to not already exist.
	err = os.Remove(out)
	if err != nil {
		return "", fmt.Errorf("remove temp dir placeholder: %w", err)
	}
	defer os.RemoveAll(out) //nolint:errcheck

	cmd := exec.CommandContext(ctx, "go", "mod", "vendor", "-o", out) // #nosec G204

	cmd.Env = append(os.Environ(), "GOWORK=off")
	cmd.Stderr = stderr

	err = cmd.Run()
	if err != nil {
		return "", fmt.Errorf("go mod vendor: %w", err)
	}

	return nardump.SRI(os.DirFS(out))
}

func readHashes() (*flakeHashes, error) {
	data, err := os.ReadFile(hashesFile)
	if errors.Is(err, os.ErrNotExist) {
		return &flakeHashes{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read %s: %w", hashesFile, err)
	}

	var h flakeHashes

	err = json.Unmarshal(data, &h)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", hashesFile, err)
	}

	return &h, nil
}

func writeHashes(h *flakeHashes) error {
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	data = append(data, '\n')

	err = os.WriteFile(hashesFile, data, 0o600)
	if err != nil {
		return fmt.Errorf("write %s: %w", hashesFile, err)
	}

	return nil
}

// Recompute: a manifest fingerprint does not establish the vendor tree's hash.
func check(computeSRI func() (string, error)) error {
	h, err := readHashes()
	if err != nil {
		return err
	}

	sri, err := computeSRI()
	if err != nil {
		return err
	}

	if h.Vendor.SRI != sri {
		return ErrStale
	}

	return nil
}

func update(ctx context.Context, stdout, stderr io.Writer) error {
	sri, err := computeVendorSRI(ctx, stderr)
	if err != nil {
		return err
	}

	h := &flakeHashes{
		Vendor: vendorBlock{SRI: sri},
	}

	err = writeHashes(h)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "updated %s: %s\n", hashesFile, sri)

	return err
}
