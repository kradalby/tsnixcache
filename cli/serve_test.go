// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
	apitype "tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"

	// modernc.org/sqlite registers the "sqlite" driver via its init function.
	_ "modernc.org/sqlite"

	"github.com/peterbourgon/ff/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kradalby/tsnixcache/auth"
	"github.com/kradalby/tsnixcache/cache"
)

const (
	testPrefix   = "/srv/cache"
	flagStore    = "--store"
	flagStoreDir = "--store-dir"
	flagListen   = "--listen"
	flagGCRule   = "--gc-rule"
	flagDB       = "--db"
	flagSpoolDir = "--spool-dir"
	// flagAllowCodecs keeps a test's own missing-codec expectations out of the
	// startup check, which every serve invocation now goes through.
	flagAllowCodecs = "--allow-missing-codecs"
)

// errWhoIsFailed stands in for a localapi hiccup.
var errWhoIsFailed = errors.New("whois failed")

// newStoreFlagSet mirrors the path flags that applyStorePrefix interacts with.
func newStoreFlagSet() *ff.FlagSet {
	fs := ff.NewFlagSet("serve")
	fs.StringLong("db", "/nix/var/nix/db/db.sqlite", "")
	fs.StringLong("store-dir", "/nix/store", "")
	fs.StringLong("nix-store-uri", "auto", "")
	fs.StringLong("gcroot-dir", "/nix/var/nix/gcroots/tsnixcache", "")
	fs.StringLong("store", "", "")

	return fs
}

