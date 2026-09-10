// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package nixcompress_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ulikunitz/xz"

	"github.com/kradalby/tsnixcache/nixcompress"
)

// The process-backed implementations are unexported, so a black-box test can
// only recognise them by name. Without that check a test named _External passes
// happily while running the pure-Go path, asserting nothing about the path it is
// named for.
const (
	externalDecoderType = "*nixcompress.externalDecoder"
	externalEncoderType = "*nixcompress.externalEncoder"
)

// Codec names, spelled once so the round-trip tables and the per-codec
// assertions cannot drift apart.
const (
	codecNone  = "none"
	codecXz    = "xz"
	codecZstd  = "zstd"
	codecBzip2 = "bzip2"
)

// allCodecs is every codec Decoder and Encoder handle.
var allCodecs = []string{codecNone, codecXz, codecZstd, codecBzip2}

// requireBinary skips loudly rather than failing when a codec's binary is
// missing, so a devShell without it does not look like a broken package.
func requireBinary(t *testing.T, name string) {
	t.Helper()

	if !nixcompress.ExternalAvailable(name) {
		t.Skipf("%s binary not in PATH", name)
	}
}

// makePattern returns n bytes filled with a repeating 0-255 pattern.
func makePattern(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i)
	}

	return data
}

// decoderType names the implementation Decoder picked for the given settings.
func decoderType(t *testing.T, compression string, useExternal bool) string {
	t.Helper()

	dec, err := nixcompress.Decoder(context.Background(), bytes.NewReader(nil), compression, useExternal)
	if err != nil {
		t.Fatalf("Decoder(%q, useExternal=%v): %v", compression, useExternal, err)
	}

	defer dec.Close()

	return fmt.Sprintf("%T", dec.(interface{ Unwrap() io.ReadCloser }).Unwrap())
}

// encoderType names the implementation Encoder picked for the given settings.
func encoderType(t *testing.T, compression string, useExternal bool) string {
	t.Helper()

	enc, err := nixcompress.Encoder(context.Background(), io.Discard, compression, useExternal)
	if err != nil {
		t.Fatalf("Encoder(%q, useExternal=%v): %v", compression, useExternal, err)
	}

	defer enc.Close()

	return fmt.Sprintf("%T", enc.(interface{ Unwrap() io.WriteCloser }).Unwrap())
}

