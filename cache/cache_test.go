// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cache

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"

	"github.com/kradalby/tsnixcache/auth"
	"github.com/kradalby/tsnixcache/nar"
	"github.com/kradalby/tsnixcache/narinfo"
	"github.com/kradalby/tsnixcache/nixbase32"
	"github.com/kradalby/tsnixcache/nixcompress"
	"github.com/kradalby/tsnixcache/signing"
	"github.com/kradalby/tsnixcache/store"
)

var errSimulatedDBFailure = errors.New("simulated db failure")

// fakeStore satisfies StoreProvider.
type fakeStore struct {
	paths map[string]*store.PathInfo
}

func (f *fakeStore) PathInfo(_ context.Context, hashPart string) (*store.PathInfo, error) {
	if pi, ok := f.paths[hashPart]; ok {
		return pi, nil
	}

	return nil, store.ErrNotFound
}

func (f *fakeStore) PathCount(_ context.Context) (int64, error) { return int64(len(f.paths)), nil }

// fakePathInfo returns a minimal PathInfo for testing.
func fakePathInfo(hash string) *store.PathInfo {
	return &store.PathInfo{
		StorePath:  "/nix/store/" + hash + "-test",
		NarHash:    testNarHash,
		NarSize:    42,
		References: []string{},
		Deriver:    "",
		Sigs:       nil,
		CA:         "",
	}
}

const testHash = "abc12300000000000000000000000000"

// Named once so a Config literal reads at a glance, and so nothing accidentally
// passes a store dir where a compression mode belongs.
const (
	compressNone = "none"
	compressZstd = "zstd"
	nixStoreDir  = "/nix/store"

	// offTailnetAddr is a peer that is neither loopback nor a tailnet node.
	offTailnetAddr = "192.0.2.1:1234"
)

// testNarHash is a real sha256 digest in nix-base32, the only form narinfo.Parse
// accepts. The store-path hash part (testHash) is 32 characters and is not a
// valid digest in any encoding, so it cannot double as one.
const testNarHash = "sha256:1zq39m4q83w4fk0x2j74v0341dy4cmjrvs8jlim8xlw1crlp8b4q"

// newTestServer creates a Server for tests with a fake store and temp spoolDir.
func newTestServer(t *testing.T, opts ...func(*Config)) (http.Handler, string) {
	t.Helper()

	spoolDir := t.TempDir()

	s := &fakeStore{
		paths: map[string]*store.PathInfo{
			testHash: fakePathInfo(testHash),
		},
	}

	// StoreDir is the logical Nix store dir reported in nix-cache-info; real
	// deployments always use /nix/store even when the physical store is elsewhere.
	cfg := Config{
		Store:            s,
		Priority:         30,
		SpoolDir:         spoolDir,
		ServeCompression: compressNone,
		StoreDir:         nixStoreDir,
	}
	for _, o := range opts {
		o(&cfg)
	}

	return newCache(t, cfg).Handler(), spoolDir
}

func TestNixCacheInfo(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/nix-cache-info", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "StoreDir: /nix/store") {
		t.Errorf("missing StoreDir in response: %q", body)
	}

	if !strings.Contains(body, "WantMassQuery: 1") {
		t.Errorf("missing WantMassQuery in response: %q", body)
	}

	if !strings.Contains(body, "Priority: 30") {
		t.Errorf("missing Priority in response: %q", body)
	}
}

func TestNarInfo200(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/"+testHash+".narinfo", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "text/x-nix-narinfo" {
		t.Errorf("unexpected Content-Type: %q", ct)
	}

	body := w.Body.String()
	if !strings.Contains(body, "StorePath: /nix/store/"+testHash+"-test") {
		t.Errorf("missing StorePath in narinfo body: %q", body)
	}

	if !strings.Contains(body, "NarHash: "+testNarHash) {
		t.Errorf("missing NarHash in narinfo body: %q", body)
	}
}

func TestNarInfo404(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/missing0000000000000000000000000.narinfo", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestNarInfoHEAD(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodHead, "/"+testHash+".narinfo", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "text/x-nix-narinfo" {
		t.Errorf("unexpected Content-Type: %q", ct)
	}

	if w.Body.Len() != 0 {
		t.Errorf("expected empty body for HEAD, got %d bytes", w.Body.Len())
	}
}

func TestNarInfoSigned(t *testing.T) {
	sk, pk, err := signing.GenerateKey("test")
	if err != nil {
		t.Fatal(err)
	}

	h, _ := newTestServer(t, func(cfg *Config) {
		cfg.Signer = sk
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/"+testHash+".narinfo", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "Sig: ") {
		t.Fatalf("missing Sig line in narinfo: %q", body)
	}

	// Verify the signature.
	ni, err := narinfo.Parse(strings.NewReader(body), "/nix/store")
	if err != nil {
		t.Fatal(err)
	}

	if len(ni.Sigs) == 0 {
		t.Fatal("no sigs in parsed narinfo")
	}

	fp := ni.Fingerprint()

	var verified bool

	for _, sig := range ni.Sigs {
		if pk.Verify(fp, sig) {
			verified = true

			break
		}
	}

	if !verified {
		t.Errorf("signature did not verify; fp=%q sigs=%v", fp, ni.Sigs)
	}
}

func TestNarGET(t *testing.T) {
	// Create a real temp store path so nar.Write has something to read.
	storeDir := t.TempDir()

	storePath := filepath.Join(storeDir, testHash+"-test")

	err := os.MkdirAll(storePath, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(storePath, "hello"), []byte("hello world"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	s := &fakeStore{
		paths: map[string]*store.PathInfo{
			testHash: {
				StorePath:  storePath,
				NarHash:    testNarHash,
				NarSize:    testNarSize(t, storePath),
				References: []string{},
			},
		},
	}
	h := newCache(t, Config{
		Store:            s,
		Priority:         30,
		SpoolDir:         t.TempDir(),
		ServeCompression: compressNone,
		StoreDir:         storeDir,
	}).Handler()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/nar/"+testHash+".nar?hash="+testHash, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if cl := w.Header().Get("Content-Length"); cl != strconv.Itoa(w.Body.Len()) {
		t.Errorf("Content-Length %q, but the body is %d bytes", cl, w.Body.Len())
	}

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %q)", w.Code, w.Body.String())
	}

	if w.Body.Len() == 0 {
		t.Error("expected non-empty NAR body")
	}

	// NAR starts with the magic string.
	if !bytes.HasPrefix(w.Body.Bytes(), []byte("\x0d\x00\x00\x00\x00\x00\x00\x00nix-archive-1")) {
		// Just check it's non-trivial binary data (NAR format).
		if w.Body.Len() < 20 {
			t.Errorf("NAR body too short: %d bytes", w.Body.Len())
		}
	}
}

func TestNarHEAD(t *testing.T) {
	storeDir := t.TempDir()

	storePath := filepath.Join(storeDir, testHash+"-test")

	err := os.MkdirAll(storePath, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	s := &fakeStore{
		paths: map[string]*store.PathInfo{
			testHash: {
				StorePath:  storePath,
				NarHash:    testNarHash,
				NarSize:    testNarSize(t, storePath),
				References: []string{},
			},
		},
	}
	h := newCache(t, Config{
		Store:            s,
		Priority:         30,
		SpoolDir:         t.TempDir(),
		ServeCompression: compressNone,
		StoreDir:         storeDir,
	}).Handler()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodHead, "/nar/"+testHash+".nar?hash="+testHash, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if w.Body.Len() != 0 {
		t.Errorf("expected empty body for HEAD, got %d bytes", w.Body.Len())
	}
}

func TestNar404(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/nar/missing0000000000000000000000000.nar", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestPutNar(t *testing.T) {
	h, spoolDir := newTestServer(t)

	body := strings.NewReader("fake nar content")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/nar/"+testHash+".nar", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %q)", w.Code, w.Body.String())
	}

	dest := filepath.Join(spoolDir, testHash+".nar")

	data, err := os.ReadFile(dest) // #nosec G304 -- dest is a test path constructed in temp dir
	if err != nil {
		t.Fatalf("spooled file not found: %v", err)
	}

	if string(data) != "fake nar content" {
		t.Errorf("unexpected spool content: %q", data)
	}
}

func TestPutNarConfinesNestedPath(t *testing.T) {
	h, spoolDir := newTestServer(t)

	// A nested name must collapse to its base so the spool file can never
	// escape spoolDir.
	req := httptest.NewRequestWithContext(
		context.Background(), http.MethodPut, "/nar/sub/dir/escape.nar", strings.NewReader("nar"),
	)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body %q)", w.Code, w.Body.String())
	}

	_, err := os.Stat(filepath.Join(spoolDir, "escape.nar"))
	if err != nil {
		t.Errorf("expected spooled file at spoolDir/escape.nar: %v", err)
	}

	_, err = os.Stat(filepath.Join(spoolDir, "sub", "dir", "escape.nar"))
	if !os.IsNotExist(err) {
		t.Errorf("nested path was not flattened: %v", err)
	}
}

func TestPutNarInfoRejectsOversizedBody(t *testing.T) {
	h, _ := newTestServer(t)

	big := strings.Repeat("x", (1<<20)+1) // just over the 1 MiB narinfo cap
	req := httptest.NewRequestWithContext(
		context.Background(), http.MethodPut, "/"+testHash+".narinfo", strings.NewReader(big),
	)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d (body %q)", w.Code, w.Body.String())
	}
}