func TestApplyStorePrefixDerivesPaths(t *testing.T) {
	fs := newStoreFlagSet()

	err := fs.Parse([]string{flagStore, testPrefix})
	if err != nil {
		t.Fatal(err)
	}

	var cfg serveConfig

	err = applyStorePrefix(fs, testPrefix, &cfg)
	if err != nil {
		t.Fatalf("applyStorePrefix: %v", err)
	}

	for _, c := range []struct{ name, got, want string }{
		{"storePrefix", cfg.storePrefix, testPrefix},
		{"storeRoot", cfg.storeRoot, testPrefix},
		{"dbPath", cfg.dbPath, "/srv/cache/nix/var/nix/db/db.sqlite"},
		{"nixStoreURI", cfg.nixStoreURI, testPrefix},
		{"gcRootDir", cfg.gcRootDir, "/srv/cache/nix/var/nix/gcroots/tsnixcache"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func TestApplyStorePrefixEmptyIsNoop(t *testing.T) {
	fs := newStoreFlagSet()
	cfg := serveConfig{storeDir: "/nix/store", dbPath: "/db"}

	err := applyStorePrefix(fs, "", &cfg)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.storeDir != "/nix/store" || cfg.dbPath != "/db" || cfg.storePrefix != "" {
		t.Errorf("empty prefix mutated cfg: %+v", cfg)
	}
}

func TestApplyStorePrefixRejectsConflicts(t *testing.T) {
	for _, conflict := range []string{"db", "store-dir", "nix-store-uri", "gcroot-dir"} {
		t.Run(conflict, func(t *testing.T) {
			fs := newStoreFlagSet()

			err := fs.Parse([]string{flagStore, testPrefix, "--" + conflict, "/x"})
			if err != nil {
				t.Fatal(err)
			}

			var cfg serveConfig

			err = applyStorePrefix(fs, testPrefix, &cfg)
			if err == nil {
				t.Fatalf("expected error combining --store with --%s", conflict)
			}
		})
	}
}

func TestLocalListenAddrs(t *testing.T) {
	// The flags the NixOS module renders for `listen = [ ]`: it maps over the
	// list, so an empty one emits no --listen at all.
	var moduleEmptyListen []string

	tsnet := []string{"hostname=cache,dir=/var/lib/ts"}

	tests := []struct {
		name  string
		flags []string
		tsnet []string
		want  []string
	}{
		{"unset falls back to loopback", nil, nil, []string{defaultListenAddr}},
		{"explicitly empty means no local listener", []string{""}, nil, []string{}},
		{"whitespace is empty too", []string{"  "}, nil, []string{}},
		{
			name:  "addresses are kept in order",
			flags: []string{defaultListenAddr, "[::1]:5000"},
			want:  []string{defaultListenAddr, "[::1]:5000"},
		},
		{"empty entries are dropped", []string{"", defaultListenAddr}, nil, []string{defaultListenAddr}},
		// The security-relevant case: an operator who sets `listen = [ ]` to
		// turn the unauthenticated listener off must not get one anyway.
		{"tsnet only, module's empty list", moduleEmptyListen, tsnet, nil},
		{"tsnet only, explicitly empty", []string{""}, tsnet, nil},
		{"listen still binds alongside tsnet", []string{defaultListenAddr}, tsnet, []string{defaultListenAddr}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := localListenAddrs(tt.flags, tt.tsnet)
			if !slices.Equal(got, tt.want) {
				t.Errorf("localListenAddrs(%q, %q) = %q, want %q", tt.flags, tt.tsnet, got, tt.want)
			}
		})
	}
}

// TestSweepOnceRemovesAbandonedSpoolFiles covers the sweeper's wiring: the loop
// only ticks every ten minutes, so without this the SweepSpool call could be
// deleted and no test in this package would notice.
func TestSweepOnceRemovesAbandonedSpoolFiles(t *testing.T) {
	spoolDir := t.TempDir()

	// The fixtures are written after New, because New sweeps the spool
	// unconditionally: anything that outlived the previous process is garbage.
	// Seeding first would test that startup sweep, not the ticker's.
	srv, openErr := cache.New(cache.Config{
		Priority:         30,
		SpoolDir:         spoolDir,
		ServeCompression: "none",
		StoreDir:         defaultStoreDir,
	})

	require.NoError(t, openErr)
	t.Cleanup(func() { require.NoError(t, srv.Close()) })

	abandoned := filepath.Join(spoolDir, "abandoned.nar")
	inFlight := filepath.Join(spoolDir, "in-flight.nar")

	for _, path := range []string{abandoned, inFlight} {
		err := os.WriteFile(path, []byte("nar body"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	// A PUT that stopped writing two days ago; its narinfo never arrived.
	stale := time.Now().Add(-2 * spoolMaxIdle)

	err := os.Chtimes(abandoned, stale, stale)
	if err != nil {
		t.Fatal(err)
	}

	sweepOnce(srv)

	_, err = os.Stat(abandoned)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("abandoned spool file survived the sweep: stat = %v", err)
	}

	_, err = os.Stat(inFlight)
	if err != nil {
		t.Errorf("sweep removed an upload still in flight: %v", err)
	}
}

// freeAddr returns a loopback address that was bound and released, so it is
// almost certainly free and definitely well-formed.
func freeAddr(t *testing.T) string {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	addr := ln.Addr().String()

	err = ln.Close()
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	return addr
}

func TestListenAllFailsOnUnbindableAddr(t *testing.T) {
	var lc net.ListenConfig

	taken, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = taken.Close() })

	free := freeAddr(t)

	listeners, err := listenAll(t.Context(), []string{free, taken.Addr().String()})
	if err == nil {
		for _, ln := range listeners {
			_ = ln.Close()
		}

		t.Fatal("listenAll succeeded on an address already in use")
	}

	// The listener bound before the failure must be released, not leaked.
	again, err := lc.Listen(t.Context(), "tcp", free)
	if err != nil {
		t.Fatalf("listenAll leaked %s: %v", free, err)
	}

	_ = again.Close()
}

// TestServeHTTPShutsDownOnCancel is the SIGTERM path: cancelling the context
// must stop the servers and let errgroup.Wait return, rather than leaving
// Serve blocked forever while the supervisor escalates to SIGKILL.
func TestServeHTTPShutsDownOnCancel(t *testing.T) {
	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	eg, egCtx := errgroup.WithContext(ctx)

	serveHTTP(egCtx, eg, ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	// Stands in for the GC watcher and spool sweeper: exits only on ctx.Done.
	eg.Go(func() error {
		<-egCtx.Done()

		return nil
	})

	resp, err := http.Get("http://" + ln.Addr().String() + nixCacheInfoPath) //nolint:noctx
	if err != nil {
		t.Fatalf("request before shutdown: %v", err)
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	cancel()

	done := make(chan error, 1)

	startTestTask(t, func() { done <- eg.Wait() })

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("eg.Wait after cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("eg.Wait did not return within 10s of cancellation")
	}
}

// TestServeHTTPFinishesInFlightRequest pins the graceful half of shutdown: a
// request already being served — an import, in production — must complete, not
// have its connection dropped the moment SIGTERM lands.
func TestServeHTTPFinishesInFlightRequest(t *testing.T) {
	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	eg, egCtx := errgroup.WithContext(ctx)

	started := make(chan struct{})
	release := make(chan struct{})

	serveHTTP(egCtx, eg, ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)

		// Stands in for nix-store --serve: still running when SIGTERM
		// arrives, and released only once shutdown has begun.
		<-release

		_, _ = io.WriteString(w, "imported")
	}))

	type result struct {
		body string
		err  error
	}

	got := make(chan result, 1)

	startTestTask(t, func() {
		resp, reqErr := http.Get("http://" + ln.Addr().String() + "/x") //nolint:noctx
		if reqErr != nil {
			got <- result{err: reqErr}

			return
		}

		defer func() { _ = resp.Body.Close() }()

		body, readErr := io.ReadAll(resp.Body)
		got <- result{body: string(body), err: readErr}
	})

	<-started
	cancel()

	// Shutdown closes the listener before it waits, so a refused connection is
	// the observable "shutdown has started". Releasing the handler only then is
	// what makes the request genuinely in-flight across the shutdown rather
	// than racing it.
	waitUntil(t, "shutdown to close the listener", func() bool {
		var d net.Dialer

		conn, dialErr := d.DialContext(t.Context(), "tcp", ln.Addr().String())
		if dialErr != nil {
			return true
		}

		_ = conn.Close()

		return false
	})

	close(release)

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("in-flight request was cut off by shutdown: %v", r.err)
		}

		if r.body != "imported" {
			t.Fatalf("in-flight response body = %q, want %q", r.body, "imported")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight request did not finish within 10s")
	}

	err = eg.Wait()
	if err != nil {
		t.Fatalf("eg.Wait after cancel: %v", err)
	}
}

