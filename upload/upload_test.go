// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package upload

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/kradalby/tsnixcache/nar"
	"github.com/kradalby/tsnixcache/narinfo"
	"github.com/kradalby/tsnixcache/nixbase32"
	"github.com/kradalby/tsnixcache/nixcompress"
)

const outOfClosure = "/out/of/closure"

// testNarHash is the canonical form of the SRI narHash used across these tests.
const testNarHash = "sha256:1xrzajc2midknsrfrhhv2cj8q1mxq26hq16qjm1c0dbcaj6fch0s"

// errTest stands in for any transient upload failure.
var errTest = errors.New("test failure")

// TestScheduleRespectsReferences walks the dependency graph and
// asserts every path is scheduled only after all of its references.
func TestScheduleRespectsReferences(t *testing.T) {
	// Diamond: d -> {b,c}; b -> a; c -> a; plus a self-reference on a and an
	// out-of-closure reference that must be ignored.
	metas := []pathMeta{
		{Path: "a", References: []string{"a", outOfClosure}},
		{Path: "b", References: []string{"a"}},
		{Path: "c", References: []string{"a"}},
		{Path: "d", References: []string{"b", "c"}},
	}

	g := buildGraph(metas)

	done := map[string]bool{}
	seen := 0

	for layer := g.ready(); len(layer) > 0; layer = g.advance(layer) {
		for _, p := range layer {
			for _, ref := range g.byPath[p].References {
				if ref == p || ref == outOfClosure {
					continue
				}

				if !done[ref] {
					t.Fatalf("%s scheduled before its reference %s", p, ref)
				}
			}
		}

		for _, p := range layer {
			done[p] = true
			seen++
		}
	}

	if seen != len(metas) {
		t.Fatalf("scheduled %d paths, want %d (a cycle or leak?)", seen, len(metas))
	}
}

func TestHasFailedDep(t *testing.T) {
	// b depends on a; c depends on nothing in-closure. Self-ref and out-of-closure
	// refs must be ignored.
	g := buildGraph([]pathMeta{
		{Path: "a"},
		{Path: "b", References: []string{"a", "b"}},
		{Path: "c", References: []string{outOfClosure}},
	})

	failed := map[string]bool{"a": true}

	if !hasFailedDep(g, g.byPath["b"], failed) {
		t.Error("b depends on failed a — should report a failed dependency")
	}

	if hasFailedDep(g, g.byPath["c"], failed) {
		t.Error("c has no in-closure dependency on a failed path")
	}

	if hasFailedDep(g, g.byPath["a"], failed) {
		t.Error("a has no dependencies; a self-reference is not a dependency")
	}
}

// TestMonitorReportsProgress checks the monitor emits onProgress with the
// uncompressed bytes-sent counter each tick while a transfer is in flight.
// Bubbled: the monitor's ticker runs on synthetic time, so waiting out two ticks
// is exact rather than a sleep that a loaded machine can make flaky.
func TestMonitorReportsProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var got []int64

		u := &uploader{
			stallTimeout: time.Second, // long: we close done before any stall fires
			onProgress: func(_ string, sent int64, _ float64) {
				got = append(got, sent)
			},
		}

		m := &meterReader{r: strings.NewReader("")}
		m.n.Store(1000) // some wire bytes have moved

		var sent atomic.Int64

		sent.Store(2000) // uncompressed bytes produced

		_, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)

		done := make(chan struct{})
		peakCh := make(chan float64, 1)

		startTestTask(t, func() { peakCh <- u.monitor("/nix/store/x", m, &sent, cancel, done) })

		synctest.Sleep(250 * time.Millisecond) // 2 ticks at 100ms
		close(done)
		<-peakCh

		if len(got) != 2 {
			t.Fatalf("onProgress fired %d times over 2 ticks, want 2", len(got))
		}

		if last := got[len(got)-1]; last != 2000 {
			t.Errorf("last reported sent = %d, want 2000", last)
		}
	})
}

