// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package store reads the Nix SQLite database to serve narinfo metadata.
package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/kradalby/tsnixcache/nixbase32"
)

// ErrNotFound is returned when the requested store path is not in the database.
var ErrNotFound = errors.New("store: path not found")

// Store provides read-only access to the Nix store database.
type Store struct {
	db       *sql.DB
	storeDir string
	stmtMain *sql.Stmt // SELECT … FROM ValidPaths WHERE path >= ? AND path < ?
	stmtRefs *sql.Stmt // SELECT vp.path FROM Refs … WHERE referrer = ?

	// warnedUnservable holds the paths already warned about, so a client
	// looping on one unservable hash part cannot turn a rare condition into
	// unbounded log volume. Keyed by the path from the row, not by client
	// input, so it is bounded by the number of broken rows in the DB.
	warnedUnservable sync.Map
}

// PathInfo holds narinfo metadata for a single store path.
type PathInfo struct {
	StorePath  string // full /nix/store/... path
	NarHash    string // "sha256:<nix-base32>"
	NarSize    int64
	References []string // sorted full /nix/store/... paths, exactly as the Nix DB holds them (a self-reference is kept)
	Deriver    string   // full drv path or empty
	Sigs       []string // "name:base64" strings
	CA         string
}

// maxConns bounds connection-local caches and descriptors during lookup bursts.
// Matching idle and open limits preserves warm connections and prepared statements.
const maxConns = 8

// A half-open range on path, unlike LIKE, is sargable against the ValidPaths
// unique index: LIKE is case-insensitive by default (case_sensitive_like=0)
// while the column collates BINARY, so SQLite cannot use the index and scans
// the whole table on every lookup.
const queryMain = `
SELECT id, path, hash, narSize, deriver, sigs, ca
FROM ValidPaths
WHERE path >= ? AND path < ?`

const queryRefs = `
SELECT vp.path
FROM Refs r
JOIN ValidPaths vp ON vp.id = r.reference
WHERE r.referrer = ?`

// Open opens the Nix database at dbPath in read-only mode.
// storeDir is the store prefix (typically "/nix/store").
func Open(ctx context.Context, dbPath, storeDir string) (*Store, error) {
	// A trailing separator on --store-dir would put a second slash into every
	// range bound ("/nix/store//<hash>-"), which matches no row, so every
	// lookup would 404 with nothing logged anywhere.
	storeDir = strings.TrimSuffix(storeDir, "/")

	dsn := ReadOnlyDSN(dbPath)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", dbPath, err)
	}

	// database/sql keeps 2 idle connections by default; every connection it
	// drops takes its prepared statements with it, so concurrent narinfo
	// lookups would spend their time re-opening and re-preparing. Idle and
	// open are set to the same number so the pool never opens a connection it
	// will immediately throw away.
	// GOMAXPROCS, not NumCPU: it tracks the cgroup CPU limit the scheduler
	// honours, and hence the real concurrency of the handler goroutines.
	conns := min(max(4, runtime.GOMAXPROCS(0)), maxConns)
	db.SetMaxIdleConns(conns)
	db.SetMaxOpenConns(conns)

	err = db.PingContext(ctx)
	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("store: ping %s: %w", dbPath, explainReadOnly(err, dbPath))
	}

	stmtMain, err := db.PrepareContext(ctx, queryMain)
	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("store: prepare main query: %w", explainReadOnly(err, dbPath))
	}

	defer func() {
		if err != nil {
			_ = stmtMain.Close()
		}
	}()

	stmtRefs, err := db.PrepareContext(ctx, queryRefs)
	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("store: prepare refs query: %w", explainReadOnly(err, dbPath))
	}

	return &Store{db: db, storeDir: storeDir, stmtMain: stmtMain, stmtRefs: stmtRefs}, nil
}

// explainReadOnly annotates the SQLITE_READONLY_* family, which is otherwise
// unreadable coming from a handle we opened read-only. Nix keeps its database
// in WAL mode, and SQLite cannot read a WAL database without a wal-index
// (db.sqlite-shm) — creating it writes to the database's *directory*, which
// mode=ro does not excuse. An unprivileged service that cannot write
// /nix/var/nix/db therefore gets "attempt to write a readonly database", or
// SQLITE_READONLY_RECOVERY when a crashed writer left a stale wal-index
// behind, neither of which names the actual problem.
func explainReadOnly(err error, dbPath string) error {
	var serr *sqlite.Error
	if !errors.As(err, &serr) || serr.Code()&0xff != sqlite3.SQLITE_READONLY {
		return err
	}

	return fmt.Errorf(
		"%w: %s is a WAL database and SQLite must create the wal-index %s-shm to read it, but %s is not writable; "+
			"another process (nix-daemon) has to hold the database open, or the directory must be writable",
		err, dbPath, dbPath, filepath.Dir(dbPath),
	)
}

