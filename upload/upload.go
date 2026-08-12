// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package upload streams Nix store paths to a tsnixcache server over the HTTP
// binary-cache protocol (PUT /nar + PUT /{hash}.narinfo), with per-path transfer
// metering. Doing the transfer itself is what lets it report each path's size,
// duration, average and peak throughput, and it never buffers a NAR — everything
// streams, so memory stays flat no matter how large the closure.
//
// Paths upload in reference order (a path only after its references are present,
// which nix-store --import requires) with bounded concurrency across independent
// paths. Already-present paths are skipped after a cheap narinfo HEAD.
package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v5"
	"golang.org/x/sync/errgroup"

	"github.com/kradalby/tsnixcache/nar"
	"github.com/kradalby/tsnixcache/narinfo"
	"github.com/kradalby/tsnixcache/nixbase32"
	"github.com/kradalby/tsnixcache/nixcompress"
)

// errBadStatus is returned when the server answers a PUT with a non-200 status.
var errBadStatus = errors.New("unexpected status")

// errPathGone marks a root that is no longer in the store, so nix cannot resolve
// its closure — usually GC removed it while it waited in watch's retry queue.
var errPathGone = errors.New("path no longer exists in the store")

// errPathsFailed is the aggregate error when one or more paths failed to upload.
var errPathsFailed = errors.New("upload: paths failed")

// errDepFailed marks a path skipped because one of its references failed.
var errDepFailed = errors.New("dependency failed to upload")

// errStalled marks an upload cancelled because no bytes moved for stallTimeout.
var errStalled = errors.New("upload stalled: no progress")

// errImportWedged marks a narinfo PUT the server never answered within the
// import deadline.
var errImportWedged = errors.New("server did not answer the narinfo PUT within the import deadline")

// errRefCycle marks a path that could never be scheduled because it sits in a
// reference cycle, so no ordering satisfies "references first".
var errRefCycle = errors.New("not uploaded: reference cycle in the closure")

// Stats is the outcome of uploading (or skipping) a single store path.
type Stats struct {
	Path      string        // the store path
	NarSize   int64         // uncompressed NAR size
	WireBytes int64         // compressed bytes actually sent (0 if skipped)
	Duration  time.Duration // wall time of this path's transfer
	AvgBps    float64       // WireBytes / Duration, bytes per second
	PeakBps   float64       // peak sampled throughput, bytes per second
	Skipped   bool          // already present on the server
	Err       error         // non-nil if this path failed (upload error or a failed dependency)
}

// Summary aggregates a whole Closure run.
type Summary struct {
	Paths     int
	Uploaded  int
	Skipped   int
	NarBytes  int64         // total uncompressed NAR bytes uploaded
	WireBytes int64         // total compressed bytes sent
	Wall      time.Duration // wall-clock duration of the run
	PeakBps   float64       // highest per-path peak observed
	Failed    []string      // paths that failed (upload error or unmet dependency)
}

// Options tune a Closure run. Zero values pick sensible defaults.
type Options struct {
	Jobs         int           // max concurrent path uploads (default 8)
	Attempts     int           // per-path attempts before giving up (default 5)
	StallTimeout time.Duration // cancel an upload with no progress for this long (default 60s)
	OnPath       func(Stats)   // called as each path finishes; may run concurrently

	// OnStart fires when a path's transfer begins (not for skipped paths).
	// OnProgress fires periodically during a transfer with the uncompressed
	// bytes streamed so far (against NarSize) and the current wire rate. Both
	// may run concurrently across paths; a UI handler must be concurrency-safe.
	OnStart    func(path string, narSize int64)
	OnProgress func(path string, sent int64, wireBps float64)
}

