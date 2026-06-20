package nixcompress_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/kradalby/tsnixcache/nixcompress"
)

// makePattern returns n bytes filled with a repeating 0-255 pattern.
func makePattern(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i)
	}
	return data
}

// roundTrip encodes data with compression+useExternal then decodes and returns the result.
func roundTrip(t *testing.T, data []byte, compression string, useExternal bool) []byte {
	t.Helper()

	var buf bytes.Buffer
	enc, err := nixcompress.Encoder(&buf, compression, useExternal)
	if err != nil {
		t.Fatalf("Encoder(%q, useExternal=%v): %v", compression, useExternal, err)
	}
	if _, err := enc.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Encoder.Close: %v", err)
	}

	dec, err := nixcompress.Decoder(&buf, compression, useExternal)
	if err != nil {
		t.Fatalf("Decoder(%q, useExternal=%v): %v", compression, useExternal, err)
	}
	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := dec.Close(); err != nil {
		t.Fatalf("Decoder.Close: %v", err)
	}
	return got
}

// testRoundTrip is the shared round-trip helper for table-driven sub-tests.
func testRoundTrip(t *testing.T, compression string, useExternal bool, data []byte, label string) {
	t.Helper()
	got := roundTrip(t, data, compression, useExternal)
	if !bytes.Equal(got, data) {
		t.Fatalf("%s: round-trip mismatch: got %d bytes, want %d", label, len(got), len(data))
	}
}

// --- Round-trip matrix (4 codecs × 2 external settings = 8 combos) ---

func TestRoundTrip_None_PureGo(t *testing.T) {
	testRoundTrip(t, "none", false, makePattern(1<<20), "1MB")
}

func TestRoundTrip_None_External(t *testing.T) {
	testRoundTrip(t, "none", true, makePattern(1<<20), "1MB")
}

func TestRoundTrip_XZ_PureGo(t *testing.T) { testRoundTrip(t, "xz", false, makePattern(1<<20), "1MB") }

func TestRoundTrip_XZ_External(t *testing.T) { testRoundTrip(t, "xz", true, makePattern(1<<20), "1MB") }

func TestRoundTrip_Zstd_PureGo(t *testing.T) {
	testRoundTrip(t, "zstd", false, makePattern(1<<20), "1MB")
}

func TestRoundTrip_Zstd_External(t *testing.T) {
	testRoundTrip(t, "zstd", true, makePattern(1<<20), "1MB")
}

// bzip2: stdlib only; external=true falls back to pure-Go automatically.
func TestRoundTrip_Bzip2_PureGo(t *testing.T) {
	testRoundTrip(t, "bzip2", false, makePattern(1<<20), "1MB")
}

func TestRoundTrip_Bzip2_External(t *testing.T) {
	testRoundTrip(t, "bzip2", true, makePattern(1<<20), "1MB")
}

// --- Edge cases ---

func TestRoundTrip_EmptyInput(t *testing.T) {
	empty := []byte{}
	for _, comp := range []string{"none", "xz", "zstd", "bzip2"} {
		for _, ext := range []bool{false, true} {
			t.Run(comp, func(t *testing.T) {
				testRoundTrip(t, comp, ext, empty, "empty")
			})
		}
	}
}

func TestRoundTrip_SingleByte(t *testing.T) {
	single := []byte{0x42}
	for _, comp := range []string{"none", "xz", "zstd", "bzip2"} {
		for _, ext := range []bool{false, true} {
			t.Run(comp, func(t *testing.T) {
				testRoundTrip(t, comp, ext, single, "single-byte")
			})
		}
	}
}

func TestRoundTrip_LargeZstdExternal(t *testing.T) {
	data := makePattern(10 << 20) // 10 MB
	testRoundTrip(t, "zstd", true, data, "10MB")
}

// --- ExternalAvailable ---

func TestExternalAvailable(t *testing.T) {
	if !nixcompress.ExternalAvailable("xz") {
		t.Error("expected xz to be available in PATH")
	}
	if !nixcompress.ExternalAvailable("zstd") {
		t.Error("expected zstd to be available in PATH")
	}
	if nixcompress.ExternalAvailable("nonexistent-binary-xyz") {
		t.Error("expected nonexistent-binary-xyz NOT to be available")
	}
}

// --- Unknown compression ---

func TestUnknownCompressionDecoder(t *testing.T) {
	var buf bytes.Buffer
	_, err := nixcompress.Decoder(&buf, "lzma-weird", false)
	if err == nil {
		t.Fatal("expected error for unknown compression")
	}
}

func TestUnknownCompressionEncoder(t *testing.T) {
	var buf bytes.Buffer
	_, err := nixcompress.Encoder(&buf, "lzma-weird", false)
	if err == nil {
		t.Fatal("expected error for unknown compression")
	}
}

// --- ProbeCodecs ---

func TestProbeCodecs(t *testing.T) {
	// Just verify it runs without panic and returns a bool.
	_ = nixcompress.ProbeCodecs()
}

// --- Benchmarks ---

func BenchmarkZstdExternal(b *testing.B) {
	data := makePattern(1 << 20)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = roundTripBench(b, data, "zstd", true)
	}
}

func BenchmarkZstdPureGo(b *testing.B) {
	data := makePattern(1 << 20)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = roundTripBench(b, data, "zstd", false)
	}
}

func roundTripBench(b *testing.B, data []byte, compression string, useExternal bool) []byte {
	b.Helper()
	var buf bytes.Buffer
	enc, err := nixcompress.Encoder(&buf, compression, useExternal)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := enc.Write(data); err != nil {
		b.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		b.Fatal(err)
	}
	dec, err := nixcompress.Decoder(&buf, compression, useExternal)
	if err != nil {
		b.Fatal(err)
	}
	got, err := io.ReadAll(dec)
	if err != nil {
		b.Fatal(err)
	}
	dec.Close()
	return got
}