// roundTrip encodes data with compression+useExternal then decodes and returns the result.
func roundTrip(t *testing.T, data []byte, compression string, useExternal bool) []byte {
	t.Helper()

	var buf bytes.Buffer

	enc, err := nixcompress.Encoder(context.Background(), &buf, compression, useExternal)
	if err != nil {
		t.Fatalf("Encoder(%q, useExternal=%v): %v", compression, useExternal, err)
	}

	_, err = enc.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	err = enc.Close()
	if err != nil {
		t.Fatalf("Encoder.Close: %v", err)
	}

	dec, err := nixcompress.Decoder(context.Background(), &buf, compression, useExternal)
	if err != nil {
		t.Fatalf("Decoder(%q, useExternal=%v): %v", compression, useExternal, err)
	}

	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	err = dec.Close()
	if err != nil {
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

// --- Round-trip matrix ---

// "none" ignores useExternal entirely, so one test covers both settings.
func TestRoundTrip_None(t *testing.T) {
	testRoundTrip(t, codecNone, false, makePattern(1<<20), "1MB")
}

func TestRoundTrip_XZ_PureGoEncoder(t *testing.T) {
	requireBinary(t, "xz")

	if got := encoderType(t, codecXz, false); got == externalEncoderType {
		t.Fatalf("Encoder(xz, useExternal=false) = %s, want the pure-Go writer", got)
	}

	testRoundTrip(t, codecXz, false, makePattern(1<<20), "1MB")
}

func TestRoundTrip_XZ_External(t *testing.T) {
	requireBinary(t, "xz")

	if got := encoderType(t, codecXz, true); got != externalEncoderType {
		t.Fatalf("Encoder(xz, useExternal=true) = %s, want %s", got, externalEncoderType)
	}

	testRoundTrip(t, codecXz, true, makePattern(1<<20), "1MB")
}

// xz decoding must never run in-process, whatever useExternal says: the pure-Go
// reader sizes its dictionary from the block header and cannot be capped.
func TestXZDecoderAlwaysExternal(t *testing.T) {
	requireBinary(t, "xz")

	for _, useExternal := range []bool{false, true} {
		if got := decoderType(t, codecXz, useExternal); got != externalDecoderType {
			t.Errorf("Decoder(xz, useExternal=%v) = %s, want %s", useExternal, got, externalDecoderType)
		}
	}
}

func TestRoundTrip_Zstd_PureGo(t *testing.T) {
	if got := decoderType(t, codecZstd, false); got == externalDecoderType {
		t.Fatalf("Decoder(zstd, useExternal=false) = %s, want the pure-Go reader", got)
	}

	testRoundTrip(t, codecZstd, false, makePattern(1<<20), "1MB")
}

func TestRoundTrip_Zstd_External(t *testing.T) {
	requireBinary(t, "zstd")

	if got := encoderType(t, codecZstd, true); got != externalEncoderType {
		t.Fatalf("Encoder(zstd, useExternal=true) = %s, want %s", got, externalEncoderType)
	}

	if got := decoderType(t, codecZstd, true); got != externalDecoderType {
		t.Fatalf("Decoder(zstd, useExternal=true) = %s, want %s", got, externalDecoderType)
	}

	testRoundTrip(t, codecZstd, true, makePattern(1<<20), "1MB")
}

// bzip2 ignores useExternal in both directions: encoding always shells out
// because compress/bzip2 is decode-only, decoding never does.
func TestRoundTrip_Bzip2(t *testing.T) {
	requireBinary(t, "bzip2")

	if got := encoderType(t, codecBzip2, false); got != externalEncoderType {
		t.Fatalf("Encoder(bzip2, useExternal=false) = %s, want %s", got, externalEncoderType)
	}

	if got := decoderType(t, codecBzip2, true); got == externalDecoderType {
		t.Fatalf("Decoder(bzip2, useExternal=true) = %s, want the stdlib reader", got)
	}

	testRoundTrip(t, codecBzip2, false, makePattern(1<<20), "1MB")
}

// --- Edge cases ---

func TestRoundTrip_EmptyInput(t *testing.T) {
	empty := []byte{}

	for _, comp := range allCodecs {
		for _, ext := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/external=%v", comp, ext), func(t *testing.T) {
				requireCodecBinaries(t, comp)
				testRoundTrip(t, comp, ext, empty, "empty")
			})
		}
	}
}

func TestRoundTrip_SingleByte(t *testing.T) {
	single := []byte{0x42}

	for _, comp := range allCodecs {
		for _, ext := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/external=%v", comp, ext), func(t *testing.T) {
				requireCodecBinaries(t, comp)
				testRoundTrip(t, comp, ext, single, "single-byte")
			})
		}
	}
}

// requireCodecBinaries skips when a codec cannot complete a round trip without a
// binary this host lacks: xz always decodes out of process and bzip2 always
// encodes out of process.
func requireCodecBinaries(t *testing.T, compression string) {
	t.Helper()

	switch compression {
	case codecXz:
		requireBinary(t, "xz")
	case codecBzip2:
		requireBinary(t, "bzip2")
	}
}

func TestRoundTrip_LargeZstdExternal(t *testing.T) {
	requireBinary(t, "zstd")

	data := makePattern(10 << 20) // 10 MB
	testRoundTrip(t, codecZstd, true, data, "10MB")
}

// --- Decompression bombs ---

