// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	flagPIDFile     = "--pid-file"
	testSessionPath = "test"
)

func TestWaitForValidation(t *testing.T) {
	for _, args := range [][]string{nil, {flagPIDFile, testSessionPath, "--ready", "--stop"}, {flagPIDFile, testSessionPath, "--pid-file-timeout", "0"}, {flagPIDFile, testSessionPath, "--timeout", "-1s"}} {
		err := newWaitForCmd().ParseAndRun(t.Context(), args)
		require.Error(t, err)
	}
}

func TestWaitForRequiresStatus(t *testing.T) {
	for _, pid := range []string{"0", "-1", "broken", strconv.Itoa(os.Getpid())} {
		t.Run(pid, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "watch.pid")
			require.NoError(t, os.WriteFile(path, []byte(pid), 0o600))
			err := newWaitForCmd().ParseAndRun(t.Context(), []string{flagPIDFile, path, "--pid-file-timeout", "10ms"})
			require.ErrorIs(t, err, context.DeadlineExceeded)
		})
	}
}
