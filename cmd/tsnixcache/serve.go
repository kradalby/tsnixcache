// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/peterbourgon/ff/v3/ffcli"
	"golang.org/x/sync/errgroup"
	"tailscale.com/net/netns"
	"tailscale.com/tsnet"

	"github.com/kradalby/tsnixcache/auth"
	"github.com/kradalby/tsnixcache/cache"
	"github.com/kradalby/tsnixcache/nixcompress"
	"github.com/kradalby/tsnixcache/niximport"
	"github.com/kradalby/tsnixcache/signing"
	"github.com/kradalby/tsnixcache/store"
)

// multiFlag is a flag.Value that accumulates multiple string values.
type multiFlag []string

func (f *multiFlag) String() string { return strings.Join(*f, ", ") }
func (f *multiFlag) Set(v string) error {
	*f = append(*f, v)

	return nil
}

func newServeCmd() *ffcli.Command {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)

	dbPath := fs.String("db", "/nix/var/nix/db/db.sqlite", "path to Nix SQLite database")
	storeDir := fs.String("store-dir", "/nix/store", "path to Nix store directory")
	signKeyFile := fs.String("sign-key-file", "", "path to signing key file (optional)")
	priority := fs.Int("priority", 30, "cache priority (lower = higher precedence)")
	spoolDir := fs.String("spool-dir", defaultSpoolDir, "directory for spooled NAR files")
	gcRootDir := fs.String("gcroot-dir", "/nix/var/nix/gcroots/tsnixcache", "directory for GC root symlinks")
	serveCompression := fs.String("serve-compression", compressionNone, "compression for served NARs (none or zstd)")
	nixStoreURI := fs.String("nix-store-uri", "auto", "store URI passed to nix's own --store (auto = system daemon)")
	storePrefix := fs.String("store", "", "serve a self-contained chroot store rooted here")
	importConcurrency := fs.Int("import-concurrency", runtime.NumCPU(), "max concurrent imports (0 = unlimited)")

	var listenAddrs multiFlag

	fs.Var(&listenAddrs, "listen", "local listen address (repeatable, default 127.0.0.1:5000)")

	localWrite := fs.Bool(
		"local-write",
		false,
		"accept pushes on --listen addresses, which have no authentication of any kind",
	)

	const tsnetUsage = "tsnet spec: hostname=...,authkey-file=...,dir=...,port=80,tls=false,control=... (repeatable)"

	var tsnetFlags multiFlag

	fs.Var(&tsnetFlags, "tsnet", tsnetUsage)

	externalCompression := fs.Bool(
		"external-compression",
		false,
		"decompress pushed NARs with the external xz/zstd binaries when they are in PATH",
	)

	allowMissingCodecs := fs.Bool(
		"allow-missing-codecs",
		false,
		"start even when a codec binary is missing; pushes in that codec then fail",
	)

	var gcRules gcRulesFlag

	fs.Var(&gcRules, "gc-rule", `GC rule as "threshold:duration" e.g. "80:20d"; repeatable`)

	gcInterval := fs.String(
		"gc-interval",
		"5m",
		`interval between GC rule checks, e.g. "5m" or "1d" (ignored without --gc-rule)`,
	)

	return &ffcli.Command{
		Name:       "serve",
		ShortUsage: "tsnixcache serve [flags]",
		ShortHelp:  "Run the binary cache HTTP server.",
		FlagSet:    fs,
		Exec: func(ctx context.Context, args []string) error {
			// Validated here, before anything is opened or bound, so a bad
			// value fails the process rather than the GC watcher.
			interval, err := parseGCInterval(*gcInterval)
			if err != nil {
				return err
			}

			// The cache treats anything that is not "zstd" as "none", so an
			// unsupported value would otherwise start a server that silently
			// serves everything uncompressed. The NixOS module has an enum; a
			// hand-written command line needs this.
			if *serveCompression != compressionNone && *serveCompression != compressionZstd {
				return fmt.Errorf("%w: %q, want %s or %s",
					errBadServeCompression, *serveCompression, compressionNone, compressionZstd)
			}

			// Before anything is opened or bound: a codec binary that is only
			// missed at push time fails the client after it has uploaded the
			// whole NAR, on a different machine to whoever deployed this.
			err = checkCodecBinaries(*externalCompression, *allowMissingCodecs)
			if err != nil {
				return err
			}

			cfg := serveConfig{
				dbPath:              *dbPath,
				storeDir:            *storeDir,
				signKeyFile:         *signKeyFile,
				priority:            *priority,
				spoolDir:            *spoolDir,
				gcRootDir:           *gcRootDir,
				serveCompression:    *serveCompression,
				nixStoreURI:         *nixStoreURI,
				externalCompression: *externalCompression,
				listenAddrs:         listenAddrs,
				localWrite:          *localWrite,
				tsnetSpecs:          tsnetFlags,
				gcRules:             []GCRule(gcRules),
				gcInterval:          interval,
				importConcurrency:   *importConcurrency,
			}

			err = applyStorePrefix(fs, *storePrefix, &cfg)
			if err != nil {
				return err
			}

			// gc measures the threshold on --store-dir and prunes its gcroots,
			// but nix-collect-garbage takes no store argument and always
			// collects /nix/store. Pairing GC with any other store would prune
			// one store's roots and collect another's, so refuse both shapes.
			if len(cfg.gcRules) > 0 {
				if cfg.storePrefix != "" {
					return errStoreGCConflict
				}

				if filepath.Clean(cfg.storeDir) != defaultStoreDir {
					return fmt.Errorf("%w, got %q", errGCNonDefaultStoreDir, cfg.storeDir)
				}
			}

			return runServe(ctx, cfg)
		},
	}
}