// TestOnStartFiresOncePerPath: OnStart marks a path's transfer beginning, not an
// attempt beginning. A UI keys its progress bar on the path, so a second start
// for the same path orphans the first bar and hangs the render.
func TestOnStartFiresOncePerPath(t *testing.T) {
	var (
		mu      sync.Mutex
		starts  []int64
		narPuts int
	)

	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			http.NotFound(w, r) // not present, so it must be uploaded

			return
		}

		_, _ = io.Copy(io.Discard, r.Body)

		mu.Lock()
		narPuts++
		mu.Unlock()

		http.Error(w, "nope", http.StatusInternalServerError) // every attempt fails
	}))

	// Client is what starts the in-memory server and fills in srv.URL, so it has
	// to be called before the URL is read below.
	client := srv.Client()

	payload := filepath.Join(t.TempDir(), "payload")

	err := os.WriteFile(payload, []byte("tsnixcache onstart\n"), 0o600)
	if err != nil {
		t.Fatalf("write payload: %v", err)
	}

	const attempts = 2

	u := &uploader{
		target:   srv.URL,
		attempts: attempts,
		client:   client,
		onStart: func(_ string, narSize int64) {
			mu.Lock()
			defer mu.Unlock()

			starts = append(starts, narSize)
		},
	}

	m := pathMeta{
		Path:    payload,
		NarHash: testNarHash,
		NarSize: 4242,
	}

	_, err = u.uploadPath(t.Context(), m)
	if err == nil {
		t.Fatal("expected the upload to fail: the server rejects every NAR PUT")
	}

	mu.Lock()
	defer mu.Unlock()

	if narPuts != attempts {
		t.Fatalf("server saw %d NAR PUTs, want %d — the path was not retried, so the test proves nothing", narPuts, attempts)
	}

	if len(starts) != 1 {
		t.Fatalf("OnStart fired %d times across %d attempts, want exactly 1", len(starts), attempts)
	}

	if starts[0] != m.NarSize {
		t.Errorf("OnStart narSize = %d, want %d", starts[0], m.NarSize)
	}
}

// TestRunReportsReferenceCycle: paths in a reference cycle can never be scheduled
// (their in-degree never reaches zero). They must be reported as failures, not
// dropped from the stats — a silently unpushed path still exits 0.
func TestRunReportsReferenceCycle(t *testing.T) {
	metas := []pathMeta{
		{Path: "a", NarSize: 1, References: []string{"b"}},
		{Path: "b", NarSize: 2, References: []string{"a"}},
	}

	u := &uploader{jobs: 1}

	stats := u.run(t.Context(), metas)

	if len(stats) != len(metas) {
		t.Fatalf("run reported %d of %d paths: a path in a reference cycle was dropped silently", len(stats), len(metas))
	}

	for _, s := range stats {
		if !errors.Is(s.Err, errRefCycle) {
			t.Errorf("stats for %s: Err = %v, want %v", s.Path, s.Err, errRefCycle)
		}
	}
}

// TestPutNarInfoIgnoresStallTimeout: the server does the whole verify +
// decompress + nix-store --import inside this one request and sends nothing
// meanwhile, so the transfer-stall watchdog must not cancel it — a multi-GB path
// would otherwise be killed mid-import on every attempt and could never land.
func TestPutNarInfoIgnoresStallTimeout(t *testing.T) {
	const stall = 50 * time.Millisecond

	release := make(chan struct{})
	cancelled := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done(): // the client gave up on us
			close(cancelled)
		}
	}))
	t.Cleanup(srv.Close)

	u := &uploader{target: srv.URL, stallTimeout: stall, client: srv.Client()}

	errCh := make(chan error, 1)

	startTestTask(t, func() {
		errCh <- u.putNarInfo(t.Context(), "8kvxvr3pmsypxiypq4g8zy13glnfr7nx", &narinfo.NarInfo{
			StorePath: "/nix/store/8kvxvr3pmsypxiypq4g8zy13glnfr7nx-big-1",
			URL:       "nar/8kvxvr3pmsypxiypq4g8zy13glnfr7nx.nar.zstd",
		})
	})

	// Hold the request open well past stallTimeout. A client that applied the
	// stall watchdog here cancels, and the handler tells us so.
	select {
	case <-cancelled:
		t.Fatal("the narinfo PUT was cancelled by the stall timeout while the server was still importing")
	case <-time.After(4 * stall):
		close(release)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("putNarInfo: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("putNarInfo never returned")
	}
}

