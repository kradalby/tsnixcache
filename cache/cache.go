// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package cache implements the HTTP handler for the tsnixcache binary cache server.
package cache

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"
	"tailscale.com/tsweb"
	_ "tailscale.com/tsweb/promvarz" // /debug/varz gathers from the default registry, which /metrics does not serve

	"github.com/kradalby/tsnixcache/auth"
	"github.com/kradalby/tsnixcache/nar"
	"github.com/kradalby/tsnixcache/narinfo"
	"github.com/kradalby/tsnixcache/nixcompress"
	"github.com/kradalby/tsnixcache/store"
)

const compressionZstd = "zstd"

// Request-body ceilings for pushes. narinfo is a few KB of text; a NAR can be
// large but a single upload past this is almost certainly abuse, not a real
// closure. Both are enforced with http.MaxBytesReader → 413.
const (
	maxNarInfoBytes = 1 << 20  // 1 MiB
	maxNarBytes     = 16 << 30 // 16 GiB
)

// importTimeout bounds a single verify+import. Generous: a multi-gigabyte
// closure on a slow disk is legitimate, and the only thing this guards against
// is a nix-store that never returns.
const importTimeout = 30 * time.Minute

// importWait bounds how long a push queues for an import slot. A server request
// context is cancelled only when the connection closes, so waiting on it alone
// means "block until the client gives up" — and the client only gives up after a
// 35-minute import deadline, having already transferred the whole NAR. A bounded
// wait turns that into a cheap 503 the client can back off from.
const importWait = 30 * time.Second

// pathCountTimeout bounds the store-path count behind /metrics and /health.
// PathCount is a full covering-index scan; without a deadline a wedged SQLite
// hangs every scrape and piles up goroutines and connections for as long as it
// lasts, rather than dropping one gauge.
const pathCountTimeout = 2 * time.Second

// ZstdCacheSubdir is the subdirectory of the spool dir holding compressed-NAR
// cache files. Kept separate from PUT-spooled NARs so cleanup never touches an
// in-flight upload.
const ZstdCacheSubdir = "zstd-cache"

// zstdCacheMaxBytes caps the compressed NARs kept on the spool filesystem. NAR
// reads are unauthenticated (auth exempts safe methods), so without a budget a
// tailnet peer walking hash parts makes the server write a compressed copy of
// the whole store here, and the idle TTL never catches up.
const zstdCacheMaxBytes = 4 << 30 // 4 GiB

// zstdWait bounds how long a compression queues for a slot before the server
// sheds it. Shedding is the better answer: a narinfo falls back to advertising
// the uncompressed variant, which costs the client bandwidth, where queueing
// behind an unauthenticated flood costs it the request.
const zstdWait = 10 * time.Second

// errZstdBusy is returned when no compression slot came free in zstdWait.
var errZstdBusy = errors.New("cache: zstd compression queue full")

// Version is what GET /version reports.
var Version = "dev"

// StoreProvider abstracts the store for testability.
type StoreProvider interface {
	PathInfo(ctx context.Context, hashPart string) (*store.PathInfo, error)
	PathCount(ctx context.Context) (int64, error)
}

// ImporterFunc is a function that imports a narinfo into the nix store.
type ImporterFunc func(ctx context.Context, ni *narinfo.NarInfo) error

// Signer is a minimal signing interface satisfied by *signing.SecretKey.
type Signer interface {
	Sign(fingerprint string) string
}

// Config describes a Server. It is a struct rather than a parameter list
// because four of its fields are consecutive directory-ish strings: passed
// positionally, swapping SpoolDir and StoreDir type-checks and silently spools
// uploads into the store.
type Config struct {
	Store    StoreProvider
	Signer   Signer
	Priority int
	SpoolDir string
	// ServeCompression is "none" or "zstd".
	ServeCompression string
	// StoreDir is the logical store dir, as recorded in the DB and reported in
	// nix-cache-info: always /nix/store, even when serving a chroot store.
	StoreDir string
	// StoreRoot is the physical prefix prepended to logical store paths when
	// reading NARs. Empty for the system store (logical == physical); set to the
	// chroot prefix when serving a separate store.
	StoreRoot string
	ImportFn  ImporterFunc
	// ImportConcurrency bounds how many imports run at once; <=0 means unlimited.
	ImportConcurrency int
	// MaxNarSize bounds decoded NAR bytes; zero uses the request-size default.
	MaxNarSize uint64
}

