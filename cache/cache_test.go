package cache_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kradalby/tsnixcache/cache"
	"github.com/kradalby/tsnixcache/narinfo"
	"github.com/kradalby/tsnixcache/signing"
	"github.com/kradalby/tsnixcache/store"
)

// fakeStore satisfies cache.StoreProvider.
type fakeStore struct {
	paths map[string]*store.PathInfo
}

func (f *fakeStore) PathInfo(_ context.Context, hashPart string) (*store.PathInfo, error) {
	if pi, ok := f.paths[hashPart]; ok {
		return pi, nil
	}
	return nil, store.ErrNotFound
}

// fakePathInfo returns a minimal PathInfo for testing.
func fakePathInfo(hash string) *store.PathInfo {
	return &store.PathInfo{
		StorePath:  "/nix/store/" + hash + "-test",
		NarHash:    "sha256:" + hash,
		NarSize:    42,
		References: []string{},
		Deriver:    "",
		Sigs:       nil,
		CA:         "",
	}
}

const testHash = "abc12300000000000000000000000000"

// newTestServer creates a Server for tests with a fake store and temp spoolDir.
func newTestServer(t *testing.T, opts ...func(*cache.Server)) (http.Handler, string) {
	t.Helper()
	spoolDir := t.TempDir()

	s := &fakeStore{
		paths: map[string]*store.PathInfo{
			testHash: fakePathInfo(testHash),
		},
	}

	srv := cache.New(s, nil, 30, spoolDir, "none", nil)
	for _, o := range opts {
		o(srv)
	}
	return srv.Handler(), spoolDir
}

func TestNixCacheInfo(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/nix-cache-info", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/"+testHash+".narinfo", nil)
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
	if !strings.Contains(body, "NarHash: sha256:"+testHash) {
		t.Errorf("missing NarHash in narinfo body: %q", body)
	}
}

func TestNarInfo404(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/missing0000000000000000000000000.narinfo", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestNarInfoHEAD(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodHead, "/"+testHash+".narinfo", nil)
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

	h, _ := newTestServer(t, func(srv *cache.Server) {
		srv.Signer = sk
	})

	req := httptest.NewRequest(http.MethodGet, "/"+testHash+".narinfo", nil)
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
	ni, err := narinfo.Parse(strings.NewReader(body))
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
	if err := os.MkdirAll(storePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storePath, "hello"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &fakeStore{
		paths: map[string]*store.PathInfo{
			testHash: {
				StorePath:  storePath,
				NarHash:    "sha256:" + testHash,
				NarSize:    42,
				References: []string{},
			},
		},
	}
	srv := cache.New(s, nil, 30, t.TempDir(), "none", nil)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/nar/"+testHash+".nar?hash="+testHash, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	if err := os.MkdirAll(storePath, 0o755); err != nil {
		t.Fatal(err)
	}

	s := &fakeStore{
		paths: map[string]*store.PathInfo{
			testHash: {
				StorePath:  storePath,
				NarHash:    "sha256:" + testHash,
				NarSize:    42,
				References: []string{},
			},
		},
	}
	srv := cache.New(s, nil, 30, t.TempDir(), "none", nil)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodHead, "/nar/"+testHash+".nar?hash="+testHash, nil)
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

	req := httptest.NewRequest(http.MethodGet, "/nar/missing0000000000000000000000000.nar", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestPutNar(t *testing.T) {
	h, spoolDir := newTestServer(t)

	body := strings.NewReader("fake nar content")
	req := httptest.NewRequest(http.MethodPut, "/nar/"+testHash+".nar", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %q)", w.Code, w.Body.String())
	}
	dest := filepath.Join(spoolDir, testHash+".nar")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("spooled file not found: %v", err)
	}
	if string(data) != "fake nar content" {
		t.Errorf("unexpected spool content: %q", data)
	}
}

func TestPutNarInfoTriggersImport(t *testing.T) {
	var mu sync.Mutex
	var importedNI *narinfo.NarInfo

	fakeFn := cache.ImporterFunc(func(_ context.Context, ni *narinfo.NarInfo) error {
		mu.Lock()
		importedNI = ni
		mu.Unlock()
		return nil
	})

	s := &fakeStore{paths: map[string]*store.PathInfo{}}
	srv := cache.New(s, nil, 30, t.TempDir(), "none", fakeFn)
	h := srv.Handler()

	niText := fmt.Sprintf(`StorePath: /nix/store/%s-test
URL: nar/%s.nar
Compression: none
FileHash: sha256:%s
FileSize: 42
NarHash: sha256:%s
NarSize: 42
References:
`, testHash, testHash, testHash, testHash)

	req := httptest.NewRequest(http.MethodPut, "/"+testHash+".narinfo", strings.NewReader(niText))
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

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
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
		"tsnixcache_import_success_total",
		"tsnixcache_import_fail_total",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("metric %q not found in /metrics output", name)
		}
	}
}