// stubBackOff always asks for the same delay, chosen longer than backoff.Retry's
// 15-minute DefaultMaxElapsedTime so a test can tell whether that default is
// still in force.
type stubBackOff struct{ d time.Duration }

func (s stubBackOff) NextBackOff() time.Duration { return s.d }
func (s stubBackOff) Reset()                     {}

// TestRetryOptionsIgnoreMaxElapsedTime: --attempts must be the only thing that
// ends a path's retries. backoff.Retry's default 15-minute elapsed cap otherwise
// abandons a slow path with attempts still unspent.
func TestRetryOptionsIgnoreMaxElapsedTime(t *testing.T) {
	u := &uploader{attempts: 5}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	// Overriding the backoff (later options win) asks for a delay past the
	// 15-minute cap without the test having to wait 15 minutes.
	opts := append(u.retryOptions(), backoff.WithBackOff(stubBackOff{d: 20 * time.Minute}))

	op := func() (struct{}, error) { return struct{}{}, errTest }

	_, err := backoff.Retry(ctx, op, opts...)

	// With the cap in force Retry gives up on the spot (20m > 15m) and returns the
	// operation's error; with it disabled it waits out the backoff, so the context
	// deadline is what stops it.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Retry returned %v, want a context deadline: the 15-minute MaxElapsedTime still overrides --attempts", err)
	}
}

// TestUploaderReusesConnections drives the client the production path actually
// installs (newUploader, as Closure calls it) and counts connections the server
// sees opened. Two rounds of `jobs` concurrent requests must open `jobs`
// connections in total: the first round's connections go idle and the second
// round reuses them.
//
// It bites in both directions. Drop the pooled client and DefaultTransport's cap
// of 2 idle connections per host throws most of the first round away, so round
// two re-dials. Build the transport per newUploader call instead of sharing one
// and round two starts from an empty pool, which is what a watch daemon does on
// every poll.
func TestUploaderReusesConnections(t *testing.T) {
	const jobs = 8

	var opened atomic.Int64

	arrived := make(chan struct{}, jobs)
	release := make(chan struct{}, jobs)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}

		<-release // one token per held request, sent once the whole round is in flight

		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()

	t.Cleanup(srv.Close)

	// round fires jobs concurrent requests, holds them all open so each needs its
	// own connection, then releases them back to the idle pool.
	round := func(c *http.Client) {
		var wg sync.WaitGroup

		for range jobs {
			wg.Go(func() {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
				if err != nil {
					t.Errorf("new request: %v", err)

					return
				}

				resp, err := c.Do(req)
				if err != nil {
					t.Errorf("GET: %v", err)

					return
				}
				defer resp.Body.Close()

				_, _ = io.Copy(io.Discard, resp.Body)
			})
		}

		for range jobs {
			<-arrived
		}

		for range jobs {
			release <- struct{}{}
		}

		wg.Wait()
	}

	// A fresh uploader per round, exactly as each Closure call builds one.
	round(newUploader(srv.URL, Options{Jobs: jobs}).client)

	first := opened.Load()

	if first != jobs {
		t.Fatalf("first round opened %d connections, want %d — the test isn't holding them concurrently", first, jobs)
	}

	round(newUploader(srv.URL, Options{Jobs: jobs}).client)

	if got := opened.Load(); got != jobs {
		t.Errorf("after two rounds the server saw %d connections, want %d: idle connections are not being reused across uploads", got, jobs)
	}
}