// xzDictBomb returns a valid 64-byte xz stream whose block header declares a
// 4 GiB LZMA2 dictionary. Nothing but the one properties byte differs from a
// stream the library itself produced.
func xzDictBomb(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer

	w, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatalf("xz.NewWriter: %v", err)
	}

	_, err = w.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	err = w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The block header follows the 12-byte stream header; its first byte holds
	// the header length in 4-byte units, less one.
	stream := buf.Bytes()
	header := stream[12 : 12+(int(stream[12])+1)*4]

	// Filter flags are the LZMA2 id, a property length of 1 and the property
	// byte itself, which encodes the dictionary size. 40 is the largest code the
	// format allows: 4 GiB.
	patched := false

	for i := 0; i+2 < len(header)-4; i++ {
		if header[i] == 0x21 && header[i+1] == 1 {
			header[i+2] = 40
			patched = true

			break
		}
	}

	if !patched {
		t.Fatal("no LZMA2 filter in the block header")
	}

	binary.LittleEndian.PutUint32(header[len(header)-4:], crc32.ChecksumIEEE(header[:len(header)-4]))

	return stream
}

// zstdFrame returns a 10-byte zstd frame declaring a 1<<windowLog window around
// a single raw byte. The frame is the same size whatever it declares, which is
// the whole problem.
func zstdFrame(windowLog uint8) []byte {
	return []byte{
		0x28, 0xb5, 0x2f, 0xfd, // magic
		0x00, // frame header descriptor: no sizes, no dictionary
		// window descriptor: exponent windowLog-10, mantissa zero.
		(windowLog - 10) << 3,
		0x09, 0x00, 0x00, // last block, raw, one byte
		0x41,
	}
}

// zstdWindowBomb returns a 10-byte zstd frame declaring a 512 MiB window around
// a single raw byte.
func zstdWindowBomb() []byte { return zstdFrame(29) }

// decodeAll runs data through Decoder and returns the output with the first
// error from either the read or the close.
func decodeAll(t *testing.T, data []byte, compression string, useExternal bool) ([]byte, error) {
	t.Helper()

	dec, err := nixcompress.Decoder(context.Background(), bytes.NewReader(data), compression, useExternal)
	if err != nil {
		return nil, err
	}

	got, err := io.ReadAll(dec)

	closeErr := dec.Close()
	if err == nil {
		err = closeErr
	}

	return got, err
}

// Both zstd decode paths must stop at the same window, or the memory a push can
// demand depends on whether --external-compression happens to be on — and the
// looser one is the setting an operator turns on for throughput.
//
// 2 MiB is what nix's default zstd declares and what this package encodes with,
// and 8 MiB is `nix copy --to ...?compression-level=19` — the ceiling exactly.
// The refusals are not all synthetic: nix passes compression-level to libzstd
// unfiltered, so level 20 really does declare 32 MiB and really is turned away.
// 64 MiB is the interesting one, being under zstd(1)'s own 128 MiB default: only
// an explicit limit refuses it, which is the gap the external path used to have.
func TestZstdWindowCeilingSameOnBothPaths(t *testing.T) {
	requireBinary(t, "zstd")

	tests := []struct {
		windowLog uint8
		accept    bool
	}{
		{21, true},
		{23, true},
		{24, false},
		{26, false},
		{29, false},
	}

	for _, tt := range tests {
		for _, useExternal := range []bool{false, true} {
			name := fmt.Sprintf("%dMiB/external=%v", 1<<(tt.windowLog-20), useExternal)

			t.Run(name, func(t *testing.T) {
				got, err := decodeAll(t, zstdFrame(tt.windowLog), codecZstd, useExternal)

				switch {
				case tt.accept && err != nil:
					t.Fatalf("a %d MiB window was refused: %v", 1<<(tt.windowLog-20), err)
				case tt.accept && !bytes.Equal(got, []byte{0x41}):
					t.Fatalf("decoded %q, want the single frame byte", got)
				case !tt.accept && err == nil:
					t.Fatalf("a %d MiB window was accepted; want it refused", 1<<(tt.windowLog-20))
				}
			})
		}
	}
}

