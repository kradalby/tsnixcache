// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package store_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/kradalby/tsnixcache/store"
)

// newWritableDB creates a database with a single table and returns its path,
// leaving no handle open.
func newWritableDB(t *testing.T, dbPath string) {
	t.Helper()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}

	_, err = db.ExecContext(t.Context(), `CREATE TABLE t (x integer)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	err = db.Close()
	if err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestReadOnlyDSNRefusesWrites pins mode=ro. We are a guest in the Nix daemon's
// database: a handle that can write it is a handle that can corrupt the store's
// metadata, and nothing else in this package would notice the flag going away.
func TestReadOnlyDSNRefusesWrites(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "db.sqlite")
	newWritableDB(t, dbPath)

	db, err := sql.Open("sqlite", store.ReadOnlyDSN(dbPath))
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}

	defer db.Close() // #nosec G104 -- test cleanup

	_, err = db.ExecContext(t.Context(), `INSERT INTO t (x) VALUES (1)`)
	if err == nil {
		t.Fatal("INSERT through the read-only DSN succeeded, want an error")
	}
}

// TestReadOnlyDSNSeesNewRows pins the absence of immutable=1. It would make
// every lookup faster and the cache permanently stale: nix-daemon adds a row
// for each imported path, and a connection opened immutable never sees one, so
// the cache would 404 everything built after start-up until it is restarted.
func TestReadOnlyDSNSeesNewRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "db.sqlite")
	newWritableDB(t, dbPath)

	reader, err := sql.Open("sqlite", store.ReadOnlyDSN(dbPath))
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}

	defer reader.Close() // #nosec G104 -- test cleanup

	count := func() int64 {
		t.Helper()

		var n int64

		err := reader.QueryRowContext(t.Context(), `SELECT count(*) FROM t`).Scan(&n)
		if err != nil {
			t.Fatalf("count: %v", err)
		}

		return n
	}

	// Warm the connection first: immutable=1 takes effect when the file is
	// opened, so a reader that never read before the write would not show it.
	if got := count(); got != 0 {
		t.Fatalf("count before write = %d, want 0", got)
	}

	writer, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}

	_, err = writer.ExecContext(t.Context(), `INSERT INTO t (x) VALUES (1)`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	err = writer.Close()
	if err != nil {
		t.Fatalf("close writer: %v", err)
	}

	if got := count(); got != 1 {
		t.Errorf("count after another process wrote = %d, want 1: the reader is not seeing new paths", got)
	}
}

// TestOpenEscapesPathInDSN pins the escaping of the DSN's URI path. The DSN is
// a URI, so an unescaped '?' or '#' in the database path used to end it early:
// mode=ro was dropped along with the rest of the query and the driver's own
// READWRITE|CREATE flags applied, quietly giving a read-write handle on a
// different file that it created on the spot. Reachable from --db.
func TestOpenEscapesPathInDSN(t *testing.T) {
	fixture := createFixtureDB(t)
	dir := filepath.Dir(fixture)
	dbPath := filepath.Join(dir, "weird?name#with%chars.sqlite")

	err := os.Rename(fixture, dbPath)
	if err != nil {
		t.Fatalf("rename fixture: %v", err)
	}

	before := dirEntries(t, dir)

	s, err := store.Open(t.Context(), dbPath, storeDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer s.Close() // #nosec G104 -- test cleanup

	info, err := s.PathInfo(t.Context(), hashPart1)
	if err != nil {
		t.Fatalf("PathInfo: %v", err)
	}

	if want := pathFor(hashPart1, "hello-2.12.1"); info.StorePath != want {
		t.Errorf("StorePath = %q, want %q: opened the wrong file", info.StorePath, want)
	}

	after := dirEntries(t, dir)
	if len(after) != len(before) {
		t.Errorf("directory went from %v to %v: opening created a file", before, after)
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}
