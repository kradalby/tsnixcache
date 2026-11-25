// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cache

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"

	"github.com/kradalby/tsnixcache/narinfo"
	"github.com/kradalby/tsnixcache/niximport"
	"github.com/kradalby/tsnixcache/store"
)

func newZstdCacheServer(tb testing.TB) *Server {
	tb.Helper()

	return newCache(tb, Config{SpoolDir: tb.TempDir(), ServeCompression: compressionZstd, StoreDir: nixStoreDir})
}

func seedZstdEntry(tb testing.TB, srv *Server, key string, size uint64, used time.Time) *zstdEntry {
	tb.Helper()

	path := filepath.Join(srv.zstdCacheDir, "tsnixcache-zstd-"+key+".nar.zstd")
	require.NoError(tb, os.WriteFile(path, []byte(key), 0o600))
	e := &zstdEntry{key: key, path: path, size: size, charged: size, lastUsed: used}
	srv.zstdCache[key] = e
	e.lru = srv.zstdLRU.PushBack(e)
	srv.zstdCacheBytes += size

	return e
}

func TestZstdEvictionAndPins(t *testing.T) {
	srv := newZstdCacheServer(t)
	srv.zstdCacheLimit = 3000
	old := seedZstdEntry(t, srv, "old", 1000, time.Now().Add(-time.Hour))
	pinned := seedZstdEntry(t, srv, "pinned", 1000, time.Now())
	srv.zstdCacheMu.Lock()
	lease := srv.pinZstdLocked(pinned)
	srv.zstdCacheMu.Unlock()
	t.Cleanup(lease.Close)

	e := &zstdEntry{path: filepath.Join(srv.zstdCacheDir, "unfinished")}
	require.NoError(t, srv.reserveZstd(e, 2000))
	require.NoFileExists(t, old.path)
	require.FileExists(t, pinned.path)
	require.EqualValues(t, 3000, srv.zstdCacheBytes)
	require.ErrorIs(t, srv.reserveZstd(e, 1), errZstdFull)
	require.Zero(t, srv.SweepZstdCache(0))
	lease.Close()
	require.Equal(t, 1, srv.SweepZstdCache(0))
	require.EqualValues(t, 2000, srv.zstdCacheBytes)
	srv.deleteZstd(e)
	require.Zero(t, srv.zstdCacheBytes)
}

func TestZstdOversizedReservation(t *testing.T) {
	srv := newZstdCacheServer(t)
	e := &zstdEntry{}
	require.ErrorIs(t, srv.reserveZstd(e, srv.zstdCacheLimit+1), errZstdFull)
	require.Zero(t, srv.zstdCacheBytes)
}

func TestZstdSweepAccounting(t *testing.T) {
	srv := newZstdCacheServer(t)
	seedZstdEntry(t, srv, "old", 1000, time.Now().Add(-time.Hour))
	seedZstdEntry(t, srv, "new", 2000, time.Now())
	require.Equal(t, 1, srv.SweepZstdCache(30*time.Minute))
	require.EqualValues(t, 2000, srv.zstdCacheBytes)
}

func TestZstdCleanupFailureRetainsCharge(t *testing.T) {
	srv := newZstdCacheServer(t)
	e := seedZstdEntry(t, srv, "blocked", 1000, time.Now())
	require.NoError(t, os.Remove(e.path))
	require.NoError(t, os.Mkdir(e.path, 0o700))
	child := filepath.Join(e.path, "child")
	require.NoError(t, os.WriteFile(child, nil, 0o600))
	require.Zero(t, srv.SweepZstdCache(0))
	require.Empty(t, srv.zstdCache)
	require.EqualValues(t, 1000, srv.zstdCacheBytes)
	require.Len(t, srv.zstdFailed, 1)
	require.NoError(t, os.Remove(child))
	require.Equal(t, 1, srv.SweepZstdCache(0))
	require.Zero(t, srv.zstdCacheBytes)
}

func TestZstdOwnerExclusion(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{SpoolDir: dir}
	first := newCache(t, cfg)
	e := seedZstdEntry(t, first, "owned", 1000, time.Now())
	second, err := New(cfg)
	require.Error(t, err)
	require.Nil(t, second)
	require.FileExists(t, e.path)
	require.Zero(t, first.SweepSpool(0))
	require.FileExists(t, filepath.Join(dir, ZstdCacheSubdir, zstdOwnerName))
	require.NoError(t, first.Close())
	second = newCache(t, cfg)
	require.NotNil(t, second)
}