// zstdLevel19 compresses data by running the binary directly, bypassing this
// package so the stream is one a pusher could really send.
func zstdLevel19(t *testing.T, data []byte) []byte {
	t.Helper()

	var out bytes.Buffer

	cmd := exec.CommandContext(t.Context(), "zstd", "-19", "-c")
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		t.Fatalf("zstd -19: %v", err)
	}

	return out.Bytes()
}

// The ceiling has to admit real content, not just refuse bombs. Level 19
// declares a window of exactly the limit, so it is the tightest fit that still
// fits — not the top of nix's range, which goes to 22 and is refused, but the
// highest level a push can use. The payload is deliberately larger than the window:
// zstd shrinks the declared window to the input size when it knows it, which
// would quietly make this test prove nothing.
func TestZstdLevel19DecodesOnBothPaths(t *testing.T) {
	requireBinary(t, "zstd")

	data := makePattern(12 << 20)
	compressed := zstdLevel19(t, data)

	for _, useExternal := range []bool{false, true} {
		t.Run(fmt.Sprintf("external=%v", useExternal), func(t *testing.T) {
			got, err := decodeAll(t, compressed, codecZstd, useExternal)
			if err != nil {
				t.Fatalf("a level-19 stream was refused: %v", err)
			}

			if !bytes.Equal(got, data) {
				t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(data))
			}
		})
	}
}

// allocatedBy returns the bytes f caused to be allocated.
func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)

	return after.TotalAlloc - before.TotalAlloc
}

// A pusher chooses the Compression field, so the decoder's buffer sizes come
// straight from bytes it controls. Both of these decode successfully with
// library defaults, after allocating gigabytes for a payload of a few bytes.
const bombAllocCeiling = 64 << 20

func TestXZDictionaryBombRefused(t *testing.T) {
	requireBinary(t, "xz")

	bomb := xzDictBomb(t)

	var err error

	allocated := allocatedBy(func() {
		var dec io.ReadCloser

		dec, err = nixcompress.Decoder(context.Background(), bytes.NewReader(bomb), codecXz, false)
		if err != nil {
			return
		}

		_, err = io.Copy(io.Discard, dec)

		closeErr := dec.Close()
		if err == nil {
			err = closeErr
		}
	})

	if err == nil {
		t.Fatal("a 4 GiB dictionary was accepted; want it refused")
	}

	if allocated > bombAllocCeiling {
		t.Fatalf("decoding the bomb allocated %d MiB, want at most %d MiB",
			allocated>>20, bombAllocCeiling>>20)
	}
}

func TestZstdWindowBombRefused(t *testing.T) {
	bomb := zstdWindowBomb()

	var err error

	allocated := allocatedBy(func() {
		var dec io.ReadCloser

		dec, err = nixcompress.Decoder(context.Background(), bytes.NewReader(bomb), codecZstd, false)
		if err != nil {
			return
		}

		_, err = io.Copy(io.Discard, dec)

		closeErr := dec.Close()
		if err == nil {
			err = closeErr
		}
	})

	if err == nil {
		t.Fatal("a 512 MiB window was accepted; want it refused")
	}

	if allocated > bombAllocCeiling {
		t.Fatalf("decoding the bomb allocated %d MiB, want at most %d MiB",
			allocated>>20, bombAllocCeiling>>20)
	}
}

// --- ExternalAvailable ---

func TestExternalAvailable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping external-binary availability check in short mode")
	}

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

// --- Requirements ---

// byBinary indexes a requirement list for assertions that read as prose.
func byBinary(reqs []nixcompress.Requirement) map[string]nixcompress.Requirement {
	m := make(map[string]nixcompress.Requirement, len(reqs))
	for _, r := range reqs {
		m[r.Binary] = r
	}

	return m
}

