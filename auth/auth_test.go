package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	apitype "tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

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
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: want 200, got %d", rec.Code)
	}
}

func TestHEADOpen(t *testing.T) {
	mw := Middleware(&fakeWhoIs{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD: want 200, got %d", rec.Code)
	}
}

func TestOPTIONSOpen(t *testing.T) {
	mw := Middleware(&fakeWhoIs{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("OPTIONS: want 200, got %d", rec.Code)
	}
}

func TestPUTWithPushGrant(t *testing.T) {
	fake := &fakeWhoIs{resp: whoIsResp(capMap(Cap{Push: true}))}
	mw := Middleware(fake)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/nar/abc.nar", nil)
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
	req := httptest.NewRequest(http.MethodPut, "/nar/abc.nar", nil)
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT+no cap: want 403, got %d", rec.Code)
	}
}

func TestPUTWithPushFalse(t *testing.T) {
	fake := &fakeWhoIs{resp: whoIsResp(capMap(Cap{Push: false}))}
	mw := Middleware(fake)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/nar/abc.nar", nil)
	mw(okHandler).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT+push=false: want 403, got %d", rec.Code)
	}
}

func TestWhoIsError(t *testing.T) {
	fake := &fakeWhoIs{err: errors.New("network error")}
	mw := Middleware(fake)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/nar/abc.nar", nil)
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
	req := httptest.NewRequest(http.MethodPut, "/nar/abc.nar", nil)
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
				req = httptest.NewRequest(http.MethodGet, "/", nil)
			} else {
				req = httptest.NewRequest(http.MethodPut, "/nar/abc.nar", nil)
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
