// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package gcroot

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const (
	phaseLinked    = "linked"
	phaseLegacy    = "legacy"
	phasePublished = "published"
)

func stripePath(dir string) string {
	sum := sha256.Sum256([]byte("path"))

	return filepath.Join(dir, LockDir, fmt.Sprintf("v1-%02x", sum[0]))
}

func TestOwnershipRejectsRedirectedMetadata(t *testing.T) {
	for _, kind := range []string{"directory-symlink", "stripe-symlink", "stripe-hardlink", "stripe-fifo", "stripe-directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			victim := filepath.Join(t.TempDir(), "victim")
			require.NoError(t, os.WriteFile(victim, []byte("unchanged"), 0o640)) // #nosec G306 -- permissions must remain unchanged.
			before, err := os.Stat(victim)
			require.NoError(t, err)

			if kind == "directory-symlink" {
				require.NoError(t, os.Symlink(filepath.Dir(victim), filepath.Join(dir, LockDir)))
			} else {
				require.NoError(t, os.Mkdir(filepath.Join(dir, LockDir), 0o750))
				path := stripePath(dir)

				switch kind {
				case "stripe-symlink":
					require.NoError(t, os.Symlink(victim, path))
				case "stripe-hardlink":
					require.NoError(t, os.Link(victim, path))
				case "stripe-fifo":
					require.NoError(t, unix.Mkfifo(path, 0o600))
				case "stripe-directory":
					require.NoError(t, os.Mkdir(path, 0o700))
				}
			}

			lock, err := TryAcquire(dir, "path")
			require.Error(t, err)
			require.Nil(t, lock)

			after, err := os.Stat(victim)
			require.NoError(t, err)
			require.True(t, os.SameFile(before, after))
			require.Equal(t, before.Mode(), after.Mode())

			contents, err := os.ReadFile(victim) // #nosec G304 -- test fixture.
			require.NoError(t, err)
			require.Equal(t, "unchanged", string(contents))
		})
	}
}

func TestOwnershipPublicationRecovery(t *testing.T) {
	for _, phase := range []string{"directory", "stripe", phaseLinked, phasePublished} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()

			var published os.FileInfo

			if phase == "directory" {
				require.NoError(t, os.Mkdir(filepath.Join(dir, ".locks.init"), 0o750))
			} else {
				require.NoError(t, os.Mkdir(filepath.Join(dir, LockDir), 0o750))
				path := stripePath(dir)
				temporary := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".init")
				require.NoError(t, os.WriteFile(temporary, nil, 0o600))

				if phase == phaseLinked || phase == phasePublished {
					require.NoError(t, os.Link(temporary, path))

					var err error

					published, err = os.Stat(path)
					require.NoError(t, err)
				}

				if phase == phasePublished {
					require.NoError(t, os.Remove(temporary))
				}
			}

			lock, err := TryAcquire(dir, "path")
			require.NoError(t, err)

			defer lock.Close()

			current, err := lock.Stat()
			require.NoError(t, err)

			if published != nil {
				require.True(t, os.SameFile(published, current))
			}

			entries, err := os.ReadDir(filepath.Join(dir, LockDir))
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, filepath.Base(stripePath(dir)), entries[0].Name())
			_, err = os.Stat(filepath.Join(dir, ".locks.init"))
			require.ErrorIs(t, err, os.ErrNotExist)
			other, err := TryAcquire(dir, "path")
			require.ErrorIs(t, err, ErrBusy)
			require.Nil(t, other)
		})
	}
}

func TestOwnershipRepairsModesWithoutReplacingLocks(t *testing.T) {
	dir := t.TempDir()
	lock, err := TryAcquire(dir, "path")
	require.NoError(t, err)

	defer lock.Close()

	before, err := lock.Stat()
	require.NoError(t, err)
	require.NoError(t, os.Chmod(filepath.Join(dir, LockDir), 0o770)) // #nosec G302 -- exercise permission repair.
	require.NoError(t, lock.Chmod(0o660))

	other, err := TryAcquire(dir, "path")
	require.ErrorIs(t, err, ErrBusy)
	require.Nil(t, other)

	after, err := os.Stat(stripePath(dir))
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	require.Equal(t, os.FileMode(0o600), after.Mode().Perm())

	directory, err := os.Stat(filepath.Join(dir, LockDir))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o750), directory.Mode().Perm())
}