type serveConfig struct {
	dbPath           string
	storeDir         string
	signKeyFile      string
	priority         int
	spoolDir         string
	gcRootDir        string
	serveCompression string
	nixStoreURI      string
	// externalCompression is niximport's UseExternal: spawn xz/zstd instead of
	// decompressing in-process. The NixOS module puts both binaries in PATH.
	externalCompression bool
	// listenAddrs holds the --listen values; empty means "no --listen was
	// usable", which only falls back to the loopback default when there is no
	// tsnet listener either. See localListenAddrs.
	listenAddrs []string
	// localWrite is --local-write: serve the bare handler on the --listen
	// sockets instead of the read-only one. See localHandler.
	localWrite        bool
	tsnetSpecs        []string
	gcRules           []GCRule
	gcInterval        time.Duration
	storePrefix       string
	storeRoot         string
	importConcurrency int
}

// The two values --serve-compression accepts; the cache reads them verbatim.
const (
	compressionNone = "none"
	compressionZstd = "zstd"
)

// defaultSpoolDir matches the NixOS module's spoolDir. Every pushed NAR is
// written here before import and the server is a nix trusted-user, so the
// directory must not sit anywhere another local user can prepare it: under
// /tmp any of them can pre-create it as a symlink, or plant symlinks inside
// it, and turn a push into a write to a path of their choosing.
const defaultSpoolDir = "/var/cache/tsnixcache/spool"

var (
	errStoreFlagConflict   = errors.New("serve: --store is mutually exclusive with the individual store path flags")
	errStoreGCConflict     = errors.New("serve: --gc-rule is not supported together with --store")
	errBadGCInterval       = errors.New("serve: invalid --gc-interval")
	errBadServeCompression = errors.New("serve: invalid --serve-compression")
	errNoListeners         = errors.New("serve: no listeners configured; pass --listen and/or --tsnet")
	errMissingCodecBinary  = errors.New("serve: codec binary not found in PATH")
	errUnsafeSpoolDir      = errors.New("serve: unsafe spool dir")
	errStoreUnavailable    = errors.New("serve: nix store database is not open")
)

// parseGCInterval parses the --gc-interval flag. It goes through
// parseDurationString rather than flag.Duration so the "1d" form the NixOS
// module allows is accepted, and so a zero or negative value is rejected at
// startup instead of panicking time.NewTicker inside the GC watcher — long
// after the server has announced that it is listening.
func parseGCInterval(s string) (time.Duration, error) {
	d, err := parseDurationString(s)
	if err != nil {
		return 0, fmt.Errorf("%w %q: %w", errBadGCInterval, s, err)
	}

	return d, nil
}

