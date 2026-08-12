// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/kradalby/tsnixcache/store"
)

// benchRows is the size of the benchmark store: a machine's real Nix store is
// tens of thousands of paths, and a lookup that degrades to a table scan only
// shows up at that scale.
const benchRows = 20000

// benchDir holds the fixture database, if one was built. TestMain removes it:
// the fixture outlives any single benchmark invocation, so it cannot use
// b.TempDir.
var benchDir string

// benchFixture builds the fixture once per process. The testing package
// re-invokes a benchmark function for every b.N ramp step, so a fixture built
// inside the benchmark is rebuilt each time — at 20000 rows that cost more than
// the benchmark it feeds.
var benchFixture = sync.OnceValues(createBenchDB)

func TestMain(m *testing.M) {
	code := m.Run()

	_ = os.RemoveAll(benchDir) // #nosec G104 -- best-effort cleanup on the way out

	os.Exit(code)
}

// createBenchDB creates a fixture SQLite database with benchRows rows for
// benchmarking. Each row has a unique hash part and two references. It panics
// rather than reporting through *testing.B: it runs once, under sync.OnceValues,
// and so has no particular b to fail.
func createBenchDB() (string, []string) {
	dir, err := os.MkdirTemp("", "tsnixcache-bench")
	if err != nil {
		panic(fmt.Sprintf("bench tempdir: %v", err))
	}

	benchDir = dir
	dbPath := filepath.Join(dir, "bench.sqlite")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		panic(fmt.Sprintf("open bench db: %v", err))
	}

	defer db.Close() // #nosec G104 -- the fixture is read-only from here on

	// One connection, so the explicit transaction below covers every insert:
	// a row-per-transaction fill of a realistically sized store takes minutes.
	db.SetMaxOpenConns(1)

	mustExec := func(query string, args ...any) {
		_, err := db.ExecContext(context.Background(), query, args...)
		if err != nil {
			panic(fmt.Sprintf("exec %q: %v", query, err))
		}
	}

	for _, stmt := range nixSchema {
		mustExec(stmt)
	}

	// Insert benchRows paths.  We also insert two shared dependency paths that
	// all main rows reference, to exercise the Refs join.
	dep1HashPart := "d0p0000000000000000000000000000a"
	dep2HashPart := "d0p0000000000000000000000000000b"
	dep1Path := storeDir + "/" + dep1HashPart + "-dep-lib"
	dep2Path := storeDir + "/" + dep2HashPart + "-dep-runtime"

	mustExec(`BEGIN`)

	mustExec(`INSERT INTO ValidPaths (path, hash, narSize, sigs) VALUES (?, ?, ?, ?)`,
		dep1Path, hexHash1, 10000, "key1:SIG1==")
	mustExec(`INSERT INTO ValidPaths (path, hash, narSize, sigs) VALUES (?, ?, ?, ?)`,
		dep2Path, hexHash2, 20000, "key1:SIG2==")

	hashParts := make([]string, benchRows)

	for i := range benchRows {
		// All-digit hash parts: 32 characters of the nix-base32 alphabet, as
		// PathInfo requires.
		hp := fmt.Sprintf("%032d", i)
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

	mustExec(`COMMIT`)

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
	dbPath, hashParts := benchFixture()

	s, err := store.Open(context.Background(), dbPath, storeDir)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}

	b.Cleanup(func() { _ = s.Close() }) // #nosec G104 -- bench cleanup, error irrelevant

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
	dbPath, hashParts := benchFixture()

	s, err := store.Open(context.Background(), dbPath, storeDir)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}

	b.Cleanup(func() { _ = s.Close() }) // #nosec G104 -- bench cleanup, error irrelevant

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