// Closure uploads the closure of roots to targetURL and returns per-path stats
// plus an aggregate summary. It is idempotent: paths already on the server are
// skipped, so re-running after a partial failure resumes cleanly.
func Closure(ctx context.Context, targetURL string, roots []string, opts Options) ([]Stats, Summary, error) {
	u := newUploader(targetURL, opts)

	metas, gone, err := resolveClosure(ctx, roots)
	if err != nil {
		return nil, Summary{}, err
	}

	start := time.Now()

	stats := u.run(ctx, metas)

	// A root that vanished fails alone rather than dropping out unreported: the
	// caller asked for it, and exiting 0 on a path nobody uploaded is the worst
	// outcome.
	for _, p := range gone {
		s := Stats{Path: p, Err: errPathGone}
		stats = append(stats, s)

		if u.onPath != nil {
			u.onPath(s)
		}
	}

	sum := Summary{Paths: len(metas) + len(gone), Wall: time.Since(start)}

	for _, s := range stats {
		switch {
		case s.Err != nil:
			sum.Failed = append(sum.Failed, s.Path)
		case s.Skipped:
			sum.Skipped++
		default:
			sum.Uploaded++
			sum.NarBytes += s.NarSize
			sum.WireBytes += s.WireBytes

			if s.PeakBps > sum.PeakBps {
				sum.PeakBps = s.PeakBps
			}
		}
	}

	if len(sum.Failed) > 0 {
		return stats, sum, fmt.Errorf("%w: %d of %d", errPathsFailed, len(sum.Failed), sum.Paths)
	}

	return stats, sum, nil
}

// maxIdleConnsPerHost is how many idle connections the shared transport keeps
// per server. DefaultTransport keeps only 2, so with the default 8 jobs most
// parallel uploads would pay a fresh TCP+TLS handshake for a server they are
// already talking to. 64 covers any sane --jobs; unused slots cost nothing.
const maxIdleConnsPerHost = 64

// defaultImportTimeout bounds how long the client waits for the server to answer
// a narinfo PUT. Above the server's own 30-minute import deadline, so an import
// that gets a slot promptly always finishes (or the server gives up) first. It
// is not above every legitimate hold: the server queues on its import semaphore
// before starting that 30-minute clock (cache.handlePutNarInfo), so under import
// contention the honest total is queue-wait + 30 minutes, which has no upper
// bound. Blowing through this deadline is therefore treated as a wedged server
// rather than a transient failure — see putNarInfo.
const defaultImportTimeout = 35 * time.Minute

// narCompression is what every NAR is pushed as, and narExt the matching file
// extension. Nothing has ever asked to push uncompressed: zstd keeps up with the
// link, and the server accepts either.
const (
	narCompression = "zstd"
	narExt         = ".nar." + narCompression
)

// maxErrBody caps how much of a failed response is read for its message. The
// server answers with one short line; anything longer is a proxy's error page.
const maxErrBody = 4 << 10

// httpClient is shared by every Closure call. The watch daemon uploads once per
// poll, so a transport built per call would start each poll with a cold
// connection pool and leave the discarded pool's connections (and their
// goroutines) alive until IdleConnTimeout.
var httpClient = sync.OnceValue(func() *http.Client {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{}
	}

	tr = tr.Clone()
	tr.MaxIdleConnsPerHost = maxIdleConnsPerHost

	return &http.Client{Transport: tr}
})

type uploader struct {
	target       string
	jobs         int
	attempts     int
	stallTimeout time.Duration
	onPath       func(Stats)
	onStart      func(path string, narSize int64)
	onProgress   func(path string, sent int64, wireBps float64)
	client       *http.Client

	// importTimeout bounds the narinfo PUT; zero means defaultImportTimeout.
	// Only tests set it, so they don't have to wait out the real one.
	importTimeout time.Duration
}

// newUploader applies the Options defaults, including the shared HTTP client.
func newUploader(targetURL string, opts Options) *uploader {
	return &uploader{
		target:       strings.TrimRight(targetURL, "/"),
		jobs:         orDefault(opts.Jobs, 8),
		attempts:     orDefault(opts.Attempts, 5),
		stallTimeout: orDefault(opts.StallTimeout, 60*time.Second),
		onPath:       opts.OnPath,
		onStart:      opts.OnStart,
		onProgress:   opts.OnProgress,
		client:       httpClient(),
	}
}

