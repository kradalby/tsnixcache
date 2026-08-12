// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package niximport verifies pushed NARs and imports them into a Nix store.
package niximport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kradalby/tsnixcache/nar"
	"github.com/kradalby/tsnixcache/narinfo"
	"github.com/kradalby/tsnixcache/nixbase32"
	"github.com/kradalby/tsnixcache/nixcompress"
)

var (
	errNarHashMismatch  = errors.New("niximport: NarHash mismatch")
	errNarSizeMismatch  = errors.New("niximport: NarSize mismatch")
	errSpoolNotRegular  = errors.New("niximport: spool is not a regular file")
	errInvalidNarURL    = errors.New("niximport: invalid NAR URL")
	errInvalidStorePath = errors.New("niximport: not a path in the store")
)

const (
	// stderrCap bounds what a nix-store that floods stderr costs us: the bytes
	// are only ever pasted into an error message, and NumCPU imports each
	// buffering an unbounded child's output is a way to run the server out of
	// memory.
	stderrCap = 8 << 10

	// waitDelay bounds cmd.Wait once the context is done. Without it a nix-store
	// killed by the import deadline leaves Wait blocked until every process that
	// inherited its stderr pipe exits, and the import semaphore slot this call
	// holds is never given back.
	waitDelay = 10 * time.Second
)

// Importer imports NAR files into a nix store.
type Importer struct {
	SpoolDir    string // directory where PUT /nar/{name} spooled files
	GCRootDir   string // /nix/var/nix/gcroots/tsnixcache or similar
	StoreDir    string // logical store dir store paths must live in; "" = /nix/store
	NixStoreURI string // passed to --store; "" or "auto" = system daemon
	UseExternal bool   // use external compression binaries if available
}

// Import processes a narinfo: finds the spooled NAR, verifies, imports.
// Called when PUT /{hash}.narinfo is received.
//
// It makes two streaming passes over one open spool file: verify (decompress
// and hash), then import (decompress and stream the export into nix-store).
// ponytail: two passes decompress twice but keep memory flat regardless of NAR
// size; a single pass would have to roll back an already-imported path on a hash
// mismatch. The prior code buffered the whole NAR in memory twice, which OOM'd
// the server under concurrent large pushes.
func (imp *Importer) Import(ctx context.Context, ni *narinfo.NarInfo) error {
	spoolPath, err := imp.SpoolPath(ni.URL)
	if err != nil {
		return err
	}

	// One open for both passes. Re-opening by name between them let a second PUT
	// of the same NAR swap the bytes after the hash check, so what nix-store
	// imported — and the server then signed with its own key — need not be what
	// verify hashed. The fd only helps if handlePutNar replaces the spool by
	// rename; a truncate in place keeps the inode and the fd follows it.
	//
	// Opened through an os.Root rather than by joined path so the open cannot
	// leave SpoolDir: the name is already reduced to a bare basename, but a
	// symlink sitting at that name inside the spool would still be followed, and
	// whatever it pointed at would be hashed, imported and signed with the
	// cache's own key. serve verifies the spool directory's ownership and mode
	// at startup; this is the same guarantee held open for the life of the
	// process, against a symlink planted after that check.
	root, err := os.OpenRoot(imp.SpoolDir)
	if err != nil {
		return fmt.Errorf("niximport: open spool dir %s: %w", imp.SpoolDir, err)
	}

	defer root.Close()

	f, err := root.Open(filepath.Base(spoolPath))
	if err != nil {
		return fmt.Errorf("niximport: open spool %s: %w", spoolPath, err)
	}

	defer f.Close()

	// verified is the spool as verify read it, and is nil when there is nothing
	// of ours to unlink; dropSpool uses it to avoid removing a copy someone else
	// is writing, or a file that was never a spooled NAR at all.
	verified, err := imp.verify(ctx, f, ni)
	if err != nil {
		dropSpool(spoolPath, verified)

		return err
	}

	// The gcroot goes in before the import, not after: in the window between
	// nix-store --import returning and the root appearing, a concurrent
	// nix-collect-garbage can reap the path we just wrote. A root pointing at a
	// path that does not exist yet is harmless — nix ignores dangling roots.
	gcroot, err := imp.addGCRoot(ni.StorePath)
	if err != nil {
		dropSpool(spoolPath, verified)

		return err
	}

	err = imp.importNar(ctx, f, ni)
	if err != nil {
		if gcroot != "" {
			// gcroot is empty unless this import is what put the root there, so
			// a failed re-push cannot strip the root off a path that is already
			// in the store and valid.
			//
			// ponytail: two first-time imports of the same path racing both see
			// no root, so the one that fails still removes the root the one that
			// succeeded is relying on. Ceiling accepted rather than refcounting
			// roots; a re-push, the case that actually happens, is now safe.
			_ = os.Remove(gcroot)
		}

		dropSpool(spoolPath, verified)

		return err
	}

	slog.Info("niximport: imported", "path", ni.StorePath)

	// Clean up spool file. Same guard as the failure paths: a concurrent PUT of
	// the same NAR may already be rewriting this path, and a successful import
	// has no more right to unlink that half-written body than a failed one.
	dropSpool(spoolPath, verified)

	return nil
}

