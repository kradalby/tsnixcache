// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cache

import (
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kradalby/tsnixcache/nar"
	"github.com/kradalby/tsnixcache/nixbase32"
	"github.com/kradalby/tsnixcache/nixcompress"
	"github.com/kradalby/tsnixcache/store"
)

const (
	zstdOwnerName        = ".tsnixcache.lock"
	zstdReservationChunk = 64 << 10
)

var (
	errZstdFull    = errors.New("cache: compressed cache budget exhausted")
	errCacheClosed = errors.New("cache: server closed")
)

type zstdEntry struct {
	key, path, fileHash string
	size, charged       uint64
	pins                int
	lastUsed            time.Time
	lru                 *list.Element
}

type zstdFlight struct {
	done     chan struct{}
	waiters  int
	entry    *zstdEntry
	err      error
	finished bool
}

type zstdLease struct {
	srv   *Server
	entry *zstdEntry
	file  *os.File
	once  sync.Once
}

func (l *zstdLease) Close() {
	l.once.Do(func() {
		if l.file != nil {
			_ = l.file.Close()
		}

		l.srv.zstdCacheMu.Lock()
		l.srv.unpinZstdLocked(l.entry)
		l.srv.zstdCacheMu.Unlock()
		l.srv.zstdLeases.Done()
	})
}

func (srv *Server) initZstdCache() error {
	err := os.MkdirAll(srv.zstdCacheDir, 0o750) // #nosec G301 -- configured service directory.
	if err != nil {
		return fmt.Errorf("cache: create spool: %w", err)
	}

	f, err := os.OpenFile(filepath.Join(srv.zstdCacheDir, zstdOwnerName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("cache: open ownership lock: %w", err)
	}

	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) // #nosec G115 -- OS descriptor fits int.
	if err != nil {
		_ = f.Close()

		return fmt.Errorf("cache: spool directory already owned: %w", err)
	}

	srv.zstdOwner = f

	entries, err := os.ReadDir(srv.zstdCacheDir)
	if err == nil {
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), "tsnixcache-zstd-") {
				continue
			}

			err = os.Remove(filepath.Join(srv.zstdCacheDir, entry.Name()))
			if err != nil {
				break
			}
		}
	}

	if err != nil {
		_ = f.Close()

		return fmt.Errorf("cache: clean compressed cache: %w", err)
	}

	srv.zstdCtx, srv.zstdCancel = context.WithCancel(context.Background())

	return nil
}

// Close cancels builds and waits for readers before releasing directory ownership.
func (srv *Server) Close() error {
	srv.zstdCacheMu.Lock()
	if srv.zstdClosed {
		srv.zstdCacheMu.Unlock()
		<-srv.zstdCloseDone

		return srv.zstdCloseErr
	}

	srv.zstdClosed = true
	srv.zstdCancel()
	srv.zstdCacheMu.Unlock()
	_ = srv.zstdGroup.Wait()
	srv.zstdRequests.Wait()
	srv.zstdLeases.Wait()
	srv.SweepZstdCache(0)
	srv.zstdCloseErr = srv.zstdOwner.Close()
	close(srv.zstdCloseDone)

	return srv.zstdCloseErr
}

func (srv *Server) pinZstdLocked(e *zstdEntry) *zstdLease {
	if e.lru != nil {
		srv.zstdLRU.Remove(e.lru)
		e.lru = nil
	}

	e.pins++

	srv.zstdLeases.Add(1)

	return &zstdLease{srv: srv, entry: e}
}

func (srv *Server) unpinZstdLocked(e *zstdEntry) {
	e.pins--
	if e.pins == 0 {
		e.lastUsed = time.Now()
		e.lru = srv.zstdLRU.PushBack(e)
	}
}

func (srv *Server) acquireZstd(ctx context.Context, pi *store.PathInfo) (*zstdLease, error) {
	for range 2 {
		lease, err := srv.zstdLease(ctx, pi)
		if err != nil {
			return nil, err
		}

		f, err := os.Open(lease.entry.path) // #nosec G304 G703 -- cache-owned temporary filename.
		if err == nil {
			lease.file = f

			return lease, nil
		}

		lease.Close()

		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}

		srv.removeMissingZstd(lease.entry)
	}

	return nil, errZstdBusy
}