// pathMeta is the subset of `nix path-info` we need per path.
type pathMeta struct {
	Path       string   `json:"path"`
	NarHash    string   `json:"narHash"`
	NarSize    int64    `json:"narSize"`
	References []string `json:"references"`
	Deriver    string   `json:"deriver"`
}

// resolveClosure returns the full closure of roots, plus the roots that are gone
// from the store.
//
// One unresolvable root fails the whole `nix path-info` invocation — it exits
// non-zero having written no JSON at all, so the other roots resolve to nothing.
// That is routine in production: auto-GC removes a path while it sits in watch's
// retry queue, and from then on every poll containing it loses the up-to-199
// healthy paths behind it. So on failure, retry once without the roots that no
// longer exist; anything still failing is nix's own problem to report.
func resolveClosure(ctx context.Context, roots []string) ([]pathMeta, []string, error) {
	metas, err := pathInfo(ctx, roots)
	if err == nil {
		return metas, nil, nil
	}

	live, gone := splitGone(roots)
	if len(gone) == 0 {
		return nil, nil, err
	}

	if len(live) == 0 {
		return nil, gone, nil
	}

	metas, err = pathInfo(ctx, live)
	if err != nil {
		return nil, nil, err
	}

	return metas, gone, nil
}

// splitGone partitions roots into those still present in the store and those
// that are not. It runs only after path-info has already failed, so the stat per
// root is free next to what follows.
func splitGone(roots []string) ([]string, []string) {
	var live, gone []string

	for _, r := range roots {
		_, err := os.Lstat(r)
		if err != nil {
			gone = append(gone, r)

			continue
		}

		live = append(live, r)
	}

	return live, gone
}

// pathInfo runs `nix path-info -r --json` over roots. nix has emitted both an
// object keyed by path and an array of objects across versions, so we accept
// either.
func pathInfo(ctx context.Context, roots []string) ([]pathMeta, error) {
	args := append([]string{"path-info", "-r", "--json"}, roots...)

	// #nosec G204 -- nix is a fixed binary; args are store paths from the caller.
	cmd := exec.CommandContext(ctx, "nix", args...)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		// (*exec.ExitError).Error() is only "exit status 1"; everything that says
		// what actually went wrong — a store path that does not exist, or the hint
		// to pass --extra-experimental-features nix-command — is on stderr.
		return nil, fmt.Errorf("upload: nix path-info: %w\n%s", err, strings.TrimSpace(stderr.String()))
	}

	var asMap map[string]pathMeta
	if json.Unmarshal(out, &asMap) == nil && len(asMap) > 0 {
		metas := make([]pathMeta, 0, len(asMap))

		for p, m := range asMap {
			m.Path = p
			metas = append(metas, m)
		}

		return metas, nil
	}

	var asArr []pathMeta

	err = json.Unmarshal(out, &asArr)
	if err != nil {
		return nil, fmt.Errorf("upload: parse path-info json: %w", err)
	}

	return asArr, nil
}

// graph is the reference DAG of a closure: an in-degree (unmet dependency count)
// and dependents list per path, used to schedule uploads deps-first.
type graph struct {
	byPath     map[string]pathMeta
	indeg      map[string]int
	dependents map[string][]string
}

func buildGraph(metas []pathMeta) *graph {
	g := &graph{
		byPath:     make(map[string]pathMeta, len(metas)),
		indeg:      make(map[string]int, len(metas)),
		dependents: make(map[string][]string, len(metas)),
	}

	for _, m := range metas {
		g.byPath[m.Path] = m
	}

	for _, m := range metas {
		for _, ref := range m.References {
			if ref == m.Path {
				continue // self-reference is not a dependency
			}

			if _, ok := g.byPath[ref]; !ok {
				continue // deriver or out-of-closure ref; not an upload dep
			}

			g.indeg[m.Path]++
			g.dependents[ref] = append(g.dependents[ref], m.Path)
		}
	}

	return g
}

// ready returns the paths whose dependencies are all satisfied (in-degree 0).
func (g *graph) ready() []string {
	layer := make([]string, 0)

	for p := range g.byPath {
		if g.indeg[p] == 0 {
			layer = append(layer, p)
		}
	}

	return layer
}