// checkCodecBinaries refuses to start when a codec binary that pushes depend on
// is not in PATH.
//
// Only the decode direction matters here: the cache compresses what it serves
// with the in-process zstd writer and never spawns an encoder, so a missing
// bzip2 or xz encoder costs a server nothing. Decoding is the pusher's choice,
// and xz decoding always shells out because the pure-Go decoder cannot be given
// a memory ceiling — which makes xz(1) mandatory for the commonest push there
// is, since `nix copy --to http://...` compresses with xz by default.
//
// Failing at startup rather than at the first push is the whole point: a push
// failure surfaces on the pusher's machine, after it has already transferred the
// entire NAR, while the operator who deployed a PATH without xz sees nothing.
// --allow-missing-codecs is the way out for a cache that only ever receives
// zstd, e.g. one fed solely by `tsnixcache push`.
func checkCodecBinaries(useExternal, allowMissing bool) error {
	for _, req := range nixcompress.Requirements(useExternal) {
		if !req.Decode || req.Present {
			continue
		}

		if req.Optional {
			// --external-compression asked for this one; the in-process decoder
			// still handles the codec, so the request simply went unhonoured.
			slog.Warn("serve: codec binary not found in PATH, decoding it in-process instead",
				"binary", req.Binary, "codec", req.Codec)

			continue
		}

		if allowMissing {
			slog.Warn("serve: codec binary not found in PATH, pushes in this codec will fail",
				"binary", req.Binary, "codec", req.Codec, "flag", "--allow-missing-codecs")

			continue
		}

		// No `nix copy` name-drop here even though xz is the only binary that
		// reaches this line today: what is true of every codec that could is
		// that nothing else decodes it, and that the pusher pays for finding
		// out.
		return fmt.Errorf(
			"%w: %s decodes %s-compressed pushes and nothing else does, so every one would fail"+
				" after its client had uploaded the whole NAR; put %s on the service PATH,"+
				" or pass --allow-missing-codecs to start a cache that refuses %s pushes",
			errMissingCodecBinary, req.Binary, req.Codec, req.Binary, req.Codec,
		)
	}

	return nil
}

// applyStorePrefix derives the store paths from a chroot store prefix and
// rejects combining --store with the individual path flags it replaces.
func applyStorePrefix(fs *flag.FlagSet, prefix string, cfg *serveConfig) error {
	if prefix == "" {
		return nil
	}

	set := map[string]bool{}

	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	for _, name := range []string{"store-dir", "db", "nix-store-uri", "gcroot-dir"} {
		if set[name] {
			return fmt.Errorf("%w: --%s", errStoreFlagConflict, name)
		}
	}

	// The chroot DB records logical /nix/store paths, so storeDir stays the
	// logical prefix; storeRoot is the physical location prepended on read.
	cfg.storePrefix = prefix
	cfg.storeRoot = prefix
	cfg.dbPath = filepath.Join(prefix, "nix", "var", "nix", "db", "db.sqlite")
	cfg.nixStoreURI = prefix
	cfg.gcRootDir = filepath.Join(prefix, "nix", "var", "nix", "gcroots", "tsnixcache")

	return nil
}

// openStore initialises a chroot store if requested, then opens its DB.
func openStore(ctx context.Context, cfg serveConfig) (*store.Store, error) {
	if cfg.storePrefix != "" {
		err := ensureChrootStore(ctx, cfg.storePrefix, cfg.dbPath)
		if err != nil {
			return nil, fmt.Errorf("serve: init chroot store: %w", err)
		}
	}

	st, err := store.Open(ctx, cfg.dbPath, cfg.storeDir)
	if err != nil {
		return nil, fmt.Errorf("serve: open store: %w", err)
	}

	return st, nil
}

// pendingStore is the store handle before store.Open has succeeded, and the
// reason a failed open is not fatal. Nix keeps its DB in WAL mode, and reading
// a WAL database needs a wal-index that an unprivileged service cannot create;
// it works only because something else — nix-daemon — already holds the DB
// open. On a fresh boot, before anything has, Open fails; exiting there turns a
// condition that resolves itself into a restart loop. So serve binds anyway,
// every store query fails, /health reports degraded, and openWithRetry keeps
// trying until it works.
type pendingStore struct {
	cfg serveConfig

	mu sync.RWMutex
	st *store.Store
}