// SpoolPath returns the expected spool file path for a NAR URL.
// e.g. URL "nar/abc123.nar.xz" → SpoolDir/abc123.nar.xz.
//
// "", "." and ".." are rejected rather than joined, the same names
// cache.handlePutNar refuses on the way in: filepath.Join cleans, so they name
// SpoolDir itself or its parent, and the import would then treat a directory as
// a spooled NAR and unlink it when it was done.
func (imp *Importer) SpoolPath(narURL string) (string, error) {
	// narURL is like "nar/abc123.nar" or "nar/abc123.nar.xz"
	// or could have ?hash=... query params — strip those
	base := narURL
	if i := strings.Index(base, "?"); i >= 0 {
		base = base[:i]
	}

	base = filepath.Base(base)
	if base == "." || base == ".." || base == "/" {
		return "", fmt.Errorf("%w: %q", errInvalidNarURL, narURL)
	}

	return filepath.Join(imp.SpoolDir, base), nil
}

// storePathHash returns the hash part of storePath, or "" when storePath is not
// a direct child of the store dir with a well-formed 32-character nix-base32
// hash part.
//
// The store dir defaults to /nix/store, matching narinfo.Parse.
func (imp *Importer) storePathHash(storePath string) string {
	storeDir := imp.StoreDir
	if storeDir == "" {
		storeDir = narinfo.DefaultStoreDir
	}

	base, ok := strings.CutPrefix(storePath, strings.TrimSuffix(storeDir, "/")+"/")
	if !ok || strings.Contains(base, "/") {
		return ""
	}

	// The hash alphabet has no dash, so cutting at the first one always splits
	// the hash from the name.
	hash, _, ok := strings.Cut(base, "-")
	if !ok || !nixbase32.ValidHashPart(hash) {
		return ""
	}

	return hash
}

// gcRootSeq makes each import's temporary gcroot name its own. A fixed ".tmp"
// let two imports of the same path collide: one removed and re-created the
// other's temp link, so a rename that was about to make a perfectly correct
// root failed with ENOENT and cost the client a 500 and a full re-upload.
var gcRootSeq atomic.Uint64

