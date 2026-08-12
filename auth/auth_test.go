// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	apitype "tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

// Fixed identities and a realistic request path, shared by the tests below.
const (
	testNode = "builder.tail.example.ts.net"
	testUser = "builder@example.com"
	testPath = "/nar/abc.nar"
)

// errNetworkError is a sentinel error for tests.
var errNetworkError = errors.New("network error")

// fakeWhoIs implements WhoIser for tests without a real Tailscale network.
type fakeWhoIs struct {
	resp *apitype.WhoIsResponse
	err  error
}

func (f *fakeWhoIs) WhoIs(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
	return f.resp, f.err
}

// okHandler always responds 200.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
})

// capMap builds a PeerCapMap with a single Cap entry.
func capMap(c Cap) tailcfg.PeerCapMap {
	raw, _ := tailcfg.MarshalCapJSON(c)

	return tailcfg.PeerCapMap{
		CapName: {raw},
	}
}

// whoIsResp builds a minimal WhoIsResponse with the given CapMap.
func whoIsResp(cm tailcfg.PeerCapMap) *apitype.WhoIsResponse {
	return &apitype.WhoIsResponse{CapMap: cm}
}

func TestGETOpen(t *testing.T) {
	// GET with a nil WhoIs response must pass through (no WhoIs call needed).
	mw := Middleware(&fakeWhoIs{resp: nil, err: nil})
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	mw(okHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET: want 200, got %d", rec.Code)
	}
}

func TestHEADOpen(t *testing.T) {
	mw := Middleware(&fakeWhoIs{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodHead, "/", nil)
	mw(okHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD: want 200, got %d", rec.Code)
	}
}

func TestOPTIONSOpen(t *testing.T) {
	mw := Middleware(&fakeWhoIs{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "/", nil)
	mw(okHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("OPTIONS: want 200, got %d", rec.Code)
	}
}

func TestPUTWithPushGrant(t *testing.T) {
	fake := &fakeWhoIs{resp: whoIsResp(capMap(Cap{Push: true}))}
	mw := Middleware(fake)
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, testPath, nil)
	mw(okHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT+push: want 200, got %d", rec.Code)
	}
}

func TestPUTWithoutCap(t *testing.T) {
	// Empty CapMap → no grant at all.
	fake := &fakeWhoIs{resp: whoIsResp(tailcfg.PeerCapMap{})}
	mw := Middleware(fake)
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, testPath, nil)
	mw(okHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT+no cap: want 403, got %d", rec.Code)
	}
}

func TestPUTWithPushFalse(t *testing.T) {
	fake := &fakeWhoIs{resp: whoIsResp(capMap(Cap{Push: false}))}
	mw := Middleware(fake)
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, testPath, nil)
	mw(okHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT+push=false: want 403, got %d", rec.Code)
	}
}

func TestWhoIsError(t *testing.T) {
	fake := &fakeWhoIs{err: errNetworkError}
	mw := Middleware(fake)
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, testPath, nil)
	mw(okHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("WhoIs error: want 401, got %d", rec.Code)
	}
}

func TestMalformedCapJSON(t *testing.T) {
	// Invalid JSON for our cap key → UnmarshalCapJSON errors → no valid rules → deny.
	cm := tailcfg.PeerCapMap{
		CapName: {tailcfg.RawMessage(`not-valid-json`)},
	}
	fake := &fakeWhoIs{resp: whoIsResp(cm)}
	mw := Middleware(fake)
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, testPath, nil)
	mw(okHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("malformed cap: want 403, got %d", rec.Code)
	}
}

func TestConcurrentRequests(t *testing.T) {
	// 20 goroutines: 10 GETs (expect 200) and 10 PUTs with push grant (expect 200).
	fake := &fakeWhoIs{resp: whoIsResp(capMap(Cap{Push: true}))}
	mw := Middleware(fake)
	handler := mw(okHandler)

	var wg sync.WaitGroup

	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			var req *http.Request
			if i%2 == 0 {
				req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			} else {
				req = httptest.NewRequestWithContext(context.Background(), http.MethodPut, testPath, nil)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Errorf("goroutine %d: want 200, got %d", i, rec.Code)
			}
		}(i)
	}

	wg.Wait()
}