func TestPutNarInfoTriggersImport(t *testing.T) {
	var mu sync.Mutex

	var importedNI *narinfo.NarInfo

	fakeFn := ImporterFunc(func(_ context.Context, ni *narinfo.NarInfo) error {
		mu.Lock()
		importedNI = ni
		mu.Unlock()

		return nil
	})

	s := &fakeStore{paths: map[string]*store.PathInfo{}}
	// The pushed narinfo names a /nix/store path, and Parse now requires the
	// StorePath to sit under the server's store dir, so it has to match.
	h := newCache(t, Config{
		Store:            s,
		Priority:         30,
		SpoolDir:         t.TempDir(),
		ServeCompression: compressNone,
		StoreDir:         nixStoreDir,
		ImportFn:         fakeFn,
	}).Handler()

	niText := fmt.Sprintf(`StorePath: /nix/store/%s-test
URL: nar/%s.nar
Compression: none
FileHash: %s
FileSize: 42
NarHash: %s
NarSize: 42
References:
`, testHash, testHash, testNarHash, testNarHash)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/"+testHash+".narinfo", strings.NewReader(niText))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %q)", w.Code, w.Body.String())
	}

	mu.Lock()
	got := importedNI
	mu.Unlock()

	if got == nil {
		t.Fatal("importer was not called")
	}

	if got.StorePath != "/nix/store/"+testHash+"-test" {
		t.Errorf("unexpected StorePath: %q", got.StorePath)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /metrics, got %d", w.Code)
	}

	body := w.Body.String()
	for _, name := range []string{
		"tsnixcache_narinfo_hits_total",
		"tsnixcache_narinfo_misses_total",
		"tsnixcache_nar_bytes_served_total",
		"tsnixcache_push_nar_total",
		"tsnixcache_push_narinfo_total",
		"tsnixcache_push_errors_total",
		"tsnixcache_import_success_total",
		"tsnixcache_import_fail_total",
		"tsnixcache_import_duration_seconds",
		"tsnixcache_store_paths",
		"tsnixcache_spool_disk_available_bytes",
		// A fresh registry starts empty and /debug/varz gathers from the default
		// one, so these are absent unless they are registered here explicitly —
		// leaving a server that streams multi-GB NARs with no memory, goroutine
		// or fd telemetry at all.
		"go_goroutines",
		"go_memstats_alloc_bytes",
		"process_open_fds",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("metric %q not found in /metrics output", name)
		}
	}

	// The gauge falls after GC, so it must not carry a _total suffix.
	if strings.Contains(body, "tsnixcache_store_paths_total") {
		t.Error("store path gauge is still named _total")
	}
}

// gaugeValue pulls a single unlabelled gauge out of a /metrics body.
func gaugeValue(t *testing.T, body, name string) float64 {
	t.Helper()

	for line := range strings.SplitSeq(body, "\n") {
		rest, ok := strings.CutPrefix(line, name+" ")
		if !ok {
			continue
		}

		v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		return v
	}

	t.Fatalf("metric %q not present in /metrics", name)

	return 0
}

// TestStoreDiskGaugesExcludeTheRootReserve pins which statfs field feeds which
// gauge: available is Bavail (space this unprivileged service can actually use),
// used is Blocks-Bfree (the root reserve counted as used, which is what df
// prints and what cli's GC threshold measures). The Overview
// dashboard's 75/90 % colours are calibrated on the two agreeing, and switching
// available to Bfree would silently inflate reported headroom by the reserve —
// typically 5 % of the store filesystem — right where GC decides to run.
//
// This used to be pinned by a source grep in cmd/dashboard's tests, which was
// removed when collectDisk was factored out; it belongs here, on behaviour.
func TestStoreDiskGaugesExcludeTheRootReserve(t *testing.T) {
	// A filesystem with no root reserve cannot tell Bfree and Bavail apart, so
	// it cannot fail this test either — find one that can.
	var dir string

	for _, cand := range []string{"/", os.TempDir(), "."} {
		var st syscall.Statfs_t
		if syscall.Statfs(cand, &st) == nil && st.Bfree != st.Bavail {
			dir = cand

			break
		}
	}

	if dir == "" {
		t.Skip("no reachable filesystem reserves blocks for root; " +
			"Bfree and Bavail are equal everywhere, so this test cannot discriminate")
	}

	// physStoreDir is StoreRoot joined with StoreDir, so this statfs's dir.
	h, _ := newTestServer(t, func(c *Config) {
		c.StoreRoot = dir
		c.StoreDir = "/"
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	total := gaugeValue(t, body, "tsnixcache_store_disk_total_bytes")
	used := gaugeValue(t, body, "tsnixcache_store_disk_used_bytes")
	avail := gaugeValue(t, body, "tsnixcache_store_disk_available_bytes")

	if total <= 0 {
		t.Fatalf("total = %v, want > 0", total)
	}

	// All three come from one statfs, so this comparison cannot drift: free
	// space (total-used, i.e. Bfree) strictly exceeds usable space (Bavail) by
	// the reserve. Equality means available was wired to Bfree.
	if total-used <= avail {
		t.Errorf("total-used = %v, available = %v: available must exclude the "+
			"root reserve (Bavail), and used must include it (Blocks-Bfree)",
			total-used, avail)
	}
}

func TestDebugSurface(t *testing.T) {
	h, _ := newTestServer(t)

	// Non-loopback, non-tailnet peer is denied by AllowDebugAccess.
	deny := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/debug/", nil)
	deny.RemoteAddr = offTailnetAddr
	w := httptest.NewRecorder()
	h.ServeHTTP(w, deny)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for /debug/ from external IP, got %d", w.Code)
	}

	// Loopback is allowed and the index links /metrics.
	allow := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/debug/", nil)
	allow.RemoteAddr = "127.0.0.1:1234"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, allow)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /debug/ from loopback, got %d", w.Code)
	}

	if !strings.Contains(w.Body.String(), "/metrics") {
		t.Errorf("/debug/ index does not link /metrics:\n%s", w.Body.String())
	}
}

func TestMetricsAfterHit(t *testing.T) {
	h, _ := newTestServer(t)

	// Trigger a hit.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/"+testHash+".narinfo", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	// Check metrics.
	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)

	body := w2.Body.String()
	if !strings.Contains(body, `tsnixcache_narinfo_hits_total{method="get"} 1`) {
		t.Errorf("expected narinfo_hits_total{get} 1 in metrics:\n%s", body)
	}
}

