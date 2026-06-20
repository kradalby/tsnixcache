package niximport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kradalby/tsnixcache/nar"
	"github.com/kradalby/tsnixcache/narinfo"
	"github.com/kradalby/tsnixcache/nixbase32"
	"github.com/kradalby/tsnixcache/nixcompress"
)

// nixStoreTempDir returns a temp directory suitable for use as a chroot nix
// store URI.  It registers a cleanup that makes every file in the dir writable
// before removal, because nix-store --import writes read-only paths.
func nixStoreTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		// Walk and chmod everything so that Go's TempDir cleanup can remove it.
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			return os.Chmod(path, 0o755)
		})
	})
	return dir
}

// findRealStorePath returns any directory entry in /nix/store.
// Skips the test only if /nix/store does not exist at all.
func findRealStorePath(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir("/nix/store")
	if err != nil {
		t.Skip("no /nix/store available")
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) > 32 && name[32] == '-' {
			return "/nix/store/" + name
		}
	}
	t.Fatal("no paths in /nix/store")
	return ""
}

// buildNarFor returns the NAR bytes for the given store path.
func buildNarFor(t *testing.T, storePath string) []byte {
	t.Helper()
	var narBuf bytes.Buffer
	if err := nar.Write(&narBuf, storePath); err != nil {
		t.Fatalf("nar.Write(%s): %v", storePath, err)
	}
	return narBuf.Bytes()
}

// spoolNar writes narBytes to spoolDir/<name> and returns the file path.
func spoolNar(t *testing.T, spoolDir, name string, narBytes []byte) string {
	t.Helper()
	p := filepath.Join(spoolDir, name)
	if err := os.WriteFile(p, narBytes, 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", p, err)
	}
	return p
}

// hashOf computes sha256 and returns the nixbase32 hash string.
func hashOf(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + nixbase32.EncodeToString(h[:])
}

// TestSpoolPath verifies SpoolPath strips query params and extracts the basename.
func TestSpoolPath(t *testing.T) {
	spoolDir := t.TempDir()
	imp := &Importer{SpoolDir: spoolDir}

	got := imp.SpoolPath("nar/abc123.nar.xz")
	want := filepath.Join(spoolDir, "abc123.nar.xz")
	if got != want {
		t.Errorf("SpoolPath(nar/abc123.nar.xz) = %q, want %q", got, want)
	}

	got = imp.SpoolPath("nar/abc123.nar?hash=xyz")
	want = filepath.Join(spoolDir, "abc123.nar")
	if got != want {
		t.Errorf("SpoolPath(nar/abc123.nar?hash=xyz) = %q, want %q", got, want)
	}
}

// TestExportFramingGolden verifies the export stream starts with u64(1) and
// contains the NAR magic immediately after.
func TestExportFramingGolden(t *testing.T) {
	// Build a minimal NAR for a temp regular file.
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	narBytes := buildNarFor(t, path)

	storePath := "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0-test"
	var out bytes.Buffer
	if err := nar.WriteExport(&out, narBytes, storePath, nil, ""); err != nil {
		t.Fatalf("WriteExport: %v", err)
	}

	b := out.Bytes()
	if len(b) < 8 {
		t.Fatal("export stream too short")
	}
	// First 8 bytes must be u64(1) little-endian.
	magic := binary.LittleEndian.Uint64(b[:8])
	if magic != 1 {
		t.Errorf("first uint64 = %d, want 1", magic)
	}
	// Bytes 8+ must start with the NAR magic string length prefix + "nix-archive-1".
	// u64(13) + "nix-archive-1" padded to 8 bytes = 8+13+3 = 24 bytes.
	if !bytes.Contains(b[8:], []byte("nix-archive-1")) {
		t.Error("NAR magic 'nix-archive-1' not found after leading uint64")
	}
}