// captureLogs redirects the default logger into a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	buf := &bytes.Buffer{}
	prev := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return buf
}

// rejectCount reads tsnixcache_auth_rejects_total for one reason. The counter
// is package-global, so tests compare before/after deltas rather than totals.
func rejectCount(t *testing.T, reason string) float64 {
	t.Helper()

	reg := prometheus.NewRegistry()

	err := reg.Register(Collector())
	if err != nil {
		t.Fatalf("register auth collector: %v", err)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	for _, mf := range mfs {
		if mf.GetName() != metricRejects {
			continue
		}

		for _, m := range mf.GetMetric() {
			labels := m.GetLabel()
			if len(labels) != 1 || labels[0].GetName() != "reason" {
				// Guard the cardinality budget: peer identity and store path
				// must never become labels.
				t.Fatalf("auth rejects metric must be labelled by reason only, got %v", labels)
			}

			if labels[0].GetValue() == reason {
				return m.GetCounter().GetValue()
			}
		}
	}

	return 0
}

// A rejected write must leave a trace an operator can act on: a log line
// naming the peer and the request, and a counter to alert on.
func TestRejectIsObservable(t *testing.T) {
	peer := &apitype.WhoIsResponse{
		Node:        &tailcfg.Node{Name: testNode},
		UserProfile: &tailcfg.UserProfile{LoginName: testUser},
		CapMap:      tailcfg.PeerCapMap{},
	}

	tests := []struct {
		name     string
		fake     *fakeWhoIs
		wantCode int
		reason   string
		wantLog  []string
	}{
		{
			name:     "whois fails",
			fake:     &fakeWhoIs{err: errNetworkError},
			wantCode: http.StatusUnauthorized,
			reason:   reasonUnidentified,
			wantLog:  []string{"peer not identified", testPath, "PUT"},
		},
		{
			name:     "no push grant",
			fake:     &fakeWhoIs{resp: peer},
			wantCode: http.StatusForbidden,
			reason:   reasonNoGrant,
			wantLog: []string{
				"no push grant",
				testNode,
				testUser,
				string(CapName),
				testPath,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureLogs(t)
			before := rejectCount(t, tt.reason)

			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, testPath, nil)
			Middleware(tt.fake)(okHandler).ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("status: want %d, got %d", tt.wantCode, rec.Code)
			}

			got := rejectCount(t, tt.reason) - before
			if got != 1 {
				t.Errorf("tsnixcache_auth_rejects_total{reason=%q}: want +1, got +%v", tt.reason, got)
			}

			for _, want := range tt.wantLog {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log missing %q, got:\n%s", want, logs.String())
				}
			}
		})
	}
}

// An allowed write must neither log a rejection nor move the counter.
func TestAllowedWriteIsNotCounted(t *testing.T) {
	logs := captureLogs(t)
	before := rejectCount(t, reasonNoGrant)

	fake := &fakeWhoIs{resp: whoIsResp(capMap(Cap{Push: true}))}
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, testPath, nil)
	Middleware(fake)(okHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT+push: want 200, got %d", rec.Code)
	}

	if got := rejectCount(t, reasonNoGrant) - before; got != 0 {
		t.Errorf("rejects counter moved on an allowed write: +%v", got)
	}

	if strings.Contains(logs.String(), "write rejected") {
		t.Errorf("allowed write logged a rejection:\n%s", logs.String())
	}
}

// The logged path is attacker-controlled: one rejected request must not be
// able to put an unbounded string in the journal.
func TestRejectLogTruncatesPath(t *testing.T) {
	logs := captureLogs(t)

	long := "/nar/" + strings.Repeat("a", 8192) + ".nar"
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, long, nil)
	Middleware(&fakeWhoIs{err: errNetworkError})(okHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rec.Code)
	}

	line := logs.String()
	if strings.Contains(line, strings.Repeat("a", maxLogPath+1)) {
		t.Errorf("rejection log carries the full %d-byte path (%d bytes logged)", len(long), len(line))
	}

	if !strings.Contains(line, "truncated") {
		t.Errorf("rejection log does not mark the path as truncated:\n%s", line)
	}
}