// TestNarInfoHeadCountedSeparately pins push presence-probes out of the
// substituter hit ratio. `tsnixcache push` HEADs every path in a closure, so a
// healthy push of mostly-new paths is thousands of misses; conflated with GETs
// it drives the hit-ratio tile red during exactly the workload the cache exists
// for.
func TestNarInfoHeadCountedSeparately(t *testing.T) {
	h, _ := newTestServer(t)

	head := httptest.NewRequestWithContext(context.Background(), http.MethodHead,
		"/missing0000000000000000000000000.narinfo", nil)
	h.ServeHTTP(httptest.NewRecorder(), head)

	// A HEAD that finds the path, which is what re-pushing an already-present
	// closure looks like. Labelled "get" it would inflate the hit ratio instead
	// of deflating it, so both halves of the split need pinning.
	headHit := httptest.NewRequestWithContext(context.Background(), http.MethodHead,
		"/"+testHash+".narinfo", nil)
	h.ServeHTTP(httptest.NewRecorder(), headHit)

	get := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/"+testHash+".narinfo", nil)
	h.ServeHTTP(httptest.NewRecorder(), get)

	body := getBytes(t, h, "/metrics").Body.String()
	for _, want := range []string{
		`tsnixcache_narinfo_misses_total{method="head"} 1`,
		`tsnixcache_narinfo_misses_total{method="get"} 0`,
		`tsnixcache_narinfo_hits_total{method="head"} 1`,
		`tsnixcache_narinfo_hits_total{method="get"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in metrics:\n%s", want, body)
		}
	}
}

func TestMetricsAfterMiss(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/missing0000000000000000000000000.narinfo", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)

	body := w2.Body.String()
	if !strings.Contains(body, `tsnixcache_narinfo_misses_total{method="get"} 1`) {
		t.Errorf("expected narinfo_misses_total{get} 1 in metrics:\n%s", body)
	}
}

func TestMetricsAfterPushNar(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/nar/x.nar", strings.NewReader("data"))
	h.ServeHTTP(httptest.NewRecorder(), req)

	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)

	body := w2.Body.String()
	if !strings.Contains(body, "tsnixcache_push_nar_total 1") {
		t.Errorf("expected push_nar_total 1 in metrics:\n%s", body)
	}
}

func TestHealthOK(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %q)", w.Code, w.Body.String())
	}

	var resp struct {
		Status              string `json:"status"`
		StoreReachable      bool   `json:"store_reachable"`
		UptimeSeconds       int64  `json:"uptime_seconds"`
		SpoolAvailableBytes uint64 `json:"spool_available_bytes"`
	}

	raw := w.Body.String()

	err := json.NewDecoder(strings.NewReader(raw)).Decode(&resp)
	if err != nil {
		t.Fatalf("decode health JSON: %v", err)
	}

	if resp.Status != "ok" {
		t.Errorf("expected status=ok, got %q", resp.Status)
	}

	if !resp.StoreReachable {
		t.Error("expected store_reachable=true")
	}

	// The spool is often a separate filesystem, and it filling up breaks every
	// push while the store still looks healthy.
	if resp.SpoolAvailableBytes == 0 {
		t.Error("health reports no spool free space")
	}

	// db_readable used to be the same bool under a second name, so the two
	// fields could never disagree.
	if strings.Contains(raw, "db_readable") {
		t.Error("health still reports db_readable alongside store_reachable")
	}
}

func TestVersion(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/version", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	body := strings.TrimSpace(w.Body.String())
	if body == "" {
		t.Error("expected non-empty version string")
	}
}

func TestConcurrentRequests(t *testing.T) {
	storeDir := t.TempDir()

	storePath := filepath.Join(storeDir, testHash+"-test")

	err := os.MkdirAll(storePath, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(storePath, "data"), []byte("content"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	s := &fakeStore{
		paths: map[string]*store.PathInfo{
			testHash: {
				StorePath:  storePath,
				NarHash:    testNarHash,
				NarSize:    42,
				References: []string{},
			},
		},
	}
	h := newCache(t, Config{
		Store:            s,
		Priority:         30,
		SpoolDir:         t.TempDir(),
		ServeCompression: compressNone,
		StoreDir:         storeDir,
	}).Handler()

	const n = 50

	var wg sync.WaitGroup

	codes := make([]int, n)

	for i := range n {
		wg.Add(1)
		startTestTask(t, func() {
			func(idx int) {
				defer wg.Done()

				var req *http.Request

				if idx%2 == 0 {
					// hit
					req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/"+testHash+".narinfo", nil)
				} else {
					// miss
					req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/missing0000000000000000000000000.narinfo", nil)
				}

				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				codes[idx] = w.Code
			}(i)
		})
	}

	wg.Wait()

	for i, code := range codes {
		if i%2 == 0 {
			if code != http.StatusOK {
				t.Errorf("goroutine %d (hit): expected 200, got %d", i, code)
			}
		} else {
			if code != http.StatusNotFound {
				t.Errorf("goroutine %d (miss): expected 404, got %d", i, code)
			}
		}
	}
}

func TestRoot200(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for GET /, got %d", w.Code)
	}
}

// TestNarInfoHEADContentLength pins a HEAD's Content-Length to the body a GET of
// the same URL would return. In zstd mode the two documents differ — the GET
// names a .nar.zstd URL and carries the compressed FileHash/FileSize — and
// measuring the real one means compressing the NAR, which a HEAD must not do, so
// the header is omitted there instead of being wrong.
func TestNarInfoHEADContentLength(t *testing.T) {
	t.Run("uncompressed", func(t *testing.T) {
		h, _ := newTestServer(t)
		assertHeadContentLength(t, h, true)
	})

	t.Run("zstd", func(t *testing.T) {
		_, h, _ := newZstdTestServer(t)
		assertHeadContentLength(t, h, false)
	})
}

// assertHeadContentLength checks HEAD /{hash}.narinfo against a GET of the same
// URL. wantHeader says whether Content-Length must be present at all; when it is
// present it must match the GET body exactly.
func assertHeadContentLength(t *testing.T, h http.Handler, wantHeader bool) {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodHead, "/"+testHash+".narinfo", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HEAD: expected 200, got %d", w.Code)
	}

	g := getBytes(t, h, "/"+testHash+".narinfo")
	if g.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d", g.Code)
	}

	cl := w.Header().Get("Content-Length")
	if cl == "" {
		if wantHeader {
			t.Error("HEAD sent no Content-Length")
		}

		return
	}

	if cl != strconv.Itoa(g.Body.Len()) {
		t.Errorf("HEAD Content-Length %s, but a GET of the same URL returns %d bytes:\n%s",
			cl, g.Body.Len(), g.Body.String())
	}
}

func TestWrongMethodNarInfo(t *testing.T) {
	h, _ := newTestServer(t)

	// POST to a .narinfo path is not an allowed method → 405.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/"+testHash+".narinfo", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST .narinfo: expected 405, got %d", w.Code)
	}
}

func TestWrongMethodNixCacheInfo(t *testing.T) {
	h, _ := newTestServer(t)

	// POST to /nix-cache-info — the catch-all "/" handler routes this to 404
	// (not 405) because /nix-cache-info doesn't look like a .narinfo path.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/nix-cache-info", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("POST /nix-cache-info: expected 404, got %d", w.Code)
	}
}

// fakeErrorStore returns an unexpected error (not ErrNotFound) from PathInfo.
type fakeErrorStore struct{}

func (f *fakeErrorStore) PathInfo(_ context.Context, _ string) (*store.PathInfo, error) {
	return nil, errSimulatedDBFailure
}

func (f *fakeErrorStore) PathCount(_ context.Context) (int64, error) { return 0, errSimulatedDBFailure }

func TestHealthBadStore(t *testing.T) {
	h := newCache(t, Config{
		Store:            &fakeErrorStore{},
		Priority:         30,
		SpoolDir:         t.TempDir(),
		ServeCompression: compressNone,
		StoreDir:         t.TempDir(),
	}).Handler()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d (body: %q)", w.Code, w.Body.String())
	}

	var resp struct {
		Status string `json:"status"`
	}

	err := json.NewDecoder(w.Body).Decode(&resp)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Status != "degraded" {
		t.Errorf("status = %q, want %q", resp.Status, "degraded")
	}
}

// unreadableDBStore models what a real *store.Store does when the database is
// gone: PathInfo screens the hash part before it ever queries, so an invalid
// probe path comes back ErrNotFound without touching the DB, while any real
// query fails. Health must notice the second one.
type unreadableDBStore struct{}

func (f *unreadableDBStore) PathInfo(_ context.Context, _ string) (*store.PathInfo, error) {
	return nil, store.ErrNotFound
}

func (f *unreadableDBStore) PathCount(_ context.Context) (int64, error) {
	return 0, errSimulatedDBFailure
}

// TestHealthProbesTheDatabase pins health to a probe that actually reaches the
// database. Probing with a sentinel path instead reports "ok" here, because a
// path that cannot exist is indistinguishable from a path that is merely absent.
func TestHealthProbesTheDatabase(t *testing.T) {
	h := newCache(t, Config{
		Store:            &unreadableDBStore{},
		Priority:         30,
		SpoolDir:         t.TempDir(),
		ServeCompression: compressNone,
		StoreDir:         t.TempDir(),
	}).Handler()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d (body: %q)", w.Code, w.Body.String())
	}

	var resp struct {
		Status         string `json:"status"`
		StoreReachable bool   `json:"store_reachable"`
	}

	err := json.NewDecoder(w.Body).Decode(&resp)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Status != "degraded" || resp.StoreReachable {
		t.Errorf("status = %q, store_reachable = %v; want %q and false",
			resp.Status, resp.StoreReachable, "degraded")
	}
}

var narMagic = []byte("\x0d\x00\x00\x00\x00\x00\x00\x00nix-archive-1")

// newZstdTestServer builds a zstd-serving Server backed by a real store path
// with non-trivial content, so the NAR/compress path has something to do.
func newZstdTestServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()

	srv, h, spoolDir, _ := newZstdTestServerDirs(t)

	return srv, h, spoolDir
}

// newZstdTestServerDirs is newZstdTestServer plus the physical store dir, which
// a caller needs to hand to narinfo.Parse: the served StorePath is under the
// temp store, and Parse rejects a StorePath outside the store dir it is given.
func newZstdTestServerDirs(t *testing.T) (*Server, http.Handler, string, string) {
	t.Helper()

	storeDir := t.TempDir()
	storePath := filepath.Join(storeDir, testHash+"-test")

	err := os.MkdirAll(storePath, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	content := bytes.Repeat([]byte("nixcache payload "), 4096)

	err = os.WriteFile(filepath.Join(storePath, "data"), content, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	s := &fakeStore{paths: map[string]*store.PathInfo{testHash: realPathInfo(t, storePath)}}

	spoolDir := t.TempDir()
	srv := newCache(t, Config{
		Store:            s,
		Priority:         30,
		SpoolDir:         spoolDir,
		ServeCompression: compressZstd,
		StoreDir:         storeDir,
	})

	return srv, srv.Handler(), spoolDir, storeDir
}

func getBytes(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	return w
}

func decodeZstd(t *testing.T, b []byte) []byte {
	t.Helper()

	rc, err := nixcompress.Decoder(context.Background(), bytes.NewReader(b), "zstd", false)
	if err != nil {
		t.Fatalf("zstd decoder: %v", err)
	}

	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("zstd read: %v", err)
	}

	_ = rc.Close()

	return out
}

// TestServeZstdNarRoundTrip confirms a zstd GET returns bytes that decompress
// to a valid NAR.
func TestServeZstdNarRoundTrip(t *testing.T) {
	_, h, _ := newZstdTestServer(t)

	w := getBytes(t, h, "/nar/"+testHash+".nar.zstd")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	nar := decodeZstd(t, w.Body.Bytes())
	if !bytes.HasPrefix(nar, narMagic) {
		t.Errorf("decompressed body is not a NAR (no magic prefix)")
	}
}

// TestNarInfoZstdHashMatchesServedNar confirms the FileHash/FileSize advertised
// in the narinfo exactly match the bytes the GET serves. This pins the cached
// hash/size to the real compressed output.
func TestNarInfoZstdHashMatchesServedNar(t *testing.T) {
	_, h, _, storeDir := newZstdTestServerDirs(t)

	ni := getBytes(t, h, "/"+testHash+".narinfo")
	if ni.Code != http.StatusOK {
		t.Fatalf("narinfo: expected 200, got %d", ni.Code)
	}

	parsed, err := narinfo.Parse(strings.NewReader(ni.Body.String()), storeDir)
	if err != nil {
		t.Fatalf("parse narinfo: %v", err)
	}

	w := getBytes(t, h, "/"+parsed.URL)
	if w.Code != http.StatusOK {
		t.Fatalf("nar GET: expected 200, got %d", w.Code)
	}

	served := w.Body.Bytes()

	sum := sha256.Sum256(served)
	wantHash := "sha256:" + nixbase32.EncodeToString(sum[:])

	if parsed.FileHash != wantHash {
		t.Errorf("FileHash %q != sha256 of served nar %q", parsed.FileHash, wantHash)
	}

	if parsed.FileSize != uint64(len(served)) {
		t.Errorf("FileSize %d != served bytes %d", parsed.FileSize, len(served))
	}

	decoded := decodeZstd(t, served)
	decodedHash := sha256.Sum256(decoded)
	require.Equal(t, parsed.NarHash, "sha256:"+nixbase32.EncodeToString(decodedHash[:]))
	require.Equal(t, parsed.NarSize, uint64(len(decoded)))
}

// TestZstdCacheFilesLandInSpoolSubdir confirms compressed NARs are cached on
// disk under spoolDir/zstd-cache, not in the process tmpfs.
func TestZstdCacheFilesLandInSpoolSubdir(t *testing.T) {
	_, h, spoolDir := newZstdTestServer(t)

	// Warm the cache via narinfo (which compresses to learn FileHash/FileSize).
	if w := getBytes(t, h, "/"+testHash+".narinfo"); w.Code != http.StatusOK {
		t.Fatalf("narinfo: expected 200, got %d", w.Code)
	}

	cacheDir := filepath.Join(spoolDir, ZstdCacheSubdir)

	entries, err := readCacheFiles(cacheDir)
	if err != nil {
		t.Fatalf("read cache dir %s: %v", cacheDir, err)
	}

	if len(entries) == 0 {
		t.Errorf("expected a cached zstd file under %s, found none", cacheDir)
	}
}

// TestZstdCacheStartupWipesOrphans confirms a stale cache file from a prior
// process is removed when a new Server is constructed.
func TestZstdCacheStartupWipesOrphans(t *testing.T) {
	spoolDir := t.TempDir()

	cacheDir := filepath.Join(spoolDir, ZstdCacheSubdir)

	err := os.MkdirAll(cacheDir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	orphan := filepath.Join(cacheDir, "tsnixcache-zstd-orphan.nar.zstd")

	err = os.WriteFile(orphan, []byte("stale"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	s := &fakeStore{paths: map[string]*store.PathInfo{}}
	_ = newCache(t, Config{
		Store:            s,
		Priority:         30,
		SpoolDir:         spoolDir,
		ServeCompression: compressZstd,
		StoreDir:         t.TempDir(),
	})

	_, statErr := os.Stat(orphan)
	if !os.IsNotExist(statErr) {
		t.Errorf("orphan cache file survived New(): stat err = %v", statErr)
	}
}

// TestSweepZstdCacheRemovesIdleKeepsFresh confirms the TTL sweep deletes idle
// cache entries and their files but keeps fresh ones.
func TestSweepZstdCacheRemovesIdleKeepsFresh(t *testing.T) {
	srv, h, spoolDir := newZstdTestServer(t)

	if w := getBytes(t, h, "/"+testHash+".narinfo"); w.Code != http.StatusOK {
		t.Fatalf("narinfo: expected 200, got %d", w.Code)
	}

	cacheDir := filepath.Join(spoolDir, ZstdCacheSubdir)

	// A generous TTL keeps the just-created entry.
	if n := srv.SweepZstdCache(time.Hour); n != 0 {
		t.Errorf("fresh entry swept with 1h TTL: removed %d", n)
	}

	if ents, _ := readCacheFiles(cacheDir); len(ents) == 0 {
		t.Errorf("fresh cache file removed by 1h-TTL sweep")
	}

	// TTL of 0 treats every existing entry as idle.
	if n := srv.SweepZstdCache(0); n == 0 {
		t.Errorf("idle entry not swept with 0 TTL")
	}

	if ents, _ := readCacheFiles(cacheDir); len(ents) != 0 {
		t.Errorf("cache file survived 0-TTL sweep: %d left", len(ents))
	}
}

// TestSweepSpoolRemovesAbandonedUploads confirms the spool sweep deletes NARs a
// client uploaded and never followed with a narinfo, keeps uploads that are
// still fresh, and never touches the zstd cache subdir.
func TestSweepSpoolRemovesAbandonedUploads(t *testing.T) {
	spoolDir := t.TempDir()

	cacheDir := filepath.Join(spoolDir, ZstdCacheSubdir)

	err := os.MkdirAll(cacheDir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	zstdFile := filepath.Join(cacheDir, "tsnixcache-zstd-old.nar.zstd")

	tests := []struct {
		name    string
		file    string
		age     time.Duration
		removed bool
	}{
		{name: "abandoned upload", file: "abandoned.nar", age: 48 * time.Hour, removed: true},
		{name: "upload in flight", file: "inflight.nar", age: 0, removed: false},
		{name: "recently written", file: "recent.nar.zstd", age: time.Minute, removed: false},
		{name: "just past the ttl", file: "stale.nar.xz", age: 2 * time.Hour, removed: true},
	}

	s := &fakeStore{paths: map[string]*store.PathInfo{}}
	srv := newCache(t, Config{
		Store:            s,
		Priority:         30,
		SpoolDir:         spoolDir,
		ServeCompression: compressNone,
		StoreDir:         t.TempDir(),
	})

	// After New(), because New() sweeps the whole spool: anything on disk when
	// the process starts is by definition abandoned.
	for _, tt := range tests {
		writeAged(t, filepath.Join(spoolDir, tt.file), tt.age)
	}

	// New() re-creates the zstd cache dir, so back-date it: the sweep must skip a
	// subdirectory on being one, not on its mtime. Without this the dir looks
	// fresh and a sweep that had lost its IsDir check would pass anyway.
	old := time.Now().Add(-48 * time.Hour)

	err = os.Chtimes(cacheDir, old, old)
	if err != nil {
		t.Fatal(err)
	}

	want := 0

	for _, tt := range tests {
		if tt.removed {
			want++
		}
	}

	if n := srv.SweepSpool(time.Hour); n != want {
		t.Errorf("SweepSpool removed %d, want %d", n, want)
	}

	for _, tt := range tests {
		_, statErr := os.Stat(filepath.Join(spoolDir, tt.file))
		if tt.removed && !os.IsNotExist(statErr) {
			t.Errorf("%s: %s survived the sweep (err %v)", tt.name, tt.file, statErr)
		}

		if !tt.removed && statErr != nil {
			t.Errorf("%s: %s was swept: %v", tt.name, tt.file, statErr)
		}
	}

	// New() wipes the zstd cache dir on startup, so only its survival as a
	// directory is meaningful here; re-create the file and sweep again.
	writeAged(t, zstdFile, 48*time.Hour)

	if n := srv.SweepSpool(time.Hour); n != 0 {
		t.Errorf("SweepSpool touched %d files on a second pass, want 0", n)
	}

	_, statErr := os.Stat(zstdFile)
	if statErr != nil {
		t.Errorf("spool sweep removed a zstd cache file: %v", statErr)
	}
}

// writeAged writes a file and back-dates its mtime by age.
func writeAged(t *testing.T, path string, age time.Duration) {
	t.Helper()

	err := os.WriteFile(path, []byte("nar bytes"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	when := time.Now().Add(-age)

	err = os.Chtimes(path, when, when)
	if err != nil {
		t.Fatal(err)
	}
}

// TestNarInfoHEADDoesNotCompress confirms a HEAD answers "do you have this"
// without compressing the NAR: `tsnixcache push` HEADs every path in a closure,
// so a no-op push must not cost a full-closure compression.
func TestNarInfoHEADDoesNotCompress(t *testing.T) {
	_, h, spoolDir := newZstdTestServer(t)

	cacheDir := filepath.Join(spoolDir, ZstdCacheSubdir)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodHead, "/"+testHash+".narinfo", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HEAD narinfo: expected 200, got %d", w.Code)
	}

	entries, err := readCacheFiles(cacheDir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 0 {
		t.Errorf("HEAD compressed the NAR: %d file(s) in %s", len(entries), cacheDir)
	}

	// A GET still compresses, because its body advertises the compressed
	// FileHash/FileSize a client will fetch.
	if g := getBytes(t, h, "/"+testHash+".narinfo"); g.Code != http.StatusOK {
		t.Fatalf("GET narinfo: expected 200, got %d", g.Code)
	}

	entries, err = readCacheFiles(cacheDir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Errorf("GET narinfo: expected 1 cached zstd file, got %d", len(entries))
	}
}

func TestZstdCompressionIsSingleFlighted(t *testing.T) {
	const concurrency = 8

	srv, infos := newZstdFixture(t, 2<<20, 1, false)
	for range cap(srv.zstdSem) {
		srv.zstdSem <- struct{}{}
	}

	release := sync.OnceFunc(func() {
		for range cap(srv.zstdSem) {
			<-srv.zstdSem
		}
	})
	defer release()

	var group errgroup.Group

	responses := make([]*httptest.ResponseRecorder, concurrency)
	h := srv.Handler()

	for i := range concurrency {
		group.Go(func() error {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, narInfoURL(infos[0]), nil))
			responses[i] = w

			return nil
		})
	}

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		srv.zstdCacheMu.Lock()
		defer srv.zstdCacheMu.Unlock()

		require.Len(c, srv.zstdFlights, 1)
		require.Equal(c, concurrency, srv.zstdFlights[infos[0].NarHash].waiters)
	}, 5*time.Second, time.Millisecond)
	release()
	require.NoError(t, group.Wait())

	for _, w := range responses {
		require.Equal(t, http.StatusOK, w.Code)
		ni, err := narinfo.Parse(w.Body, filepath.Dir(infos[0].StorePath))
		require.NoError(t, err)
		require.Equal(t, compressZstd, ni.Compression)
	}

	files, err := readCacheFiles(srv.zstdCacheDir)
	require.NoError(t, err)
	require.Len(t, files, 1)
}

// newBrokenNarServer serves a store path whose NAR generation fails part-way: a
// regular file of firstFileSize bytes is archived first, then a unix socket,
// which nar.Write cannot open. An uncompressed response is therefore already
// streaming when it fails. firstFileSize decides how much work is still in
// flight at that moment: the zstd encoder compresses blocks on background
// goroutines, so only a payload of many blocks leaves any of them running when
// the compression unwinds.
//
// The break must be one root cannot bypass. File permissions are not: root
// opens a 0o000 file happily, so a helper built on them has to skip when
// euid is 0, and every test below it — including the only gate on the zstd
// abort race — then vanishes from a root runner's suite with no failure and no
// signal. open(2) on a socket is ENXIO for every uid.
func newBrokenNarServer(t *testing.T, serveCompression string, firstFileSize int) *httptest.Server {
	t.Helper()

	storeDir := t.TempDir()
	storePath := filepath.Join(storeDir, testHash+"-test")

	err := os.MkdirAll(storePath, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	// Sorts before the socket, so its bytes are on the wire before the failure.
	payload := bytes.Repeat([]byte("payload "), firstFileSize/8)

	err = os.WriteFile(filepath.Join(storePath, "aaa"), payload, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	// Bind the socket in a short-named directory and rename it into place: a
	// bind path has to fit sockaddr_un's 108 bytes, which t.TempDir's
	// test-named directory leaves no room for. Closing the listener unlinks
	// the path it bound, not the renamed file, so the socket survives.
	bindDir, err := os.MkdirTemp("", "s") //nolint:usetesting // t.TempDir names the dir after the test, which the bind path has no room for
	if err != nil {
		t.Fatal(err)
	}

	defer os.RemoveAll(bindDir)

	var lc net.ListenConfig

	l, err := lc.Listen(t.Context(), "unix", filepath.Join(bindDir, "s"))
	if err != nil {
		t.Fatal(err)
	}

	defer l.Close()

	err = os.Rename(filepath.Join(bindDir, "s"), filepath.Join(storePath, "zzz"))
	if err != nil {
		t.Fatal(err)
	}

	// NarSize is what the uncompressed response declares as Content-Length, and
	// the NAR here never finishes, so it only has to be larger than the bytes
	// that do reach the wire — otherwise net/http would cut the response short
	// before nar.Write ever hits the socket.
	s := &fakeStore{paths: map[string]*store.PathInfo{
		testHash: {
			StorePath:  storePath,
			NarHash:    testNarHash,
			NarSize:    1 << 30,
			References: []string{},
		},
	}}
	srv := newCache(t, Config{
		Store:            s,
		Priority:         30,
		SpoolDir:         t.TempDir(),
		ServeCompression: serveCompression,
		StoreDir:         storeDir,
	})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return ts
}

// TestNarGenerationFailureAbortsConnection confirms an uncompressed NAR that
// fails mid-stream is never delivered as a well-formed short body. The 200 is
// already on the wire by then, so the connection is aborted and the client sees
// a transport error.
func TestNarGenerationFailureAbortsConnection(t *testing.T) {
	ts := newBrokenNarServer(t, compressNone, 128<<10)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/nar/"+testHash+".nar?hash="+testHash, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		// Aborting before any byte reached the client is also a hard failure;
		// nothing truncated was delivered.
		return
	}

	defer resp.Body.Close()

	_, err = io.ReadAll(resp.Body)
	if err == nil {
		t.Errorf("truncated NAR delivered as a complete %s response", resp.Status)
	}
}

// TestZstdNarGenerationFailureIsCleanError confirms a zstd NAR that cannot be
// built answers with a status instead of a torn connection: compression now
// completes to the cache file before anything is written, so the failure is
// still recoverable when it happens. Nothing partial reaches the client and no
// half-written cache file is left behind.
//
// The 64 MiB payload is the point — a small one finishes compressing before the
// failure, so only this size leaves the encoder's background block goroutines
// running when it unwinds. They write to the cache file and are joined by
// enc.Close; run with -race.
func TestZstdNarGenerationFailureIsCleanError(t *testing.T) {
	ts := newBrokenNarServer(t, compressZstd, 64<<20)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/nar/"+testHash+".nar.zstd?hash="+testHash, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("request failed instead of returning a status: %v", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500, got %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(body, narMagic) {
		t.Errorf("failed NAR leaked archive bytes to the client: %q", body)
	}
}

// TestImportRunsDetachedFromClientCancellation confirms the import is not killed
// by the client's request context. The client puts a stall deadline on the
// narinfo PUT, and cancelling nix-store part-way through wastes the whole
// transfer; the import is idempotent, so finishing it is always better.
func TestImportRunsDetachedFromClientCancellation(t *testing.T) {
	tests := []struct {
		name string
		// cancelBefore cancels the request context before the handler runs;
		// otherwise the importer cancels it while the import is under way.
		cancelBefore bool
	}{
		{name: "client gone before import starts", cancelBefore: true},
		{name: "client hangs up mid-import", cancelBefore: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var (
				called      bool
				importErr   error
				hasDeadline bool
			)

			fakeFn := ImporterFunc(func(importCtx context.Context, _ *narinfo.NarInfo) error {
				if !tt.cancelBefore {
					cancel()
				}

				called = true
				importErr = importCtx.Err()
				_, hasDeadline = importCtx.Deadline()

				return nil
			})

			s := &fakeStore{paths: map[string]*store.PathInfo{}}
			// The pushed narinfo names a /nix/store path, which Parse now
			// requires to sit under the server's store dir.
			h := newCache(t, Config{
				Store:            s,
				Priority:         30,
				SpoolDir:         t.TempDir(),
				ServeCompression: compressNone,
				StoreDir:         nixStoreDir,
				ImportFn:         fakeFn,
			}).Handler()

			if tt.cancelBefore {
				cancel()
			}

			niText := fmt.Sprintf(`StorePath: /nix/store/%s-test
URL: nar/%s.nar
Compression: none
FileHash: %s
FileSize: 42
NarHash: %s
NarSize: 42
References:
`, testHash, testHash, testNarHash, testNarHash)

			req := httptest.NewRequestWithContext(ctx, http.MethodPut, "/"+testHash+".narinfo", strings.NewReader(niText))
			h.ServeHTTP(httptest.NewRecorder(), req)

			if !called {
				t.Fatal("importer was not called")
			}

			if importErr != nil {
				t.Errorf("import ran under a cancelled context: %v", importErr)
			}

			if !hasDeadline {
				t.Error("import context has no deadline; a wedged nix-store would hold its slot forever")
			}
		})
	}
}

// lockedBuffer collects log output written from handler goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// captureLogs points the default slog logger at a buffer for the duration of the
// test and returns it. The package logs through slog.Default, so this is the only
// way to see what an operator would see.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()

	buf := &lockedBuffer{}
	prev := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return buf
}

// logLine returns the first record at the given level logged with the given
// message.
func logLine(t *testing.T, out, level, msg string) string {
	t.Helper()

	// TextHandler only quotes a message containing spaces.
	for line := range strings.SplitSeq(out, "\n") {
		if !strings.Contains(line, "level="+level) {
			continue
		}

		if strings.Contains(line, "msg="+msg+" ") || strings.Contains(line, `msg="`+msg+`" `) {
			return line
		}
	}

	t.Fatalf("no %s record with msg %q in log:\n%s", level, msg, out)

	return ""
}

// errReader fails every read, so a request body errors part-way through.
type errReader struct{}

func (errReader) Read(_ []byte) (int, error) { return 0, errSimulatedBodyFailure }

var errSimulatedBodyFailure = errors.New("simulated body failure")

// TestErrorLogsNameThePath pins the store path (or spool destination) onto the
// error logs that report a failure for one specific path. Without it an operator
// running a cache for a whole fleet sees "import failed" with no way to tell
// which of thousands of pushed paths broke.
func TestErrorLogsNameThePath(t *testing.T) {
	t.Run("import failure", func(t *testing.T) {
		logs := captureLogs(t)

		failing := ImporterFunc(func(_ context.Context, _ *narinfo.NarInfo) error {
			return errSimulatedDBFailure
		})

		s := &fakeStore{paths: map[string]*store.PathInfo{}}
		srv := newCache(t, Config{
			Store:            s,
			Priority:         30,
			SpoolDir:         t.TempDir(),
			ServeCompression: compressNone,
			StoreDir:         nixStoreDir,
			ImportFn:         failing,
		})

		niText := fmt.Sprintf(`StorePath: /nix/store/%s-test
URL: nar/%s.nar
Compression: none
FileHash: %s
FileSize: 42
NarHash: %s
NarSize: 42
References:
`, testHash, testHash, testNarHash, testNarHash)

		req := httptest.NewRequestWithContext(context.Background(), http.MethodPut,
			"/"+testHash+".narinfo", strings.NewReader(niText))
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d", w.Code)
		}

		line := logLine(t, logs.String(), "ERROR", "import")
		if !strings.Contains(line, "path=/nix/store/"+testHash+"-test") {
			t.Errorf("import error names no store path: %s", line)
		}
	})

	t.Run("nar generation abort", func(t *testing.T) {
		logs := captureLogs(t)
		ts := newBrokenNarServer(t, compressNone, 128<<10)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			ts.URL+"/nar/"+testHash+".nar?hash="+testHash, nil)
		if err != nil {
			t.Fatal(err)
		}

		resp, err := ts.Client().Do(req)
		if err == nil {
			// Both errors are expected: the handler aborts the connection
			// mid-body, which is the behaviour under test.
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}

		line := logLine(t, logs.String(), "ERROR", "nar: write")
		if !strings.Contains(line, "path=") || !strings.Contains(line, testHash+"-test") {
			t.Errorf("aborted NAR names no store path: %s", line)
		}
	})

	t.Run("put nar body", func(t *testing.T) {
		logs := captureLogs(t)
		h, _ := newTestServer(t)

		req := httptest.NewRequestWithContext(context.Background(), http.MethodPut,
			"/nar/"+testHash+".nar", errReader{})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d", w.Code)
		}

		line := logLine(t, logs.String(), "ERROR", "put nar: copy body")
		if !strings.Contains(line, testHash+".nar") {
			t.Errorf("failed upload names no destination: %s", line)
		}
	})

	// A rejected oversized upload used to return 413 and log nothing at all, so
	// the client saw a bare status code and the operator it complained to had
	// no server-side record that the push had even arrived.
	t.Run("oversized body", func(t *testing.T) {
		logs := captureLogs(t)
		h, _ := newTestServer(t)

		// Comfortably past maxNarInfoBytes (1 MiB).
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPut,
			"/"+testHash+".narinfo", strings.NewReader(strings.Repeat("x", 2<<20)))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("expected 413, got %d", w.Code)
		}

		line := logLine(t, logs.String(), "WARN", "put narinfo: read body")
		if !strings.Contains(line, testHash+".narinfo") {
			t.Errorf("rejected oversized upload names no path: %s", line)
		}
	})
}

// countingWriter counts the bytes written to it and discards them.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))

	return len(p), nil
}

// narSize returns the exact NAR size of a store path. Fake PathInfos have to
// carry the real number because the server declares it as Content-Length, and
// net/http enforces that on a real connection.
func testNarSize(t *testing.T, path string) int64 {
	t.Helper()

	var c countingWriter

	err := nar.Write(&c, path)
	if err != nil {
		t.Fatal(err)
	}

	return c.n
}

// TestNewSweepsSpoolAtStartup confirms spool files left by a previous process
// are reclaimed before anything is served. A client retry re-runs the transfer
// from the presence HEAD, so every one of them is garbage — and with a crash
// loop they accumulate one partial NAR (up to 16 GiB) per path for a full day.
func TestNewSweepsSpoolAtStartup(t *testing.T) {
	spoolDir := t.TempDir()

	orphan := filepath.Join(spoolDir, testHash+".nar")

	// Freshly written: a startup orphan is garbage whatever its age.
	writeAged(t, orphan, 0)

	s := &fakeStore{paths: map[string]*store.PathInfo{}}
	_ = newCache(t, Config{
		Store:            s,
		Priority:         30,
		SpoolDir:         spoolDir,
		ServeCompression: compressNone,
		StoreDir:         t.TempDir(),
	})

	_, err := os.Stat(orphan)
	if !os.IsNotExist(err) {
		t.Errorf("spool orphan survived New(): stat err = %v", err)
	}
}

// pacedReader emits its body in two halves and blocks on a shared barrier
// between them, so two uploads are provably inside io.Copy at the same time
// rather than relying on scheduling luck.
type pacedReader struct {
	data []byte
	off  int
	wg   *sync.WaitGroup
	once sync.Once
}

func (r *pacedReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}

	if r.off >= len(r.data)/2 {
		r.once.Do(func() {
			r.wg.Done()
			r.wg.Wait()
		})
	}

	n := copy(p, r.data[r.off:])
	r.off += n

	return n, nil
}

// TestConcurrentPutSameNarDoesNotInterleave confirms two pushes of the same
// store path cannot splice their bodies together. The spool name is derived from
// the store path, and running the post-build hook alongside watch — or any two
// builders sharing a closure — makes that collision routine. Writing in place
// truncated and interleaved them; verification downstream caught it, so the only
// symptom was a silently doubled push.
func TestConcurrentPutSameNarDoesNotInterleave(t *testing.T) {
	const size = 200_000

	h, spoolDir := newTestServer(t)

	var wg sync.WaitGroup

	wg.Add(2)

	bodies := [][]byte{bytes.Repeat([]byte("A"), size), bytes.Repeat([]byte("B"), size)}
	codes := make([]int, len(bodies))

	var done sync.WaitGroup

	for i, body := range bodies {
		done.Go(func() {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPut,
				"/nar/"+testHash+".nar", &pacedReader{data: body, wg: &wg})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			codes[i] = w.Code
		})
	}

	done.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("upload %d: expected 200, got %d", i, code)
		}
	}

	got, err := os.ReadFile(filepath.Join(spoolDir, testHash+".nar")) // #nosec G304 -- temp dir path
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, bodies[0]) && !bytes.Equal(got, bodies[1]) {
		t.Errorf("spool file is a splice of both uploads: %d bytes, %d A's, %d B's",
			len(got), bytes.Count(got, []byte("A")), bytes.Count(got, []byte("B")))
	}

	// Nothing but the finished NAR may be left behind.
	ents, err := os.ReadDir(spoolDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, ent := range ents {
		if !ent.IsDir() && ent.Name() != ".tsnixlock" && ent.Name() != testHash+".nar" {
			t.Errorf("temp file left in spool: %s", ent.Name())
		}
	}
}

// TestPutNarOverDirectoryLeavesItIntact confirms an upload named after an
// existing directory — the zstd cache subdir is the reachable one — fails
// without destroying it.
func TestPutNarOverDirectoryLeavesItIntact(t *testing.T) {
	h, spoolDir := newTestServer(t)

	dir := filepath.Join(spoolDir, ZstdCacheSubdir)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut,
		"/nar/"+ZstdCacheSubdir, strings.NewReader("nar bytes"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}

	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("zstd cache dir was clobbered: %v", err)
	}

	// The rejected upload must not leave its temp behind either.
	ents, err := os.ReadDir(spoolDir)
	if err != nil {
		t.Fatal(err)
	}

	for _, ent := range ents {
		if !ent.IsDir() && ent.Name() != ".tsnixlock" {
			t.Errorf("temp file left in spool: %s", ent.Name())
		}
	}
}

// TestStoreRootPrefix confirms a chroot store serves NARs from under the
// physical prefix while every path it reports stays logical: nix-cache-info and
// the narinfo StorePath both name /nix/store, because that is what the DB holds
// and what a client's signature covers.
func TestStoreRootPrefix(t *testing.T) {
	root := t.TempDir()

	logical := "/nix/store/" + testHash + "-test"
	physical := filepath.Join(root, logical)

	err := os.MkdirAll(physical, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(physical, "hello"), []byte("chrooted"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	s := &fakeStore{paths: map[string]*store.PathInfo{
		testHash: {
			StorePath:  logical,
			NarHash:    testNarHash,
			NarSize:    testNarSize(t, physical),
			References: []string{},
		},
	}}
	h := newCache(t, Config{
		Store:            s,
		Priority:         30,
		SpoolDir:         t.TempDir(),
		ServeCompression: compressNone,
		StoreDir:         nixStoreDir,
		StoreRoot:        root,
	}).Handler()

	info := getBytes(t, h, "/nix-cache-info")
	if !strings.Contains(info.Body.String(), "StoreDir: /nix/store") {
		t.Errorf("nix-cache-info leaked the chroot prefix: %q", info.Body.String())
	}

	ni := getBytes(t, h, "/"+testHash+".narinfo")
	if ni.Code != http.StatusOK {
		t.Fatalf("narinfo: expected 200, got %d", ni.Code)
	}

	if !strings.Contains(ni.Body.String(), "StorePath: "+logical) {
		t.Errorf("narinfo does not report the logical store path:\n%s", ni.Body.String())
	}

	if strings.Contains(ni.Body.String(), root) {
		t.Errorf("narinfo leaked the chroot prefix:\n%s", ni.Body.String())
	}

	w := getBytes(t, h, "/nar/"+testHash+".nar?hash="+testHash)
	if w.Code != http.StatusOK {
		t.Fatalf("nar: expected 200, got %d", w.Code)
	}

	if !bytes.Contains(w.Body.Bytes(), []byte("chrooted")) {
		t.Error("NAR was not read from under the chroot prefix")
	}
}

// TestStaleCompressedURLIsNotFound confirms a client replaying a narinfo it
// cached before the server was restarted with --serve-compression none gets a
// 404 rather than a raw NAR under a .zstd name, which it would fail to decode
// after a 200. nix keeps a narinfo positive for 30 days, so this outlives most
// restarts.
func TestStaleCompressedURLIsNotFound(t *testing.T) {
	h, _ := newTestServer(t)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req := httptest.NewRequestWithContext(context.Background(), method,
			"/nar/"+testHash+".nar.zstd?hash="+testHash, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("%s stale .zstd URL: expected 404, got %d", method, w.Code)
		}
	}
}

// TestPutNarInfoRejectsUnsupportedCompression confirms a codec the server cannot
// decode is refused with a 400 that names it. As a 500 out of the import it was
// retried five times, re-uploading the whole NAR each time to learn something
// the first request already knew.
func TestPutNarInfoRejectsUnsupportedCompression(t *testing.T) {
	h, _ := newTestServer(t)

	niText := fmt.Sprintf(`StorePath: /nix/store/%s-test
URL: nar/%s.nar.gz
Compression: gzip
FileHash: %s
FileSize: 42
NarHash: %s
NarSize: 42
References:
`, testHash, testHash, testNarHash, testNarHash)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut,
		"/"+testHash+".narinfo", strings.NewReader(niText))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (body %q)", w.Code, w.Body.String())
	}

	if !strings.Contains(w.Body.String(), "gzip") {
		t.Errorf("rejection does not name the codec: %q", w.Body.String())
	}
}

// TestPushFailuresAreCounted confirms a broken push moves a counter. Without one
// a push outage makes every push line fall towards zero, which on a dashboard is
// indistinguishable from "nobody is pushing".
func TestPushFailuresAreCounted(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut,
		"/nar/"+testHash+".nar", errReader{})
	h.ServeHTTP(httptest.NewRecorder(), req)

	bad := httptest.NewRequestWithContext(context.Background(), http.MethodPut,
		"/"+testHash+".narinfo", strings.NewReader("not a narinfo"))
	h.ServeHTTP(httptest.NewRecorder(), bad)

	body := getBytes(t, h, "/metrics").Body.String()
	for _, want := range []string{
		`tsnixcache_push_errors_total{reason="nar_body"} 1`,
		`tsnixcache_push_errors_total{reason="narinfo_parse"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in metrics:\n%s", want, body)
		}
	}
}

