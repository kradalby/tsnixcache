// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package niximport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kradalby/tsnixcache/nar"
	"github.com/kradalby/tsnixcache/narinfo"
	"github.com/kradalby/tsnixcache/nixbase32"
	"github.com/kradalby/tsnixcache/nixcompress"
)

const (
	testStorePath = "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0-test"
	testNarURL    = "nar/test.nar"

	// zstdCacheName mirrors cache.ZstdCacheSubdir, the one directory that always
	// sits inside the spool dir. Spelled out so this package keeps no dependency
	// on the server it is called from.
	zstdCacheName = "zstd-cache"

	compressionZstd = "zstd"
)

// requireNix skips the test when the nix-store binary is unavailable. It says so
// on stderr as well as in the skip reason: a skipped test is invisible without
// -v, and these are the only tests that check this package against the real
// importer rather than against a shell script that ignores its stdin.
func requireNix(t *testing.T) {
	t.Helper()

	_, err := exec.LookPath("nix-store")
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"SKIP %s: nix-store not in PATH — the real import is not being exercised\n", t.Name())
		t.Skip("nix-store not in PATH: the real import is not being exercised")
	}
}

// nixStoreTempDir returns a temp directory suitable for use as a chroot nix
// store URI.  It registers a cleanup that makes every file in the dir writable
// before removal, because nix-store --import writes read-only paths.
func nixStoreTempDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	t.Cleanup(func() {
		// nix-store creates 0444 files and 0555 dirs; chmod -R u+w before removal.
		_ = exec.CommandContext(context.Background(), "chmod", "-R", "u+w", dir).Run() // #nosec G204 -- test cleanup with fixed args
	})

	return dir
}

// storeFixture writes a small self-contained tree standing in for the content of
// a store path: a directory, a regular file, an executable and a symlink. The
// tests build their own content rather than borrowing an arbitrary path from the
// host's /nix/store, which can be gigabytes and needs a disk-backed TMPDIR.
func storeFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	err := os.Mkdir(filepath.Join(dir, "bin"), 0o755) // #nosec G301 -- fixture mirrors a store path's permissions
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "bin", "hello"), []byte("#!/bin/sh\necho hello\n"), 0o755) // #nosec G306 -- executable fixture
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "README"), []byte("tsnixcache test fixture\n"), 0o644) // #nosec G306 -- fixture mirrors a store path's permissions
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink("bin/hello", filepath.Join(dir, "hello"))
	if err != nil {
		t.Fatal(err)
	}

	return dir
}

// synthStorePath returns a syntactically valid store path whose hash part is
// derived from name, so two tests never pick the same one.
func synthStorePath(name string) string {
	h := sha256.Sum256([]byte(name))

	return "/nix/store/" + nixbase32.EncodeToString(h[:20]) + "-" + name
}

// buildNarFor returns the NAR bytes for the given path.
func buildNarFor(t *testing.T, path string) []byte {
	t.Helper()

	var narBuf bytes.Buffer

	err := nar.Write(&narBuf, path)
	if err != nil {
		t.Fatalf("nar.Write(%s): %v", path, err)
	}

	return narBuf.Bytes()
}

// narInfoFor builds the narinfo describing an uncompressed NAR of narBytes.
func narInfoFor(storePath, url string, narBytes []byte) *narinfo.NarInfo {
	hash := hashOf(narBytes)
	size := uint64(len(narBytes))

	return &narinfo.NarInfo{
		StorePath:   storePath,
		URL:         url,
		Compression: compressionNone,
		FileHash:    hash,
		FileSize:    size,
		NarHash:     hash,
		NarSize:     size,
	}
}

// spoolNar writes narBytes to spoolDir/<name> and returns the file path.
func spoolNar(t *testing.T, spoolDir, name string, narBytes []byte) string {
	t.Helper()

	p := filepath.Join(spoolDir, name)

	err := os.WriteFile(p, narBytes, 0o600) // #nosec G306 -- test helper, 0600 is fine
	if err != nil {
		t.Fatalf("WriteFile %s: %v", p, err)
	}

	return p
}

// hashOf computes sha256 and returns the nixbase32 hash string.
func hashOf(b []byte) string {
	h := sha256.Sum256(b)

	return "sha256:" + nixbase32.EncodeToString(h[:])
}