// addGCRoot links the gcroot that keeps storePath alive until the GC rules
// prune it. It returns the root's path, so a failed import can undo it, or ""
// when gcroots are disabled or a root for this path was already there — that
// one belongs to an earlier import whose path may still be in the store, and
// removing it would hand a valid path to the next nix-collect-garbage.
//
// A failure fails the import rather than warning: the root is the only thing
// stopping the next nix-collect-garbage reaping a freshly pushed path, so an
// import reported as successful without one is a lie the client cannot see.
// Called before the import, so nothing has been written to the store yet and
// the client's retry is cheap.
func (imp *Importer) addGCRoot(storePath string) (string, error) {
	if imp.GCRootDir == "" {
		return "", nil
	}

	// Both the link name and its target come from a pushed narinfo, so nothing
	// is written until the path is shown to be a real store path. An unchecked
	// target is permanent — pruneOldGCRoots refuses to remove a root pointing
	// outside the store — so "/etc" or "/tmp/../../root/.ssh" would sit in the
	// gcroot dir forever, pinned against nix-collect-garbage; an unchecked name
	// such as "/nix/store/.." writes a symlink above GCRootDir.
	hashPart := imp.storePathHash(storePath)
	if hashPart == "" {
		return "", fmt.Errorf("%w: %q", errInvalidStorePath, storePath)
	}

	gcroot := filepath.Join(imp.GCRootDir, hashPart)

	err := os.MkdirAll(imp.GCRootDir, 0o750) // #nosec G301 -- gcroot dir needs execute bit for traversal
	if err != nil {
		return "", fmt.Errorf("niximport: gcroot dir %s (create it, or point --gcroot-dir somewhere writable): %w",
			imp.GCRootDir, err)
	}

	_, err = os.Lstat(gcroot)
	existed := err == nil

	// Symlink to a temp name then rename over the target: rename is atomic,
	// so a concurrent import of the same hashPart can't observe a window
	// where the gcroot is missing and let GC reap the path.
	tmp := fmt.Sprintf("%s.tmp.%d.%d", gcroot, os.Getpid(), gcRootSeq.Add(1))

	err = os.Symlink(storePath, tmp)
	if err == nil {
		err = os.Rename(tmp, gcroot)
	}

	if err != nil {
		_ = os.Remove(tmp)

		return "", fmt.Errorf("niximport: gcroot symlink %s: %w", gcroot, err)
	}

	// Until the directory is synced the root only exists in the page cache,
	// while nix-store --import makes the path itself durable — a power loss in
	// between would leave an imported path for the next nix-collect-garbage to
	// reap. Best effort: a filesystem that will not sync a directory still gets
	// a correct root, just not a crash-proof one.
	dir, err := os.Open(imp.GCRootDir)
	if err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}

	if existed {
		return "", nil
	}

	return gcroot, nil
}

// dropSpool removes a spooled NAR the import is done with, whether it succeeded
// or failed. Nothing will ever read the file again — a successful import has the
// path in the store, and after a failure the client re-PUTs the NAR before
// retrying the narinfo — so keeping it leaks the spool directory until
// SweepSpool's day-long TTL catches up.
//
// It only removes the file verify actually read. cache.handlePutNar replaces the
// spool by rename, so a second push of the same NAR can have swapped a different
// file in at this path since verify stat'd it: unlinking that one throws away a
// body somebody else is about to import. A nil verified means we never had a
// spooled NAR of our own here, which is the same situation seen from the other
// side.
//
// ponytail: identity is size plus mtime, not the inode, so a replacement that
// happens to match both is still mistaken for our file. Not worth a stat-by-fd
// dance: the loss is one spool file, and the sweeper collects it.
func dropSpool(spoolPath string, verified os.FileInfo) {
	if verified == nil {
		return
	}

	cur, err := os.Stat(spoolPath)
	if err != nil || cur.Size() != verified.Size() || !cur.ModTime().Equal(verified.ModTime()) {
		return
	}

	_ = os.Remove(spoolPath)
}