// TestImportIsTimed confirms the import histogram is actually observed. It is
// the whole cost of a push as the client experiences it — the server runs
// verify+import inside the final narinfo PUT and sends nothing meanwhile — and
// an unobserved histogram leaves the dashboard's p95 panel blank, which reads
// exactly like a cache nobody is pushing to.
func TestImportIsTimed(t *testing.T) {
	h := newCache(t, Config{
		Store:            &fakeStore{paths: map[string]*store.PathInfo{}},
		Priority:         30,
		SpoolDir:         t.TempDir(),
		ServeCompression: compressNone,
		StoreDir:         nixStoreDir,
		ImportFn: ImporterFunc(func(_ context.Context, _ *narinfo.NarInfo) error {
			return nil
		}),
	}).Handler()

	niText := fmt.Sprintf(`StorePath: /nix/store/%s-test
URL: nar/%s.nar
Compression: none
FileHash: %s
FileSize: 42
NarHash: %s
NarSize: 42
References:
`, testHash, testHash, testNarHash, testNarHash)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut,
		"/"+testHash+".narinfo", strings.NewReader(niText))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("narinfo PUT = %d, want 200 (body: %q)", w.Code, w.Body.String())
	}

	body := getBytes(t, h, "/metrics").Body.String()
	if !strings.Contains(body, "tsnixcache_import_duration_seconds_count 1") {
		t.Errorf("import ran but was not timed:\n%s", body)
	}
}