// fakeNixStore puts a shell script named nix-store at the front of PATH for the
// duration of the test, so tests can observe when the import runs without
// needing nix installed. body runs before the script drains its stdin.
func fakeNixStore(t *testing.T, body string) {
	t.Helper()

	dir := t.TempDir()

	script := "#!/bin/sh\n" + body + "\ncat >/dev/null\n"

	err := os.WriteFile(filepath.Join(dir, "nix-store"), []byte(script), 0o755) // #nosec G306 -- test shim must be executable
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// gcRootFor returns the gcroot path Import is expected to create for storePath.
func gcRootFor(gcRootDir, storePath string) string {
	hashPart := filepath.Base(storePath)
	if len(hashPart) > 32 {
		hashPart = hashPart[:32]
	}

	return filepath.Join(gcRootDir, hashPart)
}

// TestSpoolPath verifies SpoolPath strips query params and extracts the
// basename, and that the names filepath.Join would clean into SpoolDir itself
// or its parent are refused instead.
func TestSpoolPath(t *testing.T) {
	spoolDir := t.TempDir()
	imp := &Importer{SpoolDir: spoolDir}

	tests := []struct {
		url  string
		want string // "" means the URL must be rejected
	}{
		{url: "nar/abc123.nar.xz", want: filepath.Join(spoolDir, "abc123.nar.xz")},
		{url: "nar/abc123.nar?hash=xyz", want: filepath.Join(spoolDir, "abc123.nar")},
		{url: "nar/."},
		{url: "nar/.."},
		{url: ".."},
		{url: ""},
		{url: "?hash=xyz"},
	}

	for _, tt := range tests {
		got, err := imp.SpoolPath(tt.url)
		if tt.want == "" {
			if err == nil {
				t.Errorf("SpoolPath(%q) = %q, want an error", tt.url, got)
			}

			continue
		}

		if err != nil {
			t.Errorf("SpoolPath(%q): %v", tt.url, err)

			continue
		}

		if got != tt.want {
			t.Errorf("SpoolPath(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// TestExportFramingGolden verifies the export stream starts with u64(1) and
// contains the NAR magic immediately after.
func TestExportFramingGolden(t *testing.T) {
	// Build a minimal NAR for a temp regular file.
	dir := t.TempDir()

	path := filepath.Join(dir, "hello.txt")

	err := os.WriteFile(path, []byte("hello"), 0o600) // #nosec G306 -- test file, 0600 is fine
	if err != nil {
		t.Fatal(err)
	}

	narBytes := buildNarFor(t, path)

	storePath := testStorePath

	var out bytes.Buffer

	_, err = nar.WriteExportStream(&out, bytes.NewReader(narBytes), storePath, nil, "")
	if err != nil {
		t.Fatalf("WriteExportStream: %v", err)
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
		StorePath:   testStorePath,
		URL:         testNarURL,
		Compression: compressionNone,
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
		StorePath:   testStorePath,
		URL:         testNarURL,
		Compression: compressionNone,
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
		StorePath:   testStorePath,
		URL:         "nar/nonexistent.nar",
		Compression: compressionNone,
		NarHash:     "sha256:0",
		NarSize:     0,
	}

	err := imp.Import(context.Background(), ni)
	if err == nil {
		t.Fatal("expected error for missing spool file, got nil")
	}
}

// TestVerifyBoundsDecompression verifies the decompressed stream is read only to
// the declared NarSize (plus the byte that proves it is oversize), so a
// decompression bomb can't be expanded in full.
func TestVerifyBoundsDecompression(t *testing.T) {
	// A few hundred bytes of zstd that expand to 8 MiB.
	var compressed bytes.Buffer

	enc, err := nixcompress.Encoder(context.Background(), &compressed, compressionZstd, false)
	if err != nil {
		t.Fatal(err)
	}

	_, err = enc.Write(make([]byte, 8<<20))
	if err != nil {
		t.Fatal(err)
	}

	err = enc.Close()
	if err != nil {
		t.Fatal(err)
	}

	spoolDir := t.TempDir()
	spoolPath := spoolNar(t, spoolDir, "bomb.nar.zst", compressed.Bytes())

	imp := &Importer{SpoolDir: spoolDir}
	ni := &narinfo.NarInfo{
		StorePath:   testStorePath,
		URL:         "nar/bomb.nar.zst",
		Compression: compressionZstd,
		NarHash:     hashOf(nil),
		NarSize:     10, // the pusher claims 10 bytes; the stream is 8 MiB
	}

	err = imp.Import(context.Background(), ni)
	if err == nil {
		t.Fatal("expected error for oversize NAR, got nil")
	}

	// The reported size is the proof the copy stopped at NarSize+1 rather than
	// decompressing all 8 MiB.
	if !strings.Contains(err.Error(), "got 11 want 10") {
		t.Errorf("error %q does not report a size of 11; the copy was not bounded", err)
	}

	_, err = os.Stat(spoolPath)
	if !os.IsNotExist(err) {
		t.Errorf("spool file still present after a failed verify: %v", err)
	}
}

// TestSpoolDroppedOnFailure verifies a spool whose import fails is removed, so
// rejected pushes don't accumulate on disk. The client re-PUTs the NAR before
// retrying the narinfo, so nothing will read this file again.
func TestSpoolDroppedOnFailure(t *testing.T) {
	good := buildNarFor(t, storeFixture(t))

	tests := []struct {
		name string
		ni   func(spoolDir string) *narinfo.NarInfo
		body []byte
	}{
		{
			name: "hash mismatch",
			body: good,
			ni: func(string) *narinfo.NarInfo {
				ni := narInfoFor(synthStorePath("drop-hash"), testNarURL, good)
				ni.NarHash = hashOf([]byte("something else"))

				return ni
			},
		},
		{
			name: "size mismatch",
			body: good,
			ni: func(string) *narinfo.NarInfo {
				ni := narInfoFor(synthStorePath("drop-size"), testNarURL, good)
				ni.NarSize++

				return ni
			},
		},
		{
			name: "decoder error",
			body: []byte("not compressed at all"),
			ni: func(string) *narinfo.NarInfo {
				ni := narInfoFor(synthStorePath("drop-decode"), testNarURL, good)
				ni.Compression = "xz"

				return ni
			},
		},
		{
			// A path outside the store never reaches nix-store: it has no
			// gcroot name, and the import fails there.
			name: "import failure",
			body: good,
			ni: func(string) *narinfo.NarInfo {
				return narInfoFor("/not/a/store/path", testNarURL, good)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spoolDir := t.TempDir()
			gcRootDir := t.TempDir()
			ni := tt.ni(spoolDir)
			spoolPath := spoolNar(t, spoolDir, filepath.Base(ni.URL), tt.body)

			imp := &Importer{
				SpoolDir:    spoolDir,
				GCRootDir:   gcRootDir,
				NixStoreURI: nixStoreTempDir(t),
			}

			err := imp.Import(context.Background(), ni)
			if err == nil {
				t.Fatal("expected Import to fail, got nil")
			}

			_, err = os.Stat(spoolPath)
			if !os.IsNotExist(err) {
				t.Errorf("spool file %s survived a failed import: %v", spoolPath, err)
			}

			// A failed import leaves no gcroot pointing at a path that was
			// never written.
			_, err = os.Lstat(gcRootFor(gcRootDir, ni.StorePath))
			if !os.IsNotExist(err) {
				t.Errorf("gcroot survived a failed import: %v", err)
			}
		})
	}
}

// TestDropSpoolKeepsAReplacedSpool verifies dropSpool only unlinks the file
// verify actually read. cache.handlePutNar rewrites a spool in place, so a
// second push of the same NAR can be filling this path while an earlier import
// fails; removing that half-written copy would turn the second push's import
// into a 500 and a wasted re-upload.
func TestDropSpoolKeepsAReplacedSpool(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name string
		// rewrite mutates the spool between verify's read and dropSpool, as a
		// concurrent PUT of the same NAR would. nil means nothing touched it.
		rewrite func(t *testing.T, path string)
		// verified reports whether verify got the file open at all.
		verified bool
		want     bool // spool still present afterwards
	}{
		{name: "verified and untouched", verified: true, want: false},
		{name: "never opened", verified: false, want: true},
		{
			name:     "concurrent push still filling it",
			verified: true,
			want:     true,
			rewrite: func(t *testing.T, path string) {
				t.Helper()

				err := os.WriteFile(path, []byte("a longer body being written right now"), 0o600) // #nosec G306 -- test file
				if err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:     "concurrent push rewrote the same length",
			verified: true,
			want:     true,
			rewrite: func(t *testing.T, path string) {
				t.Helper()

				// Same length, so only the mtime distinguishes it. Set the time
				// explicitly: the kernel's inode clock is coarse enough that a
				// rewrite this quick can land on the same nanosecond.
				err := os.WriteFile(path, []byte("SPOOLED"), 0o600) // #nosec G306 -- test file
				if err != nil {
					t.Fatal(err)
				}

				now := time.Now().Add(time.Second)

				err = os.Chtimes(path, now, now)
				if err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := spoolNar(t, dir, strings.ReplaceAll(tt.name, " ", "-")+".nar", []byte("spooled"))

			var fi os.FileInfo

			if tt.verified {
				var err error

				fi, err = os.Stat(p)
				if err != nil {
					t.Fatal(err)
				}
			}

			if tt.rewrite != nil {
				tt.rewrite(t, p)
			}

			dropSpool(p, fi)

			_, err := os.Stat(p)
			if got := err == nil; got != tt.want {
				t.Errorf("spool present = %v, want %v (stat: %v)", got, tt.want, err)
			}
		})
	}
}

// TestSpoolSurvivesImportOfAReplacedSpool drives the same race through Import:
// verify reads the spool, a concurrent PUT of the same NAR rewrites it, and the
// import then finishes. The rewritten spool must survive so the second push's
// own import can still read it — a successful import has no more right to
// unlink the other push's half-written body than a failed one.
func TestSpoolSurvivesImportOfAReplacedSpool(t *testing.T) {
	tests := []struct {
		name string
		exit int // what the nix-store shim returns
	}{
		{name: "failed import", exit: 1},
		{name: "successful import", exit: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			good := buildNarFor(t, storeFixture(t))
			ni := narInfoFor(synthStorePath("replaced-spool"), testNarURL, good)

			spoolDir := t.TempDir()
			spoolPath := spoolNar(t, spoolDir, "test.nar", good)

			// The shim stands in for the concurrent PUT by rewriting the spool
			// while the import is running — exactly the window where verify has
			// already read the file.
			fakeNixStore(t, "cp "+spoolPath+" "+spoolPath+".copy; cat "+spoolPath+".copy "+
				spoolPath+".copy >"+spoolPath+"; exit "+strconv.Itoa(tt.exit))

			imp := &Importer{SpoolDir: spoolDir, GCRootDir: t.TempDir()}

			err := imp.Import(context.Background(), ni)
			if (err != nil) != (tt.exit != 0) {
				t.Fatalf("Import error = %v, want error: %v", err, tt.exit != 0)
			}

			_, err = os.Stat(spoolPath)
			if err != nil {
				t.Errorf("import removed a spool a concurrent push had replaced: %v", err)
			}
		})
	}
}

// TestImportStreamsVerifiedBytes drives the verify → import window: verify
// hashes the spool, a concurrent PUT of the same NAR replaces it with content
// nothing has hashed, and the import must still stream the bytes verify read.
// Both passes share one open file, so the replacement is invisible to the
// import — provided the PUT replaces the spool by rename, which is
// cache.handlePutNar's half of this. A truncate in place keeps the inode and
// the fd follows it.
func TestImportStreamsVerifiedBytes(t *testing.T) {
	good := buildNarFor(t, storeFixture(t))
	ni := narInfoFor(synthStorePath("verified-bytes"), testNarURL, good)

	spoolDir := t.TempDir()
	spoolPath := spoolNar(t, spoolDir, "test.nar", good)

	captured := filepath.Join(t.TempDir(), "stdin")
	fakeNixStore(t, "cat >"+captured)

	imp := &Importer{SpoolDir: spoolDir, GCRootDir: t.TempDir()}
	ctx := context.Background()

	f, err := os.Open(spoolPath) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}

	defer f.Close()

	_, err = imp.verify(ctx, f, ni)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	// The concurrent PUT, landing between the two passes.
	poison := []byte("MALICIOUS-CONTENT-NEVER-HASHED")

	err = os.WriteFile(spoolPath+".new", poison, 0o600) // #nosec G306 -- test file
	if err != nil {
		t.Fatal(err)
	}

	err = os.Rename(spoolPath+".new", spoolPath)
	if err != nil {
		t.Fatal(err)
	}

	err = imp.importNar(ctx, f, ni)
	if err != nil {
		t.Fatalf("importNar: %v", err)
	}

	sent, err := os.ReadFile(captured) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(sent, poison) {
		t.Error("unverified bytes reached nix-store --import")
	}

	if !bytes.Contains(sent, []byte("tsnixcache test fixture")) {
		t.Error("the export stream is not the NAR verify hashed")
	}
}

// TestImportNarBoundsDecompression verifies the pass that reaches nix-store is
// bounded by NarSize the way verify is: an unbounded copy would stream whatever
// a compression bomb expands to straight into the store's stdin.
func TestImportNarBoundsDecompression(t *testing.T) {
	var compressed bytes.Buffer

	enc, err := nixcompress.Encoder(context.Background(), &compressed, compressionZstd, false)
	if err != nil {
		t.Fatal(err)
	}

	_, err = enc.Write(make([]byte, 8<<20))
	if err != nil {
		t.Fatal(err)
	}

	err = enc.Close()
	if err != nil {
		t.Fatal(err)
	}

	spoolDir := t.TempDir()
	spoolPath := spoolNar(t, spoolDir, "bomb.nar.zst", compressed.Bytes())

	counted := filepath.Join(t.TempDir(), "count")
	fakeNixStore(t, "wc -c >"+counted)

	imp := &Importer{SpoolDir: spoolDir}
	ni := &narinfo.NarInfo{
		StorePath:   testStorePath,
		URL:         "nar/bomb.nar.zst",
		Compression: compressionZstd,
		NarHash:     hashOf(nil),
		NarSize:     10, // the pusher claims 10 bytes; the stream is 8 MiB
	}

	f, err := os.Open(spoolPath) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}

	defer f.Close()

	err = imp.importNar(context.Background(), f, ni)
	if err != nil {
		t.Fatalf("importNar: %v", err)
	}

	out, err := os.ReadFile(counted) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}

	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}

	// NarSize plus the export framing; anything near 8 MiB means the copy ran
	// to the end of the bomb.
	if n > 1024 {
		t.Errorf("nix-store received %d bytes for a 10-byte NAR; the copy was not bounded", n)
	}
}