// TestPutNarInfoBoundedByImportTimeout: watch calls Closure synchronously on the
// daemon context with no deadline, so a server that accepts the narinfo PUT and
// never answers must not stop the poll loop for good.
func TestPutNarInfoBoundedByImportTimeout(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select { // accept, then never answer
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: lets srv.Close finish

	u := &uploader{
		target:        srv.URL,
		stallTimeout:  time.Minute,
		importTimeout: 200 * time.Millisecond,
		client:        srv.Client(),
	}

	errCh := make(chan error, 1)

	// A deadline-free context matches the watcher; cancellation also bounds failed tests.
	ctx := t.Context()

	startTestTask(t, func() {
		errCh <- u.putNarInfo(ctx, "8kvxvr3pmsypxiypq4g8zy13glnfr7nx", &narinfo.NarInfo{
			StorePath: "/nix/store/8kvxvr3pmsypxiypq4g8zy13glnfr7nx-wedged-1",
			URL:       "nar/8kvxvr3pmsypxiypq4g8zy13glnfr7nx.nar.zstd",
		})
	})

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("putNarInfo succeeded against a server that never answered")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("putNarInfo never returned: a wedged server blocks the watch poll loop forever")
	}
}

// TestWedgedNarInfoIsNotRetried: the import deadline bounds one narinfo PUT, but
// the retried unit is the whole transfer, so retrying multiplies the deadline by
// --attempts — 35 minutes becomes ~70 for watch and nearly three hours for push,
// with the watch poll loop blocked throughout. A server that blew the deadline
// once must therefore end the path, not be asked again.
func TestWedgedNarInfoIsNotRetried(t *testing.T) {
	var (
		mu           sync.Mutex
		narPuts      int
		narInfoPuts  int
		release      = make(chan struct{})
		payloadBytes = []byte("tsnixcache wedged\n")
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			http.NotFound(w, r) // not present, so it must be uploaded

			return
		}

		_, _ = io.Copy(io.Discard, r.Body)

		mu.Lock()

		if strings.HasSuffix(r.URL.Path, ".narinfo") {
			narInfoPuts++
			mu.Unlock()

			select { // accept the narinfo, then never answer: wedged
			case <-release:
			case <-r.Context().Done():
			}

			return
		}

		narPuts++
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: lets srv.Close finish

	payload := filepath.Join(t.TempDir(), "payload")

	err := os.WriteFile(payload, payloadBytes, 0o600)
	if err != nil {
		t.Fatalf("write payload: %v", err)
	}

	const attempts = 3

	u := &uploader{
		target:        srv.URL,
		attempts:      attempts,
		stallTimeout:  time.Minute,
		importTimeout: 200 * time.Millisecond,
		client:        srv.Client(),
	}

	// context.Background(), not t.Context(): a deadline-free context is exactly
	// what the watch daemon passes, so nothing but this bound stops the retries.
	_, err = u.uploadPath(context.Background(), pathMeta{
		Path:    payload,
		NarHash: testNarHash,
		NarSize: int64(len(payloadBytes)),
	})
	if !errors.Is(err, errImportWedged) {
		t.Fatalf("uploadPath returned %v, want %v", err, errImportWedged)
	}

	mu.Lock()
	defer mu.Unlock()

	if narInfoPuts != 1 {
		t.Errorf("server saw %d narinfo PUTs across %d attempts, want 1: the import deadline is paid once per attempt", narInfoPuts, attempts)
	}

	if narPuts != 1 {
		t.Errorf("server saw %d NAR PUTs, want 1: the whole transfer was repeated against a wedged server", narPuts)
	}
}