// TestFailedPutNarStillCountsBytes confirms an upload that dies part-way still
// contributes the bytes the link carried. README tells operators rate() gives
// write throughput, and counting only completed uploads understates it worst
// during an incident.
func TestFailedPutNarStillCountsBytes(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut,
		"/nar/"+testHash+".nar", io.MultiReader(strings.NewReader(strings.Repeat("x", 4096)), errReader{}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	body := getBytes(t, h, "/metrics").Body.String()
	if !strings.Contains(body, "tsnixcache_nar_bytes_received_total 4096") {
		t.Errorf("aborted upload contributed no received bytes:\n%s", body)
	}
}

// TestNarOverRealConnection exercises both NAR responses over a real socket,
// where net/http enforces the Content-Length the handler declares and the warm
// zstd path hands its file down through ReadFrom. A recorder checks neither.
func TestNarOverRealConnection(t *testing.T) {
	tests := []struct {
		name        string
		compression string
		path        string
	}{
		{name: "uncompressed", compression: compressNone, path: "/nar/" + testHash + ".nar?hash=" + testHash},
		{name: "zstd", compression: compressZstd, path: "/nar/" + testHash + ".nar.zstd?hash=" + testHash},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storeDir := t.TempDir()
			storePath := filepath.Join(storeDir, testHash+"-test")

			err := os.MkdirAll(storePath, 0o750)
			if err != nil {
				t.Fatal(err)
			}

			err = os.WriteFile(filepath.Join(storePath, "data"),
				bytes.Repeat([]byte("nixcache payload "), 4096), 0o600)
			if err != nil {
				t.Fatal(err)
			}

			s := &fakeStore{paths: map[string]*store.PathInfo{
				testHash: {
					StorePath:  storePath,
					NarHash:    testNarHash,
					NarSize:    testNarSize(t, storePath),
					References: []string{},
				},
			}}
			ts := httptest.NewServer(newCache(t, Config{
				Store:            s,
				Priority:         30,
				SpoolDir:         t.TempDir(),
				ServeCompression: tt.compression,
				StoreDir:         storeDir,
			}).Handler())
			t.Cleanup(ts.Close)

			// Twice: the second GET is the warm-cache path in zstd mode, which
			// exists at all only because the first one populated it.
			for range 2 {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+tt.path, nil)
				if err != nil {
					t.Fatal(err)
				}

				resp, err := ts.Client().Do(req)
				if err != nil {
					t.Fatalf("GET %s: %v", tt.path, err)
				}

				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()

				if err != nil {
					t.Fatalf("read body: %v", err)
				}

				if resp.StatusCode != http.StatusOK {
					t.Fatalf("expected 200, got %s", resp.Status)
				}

				if resp.ContentLength != int64(len(body)) {
					t.Errorf("Content-Length %d, body %d bytes", resp.ContentLength, len(body))
				}

				got := body
				if tt.compression == compressZstd {
					got = decodeZstd(t, body)
				}

				if !bytes.HasPrefix(got, narMagic) {
					t.Error("response is not a NAR")
				}
			}
		})
	}
}