// TestImportRefusesASpoolThatIsNotAFile verifies a narinfo URL naming something
// other than a spooled NAR is refused and, above all, not unlinked. "URL:
// nar/zstd-cache" used to delete the compression cache directory, which is only
// created at startup, so every later zstd encode failed for the life of the
// process; "URL: " deleted the spool directory itself.
func TestImportRefusesASpoolThatIsNotAFile(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "a directory in the spool", url: "nar/" + zstdCacheName},
		{name: "the spool directory itself", url: ""},
		{name: "the spool's parent", url: "nar/.."},
		{name: "a dot", url: "nar/."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spoolDir := t.TempDir()
			victim := filepath.Join(spoolDir, zstdCacheName)

			err := os.Mkdir(victim, 0o750)
			if err != nil {
				t.Fatal(err)
			}

			imp := &Importer{SpoolDir: spoolDir, GCRootDir: t.TempDir()}
			ni := narInfoFor(synthStorePath("not-a-file"), tt.url, nil)

			err = imp.Import(context.Background(), ni)
			if err == nil {
				t.Fatal("expected Import to fail, got nil")
			}

			for _, kept := range []string{victim, spoolDir, filepath.Dir(spoolDir)} {
				_, statErr := os.Stat(kept)
				if statErr != nil {
					t.Errorf("Import removed %s: %v", kept, statErr)
				}
			}
		})
	}
}