// TestMonitorRateUsesElapsedTime: when a sample is late, the bytes since the last
// sample accumulated over the real gap, not over one nominal tick. Dividing by
// the tick would report (and advertise as "peak") a rate several times the truth.
func TestMonitorRateUsesElapsedTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			chunk = 3_000_000              // bytes that appear between two samples
			block = 300 * time.Millisecond // how late the next sample is (3 ticks)
		)

		m := &meterReader{r: strings.NewReader("")}

		var (
			sent   atomic.Int64
			calls  int // monitor calls onProgress from one goroutine only
			second = make(chan float64, 1)
		)

		u := &uploader{
			stallTimeout: time.Minute, // long: never fires here
			onProgress: func(_ string, _ int64, bps float64) {
				calls++

				switch calls {
				case 1:
					m.n.Add(chunk)
					synctest.Sleep(block) // hold the monitor so the next tick is late
				case 2:
					second <- bps
				}
			},
		}

		_, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)

		done := make(chan struct{})
		peakCh := make(chan float64, 1)

		startTestTask(t, func() { peakCh <- u.monitor("/nix/store/x", m, &sent, cancel, done) })

		// The bytes moved over at least `block`, so the honest rate is at most this.
		want := float64(chunk) / block.Seconds()

		if got := <-second; got > want*1.5 {
			t.Errorf("rate = %.0f B/s, want no more than %.0f B/s: divided by the tick interval, not the elapsed time", got, want)
		}

		close(done)

		if peak := <-peakCh; peak > want*1.5 {
			t.Errorf("peak = %.0f B/s, want no more than %.0f B/s", peak, want)
		}
	})
}

// TestNarPutStallIsCancelled: the stall watchdog is what keeps a laptop that
// drops offline mid-upload from hanging the whole poll. This server swallows the
// body and then goes silent, so the wire meter stops moving and nothing but the
// watchdog can end the request.
func TestNarPutStallIsCancelled(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)

		select { // take the NAR, then never answer
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: lets srv.Close finish

	u := &uploader{
		target:       srv.URL,
		stallTimeout: 100 * time.Millisecond,
		client:       srv.Client(),
	}

	wire, _, _, err := u.putNar(t.Context(), testPayload(t, "tsnixcache stall\n"), "abc"+narExt)
	if !errors.Is(err, errStalled) {
		t.Fatalf("putNar sent %d bytes and returned %v, want %v: a dead link hangs the transfer",
			wire, err, errStalled)
	}
}

// TestPermanentStatusEndsRetries: a 4xx says the request itself is wrong, so
// every attempt gets the same answer — and the retried unit is the whole
// transfer, so a NAR over the server's size cap would be pushed in full and cut
// off with 413 once per attempt. The server's message must survive too: it is
// the only thing that says why the push failed.
func TestPermanentStatusEndsRetries(t *testing.T) {
	const (
		attempts = 2
		message  = "write access requires push grant"
	)

	tests := []struct {
		name     string
		status   int
		wantPuts int
	}{
		{"revoked push grant is permanent", http.StatusForbidden, 1},
		{"rate limiting is transient", http.StatusTooManyRequests, attempts},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				narPuts int
			)

			srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					http.NotFound(w, r) // not present, so it must be uploaded

					return
				}

				_, _ = io.Copy(io.Discard, r.Body)

				mu.Lock()
				narPuts++
				mu.Unlock()

				http.Error(w, message, tt.status)
			}))

			// Client starts the in-memory server and fills in srv.URL.
			client := srv.Client()

			u := &uploader{
				target:       srv.URL,
				attempts:     attempts,
				stallTimeout: time.Minute,
				client:       client,
			}

			_, err := u.uploadPath(t.Context(), pathMeta{
				Path:    testPayload(t, "tsnixcache permanent\n"),
				NarHash: testNarHash,
				NarSize: 20,
			})
			if err == nil {
				t.Fatal("expected the upload to fail: the server rejects every NAR PUT")
			}

			if !strings.Contains(err.Error(), message) {
				t.Errorf("error %q drops the server's own message %q", err, message)
			}

			mu.Lock()
			defer mu.Unlock()

			if narPuts != tt.wantPuts {
				t.Errorf("server saw %d NAR PUTs across %d attempts, want %d", narPuts, attempts, tt.wantPuts)
			}
		})
	}
}

// testPayload writes a file to serve as a stand-in store path for NAR streaming.
func testPayload(t *testing.T, content string) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "payload")

	err := os.WriteFile(p, []byte(content), 0o600)
	if err != nil {
		t.Fatalf("write payload: %v", err)
	}

	return p
}