func (srv *Server) zstdLease(ctx context.Context, pi *store.PathInfo) (*zstdLease, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}

	srv.zstdCacheMu.Lock()
	if srv.zstdClosed {
		srv.zstdCacheMu.Unlock()

		return nil, errCacheClosed
	}

	srv.zstdRequests.Add(1)
	defer srv.zstdRequests.Done()

	if e := srv.zstdCache[pi.NarHash]; e != nil {
		lease := srv.pinZstdLocked(e)
		srv.zstdCacheMu.Unlock()

		return lease, nil
	}

	flight := srv.zstdFlights[pi.NarHash]
	if flight == nil {
		if len(srv.zstdFlights) >= 2*cap(srv.zstdSem) {
			srv.zstdCacheMu.Unlock()

			return nil, errZstdBusy
		}

		flight = &zstdFlight{done: make(chan struct{})}
		srv.zstdFlights[pi.NarHash] = flight
		srv.zstdGroup.Go(func() error { //nolint:contextcheck // Shared builds use the server lifetime.
			srv.buildZstdFlight(pi, flight)

			return nil
		})
	}

	flight.waiters++
	srv.zstdCacheMu.Unlock()

	select {
	case <-ctx.Done():
	case <-flight.done:
	}

	srv.zstdCacheMu.Lock()
	defer srv.zstdCacheMu.Unlock()

	err = ctx.Err()
	if err == nil {
		err = flight.err
	}

	if srv.zstdClosed {
		err = errCacheClosed
	}

	var lease *zstdLease
	if err == nil {
		lease = srv.pinZstdLocked(flight.entry)
	}

	flight.waiters--
	if flight.waiters == 0 && flight.finished {
		if flight.entry != nil {
			srv.unpinZstdLocked(flight.entry)
		}

		delete(srv.zstdFlights, pi.NarHash)
	}

	return lease, err
}

func (srv *Server) buildZstdFlight(pi *store.PathInfo, flight *zstdFlight) {
	ctx, cancel := context.WithTimeout(srv.zstdCtx, importTimeout)
	defer cancel()

	timer := time.NewTimer(zstdWait)
	defer timer.Stop()

	var (
		entry *zstdEntry
		err   error
	)

	select {
	case srv.zstdSem <- struct{}{}:
		entry, err = srv.compressToZstdCache(ctx, pi)
		<-srv.zstdSem
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = errZstdBusy
	}

	srv.zstdCacheMu.Lock()
	defer srv.zstdCacheMu.Unlock()

	flight.entry, flight.err, flight.finished = entry, err, true
	if entry != nil {
		srv.zstdCache[pi.NarHash] = entry
		if flight.waiters > 0 {
			entry.pins = 1
		} else {
			entry.lastUsed = time.Now()
			entry.lru = srv.zstdLRU.PushBack(entry)
		}
	}

	if flight.waiters == 0 {
		delete(srv.zstdFlights, pi.NarHash)
	}

	close(flight.done)
}

func (srv *Server) getOrCreateZstdCache(ctx context.Context, pi *store.PathInfo) (string, string, uint64, error) {
	lease, err := srv.acquireZstd(ctx, pi)
	if err != nil {
		return "", "", 0, err
	}
	defer lease.Close()

	return lease.entry.path, lease.entry.fileHash, lease.entry.size, nil
}

type zstdWriter struct {
	srv   *Server
	entry *zstdEntry
	file  *os.File
}

func (w *zstdWriter) Write(p []byte) (int, error) {
	end := w.entry.size + uint64(len(p))
	charged := (end + zstdReservationChunk - 1) / zstdReservationChunk * zstdReservationChunk

	err := w.srv.reserveZstd(w.entry, charged-w.entry.charged)
	if err != nil {
		return 0, err
	}

	n, err := w.file.Write(p)
	w.entry.size += uint64(n) // #nosec G115 -- Write returns a nonnegative count.

	return n, err
}

func (srv *Server) reserveZstd(e *zstdEntry, more uint64) error {
	for {
		srv.zstdCacheMu.Lock()
		if srv.zstdClosed {
			srv.zstdCacheMu.Unlock()

			return errCacheClosed
		}

		if more <= srv.zstdCacheLimit && srv.zstdCacheBytes <= srv.zstdCacheLimit-more {
			srv.zstdCacheBytes += more
			e.charged += more
			srv.zstdCacheMu.Unlock()

			return nil
		}

		front := srv.zstdLRU.Front()
		if front == nil || more > srv.zstdCacheLimit {
			srv.zstdCacheMu.Unlock()

			return errZstdFull
		}

		victim := front.Value.(*zstdEntry)
		srv.retireZstdLocked(victim)
		srv.zstdCacheMu.Unlock()
		srv.deleteZstd(victim)
	}
}

