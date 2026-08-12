// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"errors"
	"os"
	"testing"
)

// writeHashesFile writes a flakehashes.json body into the test's working dir.
func writeHashesFile(t *testing.T, body string) {
	t.Helper()

	err := os.WriteFile(hashesFile, []byte(body), 0o600)
	if err != nil {
		t.Fatalf("write %s: %v", hashesFile, err)
	}
}

func TestCheck(t *testing.T) {
	const current = "sha256-current"

	tests := []struct {
		name    string
		file    string
		wantErr error
	}{
		{
			name: "up to date",
			file: `{"vendor":{"sri":"sha256-current"}}`,
		},
		{
			// The regression: a fingerprint of go.mod/go.sum says only what
			// vendorhash last saw, so a recorded hash that no longer matches
			// the vendor tree must still fail.
			name:    "stale hash alongside a matching fingerprint",
			file:    `{"vendor":{"goModSum":"whatever","sri":"sha256-stale"}}`,
			wantErr: errStale,
		},
		{
			name:    "no hash recorded",
			file:    `{"vendor":{"sri":""}}`,
			wantErr: errStale,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeHashesFile(t, tt.file)

			err := check(func() (string, error) { return current, nil })
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("check: got %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// A missing flakehashes.json is stale, not OK.
func TestCheckMissingFile(t *testing.T) {
	t.Chdir(t.TempDir())

	err := check(func() (string, error) { return "sha256-current", nil })
	if !errors.Is(err, errStale) {
		t.Fatalf("check with no %s: got %v, want %v", hashesFile, err, errStale)
	}
}