// TestImportRefusesASpoolSymlinkOutOfTheSpool: the NAR name is reduced to a
// basename before it is joined, so a pushed narinfo cannot name a path outside
// the spool directly — but a symlink sitting at that name inside the spool used
// to be followed all the same. Whatever it pointed at would then be hashed,
// imported and re-signed with the cache's own key, which is the one thing the
// push grant is meant to bound. serve checks the spool's ownership and mode at
// startup; this holds the same guarantee open for the life of the process.
func TestImportRefusesASpoolSymlinkOutOfTheSpool(t *testing.T) {
	narBytes := buildNarFor(t, storeFixture(t))

	spoolDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere.nar")

	err := os.WriteFile(outside, narBytes, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	// The link is valid and its target is a perfectly good NAR: only the fact
	// that it leaves the spool may stop the import.
	err = os.Symlink(outside, filepath.Join(spoolDir, "planted.nar"))
	if err != nil {
		t.Fatal(err)
	}

	// The declared hashes are the target's own, so verify would be perfectly
	// happy with it: only the fact that the open leaves the spool may stop this.
	ni := narInfoFor(synthStorePath("planted"), "nar/planted.nar", narBytes)

	// The real check. "Import returned an error" is far too weak — following the
	// link, hashing the target and handing it to nix-store also ends in an error
	// on any machine whose nix refuses an unsigned path, and that run is the
	// vulnerable one. What must be true is that the content behind the link never
	// reached nix-store at all.
	ran := filepath.Join(t.TempDir(), "ran")
	fakeNixStore(t, "touch "+ran)

	imp := &Importer{SpoolDir: spoolDir, GCRootDir: t.TempDir()}

	err = imp.Import(context.Background(), ni)
	if err == nil {
		t.Fatal("Import followed a symlink out of the spool")
	}

	_, err = os.Stat(ran)
	if err == nil {
		t.Error("nix-store --import ran on content reached through a symlink out of the spool")
	}

	// The target must survive: Import unlinks spool files it is done with, and
	// following the link to unlink it would delete a file it never owned.
	_, err = os.Stat(outside)
	if err != nil {
		t.Errorf("Import removed the symlink's target: %v", err)
	}
}

// TestImportRefusesAForeignStorePath verifies a pushed narinfo cannot aim a
// gcroot outside the store. pruneOldGCRoots refuses to remove a root pointing
// outside the store, so such a link would be permanent — pinning an arbitrary
// path against nix-collect-garbage and warning on every GC run forever.
func TestImportRefusesAForeignStorePath(t *testing.T) {
	narBytes := buildNarFor(t, storeFixture(t))

	paths := []string{
		"/etc/passwd",
		"/tmp/../../root/.ssh/authorized_keys",
		"../../../../etc",
		"/nix/store/..",
		"/nix/store/../../etc",
		"/nix/store/nothashy-name",
		"/nix/storeother/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-name",
		"",
	}

	for _, storePath := range paths {
		t.Run(storePath, func(t *testing.T) {
			spoolDir := t.TempDir()
			spoolNar(t, spoolDir, "test.nar", narBytes)

			// The root dir does not exist yet, so anything created for this
			// path shows up as a stray entry in its parent.
			parent := t.TempDir()
			gcRootDir := filepath.Join(parent, "roots")

			ran := filepath.Join(t.TempDir(), "ran")
			fakeNixStore(t, "touch "+ran)

			imp := &Importer{SpoolDir: spoolDir, GCRootDir: gcRootDir}

			err := imp.Import(context.Background(), narInfoFor(storePath, testNarURL, narBytes))
			if err == nil {
				t.Fatal("expected Import to fail for a path outside the store, got nil")
			}

			ents, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}

			if len(ents) != 0 {
				t.Errorf("Import created %v around the gcroot dir", ents)
			}

			_, err = os.Stat(ran)
			if !os.IsNotExist(err) {
				t.Errorf("nix-store ran for a path outside the store: %v", err)
			}
		})
	}
}