// advance decrements the in-degree of every dependent of the given layer and
// returns the next layer (those that just reached 0).
func (g *graph) advance(layer []string) []string {
	next := make([]string, 0)

	for _, p := range layer {
		for _, dep := range g.dependents[p] {
			g.indeg[dep]--

			if g.indeg[dep] == 0 {
				next = append(next, dep)
			}
		}
	}

	return next
}

// run schedules uploads one topological layer at a time, with bounded
// concurrency across the independent paths within a layer. A path's failure does
// not abort its siblings; instead it is recorded (Stats.Err), and any path that
// depends on a failed path is skipped — the server would reject a path whose
// references aren't present. Every path ends up in the returned stats exactly
// once, so the caller sees the full picture.
func (u *uploader) run(ctx context.Context, metas []pathMeta) []Stats {
	g := buildGraph(metas)

	var (
		statsMu sync.Mutex
		stats   []Stats
	)

	record := func(s Stats) {
		statsMu.Lock()
		stats = append(stats, s) //nolint:wsl_v5 // tight lock/append/unlock is idiomatic
		statsMu.Unlock()

		if u.onPath != nil {
			u.onPath(s)
		}
	}

	// failed is written by phase 2 (under failedMu) and read by the next layer's
	// phase 1 after the eg.Wait barrier, which establishes happens-before.
	failed := make(map[string]bool)

	var failedMu sync.Mutex

	for layer := g.ready(); len(layer) > 0; layer = g.advance(layer) {
		// Phase 1 (single-threaded): short-circuit paths with a failed dependency.
		toUpload := make([]pathMeta, 0, len(layer))

		for _, p := range layer {
			m := g.byPath[p]
			if hasFailedDep(g, m, failed) {
				failed[m.Path] = true

				record(Stats{Path: m.Path, NarSize: m.NarSize, Err: errDepFailed})

				continue
			}

			toUpload = append(toUpload, m)
		}

		// Phase 2: upload the rest concurrently; failures don't cancel siblings.
		var eg errgroup.Group

		eg.SetLimit(u.jobs)

		for _, m := range toUpload {
			eg.Go(func() error {
				s, err := u.uploadPath(ctx, m)
				if err != nil {
					s = Stats{Path: m.Path, NarSize: m.NarSize, Err: err}

					failedMu.Lock()
					failed[m.Path] = true
					failedMu.Unlock()
				}

				record(s)

				return nil
			})
		}

		_ = eg.Wait()
	}

	// Nix store paths can genuinely form reference cycles; a path in one keeps a
	// non-zero in-degree forever, so the layer walk above never schedules it.
	// Report it as failed — silently not uploading a path the user asked for, and
	// still exiting 0, is the worst outcome.
	recorded := make(map[string]bool, len(stats))
	for _, s := range stats {
		recorded[s.Path] = true
	}

	for p, m := range g.byPath {
		if !recorded[p] {
			record(Stats{Path: m.Path, NarSize: m.NarSize, Err: errRefCycle})
		}
	}

	return stats
}

// hasFailedDep reports whether any in-closure reference of m has failed.
func hasFailedDep(g *graph, m pathMeta, failed map[string]bool) bool {
	for _, ref := range m.References {
		if ref == m.Path {
			continue
		}

		if _, ok := g.byPath[ref]; !ok {
			continue
		}

		if failed[ref] {
			return true
		}
	}

	return false
}

// uploadPath uploads one path (skipping if already present), retrying transient
// failures. nar.Write and the PUTs all stream, so a retry just re-runs them.
func (u *uploader) uploadPath(ctx context.Context, m pathMeta) (Stats, error) {
	hashPart := hashPartOf(m.Path)

	present, err := u.headNarInfo(ctx, hashPart)
	if err == nil && present {
		return Stats{Path: m.Path, NarSize: m.NarSize, Skipped: true}, nil
	}

	// Once per path, not once per attempt: the transfer below is the retried unit,
	// and a UI keys its progress bar on the path, so a second start for the same
	// path would orphan the first bar (never completed, never aborted).
	if u.onStart != nil {
		u.onStart(m.Path, m.NarSize)
	}

	var s Stats

	op := func() (struct{}, error) {
		got, err := u.transfer(ctx, m, hashPart)
		if err != nil {
			return struct{}{}, err
		}

		s = got

		return struct{}{}, nil
	}

	_, err = backoff.Retry(ctx, op, u.retryOptions()...)
	if err != nil {
		return Stats{}, fmt.Errorf("upload %s: %w", m.Path, err)
	}

	return s, nil
}