func TestParseGCInterval(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"minutes", "5m", 5 * time.Minute, false},
		{"days are accepted like the NixOS module allows", "1d", 24 * time.Hour, false},
		{"zero is rejected", "0", 0, true},
		{"negative is rejected", "-5m", 0, true},
		{"empty is rejected", "", 0, true},
		{"garbage is rejected", "soon", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseGCInterval(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseGCInterval(%q) = %v, want error", tt.in, got)
				}

				if !errors.Is(err, errBadGCInterval) {
					t.Fatalf("parseGCInterval(%q) error = %v, want errBadGCInterval", tt.in, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseGCInterval(%q): %v", tt.in, err)
			}

			if got != tt.want {
				t.Errorf("parseGCInterval(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestServeRejectsBadGCIntervalBeforeStarting pins the ordering: the flag is
// rejected before the store is opened or anything is bound, so the server never
// announces itself and then dies.
func TestServeRejectsBadGCIntervalBeforeStarting(t *testing.T) {
	missingDB := filepath.Join(t.TempDir(), "db.sqlite")

	err := newServeCmd().ParseAndRun(t.Context(), []string{
		"--gc-interval", "0",
		flagDB, missingDB,
		flagListen, "",
	})
	if !errors.Is(err, errBadGCInterval) {
		t.Fatalf("serve --gc-interval 0 = %v, want errBadGCInterval", err)
	}
}

// TestServeRefusesGCOnAnotherStore pins both halves of the guard. gc prunes the
// gcroots of --store-dir but nix-collect-garbage always collects /nix/store, so
// a non-default store would leave the threshold measured on one filesystem while
// another one is collected.
func TestServeRefusesGCOnAnotherStore(t *testing.T) {
	tests := []struct {
		name  string
		flag  string
		value string
		want  error
	}{
		{"chroot store", flagStore, testPrefix, errStoreGCConflict},
		{"another store dir", flagStoreDir, "/srv/store", errGCNonDefaultStoreDir},
		{"trailing slash is still the default store", flagStoreDir, defaultStoreDir + "/", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A missing store DB no longer stops serve — it serves degraded and
			// retries — so a config the guard allows has to be stopped by
			// something else: --listen "" leaves it with no listener at all.
			// --allow-missing-codecs so the guard under test is what decides
			// the outcome: the codec check runs first and would otherwise
			// stop serve on any host without xz.
			args := []string{
				flagGCRule, "80:1d", tt.flag, tt.value,
				flagSpoolDir, filepath.Join(t.TempDir(), "spool"),
				flagAllowCodecs,
				flagListen, "",
			}

			// --store derives the DB path itself and refuses an explicit --db.
			if tt.flag != flagStore {
				args = append(args, flagDB, filepath.Join(t.TempDir(), "db.sqlite"))
			}

			err := newServeCmd().ParseAndRun(t.Context(), args)
			if tt.want == nil {
				// It must get as far as opening the missing DB rather than
				// being refused by the guard.
				if errors.Is(err, errGCNonDefaultStoreDir) || errors.Is(err, errStoreGCConflict) {
					t.Fatalf("serve %q = %v, want the GC guard to allow it", args, err)
				}

				return
			}

			if !errors.Is(err, tt.want) {
				t.Fatalf("serve %q = %v, want %v", args, err, tt.want)
			}
		})
	}
}

// nixSchema is the part of Nix's schema that store.Open prepares against.
var nixSchema = []string{
	`CREATE TABLE ValidPaths (
		id integer primary key autoincrement not null,
		path text unique not null,
		hash text not null,
		registrationTime integer not null default 0,
		deriver text,
		narSize integer,
		ultimate integer,
		sigs text,
		ca text
	)`,
	`CREATE TABLE Refs (
		referrer integer not null,
		reference integer not null,
		primary key (referrer, reference)
	)`,
}

// newFixtureDB writes an empty Nix-shaped database so runServe can open a store.
func newFixtureDB(t *testing.T) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "db.sqlite")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}

	defer func() { _ = db.Close() }()

	for _, stmt := range nixSchema {
		_, err = db.ExecContext(t.Context(), stmt)
		if err != nil {
			t.Fatalf("create fixture schema: %v", err)
		}
	}

	return dbPath
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	require.EventuallyWithT(t, func(c *assert.CollectT) { assert.True(c, cond(), what) }, 10*time.Second, 10*time.Millisecond)
}