func TestOwnershipProcessHelper(t *testing.T) {
	dir := os.Getenv("TSNIXCACHE_OWNER_TEST_DIR")
	if dir == "" {
		t.Skip("subprocess only")
	}

	lock, err := TryAcquire(dir, "path")
	if os.Getenv("TSNIXCACHE_OWNER_TEST_BUSY") == "1" {
		require.ErrorIs(t, err, ErrBusy)
		require.Nil(t, lock)

		return
	}

	require.NoError(t, err)
	require.NoError(t, lock.Close())
}

func TestOwnershipRootAndService(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise a distinct service UID")
	}

	base, err := os.MkdirTemp("", "tsnixcache-owner-") //nolint:usetesting // Service UID needs traversable ancestors.
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(base)) })
	require.NoError(t, os.Chmod(base, 0o755)) // #nosec G302 -- service subprocess must traverse.

	executable, err := os.Executable()
	require.NoError(t, err)
	contents, err := os.ReadFile(executable) // #nosec G304 -- copy this test binary.
	require.NoError(t, err)

	helper := filepath.Join(base, "helper")
	require.NoError(t, os.WriteFile(helper, contents, 0o755)) // #nosec G306,G703 -- service subprocess executable.

	const serviceID = 65534

	for _, phase := range []string{"root-first", "service-first", "directory-unowned", "stripe-unowned", phaseLinked, phaseLegacy} {
		t.Run(phase, func(t *testing.T) {
			dir := filepath.Join(base, phase)
			require.NoError(t, os.Mkdir(dir, 0o750))
			require.NoError(t, os.Chown(dir, serviceID, serviceID))

			service := func(busy bool) {
				cmd := exec.CommandContext(t.Context(), helper, "-test.run=^TestOwnershipProcessHelper$") // #nosec G204 -- copy of this test binary.

				cmd.Env = append(os.Environ(), "TSNIXCACHE_OWNER_TEST_DIR="+dir)
				if busy {
					cmd.Env = append(cmd.Env, "TSNIXCACHE_OWNER_TEST_BUSY=1")
				}

				cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: serviceID, Gid: serviceID}}
				output, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", output)
			}

			switch phase {
			case "root-first":
				lock, err := TryAcquire(dir, "path")
				require.NoError(t, err)
				require.NoError(t, lock.Close())
			case "directory-unowned":
				require.NoError(t, os.Mkdir(filepath.Join(dir, ".locks.init"), 0o750))
			case "stripe-unowned", phaseLinked, phaseLegacy:
				directory := filepath.Join(dir, LockDir)
				require.NoError(t, os.Mkdir(directory, 0o750))

				if phase != phaseLegacy {
					require.NoError(t, os.Chown(directory, serviceID, serviceID))
				}

				path := stripePath(dir)
				temporary := filepath.Join(directory, "."+filepath.Base(path)+".init")
				require.NoError(t, os.WriteFile(temporary, nil, 0o600))

				if phase == phaseLinked {
					require.NoError(t, os.Chown(temporary, serviceID, serviceID))
					require.NoError(t, os.Link(temporary, path))
				}

				if phase == phaseLegacy {
					require.NoError(t, os.Rename(temporary, path))
					held, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- test fixture.
					require.NoError(t, err)
					require.NoError(t, tryLock(held))
					before, err := held.Stat()
					require.NoError(t, err)
					other, err := TryAcquire(dir, "path")
					require.ErrorIs(t, err, ErrBusy)
					require.Nil(t, other)

					after, err := os.Stat(path)
					require.NoError(t, err)
					require.True(t, os.SameFile(before, after))
					service(true)
					require.NoError(t, held.Close())
				}
			}

			service(false)

			lock, err := TryAcquire(dir, "path")
			require.NoError(t, err)
			require.NoError(t, lock.Close())

			for _, path := range []string{filepath.Join(dir, LockDir), stripePath(dir)} {
				var st unix.Stat_t
				require.NoError(t, unix.Stat(path, &st))
				require.EqualValues(t, serviceID, st.Uid)
				require.EqualValues(t, serviceID, st.Gid)
			}
		})
	}
}