// retryOptions bounds a path's retries by attempts alone. backoff.Retry
// otherwise applies its 15-minute DefaultMaxElapsedTime, which silently gives up
// on a slow path (a multi-GB closure member) long before its attempts run out —
// making --attempts mean something other than what it says.
func (u *uploader) retryOptions() []backoff.RetryOption {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = time.Second
	bo.MaxInterval = 10 * time.Second

	return []backoff.RetryOption{
		backoff.WithBackOff(bo),
		backoff.WithMaxTries(uint(max(u.attempts, 1))),
		backoff.WithMaxElapsedTime(0),
	}
}

// transfer does one attempt: PUT the compressed NAR, then PUT the narinfo.
func (u *uploader) transfer(ctx context.Context, m pathMeta, hashPart string) (Stats, error) {
	// `nix path-info --json` reports narHash in SRI form ("sha256-<base64>");
	// narinfo and the server want "sha256:<nixbase32>".
	narHash, err := narinfo.CanonicalHash(m.NarHash)
	if err != nil {
		return Stats{}, fmt.Errorf("upload: narHash: %w", err)
	}

	narName := hashPart + narExt
	start := time.Now()

	wire, fileHash, peak, err := u.putNar(ctx, m.Path, narName)
	if err != nil {
		return Stats{}, err
	}

	ni := &narinfo.NarInfo{
		StorePath:   m.Path,
		URL:         "nar/" + narName,
		Compression: narCompression,
		FileHash:    fileHash,
		FileSize:    uint64(wire), // #nosec G115 -- wire is a non-negative byte count
		NarHash:     narHash,
		NarSize:     uint64(m.NarSize), // #nosec G115 -- NarSize is non-negative
		References:  m.References,
		Deriver:     m.Deriver,
	}

	err = u.putNarInfo(ctx, hashPart, ni)
	if err != nil {
		return Stats{}, err
	}

	dur := time.Since(start)
	avg := rate(wire, dur)

	// A transfer shorter than one sample interval yields no instantaneous
	// sample; its peak is then just its average (peak is never below average).
	if peak < avg {
		peak = avg
	}

	return Stats{
		Path:      m.Path,
		NarSize:   m.NarSize,
		WireBytes: wire,
		Duration:  dur,
		AvgBps:    avg,
		PeakBps:   peak,
	}, nil
}