// Server is the cache HTTP server. Every field is read by concurrent request
// handlers, so all of them are set once by New and none are exported.
type Server struct {
	store            StoreProvider
	priority         int
	spoolDir         string
	serveCompression string
	signer           Signer
	importFn         ImporterFunc
	maxNarSize       uint64

	importSem chan struct{}

	registry *prometheus.Registry
	storeDir string // physical store dir, for statfs/disk metrics
	// storeDirLogical is the store dir as recorded in the DB and reported in
	// nix-cache-info (always /nix/store, even when serving a chroot store).
	storeDirLogical string
	// storeRoot is the physical prefix prepended to logical store paths when
	// reading NARs. Empty for the system store (logical == physical); set to
	// the chroot prefix when serving a separate store.
	storeRoot string

	// metrics
	narInfoHits      *prometheus.CounterVec
	narInfoMisses    *prometheus.CounterVec
	narBytesServed   prometheus.Counter
	narBytesReceived prometheus.Counter
	pushNAR          prometheus.Counter
	pushNarInfo      prometheus.Counter
	pushErrors       *prometheus.CounterVec
	importSuccess    prometheus.Counter
	importFail       prometheus.Counter
	importDuration   prometheus.Histogram

	zstdCacheDir    string
	zstdCacheMu     sync.Mutex
	zstdCache       map[string]*zstdEntry
	zstdCacheBytes  uint64
	zstdCacheLimit  uint64
	zstdLRU         list.List
	zstdFailed      map[string]*zstdEntry
	zstdFlights     map[string]*zstdFlight
	zstdGroup       errgroup.Group
	zstdLeases      sync.WaitGroup
	zstdRequests    sync.WaitGroup
	zstdCtx         context.Context //nolint:containedctx // Shared builds belong to the server lifetime.
	zstdCancel      context.CancelFunc
	zstdSem         chan struct{}
	zstdOwner       *os.File
	zstdClosed      bool
	zstdCloseDone   chan struct{}
	zstdCloseErr    error
	narWriteTimeout time.Duration

	startTime time.Time
}

// New creates a new Server with its own prometheus registry.
func New(cfg Config) (*Server, error) {
	reg := prometheus.NewRegistry()

	// Physical store dir for statfs is the logical store dir under the root.
	physStoreDir := filepath.Join(cfg.StoreRoot, cfg.StoreDir)

	// Compression is bounded even when imports are not: ImportConcurrency of 0
	// is an operator saying "never queue my own pushes", not an invitation for
	// unauthenticated readers to start a compression per connection.
	zstdSlots := cfg.ImportConcurrency
	if zstdSlots <= 0 {
		zstdSlots = runtime.NumCPU()
	}

	srv := &Server{
		store:            cfg.Store,
		priority:         cfg.Priority,
		spoolDir:         cfg.SpoolDir,
		serveCompression: cfg.ServeCompression,
		signer:           cfg.Signer,
		importFn:         cfg.ImportFn,
		maxNarSize:       cfg.MaxNarSize,
		registry:         reg,
		storeDir:         physStoreDir,
		storeDirLogical:  cfg.StoreDir,
		storeRoot:        cfg.StoreRoot,
		zstdCacheDir:     filepath.Join(cfg.SpoolDir, ZstdCacheSubdir),
		zstdCache:        make(map[string]*zstdEntry),
		zstdSem:          make(chan struct{}, zstdSlots),
		zstdCacheLimit:   zstdCacheMaxBytes,
		zstdFlights:      make(map[string]*zstdFlight),
		zstdFailed:       make(map[string]*zstdEntry),
		zstdCloseDone:    make(chan struct{}),
		narWriteTimeout:  60 * time.Second,
		startTime:        time.Now(),
	}

	if cfg.ImportConcurrency > 0 {
		srv.importSem = make(chan struct{}, cfg.ImportConcurrency)
	}

	if srv.maxNarSize == 0 {
		srv.maxNarSize = maxNarBytes
	}

	err := srv.initZstdCache()
	if err != nil {
		return nil, err
	}

	srv.SweepSpool(0)

	srv.registerMetrics(reg, physStoreDir, cfg)

	return srv, nil
}

// storeCollector is a prometheus.Collector that exposes disk usage for the Nix
// store and spool filesystems and the total number of registered store paths.
// The spool is often a separate filesystem, and it filling up is the failure the
// whole sweeper design guards against.
type storeCollector struct {
	storeDir   string
	spoolDir   string
	store      StoreProvider
	diskTotal  *prometheus.Desc
	diskUsed   *prometheus.Desc
	diskAvail  *prometheus.Desc
	spoolTotal *prometheus.Desc
	spoolUsed  *prometheus.Desc
	spoolAvail *prometheus.Desc
	paths      *prometheus.Desc
}

func newStoreCollector(storeDir, spoolDir string, s StoreProvider) *storeCollector {
	return &storeCollector{
		storeDir: storeDir,
		spoolDir: spoolDir,
		store:    s,
		diskTotal: prometheus.NewDesc(
			"tsnixcache_store_disk_total_bytes",
			"Total size in bytes of the filesystem containing the Nix store.",
			nil, nil,
		),
		diskUsed: prometheus.NewDesc(
			"tsnixcache_store_disk_used_bytes",
			"Used bytes on the filesystem containing the Nix store.",
			nil, nil,
		),
		diskAvail: prometheus.NewDesc(
			"tsnixcache_store_disk_available_bytes",
			"Available bytes on the filesystem containing the Nix store.",
			nil, nil,
		),
		spoolTotal: prometheus.NewDesc(
			"tsnixcache_spool_disk_total_bytes",
			"Total size in bytes of the filesystem containing the upload spool.",
			nil, nil,
		),
		spoolUsed: prometheus.NewDesc(
			"tsnixcache_spool_disk_used_bytes",
			"Used bytes on the filesystem containing the upload spool.",
			nil, nil,
		),
		spoolAvail: prometheus.NewDesc(
			"tsnixcache_spool_disk_available_bytes",
			"Available bytes on the filesystem containing the upload spool.",
			nil, nil,
		),
		// A gauge that falls after GC, so not a _total.
		paths: prometheus.NewDesc(
			"tsnixcache_store_paths",
			"Number of store paths registered in the Nix database.",
			nil, nil,
		),
	}
}