func TestZstdCloseWaitsForLease(t *testing.T) {
	srv := newZstdCacheServer(t)
	e := seedZstdEntry(t, srv, "reader", 1000, time.Now())
	lease, err := srv.acquireZstd(t.Context(), &store.PathInfo{NarHash: e.key})
	require.NoError(t, err)
	t.Cleanup(lease.Close)

	var group errgroup.Group

	closed := make(chan struct{})

	group.Go(func() error {
		defer close(closed)

		return srv.Close()
	})
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		srv.zstdCacheMu.Lock()
		defer srv.zstdCacheMu.Unlock()

		require.True(c, srv.zstdClosed)
	}, time.Second, time.Millisecond)

	select {
	case <-closed:
		t.Fatal("closed while a reader owns its file")
	default:
	}

	data, err := io.ReadAll(lease.file)
	require.NoError(t, err)
	require.Equal(t, "reader", string(data))
	lease.Close()
	require.NoError(t, group.Wait())
	require.NoFileExists(t, e.path)
	require.Zero(t, srv.zstdCacheBytes)
}

// deadlineRecorder is a ResponseWriter that records how many bytes arrive
// between deadline renewals.
type deadlineRecorder struct {
	http.ResponseWriter

	perDeadline []int
}

func (d *deadlineRecorder) SetWriteDeadline(time.Time) error {
	d.perDeadline = append(d.perDeadline, 0)

	return nil
}

func (d *deadlineRecorder) Write(p []byte) (int, error) {
	d.perDeadline[len(d.perDeadline)-1] += len(p)

	return len(p), nil
}

// shortReaderFrom moves less than it is offered, as sendfile may.
type shortReaderFrom struct{ *deadlineRecorder }

func (s shortReaderFrom) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(s.deadlineRecorder, io.LimitReader(r, 1000))
}

// No path may put more than a chunk under one deadline, or the stall guard
// turns into a rate floor that cuts slow readers.
func TestNarDeadlineRenewsPerChunk(t *testing.T) {
	const size = 5*narDeadlineChunk + 123

	payload := make([]byte, size)

	tests := []struct {
		name  string
		short bool
		write func(w *narDeadlineWriter) (int64, error)
	}{
		{"Write", false, func(w *narDeadlineWriter) (int64, error) {
			n, err := w.Write(payload)

			return int64(n), err
		}},
		{"ReadFrom", false, func(w *narDeadlineWriter) (int64, error) {
			return w.ReadFrom(bytes.NewReader(payload))
		}},
		{"ReadFrom short transfers", true, func(w *narDeadlineWriter) (int64, error) {
			return w.ReadFrom(bytes.NewReader(payload))
		}},
		// nar.Write hands over a file already capped at its declared size; the
		// cap must still hold, and must count down for the caller.
		{"ReadFrom limited", false, func(w *narDeadlineWriter) (int64, error) {
			lr := &io.LimitedReader{R: io.MultiReader(bytes.NewReader(payload), strings.NewReader("grew")), N: size}
			n, err := w.ReadFrom(lr)
			require.Zero(t, lr.N)

			return n, err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &deadlineRecorder{ResponseWriter: httptest.NewRecorder()}

			var sink http.ResponseWriter = rec
			if tt.short {
				sink = shortReaderFrom{rec}
			}

			n, err := tt.write(&narDeadlineWriter{w: sink, timeout: time.Second})
			require.NoError(t, err)
			require.EqualValues(t, size, n)

			var total int

			for _, got := range rec.perDeadline {
				require.LessOrEqual(t, got, narDeadlineChunk)

				total += got
			}

			require.Equal(t, size, total)
		})
	}
}

func TestNarDeadlineUnsupported(t *testing.T) {
	writer := &narDeadlineWriter{w: httptest.NewRecorder(), timeout: time.Second}
	_, err := writer.Write([]byte("nar"))
	require.ErrorIs(t, err, http.ErrNotSupported)
}