func TestReadyDependentStartsBeforeUnrelatedSiblingFinishes(t *testing.T) {
	const fast = "fast"

	slowStarted := make(chan struct{})
	fastDone := make(chan struct{})
	childStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseSlow) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow.narinfo":
			close(slowStarted)

			select {
			case <-releaseSlow:
			case <-r.Context().Done():
			}
		case "/child.narinfo":
			close(childStarted)
		}

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	u := newUploader(server.URL, Options{Jobs: 2, Attempts: 1, OnPath: func(s Stats) {
		if s.Path == fast {
			close(fastDone)
		}
	}})
	u.client = server.Client()

	var (
		group errgroup.Group
		stats []Stats
	)

	group.Go(func() error {
		stats = u.run(t.Context(), []pathMeta{{Path: "slow"}, {Path: fast}, {Path: "child", References: []string{fast}}})

		return nil
	})
	t.Cleanup(func() { release(); require.NoError(t, group.Wait()) })

	for _, ready := range []<-chan struct{}{slowStarted, fastDone, childStarted} {
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			select {
			case <-ready:
			default:
				require.Fail(c, "ready dependent blocked behind sibling")
			}
		}, 3*time.Second, time.Millisecond)
	}

	release()
	require.NoError(t, group.Wait())
	require.Len(t, stats, 3)

	for _, s := range stats {
		require.NoError(t, s.Err)
	}
}

func TestRunGraphOutcomes(t *testing.T) {
	tests := []struct {
		name     string
		metas    []pathMeta
		reject   string
		failures map[string]error
	}{
		{name: "diamond and duplicates", metas: []pathMeta{
			{Path: "a", References: []string{"a", outOfClosure}},
			{Path: "b", References: []string{"a"}},
			{Path: "b", References: []string{"a", "a"}},
			{Path: "c", References: []string{"a"}},
			{Path: "d", References: []string{"b", "c"}},
		}},
		{name: "failed ancestry", reject: "a", metas: []pathMeta{
			{Path: "a"}, {Path: "b", References: []string{"a"}}, {Path: "c", References: []string{"b"}}, {Path: "d"},
		}, failures: map[string]error{"a": nil, "b": errDepFailed, "c": errDepFailed}},
		{name: "cycle and independent path", metas: []pathMeta{
			{Path: "a", References: []string{"b"}}, {Path: "b", References: []string{"a"}}, {Path: "c", References: []string{"b"}}, {Path: "d"},
		}, failures: map[string]error{"a": errRefCycle, "b": errRefCycle, "c": errRefCycle}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			graph := buildGraph(tt.metas)

			var mu sync.Mutex

			finished := make(map[string]int)

			var orderingErrors []string

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".narinfo")

				mu.Lock()
				for _, ref := range graph.byPath[path].References {
					if _, known := graph.byPath[ref]; known && ref != path && finished[ref] == 0 {
						orderingErrors = append(orderingErrors, path+" before "+ref)
					}
				}
				mu.Unlock()

				if path == tt.reject {
					http.NotFound(w, r)

					return
				}

				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			u := newUploader(server.URL, Options{Jobs: 3, Attempts: 1, OnPath: func(s Stats) { mu.Lock(); finished[s.Path]++; mu.Unlock() }})
			u.client = server.Client()
			stats := u.run(t.Context(), tt.metas)
			require.Len(t, stats, len(graph.byPath))
			require.Empty(t, orderingErrors)

			for _, s := range stats {
				require.Equal(t, 1, finished[s.Path])

				expected, failed := tt.failures[s.Path]
				if failed {
					require.Error(t, s.Err)

					if expected != nil {
						require.ErrorIs(t, s.Err, expected)
					}
				} else {
					require.NoError(t, s.Err)
				}
			}
		})
	}
}

