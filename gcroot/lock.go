// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package gcroot coordinates root ownership between importers and collectors.
package gcroot

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cenkalti/backoff/v5"
	"golang.org/x/sys/unix"
)

// ErrBusy means an import or another collector owns this root's stripe.
var ErrBusy = errors.New("gcroot: root is in use")

// LockDir is excluded from root enumeration. Lock inodes must never be replaced.
const LockDir = ".locks"

//nolint:nonamedreturns // Close failures must close and clear the returned lock.
func openLock(dir, key string) (file *os.File, retErr error) {
	err := makeDir(dir)
	if err != nil {
		return nil, err
	}

	root, err := os.Open(dir) // #nosec G304,G703 -- configured root directory.
	if err != nil {
		return nil, fmt.Errorf("gcroot: open root directory: %w", err)
	}

	var directory *os.File
	defer func() {
		if directory != nil {
			retErr = errors.Join(retErr, directory.Close())
		}

		retErr = errors.Join(retErr, root.Close())
		if retErr != nil && file != nil {
			retErr = errors.Join(retErr, file.Close())
			file = nil
		}
	}()
	// Serialize publication, not import work; root and service callers share ownership.
	err = tryLock(root)
	if err != nil {
		return nil, err
	}

	var owner unix.Stat_t

	err = unix.Fstat(int(root.Fd()), &owner)
	if err != nil {
		return nil, err
	}

	directory, err = lockDirectory(root, &owner)
	if err != nil {
		return nil, fmt.Errorf("gcroot: lock directory: %w", err)
	}
	// Version and mapping are persistent contracts between running processes.
	sum := sha256.Sum256([]byte(key))

	file, err = lockFile(directory, fmt.Sprintf("v1-%02x", sum[0]), &owner)
	if err != nil {
		return nil, fmt.Errorf("gcroot: open lock: %w", err)
	}

	return file, nil
}

func tryLock(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) // #nosec G115 -- OS descriptors fit int.
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrBusy
	}

	return err
}

// TryAcquire takes a root's lock without waiting. Closing the file releases it.
func TryAcquire(dir, key string) (*os.File, error) {
	f, err := openLock(dir, key)
	if err != nil {
		return nil, err
	}

	err = tryLock(f)
	if err != nil {
		_ = f.Close()

		return nil, err
	}

	return f, nil
}

// Acquire waits for a root's lock until cancellation. Closing the file releases it.
func Acquire(ctx context.Context, dir, key string) (*os.File, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	f, err := backoff.Retry(ctx, func() (*os.File, error) {
		file, openErr := openLock(dir, key)
		if openErr != nil && !errors.Is(openErr, ErrBusy) {
			return nil, backoff.Permanent(openErr)
		}

		return file, openErr
	}, backoff.WithBackOff(backoff.NewConstantBackOff(10*time.Millisecond)), backoff.WithMaxElapsedTime(0))
	if err != nil {
		return nil, err
	}

	_, err = backoff.Retry(ctx, func() (bool, error) {
		lockErr := tryLock(f)
		if lockErr != nil && !errors.Is(lockErr, ErrBusy) {
			return false, backoff.Permanent(lockErr)
		}

		return lockErr == nil, lockErr
	}, backoff.WithBackOff(backoff.NewConstantBackOff(10*time.Millisecond)), backoff.WithMaxElapsedTime(0))
	if err != nil {
		_ = f.Close()

		return nil, err
	}

	return f, nil
}
