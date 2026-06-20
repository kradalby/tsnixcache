package niximport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kradalby/tsnixcache/nar"
	"github.com/kradalby/tsnixcache/narinfo"
	"github.com/kradalby/tsnixcache/nixbase32"
	"github.com/kradalby/tsnixcache/nixcompress"
)

// Importer imports NAR files into a nix store.
type Importer struct {
	SpoolDir    string // directory where PUT /nar/{name} spooled files
	GCRootDir   string // /nix/var/nix/gcroots/tsnixcache or similar
	NixStoreURI string // passed to --store; "" or "auto" = system daemon
	UseExternal bool   // use external compression binaries if available
}

// Import processes a narinfo: finds the spooled NAR, verifies, imports.
// Called when PUT /{hash}.narinfo is received.
func (imp *Importer) Import(ctx context.Context, ni *narinfo.NarInfo) error {
	// 1. Find the spooled file
	spoolPath := imp.SpoolPath(ni.URL)
	f, err := os.Open(spoolPath)
	if err != nil {
		return fmt.Errorf("niximport: open spool %s: %w", spoolPath, err)
	}
	defer f.Close()

	// 2. Decompress
	dec, err := nixcompress.Decoder(f, ni.Compression, imp.UseExternal)
	if err != nil {
		return fmt.Errorf("niximport: decoder: %w", err)
	}
	defer dec.Close()

	// 3. Read NAR bytes (need to hash and keep)
	var narBuf bytes.Buffer
	h := sha256.New()
	tr := io.TeeReader(dec, h)
	narSize, err := io.Copy(&narBuf, tr)
	if err != nil {
		return fmt.Errorf("niximport: read nar: %w", err)
	}

	// 4. Verify NarHash
	gotHash := "sha256:" + nixbase32.EncodeToString(h.Sum(nil))
	if gotHash != ni.NarHash {
		return fmt.Errorf("niximport: NarHash mismatch: got %s want %s", gotHash, ni.NarHash)
	}

	// 5. Verify NarSize
	if uint64(narSize) != ni.NarSize {
		return fmt.Errorf("niximport: NarSize mismatch: got %d want %d", narSize, ni.NarSize)
	}

	// 6. Build export stream
	var exportBuf bytes.Buffer
	if err := nar.WriteExport(&exportBuf, narBuf.Bytes(), ni.StorePath, ni.References, ni.Deriver); err != nil {
		return fmt.Errorf("niximport: export: %w", err)
	}

	// 7. Run nix-store --import
	args := []string{"--import"}
	if imp.NixStoreURI != "" && imp.NixStoreURI != "auto" {
		args = append([]string{"--store", imp.NixStoreURI}, args...)
	}
	cmd := exec.CommandContext(ctx, "nix-store", args...)
	cmd.Stdin = &exportBuf
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("niximport: nix-store --import: %w\n%s", err, out)
	}
	slog.Info("niximport: imported", "path", ni.StorePath)

	// 8. Create gcroot symlink (best-effort)
	if imp.GCRootDir != "" {
		hashPart := filepath.Base(ni.StorePath)
		if len(hashPart) > 32 {
			hashPart = hashPart[:32]
		}
		gcroot := filepath.Join(imp.GCRootDir, hashPart)
		_ = os.MkdirAll(imp.GCRootDir, 0o755)
		_ = os.Remove(gcroot) // remove stale
		if err := os.Symlink(ni.StorePath, gcroot); err != nil {
			slog.Warn("niximport: gcroot symlink", "err", err)
		}
	}

	// 9. Clean up spool file
	_ = os.Remove(spoolPath)

	return nil
}

// SpoolPath returns the expected spool file path for a NAR URL.
// e.g. URL "nar/abc123.nar.xz" → SpoolDir/abc123.nar.xz
func (imp *Importer) SpoolPath(narURL string) string {
	// narURL is like "nar/abc123.nar" or "nar/abc123.nar.xz"
	// or could have ?hash=... query params — strip those
	base := narURL
	if i := strings.Index(base, "?"); i >= 0 {
		base = base[:i]
	}
	base = filepath.Base(base)
	return filepath.Join(imp.SpoolDir, base)
}