// waitForServing polls addr until the cache answers, or runServe gives up.
func waitForServing(t *testing.T, addr string, done <-chan error) {
	t.Helper()

	waitUntil(t, "the cache to answer on "+addr, func() bool {
		select {
		case err := <-done:
			t.Fatalf("runServe returned before it served anything: %v", err)
		default:
		}

		// /nix-cache-info touches no store, so it answers while the store is
		// still opening — which is the point of the degraded state.
		resp, err := http.Get("http://" + addr + nixCacheInfoPath) //nolint:noctx
		if err != nil {
			return false
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		return resp.StatusCode == http.StatusOK
	})
}

// TestRunServeShutsDownOnContextCancel drives runServe itself rather than the
// serveHTTP helper. Without it the whole shutdown path could be unwired at the
// call site — the process would then ignore SIGTERM and die only at systemd's
// SIGKILL — while every helper-level test kept passing.
func TestRunServeShutsDownOnContextCancel(t *testing.T) {
	spoolDir := t.TempDir()
	addr := freeAddr(t)

	cfg := serveConfig{
		dbPath:           newFixtureDB(t),
		storeDir:         defaultStoreDir,
		priority:         30,
		spoolDir:         filepath.Join(spoolDir, "spool"),
		gcRootDir:        filepath.Join(spoolDir, "gcroots"),
		serveCompression: "none",
		listenAddrs:      []string{addr},
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	startTestTask(t, func() { done <- runServe(ctx, cfg) })

	waitForServing(t, addr, done)

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServe after cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runServe did not return within 10s of cancellation: SIGTERM would be ignored")
	}
}

// TestRunServeStaysUpUntilTheStoreOpens covers the boot race with nix-daemon:
// Nix's DB cannot be read until something creates the wal-index, so exiting on
// the first failed open turns a self-healing condition into a restart loop. The
// server must serve degraded and pick the store up when it appears.
func TestRunServeStaysUpUntilTheStoreOpens(t *testing.T) {
	work := t.TempDir()
	dbPath := filepath.Join(work, "db.sqlite")
	addr := freeAddr(t)

	cfg := serveConfig{
		dbPath:           dbPath,
		storeDir:         defaultStoreDir,
		priority:         30,
		spoolDir:         filepath.Join(work, "spool"),
		gcRootDir:        filepath.Join(work, "gcroots"),
		serveCompression: compressionNone,
		listenAddrs:      []string{addr},
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	startTestTask(t, func() { done <- runServe(ctx, cfg) })

	waitForServing(t, addr, done)

	if reachable := storeReachable(t, addr); reachable {
		t.Fatal("/health reports the store reachable with no database at all")
	}

	// The database turning up is nix-daemon opening it: rename so the server
	// never sees a half-written file.
	err := os.Rename(newFixtureDB(t), dbPath)
	if err != nil {
		t.Fatalf("rename fixture db into place: %v", err)
	}

	waitUntil(t, "the retried store open to succeed", func() bool { return storeReachable(t, addr) })

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServe after cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runServe did not return within 10s of cancellation")
	}
}

// storeReachable reports what GET /health says about the store.
func storeReachable(t *testing.T, addr string) bool {
	t.Helper()

	resp, err := http.Get("http://" + addr + "/health") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	var health struct {
		Status         string `json:"status"`
		StoreReachable bool   `json:"store_reachable"`
	}

	err = json.NewDecoder(resp.Body).Decode(&health)
	if err != nil {
		t.Fatalf("decode /health: %v", err)
	}

	if health.StoreReachable != (health.Status == "ok") {
		t.Fatalf("/health disagrees with itself: %+v", health)
	}

	return health.StoreReachable
}

// TestRunServeReleasesListenersWhenTsnetFails pins the partial-failure path: a
// tsnet spec that will not parse must take the already-bound local listeners
// down with it, not leave them serving with nobody left to stop them.
func TestRunServeReleasesListenersWhenTsnetFails(t *testing.T) {
	work := t.TempDir()
	addr := freeAddr(t)

	cfg := serveConfig{
		dbPath:           newFixtureDB(t),
		storeDir:         defaultStoreDir,
		priority:         30,
		spoolDir:         filepath.Join(work, "spool"),
		gcRootDir:        filepath.Join(work, "gcroots"),
		serveCompression: compressionNone,
		listenAddrs:      []string{addr},
		tsnetSpecs:       []string{"hostnmae=typo"},
	}

	err := runServe(t.Context(), cfg)
	if !errors.Is(err, errTsnetUnknownKey) {
		t.Fatalf("runServe = %v, want the tsnet parse error", err)
	}

	var lc net.ListenConfig

	again, err := lc.Listen(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("runServe leaked the local listener on %s: %v", addr, err)
	}

	_ = again.Close()
}

// testHashPart is a syntactically valid store-path hash part: 32 nix-base32
// characters, which is all the store checks before it queries.
const testHashPart = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// newSeededFixtureDB writes a fixture database holding one valid path, so a
// substituter's narinfo GET has something to find. With --serve-compression
// none that answer comes from the database alone, so no NAR need exist on disk.
func newSeededFixtureDB(t *testing.T) string {
	t.Helper()

	dbPath := newFixtureDB(t)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}

	defer func() { _ = db.Close() }()

	_, err = db.ExecContext(t.Context(),
		`INSERT INTO ValidPaths (path, hash, narSize) VALUES (?, ?, ?)`,
		defaultStoreDir+"/"+testHashPart+"-hello", "sha256:"+strings.Repeat("ab", 32), 4096)
	if err != nil {
		t.Fatalf("seed ValidPaths: %v", err)
	}

	return dbPath
}

// startLocalServe runs a cache on a free loopback address with only a --listen
// listener, and returns the address and its spool dir.
func startLocalServe(t *testing.T, dbPath string, localWrite bool) (string, string) {
	t.Helper()

	work := t.TempDir()
	spoolDir := filepath.Join(work, "spool")
	addr := freeAddr(t)

	cfg := serveConfig{
		dbPath:           dbPath,
		storeDir:         defaultStoreDir,
		priority:         30,
		spoolDir:         spoolDir,
		gcRootDir:        filepath.Join(work, "gcroots"),
		serveCompression: compressionNone,
		listenAddrs:      []string{addr},
		localWrite:       localWrite,
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	startTestTask(t, func() { done <- runServe(ctx, cfg) })

	t.Cleanup(func() {
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runServe: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("runServe did not return within 10s of cancellation")
		}
	})

	waitForServing(t, addr, done)

	return addr, spoolDir
}

// request performs one request against a running server and returns its status
// and body.
func request(t *testing.T, method, url, body string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s body: %v", method, url, err)
	}

	return resp.StatusCode, string(got)
}

