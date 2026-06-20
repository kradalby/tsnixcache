// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause
// Forked from tailscale.com/cmd/nardump/nardump with streaming and root-type extensions.

package nar

import (
	"bufio"
	"encoding/binary"
	"io"
	"io/fs"
	"os"
	"sort"
)

// Write writes a NAR archive of the filesystem rooted at root to w.
// root must be an absolute path on the local filesystem.
// Streaming: regular file contents are copied via os.Open + io.Copy.
func Write(w io.Writer, root string) error {
	bw, ok := w.(*bufio.Writer)
	if !ok {
		bw = bufio.NewWriter(w)
	}

	if err := writeString(bw, "nix-archive-1"); err != nil {
		return err
	}

	fi, err := os.Lstat(root)
	if err != nil {
		return err
	}

	switch {
	case fi.IsDir():
		if err := writeDir(bw, root); err != nil {
			return err
		}
	case fi.Mode()&fs.ModeSymlink != 0:
		if err := writeSymlink(bw, root); err != nil {
			return err
		}
	default:
		if err := writeRegular(bw, root); err != nil {
			return err
		}
	}

	return bw.Flush()
}

// WriteExport writes a nix-store export stream for a single path.
// narBytes is the NAR content already serialized.
func WriteExport(w io.Writer, narBytes []byte, storePath string, refs []string, deriver string) error {
	if err := writeUint64(w, 1); err != nil {
		return err
	}
	if _, err := w.Write(narBytes); err != nil {
		return err
	}
	if _, err := w.Write([]byte("NIXE\x00\x00\x00\x00")); err != nil {
		return err
	}
	if err := writeString(w, storePath); err != nil {
		return err
	}
	if err := writeUint64(w, uint64(len(refs))); err != nil {
		return err
	}
	for _, r := range refs {
		if err := writeString(w, r); err != nil {
			return err
		}
	}
	if err := writeString(w, deriver); err != nil {
		return err
	}
	if err := writeUint64(w, 0); err != nil {
		return err
	}
	if err := writeUint64(w, 0); err != nil {
		return err
	}
	return nil
}

func writeString(w io.Writer, s string) error {
	return writeBytes(w, []byte(s))
}

func writeBytes(w io.Writer, b []byte) error {
	if err := writeUint64(w, uint64(len(b))); err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	return writePad(w, len(b))
}

func writePad(w io.Writer, n int) error {
	rem := n % 8
	if rem == 0 {
		return nil
	}
	pad := make([]byte, 8-rem)
	_, err := w.Write(pad)
	return err
}

func writeUint64(w io.Writer, v uint64) error {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], v)
	_, err := w.Write(buf[:])
	return err
}

func writeDir(w io.Writer, path string) error {
	if err := writeString(w, "("); err != nil {
		return err
	}
	if err := writeString(w, "type"); err != nil {
		return err
	}
	if err := writeString(w, "directory"); err != nil {
		return err
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if err := writeString(w, "entry"); err != nil {
			return err
		}
		if err := writeString(w, "("); err != nil {
			return err
		}
		if err := writeString(w, "name"); err != nil {
			return err
		}
		if err := writeString(w, entry.Name()); err != nil {
			return err
		}
		if err := writeString(w, "node"); err != nil {
			return err
		}

		childPath := path + "/" + entry.Name()
		// Use Lstat to detect symlinks since ReadDir follows them
		fi, err := os.Lstat(childPath)
		if err != nil {
			return err
		}

		switch {
		case fi.IsDir():
			if err := writeDir(w, childPath); err != nil {
				return err
			}
		case fi.Mode()&fs.ModeSymlink != 0:
			if err := writeSymlink(w, childPath); err != nil {
				return err
			}
		default:
			if err := writeRegular(w, childPath); err != nil {
				return err
			}
		}

		if err := writeString(w, ")"); err != nil {
			return err
		}
	}

	return writeString(w, ")")
}

func writeRegular(w io.Writer, path string) error {
	if err := writeString(w, "("); err != nil {
		return err
	}
	if err := writeString(w, "type"); err != nil {
		return err
	}
	if err := writeString(w, "regular"); err != nil {
		return err
	}

	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}

	if fi.Mode()&0o111 != 0 {
		if err := writeString(w, "executable"); err != nil {
			return err
		}
		if err := writeString(w, ""); err != nil {
			return err
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	size := fi.Size()
	if err := writeString(w, "contents"); err != nil {
		return err
	}
	if err := writeUint64(w, uint64(size)); err != nil {
		return err
	}
	if _, err := io.Copy(w, f); err != nil {
		return err
	}
	if err := writePad(w, int(size)); err != nil {
		return err
	}

	return writeString(w, ")")
}

func writeSymlink(w io.Writer, path string) error {
	if err := writeString(w, "("); err != nil {
		return err
	}
	if err := writeString(w, "type"); err != nil {
		return err
	}
	if err := writeString(w, "symlink"); err != nil {
		return err
	}
	if err := writeString(w, "target"); err != nil {
		return err
	}

	target, err := os.Readlink(path)
	if err != nil {
		return err
	}
	if err := writeString(w, target); err != nil {
		return err
	}

	return writeString(w, ")")
}
