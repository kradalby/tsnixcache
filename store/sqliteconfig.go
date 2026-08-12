// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package store

import "strings"

// escapeDSNPath escapes the three characters SQLite's URI parser gives meaning
// to. The DSN is a URI, so an unescaped '?' or '#' in the path ends the path
// early: the remainder is taken as query or fragment, mode=ro is lost, and the
// driver's own READWRITE|CREATE flags apply — a read-write handle on a
// different file, created if absent. '%' begins a percent-escape, so it has to
// be escaped to survive as itself.
var escapeDSNPath = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

// ReadOnlyDSN returns a modernc.org/sqlite connection string for opening
// a Nix database in read-only mode. It is exported so that everything reading
// another process's Nix DB shares one definition: the pragma syntax is
// driver-specific, and mattn/go-sqlite3's "_journal_mode=WAL" spelling is
// silently ignored by modernc, which loses the busy_timeout with it.
// immutable=1 is intentionally omitted: the Nix daemon writes new paths to
// the DB after imports, and we must see those updates on every query.
func ReadOnlyDSN(path string) string {
	return "file:" + escapeDSNPath.Replace(path) +
		"?mode=ro" +
		"&_pragma=busy_timeout=5000"
}