// spooledNARs lists the NAR files a PUT /nar/ would have left behind. The zstd
// cache lives in a subdirectory, so directories are skipped.
func spooledNARs(t *testing.T, spoolDir string) []string {
	t.Helper()

	ents, err := os.ReadDir(spoolDir)
	if err != nil {
		t.Fatalf("read spool dir: %v", err)
	}

	var names []string

	for _, ent := range ents {
		if !ent.IsDir() {
			names = append(names, ent.Name())
		}
	}

	return names
}

// TestLocalListenerIsReadOnly is the hole this gate closes, over a real socket.
// A --listen address carries no identity, and the service is a nix trusted-user
// that re-signs what it imports with the cache key, so an unattributable push
// on it can shadow the upstream cache for every client that trusts that key.
func TestLocalListenerIsReadOnly(t *testing.T) {
	addr, spoolDir := startLocalServe(t, newSeededFixtureDB(t), false)

	code, body := request(t, http.MethodPut, "http://"+addr+"/nar/poison.nar", "would-be NAR")
	if code != http.StatusForbidden {
		t.Errorf("PUT /nar/poison.nar = %d, want 403", code)
	}

	// The operator only sees this body, so it has to name the way back in.
	if !strings.Contains(body, "--local-write") {
		t.Errorf("403 body %q does not name --local-write", body)
	}

	// The status alone proves nothing: a gate that answers 403 and forwards the
	// request anyway has still spooled the body it was refusing.
	if got := spooledNARs(t, spoolDir); len(got) != 0 {
		t.Errorf("refused PUT reached the cache handler: spooled %q", got)
	}

	// Reads are what a loopback listener exists for and must be untouched,
	// including the store paths a substituter fetches.
	for _, path := range []string{nixCacheInfoPath, "/" + testHashPart + ".narinfo"} {
		code, body := request(t, http.MethodGet, "http://"+addr+path, "")
		if code != http.StatusOK {
			t.Errorf("GET %s = %d (%q), want 200", path, code, body)
		}
	}
}

// The opt-out restores the old behaviour verbatim: --local-write is what an
// operator pushing to a loopback cache from a post-build hook needs.
func TestLocalWriteAllowsPushes(t *testing.T) {
	addr, spoolDir := startLocalServe(t, newSeededFixtureDB(t), true)

	code, body := request(t, http.MethodPut, "http://"+addr+"/nar/push.nar", "a NAR")
	if code != http.StatusOK {
		t.Fatalf("PUT /nar/push.nar with --local-write = %d (%q), want 200", code, body)
	}

	if got := spooledNARs(t, spoolDir); !slices.Contains(got, "push.nar") {
		t.Errorf("spool holds %q, want the pushed NAR", got)
	}
}

func TestEnsureSpoolDir(t *testing.T) {
	t.Run("creates a fresh dir", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "spool")

		err := ensureSpoolDir(dir)
		if err != nil {
			t.Fatalf("ensureSpoolDir: %v", err)
		}

		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}

		if fi.Mode().Perm()&0o022 != 0 {
			t.Errorf("spool dir created writable by others: mode %#o", fi.Mode().Perm())
		}
	})

	t.Run("accepts a dir we already own", func(t *testing.T) {
		dir := t.TempDir()

		err := ensureSpoolDir(dir)
		if err != nil {
			t.Fatalf("ensureSpoolDir on an existing safe dir: %v", err)
		}
	})

	// The privilege escalation: a local user pre-creates the spool as a symlink
	// to a directory the server can write but they cannot, and every pushed NAR
	// lands there.
	t.Run("refuses a symlink", func(t *testing.T) {
		work := t.TempDir()
		target := filepath.Join(work, "target")

		err := os.Mkdir(target, 0o750)
		if err != nil {
			t.Fatal(err)
		}

		link := filepath.Join(work, "spool")

		err = os.Symlink(target, link)
		if err != nil {
			t.Fatal(err)
		}

		err = ensureSpoolDir(link)
		if !errors.Is(err, errUnsafeSpoolDir) {
			t.Fatalf("ensureSpoolDir(symlink) = %v, want errUnsafeSpoolDir", err)
		}
	})

	// The other half: a world-writable spool lets them plant symlinks inside it
	// under the names the server is about to create.
	t.Run("refuses a world-writable dir", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "spool")

		// 0777 is the attack, not an oversight: Mkdir goes through the umask,
		// so Chmod is what actually gets it there.
		err := os.Mkdir(dir, 0o777) // #nosec G301 -- the mode under test
		if err != nil {
			t.Fatal(err)
		}

		err = os.Chmod(dir, 0o777) // #nosec G302 -- the mode under test
		if err != nil {
			t.Fatal(err)
		}

		err = ensureSpoolDir(dir)
		if !errors.Is(err, errUnsafeSpoolDir) {
			t.Fatalf("ensureSpoolDir(0777) = %v, want errUnsafeSpoolDir", err)
		}
	})

	t.Run("refuses a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "spool")

		err := os.WriteFile(path, nil, 0o600)
		if err != nil {
			t.Fatal(err)
		}

		err = ensureSpoolDir(path)
		if err == nil {
			t.Fatal("ensureSpoolDir accepted a regular file")
		}
	})
}