func (c *storeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.diskTotal

	ch <- c.diskUsed

	ch <- c.diskAvail

	ch <- c.spoolTotal

	ch <- c.spoolUsed

	ch <- c.spoolAvail

	ch <- c.paths
}

func (c *storeCollector) Collect(ch chan<- prometheus.Metric) {
	collectDisk(ch, c.storeDir, c.diskTotal, c.diskUsed, c.diskAvail)
	collectDisk(ch, c.spoolDir, c.spoolTotal, c.spoolUsed, c.spoolAvail)

	// Gatherer.Gather takes no context, so Background is the only base — but it
	// needs a deadline all the same, see pathCountTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), pathCountTimeout)
	defer cancel()

	n, pathErr := c.store.PathCount(ctx)
	if pathErr == nil {
		ch <- prometheus.MustNewConstMetric(c.paths, prometheus.GaugeValue, float64(n))
	}
}

// collectDisk emits total/used/available gauges for the filesystem holding dir,
// or nothing at all if it cannot be statfs'd.
func collectDisk(ch chan<- prometheus.Metric, dir string, total, used, avail *prometheus.Desc) {
	var st syscall.Statfs_t

	err := syscall.Statfs(dir, &st)
	if err != nil {
		return
	}

	bsize := uint64(st.Bsize) // #nosec G115 -- block size is always positive

	ch <- prometheus.MustNewConstMetric(total, prometheus.GaugeValue, float64(st.Blocks*bsize))

	ch <- prometheus.MustNewConstMetric(used, prometheus.GaugeValue, float64((st.Blocks-st.Bfree)*bsize))

	ch <- prometheus.MustNewConstMetric(avail, prometheus.GaugeValue, float64(st.Bavail*bsize))
}

// availBytes returns free bytes on the filesystem holding dir, 0 when unknown.
func availBytes(dir string) uint64 {
	var st syscall.Statfs_t

	err := syscall.Statfs(dir, &st)
	if err != nil {
		return 0
	}

	return st.Bavail * uint64(st.Bsize) // #nosec G115 -- block size is always positive
}

// Registry returns the server's prometheus registry, the one served at
// /metrics. Callers that own their own collectors — the GC watcher, say —
// register them here so they land on the same endpoint.
func (srv *Server) Registry() *prometheus.Registry {
	return srv.registry
}

// Handler returns the HTTP handler for the cache server.
func (srv *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /nix-cache-info", srv.handleNixCacheInfo)
	mux.HandleFunc("HEAD /nix-cache-info", srv.handleNixCacheInfo)

	// /nar/{name} routes — registered before the catch-all below.
	mux.HandleFunc("GET /nar/", srv.handleGetNar)
	mux.HandleFunc("HEAD /nar/", srv.handleHeadNar)
	mux.HandleFunc("PUT /nar/", srv.handlePutNar)

	mux.Handle("GET /metrics", promhttp.HandlerFor(srv.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /health", srv.handleHealth)
	mux.HandleFunc("GET /version", srv.handleVersion)

	// tsweb debug surface at /debug/ (pprof, expvar, gc, varz), gated to tailnet
	// peers + loopback by AllowDebugAccess; /metrics is linked from the index.
	dbg := tsweb.Debugger(mux)
	dbg.URL("/metrics", "Metrics (Prometheus)")

	// Catch-all: dispatch .narinfo and / routes.
	mux.HandleFunc("/", srv.handleRoot)

	return loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HTTP/1 otherwise drains an unread request body before flushing the response.
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.ContentLength != 0 {
			w.Header().Set("Connection", "close")

			err := http.NewResponseController(w).SetReadDeadline(time.Now())
			if err != nil {
				panic(http.ErrAbortHandler)
			}

			http.Error(w, "read requests must not have a body", http.StatusBadRequest)

			return
		}

		mux.ServeHTTP(w, r)
	}))
}

// SweepSpool removes spooled NAR uploads last written at least maxIdle ago,
// returning the number removed. PUT /nar/{name} spools the body to disk and only
// a fully successful import deletes it, so a client that uploads NARs and never
// sends a matching narinfo (or whose narinfo fails verification) would otherwise
// fill the spool filesystem for the life of the process.
//
// Writing the body updates the file's mtime, so an upload that is making
// progress always looks fresh; one that stalls is cancelled client-side after
// --stall-timeout (60s by default). A maxIdle of hours is therefore far past the
// point where a spool file can still belong to a live upload — pick one, not
// minutes. A maxIdle of 0 removes everything, which is what startup wants.
// Subdirectories, including ZstdCacheSubdir which SweepZstdCache owns, are left
// alone.
func (srv *Server) SweepSpool(maxIdle time.Duration) int {
	ents, err := os.ReadDir(srv.spoolDir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("spool sweep: read dir", "dir", srv.spoolDir, "err", err)
		}

		return 0
	}

	cutoff := time.Now().Add(-maxIdle)
	removed := 0

	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}

		fi, infoErr := ent.Info()
		if infoErr != nil || fi.ModTime().After(cutoff) {
			continue
		}

		path := filepath.Join(srv.spoolDir, ent.Name())

		rmErr := os.Remove(path)
		if rmErr != nil {
			slog.Warn("spool sweep: remove", "file", path, "err", rmErr)

			continue
		}

		slog.Info("spool sweep: removed abandoned upload", "file", path, "size", fi.Size())

		removed++
	}

	return removed
}