// putNar streams the store path as a NAR, compresses it, and PUTs it, metering
// the compressed bytes on the wire. Nothing is buffered: nar.Write feeds the
// compressor which feeds the request body through an io.Pipe. Returns the wire
// byte count, the compressed-file hash (for the narinfo), and the peak rate.
func (u *uploader) putNar(ctx context.Context, storePath, narName string) (int64, string, float64, error) {
	pr, pw := io.Pipe()

	// sent counts uncompressed NAR bytes produced, the progress numerator
	// (NarSize is the denominator); the wire meter below counts compressed bytes.
	var sent atomic.Int64

	go func() {
		enc, err := nixcompress.Encoder(ctx, pw, narCompression, false)
		if err != nil {
			_ = pw.CloseWithError(err)

			return
		}

		werr := nar.Write(&countWriter{w: enc, n: &sent}, storePath)
		cerr := enc.Close()

		if werr != nil {
			_ = pw.CloseWithError(werr)

			return
		}

		_ = pw.CloseWithError(cerr) // nil closes cleanly
	}()

	defer pr.Close() // unblock the producer if the request aborts early

	h := sha256.New()
	m := &meterReader{r: io.TeeReader(pr, h)}

	// The monitor samples the meter for the peak rate and cancels the request if
	// no bytes move for stallTimeout — so a laptop dropping offline mid-upload
	// fails fast into retry instead of hanging the whole poll.
	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	done := make(chan struct{})
	peakCh := make(chan float64, 1)

	go func() { peakCh <- u.monitor(storePath, m, &sent, cancel, done) }()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPut, u.target+"/nar/"+narName, m)
	if err != nil {
		close(done)

		return 0, "", 0, err
	}

	req.ContentLength = -1 // unknown length: chunked, so nothing is buffered to size it

	resp, err := u.client.Do(req)

	close(done)

	peak := <-peakCh

	if err != nil {
		if errors.Is(context.Cause(reqCtx), errStalled) {
			return 0, "", 0, fmt.Errorf("put nar: %w", errStalled)
		}

		return 0, "", 0, fmt.Errorf("put nar: %w", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, "", 0, statusError("put nar", resp)
	}

	_, _ = io.Copy(io.Discard, resp.Body)

	fileHash := "sha256:" + nixbase32.EncodeToString(h.Sum(nil))

	return m.n.Load(), fileHash, peak, nil
}

// putNarInfo PUTs the narinfo that describes the just-uploaded NAR. The request
// is tiny but not quick: the server does the whole verify + decompress +
// nix-store --import inside it and sends nothing until that finishes, so for a
// multi-GB path it legitimately takes minutes.
//
// Deliberately NOT bounded by stallTimeout. That watchdog measures transfer
// progress, and there is no transfer here to make progress — the client would
// cancel a healthy import, the server would kill nix-store mid-flight, and the
// retry would start over, so a large path could never land.
//
// It is bounded by defaultImportTimeout instead, because the watch daemon calls
// Closure synchronously on its daemon context with no deadline of its own: a
// server that accepts this request and then wedges would otherwise stop the poll
// loop for good, and no newer path would ever be picked up.
//
// Hitting that deadline is permanent, not transient. The bound is per request,
// while the retried unit is the whole transfer, so retrying multiplies it by
// --attempts (watch's 2 make ~70 minutes, push's 5 nearly three hours) and the
// poll loop is blocked for all of it — which is the very thing the bound exists
// to prevent. A retry is also pointless work: the server either wedged, or it is
// so contended that it could not answer in 35 minutes, and the retry re-uploads
// the whole NAR to ask it again. Failing the path hands it to watch's retry
// queue, which comes back later, backed off, and skips it on the presence HEAD
// if the server did finish the import after all (it runs detached from this
// request, so a client giving up does not undo it).
func (u *uploader) putNarInfo(ctx context.Context, hashPart string, ni *narinfo.NarInfo) error {
	ctx, cancel := context.WithTimeoutCause(ctx,
		orDefault(u.importTimeout, defaultImportTimeout), errImportWedged)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		u.target+"/"+hashPart+".narinfo", strings.NewReader(ni.Marshal()))
	if err != nil {
		return err
	}

	resp, err := u.client.Do(req)
	if err != nil {
		if errors.Is(context.Cause(ctx), errImportWedged) {
			return backoff.Permanent(fmt.Errorf("put narinfo: %w", errImportWedged))
		}

		return fmt.Errorf("put narinfo: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return statusError("put narinfo", resp)
	}

	_, _ = io.Copy(io.Discard, resp.Body)

	return nil
}

// statusError turns a non-200 answer into an error carrying the server's own
// message ("write access requires push grant", "request body too large"), which
// is the only thing that says why the push failed. It drains the rest of the
// body so the connection stays reusable.
//
// A 4xx is permanent: the request is what is wrong, so every attempt gets the
// same answer, and the retried unit is the whole transfer — a NAR over the
// server's size cap would be pushed in full and cut off with 413 once per
// attempt, ~80 GiB for a 16 GiB path that can never land. The three retryable
// 4xx (408, 425, 429) are about timing rather than the request itself.
func statusError(op string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))

	_, _ = io.Copy(io.Discard, resp.Body)

	err := fmt.Errorf("%s: %w: %s", op, errBadStatus, resp.Status)

	if msg := strings.TrimSpace(string(body)); msg != "" {
		err = fmt.Errorf("%s: %w: %s: %s", op, errBadStatus, resp.Status, msg)
	}

	switch resp.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return err
	}

	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return backoff.Permanent(err)
	}

	return err
}

