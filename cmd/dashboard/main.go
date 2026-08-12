// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// main writes the dashboard model to stdout and nothing else. stdout is the
// product — flake.nix redirects it straight into $out/tsnixcache.json — so every
// diagnostic goes to stderr, and any failure exits non-zero rather than emitting
// a partial document the build would happily capture.
//
// Indented rather than compact: the JSON is checked into consumers' repos and
// diffed there, and a single-line dashboard makes every change look total.
func main() {
	d, err := buildDashboard()
	if err != nil {
		fmt.Fprintln(os.Stderr, "build dashboard:", err)
		os.Exit(1)
	}

	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal dashboard:", err)
		os.Exit(1)
	}

	_, err = os.Stdout.Write(append(out, '\n'))
	if err != nil {
		fmt.Fprintln(os.Stderr, "write dashboard:", err)
		os.Exit(1)
	}
}