// registerMetrics builds and registers everything /metrics serves.
func (srv *Server) registerMetrics(reg *prometheus.Registry, physStoreDir string, cfg Config) {
	// A push HEADs every path in a closure to decide what to upload, so a
	// healthy push of mostly-new paths is thousands of misses. Labelling by
	// method keeps that out of the substituter hit ratio.
	srv.narInfoHits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tsnixcache_narinfo_hits_total",
		Help: "narinfo requests that found a path in the store, by HTTP method.",
	}, []string{"method"})
	srv.narInfoMisses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tsnixcache_narinfo_misses_total",
		Help: "narinfo requests that did not find a path in the store, by HTTP method.",
	}, []string{"method"})
	srv.narBytesServed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsnixcache_nar_bytes_served_total",
		// Wire bytes, not NAR bytes: under --serve-compression zstd this counts
		// the compressed body, so it is what left the link rather than what the
		// client reconstructed. Named for the endpoint, measured at the socket.
		Help: "Total bytes of NAR data served (compressed body when serving zstd).",
	})
	srv.narBytesReceived = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsnixcache_nar_bytes_received_total",
		Help: "Total bytes of NAR data received via PUT, whether or not the upload completed.",
	})
	srv.pushNAR = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsnixcache_push_nar_total",
		Help: "Number of NAR files received via PUT.",
	})
	srv.pushNarInfo = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsnixcache_push_narinfo_total",
		Help: "Number of narinfo files received via PUT.",
	})
	// Without this a push outage makes every push line fall towards zero, which
	// looks exactly like "nobody is pushing".
	srv.pushErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tsnixcache_push_errors_total",
		Help: "Pushes rejected or abandoned before they could be imported, by reason.",
	}, []string{"reason"})
	srv.importSuccess = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsnixcache_import_success_total",
		Help: "Number of successful nix-store imports.",
	})
	srv.importFail = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsnixcache_import_fail_total",
		Help: "Imports that failed, including pushes shed because no import slot came free.",
	})
	// Import runs synchronously inside the narinfo PUT and dominates push
	// latency, so its distribution has to reach the whole importTimeout.
	srv.importDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "tsnixcache_import_duration_seconds",
		Help:    "Time spent in verify+import of a pushed path.",
		Buckets: prometheus.ExponentialBuckets(0.5, 2, 13), // 0.5s … ~2048s
	})

	// Create every child up front: a counter that has never fired must still be
	// scrapeable as 0, or a dashboard shows "no data" for a healthy server.
	for _, m := range []string{"get", "head"} {
		srv.narInfoHits.WithLabelValues(m)
		srv.narInfoMisses.WithLabelValues(m)
	}

	for _, reason := range []string{"nar_body", "narinfo_body", "narinfo_parse", "narinfo_size", "busy"} {
		srv.pushErrors.WithLabelValues(reason)
	}

	reg.MustRegister(
		srv.narInfoHits,
		srv.narInfoMisses,
		srv.narBytesServed,
		srv.narBytesReceived,
		srv.pushNAR,
		srv.pushNarInfo,
		srv.pushErrors,
		srv.importSuccess,
		srv.importFail,
		srv.importDuration,
		newStoreCollector(physStoreDir, cfg.SpoolDir, cfg.Store),
		// The push-grant middleware lives in package auth, but this registry is
		// the one served at /metrics, so its counter has to be registered here
		// or a rejected push is invisible to every scrape.
		auth.Collector(),
		// A fresh registry starts empty, and /debug/varz gathers from the
		// default one instead — so without these the process that streams
		// multi-GB NARs through in-process decoders and forks nix-store exports
		// no resident-memory, goroutine or open-fd telemetry at all.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// handleNixCacheInfo serves the /nix-cache-info endpoint.
func (srv *Server) handleNixCacheInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "StoreDir: %s\nWantMassQuery: 1\nPriority: %d\n", srv.storeDirLogical, srv.priority)
}

// narInfoHash extracts the hash part from a /{hash}.narinfo URL path.
// Returns ("", false) if the path doesn't match.
func narInfoHash(path string) (string, bool) {
	// path is like /abc123.narinfo
	base := strings.TrimPrefix(path, "/")

	hash, ok := strings.CutSuffix(base, ".narinfo")
	if !ok || hash == "" {
		return "", false
	}

	return hash, true
}

// handleGetNarInfo serves GET /{hash}.narinfo.
func (srv *Server) handleGetNarInfo(w http.ResponseWriter, r *http.Request) {
	hash, ok := narInfoHash(r.URL.Path)
	if !ok {
		http.NotFound(w, r)

		return
	}

	ni, err := srv.buildNarInfo(r.Context(), hash, true)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			srv.narInfoMisses.WithLabelValues("get").Inc()
			http.NotFound(w, r)

			return
		}

		slog.Error("narinfo", "hash", hash, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	srv.narInfoHits.WithLabelValues("get").Inc()

	body := ni.Marshal()

	w.Header().Set("Content-Type", "text/x-nix-narinfo")
	_, _ = io.WriteString(w, body) // #nosec G705 -- body is generated from trusted NarInfo data
}