// TestDefaultSpoolDirIsNotWorldWritable pins the default itself: /tmp is
// world-writable, so any local user can win the race to create the path.
func TestDefaultSpoolDirIsNotWorldWritable(t *testing.T) {
	if strings.HasPrefix(defaultSpoolDir, os.TempDir()) || strings.HasPrefix(defaultSpoolDir, "/tmp/") {
		t.Errorf("defaultSpoolDir = %q: a world-writable parent lets any local uid prepare it", defaultSpoolDir)
	}
}

func TestServeRejectsUnsupportedCompression(t *testing.T) {
	for _, tt := range []struct {
		value   string
		wantErr bool
	}{
		{compressionNone, false},
		{compressionZstd, false},
		{"gzip", true},
		{"", true},
	} {
		t.Run(tt.value, func(t *testing.T) {
			// See TestServeRefusesGCOnAnotherStore: the codec check must not be
			// what a host without xz fails this on.
			args := []string{
				"--serve-compression", tt.value,
				flagDB, filepath.Join(t.TempDir(), "db.sqlite"),
				flagSpoolDir, filepath.Join(t.TempDir(), "spool"),
				flagAllowCodecs,
				flagListen, "",
			}

			err := newServeCmd().ParseAndRun(t.Context(), args)
			if tt.wantErr {
				if !errors.Is(err, errBadServeCompression) {
					t.Fatalf("serve --serve-compression %q = %v, want errBadServeCompression", tt.value, err)
				}

				return
			}

			// A supported value must get past validation; with no listener at
			// all it stops at errNoListeners.
			if errors.Is(err, errBadServeCompression) {
				t.Fatalf("serve --serve-compression %q was rejected", tt.value)
			}
		})
	}
}

// fakeWhoIs answers WhoIs without a tailnet.
type fakeWhoIs struct {
	resp *apitype.WhoIsResponse
	err  error
}

func (f *fakeWhoIs) WhoIs(context.Context, string) (*apitype.WhoIsResponse, error) {
	return f.resp, f.err
}

func pushCapMap(push bool) tailcfg.PeerCapMap {
	return tailcfg.PeerCapMap{auth.CapName: []tailcfg.RawMessage{
		tailcfg.RawMessage(`{"push":` + strconv.FormatBool(push) + `}`),
	}}
}

// TestGateDebugRequiresPushCap pins the tsnet debug gate. tsweb admits every
// tailnet address and auth.Middleware waves safe methods through, so without
// this a peer explicitly denied push could still read argv and goroutine stacks
// and force a GC per request.

// Paths the gate tests share: a debug endpoint both the gate's own test and
// the tsnet-chain test reach for, the read every substituter starts with, and
// a NAR write.
const (
	debugPprofPath   = "/debug/pprof/"
	nixCacheInfoPath = "/nix-cache-info"
	narPutPath       = "/nar/x.nar"
)

func TestGateDebugRequiresPushCap(t *testing.T) {
	tests := []struct {
		name   string
		who    *fakeWhoIs
		path   string
		want   int
		served bool
	}{
		{
			name:   "pusher reaches debug",
			who:    &fakeWhoIs{resp: &apitype.WhoIsResponse{CapMap: pushCapMap(true)}},
			path:   debugPprofPath,
			want:   http.StatusOK,
			served: true,
		},
		{
			name: "peer without the grant does not",
			who:  &fakeWhoIs{resp: &apitype.WhoIsResponse{CapMap: pushCapMap(false)}},
			path: debugPprofPath,
			want: http.StatusForbidden,
		},
		{
			name: "unidentified peer does not",
			who:  &fakeWhoIs{err: errWhoIsFailed},
			path: "/debug/",
			want: http.StatusForbidden,
		},
		{
			name:   "reads are untouched",
			who:    &fakeWhoIs{resp: &apitype.WhoIsResponse{CapMap: pushCapMap(false)}},
			path:   nixCacheInfoPath,
			want:   http.StatusOK,
			served: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			served := false
			handler := gateDebug(tt.who, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				served = true

				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("GET %s = %d, want %d", tt.path, rec.Code, tt.want)
			}

			if served != tt.served {
				t.Errorf("GET %s reached the debug handler = %v, want %v", tt.path, served, tt.served)
			}
		})
	}
}

// TestTsnetHandlerAppliesBothGates pins the chain a tsnet listener actually
// serves. gateDebug and auth.Middleware each have their own tests, but nothing
// asserted they are both in the chain: dropping either from setupTsnetListener
// left every test green while /debug went open to the whole tailnet, or writes
// stopped needing a grant.
func TestTsnetHandlerAppliesBothGates(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{name: "debug is gated", method: http.MethodGet, path: debugPprofPath, want: http.StatusForbidden},
		{name: "writes are gated", method: http.MethodPut, path: narPutPath, want: http.StatusForbidden},
		{name: "reads are not", method: http.MethodGet, path: nixCacheInfoPath, want: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A peer that is identified but holds no push grant: the one that
			// separates "gated" from "open".
			who := &fakeWhoIs{resp: &apitype.WhoIsResponse{CapMap: pushCapMap(false)}}
			h := tsnetHandler(who, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil))

			if rec.Code != tt.want {
				t.Errorf("%s %s = %d, want %d", tt.method, tt.path, rec.Code, tt.want)
			}
		})
	}
}