// TestDebugSurfaceForTailnetPeer pins what a tailnet peer without the push grant
// can read. Reads are open by design, but /debug/ exposes pprof and expvar and
// /metrics is not behind the debug gate at all, so both are worth stating
// explicitly rather than discovering.
func TestDebugSurfaceForTailnetPeer(t *testing.T) {
	h, _ := newTestServer(t)

	tests := []struct {
		name string
		addr string
		path string
		want int
	}{
		{name: "debug from tailnet peer", addr: "100.100.100.100:1234", path: "/debug/", want: http.StatusOK},
		{name: "debug from off-tailnet", addr: offTailnetAddr, path: "/debug/", want: http.StatusForbidden},
		{name: "metrics from tailnet peer", addr: "100.100.100.100:1234", path: "/metrics", want: http.StatusOK},
		// /metrics is registered on the mux directly, not under the debug gate,
		// so any reader that can reach the listener can scrape it.
		{name: "metrics from off-tailnet", addr: offTailnetAddr, path: "/metrics", want: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, tt.path, nil)
			req.RemoteAddr = tt.addr
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != tt.want {
				t.Errorf("%s %s: expected %d, got %d", tt.addr, tt.path, tt.want, w.Code)
			}
		})
	}
}

func newCache(tb testing.TB, cfg Config) *Server {
	tb.Helper()

	srv, err := New(cfg)
	require.NoError(tb, err)
	srv.SetNarWriteTimeoutForTesting(0)
	tb.Cleanup(func() { require.NoError(tb, srv.Close()) })

	return srv
}

