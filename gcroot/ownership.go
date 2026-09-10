// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package gcroot

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// ErrInvalidLock rejects redirected or non-file lock metadata.
var ErrInvalidLock = errors.New("gcroot: invalid lock metadata")

const directoryOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

func openAt(parent *os.File, name string, flags int, mode uint32) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, flags, mode)
	if err != nil {
		return nil, err
	}

	return os.NewFile(uintptr(fd), name), nil
}

func inheritOwner(f *os.File, owner *unix.Stat_t, directory bool) error {
	var st unix.Stat_t

	err := unix.Fstat(int(f.Fd()), &st)
	if err != nil {
		return err
	}

	mode := os.FileMode(0o600)

	if directory {
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return ErrInvalidLock
		}

		mode = 0o750
	} else if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return ErrInvalidLock
	}

	if st.Uid == owner.Uid && st.Gid == owner.Gid && os.FileMode(st.Mode)&0o7777 == mode {
		return nil
	}

	err = f.Chown(int(owner.Uid), int(owner.Gid))
	if err != nil {
		return err
	}

	err = f.Chmod(mode)
	if err != nil {
		return err
	}

	return f.Sync()
}

func removeAt(parent *os.File, name string, flags int) error {
	err := unix.Unlinkat(int(parent.Fd()), name, flags)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	return err
}

func lockDirectory(root *os.File, owner *unix.Stat_t) (_ *os.File, retErr error) {
	f, err := openAt(root, LockDir, directoryOpenFlags, 0)
	if err == nil {
		err = inheritOwner(f, owner, true)
		if err != nil {
			_ = f.Close()

			return nil, err
		}

		return f, nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// Publish complete ownership; creator death leaves only an unused temp entry.
	const temporary = ".locks.init"

	err = removeAt(root, temporary, unix.AT_REMOVEDIR)
	if err != nil {
		return nil, err
	}

	err = unix.Mkdirat(int(root.Fd()), temporary, 0o750)
	if err != nil {
		return nil, err
	}
	defer func() {
		retErr = errors.Join(retErr, removeAt(root, temporary, unix.AT_REMOVEDIR))
		if retErr != nil && f != nil {
			retErr = errors.Join(retErr, f.Close())
		}
	}()

	f, err = openAt(root, temporary, directoryOpenFlags, 0)
	if err != nil {
		return nil, err
	}

	err = inheritOwner(f, owner, true)
	if err != nil {
		return nil, err
	}

	err = unix.Renameat(int(root.Fd()), temporary, int(root.Fd()), LockDir)
	if err != nil {
		return nil, err
	}

	err = root.Sync()
	if err != nil {
		return nil, err
	}

	return f, nil
}

func lockFile(directory *os.File, name string, owner *unix.Stat_t) (_ *os.File, retErr error) {
	// Remove a second link left by creator death before checking the final inode.
	temporary := "." + name + ".init"

	err := removeAt(directory, temporary, 0)
	if err != nil {
		return nil, err
	}

	const flags = unix.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK

	f, err := openAt(directory, name, flags, 0)
	if err == nil {
		err = inheritOwner(f, owner, false)
		if err != nil {
			_ = f.Close()

			return nil, err
		}

		return f, nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	f, err = openAt(directory, temporary, flags|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}

	defer func() {
		retErr = errors.Join(retErr, removeAt(directory, temporary, 0))
		if retErr != nil {
			retErr = errors.Join(retErr, f.Close())
		}
	}()

	err = inheritOwner(f, owner, false)
	if err != nil {
		return nil, err
	}
	// Linking never replaces a published lock inode.
	err = unix.Linkat(int(directory.Fd()), temporary, int(directory.Fd()), name, 0)
	if err != nil {
		return nil, err
	}

	err = removeAt(directory, temporary, 0)
	if err != nil {
		return nil, err
	}

	err = directory.Sync()
	if err != nil {
		return nil, err
	}

	return f, nil
}
