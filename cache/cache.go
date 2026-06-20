// Package cache implements the HTTP handler for the tsnixcache binary cache server.
package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kradalby/tsnixcache/nar"
	"github.com/kradalby/tsnixcache/narinfo"
	"github.com/kradalby/tsnixcache/nixbase32"
	"github.com/kradalby/tsnixcache/nixcompress"
	"github.com/kradalby/tsnixcache/store"
)

// Version is set at build time.
var Version = "dev"

// StoreProvider abstracts the store for testability.
type StoreProvider interface {
	PathInfo(ctx context.Context, hashPart string) (*store.PathInfo, error)
}

// ImporterFunc is a function that imports a narinfo into the nix store.
type ImporterFunc func(ctx context.Context, ni *narinfo.NarInfo) error

// Server is the cache HTTP server.
type Server struct {
	Store            StoreProvider
	Priority         int
	SpoolDir         string
	ServeCompression string // "none" or "zstd"
	Signer           interface {
		Sign(fingerprint string) string
	}
	ImportFn ImporterFunc
	Registry *prometheus.Registry

	// metrics
	narInfoHits    prometheus.Counter
	narInfoMisses  prometheus.Counter
	narBytesServed prometheus.Counter
	pushNAR        prometheus.Counter
	pushNarInfo    prometheus.Counter
	importSuccess  prometheus.Counter
	importFail     prometheus.Counter

	// zstd cache: maps narHash → path of compressed temp file
	zstdCacheMu sync.Mutex
	zstdCache   map[string]string

	startTime time.Time
}

// Signer is a minimal signing interface satisfied by *signing.SecretKey.
type Signer interface {
	Sign(fingerprint string) string
}

// New creates a new Server with its own prometheus registry.
func New(
	s StoreProvider,
	signer Signer,
	priority int,
	spoolDir, serveCompression string,
	importFn ImporterFunc,
) *Server {
	reg := prometheus.NewRegistry()

	srv := &Server{
		Store:            s,
		Priority:         priority,
		SpoolDir:         spoolDir,
		ServeCompression: serveCompression,
		ImportFn:         importFn,
		Registry:         reg,
		zstdCache:        make(map[string]string),
		startTime:        time.Now(),
	}
	if signer != nil {
		srv.Signer = signer
	}

	srv.narInfoHits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsnixcache_narinfo_hits_total",
		Help: "Number of narinfo requests that found a hit in the store.",
	})
	srv.narInfoMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsnixcache_narinfo_misses_total",
		Help: "Number of narinfo requests that did not find a path in the store.",
	})
	srv.narBytesServed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsnixcache_nar_bytes_served_total",
		Help: "Total bytes of NAR data served.",
	})
	srv.pushNAR = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsnixcache_push_nar_total",
		Help: "Number of NAR files received via PUT.",
	})
	srv.pushNarInfo = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsnixcache_push_narinfo_total",
		Help: "Number of narinfo files received via PUT.",
	})
	srv.importSuccess = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsnixcache_import_success_total",
		Help: "Number of successful nix-store imports.",
	})
	srv.importFail = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsnixcache_import_fail_total",
		Help: "Number of failed nix-store imports.",
	})

	reg.MustRegister(
		srv.narInfoHits,
		srv.narInfoMisses,
		srv.narBytesServed,
		srv.pushNAR,
		srv.pushNarInfo,
		srv.importSuccess,
		srv.importFail,
	)

	return srv
}