func TestRunConcurrencyAndCancellation(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelRun), func(t *testing.T) {
			const jobs = 3

			var active, peak, requests atomic.Int64

			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := active.Add(1)
				defer active.Add(-1)

				for prev := peak.Load(); n > prev && !peak.CompareAndSwap(prev, n); prev = peak.Load() {
				}

				requests.Add(1)

				select {
				case <-release:
					w.WriteHeader(http.StatusOK)
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(server.Close)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			metas := make([]pathMeta, 0, 20)
			for i := range 20 {
				metas = append(metas, pathMeta{Path: fmt.Sprintf("p%d", i), NarHash: testNarHash})
			}

			u := newUploader(server.URL, Options{Jobs: jobs, Attempts: 1})
			u.client = server.Client()

			var (
				group errgroup.Group
				stats []Stats
			)

			group.Go(func() error {
				stats = u.run(ctx, metas)

				return nil
			})
			t.Cleanup(func() { cancel(); unblock(); require.NoError(t, group.Wait()) })
			require.EventuallyWithT(t, func(c *assert.CollectT) { require.EqualValues(c, jobs, active.Load()) }, 3*time.Second, time.Millisecond)

			if cancelRun {
				cancel()
			} else {
				unblock()
			}

			require.NoError(t, group.Wait())
			require.Len(t, stats, len(metas))
			require.EqualValues(t, jobs, peak.Load())

			for _, s := range stats {
				if cancelRun {
					require.ErrorIs(t, s.Err, context.Canceled)
				} else {
					require.NoError(t, s.Err)
				}
			}

			if cancelRun {
				require.EqualValues(t, jobs, requests.Load())
			} else {
				require.EqualValues(t, len(metas), requests.Load())
			}
		})
	}
}

func TestRunPreCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	u := &uploader{jobs: 2}
	stats := u.run(ctx, []pathMeta{{Path: "a"}, {Path: "b"}, {Path: "c", References: []string{"a"}}})
	require.Len(t, stats, 3)

	for _, s := range stats {
		if s.Path == "c" {
			require.ErrorIs(t, s.Err, errDepFailed)
		} else {
			require.ErrorIs(t, s.Err, context.Canceled)
		}
	}
}

func TestSplitGoneDeduplicatesRoots(t *testing.T) {
	livePath := filepath.Join(t.TempDir(), "live")
	missing := filepath.Join(t.TempDir(), "missing")
	require.NoError(t, os.WriteFile(livePath, nil, 0o600))
	live, gone := splitGone([]string{livePath, missing, livePath, missing})
	require.Equal(t, []string{livePath}, live)
	require.Equal(t, []string{missing}, gone)
}

func TestRunEmptyClosureWithLargeJobLimit(t *testing.T) {
	u := &uploader{jobs: math.MaxInt}
	require.Empty(t, u.run(t.Context(), nil))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPutNarRejectsEarlyResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload")
	require.NoError(t, os.WriteFile(path, []byte("never consumed"), 0o600))

	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			u := newUploader("http://cache.test", Options{})
			u.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				_ = r.Body.Close()

				return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: make(http.Header), Body: http.NoBody, Request: r}, nil
			})}
			wire, hash, _, err := u.putNar(t.Context(), path, "payload.nar.zstd")
			require.Error(t, err)
			require.Zero(t, wire)
			require.Empty(t, hash)

			if status != http.StatusOK {
				require.ErrorIs(t, err, errBadStatus)
			}

			var permanent *backoff.PermanentError
			require.Equal(t, status == http.StatusBadRequest, errors.As(err, &permanent))
		})
	}
}

func TestIncompleteNarDoesNotPublishNarInfo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload")
	require.NoError(t, os.WriteFile(path, []byte("never consumed"), 0o600))

	var narinfos atomic.Int64

	u := newUploader("http://cache.test", Options{Attempts: 1})
	u.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		if r.Method == http.MethodHead {
			status = http.StatusNotFound
		}

		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, ".narinfo") {
			narinfos.Add(1)
		}

		if r.Body != nil {
			_ = r.Body.Close()
		}

		return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: make(http.Header), Body: http.NoBody, Request: r}, nil
	})}
	stats := u.run(t.Context(), []pathMeta{{Path: path, NarHash: testNarHash}})
	require.Len(t, stats, 1)
	require.Error(t, stats[0].Err)
	require.Zero(t, narinfos.Load())
}