// headNarInfo reports whether the server already has this path.
func (u *uploader) headNarInfo(ctx context.Context, hashPart string) (bool, error) {
	ctx, cancel := u.shortCtx(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.target+"/"+hashPart+".narinfo", nil)
	if err != nil {
		return false, err
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.StatusCode == http.StatusOK, nil
}

// shortCtx bounds the presence HEAD by stallTimeout so a server that accepts the
// connection but never answers can't hang the poll. Only the HEAD qualifies: it
// is pure lookup, with no server-side work that could legitimately take longer.
func (u *uploader) shortCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if u.stallTimeout <= 0 {
		return context.WithCancel(ctx)
	}

	return context.WithTimeout(ctx, u.stallTimeout)
}

// countWriter counts the bytes written through it (the uncompressed NAR stream,
// for progress against a known NarSize).
type countWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	k, err := c.w.Write(p)
	if k > 0 {
		c.n.Add(int64(k))
	}

	return k, err
}

// meterReader counts the bytes read through it.
type meterReader struct {
	r io.Reader
	n atomic.Int64
}

func (m *meterReader) Read(p []byte) (int, error) {
	k, err := m.r.Read(p)
	if k > 0 {
		m.n.Add(int64(k))
	}

	return k, err
}

// monitor polls the wire meter until done, returning the highest observed rate.
// Each tick it also reports progress via onProgress (uncompressed bytes sent so
// far and the current wire rate). If no bytes move for stallTimeout it cancels
// the request (cause errStalled) so a dead link doesn't hang the transfer.
//
// The stall check cannot be turned off through Options: newUploader substitutes
// 60s for a zero StallTimeout, and both CLI commands reject --stall-timeout 0
// outright rather than let it read as "disabled". The u.stallTimeout > 0 guard
// below only covers an uploader built by hand inside this package.
func (u *uploader) monitor(
	path string,
	m *meterReader,
	sent *atomic.Int64,
	cancel context.CancelCauseFunc,
	done <-chan struct{},
) float64 {
	const dt = 100 * time.Millisecond

	t := time.NewTicker(dt)
	defer t.Stop()

	var (
		last int64
		peak float64
	)

	// Rates divide by the time that actually elapsed since the previous sample,
	// never by the nominal tick interval: a late tick (loaded machine, blocked
	// callback, dropped ticks) would otherwise inflate the reported rate — and the
	// peak we print — by however late it was.
	lastSample := time.Now()
	lastMoved := lastSample

	for {
		select {
		case <-done:
			return peak
		case <-t.C:
			now := time.Now()
			cur := m.n.Load()
			bps := rate(cur-last, now.Sub(lastSample))
			lastSample = now

			if bps > peak {
				peak = bps
			}

			if u.onProgress != nil {
				u.onProgress(path, sent.Load(), bps)
			}

			if cur > last {
				last = cur
				lastMoved = now

				continue
			}

			if u.stallTimeout > 0 && now.Sub(lastMoved) >= u.stallTimeout {
				cancel(errStalled)

				return peak
			}
		}
	}
}

// hashPartOf returns the nix-base32 hash prefix of a store path's basename.
func hashPartOf(storePath string) string {
	base := filepath.Base(storePath)
	if len(base) > nixbase32.HashPartLen {
		return base[:nixbase32.HashPartLen]
	}

	return base
}

func rate(n int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}

	return float64(n) / d.Seconds()
}

// orDefault substitutes def for a non-positive v. Not cmp.Or: --jobs -1 reaches
// here from the flag, and a negative is as much "unset" as a zero.
func orDefault[T int | time.Duration](v, def T) T {
	if v <= 0 {
		return def
	}

	return v
}