// TestLocalHandlerGatesWrites pins the chain a --listen socket serves. It is
// the counterpart of TestTsnetHandlerAppliesBothGates: nothing else asserts
// that the read-only wrapper is in the chain at all, and dropping it would put
// the unauthenticated push path straight back.
func TestLocalHandlerGatesWrites(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		localWrite bool
		want       int
		reached    bool
	}{
		{name: "writes are refused", method: http.MethodPut, path: narPutPath, want: http.StatusForbidden},
		{name: "reads are not", method: http.MethodGet, path: nixCacheInfoPath, want: http.StatusOK, reached: true},
		{
			name: "debug reads keep tsweb's own audience",
			// gateDebug is not applied here — there is no push grant to check —
			// so /debug stays as open to a local reader as it always was.
			method: http.MethodGet, path: debugPprofPath, want: http.StatusOK, reached: true,
		},
		{
			name:   "the flag restores writes",
			method: http.MethodPut, path: narPutPath,
			localWrite: true, want: http.StatusOK, reached: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached := false
			h := localHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true

				w.WriteHeader(http.StatusOK)
			}), tt.localWrite)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil))

			if rec.Code != tt.want {
				t.Errorf("%s %s = %d, want %d", tt.method, tt.path, rec.Code, tt.want)
			}

			if reached != tt.reached {
				t.Errorf("%s %s reached the cache = %v, want %v", tt.method, tt.path, reached, tt.reached)
			}
		})
	}
}

// TestNewHTTPServerHasTimeouts and TestShutdownGraceFitsTheSystemdStopWindow
// guard two constants nothing else reads. They were dropped during the hardening
// pass with no replacement; the values survived, the guards did not.
func TestNewHTTPServerHasTimeouts(t *testing.T) {
	srv := newHTTPServer(http.NotFoundHandler())

	if srv.ReadHeaderTimeout <= 0 {
		t.Error("ReadHeaderTimeout is unset: slowloris protection is gone")
	}

	if srv.IdleTimeout <= 0 {
		t.Error("IdleTimeout is unset: idle keep-alive connections are never reaped")
	}
}

func TestShutdownGraceFitsTheSystemdStopWindow(t *testing.T) {
	const (
		// systemd's TimeoutStopSec default, cited by shutdownGrace's comment.
		systemdStopTimeout = 90 * time.Second
		oldSIGKILLGap      = time.Minute
	)

	if shutdownGrace < oldSIGKILLGap {
		t.Errorf("shutdownGrace = %s: shorter than %s truncates imports that used to run until systemd's SIGKILL",
			shutdownGrace, oldSIGKILLGap)
	}

	if shutdownGrace >= systemdStopTimeout {
		t.Errorf("shutdownGrace = %s: must stay clear of systemd TimeoutStopSec (%s)", shutdownGrace, systemdStopTimeout)
	}
}

// fakeBinDir builds a PATH entry holding an executable for each name, so a test
// can decide what exec.LookPath finds instead of inheriting whatever the host
// happens to have installed.
func fakeBinDir(t *testing.T, names ...string) string {
	t.Helper()

	dir := t.TempDir()

	for _, name := range names {
		path := filepath.Join(dir, name)

		err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600)
		if err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}

		// The executable bit is the whole point: it is what LookPath looks for.
		err = os.Chmod(path, 0o700) // #nosec G302 -- a fake binary must look executable
		if err != nil {
			t.Fatalf("chmod fake %s: %v", name, err)
		}
	}

	return dir
}

// captureLogs points the default logger at a buffer for the test's duration.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	buf := &bytes.Buffer{}
	prev := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return buf
}

