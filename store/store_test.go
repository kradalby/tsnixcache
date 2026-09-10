// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/kradalby/tsnixcache/nixbase32"
)

// Fixture hex hashes (32 bytes = 64 hex chars each) and their pre-computed
// nix-base32 representations (computed from nixbase32.EncodeToString).
//
// Nix store path hash parts (32 chars) are nix-base32 of 20-byte values; the
// fixtures below are plain 32-char strings drawn from the nix-base32 alphabet,
// which is what PathInfo requires of a hash part.
const (
	// narHash hex values (32-byte SHA256).
	hexHash1 = "0000000000000000000000000000000000000000000000000000000000000000"
	hexHash2 = "aabbccddaabbccddaabbccddaabbccddaabbccddaabbccddaabbccddaabbccdd"
	hexHash3 = "1111111111111111111111111111111111111111111111111111111111111111"
	hexHash4 = "2222222222222222222222222222222222222222222222222222222222222222"
	hexHash5 = "3333333333333333333333333333333333333333333333333333333333333333"
	hexHash6 = "4444444444444444444444444444444444444444444444444444444444444444"

	// Path hash parts (32 chars, valid nix-base32 chars).
	hashPart1 = "00000000000000000000000000000001"
	hashPart2 = "00000000000000000000000000000002"
	hashPart3 = "00000000000000000000000000000003"
	hashPart4 = "00000000000000000000000000000004"
	hashPart5 = "00000000000000000000000000000005"
	hashPart6 = "00000000000000000000000000000006"

	// hashPartUpper is 32 characters but 'A' is outside the nix-base32
	// alphabet, so it can never name a store path. The fixture holds a row
	// under it anyway, which is what makes the alphabet check observable: a
	// range lookup on this string would find the row.
	hashPartUpper = "0000000000000000000000000000000A"

	storeDir = "/nix/store"
)

func pathFor(hp, name string) string {
	return storeDir + "/" + hp + "-" + name
}

// nixSchema mirrors the parts of Nix's schema.sql that we read, indexes
// included: without them a fixture flatters a lookup that scans in production.
// registrationTime gets a default so fixtures need not supply it.
var nixSchema = []string{
	`CREATE TABLE ValidPaths (
		id               integer primary key autoincrement not null,
		path             text unique not null,
		hash             text not null,
		registrationTime integer not null default 0,
		deriver          text,
		narSize          integer,
		ultimate         integer,
		sigs             text,
		ca               text
	)`,
	`CREATE TABLE Refs (
		referrer  integer not null,
		reference integer not null,
		primary key (referrer, reference),
		foreign key (referrer) references ValidPaths(id) on delete cascade,
		foreign key (reference) references ValidPaths(id) on delete restrict
	)`,
	`CREATE INDEX IndexReferrer on Refs(referrer)`,
	`CREATE INDEX IndexReference on Refs(reference)`,
}

// createFixtureDB creates a temporary Nix-like SQLite database for testing.
func createFixtureDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.sqlite")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() }) // #nosec G104 -- test cleanup, error irrelevant

	createSchema(t, db)

	// path1: hello, no deps, with deriver and sigs
	mustInsert(t, db, pathFor(hashPart1, "hello-2.12.1"), hexHash1, 12345,
		sql.NullString{String: pathFor("drv00000000000000000000000000001", "hello.drv"), Valid: true},
		sql.NullString{String: "cache.example.org-1:AAAA==", Valid: true},
		sql.NullString{})

	// path2: openssl, references path3 (libc)
	mustInsert(t, db, pathFor(hashPart2, "openssl-3.0.0"), hexHash2, 99999,
		sql.NullString{String: pathFor("drv00000000000000000000000000002", "openssl.drv"), Valid: true},
		sql.NullString{String: "cache.example.org-1:BBBB==", Valid: true},
		sql.NullString{})

	// path3: libc (dep of path2), no deps, no deriver
	mustInsert(t, db, pathFor(hashPart3, "glibc-2.38"), hexHash3, 55555,
		sql.NullString{},
		sql.NullString{},
		sql.NullString{String: "fixed:r:sha256:abc", Valid: true})

	// path4: self-referencing package (references itself)
	mustInsert(t, db, pathFor(hashPart4, "self-ref-1.0"), hexHash4, 1000,
		sql.NullString{},
		sql.NullString{},
		sql.NullString{})

	// path5: multiple sigs
	mustInsert(t, db, pathFor(hashPart5, "multi-sig-1.0"), hexHash5, 2000,
		sql.NullString{},
		sql.NullString{String: "key1:AAA== key2:BBB==", Valid: true},
		sql.NullString{})

	// path6: empty sigs (NULL)
	mustInsert(t, db, pathFor(hashPart6, "empty-sigs-1.0"), hexHash6, 3000,
		sql.NullString{},
		sql.NullString{},
		sql.NullString{})

	// A row whose hash part is not nix-base32 at all. Nix cannot produce one;
	// it is here so the alphabet check has something to refuse to serve.
	mustInsert(t, db, pathFor(hashPartUpper, "uppercase-1.0"), hexHash1, 4000,
		sql.NullString{},
		sql.NullString{},
		sql.NullString{})

	// Add refs: path2 references path3
	addRef(t, db, pathFor(hashPart2, "openssl-3.0.0"), pathFor(hashPart3, "glibc-2.38"))

	// path4 references itself
	addRef(t, db, pathFor(hashPart4, "self-ref-1.0"), pathFor(hashPart4, "self-ref-1.0"))

	return dbPath
}

