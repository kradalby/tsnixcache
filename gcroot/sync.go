// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package gcroot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var syncDir = syncDirectory

// SyncDir persists directory entries before their mutation is acknowledged.
func SyncDir(path string) error { return syncDir(path) }

// SetSyncDirForTesting overrides directory syncing. Call only in serial tests.
func SetSyncDirForTesting(fn func(string) error) func() {
	previous := syncDir
	syncDir = fn

	return func() { syncDir = previous }
}

func syncDirectory(path string) error {
	f, err := os.Open(path) // #nosec G304 G703 -- trusted configured root directory.
	if err != nil {
		return fmt.Errorf("gcroot: open directory %s: %w", path, err)
	}

	err = syncAndClose(f)
	if err != nil {
		return fmt.Errorf("gcroot: persist directory %s: %w", path, err)
	}

	return nil
}

func syncAndClose(f interface {
	Sync() error
	Close() error
},
) error {
	return errors.Join(f.Sync(), f.Close())
}

// Persist each new parent before creating a child that depends on it.
func makeDir(path string) error {
	path = filepath.Clean(path)
	parent := filepath.Dir(path)

	err := os.Mkdir(path, 0o750) // #nosec G301 G703 -- trusted configured root directory.
	if errors.Is(err, os.ErrNotExist) {
		err = makeDir(parent)
		if err != nil {
			return err
		}

		err = os.Mkdir(path, 0o750) // #nosec G301 G703 -- trusted configured root directory.
	}

	if errors.Is(err, os.ErrExist) {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return statErr
		}

		if !info.IsDir() {
			return fmt.Errorf("gcroot: directory %s: %w", path, err)
		}

		err = nil
	}

	if err != nil {
		return fmt.Errorf("gcroot: create directory %s: %w", path, err)
	}

	// Another process may have created this entry but not synced its parent.
	return SyncDir(parent)
}