func TestRequirements(t *testing.T) {
	for _, useExternal := range []bool{false, true} {
		t.Run(fmt.Sprintf("external=%v", useExternal), func(t *testing.T) {
			reqs := byBinary(nixcompress.Requirements(useExternal))

			xzReq, ok := reqs["xz"]
			if !ok {
				t.Fatal("xz missing from Requirements; Decoder always runs it")
			}

			if !xzReq.Decode {
				t.Error("xz: Decode = false, want true — xz never decodes in process")
			}

			if xzReq.Encode != useExternal {
				t.Errorf("xz: Encode = %v, want %v — encoding is the opt-in half", xzReq.Encode, useExternal)
			}

			if xzReq.Optional {
				t.Error("xz: Optional = true, want false — there is no fallback decoder")
			}

			if xzReq.Codec != codecXz {
				t.Errorf("xz: Codec = %q, want %q", xzReq.Codec, codecXz)
			}

			if xzReq.Present != nixcompress.ExternalAvailable("xz") {
				t.Errorf("xz: Present = %v, disagrees with PATH", xzReq.Present)
			}

			bzReq, ok := reqs["bzip2"]
			if !ok {
				t.Fatal("bzip2 missing from Requirements; Encoder always runs it")
			}

			if bzReq.Decode {
				t.Error("bzip2: Decode = true, want false — compress/bzip2 decodes in process")
			}

			if !bzReq.Encode {
				t.Error("bzip2: Encode = false, want true — compress/bzip2 is decode-only")
			}

			zsReq, listed := reqs["zstd"]
			if listed != useExternal {
				t.Errorf("zstd listed = %v, want %v — it only runs when asked for", listed, useExternal)
			}

			if listed && !zsReq.Optional {
				t.Error("zstd: Optional = false, want true — both directions fall back in process")
			}
		})
	}
}

// Present has to come from the PATH the process will actually search, not from
// whatever was there when the package was built.
func TestRequirementsPresentFollowsPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	for _, req := range nixcompress.Requirements(true) {
		if req.Present {
			t.Errorf("%s: Present = true with an empty PATH", req.Binary)
		}
	}

	_, err := nixcompress.Decoder(context.Background(), bytes.NewReader(nil), codecXz, false)
	if err == nil {
		t.Error("Decoder(xz) succeeded without the binary Requirements says it needs")
	}
}

// --- External process failures ---

// fakeBinary puts a script of the given name first in PATH.
func fakeBinary(t *testing.T, name, body string) {
	t.Helper()

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755) // #nosec G306 -- must be executable
	if err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}

	t.Setenv("PATH", dir)
}

// An encoder that reports a bare exit status leaves the operator guessing, the
// same way a decoder would; both fold the child's stderr into the error.
func TestExternalEncoderReportsStderr(t *testing.T) {
	fakeBinary(t, "bzip2", "#!/bin/sh\necho 'fake bzip2 exploded' >&2\nexit 3\n")

	enc, err := nixcompress.Encoder(context.Background(), io.Discard, codecBzip2, false)
	if err != nil {
		t.Fatalf("Encoder(bzip2): %v", err)
	}

	err = enc.Close()
	if err == nil {
		t.Fatal("Encoder.Close succeeded on a child that exited 3")
	}

	if !strings.Contains(err.Error(), "fake bzip2 exploded") {
		t.Errorf("error %q does not carry the child's stderr", err)
	}

	if !strings.Contains(err.Error(), "bzip2") {
		t.Errorf("error %q does not name the binary that failed", err)
	}
}

// --- Supported ---

func TestSupported(t *testing.T) {
	for _, comp := range allCodecs {
		if !nixcompress.Supported(comp) {
			t.Errorf("Supported(%q) = false, want true", comp)
		}
	}

	// Codecs nix accepts but this package does not, plus an outright unknown.
	for _, comp := range []string{"gzip", "br", "lzma", "lzip", "lzma-weird", ""} {
		if nixcompress.Supported(comp) {
			t.Errorf("Supported(%q) = true, want false", comp)
		}
	}
}

// --- Unknown compression ---

