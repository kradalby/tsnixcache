package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/kradalby/tsnixcache/nixbase32"

	"github.com/kradalby/tsnixcache/store"
)

// Fixture hex hashes (32 bytes = 64 hex chars each) and their pre-computed
// nix-base32 representations (computed from nixbase32.EncodeToString).
//
// Nix store path hash parts (32 chars) are nix-base32 of 20-byte values;
// we use plain 32-char strings that happen to be valid nix-base32 chars for the
// path component (since PathInfo matches by LIKE pattern, not by re-encoding).
const (
	// narHash hex values (32-byte SHA256)
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

	storeDir = "/nix/store"
)

func pathFor(hp, name string) string {
	return storeDir + "/" + hp + "-" + name
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
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`CREATE TABLE ValidPaths (
		id               INTEGER PRIMARY KEY,
		path             TEXT UNIQUE NOT NULL,
		hash             TEXT NOT NULL,
		registrationTime INTEGER,
		deriver          TEXT,
		narSize          INTEGER,
		ultimate         INTEGER,
		sigs             TEXT,
		ca               TEXT
	)`)
	if err != nil {
		t.Fatalf("create ValidPaths: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE Refs (
		referrer  INTEGER REFERENCES ValidPaths(id),
		reference INTEGER REFERENCES ValidPaths(id)
	)`)
	if err != nil {
		t.Fatalf("create Refs: %v", err)
	}

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

	// Add refs: path2 references path3
	addRef(t, db, pathFor(hashPart2, "openssl-3.0.0"), pathFor(hashPart3, "glibc-2.38"))

	// path4 references itself
	addRef(t, db, pathFor(hashPart4, "self-ref-1.0"), pathFor(hashPart4, "self-ref-1.0"))

	return dbPath
}

func mustInsert(t *testing.T, db *sql.DB, path, hash string, narSize int64, deriver, sigs, ca sql.NullString) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO ValidPaths (path, hash, narSize, deriver, sigs, ca) VALUES (?, ?, ?, ?, ?, ?)`,
		path, hash, narSize, deriver, sigs, ca,
	)
	if err != nil {
		t.Fatalf("insert %s: %v", path, err)
	}
}

func addRef(t *testing.T, db *sql.DB, referrerPath, referencePath string) {
	t.Helper()
	_, err := db.Exec(`
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
	s, err := store.Open(dbPath, storeDir)
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
	s, err := store.Open(dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	_, err = s.PathInfo(context.Background(), "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz9")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestNarHashConversion verifies base16 → nix-base32 conversion.
func TestNarHashConversion(t *testing.T) {
	dbPath := createFixtureDB(t)
	s, err := store.Open(dbPath, storeDir)
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
	_, _ = db.Exec(`CREATE TABLE ValidPaths (id INTEGER PRIMARY KEY, path TEXT UNIQUE NOT NULL, hash TEXT NOT NULL, registrationTime INTEGER, deriver TEXT, narSize INTEGER, ultimate INTEGER, sigs TEXT, ca TEXT)`)
	_, _ = db.Exec(`CREATE TABLE Refs (referrer INTEGER, reference INTEGER)`)

	const prefixedHash = "sha256:" + hexHash2
	_, _ = db.Exec(`INSERT INTO ValidPaths (path, hash, narSize) VALUES (?, ?, ?)`,
		pathFor(hashPart2, "openssl-3.0.0"), prefixedHash, 99999)
	db.Close()

	s, err := store.Open(dbPath, storeDir)
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
	_, _ = db.Exec(`CREATE TABLE ValidPaths (id INTEGER PRIMARY KEY, path TEXT UNIQUE NOT NULL, hash TEXT NOT NULL, registrationTime INTEGER, deriver TEXT, narSize INTEGER, ultimate INTEGER, sigs TEXT, ca TEXT)`)
	_, _ = db.Exec(`CREATE TABLE Refs (referrer INTEGER, reference INTEGER)`)

	// Insert main package and 3 deps inserted in reverse-alphabetical order.
	paths := []string{
		pathFor(hashPart1, "pkg-1.0"),
		pathFor(hashPart4, "zzz-dep-3.0"),
		pathFor(hashPart3, "mmm-dep-2.0"),
		pathFor(hashPart2, "aaa-dep-1.0"),
	}
	hashes := []string{hexHash1, hexHash4, hexHash3, hexHash2}
	for i, p := range paths {
		_, _ = db.Exec(`INSERT INTO ValidPaths (path, hash, narSize) VALUES (?, ?, ?)`, p, hashes[i], 1000)
	}
	// Add refs: pkg -> zzz, mmm, aaa
	for _, dep := range paths[1:] {
		_, _ = db.Exec(`INSERT INTO Refs (referrer, reference)
			SELECT r.id, e.id FROM ValidPaths r, ValidPaths e WHERE r.path=? AND e.path=?`,
			paths[0], dep)
	}
	db.Close()

	s, err := store.Open(dbPath, storeDir)
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

// TestReferencesSelfExcluded verifies that self-references are excluded.
func TestReferencesSelfExcluded(t *testing.T) {
	dbPath := createFixtureDB(t)
	s, err := store.Open(dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	info, err := s.PathInfo(context.Background(), hashPart4)
	if err != nil {
		t.Fatalf("PathInfo: %v", err)
	}

	if len(info.References) != 0 {
		t.Errorf("expected no refs (self excluded), got %v", info.References)
	}
}

// TestSigsSplit verifies that space-separated sigs are parsed into a slice.
func TestSigsSplit(t *testing.T) {
	dbPath := createFixtureDB(t)
	s, err := store.Open(dbPath, storeDir)
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
	s, err := store.Open(dbPath, storeDir)
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
	s, err := store.Open(dbPath, storeDir)
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
	s, err := store.Open(dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.PathInfo(context.Background(), hashPart1)
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent PathInfo error: %v", err)
	}
}

// TestMissingDB verifies that Open fails for a non-existent database file.
func TestMissingDB(t *testing.T) {
	_, err := store.Open("/nonexistent/path/to/db.sqlite", storeDir)
	if err == nil {
		t.Error("expected error for missing DB, got nil")
	}
}
