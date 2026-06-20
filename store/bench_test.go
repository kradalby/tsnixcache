package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/kradalby/tsnixcache/store"
)

// createBenchDB creates a fixture SQLite database with n rows for benchmarking.
// Each row has a unique hash part and two references.
func createBenchDB(b *testing.B, n int) (dbPath string, hashParts []string) {
	b.Helper()
	dir := b.TempDir()
	dbPath = filepath.Join(dir, "bench.sqlite")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		b.Fatalf("open bench db: %v", err)
	}
	b.Cleanup(func() { db.Close() })

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
		b.Fatalf("create ValidPaths: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE Refs (
		referrer  INTEGER REFERENCES ValidPaths(id),
		reference INTEGER REFERENCES ValidPaths(id)
	)`)
	if err != nil {
		b.Fatalf("create Refs: %v", err)
	}

	// Insert n paths.  We also insert two shared dependency paths that all
	// main rows reference, to exercise the Refs join.
	dep1HashPart := "dep0000000000000000000000000000a"
	dep2HashPart := "dep0000000000000000000000000000b"
	dep1Path := storeDir + "/" + dep1HashPart + "-dep-lib"
	dep2Path := storeDir + "/" + dep2HashPart + "-dep-runtime"

	mustExec := func(query string, args ...any) {
		b.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			b.Fatalf("exec %q: %v", query, err)
		}
	}

	mustExec(`INSERT INTO ValidPaths (path, hash, narSize, sigs) VALUES (?, ?, ?, ?)`,
		dep1Path, hexHash1, 10000, "key1:SIG1==")
	mustExec(`INSERT INTO ValidPaths (path, hash, narSize, sigs) VALUES (?, ?, ?, ?)`,
		dep2Path, hexHash2, 20000, "key1:SIG2==")

	hashParts = make([]string, n)
	for i := range n {
		hp := fmt.Sprintf("bench%027d", i)
		hashParts[i] = hp
		path := storeDir + "/" + hp + fmt.Sprintf("-pkg-%d", i)
		hash := fmt.Sprintf("%064x", i) // 64 hex chars (32 bytes)
		sigs := fmt.Sprintf("key1:BENCH%d==", i)
		mustExec(`INSERT INTO ValidPaths (path, hash, narSize, sigs) VALUES (?, ?, ?, ?)`,
			path, hash, int64(1000+i), sigs)
		// Each bench row references dep1 and dep2.
		mustExec(`INSERT INTO Refs (referrer, reference)
			SELECT r.id, e.id FROM ValidPaths r, ValidPaths e WHERE r.path=? AND e.path=?`,
			path, dep1Path)
		mustExec(`INSERT INTO Refs (referrer, reference)
			SELECT r.id, e.id FROM ValidPaths r, ValidPaths e WHERE r.path=? AND e.path=?`,
			path, dep2Path)
	}

	return dbPath, hashParts
}

// pathInfoBytes estimates the bytes "processed" for a PathInfo result:
// the sum of all string field lengths plus 8 bytes for NarSize.
func pathInfoBytes(pi *store.PathInfo) int64 {
	n := int64(len(pi.StorePath) + len(pi.NarHash) + len(pi.Deriver) + len(pi.CA) + 8)
	for _, r := range pi.References {
		n += int64(len(r))
	}
	for _, s := range pi.Sigs {
		n += int64(len(s))
	}
	return n
}

func BenchmarkPathInfo(b *testing.B) {
	dbPath, hashParts := createBenchDB(b, 100)
	s, err := store.Open(dbPath, storeDir)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { s.Close() })

	ctx := context.Background()
	n := len(hashParts)

	// Probe response size for b.SetBytes.
	pi, err := s.PathInfo(ctx, hashParts[0])
	if err != nil {
		b.Fatalf("probe PathInfo: %v", err)
	}
	b.SetBytes(pathInfoBytes(pi))
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		_, err := s.PathInfo(ctx, hashParts[i%n])
		if err != nil {
			b.Fatal(err)
		}
		i++
	}
}

func BenchmarkPathInfo_Parallel(b *testing.B) {
	dbPath, hashParts := createBenchDB(b, 100)
	s, err := store.Open(dbPath, storeDir)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { s.Close() })

	ctx := context.Background()
	n := len(hashParts)

	// Probe response size for b.SetBytes.
	pi, err := s.PathInfo(ctx, hashParts[0])
	if err != nil {
		b.Fatalf("probe PathInfo: %v", err)
	}
	b.SetBytes(pathInfoBytes(pi))
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_, err := s.PathInfo(ctx, hashParts[i%n])
			if err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}