func TestZstdCancelledWaiterDoesNotCancelSharedBuild(t *testing.T) {
	srv := newZstdCacheServer(t)

	srv.zstdSem = make(chan struct{}, 1)
	srv.zstdSem <- struct{}{}

	path := filepath.Join(t.TempDir(), "store-path")
	require.NoError(t, os.WriteFile(path, []byte("nar contents"), 0o600))
	pi := &store.PathInfo{NarHash: "shared", StorePath: path}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var group errgroup.Group

	cancelled := make(chan error, 1)

	group.Go(func() error {
		lease, err := srv.acquireZstd(ctx, pi)
		if lease != nil {
			lease.Close()
		}

		cancelled <- err

		return nil
	})

	waiters := func(want int) {
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			srv.zstdCacheMu.Lock()
			defer srv.zstdCacheMu.Unlock()

			require.NotNil(c, srv.zstdFlights[pi.NarHash])
			require.Equal(c, want, srv.zstdFlights[pi.NarHash].waiters)
		}, time.Second, time.Millisecond)
	}
	waiters(1)
	group.Go(func() error {
		lease, err := srv.acquireZstd(t.Context(), pi)
		if err != nil {
			return err
		}
		defer lease.Close()

		_, err = io.Copy(io.Discard, lease.file)

		return err
	})
	waiters(2)
	cancel()
	require.ErrorIs(t, <-cancelled, context.Canceled)
	waiters(1)
	<-srv.zstdSem
	require.NoError(t, group.Wait())
	require.Len(t, srv.zstdCache, 1)
	require.Empty(t, srv.zstdFlights)
}

func TestZstdStalledTCPReaderReleasesBudget(t *testing.T) {
	srv := newZstdCacheServer(t)

	const size = 16 << 20

	srv.zstdCacheLimit = size
	srv.narWriteTimeout = 100 * time.Millisecond
	e := seedZstdEntry(t, srv, "stalled", size, time.Now())
	// Allocate real data so the test also measures retained disk blocks.
	payload := make([]byte, size)
	_, err := rand.Read(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(e.path, payload, 0o600))

	var stat unix.Stat_t
	require.NoError(t, unix.Stat(e.path, &stat))
	t.Logf("file bytes=%d allocated bytes=%d charged bytes=%d", stat.Size, stat.Blocks*512, e.charged)

	entered := make(chan struct{})
	finished := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		defer close(finished)

		srv.serveZstdNar(r.Context(), w, &store.PathInfo{NarHash: e.key})
	}))
	defer ts.Close()

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ts.Listener.Addr().String())
	require.NoError(t, err)

	defer conn.Close()

	require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(1024))
	_, err = fmt.Fprintf(conn, "GET /nar/stalled.nar.zstd HTTP/1.1\r\nHost: test\r\n\r\n")
	require.NoError(t, err)
	<-entered
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		srv.zstdCacheMu.Lock()
		defer srv.zstdCacheMu.Unlock()

		require.Positive(c, e.pins)
	}, time.Second, time.Millisecond)
	require.ErrorIs(t, srv.reserveZstd(&zstdEntry{}, 1), errZstdFull)
	require.Zero(t, srv.SweepZstdCache(0))
	require.FileExists(t, e.path)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		select {
		case <-finished:
		default:
			require.Fail(c, "reader still pinned")
		}
	}, 5*time.Second, time.Millisecond)
	require.Equal(t, 1, srv.SweepZstdCache(0))
	require.Zero(t, srv.zstdCacheBytes)
}

// A reader that is slow but alive must get the whole NAR: the deadline bounds
// a stall, not the transfer rate.
func TestZstdSlowTCPReaderGetsWholeNar(t *testing.T) {
	srv := newZstdCacheServer(t)

	const size = 4 << 20

	srv.zstdCacheLimit = size
	srv.narWriteTimeout = 100 * time.Millisecond
	e := seedZstdEntry(t, srv, "slow", size, time.Now())
	require.NoError(t, os.WriteFile(e.path, make([]byte, size), 0o600))

	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.serveZstdNar(r.Context(), w, &store.PathInfo{NarHash: e.key})
	}))
	// Small socket buffers on both ends, so the server feels the reader's pace
	// instead of the kernel swallowing the body.
	ts.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if state == http.StateNew {
			assert.NoError(t, c.(*net.TCPConn).SetWriteBuffer(64<<10))
		}
	}
	ts.Start()

	defer ts.Close()

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ts.Listener.Addr().String())
	require.NoError(t, err)

	defer conn.Close()

	require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(64<<10))
	_, err = fmt.Fprintf(conn, "GET /nar/slow.nar.zstd HTTP/1.1\r\nHost: test\r\n\r\n")
	require.NoError(t, err)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)

	defer resp.Body.Close()

	// Slower than a megabyte per timeout, faster than a chunk per timeout.
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()

	var total int64

	var readErr error

	buf := make([]byte, 32<<10)

	for range tick.C {
		var n int

		n, readErr = io.ReadFull(resp.Body, buf)

		total += int64(n)

		if readErr != nil {
			break
		}
	}

	require.EqualValuesf(t, size, total, "body ended with: %v", readErr)
}

