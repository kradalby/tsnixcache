// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package store

import "database/sql"

// QueryMain is the real lookup statement, exported so the query-plan test can
// EXPLAIN it instead of a copy that would keep passing after a bad rewrite.
const QueryMain = queryMain

// Stats exposes the connection pool state so the pool-sizing test can observe
// how many connections survive a burst of concurrent lookups.
func (s *Store) Stats() sql.DBStats { return s.db.Stats() }

// DB exposes the pool so the pool-sizing test can check out connections
// itself. Observed occupancy after a burst of lookups measures the scheduler,
// not the configured idle ceiling, and settles at 1 on a single-CPU runner.
func (s *Store) DB() *sql.DB { return s.db }