// localapi decodes a JSON null body into (nil, nil), so a write must survive a
// WhoIs that returns neither a peer nor an error.
func TestPUTWithNilWhoIsResponse(t *testing.T) {
	before := rejectCount(t, reasonUnidentified)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, testPath, nil)
	Middleware(&fakeWhoIs{resp: nil, err: nil})(okHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("PUT+nil WhoIs: want 401, got %d", rec.Code)
	}

	if got := rejectCount(t, reasonUnidentified) - before; got != 1 {
		t.Errorf("tsnixcache_auth_rejects_total{reason=%q}: want +1, got +%v", reasonUnidentified, got)
	}
}

// A rejection must stop the request: writing 403 and calling the next handler
// anyway would still let the push through.
func TestRejectedRequestDoesNotReachNextHandler(t *testing.T) {
	peer := &apitype.WhoIsResponse{
		Node:        &tailcfg.Node{Name: testNode},
		UserProfile: &tailcfg.UserProfile{LoginName: testUser},
		CapMap:      tailcfg.PeerCapMap{},
	}

	tests := []struct {
		name     string
		fake     *fakeWhoIs
		wantCode int
	}{
		{"whois fails", &fakeWhoIs{err: errNetworkError}, http.StatusUnauthorized},
		{"nil whois response", &fakeWhoIs{}, http.StatusUnauthorized},
		{"identified peer, no grant", &fakeWhoIs{resp: peer}, http.StatusForbidden},
		{"push false", &fakeWhoIs{resp: whoIsResp(capMap(Cap{Push: false}))}, http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				called = true
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, testPath, nil)
			Middleware(tt.fake)(next).ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("status: want %d, got %d", tt.wantCode, rec.Code)
			}

			if called {
				t.Error("rejected write reached the next handler")
			}
		})
	}
}

// The shape that matters in production is a real tailnet peer that simply lacks
// the grant: every read stays open to it, every write is refused.
func TestIdentifiedPeerWithoutGrant(t *testing.T) {
	fake := &fakeWhoIs{resp: &apitype.WhoIsResponse{
		Node:        &tailcfg.Node{Name: testNode},
		UserProfile: &tailcfg.UserProfile{LoginName: testUser},
		CapMap:      tailcfg.PeerCapMap{},
	}}
	handler := Middleware(fake)(okHandler)

	tests := []struct {
		method string
		want   int
	}{
		{http.MethodGet, http.StatusOK},
		{http.MethodHead, http.StatusOK},
		{http.MethodOptions, http.StatusOK},
		{http.MethodPut, http.StatusForbidden},
		{http.MethodPost, http.StatusForbidden},
		{http.MethodPatch, http.StatusForbidden},
		{http.MethodDelete, http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), tt.method, testPath, nil)
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("%s: want %d, got %d", tt.method, tt.want, rec.Code)
			}
		})
	}
}

// Every reason must be exported before it is ever incremented, so a clean
// server reports zeros rather than nothing at all.
func TestRejectCounterIsZeroInitialised(t *testing.T) {
	reg := prometheus.NewRegistry()

	err := reg.Register(Collector())
	if err != nil {
		t.Fatalf("register auth collector: %v", err)
	}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	seen := map[string]bool{}

	for _, mf := range mfs {
		if mf.GetName() != metricRejects {
			continue
		}

		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				seen[l.GetValue()] = true
			}
		}
	}

	for _, reason := range []string{reasonUnidentified, reasonNoGrant, reasonLocalWrite} {
		if !seen[reason] {
			t.Errorf("tsnixcache_auth_rejects_total{reason=%q} is absent from a scrape", reason)
		}
	}
}

