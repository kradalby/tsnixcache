// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The compressed-NAR cache has a byte budget because NAR reads need no grant:
// a tailnet peer walking hash parts otherwise makes the server write a
// compressed copy of the whole store onto the spool filesystem. The budget is
// 4 GiB, which no black-box test can reach, so the eviction it drives is
// exercised here on the map directly.

// seedZstdEntry adds one cache entry of the given size, backed by a real file so
// eviction's unlink has something to remove.
func seedZstdEntry(t *testing.T, srv *Server, hash string, size uint64, lastUsed time.Time) string {
	t.Helper()

	path := filepath.Join(srv.zstdCacheDir, hash+".nar.zstd")

	err := os.WriteFile(path, []byte(hash), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	srv.zstdCache[hash] = zstdEntry{path: path, size: size, lastUsed: lastUsed}
	srv.zstdCacheBytes += size

	return path
}

func newZstdCacheServer(t *testing.T) *Server {
	t.Helper()

	spool := t.TempDir()

	return New(Config{
		Store:            nil,
		SpoolDir:         spool,
		ServeCompression: compressionZstd,
		StoreDir:         "/nix/store",
	})
}

// TestEvictZstdKeepsCacheInsideBudget: entries go in oldest-last-used first and
// the total has to come back under the budget, with the files unlinked rather
// than merely forgotten — a map that shrinks while the spool keeps filling is
// the same unbounded disk use with better bookkeeping.
func TestEvictZstdKeepsCacheInsideBudget(t *testing.T) {
	srv := newZstdCacheServer(t)

	now := time.Now()
	// Three entries of half the budget each: the oldest two must go.
	half := uint64(zstdCacheMaxBytes/2) + 1
	oldest := seedZstdEntry(t, srv, "aaaa", half, now.Add(-3*time.Hour))
	middle := seedZstdEntry(t, srv, "bbbb", half, now.Add(-2*time.Hour))
	newest := seedZstdEntry(t, srv, "cccc", half, now.Add(-time.Hour))

	srv.zstdCacheMu.Lock()
	srv.evictZstdLocked()
	srv.zstdCacheMu.Unlock()

	if srv.zstdCacheBytes > zstdCacheMaxBytes {
		t.Errorf("cache holds %d bytes after eviction, budget is %d", srv.zstdCacheBytes, uint64(zstdCacheMaxBytes))
	}

	if len(srv.zstdCache) != 1 {
		t.Errorf("%d entries left, want 1", len(srv.zstdCache))
	}

	if _, ok := srv.zstdCache["cccc"]; !ok {
		t.Error("eviction dropped the most recently used entry")
	}

	for _, gone := range []string{oldest, middle} {
		_, err := os.Stat(gone)
		if err == nil {
			t.Errorf("%s still on disk: the entry was forgotten but the file was not removed", gone)
		}
	}

	_, err := os.Stat(newest)
	if err != nil {
		t.Errorf("the surviving entry's file was removed: %v", err)
	}
}

// TestEvictZstdKeepsTheLastEntry: a single NAR larger than the whole budget must
// still be servable. Evicting it would unlink the file the request that just
// wrote it is about to send, and the next request would recompress it to be
// evicted again.
func TestEvictZstdKeepsTheLastEntry(t *testing.T) {
	srv := newZstdCacheServer(t)

	path := seedZstdEntry(t, srv, "aaaa", uint64(zstdCacheMaxBytes)*2, time.Now())

	srv.zstdCacheMu.Lock()
	srv.evictZstdLocked()
	srv.zstdCacheMu.Unlock()

	if len(srv.zstdCache) != 1 {
		t.Fatalf("%d entries left, want the oversized one kept", len(srv.zstdCache))
	}

	_, err := os.Stat(path)
	if err != nil {
		t.Errorf("the only entry's file was removed: %v", err)
	}
}

// TestDropZstdKeepsByteTotal: the total is what the budget is enforced against,
// so an entry removed by the TTL sweep or a vanished file has to give its bytes
// back. Leaking them makes the cache evict everything for ever.
func TestDropZstdKeepsByteTotal(t *testing.T) {
	srv := newZstdCacheServer(t)

	seedZstdEntry(t, srv, "aaaa", 1000, time.Now().Add(-time.Hour))
	seedZstdEntry(t, srv, "bbbb", 2000, time.Now())

	if n := srv.SweepZstdCache(30 * time.Minute); n != 1 {
		t.Fatalf("sweep removed %d entries, want 1", n)
	}

	if srv.zstdCacheBytes != 2000 {
		t.Errorf("cacheBytes = %d after sweeping a 1000-byte entry, want 2000", srv.zstdCacheBytes)
	}
}
