// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package upload

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// addStorePath writes a small unique file and adds it to the store, returning the
// store path. It skips the test if nix isn't available.
func addStorePath(t *testing.T, content string) string {
	t.Helper()

	_, err := exec.LookPath("nix-store")
	if err != nil {
		t.Skip("nix-store not in PATH")
	}

	f := filepath.Join(t.TempDir(), "payload")

	err = os.WriteFile(f, []byte(content), 0o600)
	if err != nil {
		t.Fatalf("write payload: %v", err)
	}

	// #nosec G204 -- test-only; f is a temp file we just wrote.
	out, err := exec.CommandContext(t.Context(), "nix-store", "--add", f).Output()
	if err != nil {
		t.Skipf("nix-store --add failed (no daemon?): %v", err)
	}

	path := strings.TrimSpace(string(out))

	// A zero exit is not the precondition these tests need — a readable path is.
	// Inside a nix build sandbox /nix/store is a read-only bind mount, and
	// `nix-store --add` still prints the store path it computed while the file
	// never appears there; the callers then fail on lstat, which reads as
	// "upload is broken" rather than "this environment has no writable store".
	// Everything upload does after this needs to open the path, so check it here
	// once and say plainly why the test cannot run.
	_, err = os.Lstat(path)
	if err != nil {
		t.Skipf("nix-store --add printed %s but it is not readable, so there is no "+
			"writable /nix/store here (a nix build sandbox, say): %v", path, err)
	}

	return path
}

// TestResolveClosureReportsNixStderr: (*exec.ExitError).Error() is only "exit
// status 1", and nix writes everything that says what went wrong — often with
// the flag needed to fix it — to stderr. Dropping it leaves an operator with a
// bare exit code for every kind of failure.
func TestResolveClosureReportsNixStderr(t *testing.T) {
	_, err := exec.LookPath("nix")
	if err != nil {
		t.Skip("nix not in PATH")
	}

	outside := filepath.Join(t.TempDir(), "not-a-store-path")

	err = os.WriteFile(outside, []byte("outside the store\n"), 0o600)
	if err != nil {
		t.Fatalf("write payload: %v", err)
	}

	_, gone, err := resolveClosure(t.Context(), []string{outside})
	if err == nil {
		t.Fatal("expected an error: the path is not in the store")
	}

	if len(gone) != 0 {
		t.Errorf("gone = %v, want none: the path exists, it is just not resolvable", gone)
	}

	if !strings.Contains(err.Error(), outside) {
		t.Errorf("error %q carries none of nix's own message", err)
	}
}

// TestClosureContinuesPastFailure verifies a failing path does not abort its
// independent siblings: one path's NAR PUT is rejected, the other still uploads.
func TestClosureContinuesPastFailure(t *testing.T) {
	good := addStorePath(t, "tsnixcache good payload\n")
	bad := addStorePath(t, "tsnixcache bad payload\n")

	badNar := "/nar/" + hashPartOf(bad) + ".nar.zstd"

	var mu sync.Mutex

	seen := map[string]bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Method+" "+r.URL.Path] = true
		mu.Unlock()

		switch {
		case r.Method == http.MethodHead: // nothing present yet
			http.NotFound(w, r)
		case r.URL.Path == badNar: // reject this one NAR
			http.Error(w, "nope", http.StatusInternalServerError)
		default: // accept everything else
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	_, sum, err := Closure(context.Background(), srv.URL, []string{good, bad}, Options{Jobs: 2, Attempts: 1})
	if err == nil {
		t.Fatal("expected an aggregate error when one path fails")
	}

	if sum.Uploaded != 1 {
		t.Errorf("Uploaded = %d, want 1 (the good path despite the bad one)", sum.Uploaded)
	}

	if len(sum.Failed) != 1 || sum.Failed[0] != bad {
		t.Errorf("Failed = %v, want [%s]", sum.Failed, bad)
	}
}

// TestClosureSurvivesVanishedRoot: `nix path-info` fails its whole invocation
// over a single bad argument and writes no JSON at all, so a path that GC removed
// while it waited in watch's retry queue used to take every healthy path in the
// same batch down with it — for as long as the queue held it.
func TestClosureSurvivesVanishedRoot(t *testing.T) {
	good := addStorePath(t, "tsnixcache survivor payload\n")
	gone := "/nix/store/00000000000000000000000000000000-collected-1"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			http.NotFound(w, r) // nothing present yet

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, sum, err := Closure(context.Background(), srv.URL, []string{gone, good}, Options{Jobs: 2, Attempts: 1})
	if err == nil {
		t.Fatal("expected an aggregate error naming the vanished root")
	}

	if sum.Uploaded != 1 {
		t.Errorf("Uploaded = %d, want 1: the live root went up despite the vanished one", sum.Uploaded)
	}

	if len(sum.Failed) != 1 || sum.Failed[0] != gone {
		t.Errorf("Failed = %v, want [%s]", sum.Failed, gone)
	}
}
