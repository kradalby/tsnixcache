// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package gcroot

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

type failingDirectory struct {
	syncErr  error
	closeErr error
	closed   bool
}

func (f *failingDirectory) Sync() error { return f.syncErr }
func (f *failingDirectory) Close() error {
	f.closed = true

	return f.closeErr
}

func TestSyncDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, SyncDir(dir))
	require.ErrorIs(t, SyncDir(filepath.Join(dir, "absent")), os.ErrNotExist)

	syncErr := syscall.EIO

	closeErr := syscall.EBADF
	for _, test := range []struct {
		name     string
		syncErr  error
		closeErr error
	}{
		{name: "sync", syncErr: syncErr},
		{name: "close", closeErr: closeErr},
		{name: "both", syncErr: syncErr, closeErr: closeErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &failingDirectory{syncErr: test.syncErr, closeErr: test.closeErr}
			err := syncAndClose(f)
			require.True(t, f.closed)

			if test.syncErr != nil {
				require.ErrorIs(t, err, test.syncErr)
			}

			if test.closeErr != nil {
				require.ErrorIs(t, err, test.closeErr)
			}
		})
	}
}

func TestMakeDirPersistsParentsBeforeChildren(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "new", "roots")
	injected := syscall.EIO

	var synced []string

	restore := SetSyncDirForTesting(func(path string) error {
		synced = append(synced, path)
		if path == parent {
			return injected
		}

		return syncDirectory(path)
	})
	t.Cleanup(restore)

	_, err := TryAcquire(dir, "key")
	require.ErrorIs(t, err, injected)
	_, err = os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, []string{parent}, synced)
	restore()

	lock, err := TryAcquire(dir, "key")
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	require.DirExists(t, filepath.Join(dir, LockDir))
}