// storeRetry bounds the wait between store.Open attempts. The first attempts
// are quick because the usual cause — racing nix-daemon's first open at boot —
// clears in seconds; the cap keeps a genuinely broken config down to a log line
// a minute.
const (
	storeRetryMin = 500 * time.Millisecond
	storeRetryMax = 30 * time.Second
)

func (p *pendingStore) PathInfo(ctx context.Context, hashPart string) (*store.PathInfo, error) {
	st := p.get()
	if st == nil {
		return nil, errStoreUnavailable
	}

	return st.PathInfo(ctx, hashPart)
}

func (p *pendingStore) PathCount(ctx context.Context) (int64, error) {
	st := p.get()
	if st == nil {
		return 0, errStoreUnavailable
	}

	return st.PathCount(ctx)
}

// Close releases the DB handle and its prepared statements. Only meaningful on
// a clean shutdown; the process exiting would do it anyway.
func (p *pendingStore) Close() {
	if st := p.get(); st != nil {
		_ = st.Close()
	}
}

func (p *pendingStore) get() *store.Store {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.st
}

// open makes one attempt, keeping the handle on success.
func (p *pendingStore) open(ctx context.Context) error {
	st, err := openStore(ctx, p.cfg)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.st = st

	return nil
}

// openWithRetry retries until the store opens or ctx is cancelled.
func (p *pendingStore) openWithRetry(ctx context.Context) {
	delay := storeRetryMin

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		err := p.open(ctx)
		if err == nil {
			slog.Info("serve: nix store database opened, no longer degraded", "db", p.cfg.dbPath)

			return
		}

		slog.Error("serve: nix store database still unreadable", "db", p.cfg.dbPath, "retry_in", delay, "err", err)

		delay = min(delay*2, storeRetryMax)
	}
}

// ensureChrootStore initialises a chroot Nix store if its database does not yet
// exist, so the read-only DB open at startup succeeds on a fresh prefix.
func ensureChrootStore(ctx context.Context, prefix, dbPath string) error {
	_, err := os.Stat(dbPath)
	if err == nil {
		return nil
	}

	cmd := exec.CommandContext(ctx, "nix-store", "--store", prefix, "--init") // #nosec G204 -- trusted binary

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nix-store --init: %w\n%s", err, out)
	}

	return nil
}

