package nar

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// countingWriter discards data but counts bytes written, satisfying io.Writer.
type countingWriter struct {
	n int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	cw.n += int64(len(p))
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
		if err := os.WriteFile(name, content, 0o644); err != nil {
			b.Fatal(err)
		}
	}

	totalBytes := int64(numFiles * fileSize)
	b.SetBytes(totalBytes)
	b.ReportAllocs()
	for b.Loop() {
		cw := &countingWriter{}
		if err := Write(cw, dir); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteRegular_1MB writes a single 1 MB regular file.
func BenchmarkWriteRegular_1MB(b *testing.B) {
	const size = 1 << 20 // 1 MiB
	dir := b.TempDir()
	path := filepath.Join(dir, "big.bin")
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	f.Close()
	if err := os.Truncate(path, size); err != nil {
		b.Fatal(err)
	}

	b.SetBytes(size)
	b.ReportAllocs()
	for b.Loop() {
		if err := Write(io.Discard, path); err != nil {
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
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	f.Close()
	if err := os.Truncate(path, size); err != nil {
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
		if err := Write(cw, path); err != nil {
			b.Fatal(err)
		}
		iters++
	}

	b.StopTimer()

	runtime.ReadMemStats(&msAfter)
	allocPerIter := int64(msAfter.TotalAlloc-msBefore.TotalAlloc) / int64(iters)
	const limit = 2 * size
	if allocPerIter > limit {
		b.Errorf("BenchmarkWriteRegular_10MB: TotalAlloc/iter=%d > 2×fileSize=%d — writer may be buffering the whole file", allocPerIter, limit)
	}
}