// TestServeChecksCodecBinariesAtStartup pins the startup check: xz decoding
// always shells out, and `nix copy --to http://...` compresses with xz by
// default, so a PATH without xz means every default push fails after the client
// has uploaded the whole NAR. serve must refuse to start instead, and say what
// is missing and how to start anyway.
func TestServeChecksCodecBinariesAtStartup(t *testing.T) {
	for _, tt := range []struct {
		name     string
		binaries []string
		allow    bool
		wantErr  bool
	}{
		{name: "xz missing is fatal", wantErr: true},
		{name: "xz missing is allowed explicitly", allow: true},
		{name: "xz present", binaries: []string{"xz"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PATH", fakeBinDir(t, tt.binaries...))

			args := []string{
				flagDB, filepath.Join(t.TempDir(), "db.sqlite"),
				flagSpoolDir, filepath.Join(t.TempDir(), "spool"),
				flagListen, "",
			}
			if tt.allow {
				args = append(args, flagAllowCodecs)
			}

			err := newServeCmd().ParseAndRun(t.Context(), args)

			if !tt.wantErr {
				if errors.Is(err, errMissingCodecBinary) {
					t.Fatalf("serve refused to start: %v", err)
				}

				return
			}

			if !errors.Is(err, errMissingCodecBinary) {
				t.Fatalf("serve = %v, want errMissingCodecBinary", err)
			}

			// The operator reading this has to learn the binary, the codec it
			// costs them, and the way to start without it.
			for _, want := range []string{"xz", "xz-compressed", flagAllowCodecs} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestCheckCodecBinariesWarns covers the two absences that are not fatal: a
// codec that still decodes in-process, and one the operator has said they can
// live without. Both have to name the binary in the log, or the only record of
// a half-working cache is silence.
func TestCheckCodecBinariesWarns(t *testing.T) {
	for _, tt := range []struct {
		name        string
		binaries    []string
		useExternal bool
		allow       bool
		want        string
	}{
		{
			name:        "external zstd falls back in-process",
			binaries:    []string{"xz"},
			useExternal: true,
			want:        "zstd",
		},
		{
			name:  "allowed xz is still announced",
			allow: true,
			want:  "xz",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PATH", fakeBinDir(t, tt.binaries...))

			logs := captureLogs(t)

			err := checkCodecBinaries(tt.useExternal, tt.allow)
			if err != nil {
				t.Fatalf("checkCodecBinaries(%v, %v) = %v, want nil", tt.useExternal, tt.allow, err)
			}

			got := logs.String()
			if !strings.Contains(got, "WARN") || !strings.Contains(got, tt.want) {
				t.Errorf("logs = %q, want a warning naming %q", got, tt.want)
			}
		})
	}
}

func TestNewImporterWiresConfig(t *testing.T) {
	const (
		spoolDir  = "/spool"
		gcRootDir = "/roots"
	)

	tests := []struct {
		name         string
		cfg          serveConfig
		wantURI      string
		wantExtCodec bool
	}{
		{
			name:    "auto store uri becomes empty",
			cfg:     serveConfig{spoolDir: spoolDir, gcRootDir: gcRootDir, nixStoreURI: "auto"},
			wantURI: "",
		},
		{
			name:    "explicit store uri is passed through",
			cfg:     serveConfig{spoolDir: spoolDir, gcRootDir: gcRootDir, nixStoreURI: "/srv/cache"},
			wantURI: "/srv/cache",
		},
		{
			name:         "external compression flag reaches the importer",
			cfg:          serveConfig{spoolDir: spoolDir, gcRootDir: gcRootDir, externalCompression: true},
			wantExtCodec: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			imp := newImporter(tt.cfg)

			if imp.NixStoreURI != tt.wantURI {
				t.Errorf("NixStoreURI = %q, want %q", imp.NixStoreURI, tt.wantURI)
			}

			if imp.UseExternal != tt.wantExtCodec {
				t.Errorf("UseExternal = %v, want %v", imp.UseExternal, tt.wantExtCodec)
			}

			if imp.SpoolDir != tt.cfg.spoolDir || imp.GCRootDir != tt.cfg.gcRootDir {
				t.Errorf("importer dirs = %q/%q, want %q/%q",
					imp.SpoolDir, imp.GCRootDir, tt.cfg.spoolDir, tt.cfg.gcRootDir)
			}
		})
	}
}

func TestParseTsnetSpec(t *testing.T) {
	const testTsnetHostname = "cache"

	tests := []struct {
		name    string
		spec    string
		want    tsnetSpec
		wantErr error
	}{
		{
			name: "full spec",
			spec: "hostname=cache,authkey-file=/run/key,dir=/var/lib/ts,port=8080,tls=false,control=https://hs",
			want: tsnetSpec{
				hostname:    testTsnetHostname,
				authKeyFile: "/run/key",
				dir:         "/var/lib/ts",
				port:        "8080",
				control:     "https://hs",
			},
		},
		{"hostname defaults", "dir=/var/lib/ts", tsnetSpec{hostname: "tsnixcache", dir: "/var/lib/ts"}, nil},
		{"trailing comma tolerated", "hostname=cache,", tsnetSpec{hostname: testTsnetHostname}, nil},
		{
			name: "preserve path whitespace",
			spec: "hostname=cache, authkey-file= /run/key \t, dir= /var/lib/cache \t, ",
			want: tsnetSpec{hostname: testTsnetHostname, authKeyFile: " /run/key \t", dir: " /var/lib/cache \t"},
		},
		{"tls=true is rejected, not ignored", "hostname=cache,tls=true", tsnetSpec{}, errTsnetTLSUnsupported},
		{"tls must be a bool", "tls=yes please", tsnetSpec{}, strconv.ErrSyntax},
		{"typo'd key is rejected", "hostnmae=cache", tsnetSpec{}, errTsnetUnknownKey},
		{"bare word is rejected", "hostname=cache,verbose", tsnetSpec{}, errTsnetMalformedPart},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTsnetSpec(tt.spec)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("parseTsnetSpec(%q) error = %v, want %v", tt.spec, err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseTsnetSpec(%q): %v", tt.spec, err)
			}

			if got != tt.want {
				t.Errorf("parseTsnetSpec(%q) = %+v, want %+v", tt.spec, got, tt.want)
			}
		})
	}
}

// TestNonLoopbackListenerWarns pins the exposure warning for a hand-run serve.
// Reads are open on every listener, so binding beyond loopback hands the whole
// store to anything that can route to it. The NixOS module warns at evaluation
// time; nothing else covers the command line the README documents.
func TestNonLoopbackListenerWarns(t *testing.T) {
	tests := []struct {
		name     string
		addr     net.Addr
		loopback bool
	}{
		{"ipv4 loopback", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5000}, true},
		{"ipv6 loopback", &net.TCPAddr{IP: net.IPv6loopback, Port: 5000}, true},
		{"wildcard", &net.TCPAddr{IP: net.IPv4zero, Port: 5000}, false},
		{"routable", &net.TCPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 5000}, false},
		{"unix socket", &net.UnixAddr{Name: "/run/tsnixcache.sock", Net: "unix"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLoopbackAddr(tt.addr); got != tt.loopback {
				t.Errorf("isLoopbackAddr(%v) = %v, want %v", tt.addr, got, tt.loopback)
			}
		})
	}
}