func TestMetricsAfterHit(t *testing.T) {
	h, _ := newTestServer(t)

	// Trigger a hit.
	req := httptest.NewRequest(http.MethodGet, "/"+testHash+".narinfo", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	// Check metrics.
	req2 := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	body := w2.Body.String()
	if !strings.Contains(body, "tsnixcache_narinfo_hits_total 1") {
		t.Errorf("expected narinfo_hits_total 1 in metrics:\n%s", body)
	}
}

func TestMetricsAfterMiss(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/missing0000000000000000000000000.narinfo", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	req2 := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	body := w2.Body.String()
	if !strings.Contains(body, "tsnixcache_narinfo_misses_total 1") {
		t.Errorf("expected narinfo_misses_total 1 in metrics:\n%s", body)
	}
}

func TestMetricsAfterPushNar(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPut, "/nar/x.nar", strings.NewReader("data"))
	h.ServeHTTP(httptest.NewRecorder(), req)

	req2 := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	body := w2.Body.String()
	if !strings.Contains(body, "tsnixcache_push_nar_total 1") {
		t.Errorf("expected push_nar_total 1 in metrics:\n%s", body)
	}
}

func TestHealthOK(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %q)", w.Code, w.Body.String())
	}
	var resp struct {
		Status         string `json:"status"`
		StoreReachable bool   `json:"store_reachable"`
		DBReadable     bool   `json:"db_readable"`
		UptimeSeconds  int64  `json:"uptime_seconds"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode health JSON: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("expected status=ok, got %q", resp.Status)
	}
	if !resp.StoreReachable {
		t.Error("expected store_reachable=true")
	}
}

func TestVersion(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/version", nil)
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
	if err := os.MkdirAll(storePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storePath, "data"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &fakeStore{
		paths: map[string]*store.PathInfo{
			testHash: {
				StorePath:  storePath,
				NarHash:    "sha256:" + testHash,
				NarSize:    42,
				References: []string{},
			},
		},
	}
	srv := cache.New(s, nil, 30, t.TempDir(), "none", nil)
	h := srv.Handler()

	const n = 50
	var wg sync.WaitGroup
	codes := make([]int, n)

	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			var req *http.Request
			if idx%2 == 0 {
				// hit
				req = httptest.NewRequest(http.MethodGet, "/"+testHash+".narinfo", nil)
			} else {
				// miss
				req = httptest.NewRequest(http.MethodGet, "/missing0000000000000000000000000.narinfo", nil)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			codes[idx] = w.Code
		}(i)
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
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for GET /, got %d", w.Code)
	}
}

// TestNarInfoBodyContentLength verifies HEAD returns a Content-Length.
func TestNarInfoHEADContentLength(t *testing.T) {
	h, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodHead, "/"+testHash+".narinfo", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	cl := w.Header().Get("Content-Length")
	if cl == "" || cl == "0" {
		t.Errorf("expected non-zero Content-Length, got %q", cl)
	}
}

func TestWrongMethodNarInfo(t *testing.T) {
	h, _ := newTestServer(t)
	// POST to a .narinfo path is not an allowed method → 405.
	req := httptest.NewRequest(http.MethodPost, "/"+testHash+".narinfo", nil)
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
	req := httptest.NewRequest(http.MethodPost, "/nix-cache-info", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("POST /nix-cache-info: expected 404, got %d", w.Code)
	}
}

// fakeErrorStore returns an unexpected error (not ErrNotFound) from PathInfo.
type fakeErrorStore struct{}

func (f *fakeErrorStore) PathInfo(_ context.Context, _ string) (*store.PathInfo, error) {
	return nil, fmt.Errorf("simulated db failure")
}

func TestHealthBadStore(t *testing.T) {
	srv := cache.New(&fakeErrorStore{}, nil, 30, t.TempDir(), "none", nil)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d (body: %q)", w.Code, w.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "degraded" {
		t.Errorf("status = %q, want %q", resp.Status, "degraded")
	}
}

// mustReadAll is a helper that reads all bytes from rc and closes it.
func mustReadAll(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	return b
}

var _ = mustReadAll // suppress unused warning
