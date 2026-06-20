// Package store reads the Nix SQLite database to serve narinfo metadata.
package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/kradalby/tsnixcache/nixbase32"
)

// ErrNotFound is returned when the requested store path is not in the database.
var ErrNotFound = errors.New("store: path not found")

// Store provides read-only access to the Nix store database.
type Store struct {
	db       *sql.DB
	storeDir string
	stmtMain *sql.Stmt // SELECT … FROM ValidPaths WHERE path LIKE ?
	stmtRefs *sql.Stmt // SELECT vp.path FROM Refs … WHERE referrer = ?
}

// PathInfo holds narinfo metadata for a single store path.
type PathInfo struct {
	StorePath  string // full /nix/store/... path
	NarHash    string // "sha256:<nix-base32>"
	NarSize    int64
	References []string // sorted full /nix/store/... paths (self-reference excluded)
	Deriver    string   // full drv path or empty
	Sigs       []string // "name:base64" strings
	CA         string
}

const queryMain = `
SELECT id, path, hash, narSize, deriver, sigs, ca
FROM ValidPaths
WHERE path LIKE ?`

const queryRefs = `
SELECT vp.path
FROM Refs r
JOIN ValidPaths vp ON vp.id = r.reference
WHERE r.referrer = ?`

// Open opens the Nix database at dbPath in read-only mode.
// storeDir is the store prefix (typically "/nix/store").
func Open(dbPath, storeDir string) (*Store, error) {
	dsn := "file:" + dbPath + "?mode=ro&immutable=1&_journal_mode=WAL"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", dbPath, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", dbPath, err)
	}
	stmtMain, err := db.Prepare(queryMain)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store: prepare main query: %w", err)
	}
	stmtRefs, err := db.Prepare(queryRefs)
	if err != nil {
		stmtMain.Close()
		db.Close()
		return nil, fmt.Errorf("store: prepare refs query: %w", err)
	}
	return &Store{db: db, storeDir: storeDir, stmtMain: stmtMain, stmtRefs: stmtRefs}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	s.stmtMain.Close()
	s.stmtRefs.Close()
	return s.db.Close()
}

// PathInfo returns metadata for the store path whose hash part matches hashPart.
// hashPart is the 32-character nix-base32 component of the store path (e.g. "abc123...").
// Returns ErrNotFound if no such path exists in the database.
func (s *Store) PathInfo(ctx context.Context, hashPart string) (*PathInfo, error) {
	pattern := s.storeDir + "/" + hashPart + "-%"

	var (
		id          int64
		path        string
		hashHex     string
		narSize     int64
		deriverNull sql.NullString
		sigsNull    sql.NullString
		caNull      sql.NullString
	)

	err := s.stmtMain.QueryRowContext(ctx, pattern).
		Scan(&id, &path, &hashHex, &narSize, &deriverNull, &sigsNull, &caNull)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: query ValidPaths: %w", err)
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
		if err := rows.Scan(&refPath); err != nil {
			return nil, fmt.Errorf("store: scan ref: %w", err)
		}
		if refPath != path {
			refs = append(refs, refPath)
		}
	}
	if err := rows.Err(); err != nil {
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
		NarSize:    narSize,
		References: refs,
		Deriver:    deriverNull.String,
		Sigs:       sigs,
		CA:         caNull.String,
	}, nil
}