// PathCount returns the total number of valid store paths in the database.
func (s *Store) PathCount(ctx context.Context) (int64, error) {
	var n int64

	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM ValidPaths").Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count paths: %w", err)
	}

	return n, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	_ = s.stmtMain.Close() // #nosec G104 -- best-effort cleanup
	_ = s.stmtRefs.Close() // #nosec G104 -- best-effort cleanup

	return s.db.Close()
}

// PathInfo returns metadata for the store path whose hash part matches hashPart.
// hashPart is the 32-character nix-base32 component of the store path (e.g. "abc123...");
// anything else is rejected, since it cannot name a store path.
// Returns ErrNotFound if no such path exists in the database.
func (s *Store) PathInfo(ctx context.Context, hashPart string) (*PathInfo, error) {
	// A hash part that isn't 32 nix-base32 characters names no store path.
	// Rejecting it keeps client-supplied text out of the query range and out
	// of the constructed URLs downstream.
	if !nixbase32.ValidHashPart(hashPart) {
		return nil, ErrNotFound
	}

	// '.' (0x2E) is the immediate successor of '-' (0x2D) and path collates
	// BINARY, so [lo, hi) is exactly the set of paths with this hash part.
	lo := s.storeDir + "/" + hashPart + "-"
	hi := s.storeDir + "/" + hashPart + "."

	var (
		id          int64
		path        string
		hashHex     string
		narSize     sql.NullInt64
		deriverNull sql.NullString
		sigsNull    sql.NullString
		caNull      sql.NullString
	)

	err := s.stmtMain.QueryRowContext(ctx, lo, hi).
		Scan(&id, &path, &hashHex, &narSize, &deriverNull, &sigsNull, &caNull)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("store: query ValidPaths: %w", err)
	}

	// NarSize is covered by the narinfo signature, so serving 0 would hand out
	// a narinfo every client rejects, and a negative one wraps to ~1.8e19 when
	// the cache converts it to uint64. NULL, 0 and negative are all the same
	// unusable row. Treat it as such — but say so once per path: to a client
	// this is a plain 404, indistinguishable from a path the machine never had,
	// so without a log line nobody ever learns that the cache is silently
	// under-serving a path it does hold.
	if !narSize.Valid || narSize.Int64 <= 0 {
		_, warned := s.warnedUnservable.LoadOrStore(path, struct{}{})
		if !warned {
			// NULL scans as 0, which is exactly as unusable, so the two need
			// not be told apart in the log.
			slog.Warn("store: skipping path with unusable narSize", "path", path, "narSize", narSize.Int64)
		}

		return nil, ErrNotFound
	}

	// Convert base16 hash to nix-base32; strip optional "sha256:" prefix.
	hexStr := strings.TrimPrefix(hashHex, "sha256:")

	hashBytes, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("store: decode hash %q: %w", hashHex, err)
	}

	narHash := "sha256:" + nixbase32.EncodeToString(hashBytes)

	// Fetch references via the prepared statement.
	rows, err := s.stmtRefs.QueryContext(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store: query Refs: %w", err)
	}
	defer rows.Close()

	var refs []string

	for rows.Next() {
		var refPath string

		err = rows.Scan(&refPath)
		if err != nil {
			return nil, fmt.Errorf("store: scan ref: %w", err)
		}

		// Self-references are kept: Nix records them (glibc references itself)
		// and they are covered by the upstream signature, so dropping one
		// invalidates every signature we pass through.
		refs = append(refs, refPath)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("store: iterate refs: %w", err)
	}

	sort.Strings(refs)

	var sigs []string
	if sigsNull.Valid && sigsNull.String != "" {
		sigs = strings.Fields(sigsNull.String)
	}

	return &PathInfo{
		StorePath:  path,
		NarHash:    narHash,
		NarSize:    narSize.Int64,
		References: refs,
		Deriver:    deriverNull.String,
		Sigs:       sigs,
		CA:         caNull.String,
	}, nil
}
