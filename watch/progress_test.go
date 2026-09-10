// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kradalby/tsnixcache/upload"
)

func TestProgressBindsAbsoluteState(t *testing.T) {
	f := newRetryFixture(t, func(_ context.Context, _ string, names []string) (upload.Summary, error) {
		return upload.Summary{Uploaded: len(names)}, nil
	})
	cwd, err := os.Getwd()
	require.NoError(t, err)
	f.w.StateDir, err = filepath.Rel(cwd, f.w.StateDir)
	require.NoError(t, err)
	f.reopen(t)

	var progress Progress

	f.w.Report = func(value Progress) error {
		progress = value

		return nil
	}
	require.NoError(t, f.p.finish(t.Context()))
	require.True(t, filepath.IsAbs(progress.StateFile))
	require.NoError(t, f.p.close())
	f.p = nil

	t.Chdir(t.TempDir())
	_, err = recoverDrain(t.Context(), progress)
	require.NoError(t, err)
}

func TestProgressDoesNotReusePreviousResult(t *testing.T) {
	f := newRetryFixture(t, func(context.Context, string, []string) (upload.Summary, error) { return upload.Summary{}, nil })
	require.NoError(t, f.p.finish(t.Context()))
	f.reopen(t)

	var progress Progress

	f.w.Report = func(value Progress) error {
		progress = value

		return nil
	}
	require.NoError(t, f.p.report(t.Context()))
	require.Zero(t, progress.Generation)
	require.Nil(t, progress.Boundary)
	_, err := recoverDrain(t.Context(), progress)
	require.ErrorIs(t, err, ErrAbandoned)
}
