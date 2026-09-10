// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/kradalby/tsnixcache/narinfo"
	"github.com/kradalby/tsnixcache/store"
)

// newBenchServer creates a Server backed by a fake in-memory store.
// It is analogous to newTestServer but accepts testing.TB so it works for both
// tests and benchmarks.
func newBenchServer(tb testing.TB) http.Handler {
	tb.Helper()

	s := &fakeStore{
		paths: map[string]*store.PathInfo{
			testHash: fakePathInfo(testHash),
		},
	}

	return newCache(tb, Config{
		Store:            s,
		Priority:         30,
		SpoolDir:         tb.TempDir(),
		ServeCompression: compressNone,
		StoreDir:         tb.TempDir(),
	}).Handler()
}

// probeResponseSize issues one request and returns the response body length in bytes.
func probeResponseSize(h http.Handler, method, url string) int64 {
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), method, url, nil)
	h.ServeHTTP(w, req)

	return int64(w.Body.Len())
}

// BenchmarkNarInfoHit measures GET /{hash}.narinfo for a path that exists.
func BenchmarkNarInfoHit(b *testing.B) {
	h := newBenchServer(b)
	url := "/" + testHash + ".narinfo"
	b.SetBytes(probeResponseSize(h, http.MethodGet, url))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)

	b.ReportAllocs()

	for b.Loop() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			b.Fatalf("expected 200, got %d", w.Code)
		}
	}
}

// BenchmarkNarInfoMiss measures GET /{hash}.narinfo for a path that is absent.
func BenchmarkNarInfoMiss(b *testing.B) {
	h := newBenchServer(b)
	url := "/missing0000000000000000000000000.narinfo"
	b.SetBytes(probeResponseSize(h, http.MethodGet, url))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)

	b.ReportAllocs()

	for b.Loop() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			b.Fatalf("expected 404, got %d", w.Code)
		}
	}
}

// BenchmarkNarInfoHit_Parallel measures concurrent GET /{hash}.narinfo hits.
func BenchmarkNarInfoHit_Parallel(b *testing.B) {
	h := newBenchServer(b)
	url := "/" + testHash + ".narinfo"
	b.SetBytes(probeResponseSize(h, http.MethodGet, url))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)

		for pb.Next() {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				b.Fatalf("expected 200, got %d", w.Code)
			}
		}
	})
}

func BenchmarkZstdEviction(b *testing.B) {
	for _, count := range []int{500, 2000, 8000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			for range b.N {
				b.StopTimer()
				srv := evictionBenchServer(b, count)

				b.StartTimer()
				require.NoError(b, srv.reserveZstd(&zstdEntry{}, 0))
			}
		})
	}
}

const (
	benchHigh = "high"
	benchHead = "head"
)

var errBenchResponse = errors.New("unexpected benchmark response")

func BenchmarkNarInfoZstd(b *testing.B) {
	quietCacheBench(b)

	for _, entropy := range []string{"low", benchHigh} {
		b.Run(entropy, func(b *testing.B) {
			for _, mode := range []string{benchHead, "cold", "warm"} {
				b.Run(mode, func(b *testing.B) {
					srv, infos := newZstdFixture(b, 32<<20, 1, entropy == benchHigh)
					h := srv.Handler()

					method := http.MethodGet
					if mode == benchHead {
						method = http.MethodHead
					}

					req := httptest.NewRequestWithContext(b.Context(), method, narInfoURL(infos[0]), nil)
					if mode == "warm" {
						h.ServeHTTP(httptest.NewRecorder(), req)
					}

					b.ReportAllocs()
					b.ResetTimer()

					var w *httptest.ResponseRecorder

					for range b.N {
						if mode == "cold" {
							b.StopTimer()
							srv.SweepZstdCache(0)
							b.StartTimer()
						}

						w = httptest.NewRecorder()
						h.ServeHTTP(w, req)
						require.Equal(b, http.StatusOK, w.Code)
					}

					b.StopTimer()

					if mode == benchHead {
						require.Empty(b, w.Body.Bytes())
						require.Empty(b, w.Header().Get("Content-Encoding"))
						require.Empty(b, srv.zstdCache)
					} else {
						ni, err := narinfo.Parse(w.Body, filepath.Dir(infos[0].StorePath))
						require.NoError(b, err)
						require.Equal(b, compressZstd, ni.Compression)
						b.ReportMetric(float64(ni.FileSize), "compressed-B")
						b.ReportMetric(float64(srv.zstdCacheBytes), "charged-B")
					}
				})
			}
		})
	}
}