// handleHeadNarInfo serves HEAD /{hash}.narinfo.
func (srv *Server) handleHeadNarInfo(w http.ResponseWriter, r *http.Request) {
	hash, ok := narInfoHash(r.URL.Path)
	if !ok {
		http.NotFound(w, r)

		return
	}

	// A HEAD only answers "do you have this path" — `tsnixcache push` HEADs every
	// path in a closure to decide what to upload, so compressing here would make
	// a no-op push do the compression work of the whole closure. For the same
	// reason its hit/miss counts are labelled separately from a GET's: a healthy
	// push of new paths is thousands of misses.
	ni, err := srv.buildNarInfo(r.Context(), hash, false)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			srv.narInfoMisses.WithLabelValues("head").Inc()
			http.NotFound(w, r)

			return
		}

		slog.Error("narinfo head", "hash", hash, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	srv.narInfoHits.WithLabelValues("head").Inc()

	body := ni.Marshal()

	w.Header().Set("Content-Type", "text/x-nix-narinfo")

	// Content-Length must describe the body a GET of this URL would send. In zstd
	// mode that is a different document — a .nar.zstd URL with the compressed
	// FileHash and FileSize — and computing its length means compressing the NAR,
	// which is the one thing a HEAD must not do. Omit the header there rather than
	// advertise the uncompressed variant's length for it.
	if srv.serveCompression != compressionZstd {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}

	w.WriteHeader(http.StatusOK)
}

// narSize clamps a NarSize from the store. It is an int64 there, and a negative
// value would wrap to ~1.8e19 in the narinfo, which no client can reconcile with
// the bytes it actually receives.
func narSize(n int64) uint64 {
	if n < 0 {
		return 0
	}

	return uint64(n)
}

// buildNarInfo constructs a NarInfo from the store for the given hash part.
// With compress false the NAR is never compressed: the returned narinfo
// describes the uncompressed variant even in zstd mode. Only callers that will
// actually hand the body to a client that may fetch the NAR should pass true.
func (srv *Server) buildNarInfo(ctx context.Context, hashPart string, compress bool) (*narinfo.NarInfo, error) {
	pi, err := srv.store.PathInfo(ctx, hashPart)
	if err != nil {
		return nil, err
	}

	var (
		url         string
		compression string
		fileHash    string
		fileSize    uint64
	)

	if compress && srv.serveCompression == compressionZstd {
		// Compress NAR to a temp file to get FileHash/FileSize.
		_, compHash, compSize, cerr := srv.getOrCreateZstdCache(ctx, pi)
		if cerr != nil {
			slog.Warn("narinfo: zstd compress failed, falling back to none",
				"path", pi.StorePath, "err", cerr)

			// Fall back to uncompressed.
			// Use store hash in URL so serveNar can look up the path.
			url = "nar/" + hashPart + ".nar"
			compression = "none"
			fileHash = pi.NarHash
			fileSize = narSize(pi.NarSize)
		} else {
			url = "nar/" + hashPart + ".nar.zstd"
			compression = compressionZstd
			fileHash = "sha256:" + compHash
			fileSize = compSize
		}
	} else {
		// Use store hash in URL so serveNar can look up the path.
		url = "nar/" + hashPart + ".nar"
		compression = "none"
		fileHash = pi.NarHash
		fileSize = narSize(pi.NarSize)
	}

	refs := make([]string, len(pi.References))
	copy(refs, pi.References)

	ni := &narinfo.NarInfo{
		StorePath:   pi.StorePath,
		URL:         url,
		Compression: compression,
		FileHash:    fileHash,
		FileSize:    fileSize,
		NarHash:     pi.NarHash,
		NarSize:     narSize(pi.NarSize),
		References:  refs,
		Deriver:     pi.Deriver,
		Sigs:        append([]string(nil), pi.Sigs...),
		CA:          pi.CA,
	}

	if srv.signer != nil {
		sig := srv.signer.Sign(ni.Fingerprint())
		ni.Sigs = append(ni.Sigs, sig)
	}

	return ni, nil
}

// handleGetNar serves GET /nar/{name}.
func (srv *Server) handleGetNar(w http.ResponseWriter, r *http.Request) {
	srv.serveNar(w, r, false)
}

// handleHeadNar serves HEAD /nar/{name}.
func (srv *Server) handleHeadNar(w http.ResponseWriter, r *http.Request) {
	srv.serveNar(w, r, true)
}