func runServe(ctx context.Context, cfg serveConfig) error {
	signer, err := loadSigner(cfg.signKeyFile)
	if err != nil {
		return err
	}

	err = ensureSpoolDir(cfg.spoolDir)
	if err != nil {
		return err
	}

	// Open the Nix store DB (initialising a chroot store first if needed). A
	// failure here is not fatal: see pendingStore.
	st := &pendingStore{cfg: cfg}

	err = st.open(ctx)
	if err != nil {
		slog.Error("serve: nix store database unreadable, serving degraded until it opens", "err", err)
	}

	defer st.Close()

	// Build the importer.
	imp := newImporter(cfg)

	// Build the cache server. cache.New wipes whatever the last process left in
	// the spool, so it has to run before anything can bind and spool afresh.
	srv := cache.New(cache.Config{
		Store:             st,
		Signer:            signer,
		Priority:          cfg.priority,
		SpoolDir:          cfg.spoolDir,
		ServeCompression:  cfg.serveCompression,
		StoreDir:          cfg.storeDir,
		StoreRoot:         cfg.storeRoot,
		ImportFn:          imp.Import,
		ImportConcurrency: cfg.importConcurrency,
	})

	handler := srv.Handler()

	addrs := localListenAddrs(cfg.listenAddrs, cfg.tsnetSpecs)
	if len(addrs) == 0 && len(cfg.tsnetSpecs) == 0 {
		return errNoListeners
	}

	// Bind before announcing: a listener that cannot bind must bring the process
	// down with its error, not leave it serving on whatever else came up.
	listeners, err := listenAll(ctx, addrs)
	if err != nil {
		return err
	}

	// Every goroutine below runs under egCtx, so cancelling ctx (SIGTERM) or any
	// one of them failing stops all the others and eg.Wait returns.
	eg, egCtx := errgroup.WithContext(ctx)

	local := localHandler(handler, cfg.localWrite)

	for _, ln := range listeners {
		slog.Info("serve: listening", "addr", ln.Addr().String())

		if cfg.localWrite {
			// The sibling of the NixOS module's non-loopback warning, and the
			// same consequence: nothing identifies whoever pushes here, the
			// service imports as a nix trusted-user, and the imported path is
			// re-signed with the cache key.
			slog.Warn("serve: --local-write accepts unauthenticated pushes on this listener;"+
				" imported paths are re-signed with the cache key and, at this priority,"+
				" shadow the upstream cache for every client that trusts it",
				"addr", ln.Addr().String(), "priority", cfg.priority)
		}

		// Reads are open on every listener, so an address beyond loopback hands
		// the whole store to anything that can route to it. The NixOS module
		// warns at evaluation time; a hand-run serve, which the README documents,
		// would otherwise say nothing at all.
		if !isLoopbackAddr(ln.Addr()) {
			slog.Warn("serve: listener is not on loopback; reads are unauthenticated,"+
				" so anything that can reach this address can read the whole store",
				"addr", ln.Addr().String())
		}

		serveHTTP(egCtx, eg, ln, local)
	}

	for _, spec := range cfg.tsnetSpecs {
		err := setupTsnetListener(egCtx, eg, spec, handler)
		if err != nil {
			// Hand the failure to the group rather than returning: that
			// cancels egCtx, so the listeners bound above shut down and their
			// goroutines return instead of serving on past runServe.
			eg.Go(func() error { return err })

			break
		}
	}

	if len(cfg.gcRules) > 0 {
		gcm := newGCMetrics(srv.Registry(), cfg.gcRootDir, cfg.gcRules)

		eg.Go(func() error {
			runGCWatcher(egCtx, cfg.storeDir, cfg.gcRootDir, cfg.gcRules, cfg.gcInterval, gcm)

			return nil
		})
	}

	eg.Go(func() error {
		runSpoolSweeper(egCtx, srv)

		return nil
	})

	if st.get() == nil {
		eg.Go(func() error {
			st.openWithRetry(egCtx)

			return nil
		})
	}

	return eg.Wait()
}

// loadSigner parses the signing key, if one was configured.
func loadSigner(keyFile string) (cache.Signer, error) {
	if keyFile == "" {
		return nil, nil //nolint:nilnil // no key configured is not an error
	}

	// The secret key is the whole of the cache's authority over its clients, so
	// say so when the deployment left it readable by anyone but us. The NixOS
	// module's 0400 credential is fine; a hand-rolled one often is not.
	fi, err := os.Stat(keyFile)
	if err == nil && fi.Mode().Perm()&0o077 != 0 {
		slog.Warn("serve: signing key file is readable by other users",
			"file", keyFile, "mode", fmt.Sprintf("%#o", fi.Mode().Perm()))
	}

	data, err := os.ReadFile(keyFile) // #nosec G304 -- the operator names the key file
	if err != nil {
		return nil, fmt.Errorf("serve: read sign-key-file: %w", err)
	}

	sk, err := signing.ParseSecretKey(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("serve: parse sign key: %w", err)
	}

	return sk, nil
}

// ensureSpoolDir creates the spool directory and refuses one that somebody else
// could have prepared. os.MkdirAll accepts an existing path whatever its owner
// or mode, and it follows a symlink; every pushed NAR is written here by a
// process that is a nix trusted-user, so a spool another local user controls is
// a write primitive pointed wherever they like.
func ensureSpoolDir(dir string) error {
	err := os.MkdirAll(dir, 0o750)
	if err != nil {
		return fmt.Errorf("serve: create spool dir (or point --spool-dir at one you own): %w", err)
	}

	// Lstat, not Stat: a symlink is exactly the case being refused.
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("serve: stat spool dir: %w", err)
	}

	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%w: %s is a symlink; point --spool-dir at the real directory", errUnsafeSpoolDir, dir)
	case !fi.IsDir():
		return fmt.Errorf("%w: %s is not a directory", errUnsafeSpoolDir, dir)
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("%w: %s is writable by other users (mode %#o)", errUnsafeSpoolDir, dir, fi.Mode().Perm())
	}

	uid, ok := ownerUID(fi)
	if ok && uid != os.Getuid() {
		return fmt.Errorf("%w: %s is owned by uid %d, not %d", errUnsafeSpoolDir, dir, uid, os.Getuid())
	}

	return nil
}