// verify streams the spooled NAR through a hasher and checks it against the
// narinfo's NarHash and NarSize, without buffering the NAR. It returns the
// spool's stat as of the read, which dropSpool uses to tell our file from one a
// concurrent push has replaced; it is nil whenever there is no spooled NAR of
// ours to remove.
func (imp *Importer) verify(ctx context.Context, f *os.File, ni *narinfo.NarInfo) (os.FileInfo, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("niximport: stat spool %s: %w", f.Name(), err)
	}

	// A URL naming anything but a regular file is not a spooled NAR, and must not
	// be treated as one: "nar/zstd-cache" would otherwise have dropSpool remove
	// the compression cache directory, which is only ever created at startup, so
	// every later zstd encode fails ENOENT for the life of the process.
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s", errSpoolNotRegular, f.Name())
	}

	dec, err := nixcompress.Decoder(ctx, f, ni.Compression, imp.UseExternal)
	if err != nil {
		return fi, fmt.Errorf("niximport: decoder: %w", err)
	}
	defer dec.Close()

	// Read at most one byte past the declared size: an unbounded copy lets a
	// pusher spend a few KiB of spool on gigabytes of decompressed zeroes, and
	// the extra byte turns an oversize stream into a size mismatch instead of a
	// silent truncation.
	// #nosec G115 -- a NarSize beyond MaxInt64 wraps to a negative limit, which
	// reads nothing and is rejected as a size mismatch below.
	limited := io.LimitReader(dec, int64(ni.NarSize)+1)

	h := sha256.New()

	narSize, err := io.Copy(h, limited)
	if err != nil {
		return fi, fmt.Errorf("niximport: read nar: %w", err)
	}

	// Size before hash: an oversize stream hashes to garbage, and "got N+1 want
	// N" says what actually went wrong.
	if uint64(narSize) != ni.NarSize { // #nosec G115 -- narSize is from io.Copy, always non-negative
		return fi, fmt.Errorf("%w: got %d want %d", errNarSizeMismatch, narSize, ni.NarSize)
	}

	gotHash := "sha256:" + nixbase32.EncodeToString(h.Sum(nil))
	if gotHash != ni.NarHash {
		return fi, fmt.Errorf("%w: got %s want %s", errNarHashMismatch, gotHash, ni.NarHash)
	}

	return fi, nil
}

// capWriter keeps the first max bytes written to it and discards the rest.
type capWriter struct {
	buf bytes.Buffer
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	n := len(p)

	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}

		_, _ = w.buf.Write(p)
	}

	return n, nil
}

func (w *capWriter) String() string { return w.buf.String() }

// importNar rewinds the verified spool and streams the nix-store export format
// straight into `nix-store --import`'s stdin, so the NAR never sits in memory.
// It reads the same open file verify hashed, because re-opening the spool by
// name would import whatever a concurrent PUT had replaced it with.
func (imp *Importer) importNar(ctx context.Context, f *os.File, ni *narinfo.NarInfo) error {
	_, err := f.Seek(0, io.SeekStart)
	if err != nil {
		return fmt.Errorf("niximport: rewind spool %s: %w", f.Name(), err)
	}

	dec, err := nixcompress.Decoder(ctx, f, ni.Compression, imp.UseExternal)
	if err != nil {
		return fmt.Errorf("niximport: decoder: %w", err)
	}
	defer dec.Close()

	args := []string{"--import"}
	if imp.NixStoreURI != "" && imp.NixStoreURI != "auto" {
		args = append([]string{"--store", imp.NixStoreURI}, args...)
	}

	cmd := exec.CommandContext(ctx, "nix-store", args...) // #nosec G204 -- nix-store is a trusted system binary
	cmd.WaitDelay = waitDelay

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("niximport: stdin pipe: %w", err)
	}

	stderr := &capWriter{max: stderrCap}

	cmd.Stderr = stderr

	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("niximport: start nix-store: %w", err)
	}

	// Bound the decompression exactly as verify bounds it. This is the pass that
	// actually reaches the store, so it is the one that must not hand nix-store
	// whatever a compression bomb expands to, whether the spool changed under us
	// or a decoder behaves differently on the second read.
	// #nosec G115 -- a NarSize beyond MaxInt64 wraps to a negative limit, which
	// reads nothing, so nix-store gets a truncated export and rejects it.
	limited := io.LimitReader(dec, int64(ni.NarSize))

	_, writeErr := nar.WriteExportStream(stdin, limited, ni.StorePath, ni.References, ni.Deriver)
	closeErr := stdin.Close()

	// Wait first: if nix-store failed, its stderr is the useful error, and a
	// broken pipe from writeErr is just a downstream symptom of that.
	waitErr := cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("niximport: nix-store --import: %w\n%s", waitErr, stderr.String())
	}

	if writeErr != nil {
		return fmt.Errorf("niximport: write export: %w", writeErr)
	}

	if closeErr != nil {
		return fmt.Errorf("niximport: close stdin: %w", closeErr)
	}

	return nil
}