func (srv *Server) serveNar(w http.ResponseWriter, r *http.Request, headOnly bool) {
	// Path is /nar/<name>
	name := strings.TrimPrefix(r.URL.Path, "/nar/")

	// Extract the hash prefix (everything up to first '.').
	hashPart, _, _ := strings.Cut(name, ".")
	if hashPart == "" {
		http.Error(w, "invalid NAR name", http.StatusBadRequest)

		return
	}

	isZstd := strings.HasSuffix(name, ".zstd")

	// A client replaying a narinfo it cached before the server was restarted with
	// --serve-compression none still asks for .nar.zstd. Serving the raw NAR
	// under that name is a 200 the client cannot decode; a 404 makes it re-fetch
	// the narinfo instead.
	if isZstd && srv.serveCompression != compressionZstd {
		http.NotFound(w, r)

		return
	}

	// Also support ?hash= query param as the store hash (not the file hash).
	// In the simple case, the URL file hash == NarHash part == hashPart.
	// Callers may also pass ?hash=<storehash> to route by store hash.
	queryHash := r.URL.Query().Get("hash")

	lookupHash := hashPart
	if queryHash != "" {
		lookupHash = queryHash
	}

	pi, err := srv.store.PathInfo(r.Context(), lookupHash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)

			return
		}

		slog.Error("nar: store lookup", "name", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/x-nix-nar")

	if isZstd {
		// A HEAD must not compress, and the compressed length is unknowable
		// without doing so, so it goes out without one.
		if headOnly {
			w.WriteHeader(http.StatusOK)

			return
		}

		srv.serveZstdNar(r.Context(), w, pi)

		return
	}

	// Declaring the length is what lets net/http skip chunked framing, and gives
	// the client a progress denominator on a multi-gigabyte NAR. A short body
	// against a declared length still errors the client, so abortNar below keeps
	// its meaning.
	if pi.NarSize > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(pi.NarSize, 10))
	}

	if headOnly {
		w.WriteHeader(http.StatusOK)

		return
	}

	// Serve uncompressed NAR.
	cw := &countWriter{w: &narDeadlineWriter{w: w, timeout: srv.narWriteTimeout}}

	err = nar.Write(cw, filepath.Join(srv.storeRoot, pi.StorePath))

	srv.narBytesServed.Add(float64(cw.n))

	if err != nil {
		abortNar("nar: write", pi, err)
	}
}

// abortNar kills the connection under an in-flight NAR response. By the time NAR
// generation fails the 200 is already on the wire, so the only honest signal left
// is a broken transport: net/http turns ErrAbortHandler into a closed connection
// with no terminating chunk, so the client errors instead of trusting a NAR that
// is well-formed HTTP but silently short.
func abortNar(msg string, pi *store.PathInfo, err error) {
	slog.Error(msg, "path", pi.StorePath, "err", err)
	panic(http.ErrAbortHandler)
}

// serveZstdNar serves the NAR for pi compressed with zstd, from the shared cache
// file. Compressing straight to the client instead would start faster, but it
// built a fresh encoder per request — unbounded, on an unauthenticated path —
// and left the cache cold, so every repeat GET paid for it again. Going through
// the cache makes the first request pay once, under singleflight and the
// compression budget, and every one after it a file copy.
func (srv *Server) serveZstdNar(ctx context.Context, w http.ResponseWriter, pi *store.PathInfo) {
	lease, err := srv.acquireZstd(ctx, pi)
	if err != nil {
		if errors.Is(err, errZstdBusy) || errors.Is(err, errZstdFull) {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "server busy", http.StatusServiceUnavailable)

			return
		}

		slog.Error("nar: zstd cache", "path", pi.StorePath, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}
	defer lease.Close()

	w.Header().Set("Content-Length", strconv.FormatUint(lease.entry.size, 10))
	cw := &countWriter{w: &narDeadlineWriter{w: w, timeout: srv.narWriteTimeout}}
	_, err = io.Copy(cw, lease.file)
	srv.narBytesServed.Add(float64(cw.n))

	if err != nil {
		abortNar("nar: serve zstd cached", pi, err)
	}
}

// countWriter counts bytes written. It takes an io.Writer because that is all it
// uses, and forwards ReadFrom so a file copy underneath it can still reach
// sendfile instead of bouncing through a userspace buffer.
type countWriter struct {
	w io.Writer
	n int64
}

func (cw *countWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)

	return n, err
}

func (cw *countWriter) ReadFrom(r io.Reader) (int64, error) {
	n, err := readFrom(cw.w, r)
	cw.n += n

	return n, err
}

// onlyWriter hides everything but Write, so io.Copy cannot find a ReadFrom and
// recurse back into the wrapper that called it.
type onlyWriter struct {
	io.Writer
}

// readFrom copies r into w through w's own ReadFrom when it has one.
func readFrom(w io.Writer, r io.Reader) (int64, error) {
	if rf, ok := w.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}

	return io.Copy(onlyWriter{w}, r)
}

// closeSpool closes a spooled NAR. It is a variable purely so a test can make
// the close fail: the errors this close exists to catch — ENOSPC and EIO under
// delayed allocation — cannot be provoked on a real file by any portable means,
// and an unguarded data-loss check is one refactor from disappearing silently.
// Production always calls (*os.File).Close.
var closeSpool = (*os.File).Close

