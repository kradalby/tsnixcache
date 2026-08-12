// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package nar

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// countingWriter discards data, counting the bytes written and the largest
// single write it was handed.
type countingWriter struct {
	n   int64
	max int
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	cw.n += int64(len(p))

	if len(p) > cw.max {
		cw.max = len(p)
	}

	return len(p), nil
}

// BenchmarkWriteDir writes a directory with 10 files × 1 KB.
func BenchmarkWriteDir(b *testing.B) {
	dir := b.TempDir()

	const (
		numFiles = 10
		fileSize = 1024
	)

	content := make([]byte, fileSize)
	for i := range content {
		content[i] = byte(i)
	}

	for i := range numFiles {
		name := filepath.Join(dir, "file"+string(rune('a'+i)))

		err := os.WriteFile(name, content, 0o600) // #nosec G306 -- benchmark test file
		if err != nil {
			b.Fatal(err)
		}
	}

	totalBytes := int64(numFiles * fileSize)
	b.SetBytes(totalBytes)
	b.ReportAllocs()

	for b.Loop() {
		cw := &countingWriter{}

		err := Write(cw, dir)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteRegular_1MB writes a single 1 MB regular file.
func BenchmarkWriteRegular_1MB(b *testing.B) {
	const size = 1 << 20 // 1 MiB

	dir := b.TempDir()

	path := filepath.Join(dir, "big.bin")

	f, err := os.Create(path) // #nosec G304 -- benchmark test path
	if err != nil {
		b.Fatal(err)
	}

	err = f.Close()
	if err != nil {
		b.Fatal(err)
	}

	err = os.Truncate(path, size)
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(size)
	b.ReportAllocs()

	for b.Loop() {
		err = Write(io.Discard, path)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteRegular_10MB writes a single 10 MB regular file, writing to a
// countingWriter that discards output.  After each iteration we verify that the
// NAR writer never buffered the full file in memory: peak TotalAlloc per
// iteration must not exceed 2× the file size.
func BenchmarkWriteRegular_10MB(b *testing.B) {
	const size = 10 << 20 // 10 MiB

	dir := b.TempDir()

	path := filepath.Join(dir, "huge.bin")

	f, err := os.Create(path) // #nosec G304 -- benchmark test path
	if err != nil {
		b.Fatal(err)
	}

	err = f.Close()
	if err != nil {
		b.Fatal(err)
	}

	err = os.Truncate(path, size)
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(size)
	b.ReportAllocs()

	// Measure allocations around iterations to detect if the writer
	// ever holds the full file in memory.
	var msBefore, msAfter runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&msBefore)

	iters := 0

	for b.Loop() {
		cw := &countingWriter{}

		err = Write(cw, path)
		if err != nil {
			b.Fatal(err)
		}

		iters++
	}

	b.StopTimer()

	runtime.ReadMemStats(&msAfter)

	allocPerIter := int64(msAfter.TotalAlloc-msBefore.TotalAlloc) / int64(iters) // #nosec G115 -- TotalAlloc always increases monotonically

	const limit = 2 * size
	if allocPerIter > limit {
		b.Errorf("BenchmarkWriteRegular_10MB: TotalAlloc/iter=%d > 2×fileSize=%d — writer may be buffering the whole file", allocPerIter, limit)
	}
}
