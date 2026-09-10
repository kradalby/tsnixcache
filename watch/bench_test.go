// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package watch

import (
	"database/sql"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkLivePaths(b *testing.B) {
	for _, n := range []int{128, 512, 2000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			ctx := b.Context()
			source, err := sql.Open("sqlite", ":memory:")
			require.NoError(b, err)

			defer source.Close()

			_, err = source.ExecContext(ctx, `CREATE TABLE ValidPaths (id INTEGER PRIMARY KEY, path TEXT NOT NULL UNIQUE, narSize INTEGER)`)
			require.NoError(b, err)

			names := make([]string, n)
			tx, err := source.BeginTx(ctx, nil)
			require.NoError(b, err)

			for i := range names {
				names[i] = fmt.Sprintf("/nix/store/%032d-benchmark-path", i)
				_, err = tx.ExecContext(ctx, `INSERT INTO ValidPaths(path) VALUES (?)`, names[i])
				require.NoError(b, err)
			}

			require.NoError(b, tx.Commit())

			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				got := livePaths(ctx, source, names)
				if len(got) != len(names) {
					b.Fatal(len(got))
				}
			}
		})
	}
}
