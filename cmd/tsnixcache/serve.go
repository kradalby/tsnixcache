package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/peterbourgon/ff/v3/ffcli"
	"golang.org/x/sync/errgroup"
	"tailscale.com/tsnet"

	"github.com/kradalby/tsnixcache/auth"
	"github.com/kradalby/tsnixcache/cache"
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
	spoolDir := fs.String("spool-dir", "/tmp/tsnixcache-spool", "directory for spooled NAR files")
	gcRootDir := fs.String("gcroot-dir", "/nix/var/nix/gcroots/tsnixcache", "directory for GC root symlinks")
	serveCompression := fs.String("serve-compression", "none", "compression for served NARs (none or zstd)")
	nixStoreURI := fs.String("nix-store-uri", "auto", "Nix store URI passed to --store (auto = system daemon)")

	var listenAddrs multiFlag
	fs.Var(&listenAddrs, "listen", "local listen address (repeatable, default 127.0.0.1:5000)")

	var tsnetFlags multiFlag
	fs.Var(&tsnetFlags, "tsnet", "tsnet instance spec: hostname=...,authkey-file=...,dir=...,port=80,tls=false,control=... (repeatable)")

	return &ffcli.Command{
		Name:       "serve",
		ShortUsage: "tsnixcache serve [flags]",
		ShortHelp:  "Run the binary cache HTTP server.",
		FlagSet:    fs,
		Exec: func(ctx context.Context, args []string) error {
			return runServe(ctx, serveConfig{
				dbPath:           *dbPath,
				storeDir:         *storeDir,
				signKeyFile:      *signKeyFile,
				priority:         *priority,
				spoolDir:         *spoolDir,
				gcRootDir:        *gcRootDir,
				serveCompression: *serveCompression,
				nixStoreURI:      *nixStoreURI,
				listenAddrs:      listenAddrs,
				tsnetSpecs:       tsnetFlags,
			})
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
	listenAddrs      []string
	tsnetSpecs       []string
}

func runServe(ctx context.Context, cfg serveConfig) error {
	// Parse signing key if provided.
	var signer cache.Signer
	if cfg.signKeyFile != "" {
		data, err := os.ReadFile(cfg.signKeyFile)
		if err != nil {
			return fmt.Errorf("serve: read sign-key-file: %w", err)
		}
		sk, err := signing.ParseSecretKey(strings.TrimSpace(string(data)))
		if err != nil {
			return fmt.Errorf("serve: parse sign key: %w", err)
		}
		signer = sk
	}

	// Open the Nix store DB.
	st, err := store.Open(cfg.dbPath, cfg.storeDir)
	if err != nil {
		return fmt.Errorf("serve: open store: %w", err)
	}

	// Ensure spool dir exists.
	if err := os.MkdirAll(cfg.spoolDir, 0o755); err != nil {
		return fmt.Errorf("serve: create spool dir: %w", err)
	}

	// Build the importer.
	nixURI := cfg.nixStoreURI
	if nixURI == "auto" {
		nixURI = ""
	}
	imp := &niximport.Importer{
		SpoolDir:    cfg.spoolDir,
		GCRootDir:   cfg.gcRootDir,
		NixStoreURI: nixURI,
	}

	// Build the cache server.
	srv := cache.New(st, signer, cfg.priority, cfg.spoolDir, cfg.serveCompression, imp.Import)

	handler := srv.Handler()

	// Default listen address.
	addrs := cfg.listenAddrs
	if len(addrs) == 0 {
		addrs = []string{"127.0.0.1:5000"}
	}

	eg, _ := errgroup.WithContext(ctx)

	// Local listeners.
	for _, addr := range addrs {
		addr := addr
		eg.Go(func() error {
			slog.Info("serve: listening", "addr", addr)
			return http.ListenAndServe(addr, handler)
		})
	}

	// tsnet listeners.
	for _, spec := range cfg.tsnetSpecs {
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
		}
		if err := ts.Start(); err != nil {
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

		tsHandler := auth.Middleware(lc)(handler)
		slog.Info("serve: tsnet listening", "hostname", tsSpec.hostname, "port", port)

		eg.Go(func() error {
			return http.Serve(ln, tsHandler)
		})
	}

	return eg.Wait()
}

type tsnetSpec struct {
	hostname    string
	authKeyFile string
	dir         string
	port        string
	tls         bool
	control     string
}

func parseTsnetSpec(s string) (tsnetSpec, error) {
	parts := strings.Split(s, ",")
	var spec tsnetSpec
	for _, part := range parts {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
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
			b, err := strconv.ParseBool(v)
			if err != nil {
				return tsnetSpec{}, fmt.Errorf("parseTsnetSpec: tls=%q: %w", v, err)
			}
			spec.tls = b
		case "control":
			spec.control = v
		}
	}
	if spec.hostname == "" {
		return tsnetSpec{}, fmt.Errorf("parseTsnetSpec: hostname is required")
	}
	return spec, nil
}
