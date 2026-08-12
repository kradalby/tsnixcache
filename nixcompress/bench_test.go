// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package nixcompress_test

import (
	"bytes"
	"context"
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
		enc, err := nixcompress.Encoder(context.Background(), io.Discard, compression, useExternal)
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
	}
}

// compressOnce returns the compressed form of data using the given codec.
func compressOnce(b *testing.B, data []byte, compression string, useExternal bool) []byte {
	b.Helper()

	var buf bytes.Buffer

	enc, err := nixcompress.Encoder(context.Background(), &buf, compression, useExternal)
	if err != nil {
		b.Fatalf("Encoder(%q, useExternal=%v): %v", compression, useExternal, err)
	}

	_, err = enc.Write(data)
	if err != nil {
		b.Fatal(err)
	}

	err = enc.Close()
	if err != nil {
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

		dec, err := nixcompress.Decoder(context.Background(), r, compression, useExternal)
		if err != nil {
			b.Fatal(err)
		}

		_, err = io.Copy(io.Discard, dec)
		if err != nil {
			b.Fatal(err)
		}

		err = dec.Close()
		if err != nil {
			b.Fatal(err)
		}
	}
}

// --- Zstd encode: PureGo vs External ---

func BenchmarkEncode_Zstd_PureGo_1MB(b *testing.B) {
	data := makePattern(1 << 20)
	encodeBench(b, data, codecZstd, false)
}

func BenchmarkEncode_Zstd_External_1MB(b *testing.B) {
	if !nixcompress.ExternalAvailable("zstd") {
		b.Skip("zstd binary not in PATH")
	}

	data := makePattern(1 << 20)
	encodeBench(b, data, codecZstd, true)
}

// --- Xz decode ---

// There is only one xz decode path: Decoder always runs the binary, whatever
// useExternal says.
func BenchmarkDecode_Xz_1MB(b *testing.B) {
	if !nixcompress.ExternalAvailable("xz") {
		b.Skip("xz binary not in PATH")
	}

	raw := makePattern(1 << 20)
	compressed := compressOnce(b, raw, codecXz, false)
	decodeBench(b, compressed, codecXz, false)
}