// Each operation is eight compressed-cache misses; filesystem pages stay warm.
func BenchmarkNarInfoZstdConcurrent(b *testing.B) {
	quietCacheBench(b)

	for _, entropy := range []string{"low", benchHigh} {
		b.Run(entropy, func(b *testing.B) {
			for _, mode := range []string{"same", "distinct"} {
				b.Run(mode, func(b *testing.B) {
					count := 1
					if mode == "distinct" {
						count = 8
					}

					srv, infos := newZstdFixture(b, 32<<20, count, entropy == benchHigh)
					h := srv.Handler()

					b.ReportAllocs()
					b.ResetTimer()

					for range b.N {
						b.StopTimer()
						srv.SweepZstdCache(0)
						b.StartTimer()

						var group errgroup.Group

						start := make(chan struct{})

						for i := range 8 {
							group.Go(func() error {
								<-start

								pi := infos[i%count]
								w := httptest.NewRecorder()
								h.ServeHTTP(w, httptest.NewRequestWithContext(b.Context(), http.MethodGet, narInfoURL(pi), nil))

								if w.Code != http.StatusOK {
									return fmt.Errorf("%w: status %d", errBenchResponse, w.Code)
								}

								ni, err := narinfo.Parse(w.Body, filepath.Dir(pi.StorePath))
								if err != nil {
									return err
								}

								if ni.Compression != compressZstd {
									return fmt.Errorf("%w: compression %s", errBenchResponse, ni.Compression)
								}

								return nil
							})
						}

						close(start)
						require.NoError(b, group.Wait())
						b.StopTimer()
						require.Len(b, srv.zstdCache, count)
						b.StartTimer()
					}

					b.StopTimer()
					b.ReportMetric(float64(srv.zstdCacheBytes), "charged-B")
				})
			}
		})
	}
}

func quietCacheBench(b *testing.B) {
	b.Helper()

	previous := slog.Default()

	slog.SetDefault(slog.New(slog.DiscardHandler))
	b.Cleanup(func() { slog.SetDefault(previous) })
}

func evictionBenchServer(tb testing.TB, count int) *Server {
	tb.Helper()
	dir := tb.TempDir()
	srv := &Server{zstdCacheDir: dir, zstdCache: make(map[string]*zstdEntry), zstdCacheLimit: zstdCacheMaxBytes, zstdFailed: make(map[string]*zstdEntry)}

	size := uint64(zstdCacheMaxBytes) / uint64(count/2) // #nosec G115 -- positive benchmark fixture count.
	for i := range count {
		key := strconv.Itoa(i)
		path := filepath.Join(dir, key)
		require.NoError(tb, os.WriteFile(path, []byte("x"), 0o600))
		e := &zstdEntry{key: key, path: path, size: size, charged: size, lastUsed: time.Unix(int64(i), 0)}
		srv.zstdCache[key] = e
		e.lru = srv.zstdLRU.PushBack(e)
		srv.zstdCacheBytes += size
	}

	return srv
}

func BenchmarkZstdEvictionParallelLookups(b *testing.B) {
	quietCacheBench(b)

	for _, count := range []int{500, 2000, 8000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			var longest time.Duration

			for range b.N {
				b.StopTimer()
				srv := evictionBenchServer(b, count)
				hot := &store.PathInfo{NarHash: strconv.Itoa(count - 1)}
				wantedPath := srv.zstdCache[hot.NarHash].path
				start := make(chan struct{})

				var (
					group     errgroup.Group
					latencies [4]time.Duration
				)
				for worker := range latencies {
					group.Go(func() error {
						<-start

						for range 128 {
							began := time.Now()
							path, _, _, err := srv.getOrCreateZstdCache(b.Context(), hot)
							latencies[worker] = max(latencies[worker], time.Since(began))

							if path != wantedPath {
								return errBenchResponse
							}

							if err != nil {
								return err
							}
						}

						return nil
					})
				}

				group.Go(func() error {
					<-start

					return srv.reserveZstd(&zstdEntry{}, 0)
				})
				b.StartTimer()
				close(start)
				require.NoError(b, group.Wait())
				b.StopTimer()

				longest += slices.Max(latencies[:])

				require.LessOrEqual(b, srv.zstdCacheBytes, uint64(zstdCacheMaxBytes))
				require.Contains(b, srv.zstdCache, hot.NarHash)
			}

			b.ReportMetric(float64(longest.Nanoseconds())/float64(b.N), "lookup-max-ns/op")
		})
	}
}
