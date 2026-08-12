// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package upload

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cenkalti/backoff/v5"

	"github.com/kradalby/tsnixcache/narinfo"
)

const outOfClosure = "/out/of/closure"

// testNarHash is the canonical form of the SRI narHash used across these tests.
const testNarHash = "sha256:1xrzajc2midknsrfrhhv2cj8q1mxq26hq16qjm1c0dbcaj6fch0s"

// errTest stands in for any transient upload failure.
var errTest = errors.New("test failure")

// TestScheduleRespectsReferences walks the layers the scheduler would use and
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

		go func() { peakCh <- u.monitor("/nix/store/x", m, &sent, cancel, done) }()

		time.Sleep(250 * time.Millisecond) // 2 ticks at 100ms
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(srv.Close)

	payload := filepath.Join(t.TempDir(), "payload")

	err := os.WriteFile(payload, []byte("tsnixcache onstart\n"), 0o600)
	if err != nil {
		t.Fatalf("write payload: %v", err)
	}

	const attempts = 2

	u := &uploader{
		target:   srv.URL,
		attempts: attempts,
		client:   srv.Client(),
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

	go func() {
		errCh <- u.putNarInfo(t.Context(), "8kvxvr3pmsypxiypq4g8zy13glnfr7nx", &narinfo.NarInfo{
			StorePath: "/nix/store/8kvxvr3pmsypxiypq4g8zy13glnfr7nx-big-1",
			URL:       "nar/8kvxvr3pmsypxiypq4g8zy13glnfr7nx.nar.zstd",
		})
	}()

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

	// context.Background(), not t.Context(): a deadline-free context is exactly
	// what the watch daemon passes.
	go func() {
		errCh <- u.putNarInfo(context.Background(), "8kvxvr3pmsypxiypq4g8zy13glnfr7nx", &narinfo.NarInfo{
			StorePath: "/nix/store/8kvxvr3pmsypxiypq4g8zy13glnfr7nx-wedged-1",
			URL:       "nar/8kvxvr3pmsypxiypq4g8zy13glnfr7nx.nar.zstd",
		})
	}()

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
					time.Sleep(block) // hold the monitor so the next tick is late
				case 2:
					second <- bps
				}
			},
		}

		_, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)

		done := make(chan struct{})
		peakCh := make(chan float64, 1)

		go func() { peakCh <- u.monitor("/nix/store/x", m, &sent, cancel, done) }()

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

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			t.Cleanup(srv.Close)

			u := &uploader{
				target:       srv.URL,
				attempts:     attempts,
				stallTimeout: time.Minute,
				client:       srv.Client(),
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