// createSchema creates the two Nix DB tables we read, mirroring Nix's own
// schema — in particular the indexes the lookups depend on.
func createSchema(t *testing.T, db *sql.DB) {
	t.Helper()

	for _, stmt := range nixSchema {
		_, err := db.ExecContext(context.Background(), stmt)
		if err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

func mustInsert(t *testing.T, db *sql.DB, path, hash string, narSize int64, deriver, sigs, ca sql.NullString) {
	t.Helper()

	_, err := db.ExecContext(
		context.Background(),
		`INSERT INTO ValidPaths (path, hash, narSize, deriver, sigs, ca) VALUES (?, ?, ?, ?, ?, ?)`,
		path, hash, narSize, deriver, sigs, ca,
	)
	if err != nil {
		t.Fatalf("insert %s: %v", path, err)
	}
}

func addRef(t *testing.T, db *sql.DB, referrerPath, referencePath string) {
	t.Helper()

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO Refs (referrer, reference)
		SELECT r.id, e.id FROM ValidPaths r, ValidPaths e
		WHERE r.path = ? AND e.path = ?`, referrerPath, referencePath)
	if err != nil {
		t.Fatalf("addRef %s -> %s: %v", referrerPath, referencePath, err)
	}
}

// narHashFor returns the expected NarHash string for a hex-encoded hash.
func narHashFor(hexHash string) string {
	return "sha256:" + nixbase32.EncodeToString(hexDecodeStr(hexHash))
}

func hexDecodeStr(s string) []byte {
	b := make([]byte, len(s)/2)

	for i := 0; i < len(s); i += 2 {
		hi, lo := hexNibble(s[i]), hexNibble(s[i+1])
		b[i/2] = hi<<4 | lo
	}

	return b
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	default:
		panic("bad hex nibble")
	}
}

// TestPathInfoFound verifies basic field retrieval.
func TestPathInfoFound(t *testing.T) {
	dbPath := createFixtureDB(t)

	s, err := Open(context.Background(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	info, err := s.PathInfo(context.Background(), hashPart1)
	if err != nil {
		t.Fatalf("PathInfo: %v", err)
	}

	if info.StorePath != pathFor(hashPart1, "hello-2.12.1") {
		t.Errorf("StorePath = %q, want %q", info.StorePath, pathFor(hashPart1, "hello-2.12.1"))
	}

	if info.NarSize != 12345 {
		t.Errorf("NarSize = %d, want 12345", info.NarSize)
	}

	want := pathFor("drv00000000000000000000000000001", "hello.drv")
	if info.Deriver != want {
		t.Errorf("Deriver = %q, want %q", info.Deriver, want)
	}

	if len(info.Sigs) != 1 || info.Sigs[0] != "cache.example.org-1:AAAA==" {
		t.Errorf("Sigs = %v, want [cache.example.org-1:AAAA==]", info.Sigs)
	}
}

// TestPathInfoNotFound verifies ErrNotFound for missing paths.
func TestPathInfoNotFound(t *testing.T) {
	dbPath := createFixtureDB(t)

	s, err := Open(context.Background(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	_, err = s.PathInfo(context.Background(), "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz9")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestNarHashConversion verifies base16 → nix-base32 conversion.
func TestNarHashConversion(t *testing.T) {
	dbPath := createFixtureDB(t)

	s, err := Open(context.Background(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	info, err := s.PathInfo(context.Background(), hashPart1)
	if err != nil {
		t.Fatalf("PathInfo: %v", err)
	}

	want := narHashFor(hexHash1)
	if info.NarHash != want {
		t.Errorf("NarHash = %q, want %q", info.NarHash, want)
	}
}

// TestNarHashConversionSha256Prefix verifies stripping of "sha256:" prefix in hash field.
func TestNarHashConversionSha256Prefix(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.sqlite")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	createSchema(t, db)

	const prefixedHash = "sha256:" + hexHash2

	_, _ = db.ExecContext(context.Background(), `INSERT INTO ValidPaths (path, hash, narSize) VALUES (?, ?, ?)`,
		pathFor(hashPart2, "openssl-3.0.0"), prefixedHash, 99999)

	_ = db.Close() // #nosec G104 -- best-effort cleanup

	s, err := Open(context.Background(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	info, err := s.PathInfo(context.Background(), hashPart2)
	if err != nil {
		t.Fatalf("PathInfo: %v", err)
	}

	want := narHashFor(hexHash2)
	if info.NarHash != want {
		t.Errorf("NarHash = %q, want %q", info.NarHash, want)
	}
}

// TestReferencesSorted verifies that references are returned sorted.
func TestReferencesSorted(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.sqlite")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	createSchema(t, db)

	// Insert main package and 3 deps inserted in reverse-alphabetical order.
	paths := []string{
		pathFor(hashPart1, "pkg-1.0"),
		pathFor(hashPart4, "zzz-dep-3.0"),
		pathFor(hashPart3, "mmm-dep-2.0"),
		pathFor(hashPart2, "aaa-dep-1.0"),
	}

	hashes := []string{hexHash1, hexHash4, hexHash3, hexHash2}

	for i, p := range paths {
		_, _ = db.ExecContext(context.Background(), `INSERT INTO ValidPaths (path, hash, narSize) VALUES (?, ?, ?)`, p, hashes[i], 1000)
	}

	// Add refs: pkg -> zzz, mmm, aaa
	for _, dep := range paths[1:] {
		_, _ = db.ExecContext(context.Background(), `INSERT INTO Refs (referrer, reference)
			SELECT r.id, e.id FROM ValidPaths r, ValidPaths e WHERE r.path=? AND e.path=?`,
			paths[0], dep)
	}

	_ = db.Close() // #nosec G104 -- best-effort cleanup

	s, err := Open(context.Background(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	info, err := s.PathInfo(context.Background(), hashPart1)
	if err != nil {
		t.Fatalf("PathInfo: %v", err)
	}

	if len(info.References) != 3 {
		t.Fatalf("want 3 refs, got %d: %v", len(info.References), info.References)
	}

	wantRefs := []string{paths[3], paths[2], paths[1]} // aaa, mmm, zzz
	sort.Strings(wantRefs)

	for i, got := range info.References {
		if got != wantRefs[i] {
			t.Errorf("References[%d] = %q, want %q", i, got, wantRefs[i])
		}
	}
}

// TestReferencesSelfPreserved verifies that a self-reference is served as the
// Nix DB holds it: it is covered by the upstream signature we pass through, so
// dropping it would make the narinfo we serve unverifiable.
func TestReferencesSelfPreserved(t *testing.T) {
	dbPath := createFixtureDB(t)

	s, err := Open(context.Background(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	info, err := s.PathInfo(context.Background(), hashPart4)
	if err != nil {
		t.Fatalf("PathInfo: %v", err)
	}

	want := []string{pathFor(hashPart4, "self-ref-1.0")}
	if len(info.References) != len(want) || info.References[0] != want[0] {
		t.Errorf("References = %v, want %v", info.References, want)
	}
}

// TestPathInfoRejectsInvalidHashPart verifies that anything which cannot name a
// store path is rejected before the query runs. With the old LIKE lookup the
// wildcard cases returned a signed narinfo for an arbitrary store path.
//
// The first two cases are the ones the validation itself has to catch: the
// fixture holds a matching row, so the range predicate would happily serve them.
// The rest are already dead by the range predicate and are kept to pin that.
func TestPathInfoRejectsInvalidHashPart(t *testing.T) {
	dbPath := createFixtureDB(t)

	s, err := Open(context.Background(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	tests := []struct {
		name     string
		hashPart string
	}{
		{"hash part plus basename", hashPart1 + "-hello"},
		{"outside nix-base32", hashPartUpper},
		{"percent wildcard", "%"},
		{"underscore wildcards", strings.Repeat("_", 32)},
		{"percent suffix", hashPart1[:31] + "%"},
		{"too short", hashPart1[:31]},
		{"too long", hashPart1 + "0"},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := s.PathInfo(t.Context(), tt.hashPart)
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("PathInfo(%q) = %+v, %v; want ErrNotFound", tt.hashPart, info, err)
			}
		})
	}
}

// TestPathInfoCaseSensitive verifies the lookup is byte-exact. SQLite's LIKE is
// case-insensitive by default, so the old query matched a store path whose hash
// part differed only in case — the same reason it could not use the index.
func TestPathInfoCaseSensitive(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.sqlite")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	createSchema(t, db)

	// A path whose hash part differs from hashPart1 only in case.
	upper := strings.ToUpper(hashPart1[:31]) + "A"
	lower := strings.ToLower(upper)

	_, err = db.ExecContext(t.Context(), `INSERT INTO ValidPaths (path, hash, narSize) VALUES (?, ?, ?)`,
		pathFor(upper, "pkg-1.0"), hexHash1, 1000)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	_ = db.Close() // #nosec G104 -- best-effort cleanup

	s, err := Open(t.Context(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	info, err := s.PathInfo(t.Context(), lower)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("PathInfo(%q) = %+v, %v; want ErrNotFound", lower, info, err)
	}
}

// TestPathInfoUsesIndex verifies the lookup predicate is sargable against the
// ValidPaths unique index. On the real 68k-row Nix DB the LIKE form scanned the
// whole table (8 ms per lookup) — once per closure path with WantMassQuery set.
//
// It EXPLAINs queryMain, the statement PathInfo actually prepares, so any
// rewrite that gives up the index fails here even if it stays byte-exact.
func TestPathInfoUsesIndex(t *testing.T) {
	dbPath := createFixtureDB(t)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() }) // #nosec G104 -- test cleanup

	plan := func(query string, args ...any) string {
		t.Helper()

		rows, err := db.QueryContext(t.Context(), query, args...)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		defer rows.Close()

		var out []string

		for rows.Next() {
			var (
				id, parent, notUsed int64
				detail              string
			)

			err = rows.Scan(&id, &parent, &notUsed, &detail)
			if err != nil {
				t.Fatalf("scan plan: %v", err)
			}

			out = append(out, detail)
		}

		err = rows.Err()
		if err != nil {
			t.Fatalf("iterate plan: %v", err)
		}

		return strings.Join(out, "; ")
	}

	const explainLike = `EXPLAIN QUERY PLAN
SELECT id, path, hash, narSize, deriver, sigs, ca
FROM ValidPaths
WHERE path LIKE ?`

	got := plan("EXPLAIN QUERY PLAN"+queryMain,
		storeDir+"/"+hashPart1+"-", storeDir+"/"+hashPart1+".")
	if !strings.Contains(got, "USING INDEX") {
		t.Errorf("plan for queryMain = %q, want an index lookup", got)
	}

	// Sanity check that this fixture would have shown the problem: the old LIKE
	// predicate scans the same table.
	old := plan(explainLike, storeDir+"/"+hashPart1+"-%")
	if !strings.Contains(old, "SCAN") {
		t.Errorf("LIKE predicate plan = %q, expected a full scan on this SQLite build", old)
	}
}

// TestNullNarSize verifies that a row with no narSize is reported as not found
// rather than as a query error: narSize is covered by the narinfo signature, so
// there is nothing serveable to build from such a row. The client sees a plain
// 404 either way, so the log line asserted here is an operator's only sign that
// the cache holds a path it cannot serve.
func TestNullNarSize(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.sqlite")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	createSchema(t, db)

	_, err = db.ExecContext(t.Context(), `INSERT INTO ValidPaths (path, hash) VALUES (?, ?)`,
		pathFor(hashPart1, "no-narsize-1.0"), hexHash1)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	_ = db.Close() // #nosec G104 -- best-effort cleanup

	s, err := Open(t.Context(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var logBuf bytes.Buffer

	prev := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))

	t.Cleanup(func() { slog.SetDefault(prev) })

	info, err := s.PathInfo(t.Context(), hashPart1)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("PathInfo = %+v, %v; want ErrNotFound", info, err)
	}

	// Repeated: the hash part comes straight from the client's URL, so a client
	// can hold a loop on one unservable path. The operator needs the line, but
	// once — not once per request.
	for range 20 {
		_, _ = s.PathInfo(t.Context(), hashPart1)
	}

	got := strings.Count(logBuf.String(), pathFor(hashPart1, "no-narsize-1.0"))
	if got != 1 {
		t.Errorf("unservable row logged %d times over 21 lookups, want exactly 1", got)
	}
}

// TestPoolKeepsConnectionsIdle verifies the store retains more than
// database/sql's default two idle connections. Every connection the pool drops
// takes its prepared statements with it, so a narinfo burst against a
// two-connection pool spends its time re-opening and re-preparing.
//
// The connections are checked out by hand rather than driven by concurrent
// lookups: how many connections a burst opens is a property of the scheduler
// (one, on a single-CPU runner), whereas how many survive being handed back is
// the idle ceiling this test is about. sql.DBStats does not expose
// MaxIdleConns, so the ceiling is observed through what the pool keeps.
func TestPoolKeepsConnectionsIdle(t *testing.T) {
	dbPath := createFixtureDB(t)

	s, err := Open(t.Context(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// The floor of the configured size, min(max(4, GOMAXPROCS), 8), so the
	// expectation holds on any machine.
	const want = 4

	conns := make([]*sql.Conn, 0, want)

	for range want {
		// Held simultaneously, so each has to come from a distinct connection.
		conn, err := s.db.Conn(t.Context())
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}

		conns = append(conns, conn)
	}

	for _, conn := range conns {
		err = conn.Close()
		if err != nil {
			t.Fatalf("close conn: %v", err)
		}
	}

	idle := s.db.Stats().Idle
	if idle < want {
		t.Errorf("Idle = %d after releasing %d simultaneous connections, want %d: database/sql's default of 2 idle would have closed the rest", idle, want, want)
	}
}

// TestPoolBoundsOpenConnections verifies the pool has a ceiling at all.
// database/sql defaults to unlimited, so a burst of narinfo lookups opened a
// SQLite connection per request — each with its own libc thread state, page
// cache and file descriptors. Against the real store DB the bounded pool also
// measured roughly twice the lookup throughput of the unbounded one.
func TestPoolBoundsOpenConnections(t *testing.T) {
	dbPath := createFixtureDB(t)

	s, err := Open(t.Context(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// The configured size is min(max(4, GOMAXPROCS), 8) on any machine.
	got := s.db.Stats().MaxOpenConnections
	if got < 4 || got > 8 {
		t.Errorf("MaxOpenConnections = %d, want between 4 and 8 (0 means unlimited)", got)
	}
}

// TestUnusableNarSize verifies that a stored narSize which cannot yield a
// serveable narinfo is treated exactly like a missing one. 0 fails every
// client's signature check, and a negative value wraps to ~1.8e19 when the
// narinfo layer converts it to uint64.
func TestUnusableNarSize(t *testing.T) {
	for name, narSize := range map[string]int64{"zero": 0, "negative": -1} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "db.sqlite")

			db, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatalf("open db: %v", err)
			}

			createSchema(t, db)

			_, err = db.ExecContext(t.Context(), `INSERT INTO ValidPaths (path, hash, narSize) VALUES (?, ?, ?)`,
				pathFor(hashPart1, "bad-narsize-1.0"), hexHash1, narSize)
			if err != nil {
				t.Fatalf("insert: %v", err)
			}

			_ = db.Close() // #nosec G104 -- best-effort cleanup

			s, err := Open(t.Context(), dbPath, storeDir)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer s.Close()

			info, err := s.PathInfo(t.Context(), hashPart1)
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("PathInfo = %+v, %v; want ErrNotFound", info, err)
			}
		})
	}
}

// TestPathCount verifies the count backing /health and the Prometheus gauge.
// Both report it verbatim, so a wrong number here is an operator's whole view
// of the store.
func TestPathCount(t *testing.T) {
	dbPath := createFixtureDB(t)

	s, err := Open(t.Context(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Every row the fixture inserts, including the ones PathInfo refuses to
	// serve: the count is of what the Nix DB holds, not of what we would serve.
	const want = 7

	got, err := s.PathCount(t.Context())
	if err != nil {
		t.Fatalf("PathCount: %v", err)
	}

	if got != want {
		t.Errorf("PathCount = %d, want %d", got, want)
	}
}

// TestOpenTrimsStoreDir verifies a trailing separator on the store directory is
// dropped. It is concatenated into the lookup's range bounds, so "/nix/store/"
// would ask for "/nix/store//<hash>-", match nothing, and turn every request
// into a 404 without a single error anywhere.
func TestOpenTrimsStoreDir(t *testing.T) {
	dbPath := createFixtureDB(t)

	s, err := Open(t.Context(), dbPath, storeDir+"/")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	info, err := s.PathInfo(t.Context(), hashPart1)
	if err != nil {
		t.Fatalf("PathInfo: %v", err)
	}

	if want := pathFor(hashPart1, "hello-2.12.1"); info.StorePath != want {
		t.Errorf("StorePath = %q, want %q", info.StorePath, want)
	}
}

// TestOpenWALReadOnlyDirExplains verifies the SQLITE_READONLY_* family arrives
// with an explanation. Nix's DB is in WAL mode and SQLite must create a
// wal-index beside it to read one, which the service cannot do:
// /nix/var/nix/db is root-owned and the unit mounts it read-only. The bare
// driver error is "attempt to write a readonly database (1544)" from a handle
// we opened read-only, which sends an operator looking in the wrong place.
func TestOpenWALReadOnlyDirExplains(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.sqlite")

	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode=WAL")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	createSchema(t, db)

	_ = db.Close() // #nosec G104 -- best-effort cleanup

	// The state a freshly booted machine is in: the WAL database is there, its
	// wal-index is not, and nothing else holds the database open.
	for _, suffix := range []string{"-shm", "-wal"} {
		err = os.Remove(dbPath + suffix)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", dbPath+suffix, err)
		}
	}

	err = os.Chmod(dir, 0o555) // #nosec G302 -- a read-only directory is the point
	if err != nil {
		t.Fatalf("chmod: %v", err)
	}

	// t.TempDir cannot remove a directory it may not write.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) // #nosec G104,G302 -- test cleanup

	_, err = Open(t.Context(), dbPath, storeDir)
	if err == nil {
		t.Fatal("Open succeeded on a WAL database in an unwritable directory")
	}

	for _, want := range []string{"wal-index", dir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Open error %q does not mention %q", err, want)
		}
	}
}

// TestSigsSplit verifies that space-separated sigs are parsed into a slice.
func TestSigsSplit(t *testing.T) {
	dbPath := createFixtureDB(t)

	s, err := Open(context.Background(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	info, err := s.PathInfo(context.Background(), hashPart5)
	if err != nil {
		t.Fatalf("PathInfo: %v", err)
	}

	wantSigs := []string{"key1:AAA==", "key2:BBB=="}
	if len(info.Sigs) != len(wantSigs) {
		t.Fatalf("Sigs = %v, want %v", info.Sigs, wantSigs)
	}

	for i, got := range info.Sigs {
		if got != wantSigs[i] {
			t.Errorf("Sigs[%d] = %q, want %q", i, got, wantSigs[i])
		}
	}
}

// TestEmptySigs verifies that NULL or empty sigs yields an empty slice.
func TestEmptySigs(t *testing.T) {
	dbPath := createFixtureDB(t)

	s, err := Open(context.Background(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// path6 has NULL sigs
	info, err := s.PathInfo(context.Background(), hashPart6)
	if err != nil {
		t.Fatalf("PathInfo: %v", err)
	}

	if len(info.Sigs) != 0 {
		t.Errorf("Sigs = %v, want empty", info.Sigs)
	}
}

// TestEmptyDeriver verifies that NULL deriver yields an empty string.
func TestEmptyDeriver(t *testing.T) {
	dbPath := createFixtureDB(t)

	s, err := Open(context.Background(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// path3 (glibc) has no deriver
	info, err := s.PathInfo(context.Background(), hashPart3)
	if err != nil {
		t.Fatalf("PathInfo: %v", err)
	}

	if info.Deriver != "" {
		t.Errorf("Deriver = %q, want empty", info.Deriver)
	}
}

// TestConcurrentReads verifies that 10 goroutines can call PathInfo simultaneously.
func TestConcurrentReads(t *testing.T) {
	dbPath := createFixtureDB(t)

	s, err := Open(context.Background(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	const n = 10

	var wg sync.WaitGroup

	errs := make(chan error, n)

	for range n {
		wg.Go(func() {
			_, err := s.PathInfo(context.Background(), hashPart1)
			if err != nil {
				errs <- err
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent PathInfo error: %v", err)
	}
}

// TestMissingDB verifies that Open fails for a non-existent database file.
func TestMissingDB(t *testing.T) {
	_, err := Open(context.Background(), "/nonexistent/path/to/db.sqlite", storeDir)
	if err == nil {
		t.Error("expected error for missing DB, got nil")
	}
}
