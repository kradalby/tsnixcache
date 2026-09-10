// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package upload

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func BenchmarkDependencyScheduling(b *testing.B) {
	for _, jobs := range []int{2, 8} {
		b.Run(fmt.Sprintf("%djobs", jobs), func(b *testing.B) {
			chains := 1
			if jobs > 2 {
				chains = 8
			}

			metas := make([]pathMeta, 1, 1+6*chains)
			metas[0] = pathMeta{Path: "slow"}
			delays := map[string]time.Duration{"/slow.narinfo": 250 * time.Millisecond}

			for chain := range chains {
				for step := range 6 {
					path := fmt.Sprintf("chain%dstep%d", chain, step)

					meta := pathMeta{Path: path}
					if step > 0 {
						meta.References = []string{fmt.Sprintf("chain%dstep%d", chain, step-1)}
					}

					metas = append(metas, meta)

					delay := 25 * time.Millisecond
					if chains > 1 {
						delay = time.Duration(10+chain*3) * time.Millisecond
					}

					delays["/"+path+".narinfo"] = delay
				}
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				timer := time.NewTimer(delays[r.URL.Path])
				defer timer.Stop()

				select {
				case <-timer.C:
					w.WriteHeader(http.StatusOK)
				case <-r.Context().Done():
				}
			}))
			defer server.Close()

			var (
				chainEnd atomic.Int64
				start    time.Time
			)

			u := newUploader(server.URL, Options{Jobs: jobs, Attempts: 1, OnPath: func(s Stats) {
				if strings.HasSuffix(s.Path, "step5") {
					elapsed := time.Since(start).Nanoseconds()
					for old := chainEnd.Load(); elapsed > old && !chainEnd.CompareAndSwap(old, elapsed); old = chainEnd.Load() {
					}
				}
			}})
			u.client = server.Client()

			var chainTotal int64

			b.ResetTimer()

			for range b.N {
				chainEnd.Store(0)

				start = time.Now()
				stats := u.run(b.Context(), metas)
				require.Len(b, stats, len(metas))

				for _, s := range stats {
					require.NoError(b, s.Err)
				}

				chainTotal += chainEnd.Load()
			}

			b.ReportMetric(float64(chainTotal)/float64(b.N), "chain-ns/op")
		})
	}
}
