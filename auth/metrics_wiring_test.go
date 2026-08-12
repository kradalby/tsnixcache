// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package auth_test

// The auth counter is only worth having if it reaches the registry the server
// actually serves. This test builds a real cache.Server and scrapes its own
// /metrics endpoint — the one an operator points Prometheus at — rather than a
// registry the test wires up itself.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apitype "tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"

	"github.com/kradalby/tsnixcache/auth"
	"github.com/kradalby/tsnixcache/cache"
	"github.com/kradalby/tsnixcache/store"
)

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
	srv := cache.New(cache.Config{
		Store:            emptyStore{},
		Priority:         30,
		SpoolDir:         t.TempDir(),
		ServeCompression: "none",
		StoreDir:         "/nix/store",
	})

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