// TestConcurrentGCRootsForSamePath verifies two imports of the same path do not
// trip over each other's temporary link. A fixed ".tmp" name let one import
// unlink the other's, so a rename that was about to install a perfectly correct
// root failed with ENOENT — a 500 and a full re-upload for the client.
func TestConcurrentGCRootsForSamePath(t *testing.T) {
	gcRootDir := t.TempDir()
	imp := &Importer{GCRootDir: gcRootDir}
	storePath := synthStorePath("gcroot-race")

	var wg sync.WaitGroup

	errs := make([]error, 16)

	for i := range errs {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			_, errs[idx] = imp.addGCRoot(storePath)
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("addGCRoot %d: %v", i, err)
		}
	}

	// One root, no temporary litter: pruneOldGCRoots warns about every entry
	// that is not a gcroot.
	ents, err := os.ReadDir(gcRootDir)
	if err != nil {
		t.Fatal(err)
	}

	if len(ents) != 1 || ents[0].Name() != filepath.Base(gcRootFor(gcRootDir, storePath)) {
		t.Errorf("gcroot dir holds %v, want just the root", ents)
	}
}

// TestGCRootCreatedBeforeImport verifies the gcroot exists while nix-store is
// running, not just after it returns: in that window a concurrent
// nix-collect-garbage would reap the freshly imported path.
func TestGCRootCreatedBeforeImport(t *testing.T) {
	narBytes := buildNarFor(t, storeFixture(t))
	ni := narInfoFor(synthStorePath("gcroot-order"), testNarURL, narBytes)

	spoolDir := t.TempDir()
	spoolNar(t, spoolDir, "test.nar", narBytes)

	gcRootDir := t.TempDir()
	gcroot := gcRootFor(gcRootDir, ni.StorePath)
	observed := filepath.Join(t.TempDir(), "observed")

	// The shim stands in for nix-store and records whether the gcroot was
	// already in place when the import ran.
	fakeNixStore(t, "test -L "+gcroot+" && echo yes >"+observed)

	imp := &Importer{SpoolDir: spoolDir, GCRootDir: gcRootDir}

	err := imp.Import(context.Background(), ni)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	_, err = os.Stat(observed)
	if err != nil {
		t.Errorf("gcroot %s did not exist while nix-store was running: %v", gcroot, err)
	}
}

