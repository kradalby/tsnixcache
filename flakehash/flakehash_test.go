// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package flakehash

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/peterbourgon/ff/v4"
	"github.com/stretchr/testify/require"
)

// writeHashesFile writes a flakehashes.json body into the test's working dir.
func writeHashesFile(t *testing.T, body string) {
	t.Helper()

	err := os.WriteFile(hashesFile, []byte(body), 0o600)
	require.NoError(t, err)
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
			wantErr: ErrStale,
		},
		{
			name:    "no hash recorded",
			file:    `{"vendor":{"sri":""}}`,
			wantErr: ErrStale,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeHashesFile(t, tt.file)

			err := check(func() (string, error) { return current, nil })
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

// A missing flakehashes.json is stale, not OK.
func TestCheckMissingFile(t *testing.T) {
	t.Chdir(t.TempDir())

	err := check(func() (string, error) { return "sha256-current", nil })
	require.ErrorIs(t, err, ErrStale)
}

func TestRunArguments(t *testing.T) {
	for _, args := range [][]string{{"unknown"}, {"check", "extra"}, {"update", "extra"}} {
		require.ErrorIs(t, Run(t.Context(), args, io.Discard, io.Discard), errUsage)
	}

	require.ErrorIs(t, Run(t.Context(), nil, io.Discard, io.Discard), ff.ErrHelp)
}

func TestRunCanceled(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, Run(ctx, []string{"check"}, io.Discard, io.Discard), context.Canceled)
}