func TestZstdNarInfoCannotDeleteOwnership(t *testing.T) {
	dir := t.TempDir()
	imp := &niximport.Importer{SpoolDir: dir}
	srv := newCache(t, Config{SpoolDir: dir, ImportFn: imp.Import})
	ni := &narinfo.NarInfo{StorePath: nixStoreDir + "/" + testHash + "-test", URL: "nar/" + ZstdCacheSubdir + "/" + zstdOwnerName, Compression: compressNone, NarHash: testNarHash, NarSize: 1, FileHash: testNarHash, FileSize: 1}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/"+testHash+".narinfo", strings.NewReader(ni.Marshal()))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.FileExists(t, filepath.Join(dir, ZstdCacheSubdir, zstdOwnerName))
	_, err := New(Config{SpoolDir: dir})
	require.Error(t, err)
}

func readCacheFiles(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)

	return slices.DeleteFunc(entries, func(e os.DirEntry) bool { return e.Name() == zstdOwnerName }), err
}

func TestZstdPendingFlightsBounded(t *testing.T) {
	srv := newZstdCacheServer(t)

	srv.zstdSem = make(chan struct{}, 1)
	srv.zstdSem <- struct{}{}

	for i := range 2 {
		ctx, cancel := context.WithCancel(t.Context())

		var group errgroup.Group
		group.Go(func() error {
			_, err := srv.acquireZstd(ctx, &store.PathInfo{NarHash: strconv.Itoa(i)})

			return err
		})
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			srv.zstdCacheMu.Lock()
			defer srv.zstdCacheMu.Unlock()

			require.Len(c, srv.zstdFlights, i+1)
		}, time.Second, time.Millisecond)
		cancel()
		require.ErrorIs(t, group.Wait(), context.Canceled)
	}

	for i := range 100 {
		_, err := srv.acquireZstd(t.Context(), &store.PathInfo{NarHash: fmt.Sprintf("overflow-%d", i)})
		require.ErrorIs(t, err, errZstdBusy)
	}

	require.NoError(t, srv.Close())
	require.Empty(t, srv.zstdFlights)
	require.Zero(t, srv.zstdCacheBytes)
}

func TestZstdOwnerProcess(t *testing.T) {
	if dir := os.Getenv("TSNIXCACHE_OWNER_TEST_DIR"); dir != "" {
		srv, err := New(Config{SpoolDir: dir})
		if os.Getenv("TSNIXCACHE_OWNER_TEST_BUSY") == "1" {
			require.Error(t, err)
			require.Nil(t, srv)
		} else {
			require.NoError(t, err)
			require.NoError(t, srv.Close())
		}

		return
	}

	dir := t.TempDir()
	srv := newCache(t, Config{SpoolDir: dir})
	executable, err := os.Executable()
	require.NoError(t, err)

	for _, busy := range []string{"1", "0"} {
		if busy == "0" {
			require.NoError(t, srv.Close())
		}

		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestZstdOwnerProcess$") // #nosec G204 G702 -- re-execute this test binary.

		cmd.Env = append(os.Environ(), "TSNIXCACHE_OWNER_TEST_DIR="+dir, "TSNIXCACHE_OWNER_TEST_BUSY="+busy)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
}

func TestZstdOversizedBuildReleasesPartialFiles(t *testing.T) {
	srv := newZstdCacheServer(t)
	srv.zstdCacheLimit = zstdReservationChunk
	path := filepath.Join(t.TempDir(), "incompressible")
	payload := make([]byte, 2<<20)
	_, err := rand.Read(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, payload, 0o600))
	_, err = srv.acquireZstd(t.Context(), &store.PathInfo{NarHash: "oversized", StorePath: path})
	require.ErrorIs(t, err, errZstdFull)
	require.Zero(t, srv.zstdCacheBytes)
	files, err := readCacheFiles(srv.zstdCacheDir)
	require.NoError(t, err)
	require.Empty(t, files)
}