func TestPutNarFailsWhenSpoolCloseFails(t *testing.T) {
	spoolDir := t.TempDir()
	srv := newCache(t, Config{
		SpoolDir: spoolDir,
		StoreDir: nixStoreDir,
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

func TestDecodedSizeRejectedBeforeImport(t *testing.T) {
	called := false
	h, _ := newTestServer(t, func(cfg *Config) {
		cfg.MaxNarSize = 100
		cfg.ImportFn = func(context.Context, *narinfo.NarInfo) error {
			called = true

			return nil
		}
	})
	ni := &narinfo.NarInfo{
		StorePath: "/nix/store/" + testHash + "-test", URL: "nar/test.nar",
		Compression: "none", NarHash: testNarHash, NarSize: 101,
		FileHash: testNarHash, FileSize: 101,
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/"+testHash+".narinfo", strings.NewReader(ni.Marshal()))
	h.ServeHTTP(w, r)

	if w.Code != http.StatusRequestEntityTooLarge || called {
		t.Fatalf("status=%d imported=%t body=%s", w.Code, called, w.Body.String())
	}
}

func TestReadRequestWithMissingBodyClosesConnection(t *testing.T) {
	srv := newCache(t, Config{SpoolDir: t.TempDir()})

	var closed atomic.Int64

	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed.Add(1)
		}
	}

	ts.Start()
	defer ts.Close()

	for _, framing := range []string{"Content-Length: 1", "Transfer-Encoding: chunked"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+"/"+framing, func(t *testing.T) {
				before := closed.Load()
				conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", ts.Listener.Addr().String())
				require.NoError(t, err)

				defer conn.Close()

				require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
				_, err = fmt.Fprintf(conn, "%s /nar/%s.nar.zstd HTTP/1.1\r\nHost: test\r\n%s\r\n\r\n", method, testHash, framing)
				require.NoError(t, err)
				response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
				require.NoError(t, err)

				defer response.Body.Close()

				require.Equal(t, http.StatusBadRequest, response.StatusCode)
				require.True(t, response.Close)
				_, err = io.Copy(io.Discard, response.Body)
				require.NoError(t, err)
				require.EventuallyWithT(t, func(c *assert.CollectT) { require.Greater(c, closed.Load(), before) }, time.Second, time.Millisecond)
				require.Empty(t, srv.zstdFlights)
				require.Zero(t, srv.zstdCacheBytes)
			})
		}
	}
}

