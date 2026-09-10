// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package niximport

import (
	"bytes"
	"crypto/sha256"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kradalby/tsnixcache/narinfo"
	"github.com/kradalby/tsnixcache/nixcompress"
)

const (
	compressionNone  = "none"
	compressionBzip2 = "bzip2"
)

type benchCodec struct {
	name, compression string
	external          bool
}

var benchCodecs = []benchCodec{
	{name: compressionNone, compression: compressionNone},
	{name: "zstd-go", compression: compressionZstd},
	{name: "zstd-external", compression: compressionZstd, external: true},
	{name: "xz", compression: "xz"},
	{name: compressionBzip2, compression: compressionBzip2},
}

func benchNar(tb testing.TB, size int, high bool) []byte {
	tb.Helper()
	dir := tb.TempDir()
	f, err := os.Create(filepath.Join(dir, "blob")) // #nosec G304 -- private fixture under b.TempDir.
	require.NoError(tb, err)

	seed := sha256.Sum256([]byte("tsnixcache import fixture"))

	chunk := bytes.Repeat(seed[:], 2048)
	for remaining := size; remaining > 0; remaining -= min(remaining, len(chunk)) {
		if high {
			for offset := 0; offset < len(chunk); offset += len(seed) {
				seed = sha256.Sum256(seed[:])
				copy(chunk[offset:], seed[:])
			}
		}

		_, err = f.Write(chunk[:min(remaining, len(chunk))])
		require.NoError(tb, err)
	}

	require.NoError(tb, f.Close())

	return buildNarFor(tb, dir)
}

func benchEncoded(tb testing.TB, raw []byte, compression string) ([]byte, *narinfo.NarInfo) {
	tb.Helper()

	var encoded bytes.Buffer

	w, err := nixcompress.Encoder(tb.Context(), &encoded, compression, false)
	require.NoError(tb, err)
	_, err = w.Write(raw)
	require.NoError(tb, err)
	require.NoError(tb, w.Close())

	ni := narInfoFor(synthStorePath("bench"), "nar/bench.nar", raw)
	ni.Compression = compression
	ni.FileHash = hashOf(encoded.Bytes())
	ni.FileSize = uint64(len(encoded.Bytes()))

	return encoded.Bytes(), ni
}

func BenchmarkVerify(b *testing.B) { benchmarkPipeline(b, true) }
func BenchmarkImport(b *testing.B) { benchmarkPipeline(b, false) }

func benchmarkPipeline(b *testing.B, verify bool) {
	b.Helper()
	requireNix(b)

	for _, binary := range []string{"xz", compressionZstd, compressionBzip2} {
		require.True(b, nixcompress.ExternalAvailable(binary), "%s required for codec matrix", binary)
	}

	previous := slog.Default()

	slog.SetDefault(slog.New(slog.DiscardHandler))
	b.Cleanup(func() { slog.SetDefault(previous) })

	for _, entropy := range []string{"low", "high"} {
		b.Run(entropy, func(b *testing.B) {
			raw := benchNar(b, 4<<20, entropy == "high")
			for _, codec := range benchCodecs {
				b.Run(codec.name, func(b *testing.B) {
					encoded, ni := benchEncoded(b, raw, codec.compression)
					b.ReportAllocs()
					b.SetBytes(int64(len(raw)))

					if verify {
						benchmarkVerify(b, encoded, ni, codec.external)
					} else {
						benchmarkImport(b, encoded, ni, codec.external)
					}

					b.ReportMetric(float64(len(encoded)), "compressed-B")
				})
			}
		})
	}
}

func benchmarkVerify(b *testing.B, encoded []byte, ni *narinfo.NarInfo, external bool) {
	b.Helper()
	spool := spoolNar(b, b.TempDir(), "bench.nar", encoded)
	f, err := os.Open(spool) // #nosec G304 -- private fixture under b.TempDir.
	require.NoError(b, err)

	defer f.Close()

	imp := &Importer{UseExternal: external}

	b.ResetTimer()

	for range b.N {
		_, err = f.Seek(0, io.SeekStart)
		require.NoError(b, err)
		_, err = imp.verify(b.Context(), f, ni)
		require.NoError(b, err)
	}

	b.StopTimer()
}

func benchmarkImport(b *testing.B, encoded []byte, ni *narinfo.NarInfo, external bool) {
	b.Helper()
	parent := nixStoreTempDir(b)
	b.ResetTimer()

	for range b.N {
		b.StopTimer()

		dir, err := os.MkdirTemp(parent, "import-") //nolint:usetesting // Remove each imported store before the next iteration.
		require.NoError(b, err)

		spool := filepath.Join(dir, "spool")
		require.NoError(b, os.Mkdir(spool, 0o700))
		spoolPath := spoolNar(b, spool, "bench.nar", encoded)
		uri := filepath.Join(dir, "store")
		out, err := exec.CommandContext(b.Context(), "nix-store", "--store", uri, "--init").CombinedOutput() // #nosec G204 -- fixed command and private benchmark store.
		require.NoError(b, err, "%s", out)

		imp := &Importer{SpoolDir: spool, GCRootDir: filepath.Join(dir, "roots"), NixStoreURI: uri, UseExternal: external}

		b.StartTimer()
		err = imp.Import(b.Context(), ni)
		b.StopTimer()
		require.NoError(b, err)
		out, err = exec.CommandContext(b.Context(), "nix-store", "--store", uri, "--query", "--hash", ni.StorePath).CombinedOutput() // #nosec G204 -- fixed command and private benchmark store.
		require.NoError(b, err, "%s", out)
		require.Equal(b, ni.NarHash, strings.TrimSpace(string(out)))
		target, err := os.Readlink(gcRootFor(imp.GCRootDir, ni.StorePath))
		require.NoError(b, err)
		require.Equal(b, ni.StorePath, target)
		require.NoFileExists(b, spoolPath)
		out, err = exec.CommandContext(b.Context(), "chmod", "-R", "u+w", dir).CombinedOutput() // #nosec G204 -- fixed command and private benchmark store.
		require.NoError(b, err, "%s", out)
		require.NoError(b, os.RemoveAll(dir))
		b.StartTimer()
	}

	b.StopTimer()
}

func BenchmarkGCRootDurability(b *testing.B) {
	for _, mode := range []string{"create", "refresh"} {
		b.Run(mode, func(b *testing.B) {
			imp := &Importer{GCRootDir: b.TempDir()}
			path := synthStorePath("bench-root")
			lock, err := imp.lockGCRoot(b.Context(), path)
			require.NoError(b, err)

			defer lock.Close()

			_, err = imp.addGCRoot(path)
			require.NoError(b, err)
			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				if mode == "create" {
					b.StopTimer()
					require.NoError(b, os.Remove(gcRootFor(imp.GCRootDir, path)))
					b.StartTimer()
				}

				_, err = imp.addGCRoot(path)
				require.NoError(b, err)
			}
		})
	}
}