// Handler returns the HTTP handler for the cache server.
func (srv *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /nix-cache-info", srv.handleNixCacheInfo)
	mux.HandleFunc("HEAD /nix-cache-info", srv.handleNixCacheInfo)

	// /nar/{name} routes — registered before the catch-all below.
	mux.HandleFunc("GET /nar/", func(w http.ResponseWriter, r *http.Request) {
		srv.handleGetNar(w, r)
	})
	mux.HandleFunc("HEAD /nar/", func(w http.ResponseWriter, r *http.Request) {
		srv.handleHeadNar(w, r)
	})
	mux.HandleFunc("PUT /nar/", func(w http.ResponseWriter, r *http.Request) {
		srv.handlePutNar(w, r)
	})

	mux.Handle("GET /metrics", promhttp.HandlerFor(srv.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /health", srv.handleHealth)
	mux.HandleFunc("GET /version", srv.handleVersion)

	// Catch-all: dispatch .narinfo and / routes.
	mux.HandleFunc("/", srv.handleRoot)

	return loggingMiddleware(mux)
}

// handleNixCacheInfo serves the /nix-cache-info endpoint.
func (srv *Server) handleNixCacheInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "StoreDir: /nix/store\nWantMassQuery: 1\nPriority: %d\n", srv.Priority)
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
	ni, err := srv.buildNarInfo(r.Context(), hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			srv.narInfoMisses.Add(1)
			http.NotFound(w, r)
			return
		}
		slog.Error("narinfo", "hash", hash, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	srv.narInfoHits.Add(1)
	body := ni.Marshal()
	w.Header().Set("Content-Type", "text/x-nix-narinfo")
	fmt.Fprint(w, body)
}

// handleHeadNarInfo serves HEAD /{hash}.narinfo.
func (srv *Server) handleHeadNarInfo(w http.ResponseWriter, r *http.Request) {
	hash, ok := narInfoHash(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ni, err := srv.buildNarInfo(r.Context(), hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			srv.narInfoMisses.Add(1)
			http.NotFound(w, r)
			return
		}
		slog.Error("narinfo head", "hash", hash, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	srv.narInfoHits.Add(1)
	body := ni.Marshal()
	w.Header().Set("Content-Type", "text/x-nix-narinfo")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(http.StatusOK)
}

// buildNarInfo constructs a NarInfo from the store for the given hash part.
func (srv *Server) buildNarInfo(ctx context.Context, hashPart string) (*narinfo.NarInfo, error) {
	pi, err := srv.Store.PathInfo(ctx, hashPart)
	if err != nil {
		return nil, err
	}

	// Extract the file hash from NarHash (strip "sha256:" prefix).
	fileHashPart := strings.TrimPrefix(pi.NarHash, "sha256:")

	var (
		url         string
		compression string
		fileHash    string
		fileSize    uint64
	)

	if srv.ServeCompression == "zstd" {
		// Compress NAR to a temp file to get FileHash/FileSize.
		compPath, compHash, compSize, cerr := srv.getOrCreateZstdCache(pi)
		if cerr != nil {
			slog.Warn("narinfo: zstd compress failed, falling back to none", "err", cerr)
			// Fall back to uncompressed.
			url = "nar/" + fileHashPart + ".nar"
			compression = "none"
			fileHash = pi.NarHash
			fileSize = uint64(pi.NarSize)
			_ = compPath
		} else {
			url = "nar/" + fileHashPart + ".nar.zstd"
			compression = "zstd"
			fileHash = "sha256:" + compHash
			fileSize = compSize
		}
	} else {
		url = "nar/" + fileHashPart + ".nar"
		compression = "none"
		fileHash = pi.NarHash
		fileSize = uint64(pi.NarSize)
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
		NarSize:     uint64(pi.NarSize),
		References:  refs,
		Deriver:     pi.Deriver,
		Sigs:        append([]string(nil), pi.Sigs...),
		CA:          pi.CA,
	}

	if srv.Signer != nil {
		sig := srv.Signer.Sign(ni.Fingerprint())
		ni.Sigs = append(ni.Sigs, sig)
	}

	return ni, nil
}

// getOrCreateZstdCache compresses the NAR for pi to a temp file, caching by NarHash.
// Returns (filePath, sha256hex-in-nixbase32, size, error).
func (srv *Server) getOrCreateZstdCache(pi *store.PathInfo) (string, string, uint64, error) {
	srv.zstdCacheMu.Lock()
	if p, ok := srv.zstdCache[pi.NarHash]; ok {
		srv.zstdCacheMu.Unlock()
		fi, err := os.Stat(p)
		if err == nil {
			h, herr := fileHash(p)
			if herr == nil {
				return p, h, uint64(fi.Size()), nil
			}
		}
		// Stale entry; fall through to recreate.
		srv.zstdCacheMu.Lock()
		delete(srv.zstdCache, pi.NarHash)
		srv.zstdCacheMu.Unlock()
		srv.zstdCacheMu.Lock()
	}
	srv.zstdCacheMu.Unlock()

	// Compress to a temp file.
	tmp, err := os.CreateTemp("", "tsnixcache-zstd-*.nar.zstd")
	if err != nil {
		return "", "", 0, fmt.Errorf("zstd cache: create temp: %w", err)
	}
	tmpName := tmp.Name()

	enc, err := nixcompress.Encoder(tmp, "zstd", false)
	if err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", "", 0, fmt.Errorf("zstd cache: encoder: %w", err)
	}

	if err := nar.Write(enc, pi.StorePath); err != nil {
		enc.Close()
		tmp.Close()
		os.Remove(tmpName)
		return "", "", 0, fmt.Errorf("zstd cache: nar.Write: %w", err)
	}
	if err := enc.Close(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", "", 0, fmt.Errorf("zstd cache: encoder close: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", "", 0, fmt.Errorf("zstd cache: close temp: %w", err)
	}

	fi, err := os.Stat(tmpName)
	if err != nil {
		os.Remove(tmpName)
		return "", "", 0, fmt.Errorf("zstd cache: stat: %w", err)
	}

	h, err := fileHash(tmpName)
	if err != nil {
		os.Remove(tmpName)
		return "", "", 0, err
	}

	srv.zstdCacheMu.Lock()
	srv.zstdCache[pi.NarHash] = tmpName
	srv.zstdCacheMu.Unlock()

	return tmpName, h, uint64(fi.Size()), nil
}

// fileHash returns the sha256 hash of a file in nix-base32 encoding.
func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("fileHash: open: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("fileHash: copy: %w", err)
	}
	return nixbase32.EncodeToString(h.Sum(nil)), nil
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
	hashPart := strings.SplitN(name, ".", 2)[0]
	if hashPart == "" {
		http.Error(w, "invalid NAR name", http.StatusBadRequest)
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

	pi, err := srv.Store.PathInfo(r.Context(), lookupHash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		slog.Error("nar: store lookup", "name", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	isZstd := strings.HasSuffix(name, ".nar.zstd") || strings.HasSuffix(name, ".zstd")

	w.Header().Set("Content-Type", "application/x-nix-nar")

	if headOnly {
		w.WriteHeader(http.StatusOK)
		return
	}

	if isZstd && srv.ServeCompression == "zstd" {
		// Serve from zstd cache if available, else compress on the fly.
		compPath, _, _, cerr := srv.getOrCreateZstdCache(pi)
		if cerr == nil {
			cw := &countWriter{w: w}
			f, err := os.Open(compPath)
			if err == nil {
				defer f.Close()
				if _, err := io.Copy(cw, f); err != nil {
					slog.Error("nar: serve zstd cached", "err", err)
				}
				srv.narBytesServed.Add(float64(cw.n))
				return
			}
		}
		// Fall back: compress on the fly.
		cw := &countWriter{w: w}
		enc, err := nixcompress.Encoder(cw, "zstd", false)
		if err != nil {
			slog.Error("nar: zstd encoder", "err", err)
			return
		}
		if err := nar.Write(enc, pi.StorePath); err != nil {
			slog.Error("nar: write zstd", "err", err)
		}
		if err := enc.Close(); err != nil {
			slog.Error("nar: close zstd encoder", "err", err)
		}
		srv.narBytesServed.Add(float64(cw.n))
		return
	}

	// Serve uncompressed NAR.
	cw := &countWriter{w: w}
	if err := nar.Write(cw, pi.StorePath); err != nil {
		slog.Error("nar: write", "err", err)
	}
	srv.narBytesServed.Add(float64(cw.n))
}

// countWriter counts bytes written.
type countWriter struct {
	w http.ResponseWriter
	n int64
}

func (cw *countWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	return n, err
}

// handlePutNar handles PUT /nar/{name}: spool body to spoolDir.
func (srv *Server) handlePutNar(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/nar/")
	if name == "" {
		http.Error(w, "invalid NAR name", http.StatusBadRequest)
		return
	}
	dest := filepath.Join(srv.SpoolDir, name)
	if err := os.MkdirAll(srv.SpoolDir, 0o755); err != nil {
		slog.Error("put nar: mkdir spoolDir", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	f, err := os.Create(dest)
	if err != nil {
		slog.Error("put nar: create spool file", "dest", dest, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	if _, err := io.Copy(f, r.Body); err != nil {
		slog.Error("put nar: copy body", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	srv.pushNAR.Add(1)
	w.WriteHeader(http.StatusOK)
}

// handlePutNarInfo handles PUT /{hash}.narinfo: parse and trigger import.
func (srv *Server) handlePutNarInfo(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	ni, err := narinfo.Parse(bytes.NewReader(body))
	if err != nil {
		http.Error(w, "parse error", http.StatusBadRequest)
		return
	}
	srv.pushNarInfo.Add(1)

	if srv.ImportFn != nil {
		if err := srv.ImportFn(r.Context(), ni); err != nil {
			slog.Error("import", "err", err)
			srv.importFail.Add(1)
			http.Error(w, "import failed", http.StatusInternalServerError)
			return
		}
		srv.importSuccess.Add(1)
	}
	w.WriteHeader(http.StatusOK)
}

// healthResponse is the JSON body for /health.
type healthResponse struct {
	Status         string `json:"status"`
	StoreReachable bool   `json:"store_reachable"`
	DBReadable     bool   `json:"db_readable"`
	UptimeSeconds  int64  `json:"uptime_seconds"`
}

// handleHealth serves GET /health.
func (srv *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Probe the store with a lookup that should return ErrNotFound.
	_, err := srv.Store.PathInfo(r.Context(), "healthcheck000000000000000000000")
	storeReachable := err == nil || errors.Is(err, store.ErrNotFound)

	status := "ok"
	if !storeReachable {
		status = "degraded"
	}

	resp := healthResponse{
		Status:         status,
		StoreReachable: storeReachable,
		DBReadable:     storeReachable,
		UptimeSeconds:  int64(time.Since(srv.startTime).Seconds()),
	}
	w.Header().Set("Content-Type", "application/json")
	if !storeReachable {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(resp)
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

// statusWriter captures the HTTP status and bytes written for logging.
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

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		slog.Info(
			"http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"bytes", sw.bytes,
			"dur", time.Since(start),
		)
	})
}