// ownerUID returns the owning uid of fi, if the platform reports one.
func ownerUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}

	return int(st.Uid), true
}

// newImporter builds the NAR importer from the serve configuration.
func newImporter(cfg serveConfig) *niximport.Importer {
	nixURI := cfg.nixStoreURI
	if nixURI == "auto" {
		nixURI = ""
	}

	return &niximport.Importer{
		SpoolDir:  cfg.spoolDir,
		GCRootDir: cfg.gcRootDir,
		// Logical store dir: the importer validates the pushed StorePath
		// against it, and the DB (and so every narinfo) records logical paths
		// even when a chroot store is served.
		StoreDir:    cfg.storeDir,
		NixStoreURI: nixURI,
		UseExternal: cfg.externalCompression,
	}
}

// defaultListenAddr is used when --listen is not given at all.
const defaultListenAddr = "127.0.0.1:5000"

// localListenAddrs resolves the --listen values. The local listener has no
// authentication, so the loopback default applies only when nothing else was
// asked for: with a --tsnet listener configured and no usable --listen, there is
// no local listener at all. That is what the NixOS module produces for
// `listen = [ ]` — it maps over the list, so an empty one emits no flag — and an
// operator who empties that list must not keep an unauthenticated listener.
func localListenAddrs(flagValues, tsnetSpecs []string) []string {
	if len(flagValues) == 0 {
		if len(tsnetSpecs) > 0 {
			return nil
		}

		return []string{defaultListenAddr}
	}

	addrs := make([]string, 0, len(flagValues))

	for _, addr := range flagValues {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			addrs = append(addrs, addr)
		}
	}

	return addrs
}

// listenAll binds every address, closing the ones already bound if a later one
// fails so the error is the only outcome.
func listenAll(ctx context.Context, addrs []string) ([]net.Listener, error) {
	var lc net.ListenConfig

	listeners := make([]net.Listener, 0, len(addrs))

	for _, addr := range addrs {
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}

			return nil, fmt.Errorf("serve: listen on %s: %w", addr, err)
		}

		listeners = append(listeners, ln)
	}

	return listeners, nil
}

// runSpoolSweeper periodically evicts idle compressed-NAR cache files and
// abandoned upload spool files so the spool dir doesn't grow unbounded.
func runSpoolSweeper(ctx context.Context, srv *cache.Server) {
	const sweepInterval = 10 * time.Minute

	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepOnce(srv)
		}
	}
}

const (
	// The compressed-NAR cache only bridges a narinfo request to the GET
	// seconds later, so a generous idle TTL is ample.
	zstdMaxIdle = time.Hour

	// A spool file's mtime advances as the upload streams into it, so "idle"
	// means a PUT /nar that stopped writing and whose narinfo never arrived. A
	// day is far past any upload still in flight — clients give up after a 60s
	// stall — while still bounding the leak.
	spoolMaxIdle = 24 * time.Hour
)

// sweepOnce is one pass of the sweeper. Separate from the loop so a test can
// drive a pass without waiting out sweepInterval.
func sweepOnce(srv *cache.Server) {
	if n := srv.SweepZstdCache(zstdMaxIdle); n > 0 {
		slog.Debug("zstd cache: swept idle entries", "removed", n)
	}

	if n := srv.SweepSpool(spoolMaxIdle); n > 0 {
		slog.Info("spool: swept abandoned uploads", "removed", n)
	}
}

