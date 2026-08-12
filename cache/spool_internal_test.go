// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestPutNarFailsWhenSpoolCloseFails: with delayed allocation (ext4, XFS) and on
// NFS, a spool filesystem that fills or errors reports ENOSPC/EIO from close(2),
// not from write(2), so io.Copy can succeed over a file that is short on disk. A
// 200 in that case reaches the client as a NarHash mismatch on the following
// narinfo PUT — indistinguishable from corruption, so the operator looks in the
// wrong place while the client re-uploads the whole NAR to fail identically.
//
// The close is stubbed because nothing portable makes a real close(2) fail; the
// stub still closes the descriptor, as a failing close(2) does.
func TestPutNarFailsWhenSpoolCloseFails(t *testing.T) {
	spoolDir := t.TempDir()
	srv := New(Config{
		SpoolDir: spoolDir,
		StoreDir: "/nix/store",
	})

	realClose := closeSpool

	t.Cleanup(func() { closeSpool = realClose })

	closeSpool = func(f *os.File) error {
		_ = realClose(f)

		return syscall.ENOSPC
	}

	req := httptest.NewRequestWithContext(
		context.Background(), http.MethodPut, "/nar/spoolclose.nar", strings.NewReader("fake nar content"),
	)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500: a spool close that failed was reported as a successful upload", w.Code)
	}

	_, err := os.Stat(filepath.Join(spoolDir, "spoolclose.nar"))
	if !os.IsNotExist(err) {
		t.Errorf("spool file was published despite the failed close: %v", err)
	}

	// The partial upload must not be left behind either: the sweeper only clears
	// it after a day, and it can be up to maxNarBytes.
	entries, err := os.ReadDir(spoolDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if e.IsDir() && e.Name() == ZstdCacheSubdir {
			continue
		}

		t.Errorf("%s left in the spool after the failed close", e.Name())
	}
}
