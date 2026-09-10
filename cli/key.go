// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/peterbourgon/ff/v4"

	"github.com/kradalby/tsnixcache/signing"
)

var errKeyPublicArgs = errors.New("key public: expected exactly one key file argument")

func newKeyCmd() *ff.Command {
	return &ff.Command{
		Name:        "key",
		Usage:       "tsnixcache key <subcommand>",
		ShortHelp:   "Key management subcommands.",
		Subcommands: []*ff.Command{newKeyGenerateCmd(), newKeyPublicCmd()},
		Exec: func(ctx context.Context, args []string) error {
			return ff.ErrHelp
		},
	}
}

func newKeyGenerateCmd() *ff.Command {
	fs := ff.NewFlagSet("key generate")
	name := fs.StringLong("name", "tsnixcache", "key name (used as cache.example.com)")

	return &ff.Command{
		Name:      "generate",
		Usage:     "tsnixcache key generate [--name <name>] > /etc/tsnixcache/key",
		ShortHelp: "Generate a new ed25519 signing keypair (secret key to stdout, public key to stderr).",
		Flags:     fs,
		Exec: func(ctx context.Context, args []string) error {
			return generateKey(*name, os.Stdout, os.Stderr)
		},
	}
}

// generateKey writes the secret key, and nothing else, to out: the documented
// setup is `tsnixcache key generate > /etc/tsnixcache/key`, so anything else on
// stdout lands in the key file and the server fails to parse it. The public key
// goes to info, which the same redirection leaves on the terminal.
func generateKey(name string, out, info io.Writer) error {
	sk, pk, err := signing.GenerateKey(name)
	if err != nil {
		return err
	}

	// What stdout is decides what care the secret needs, and the care has to
	// come first: the key file exists at the caller's umask, typically
	// world-readable, from the moment the shell opens it, so tightening it
	// after the write leaves the secret exposed for the length of the write.
	file, isFile := out.(*os.File)
	if isFile {
		st, statErr := file.Stat()

		switch {
		case statErr != nil:
			fmt.Fprintf(info, "warning: could not inspect stdout, so could not restrict permissions: %v\n", statErr)
		case st.Mode().IsRegular():
			chmodErr := file.Chmod(0o600)
			if chmodErr != nil {
				fmt.Fprintf(info, "warning: could not restrict permissions on the key file: %v\n", chmodErr)
			}
		case isatty.IsTerminal(file.Fd()):
			fmt.Fprintln(info, "warning: writing a secret key to the terminal; redirect stdout to a file")
		}
	}

	_, err = fmt.Fprintln(out, sk.String())
	if err != nil {
		return fmt.Errorf("key generate: write secret key: %w", err)
	}

	fmt.Fprintf(info, "public key: %s\n", pk.String())

	return nil
}

func newKeyPublicCmd() *ff.Command {
	return &ff.Command{
		Name:      "public",
		Usage:     "tsnixcache key public <key-file>",
		ShortHelp: "Print the public key for a secret key file.",
		Exec: func(ctx context.Context, args []string) error {
			if len(args) != 1 {
				return errKeyPublicArgs
			}

			return printPublicKey(args[0], os.Stdout)
		},
	}
}

// printPublicKey derives the public key from a secret key file and prints it in
// the form clients put in trusted-public-keys.
func printPublicKey(path string, out io.Writer) error {
	data, err := os.ReadFile(path) // #nosec G304 -- path is from a trusted CLI argument
	if err != nil {
		return fmt.Errorf("key public: read %s: %w", path, err)
	}

	sk, err := signing.ParseSecretKey(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("key public: parse %s: %w", path, err)
	}

	fmt.Fprintln(out, sk.Public().String())

	return nil
}