// TestGCRootFailureFailsImport verifies an unusable gcroot directory fails the
// import instead of warning: a path with no root can be collected minutes later,
// so reporting success would be a lie the client cannot see.
func TestGCRootFailureFailsImport(t *testing.T) {
	narBytes := buildNarFor(t, storeFixture(t))
	ni := narInfoFor(synthStorePath("gcroot-fail"), testNarURL, narBytes)

	spoolDir := t.TempDir()
	spoolNar(t, spoolDir, "test.nar", narBytes)

	// A regular file where the gcroot dir should be: MkdirAll fails with ENOTDIR.
	blocker := filepath.Join(t.TempDir(), "file")

	err := os.WriteFile(blocker, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	ran := filepath.Join(t.TempDir(), "ran")
	fakeNixStore(t, "touch "+ran)

	imp := &Importer{SpoolDir: spoolDir, GCRootDir: filepath.Join(blocker, "roots")}

	err = imp.Import(context.Background(), ni)
	if err == nil {
		t.Fatal("expected Import to fail when the gcroot cannot be created, got nil")
	}

	if !strings.Contains(err.Error(), "gcroot") {
		t.Errorf("error %q does not mention the gcroot", err)
	}

	// The root goes in first, so the import never started.
	_, statErr := os.Stat(ran)
	if !os.IsNotExist(statErr) {
		t.Errorf("nix-store ran despite the gcroot failing: %v", statErr)
	}
}

// TestGCRootSurvivesFailedRepush verifies a failing import leaves alone a gcroot
// it did not create. The same path is pushed twice — a retry, a second builder,
// a client whose HEAD raced — and the second import fails. The path from the
// first push is in the store and valid; stripping its root hands it to the next
// nix-collect-garbage, and nothing ever re-pushes it (upload skips paths whose
// narinfo is already present).
func TestGCRootSurvivesFailedRepush(t *testing.T) {
	narBytes := buildNarFor(t, storeFixture(t))
	ni := narInfoFor(synthStorePath("gcroot-repush"), testNarURL, narBytes)

	spoolDir := t.TempDir()
	spoolNar(t, spoolDir, "test.nar", narBytes)

	// The root an earlier, successful import left behind.
	gcRootDir := t.TempDir()
	gcroot := gcRootFor(gcRootDir, ni.StorePath)

	err := os.Symlink(ni.StorePath, gcroot)
	if err != nil {
		t.Fatal(err)
	}

	fakeNixStore(t, "exit 1")

	imp := &Importer{SpoolDir: spoolDir, GCRootDir: gcRootDir}

	err = imp.Import(context.Background(), ni)
	if err == nil {
		t.Fatal("expected Import to fail, got nil")
	}

	target, err := os.Readlink(gcroot)
	if err != nil {
		t.Fatalf("a failed re-push removed the gcroot of a path already in the store: %v", err)
	}

	if target != ni.StorePath {
		t.Errorf("gcroot target = %q, want %q", target, ni.StorePath)
	}
}

// requireInStore fails the test unless storePath is a valid path in the chroot
// store at storeURI. --query --hash is the check that bites: --query --outputs
// prints the path and exits 0 for a path that was never imported.
func requireInStore(t *testing.T, storeURI, storePath string) {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), "nix-store", "--store", storeURI, "--query", "--hash", storePath) // #nosec G204 -- nix-store is a trusted system binary

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s is not in the store at %s: %v\n%s", storePath, storeURI, err, out)
	}
}

// TestImportRoundTrip imports a fixture into a chroot store and then verifies
// nix-store can query the imported path.
func TestImportRoundTrip(t *testing.T) {
	requireNix(t)

	storePath := synthStorePath("round-trip")
	narBytes := buildNarFor(t, storeFixture(t))
	ni := narInfoFor(storePath, testNarURL, narBytes)

	spoolDir := t.TempDir()
	spoolNar(t, spoolDir, "test.nar", narBytes)

	storeURI := nixStoreTempDir(t)
	imp := &Importer{
		SpoolDir:    spoolDir,
		GCRootDir:   t.TempDir(),
		NixStoreURI: storeURI,
	}

	err := imp.Import(context.Background(), ni)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	requireInStore(t, storeURI, storePath)
}

