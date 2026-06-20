package nixbase32

import "testing"

var sink string
var sinkBytes []byte

func BenchmarkEncodeToString_20(b *testing.B) {
	input := make([]byte, 20)
	for i := range input {
		input[i] = byte(i * 13)
	}
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	for b.Loop() {
		sink = EncodeToString(input)
	}
}

func BenchmarkEncodeToString_32(b *testing.B) {
	input := make([]byte, 32)
	for i := range input {
		input[i] = byte(i * 7)
	}
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	for b.Loop() {
		sink = EncodeToString(input)
	}
}

func BenchmarkEncodeToString_100(b *testing.B) {
	input := make([]byte, 100)
	for i := range input {
		input[i] = byte(i)
	}
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	for b.Loop() {
		sink = EncodeToString(input)
	}
}

func BenchmarkDecodeString_20(b *testing.B) {
	input := make([]byte, 20)
	for i := range input {
		input[i] = byte(i * 13)
	}
	encoded := EncodeToString(input)
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	for b.Loop() {
		sinkBytes, _ = DecodeString(encoded)
	}
}

func BenchmarkDecodeString_32(b *testing.B) {
	input := make([]byte, 32)
	for i := range input {
		input[i] = byte(i * 7)
	}
	encoded := EncodeToString(input)
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	for b.Loop() {
		sinkBytes, _ = DecodeString(encoded)
	}
}

func BenchmarkDecodeString_100(b *testing.B) {
	input := make([]byte, 100)
	for i := range input {
		input[i] = byte(i)
	}
	encoded := EncodeToString(input)
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	for b.Loop() {
		sinkBytes, _ = DecodeString(encoded)
	}
}