// setupTsnetListener parses a tsnet spec, starts the tsnet server, and registers
// the listener goroutine with the errgroup.
func setupTsnetListener(ctx context.Context, eg *errgroup.Group, spec string, handler http.Handler) error {
	tsSpec, err := parseTsnetSpec(spec)
	if err != nil {
		return fmt.Errorf("serve: parse --tsnet flag: %w", err)
	}

	authKey := ""

	if tsSpec.authKeyFile != "" {
		data, err := os.ReadFile(tsSpec.authKeyFile)
		if err != nil {
			return fmt.Errorf("serve: read tsnet authkey-file: %w", err)
		}

		authKey = strings.TrimSpace(string(data))
	}

	ts := &tsnet.Server{
		Hostname:   tsSpec.hostname,
		AuthKey:    authKey,
		Dir:        tsSpec.dir,
		ControlURL: tsSpec.control,
		Logf:       log.Printf,
	}

	// tsnet uses userspace networking; SO_BINDTODEVICE to the default route
	// interface breaks control-plane dials when headscale is on a different subnet.
	netns.SetEnabled(false)

	err = ts.Start()
	if err != nil {
		return fmt.Errorf("serve: tsnet start: %w", err)
	}

	lc, err := ts.LocalClient()
	if err != nil {
		return fmt.Errorf("serve: tsnet local client: %w", err)
	}

	port := tsSpec.port
	if port == "" {
		port = "80"
	}

	ln, err := ts.Listen("tcp", ":"+port)
	if err != nil {
		return fmt.Errorf("serve: tsnet listen: %w", err)
	}

	slog.Info("serve: tsnet listening", "hostname", tsSpec.hostname, "port", port)

	serveHTTP(ctx, eg, ln, tsnetHandler(lc, handler))

	return nil
}

const (
	// readHeaderTimeout bounds how long a client may take to send request
	// headers (slowloris protection). The request body is deliberately NOT
	// bounded: NAR uploads are large and stream over a possibly slow tailnet,
	// so a body deadline severs legitimate pushes mid-transfer. Body size is
	// capped in the handler.
	readHeaderTimeout = 30 * time.Second

	// idleTimeout reaps keep-alive connections nobody is using any more. Nix
	// clients hold a connection open across the many small requests of a query
	// batch, so it must be well clear of the gap between them.
	idleTimeout = 2 * time.Minute

	// shutdownGrace bounds the wait for in-flight requests on shutdown. An
	// import runs synchronously inside PUT /{hash}.narinfo, so this is also how
	// long a restart lets nix-store --import finish. Before there was a
	// shutdown path at all, SIGTERM was ignored and an import ran until
	// systemd's TimeoutStopSec (90s by default) escalated to SIGKILL; staying
	// well inside that ceiling while keeping most of the old window means
	// leaving a restart no shorter than it used to be for the common closure.
	//
	// ponytail: an import longer than this is still cut off. Draining imports
	// properly means moving them off the request goroutine, which is a much
	// larger change than a restart-time truncation warrants.
	shutdownGrace = 60 * time.Second
)

// newHTTPServer builds a server with the timeouts every listener shares.
func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// serveHTTP serves handler on ln and shuts the server down when ctx is
// cancelled. Both goroutines join eg, so eg.Wait returns only once the server
// has really stopped — without the shutdown half, Serve never returns and Wait
// blocks past the point where the supervisor gives up and kills the process.
func serveHTTP(ctx context.Context, eg *errgroup.Group, ln net.Listener, handler http.Handler) {
	srv := newHTTPServer(handler)

	eg.Go(func() error {
		<-ctx.Done()

		// Detached from ctx: it is already cancelled, and the grace period is
		// the whole point.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()

		err := srv.Shutdown(shutdownCtx)
		if err != nil {
			slog.Warn("serve: graceful shutdown timed out, dropping connections", "err", err)

			_ = srv.Close()
		}

		return nil
	})

	eg.Go(func() error {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return err
	})
}

// isLoopbackAddr reports whether a bound listener address is on loopback. A
// unix socket counts: it is reachable only through the filesystem, which has its
// own permissions.
func isLoopbackAddr(a net.Addr) bool {
	tcp, ok := a.(*net.TCPAddr)
	if !ok {
		return true
	}

	return tcp.IP.IsLoopback()
}

