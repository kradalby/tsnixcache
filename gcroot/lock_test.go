// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package gcroot

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

func TestLockProcessHelper(t *testing.T) {
	dir := os.Getenv("TSNIXCACHE_LOCK_TEST_DIR")
	if dir == "" {
		t.Skip("subprocess only")
	}

	lock, err := Acquire(t.Context(), dir, "path")
	require.NoError(t, err)

	defer lock.Close()

	_, err = os.Stdout.WriteString("ready\n")
	require.NoError(t, err)

	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestLocksCoordinateProcesses(t *testing.T) {
	for _, kill := range []bool{false, true} {
		t.Run(fmt.Sprintf("kill=%t", kill), func(t *testing.T) {
			dir := t.TempDir()
			executable, err := os.Executable()
			require.NoError(t, err)
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestLockProcessHelper$") // #nosec G204 -- re-exec this test binary.

			cmd.Env = append(os.Environ(), "TSNIXCACHE_LOCK_TEST_DIR="+dir)
			stdin, err := cmd.StdinPipe()
			require.NoError(t, err)
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, cmd.Start())
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

			line, err := bufio.NewReader(stdout).ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, "ready\n", line)

			lock, err := TryAcquire(dir, "path")
			require.ErrorIs(t, err, ErrBusy)
			require.Nil(t, lock)

			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
			defer cancel()

			lock, err = Acquire(ctx, dir, "path")
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Nil(t, lock)

			if kill {
				require.NoError(t, cmd.Process.Kill())
				require.Error(t, cmd.Wait())
			} else {
				require.NoError(t, stdin.Close())
				require.NoError(t, cmd.Wait())
			}

			lock, err = TryAcquire(dir, "path")
			require.NoError(t, err)
			require.NoError(t, lock.Close())
		})
	}
}

func TestLockCancellationAndInvalidDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	lock, err := Acquire(ctx, dir, "key")
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, lock)

	_, err = os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, os.WriteFile(dir, nil, 0o600))
	_, err = TryAcquire(dir, "key")
	require.Error(t, err)
}

func TestLockSetupContention(t *testing.T) {
	dir := t.TempDir()
	setup, err := os.Open(dir) // #nosec G304 -- test directory.
	require.NoError(t, err)

	defer setup.Close()

	require.NoError(t, tryLock(setup))

	before := openDescriptorStats(t)

	for range 50 {
		lock, err := TryAcquire(dir, "path")
		require.ErrorIs(t, err, ErrBusy)
		require.Nil(t, lock)
	}

	after := openDescriptorStats(t)
	require.Len(t, after, len(before))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	lock, err := Acquire(ctx, dir, "path")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, lock)
	require.NoError(t, setup.Close())
	lock, err = Acquire(t.Context(), dir, "path")
	require.NoError(t, err)
	require.NoError(t, lock.Close())
}

func TestLockWaitDoesNotBlockOtherStripes(t *testing.T) {
	dir := t.TempDir()
	held, err := Acquire(t.Context(), dir, "path")
	require.NoError(t, err)

	defer held.Close()

	var info unix.Stat_t
	require.NoError(t, unix.Fstat(int(held.Fd()), &info))
	ctx, cancel := context.WithCancel(t.Context())

	var group errgroup.Group
	group.Go(func() error {
		f, err := Acquire(ctx, dir, "path")
		if f != nil {
			_ = f.Close()
		}

		return err
	})
	t.Cleanup(func() { cancel(); _ = group.Wait() })
	// Observe the waiter's open stripe before testing independent acquisition.
	require.Eventually(t, func() bool {
		count := 0

		for _, other := range openDescriptorStats(t) {
			if info.Dev == other.Dev && info.Ino == other.Ino {
				count++
			}
		}

		return count >= 2
	}, 5*time.Second, time.Millisecond)
	require.NotEqual(t, sha256.Sum256([]byte("path"))[0], sha256.Sum256([]byte("other"))[0])

	otherCtx, otherCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer otherCancel()

	other, err := Acquire(otherCtx, dir, "other")
	require.NoError(t, err)
	require.NoError(t, other.Close())
	cancel()
	require.ErrorIs(t, group.Wait(), context.Canceled)
}

// Match descriptor identity without depending on /dev/fd path resolution.
func openDescriptorStats(t *testing.T) []unix.Stat_t {
	t.Helper()

	dir, err := os.Open("/dev/fd")
	require.NoError(t, err)
	entries, err := dir.Readdirnames(-1)
	require.NoError(t, dir.Close())
	require.NoError(t, err)

	var result []unix.Stat_t

	for _, entry := range entries {
		fd, err := strconv.Atoi(entry)
		if err != nil {
			continue
		}

		var info unix.Stat_t

		err = unix.Fstat(fd, &info)
		if err == nil {
			result = append(result, info)
		}
	}

	return result
}