// handlePutNar handles PUT /nar/{name}: spool body to spoolDir.
func (srv *Server) handlePutNar(w http.ResponseWriter, r *http.Request) {
	// filepath.Base collapses any path away, so the spooled file can only ever
	// land directly in SpoolDir regardless of what the client sends.
	name := filepath.Base(strings.TrimPrefix(r.URL.Path, "/nar/"))
	if name == "." || name == ".." || name == "/" {
		http.Error(w, "invalid NAR name", http.StatusBadRequest)

		return
	}

	dest := filepath.Join(srv.spoolDir, name)

	err := os.MkdirAll(srv.spoolDir, 0o750)
	if err != nil {
		slog.Error("put nar: mkdir spoolDir", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	// Write to a private temp file and rename over dest. The name is derived from
	// the store path, so two pushes of the same closure — the post-build hook and
	// watch, or any two builders — race on it: writing in place truncated and
	// spliced them together, and the loser's error path unlinked the winner's
	// finished file. Rename is atomic and intra-directory, so there is no EXDEV
	// risk and no window in which dest is half a NAR.
	f, err := os.CreateTemp(srv.spoolDir, name+".*")
	if err != nil {
		slog.Error("put nar: create spool file", "dest", dest, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	tmpName := f.Name()

	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, maxNarBytes))

	// The link carried these bytes whether or not the upload finished, and an
	// upload that dies at 15 of 16 GiB is exactly when an operator wants to see
	// them.
	srv.narBytesReceived.Add(float64(n))

	if err != nil {
		f.Close()          // #nosec G104 -- cleanup in error path
		os.Remove(tmpName) // #nosec G104 -- drop the partial upload; best-effort
		srv.pushErrors.WithLabelValues("nar_body").Inc()
		srv.writeBodyError(w, "put nar: copy body", err, "dest", dest)

		return
	}

	// Close before committing to a 200. With delayed allocation, ENOSPC and EIO
	// surface at close(2), and a 200 over a short spool file reaches the client
	// as a NarHash mismatch on the narinfo PUT — indistinguishable from
	// corruption, and re-uploaded in full to fail the same way.
	err = closeSpool(f)
	if err != nil {
		os.Remove(tmpName) // #nosec G104 -- cleanup in error path
		srv.pushErrors.WithLabelValues("nar_body").Inc()
		slog.Error("put nar: close spool file", "dest", dest, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	err = os.Rename(tmpName, dest)
	if err != nil {
		os.Remove(tmpName) // #nosec G104 -- cleanup in error path
		srv.pushErrors.WithLabelValues("nar_body").Inc()
		slog.Error("put nar: rename spool file", "dest", dest, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	srv.pushNAR.Add(1)
	w.WriteHeader(http.StatusOK)
}

// writeBodyError logs a failed request-body read and replies 413 if the body
// exceeded its limit, else 500. attrs are extra slog key/value pairs naming the
// path the failure belongs to, without which the log says only that some upload
// broke.
func (srv *Server) writeBodyError(w http.ResponseWriter, msg string, err error, attrs ...any) {
	if maxErr, ok := errors.AsType[*http.MaxBytesError](err); ok {
		// An oversized upload is the client's fault rather than ours, so warn
		// rather than error — but it still has to leave a trace. The client only
		// learns "413", and without this line the operator it complains to has
		// nothing at all to look at. It cannot be used to flood the log either:
		// one record costs the client maxErr.Limit bytes on the wire.
		slog.Warn(msg, append(attrs, "limit", maxErr.Limit, "err", err)...)
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)

		return
	}

	slog.Error(msg, append(attrs, "err", err)...)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// handlePutNarInfo handles PUT /{hash}.narinfo: parse and trigger import.
func (srv *Server) handlePutNarInfo(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxNarInfoBytes))
	if err != nil {
		srv.pushErrors.WithLabelValues("narinfo_body").Inc()
		srv.writeBodyError(w, "put narinfo: read body", err, "url", r.URL.Path)

		return
	}

	ni, err := narinfo.Parse(bytes.NewReader(body), srv.storeDirLogical)
	if err != nil {
		srv.pushErrors.WithLabelValues("narinfo_parse").Inc()
		http.Error(w, "parse error", http.StatusBadRequest)

		return
	}

	if ni.NarSize > srv.maxNarSize || ni.NarSize >= math.MaxInt64 {
		srv.pushErrors.WithLabelValues("narinfo_size").Inc()
		http.Error(w, "decoded NAR exceeds size limit", http.StatusRequestEntityTooLarge)

		return
	}

	// Reject a codec we cannot decode here rather than 500 out of the import: a
	// 500 is retried, so the client re-uploads the whole NAR five times to learn
	// something the first request already knew, and the spool file lingers until
	// the sweeper's TTL.
	if !nixcompress.Supported(ni.Compression) {
		srv.pushErrors.WithLabelValues("narinfo_parse").Inc()
		slog.Warn("put narinfo: unsupported compression",
			"path", ni.StorePath, "compression", ni.Compression)
		http.Error(w, "unsupported compression: "+ni.Compression, http.StatusBadRequest)

		return
	}

	srv.pushNarInfo.Add(1)

	if srv.importFn != nil {
		if !srv.acquireImportSlot(w, r) {
			return
		}

		defer srv.releaseImportSlot()

		// Run the import detached from the request context. The client puts a
		// stall deadline on this PUT (upload/upload.go), and verify+import of a
		// large closure can outlive it; a cancellation arriving here kills
		// nix-store mid-import, so the transfer is wasted and the client's retry
		// starts from nothing. Importing is idempotent, so finishing the work a
		// disconnected client asked for is always better than abandoning it
		// half-way. The timeout is the backstop that keeps a wedged nix-store
		// from holding an import slot forever.
		importCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), importTimeout)

		start := time.Now()
		err = srv.importFn(importCtx, ni)

		cancel()
		srv.importDuration.Observe(time.Since(start).Seconds())

		if err != nil {
			slog.Error("import", "path", ni.StorePath, "err", err)
			srv.importFail.Add(1)
			http.Error(w, "import failed", http.StatusInternalServerError)

			return
		}

		srv.importSuccess.Add(1)
	}

	w.WriteHeader(http.StatusOK)
}

// acquireImportSlot takes an import slot, bounding concurrent imports so a burst
// of pushes can't exhaust memory or pile up nix-store processes contending on
// the store lock. It reports whether the caller may proceed, having already
// answered the client if not.
func (srv *Server) acquireImportSlot(w http.ResponseWriter, r *http.Request) bool {
	if srv.importSem == nil {
		return true
	}

	timer := time.NewTimer(importWait)
	defer timer.Stop()

	select {
	case srv.importSem <- struct{}{}:
		return true
	case <-timer.C:
		// A real answer a live client can act on: it still holds the NAR, so
		// backing off and retrying costs it nothing.
		srv.pushErrors.WithLabelValues("busy").Inc()
		srv.importFail.Add(1)
		w.Header().Set("Retry-After", strconv.Itoa(int(importWait.Seconds())))
		http.Error(w, "server busy", http.StatusServiceUnavailable)

		return false
	case <-r.Context().Done():
		// The connection is already gone; there is nobody left to answer.
		srv.pushErrors.WithLabelValues("busy").Inc()

		return false
	}
}

func (srv *Server) releaseImportSlot() {
	if srv.importSem != nil {
		<-srv.importSem
	}
}

// healthResponse is the JSON body for /health.
type healthResponse struct {
	Status string `json:"status"`
	// StoreReachable is one probe, not two: the DB read is how the store is
	// reached, so reporting it twice could only ever say the same thing.
	StoreReachable      bool   `json:"store_reachable"`
	UptimeSeconds       int64  `json:"uptime_seconds"`
	DiskAvailableBytes  uint64 `json:"disk_available_bytes,omitempty"`
	SpoolAvailableBytes uint64 `json:"spool_available_bytes,omitempty"`
	StorePathsTotal     int64  `json:"store_paths_total,omitempty"`
}

// handleHealth serves GET /health.
func (srv *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Probe with a real query. A PathInfo lookup is no good here: it rejects
	// anything that isn't a valid hash part before it reaches the database, so
	// a sentinel probe path would report the store healthy while the DB was
	// unreadable. PathCount always touches the DB.
	ctx, cancel := context.WithTimeout(r.Context(), pathCountTimeout)
	defer cancel()

	paths, err := srv.store.PathCount(ctx)
	storeReachable := err == nil

	status := "ok"
	if !storeReachable {
		status = "degraded"
	}

	resp := healthResponse{
		Status:         status,
		StoreReachable: storeReachable,
		UptimeSeconds:  int64(time.Since(srv.startTime).Seconds()),
		// The spool is often a separate filesystem, and it filling up breaks
		// every push while the store looks perfectly healthy.
		DiskAvailableBytes:  availBytes(srv.storeDir),
		SpoolAvailableBytes: availBytes(srv.spoolDir),
		StorePathsTotal:     paths,
	}

	w.Header().Set("Content-Type", "application/json")

	if !storeReachable {
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	encErr := json.NewEncoder(w).Encode(resp)
	if encErr != nil {
		slog.Error("health: encode", "err", encErr)
	}
}

// handleVersion serves GET /version.
func (srv *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintln(w, Version)
}

// handleRoot is the catch-all that dispatches .narinfo routes and serves /.
func (srv *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if _, ok := narInfoHash(r.URL.Path); ok {
		switch r.Method {
		case http.MethodGet:
			srv.handleGetNarInfo(w, r)
		case http.MethodHead:
			srv.handleHeadNarInfo(w, r)
		case http.MethodPut:
			srv.handlePutNarInfo(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

		return
	}

	if r.URL.Path != "/" {
		http.NotFound(w, r)

		return
	}

	w.WriteHeader(http.StatusOK)
}

// statusWriter captures the HTTP status and bytes written for logging. It
// forwards Unwrap and ReadFrom: without them http.ResponseController cannot
// recover Flush or the write deadlines from the writer underneath, and a file
// copy never reaches sendfile.
type statusWriter struct {
	http.ResponseWriter

	status int
	bytes  int64
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	n, err := sw.ResponseWriter.Write(b)
	sw.bytes += int64(n)

	return n, err
}

func (sw *statusWriter) Unwrap() http.ResponseWriter {
	return sw.ResponseWriter
}

func (sw *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	n, err := readFrom(sw.ResponseWriter, r)
	sw.bytes += n

	return n, err
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}

		// Deferred so an aborted NAR still leaves an access-log line: abortNar
		// panics with http.ErrAbortHandler, which unwinds straight past a plain
		// call here.
		defer func() {
			slog.Info(
				"http",
				"method", r.Method,
				// The path is whatever the peer sent, and logging it whole let a
				// 900 kB request write a 900 kB record, needing no grant.
				"path", auth.LogPath(r.URL.Path),
				"status", sw.status,
				"bytes", sw.bytes,
				"dur", time.Since(start),
			)
		}()

		next.ServeHTTP(sw, r)
	})
}
