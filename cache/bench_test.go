// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cache_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kradalby/tsnixcache/cache"
	"github.com/kradalby/tsnixcache/store"
)

// newBenchServer creates a cache.Server backed by a fake in-memory store.
// It is analogous to newTestServer but accepts testing.TB so it works for both
// tests and benchmarks.
func newBenchServer(tb testing.TB) http.Handler {
	tb.Helper()

	s := &fakeStore{
		paths: map[string]*store.PathInfo{
			testHash: fakePathInfo(testHash),
		},
	}

	return cache.New(cache.Config{
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
