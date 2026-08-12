// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package niximport

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kradalby/tsnixcache/nar"
	"github.com/kradalby/tsnixcache/narinfo"
)

const compressionNone = "none"

// storeFixtureB builds the benchmark's store-path content: a fixed 4 MiB, big
// enough for the reported MB/s to mean something and small enough to run
// anywhere. Borrowing a path from the host's /nix/store made the numbers depend
// on whatever happened to be installed.
func storeFixtureB(b *testing.B) string {
	b.Helper()

	dir := b.TempDir()

	err := os.WriteFile(filepath.Join(dir, "blob"), bytes.Repeat([]byte("tsnixcache"), 4<<20/10), 0o644) // #nosec G306 -- fixture mirrors a store path's permissions
	if err != nil {
		b.Fatal(err)
	}

	return dir
}

// buildNarForB is the benchmark analogue of buildNarFor (which accepts *testing.T).
func buildNarForB(b *testing.B, path string) []byte {
	b.Helper()

	var narBuf bytes.Buffer

	err := nar.Write(&narBuf, path)
	if err != nil {
		b.Fatalf("nar.Write(%s): %v", path, err)
	}

	return narBuf.Bytes()
}

// spoolNarB writes narBytes to spoolDir/<name>.
func spoolNarB(b *testing.B, spoolDir, name string, narBytes []byte) {
	b.Helper()

	p := filepath.Join(spoolDir, name)

	err := os.WriteFile(p, narBytes, 0o600) // #nosec G306 -- test helper, 0600 is fine
	if err != nil {
		b.Fatalf("WriteFile %s: %v", p, err)
	}
}

// nixStoreTempDirB creates a chroot temp store dir for benchmarks.
func nixStoreTempDirB(b *testing.B) string {
	b.Helper()

	dir := b.TempDir()
	b.Cleanup(func() {
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			return os.Chmod(path, 0o700) // #nosec G302,G122 -- test cleanup, needs traversal
		})
	})

	return dir
}

// makeNarInfo constructs a minimal NarInfo for the given store path and hashes.
func makeNarInfo(storePath, narHash string, narSize uint64) *narinfo.NarInfo {
	return &narinfo.NarInfo{
		StorePath:   storePath,
		URL:         "nar/bench.nar",
		Compression: compressionNone,
		FileHash:    narHash,
		FileSize:    narSize,
		NarHash:     narHash,
		NarSize:     narSize,
		References:  nil,
	}
}

// BenchmarkImport benchmarks the full import pipeline:
// build NAR → hash/size → build export stream → nix-store --import.
//
// This benchmark requires the nix-store binary to be present. It is skipped in
// short mode because it forks an external process each iteration.
func BenchmarkImport(b *testing.B) {
	if testing.Short() {
		b.Skip("BenchmarkImport skipped in short mode (requires nix-store)")
	}

	_, err := exec.LookPath("nix-store")
	if err != nil {
		b.Skip("nix-store not in PATH")
	}

	storePath := synthStorePath("bench")

	// Build the NAR bytes once during setup.
	narBytes := buildNarForB(b, storeFixtureB(b))
	narHash := hashOf(narBytes)
	narSize := uint64(len(narBytes))

	b.SetBytes(int64(narSize)) // #nosec G115 -- narSize is len() of a slice, always fits in int64
	b.ReportAllocs()

	for b.Loop() {
		// Stop the timer during per-iteration setup so we only measure Import.
		b.StopTimer()

		spoolDir := b.TempDir()
		spoolNarB(b, spoolDir, "bench.nar", narBytes)

		gcRootDir := b.TempDir()
		storeURI := nixStoreTempDirB(b)
		ni := makeNarInfo(storePath, narHash, narSize)
		imp := &Importer{
			SpoolDir:    spoolDir,
			GCRootDir:   gcRootDir,
			NixStoreURI: storeURI,
		}

		b.StartTimer()

		err = imp.Import(context.Background(), ni)
		if err != nil {
			b.Fatal(err)
		}
	}
}