// noGrantWhoIs identifies the peer but hands it no push grant.
type noGrantWhoIs struct{}

func (noGrantWhoIs) WhoIs(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
	return &apitype.WhoIsResponse{
		Node:        &tailcfg.Node{Name: "builder.tail.example.ts.net"},
		UserProfile: &tailcfg.UserProfile{LoginName: "builder@example.com"},
		CapMap:      tailcfg.PeerCapMap{},
	}, nil
}

// emptyStore satisfies cache.StoreProvider; the server's own store collector
// is scraped alongside the auth counter, so it needs something non-nil.
type emptyStore struct{}

func (emptyStore) PathInfo(_ context.Context, _ string) (*store.PathInfo, error) {
	return nil, nil //nolint:nilnil // unused by this test
}

func (emptyStore) PathCount(_ context.Context) (int64, error) { return 0, nil }

func TestRejectsMetricIsServedOnMetricsEndpoint(t *testing.T) {
	srv, err := New(Config{
		Store:            emptyStore{},
		Priority:         30,
		SpoolDir:         t.TempDir(),
		ServeCompression: "none",
		StoreDir:         "/nix/store",
	})

	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	// Reject one write of each shape: one on a tsnet listener whose peer holds
	// no grant, one on a listener that has no identity at all. The counter is
	// package-global, so this is the same counter the server's registry has to
	// be exporting.
	rec := httptest.NewRecorder()
	put := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/nar/abc.nar", nil)
	auth.Middleware(noGrantWhoIs{})(http.NotFoundHandler()).ServeHTTP(rec, put)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT without push grant: want 403, got %d", rec.Code)
	}

	local := httptest.NewRecorder()
	localPut := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/nar/abc.nar", nil)
	auth.ReadOnly(http.NotFoundHandler()).ServeHTTP(local, localPut)

	if local.Code != http.StatusForbidden {
		t.Fatalf("PUT on a listener with no identity: want 403, got %d", local.Code)
	}

	scrape := httptest.NewRecorder()
	get := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	srv.Handler().ServeHTTP(scrape, get)

	if scrape.Code != http.StatusOK {
		t.Fatalf("GET /metrics: want 200, got %d", scrape.Code)
	}

	for _, want := range []string{
		`tsnixcache_auth_rejects_total{reason="no_grant"}`,
		`tsnixcache_auth_rejects_total{reason="local_write"}`,
	} {
		if !strings.Contains(scrape.Body.String(), want) {
			t.Errorf("server /metrics does not export %s; an operator cannot alert on rejected pushes.\nexported:\n%s",
				want, scrape.Body.String())
		}
	}
}

func startTestTask(tb testing.TB, fn func()) {
	tb.Helper()

	var group errgroup.Group
	group.Go(func() error {
		fn()

		return nil
	})
	tb.Cleanup(func() { require.NoError(tb, group.Wait()) })
}

// Streaming fixture creation keeps payload-sized allocations out of benchmarks.
func newZstdFixture(tb testing.TB, size, count int, high bool) (*Server, []*store.PathInfo) {
	tb.Helper()
	storeDir := tb.TempDir()
	paths := make(map[string]*store.PathInfo, count)

	infos := make([]*store.PathInfo, 0, count)
	for variant := range count {
		seed := sha256.Sum256([]byte(strconv.Itoa(variant)))
		hash := nixbase32.EncodeToString(seed[:20])
		path := filepath.Join(storeDir, hash+"-fixture")
		require.NoError(tb, os.Mkdir(path, 0o700))
		f, err := os.Create(filepath.Join(path, "data")) // #nosec G304 -- private fixture under tb.TempDir.
		require.NoError(tb, err)

		chunk := bytes.Repeat(seed[:], 2048)
		for remaining := size; remaining > 0; remaining -= min(remaining, len(chunk)) {
			if high {
				for offset := 0; offset < len(chunk); offset += len(seed) {
					seed = sha256.Sum256(seed[:])
					copy(chunk[offset:], seed[:])
				}
			}

			_, err = f.Write(chunk[:min(remaining, len(chunk))])
			require.NoError(tb, err)
		}

		require.NoError(tb, f.Close())
		pi := realPathInfo(tb, path)
		paths[hash] = pi
		infos = append(infos, pi)
	}

	srv := newCache(tb, Config{
		Store: &fakeStore{paths: paths}, Priority: 30,
		SpoolDir: tb.TempDir(), StoreDir: storeDir, ServeCompression: compressZstd, ImportConcurrency: 4,
	})

	return srv, infos
}

func realPathInfo(tb testing.TB, path string) *store.PathInfo {
	tb.Helper()

	h := sha256.New()
	cw := &countWriter{w: h}
	require.NoError(tb, nar.Write(cw, path))

	return &store.PathInfo{StorePath: path, NarHash: "sha256:" + nixbase32.EncodeToString(h.Sum(nil)), NarSize: cw.n}
}

func narInfoURL(pi *store.PathInfo) string {
	return "/" + filepath.Base(pi.StorePath)[:32] + ".narinfo"
}

func TestNarInfoZstdOversizedFallback(t *testing.T) {
	srv, infos := newZstdFixture(t, 2<<20, 1, true)
	srv.zstdCacheLimit = zstdReservationChunk
	host := httptest.NewServer(srv.Handler())
	t.Cleanup(host.Close)

	fetch := func(method, path string) (int, http.Header, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, host.URL+path, nil)
		require.NoError(t, err)
		resp, err := host.Client().Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err)

		return resp.StatusCode, resp.Header, body
	}
	assertEmpty := func() {
		t.Helper()
		srv.zstdCacheMu.Lock()
		defer srv.zstdCacheMu.Unlock()

		require.Zero(t, srv.zstdCacheBytes)
		require.Empty(t, srv.zstdCache)
		files, err := readCacheFiles(srv.zstdCacheDir)
		require.NoError(t, err)
		require.Empty(t, files)
	}
	code, _, body := fetch(http.MethodHead, narInfoURL(infos[0]))
	require.Equal(t, http.StatusOK, code)
	require.Empty(t, body)
	assertEmpty()

	code, _, body = fetch(http.MethodGet, narInfoURL(infos[0]))
	require.Equal(t, http.StatusOK, code)

	ni, err := narinfo.Parse(bytes.NewReader(body), filepath.Dir(infos[0].StorePath))
	require.NoError(t, err)
	require.Equal(t, compressNone, ni.Compression)
	require.Equal(t, ni.NarHash, ni.FileHash)
	require.Equal(t, ni.NarSize, ni.FileSize)
	assertEmpty()

	code, _, body = fetch(http.MethodGet, "/"+ni.URL)
	require.Equal(t, http.StatusOK, code)

	sum := sha256.Sum256(body)
	require.Equal(t, ni.FileHash, "sha256:"+nixbase32.EncodeToString(sum[:]))
	require.Equal(t, ni.FileSize, uint64(len(body)))
	code, headers, _ := fetch(http.MethodGet, "/"+ni.URL+".zstd")
	require.Equal(t, http.StatusServiceUnavailable, code)
	require.NotEmpty(t, headers.Get("Retry-After"))
	assertEmpty()
}

func TestAdvertisedZstdURLSurvivesAdmissionPressure(t *testing.T) {
	srv, infos := newZstdFixture(t, 2<<20, 2, false)
	srv.zstdCacheLimit = zstdReservationChunk
	h := srv.Handler()
	w := getBytes(t, h, narInfoURL(infos[0]))
	require.Equal(t, http.StatusOK, w.Code)
	ni, err := narinfo.Parse(w.Body, filepath.Dir(infos[0].StorePath))
	require.NoError(t, err)
	require.Equal(t, compressZstd, ni.Compression)
	require.Equal(t, 1, srv.SweepZstdCache(0))
	pinned, err := srv.acquireZstd(t.Context(), infos[1])
	require.NoError(t, err)

	defer pinned.Close()

	w = getBytes(t, h, "/"+ni.URL)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.NotEmpty(t, w.Header().Get("Retry-After"))
	w = getBytes(t, h, narInfoURL(infos[0]))
	require.Equal(t, http.StatusOK, w.Code)
	fallback, err := narinfo.Parse(w.Body, filepath.Dir(infos[0].StorePath))
	require.NoError(t, err)
	require.Equal(t, compressNone, fallback.Compression)
	pinned.Close()

	w = getBytes(t, h, "/"+ni.URL)
	require.Equal(t, http.StatusOK, w.Code)
	sum := sha256.Sum256(w.Body.Bytes())
	require.Equal(t, ni.FileHash, "sha256:"+nixbase32.EncodeToString(sum[:]))
	require.Equal(t, ni.FileSize, uint64(len(w.Body.Bytes())))
}
