// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package main implements vendorhash: keeps flakehashes.json in sync with go.mod/go.sum.
//
// Usage:
//
//	go run ./cmd/vendorhash check   – exits non-zero if flakehashes.json is stale
//	go run ./cmd/vendorhash update  – recomputes and rewrites flakehashes.json
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"tailscale.com/cmd/nardump/nardump"
)

const hashesFile = "flakehashes.json"

var errStale = errors.New("flakehashes.json is stale: run `go run ./cmd/vendorhash update`")

type flakeHashes struct {
	Vendor vendorBlock `json:"vendor"`
}

type vendorBlock struct {
	SRI string `json:"sri"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: vendorhash <check|update>")
		os.Exit(2)
	}

	var err error

	switch os.Args[1] {
	case "check":
		err = cmdCheck()
	case "update":
		err = cmdUpdate()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// computeVendorSRI runs `go mod vendor` into a temporary directory and
// returns the Nix SRI hash of the resulting tree. This produces the
// same hash that Nix uses as vendorHash in buildGoModule.
func computeVendorSRI() (string, error) {
	ctx := context.Background()

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
	cmd.Stderr = os.Stderr

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

func cmdCheck() error {
	return check(computeVendorSRI)
}

// check compares the recorded vendor hash with a freshly computed one. There
// is deliberately no fingerprint shortcut: a recorded fingerprint only says
// what vendorhash last saw, not what the vendor tree hashes to now, so
// trusting it let a stale flakehashes.json pass CI. computeSRI is a parameter
// so the test can exercise this without running `go mod vendor`.
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
		return errStale
	}

	return nil
}

func cmdUpdate() error {
	sri, err := computeVendorSRI()
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

	fmt.Fprintf(os.Stdout, "updated %s: %s\n", hashesFile, sri)

	return nil
}