// TestNarHashMismatch verifies Import rejects a spool file whose hash doesn't
// match the narinfo.
func TestNarHashMismatch(t *testing.T) {
	spoolDir := t.TempDir()
	imp := &Importer{SpoolDir: spoolDir}

	content := []byte("not a real nar")
	spoolNar(t, spoolDir, "test.nar", content)

	ni := &narinfo.NarInfo{
		StorePath:   "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0-test",
		URL:         "nar/test.nar",
		Compression: "none",
		NarHash:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // wrong
		NarSize:     uint64(len(content)),
	}

	err := imp.Import(context.Background(), ni)
	if err == nil {
		t.Fatal("expected error for NarHash mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "NarHash mismatch") {
		t.Errorf("error %q does not mention NarHash mismatch", err)
	}
}

// TestNarSizeMismatch verifies Import rejects when the NAR byte count doesn't
// match NarSize in the narinfo.
func TestNarSizeMismatch(t *testing.T) {
	spoolDir := t.TempDir()
	imp := &Importer{SpoolDir: spoolDir}

	content := []byte("some content")
	spoolNar(t, spoolDir, "test.nar", content)

	ni := &narinfo.NarInfo{
		StorePath:   "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0-test",
		URL:         "nar/test.nar",
		Compression: "none",
		NarHash:     hashOf(content), // correct hash
		NarSize:     9999,            // wrong size
	}

	err := imp.Import(context.Background(), ni)
	if err == nil {
		t.Fatal("expected error for NarSize mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "NarSize mismatch") {
		t.Errorf("error %q does not mention NarSize mismatch", err)
	}
}

// TestMissingSpoolFile verifies Import returns an error when the spool file
// does not exist.
func TestMissingSpoolFile(t *testing.T) {
	spoolDir := t.TempDir()
	imp := &Importer{SpoolDir: spoolDir}

	ni := &narinfo.NarInfo{
		StorePath:   "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0-test",
		URL:         "nar/nonexistent.nar",
		Compression: "none",
		NarHash:     "sha256:0",
		NarSize:     0,
	}

	err := imp.Import(context.Background(), ni)
	if err == nil {
		t.Fatal("expected error for missing spool file, got nil")
	}
}

// TestImportRoundTrip imports a real store path into a chroot store and then
// verifies nix-store can query the imported path.
func TestImportRoundTrip(t *testing.T) {
	realPath := findRealStorePath(t)

	narBytes := buildNarFor(t, realPath)
	narHash := hashOf(narBytes)
	narSize := uint64(len(narBytes))

	spoolDir := t.TempDir()
	spoolNar(t, spoolDir, "test.nar", narBytes)

	ni := &narinfo.NarInfo{
		StorePath:   realPath,
		URL:         "nar/test.nar",
		Compression: "none",
		FileHash:    narHash,
		FileSize:    narSize,
		NarHash:     narHash,
		NarSize:     narSize,
		References:  nil,
	}

	storeURI := nixStoreTempDir(t)
	gcRootDir := t.TempDir()
	imp := &Importer{
		SpoolDir:    spoolDir,
		GCRootDir:   gcRootDir,
		NixStoreURI: storeURI,
	}

	if err := imp.Import(context.Background(), ni); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Verify the path was imported by querying the chroot store.
	cmd := exec.Command("nix-store", "--store", storeURI, "--query", "--outputs", realPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("nix-store query failed: %v\n%s", err, out)
	}
}

// TestDuplicateImport verifies that importing the same path twice does not error.
func TestDuplicateImport(t *testing.T) {
	realPath := findRealStorePath(t)
	narBytes := buildNarFor(t, realPath)
	narHash := hashOf(narBytes)
	narSize := uint64(len(narBytes))

	storeURI := nixStoreTempDir(t)
	gcRootDir := t.TempDir()

	makeNI := func() *narinfo.NarInfo {
		return &narinfo.NarInfo{
			StorePath:   realPath,
			URL:         "nar/test.nar",
			Compression: "none",
			FileHash:    narHash,
			FileSize:    narSize,
			NarHash:     narHash,
			NarSize:     narSize,
			References:  nil,
		}
	}

	// First import.
	spoolDir1 := t.TempDir()
	spoolNar(t, spoolDir1, "test.nar", narBytes)
	imp1 := &Importer{SpoolDir: spoolDir1, GCRootDir: gcRootDir, NixStoreURI: storeURI}
	if err := imp1.Import(context.Background(), makeNI()); err != nil {
		t.Fatalf("first Import: %v", err)
	}

	// Second import — should be idempotent.
	spoolDir2 := t.TempDir()
	spoolNar(t, spoolDir2, "test.nar", narBytes)
	imp2 := &Importer{SpoolDir: spoolDir2, GCRootDir: gcRootDir, NixStoreURI: storeURI}
	if err := imp2.Import(context.Background(), makeNI()); err != nil {
		t.Fatalf("second Import (duplicate): %v", err)
	}
}

// TestGCRootCreated verifies a gcroot symlink is created after a successful import.
func TestGCRootCreated(t *testing.T) {
	realPath := findRealStorePath(t)
	narBytes := buildNarFor(t, realPath)
	narHash := hashOf(narBytes)
	narSize := uint64(len(narBytes))

	spoolDir := t.TempDir()
	spoolNar(t, spoolDir, "test.nar", narBytes)

	gcRootDir := t.TempDir()
	imp := &Importer{
		SpoolDir:    spoolDir,
		GCRootDir:   gcRootDir,
		NixStoreURI: nixStoreTempDir(t),
	}

	ni := &narinfo.NarInfo{
		StorePath:   realPath,
		URL:         "nar/test.nar",
		Compression: "none",
		FileHash:    narHash,
		FileSize:    narSize,
		NarHash:     narHash,
		NarSize:     narSize,
		References:  nil,
	}

	if err := imp.Import(context.Background(), ni); err != nil {
		t.Fatalf("Import: %v", err)
	}

	hashPart := filepath.Base(realPath)
	if len(hashPart) > 32 {
		hashPart = hashPart[:32]
	}
	gcroot := filepath.Join(gcRootDir, hashPart)
	fi, err := os.Lstat(gcroot)
	if err != nil {
		t.Fatalf("gcroot symlink not found at %s: %v", gcroot, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("gcroot %s is not a symlink", gcroot)
	}
	target, err := os.Readlink(gcroot)
	if err != nil {
		t.Fatal(err)
	}
	if target != realPath {
		t.Errorf("gcroot target = %q, want %q", target, realPath)
	}
}

// TestSpoolFileRemovedAfterImport verifies the spool file is deleted on success.
func TestSpoolFileRemovedAfterImport(t *testing.T) {
	realPath := findRealStorePath(t)
	narBytes := buildNarFor(t, realPath)
	narHash := hashOf(narBytes)
	narSize := uint64(len(narBytes))

	spoolDir := t.TempDir()
	spoolPath := spoolNar(t, spoolDir, "test.nar", narBytes)

	imp := &Importer{
		SpoolDir:    spoolDir,
		GCRootDir:   t.TempDir(),
		NixStoreURI: nixStoreTempDir(t),
	}

	ni := &narinfo.NarInfo{
		StorePath:   realPath,
		URL:         "nar/test.nar",
		Compression: "none",
		FileHash:    narHash,
		FileSize:    narSize,
		NarHash:     narHash,
		NarSize:     narSize,
		References:  nil,
	}

	if err := imp.Import(context.Background(), ni); err != nil {
		t.Fatalf("Import: %v", err)
	}

	if _, err := os.Stat(spoolPath); err == nil {
		t.Errorf("spool file %s still exists after import", spoolPath)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected error checking spool file: %v", err)
	}
}

// TestImportZstdCompression spools a zstd-compressed NAR, sets Compression:zstd
// in the narinfo, and verifies Import decompresses and imports correctly.
func TestImportZstdCompression(t *testing.T) {
	realPath := findRealStorePath(t)
	narBytes := buildNarFor(t, realPath)
	narHash := hashOf(narBytes)
	narSize := uint64(len(narBytes))

	// Compress the NAR with zstd.
	var compressed bytes.Buffer
	enc, err := nixcompress.Encoder(&compressed, "zstd", false)
	if err != nil {
		t.Fatalf("nixcompress.Encoder(zstd): %v", err)
	}
	if _, err := enc.Write(narBytes); err != nil {
		t.Fatalf("write to zstd encoder: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("close zstd encoder: %v", err)
	}
	compressedBytes := compressed.Bytes()

	spoolDir := t.TempDir()
	spoolNar(t, spoolDir, "test.nar.zst", compressedBytes)

	fileHash := hashOf(compressedBytes)
	fileSize := uint64(len(compressedBytes))

	ni := &narinfo.NarInfo{
		StorePath:   realPath,
		URL:         "nar/test.nar.zst",
		Compression: "zstd",
		FileHash:    fileHash,
		FileSize:    fileSize,
		NarHash:     narHash,
		NarSize:     narSize,
		References:  nil,
	}

	imp := &Importer{
		SpoolDir:    spoolDir,
		GCRootDir:   t.TempDir(),
		NixStoreURI: nixStoreTempDir(t),
	}

	if err := imp.Import(context.Background(), ni); err != nil {
		t.Fatalf("Import with zstd: %v", err)
	}
}

// TestConcurrentImports runs 3 goroutines each importing a different store path
// concurrently to detect data races. Each goroutine uses its own chroot store
// to avoid SQLite busy errors from concurrent nix-store --import processes.
func TestConcurrentImports(t *testing.T) {
	entries, err := os.ReadDir("/nix/store")
	if err != nil {
		t.Skip("no /nix/store available")
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) > 32 && name[32] == '-' {
			paths = append(paths, "/nix/store/"+name)
		}
		if len(paths) == 3 {
			break
		}
	}
	if len(paths) < 3 {
		t.Skip("need at least 3 paths in /nix/store")
	}

	var wg sync.WaitGroup
	errs := make([]error, len(paths))
	for i, p := range paths {
		wg.Add(1)
		go func(idx int, storePath string) {
			defer wg.Done()
			narBytes := buildNarFor(t, storePath)
			narHash := hashOf(narBytes)
			narSize := uint64(len(narBytes))

			spoolDir := t.TempDir()
			spoolNar(t, spoolDir, "test.nar", narBytes)

			ni := &narinfo.NarInfo{
				StorePath:   storePath,
				URL:         "nar/test.nar",
				Compression: "none",
				FileHash:    narHash,
				FileSize:    narSize,
				NarHash:     narHash,
				NarSize:     narSize,
				References:  nil,
			}
			// Each goroutine gets its own chroot store; nix-store --import
			// holds an exclusive SQLite lock, so sharing a store causes busy errors.
			imp := &Importer{
				SpoolDir:    spoolDir,
				GCRootDir:   t.TempDir(),
				NixStoreURI: nixStoreTempDir(t),
			}
			errs[idx] = imp.Import(context.Background(), ni)
		}(i, p)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("goroutine %d: %v", i, e)
		}
	}
}
