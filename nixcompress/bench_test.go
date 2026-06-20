package nixcompress_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/kradalby/tsnixcache/nixcompress"
)

// encodeBench compresses data with the given codec+external flag, discarding output.
func encodeBench(b *testing.B, data []byte, compression string, useExternal bool) {
	b.Helper()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		enc, err := nixcompress.Encoder(io.Discard, compression, useExternal)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := enc.Write(data); err != nil {
			b.Fatal(err)
		}
		if err := enc.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// compressOnce returns the compressed form of data using the given codec.
func compressOnce(b *testing.B, data []byte, compression string, useExternal bool) []byte {
	b.Helper()
	var buf bytes.Buffer
	enc, err := nixcompress.Encoder(&buf, compression, useExternal)
	if err != nil {
		b.Fatalf("Encoder(%q, useExternal=%v): %v", compression, useExternal, err)
	}
	if _, err := enc.Write(data); err != nil {
		b.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}

// decodeBench decompresses pre-compressed data, discarding output.
func decodeBench(b *testing.B, compressed []byte, compression string, useExternal bool) {
	b.Helper()
	b.SetBytes(int64(len(compressed)))
	b.ReportAllocs()
	for b.Loop() {
		r := bytes.NewReader(compressed)
		dec, err := nixcompress.Decoder(r, compression, useExternal)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, dec); err != nil {
			b.Fatal(err)
		}
		if err := dec.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// --- Zstd encode: PureGo vs External ---

func BenchmarkEncode_Zstd_PureGo_1MB(b *testing.B) {
	data := makePattern(1 << 20)
	encodeBench(b, data, "zstd", false)
}

func BenchmarkEncode_Zstd_External_1MB(b *testing.B) {
	if !nixcompress.ExternalAvailable("zstd") {
		b.Skip("zstd binary not in PATH")
	}
	data := makePattern(1 << 20)
	encodeBench(b, data, "zstd", true)
}

// --- Xz decode: PureGo vs External ---

func BenchmarkDecode_Xz_PureGo_1MB(b *testing.B) {
	raw := makePattern(1 << 20)
	compressed := compressOnce(b, raw, "xz", false)
	decodeBench(b, compressed, "xz", false)
}

func BenchmarkDecode_Xz_External_1MB(b *testing.B) {
	if !nixcompress.ExternalAvailable("xz") {
		b.Skip("xz binary not in PATH")
	}
	raw := makePattern(1 << 20)
	compressed := compressOnce(b, raw, "xz", true)
	decodeBench(b, compressed, "xz", true)
}