// A listener with no identity serves reads and refuses writes. The reached
// flag is the assertion that matters: a gate that writes 403 and calls next
// anyway has still imported the push.
func TestReadOnlyRefusesWrites(t *testing.T) {
	tests := []struct {
		method  string
		want    int
		reached bool
	}{
		{http.MethodGet, http.StatusOK, true},
		{http.MethodHead, http.StatusOK, true},
		{http.MethodOptions, http.StatusOK, true},
		{http.MethodPut, http.StatusForbidden, false},
		{http.MethodPost, http.StatusForbidden, false},
		{http.MethodPatch, http.StatusForbidden, false},
		{http.MethodDelete, http.StatusForbidden, false},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			reached := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true

				w.WriteHeader(http.StatusOK)
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), tt.method, testPath, nil)
			ReadOnly(next).ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("%s: want %d, got %d", tt.method, tt.want, rec.Code)
			}

			if reached != tt.reached {
				t.Errorf("%s reached the handler behind the gate = %v, want %v", tt.method, reached, tt.reached)
			}
		})
	}
}

// The operator meeting this 403 has only the response body to go on, so it has
// to name the flag that lifts it.
func TestReadOnlyRefusalNamesTheFlag(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, testPath, nil)
	ReadOnly(okHandler).ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), LocalWriteFlag) {
		t.Errorf("403 body %q does not name %s", rec.Body.String(), LocalWriteFlag)
	}
}

// The refusal must be as visible as the push-grant ones: a counted series to
// alert on and a log line naming the request.
func TestReadOnlyRejectIsObservable(t *testing.T) {
	logs := captureLogs(t)
	before := rejectCount(t, reasonLocalWrite)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, testPath, nil)
	ReadOnly(okHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: want 403, got %d", rec.Code)
	}

	if got := rejectCount(t, reasonLocalWrite) - before; got != 1 {
		t.Errorf("tsnixcache_auth_rejects_total{reason=%q}: want +1, got +%v", reasonLocalWrite, got)
	}

	for _, want := range []string{"no authentication", testPath, LocalWriteFlag} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log missing %q, got:\n%s", want, logs.String())
		}
	}
}

// A read on such a listener is the substituter case, and must leave the
// rejection counter alone.
func TestReadOnlyReadIsNotCounted(t *testing.T) {
	before := rejectCount(t, reasonLocalWrite)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, testPath, nil)
	ReadOnly(okHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET: want 200, got %d", rec.Code)
	}

	if got := rejectCount(t, reasonLocalWrite) - before; got != 0 {
		t.Errorf("rejects counter moved on a read: +%v", got)
	}
}

func TestLogPathTruncates(t *testing.T) {
	short := testPath
	if got := LogPath(short); got != short {
		t.Errorf("LogPath(%q) = %q, want unchanged", short, got)
	}

	long := "/nar/" + strings.Repeat("a", 8192) + ".nar"

	got := LogPath(long)
	if len(got) > maxLogPath+len("…truncated") {
		t.Errorf("LogPath returned %d bytes, want at most %d", len(got), maxLogPath+len("…truncated"))
	}

	if !strings.HasSuffix(got, "truncated") {
		t.Errorf("LogPath(long) = %q, want a truncation marker", got)
	}
}

func TestHasPushCapTrue(t *testing.T) {
	cm := capMap(Cap{Push: true})
	if !HasPushCap(cm) {
		t.Fatal("HasPushCap: want true, got false")
	}
}

func TestHasPushCapFalse(t *testing.T) {
	if HasPushCap(tailcfg.PeerCapMap{}) {
		t.Fatal("HasPushCap empty map: want false, got true")
	}
}

// TestReadOnlyRefusesForceGCByGet pins the one endpoint a method rule cannot
// see. tsweb links force-GC as a plain anchor, so the button sends GET and its
// handler ignores the method — without a path rule a "read-only" listener would
// still let anything that reaches it force a stop-the-world collection.
func TestReadOnlyRefusesForceGCByGet(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    int
		reached bool
	}{
		{"force gc", "/debug/gc", http.StatusForbidden, false},
		{"force gc uncleaned", "/debug/./gc", http.StatusForbidden, false},
		{"pprof stays readable", "/debug/pprof/goroutine", http.StatusOK, true},
		{"debug index stays readable", "/debug/", http.StatusOK, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true

				w.WriteHeader(http.StatusOK)
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, tt.path, nil)
			ReadOnly(next).ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("GET %s: want %d, got %d", tt.path, tt.want, rec.Code)
			}

			if reached != tt.reached {
				t.Errorf("GET %s: handler reached = %v, want %v", tt.path, reached, tt.reached)
			}
		})
	}
}