func (srv *Server) retireZstdLocked(e *zstdEntry) {
	delete(srv.zstdCache, e.key)

	if e.lru != nil {
		srv.zstdLRU.Remove(e.lru)
		e.lru = nil
	}
}

func (srv *Server) deleteZstd(e *zstdEntry) bool {
	err := os.Remove(e.path)

	srv.zstdCacheMu.Lock()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		srv.zstdFailed[e.path] = e
		srv.zstdCacheMu.Unlock()
		slog.Warn("zstd cache: cleanup failed", "path", e.path, "err", err)

		return false
	}

	srv.zstdCacheBytes -= e.charged
	e.charged = 0
	delete(srv.zstdFailed, e.path)
	srv.zstdCacheMu.Unlock()

	return true
}

func (srv *Server) removeMissingZstd(e *zstdEntry) {
	srv.zstdCacheMu.Lock()
	if e.pins != 0 || srv.zstdCache[e.key] != e {
		srv.zstdCacheMu.Unlock()

		return
	}

	srv.retireZstdLocked(e)
	srv.zstdCacheMu.Unlock()
	srv.deleteZstd(e)
}

// SweepZstdCache reclaims idle, unpinned files and retries failed cleanup once.
func (srv *Server) SweepZstdCache(maxIdle time.Duration) int {
	srv.zstdCacheMu.Lock()
	victims := slices.Collect(maps.Values(srv.zstdFailed))
	clear(srv.zstdFailed)

	cutoff := time.Now().Add(-maxIdle)

	for front := srv.zstdLRU.Front(); front != nil; front = srv.zstdLRU.Front() {
		e := front.Value.(*zstdEntry)
		if e.lastUsed.After(cutoff) {
			break
		}

		srv.retireZstdLocked(e)
		victims = append(victims, e)
	}
	srv.zstdCacheMu.Unlock()

	removed := 0

	for _, e := range victims {
		if srv.deleteZstd(e) {
			removed++
		}
	}

	return removed
}

func (srv *Server) compressToZstdCache(ctx context.Context, pi *store.PathInfo) (*zstdEntry, error) {
	tmp, err := os.CreateTemp(srv.zstdCacheDir, "tsnixcache-zstd-*.nar.zstd")
	if err != nil {
		return nil, err
	}

	e := &zstdEntry{key: pi.NarHash, path: tmp.Name()}
	complete := false

	defer func() {
		_ = tmp.Close()

		if !complete {
			srv.deleteZstd(e)
		}
	}()

	hasher := sha256.New()
	writer := &zstdWriter{srv: srv, entry: e, file: tmp}

	enc, err := nixcompress.Encoder(ctx, io.MultiWriter(writer, hasher), compressionZstd, false)
	if err != nil {
		return nil, err
	}

	writeErr := nar.Write(enc, filepath.Join(srv.storeRoot, pi.StorePath))

	err = errors.Join(writeErr, enc.Close(), ctx.Err())
	if err != nil {
		return nil, err
	}

	err = tmp.Close()
	if err != nil {
		return nil, err
	}

	e.fileHash = nixbase32.EncodeToString(hasher.Sum(nil))
	complete = true

	return e, nil
}

type narDeadlineWriter struct {
	w       http.ResponseWriter
	timeout time.Duration
}

func (w *narDeadlineWriter) Write(p []byte) (int, error) {
	err := w.deadline()
	if err != nil {
		return 0, err
	}

	return w.w.Write(p)
}

// ReadFrom retains sendfile while renewing the deadline between bounded transfers.
func (w *narDeadlineWriter) ReadFrom(r io.Reader) (int64, error) {
	var total int64

	for {
		err := w.deadline()
		if err != nil {
			return total, err
		}

		n, err := io.CopyN(w.w, r, 1<<20)

		total += n
		if errors.Is(err, io.EOF) {
			return total, nil
		}

		if err != nil {
			return total, err
		}
	}
}

func (w *narDeadlineWriter) deadline() error {
	if w.timeout == 0 {
		return nil
	}

	err := http.NewResponseController(w.w).SetWriteDeadline(time.Now().Add(w.timeout))
	if err != nil {
		return fmt.Errorf("cache: NAR write deadline: %w", err)
	}

	return nil
}

// SetNarWriteTimeoutForTesting permits deterministic transport tests.
func (srv *Server) SetNarWriteTimeoutForTesting(timeout time.Duration) { srv.narWriteTimeout = timeout }