func TestUnknownCompressionDecoder(t *testing.T) {
	var buf bytes.Buffer

	_, err := nixcompress.Decoder(context.Background(), &buf, "lzma-weird", false)
	if err == nil {
		t.Fatal("expected error for unknown compression")
	}
}

func TestUnknownCompressionEncoder(t *testing.T) {
	var buf bytes.Buffer

	_, err := nixcompress.Encoder(context.Background(), &buf, "lzma-weird", false)
	if err == nil {
		t.Fatal("expected error for unknown compression")
	}
}

// --- Benchmarks ---

func BenchmarkZstdExternal(b *testing.B) {
	if !nixcompress.ExternalAvailable("zstd") {
		b.Skip("zstd binary not in PATH")
	}

	data := makePattern(1 << 20)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for range b.N {
		_ = roundTripBench(b, data, codecZstd, true)
	}
}

func BenchmarkZstdPureGo(b *testing.B) {
	data := makePattern(1 << 20)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for range b.N {
		_ = roundTripBench(b, data, "zstd", false)
	}
}

func roundTripBench(b *testing.B, data []byte, compression string, useExternal bool) []byte {
	b.Helper()

	var buf bytes.Buffer

	enc, err := nixcompress.Encoder(context.Background(), &buf, compression, useExternal)
	if err != nil {
		b.Fatal(err)
	}

	_, err = enc.Write(data)
	if err != nil {
		b.Fatal(err)
	}

	err = enc.Close()
	if err != nil {
		b.Fatal(err)
	}

	dec, err := nixcompress.Decoder(context.Background(), &buf, compression, useExternal)
	if err != nil {
		b.Fatal(err)
	}

	got, err := io.ReadAll(dec)
	if err != nil {
		b.Fatal(err)
	}

	_ = dec.Close() // #nosec G104 -- best-effort close in benchmark helper

	return got
}

func TestDecoderCancellation(t *testing.T) {
	for _, codec := range allCodecs {
		for _, external := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/external=%t", codec, external), func(t *testing.T) {
				if (codec == codecXz || codec == codecBzip2) && !nixcompress.ExternalAvailable(codec) {
					t.Skipf("%s unavailable", codec)
				}

				var compressed bytes.Buffer

				enc, err := nixcompress.Encoder(t.Context(), &compressed, codec, external)
				require.NoError(t, err)

				_, err = enc.Write(bytes.Repeat([]byte("nar content"), 1<<17))
				require.NoError(t, err)

				err = enc.Close()
				require.NoError(t, err)

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				dec, err := nixcompress.Decoder(ctx, bytes.NewReader(compressed.Bytes()), codec, external)
				require.NoError(t, err)

				defer dec.Close()

				buf := make([]byte, 1024)

				_, err = io.ReadFull(dec, buf)
				require.NoError(t, err)

				cancel()

				_, err = dec.Read(buf)

				require.ErrorIs(t, err, context.Canceled)

				_, err = nixcompress.Decoder(ctx, bytes.NewReader(compressed.Bytes()), codec, external)

				require.ErrorIs(t, err, context.Canceled)
			})
		}
	}
}

func TestEncoderCancellation(t *testing.T) {
	for _, codec := range allCodecs {
		for _, external := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/external=%t", codec, external), func(t *testing.T) {
				if (codec == codecXz || codec == codecBzip2) && !nixcompress.ExternalAvailable(codec) {
					t.Skipf("%s unavailable", codec)
				}

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				var compressed bytes.Buffer

				enc, err := nixcompress.Encoder(ctx, &compressed, codec, external)
				require.NoError(t, err)
				_, err = enc.Write([]byte("nar content"))
				require.NoError(t, err)
				cancel()

				_, err = enc.Write(bytes.Repeat([]byte("cancelled"), 1<<17))
				require.ErrorIs(t, err, context.Canceled)

				_ = enc.Close()
				_, err = nixcompress.Encoder(ctx, &compressed, codec, external)
				require.ErrorIs(t, err, context.Canceled)
			})
		}
	}
}