func TestPutNarEarlyResponseRealHTTP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload")
	payload := make([]byte, 16<<20)
	_, err := rand.Read(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, payload, 0o600))

	hasher := sha256.New()

	var expectedBytes atomic.Int64

	encoder, err := nixcompress.Encoder(t.Context(), &countWriter{w: hasher, n: &expectedBytes}, narCompression, false)
	require.NoError(t, err)
	require.NoError(t, nar.Write(encoder, path))
	require.NoError(t, encoder.Close())

	expectedHash := "sha256:" + nixbase32.EncodeToString(hasher.Sum(nil))

	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			handlerErr := make(chan error, 1)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				controller := http.NewResponseController(w)

				duplexErr := controller.EnableFullDuplex()
				if duplexErr != nil {
					handlerErr <- duplexErr

					return
				}

				w.Header().Set("Content-Length", "0")
				w.WriteHeader(status)

				flushErr := controller.Flush()
				if flushErr != nil {
					handlerErr <- flushErr

					return
				}

				_, _ = io.Copy(io.Discard, r.Body)
			}))
			defer server.Close()

			u := newUploader(server.URL, Options{})

			u.client = server.Client()
			for range 5 {
				wire, hash, _, err := u.putNar(t.Context(), path, "payload.nar.zstd")
				if status == http.StatusOK && err == nil {
					require.Equal(t, expectedBytes.Load(), wire)
					require.Equal(t, expectedHash, hash)
				} else {
					require.Error(t, err)

					if status != http.StatusOK {
						require.ErrorIs(t, err, errBadStatus)
					}
				}
			}

			select {
			case err := <-handlerErr:
				require.NoError(t, err)
			default:
			}
		})
	}
}

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

func TestResponseBodyStall(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusServiceUnavailable} {
		for _, narinfoPut := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/narinfo=%t", status, narinfoPut), func(t *testing.T) {
				file := filepath.Join(t.TempDir(), "input")

				err := os.WriteFile(file, []byte("hello"), 0o600)
				require.NoError(t, err)

				release := make(chan struct{})

				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)

					w.Header().Set("Content-Length", "10")
					w.WriteHeader(status)
					_ = http.NewResponseController(w).Flush()

					select {
					case <-r.Context().Done():
					case <-release:
					}
				}))
				defer srv.Close()
				defer close(release)

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				u := newUploader(srv.URL, Options{StallTimeout: 25 * time.Millisecond})
				result := make(chan error, 1)

				startTestTask(t, func() {
					if narinfoPut {
						result <- u.putNarInfo(ctx, "hash", &narinfo.NarInfo{})

						return
					}

					_, _, _, err := u.putNar(ctx, file, "test.nar.zstd")
					result <- err
				})

				select {
				case err := <-result:
					require.Error(t, err, "stalled response succeeded")

					if status == http.StatusBadRequest {
						if !errors.Is(err, errBadStatus) {
							t.Fatalf("permanent status lost: %v", err)
						}
					}
				case <-time.After(time.Second):
					cancel()
					<-result
					t.Fatal("response body bypassed timeout")
				}
			})
		}
	}
}

func TestResponseBodyFailures(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(fmt.Sprintf("oversized=%t", oversized), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)

				if oversized {
					_, _ = io.WriteString(w, strings.Repeat("x", maxErrBody+1))

					return
				}

				w.Header().Set("Content-Length", "100")
				_, _ = io.WriteString(w, "short")
			}))
			defer srv.Close()

			u := newUploader(srv.URL, Options{})
			err := u.putNarInfo(t.Context(), "hash", &narinfo.NarInfo{})

			want := io.ErrUnexpectedEOF
			if oversized {
				want = errResponseTooLarge
			}

			if !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
		})
	}
}

func startTestTask(tb testing.TB, task func()) {
	tb.Helper()

	var group errgroup.Group
	group.Go(func() error {
		task()

		return nil
	})
	tb.Cleanup(func() { require.NoError(tb, group.Wait()) })
}