// importPath pushes storePath into the chroot store at storeURI with the
// references and deriver a pusher's narinfo would have carried.
func importPath(t *testing.T, storeURI, gcRootDir, storePath string, refs []string, deriver string) {
	t.Helper()

	narBytes := buildNarFor(t, storeFixture(t))
	ni := narInfoFor(storePath, testNarURL, narBytes)
	ni.References = refs
	ni.Deriver = deriver

	spoolDir := t.TempDir()
	spoolNar(t, spoolDir, "test.nar", narBytes)

	imp := &Importer{SpoolDir: spoolDir, GCRootDir: gcRootDir, NixStoreURI: storeURI}

	err := imp.Import(t.Context(), ni)
	if err != nil {
		t.Fatalf("Import %s: %v", storePath, err)
	}

	requireInStore(t, storeURI, storePath)
}

// storeQuery runs `nix-store --store storeURI --query <args>` and returns the
// non-empty lines it printed.
func storeQuery(t *testing.T, storeURI string, args ...string) []string {
	t.Helper()

	argv := append([]string{"--store", storeURI, "--query"}, args...)
	cmd := exec.CommandContext(t.Context(), "nix-store", argv...) // #nosec G204 -- nix-store is a trusted system binary

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("nix-store %v: %v", argv, err)
	}

	var lines []string

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}

	return lines
}

// TestImportPreservesReferencesAndDeriver: the export stream is the only route
// the pushed narinfo's References and Deriver take into the store, and nothing
// downstream reads them back. A stream that dropped them imports perfectly
// cleanly and leaves the store believing the path stands alone — so the next
// nix-collect-garbage takes its dependencies out from under it, and a serve of
// the closure from this cache is missing everything the path actually needs.
func TestImportPreservesReferencesAndDeriver(t *testing.T) {
	requireNix(t)

	storeURI := nixStoreTempDir(t)
	gcRootDir := t.TempDir()

	// The dependency has to be valid in the store first: nix refuses to register
	// a path whose references are not themselves registered.
	dep := synthStorePath("refs-dep")
	importPath(t, storeURI, gcRootDir, dep, nil, "")

	root := synthStorePath("refs-root")
	deriver := synthStorePath("refs-root.drv")

	// Self-reference alongside the dependency: a store path referring to itself is
	// the ordinary case, and it must survive the round trip with the rest.
	importPath(t, storeURI, gcRootDir, root, []string{root, dep}, deriver)

	want := []string{dep, root}
	slices.Sort(want)

	got := storeQuery(t, storeURI, "--references", root)
	slices.Sort(got)

	if !slices.Equal(got, want) {
		t.Errorf("nix-store -q --references = %v, want %v: the export stream lost references", got, want)
	}

	if got := storeQuery(t, storeURI, "--deriver", root); !slices.Equal(got, []string{deriver}) {
		t.Errorf("nix-store -q --deriver = %v, want [%s]: the export stream lost the deriver", got, deriver)
	}
}

// TestDuplicateImport verifies that importing the same path twice does not error.
func TestDuplicateImport(t *testing.T) {
	requireNix(t)

	storePath := synthStorePath("duplicate")
	narBytes := buildNarFor(t, storeFixture(t))

	storeURI := nixStoreTempDir(t)
	gcRootDir := t.TempDir()

	for _, attempt := range []string{"first", "second"} {
		spoolDir := t.TempDir()
		spoolNar(t, spoolDir, "test.nar", narBytes)

		imp := &Importer{SpoolDir: spoolDir, GCRootDir: gcRootDir, NixStoreURI: storeURI}

		err := imp.Import(context.Background(), narInfoFor(storePath, testNarURL, narBytes))
		if err != nil {
			t.Fatalf("%s Import: %v", attempt, err)
		}

		requireInStore(t, storeURI, storePath)
	}
}

// TestGCRootCreated verifies a gcroot symlink is created for an imported path.
func TestGCRootCreated(t *testing.T) {
	requireNix(t)

	storePath := synthStorePath("gcroot-created")
	narBytes := buildNarFor(t, storeFixture(t))

	spoolDir := t.TempDir()
	spoolNar(t, spoolDir, "test.nar", narBytes)

	gcRootDir := t.TempDir()
	imp := &Importer{
		SpoolDir:    spoolDir,
		GCRootDir:   gcRootDir,
		NixStoreURI: nixStoreTempDir(t),
	}

	err := imp.Import(context.Background(), narInfoFor(storePath, testNarURL, narBytes))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	gcroot := gcRootFor(gcRootDir, storePath)

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

	if target != storePath {
		t.Errorf("gcroot target = %q, want %q", target, storePath)
	}
}

// TestSpoolFileRemovedAfterImport verifies the spool file is deleted on success.
func TestSpoolFileRemovedAfterImport(t *testing.T) {
	requireNix(t)

	storePath := synthStorePath("spool-removed")
	narBytes := buildNarFor(t, storeFixture(t))

	spoolDir := t.TempDir()
	spoolPath := spoolNar(t, spoolDir, "test.nar", narBytes)

	imp := &Importer{
		SpoolDir:    spoolDir,
		GCRootDir:   t.TempDir(),
		NixStoreURI: nixStoreTempDir(t),
	}

	err := imp.Import(context.Background(), narInfoFor(storePath, testNarURL, narBytes))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	_, err = os.Stat(spoolPath)
	if err == nil {
		t.Errorf("spool file %s still exists after import", spoolPath)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected error checking spool file: %v", err)
	}
}

