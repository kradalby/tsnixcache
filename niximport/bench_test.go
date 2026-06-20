package niximport

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kradalby/tsnixcache/nar"
	"github.com/kradalby/tsnixcache/narinfo"
)

// buildNarForB is the benchmark analogue of buildNarFor (which accepts *testing.T).
func buildNarForB(b *testing.B, storePath string) []byte {
	b.Helper()
	var narBuf bytes.Buffer
	if err := nar.Write(&narBuf, storePath); err != nil {
		b.Fatalf("nar.Write(%s): %v", storePath, err)
	}
	return narBuf.Bytes()
}

// spoolNarB writes narBytes to spoolDir/<name>.
func spoolNarB(b *testing.B, spoolDir, name string, narBytes []byte) {
	b.Helper()
	p := filepath.Join(spoolDir, name)
	if err := os.WriteFile(p, narBytes, 0o644); err != nil {
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
				return nil
			}
			return os.Chmod(path, 0o755)
		})
	})
	return dir
}

// makeNarInfo constructs a minimal NarInfo for the given store path and hashes.
func makeNarInfo(storePath, narHash string, narSize uint64) *narinfo.NarInfo {
	return &narinfo.NarInfo{
		StorePath:   storePath,
		URL:         "nar/bench.nar",
		Compression: "none",
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
// This benchmark requires /nix/store and the nix-store binary to be present.
// It is skipped in short mode because it forks an external process each iteration.
func BenchmarkImport(b *testing.B) {
	if testing.Short() {
		b.Skip("BenchmarkImport skipped in short mode (requires nix-store)")
	}

	// Locate a real store path to import.
	entries, err := os.ReadDir("/nix/store")
	if err != nil {
		b.Skip("no /nix/store available")
	}
	var realPath string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) > 32 && name[32] == '-' {
			realPath = "/nix/store/" + name
			break
		}
	}
	if realPath == "" {
		b.Skip("no paths found in /nix/store")
	}

	// Build the NAR bytes once during setup.
	narBytes := buildNarForB(b, realPath)
	narHash := hashOf(narBytes)
	narSize := uint64(len(narBytes))

	b.SetBytes(int64(narSize))
	b.ReportAllocs()
	for b.Loop() {
		// Stop the timer during per-iteration setup so we only measure Import.
		b.StopTimer()
		spoolDir := b.TempDir()
		spoolNarB(b, spoolDir, "bench.nar", narBytes)
		gcRootDir := b.TempDir()
		storeURI := nixStoreTempDirB(b)
		ni := makeNarInfo(realPath, narHash, narSize)
		imp := &Importer{
			SpoolDir:    spoolDir,
			GCRootDir:   gcRootDir,
			NixStoreURI: storeURI,
		}
		b.StartTimer()

		if err := imp.Import(context.Background(), ni); err != nil {
			b.Fatal(err)
		}
	}
}