// localHandler is what a --listen socket puts in front of the cache. Such a
// listener is not wrapped in auth.Middleware — there is no WhoIs to ask about a
// connection that did not come off the tailnet — so it carries no identity, and
// a write on it can be neither attributed nor authorised. auth.ReadOnly refuses
// those writes; --local-write drops the wrapper and restores the old behaviour.
//
// gateDebug is deliberately absent for the same reason: it decides on a push
// grant, which nothing here has. /debug therefore keeps exactly the audience
// tsweb's own AllowDebugAccess gives it — loopback and tailnet peers — that it
// had before this gate existed.
//
// The one write-shaped corner of that surface needs a path rule rather than a
// method one: tsweb's force-GC handler ignores the method and the debug index
// links it with a plain <a href>, so the button sends GET. auth.ReadOnly refuses
// it by path for that reason. Everything else under /debug is introspection and
// stays reachable, which is what makes profiling a running cache possible.
func localHandler(next http.Handler, localWrite bool) http.Handler {
	if localWrite {
		return next
	}

	return auth.ReadOnly(next)
}

// tsnetHandler is everything a tsnet listener puts in front of the cache: the
// push-grant middleware for writes, and the debug gate for /debug. A named
// function rather than two calls inline at the listener, because that is the
// only place either wrapper is applied and a listener is not something a test
// can stand up — dropping one from the chain would otherwise leave every test
// green while the debug surface went open to the tailnet.
func tsnetHandler(whoiser auth.WhoIser, next http.Handler) http.Handler {
	return auth.Middleware(whoiser)(gateDebug(whoiser, next))
}

// gateDebug puts tsweb's debug surface behind the push grant. tsweb admits any
// tailnet address and auth.Middleware exempts safe methods, so without this
// every peer — including ones deliberately denied push — can read argv,
// hostname and goroutine stacks, force a stop-the-world GC per request, and
// start an unbounded CPU profile. Pushers are already inside the trust
// boundary, so gating on the same grant costs an operator nothing.
func gateDebug(whoiser auth.WhoIser, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/debug") {
			next.ServeHTTP(w, r)

			return
		}

		who, err := whoiser.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil || who == nil || !auth.HasPushCap(who.CapMap) {
			slog.Warn("serve: debug request rejected, no push grant",
				"remote", r.RemoteAddr, "path", auth.LogPath(r.URL.Path), "err", err)
			http.Error(w, "debug access requires push grant", http.StatusForbidden)

			return
		}

		next.ServeHTTP(w, r)
	})
}

type tsnetSpec struct {
	hostname    string
	authKeyFile string
	dir         string
	port        string
	control     string
}

var (
	errTsnetMalformedPart = errors.New("parseTsnetSpec: expected key=value")
	errTsnetUnknownKey    = errors.New("parseTsnetSpec: unknown key")
	// ponytail: tsnet could serve TLS via its cert helpers, but the tailnet is
	// already encrypted, so the ceiling is that a browser sees plain HTTP.
	// Rejecting tls=true beats silently serving it to a config that asked for TLS.
	errTsnetTLSUnsupported = errors.New(
		"parseTsnetSpec: tls=true is not supported; tsnixcache serves plain HTTP over the tailnet",
	)
)

func parseTsnetSpec(s string) (tsnetSpec, error) {
	var spec tsnetSpec

	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return tsnetSpec{}, fmt.Errorf("%w, got %q", errTsnetMalformedPart, part)
		}

		switch k {
		case "hostname":
			spec.hostname = v
		case "authkey-file":
			spec.authKeyFile = v
		case "dir":
			spec.dir = v
		case "port":
			spec.port = v
		case "tls":
			enabled, err := strconv.ParseBool(v)
			if err != nil {
				return tsnetSpec{}, fmt.Errorf("parseTsnetSpec: tls=%q: %w", v, err)
			}

			if enabled {
				return tsnetSpec{}, errTsnetTLSUnsupported
			}
		case "control":
			spec.control = v
		default:
			return tsnetSpec{}, fmt.Errorf("%w %q in %q", errTsnetUnknownKey, k, s)
		}
	}

	if spec.hostname == "" {
		spec.hostname = "tsnixcache"
	}

	return spec, nil
}