// TestImportZstdCompression spools a zstd-compressed NAR, sets Compression:zstd
// in the narinfo, and verifies Import decompresses and imports correctly.
func TestImportZstdCompression(t *testing.T) {
	requireNix(t)

	storePath := synthStorePath("zstd")
	narBytes := buildNarFor(t, storeFixture(t))

	// Compress the NAR with zstd.
	var compressed bytes.Buffer

	enc, err := nixcompress.Encoder(context.Background(), &compressed, compressionZstd, false)
	if err != nil {
		t.Fatalf("nixcompress.Encoder(context.Background(), zstd): %v", err)
	}

	_, err = enc.Write(narBytes)
	if err != nil {
		t.Fatalf("write to zstd encoder: %v", err)
	}

	err = enc.Close()
	if err != nil {
		t.Fatalf("close zstd encoder: %v", err)
	}

	compressedBytes := compressed.Bytes()

	spoolDir := t.TempDir()
	spoolNar(t, spoolDir, "test.nar.zst", compressedBytes)

	ni := narInfoFor(storePath, "nar/test.nar.zst", narBytes)
	ni.Compression = compressionZstd
	ni.FileHash = hashOf(compressedBytes)
	ni.FileSize = uint64(len(compressedBytes))

	storeURI := nixStoreTempDir(t)
	imp := &Importer{
		SpoolDir:    spoolDir,
		GCRootDir:   t.TempDir(),
		NixStoreURI: storeURI,
	}

	err = imp.Import(context.Background(), ni)
	if err != nil {
		t.Fatalf("Import with zstd: %v", err)
	}

	requireInStore(t, storeURI, storePath)
}

// TestImportXzCompression is the same round trip through xz, which is always
// decoded by the xz binary. That decoder is handed the spool's fd directly, so
// the child moves the offset both passes share — the case where importing from
// the file verify read, rather than re-opening it, could have gone wrong.
func TestImportXzCompression(t *testing.T) {
	requireNix(t)

	if !nixcompress.ExternalAvailable("xz") {
		t.Skip("xz not in PATH")
	}

	storePath := synthStorePath("xz")
	narBytes := buildNarFor(t, storeFixture(t))

	var compressed bytes.Buffer

	enc, err := nixcompress.Encoder(context.Background(), &compressed, "xz", false)
	if err != nil {
		t.Fatal(err)
	}

	_, err = enc.Write(narBytes)
	if err != nil {
		t.Fatal(err)
	}

	err = enc.Close()
	if err != nil {
		t.Fatal(err)
	}

	spoolDir := t.TempDir()
	spoolNar(t, spoolDir, "test.nar.xz", compressed.Bytes())

	ni := narInfoFor(storePath, "nar/test.nar.xz", narBytes)
	ni.Compression = "xz"
	ni.FileHash = hashOf(compressed.Bytes())
	ni.FileSize = uint64(compressed.Len()) // #nosec G115 -- len() of a test fixture

	storeURI := nixStoreTempDir(t)
	imp := &Importer{
		SpoolDir:    spoolDir,
		GCRootDir:   t.TempDir(),
		NixStoreURI: storeURI,
	}

	err = imp.Import(context.Background(), ni)
	if err != nil {
		t.Fatalf("Import with xz: %v", err)
	}

	requireInStore(t, storeURI, storePath)
}

// TestConcurrentImports imports several paths at once through one shared gcroot
// directory, which is what the server does with ImportConcurrency set to NumCPU:
// concurrent renames and Lstats against the same dir, and every import must end
// up rooted.
//
// The chroot stores are per goroutine on purpose. A chroot store is a bare
// SQLite database with no daemon to serialise writers, so several nix-store
// --import processes sharing one fail with "database is busy" — a property of
// nix, not of this package. The server's own store is the daemon, which does
// serialise.
func TestConcurrentImports(t *testing.T) {
	requireNix(t)

	narBytes := buildNarFor(t, storeFixture(t))

	paths := []string{
		synthStorePath("concurrent-a"),
		synthStorePath("concurrent-b"),
		synthStorePath("concurrent-c"),
		synthStorePath("concurrent-d"),
	}

	gcRootDir := t.TempDir()
	stores := make([]string, len(paths))

	var wg sync.WaitGroup

	errs := make([]error, len(paths))

	for i, p := range paths {
		spoolDir := t.TempDir()
		spoolNar(t, spoolDir, "test.nar", narBytes)

		stores[i] = nixStoreTempDir(t)
		imp := &Importer{
			SpoolDir:    spoolDir,
			GCRootDir:   gcRootDir,
			NixStoreURI: stores[i],
		}

		wg.Add(1)

		go func(idx int, storePath string) {
			defer wg.Done()

			errs[idx] = imp.Import(context.Background(), narInfoFor(storePath, testNarURL, narBytes))
		}(i, p)
	}

	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("goroutine %d: %v", i, e)
		}
	}

	for i, p := range paths {
		requireInStore(t, stores[i], p)

		_, err := os.Lstat(gcRootFor(gcRootDir, p))
		if err != nil {
			t.Errorf("gcroot missing after a concurrent import: %v", err)
		}
	}
}
