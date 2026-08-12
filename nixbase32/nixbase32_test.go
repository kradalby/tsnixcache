// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package nixbase32

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	vectors := [][]byte{
		{0x00},
		{0xff},
		{0x00, 0x00, 0x00, 0x00},
		{0x01, 0x02, 0x03, 0x04, 0x05},
		// 20 random bytes
		{0xe3, 0xb0, 0xc4, 0x42, 0x98, 0xfc, 0x1c, 0x14, 0x9a, 0xfb, 0xf4, 0xc8, 0x99, 0x6f, 0xb9, 0x24, 0x27, 0xae, 0x41, 0xe4},
		// 32 random bytes
		{0xba, 0x78, 0x16, 0xbf, 0x8f, 0x01, 0xcf, 0xea, 0x41, 0x41, 0x40, 0x99, 0x32, 0x08, 0xe8, 0xa8, 0x04, 0x7e, 0x3d, 0x6a, 0x7a, 0x11, 0x44, 0x32, 0x0f, 0xa3, 0x0e, 0x11, 0x58, 0x82, 0x1b, 0x45},
	}
	for _, input := range vectors {
		encoded := EncodeToString(input)

		decoded, err := DecodeString(encoded)
		if err != nil {
			t.Errorf("DecodeString(%q) error: %v", encoded, err)

			continue
		}

		if !bytes.Equal(decoded, input) {
			t.Errorf("round-trip failed for %x: got %x", input, decoded)
		}
	}
}

func TestDecodeEncodeRoundTrip(t *testing.T) {
	// Round-trip property: EncodeToString(DecodeString(EncodeToString(b))) == EncodeToString(b)
	vectors := [][]byte{
		{0x00},
		{0xff},
		{0x00, 0x00, 0x00, 0x00},
		{0x01, 0x02, 0x03, 0x04, 0x05},
		{0xe3, 0xb0, 0xc4, 0x42, 0x98, 0xfc, 0x1c, 0x14, 0x9a, 0xfb, 0xf4, 0xc8, 0x99, 0x6f, 0xb9, 0x24, 0x27, 0xae, 0x41, 0xe4},
	}
	for _, input := range vectors {
		enc1 := EncodeToString(input)

		decoded, err := DecodeString(enc1)
		if err != nil {
			t.Errorf("DecodeString(%q) error: %v", enc1, err)

			continue
		}

		enc2 := EncodeToString(decoded)
		if enc1 != enc2 {
			t.Errorf("double encode mismatch for %x: %q != %q", input, enc1, enc2)
		}
	}
}

func TestLengthProperty(t *testing.T) {
	vectors := [][]byte{
		{0x00},
		{0xff},
		{0x00, 0x00, 0x00, 0x00},
		{0x01, 0x02, 0x03, 0x04, 0x05},
		make([]byte, 20),
		make([]byte, 32),
	}
	for _, input := range vectors {
		expected := (len(input)*8 + 4) / 5

		encoded := EncodeToString(input)
		if len(encoded) != expected {
			t.Errorf("length mismatch for %d-byte input: expected %d, got %d", len(input), expected, len(encoded))
		}
	}
}

func TestAllZero(t *testing.T) {
	input := make([]byte, 20)
	expected := "00000000000000000000000000000000"

	got := EncodeToString(input)
	if got != expected {
		t.Errorf("all-zero: expected %q, got %q", expected, got)
	}
}

func TestInvalidChar(t *testing.T) {
	_, err := DecodeString("!@#")
	if err == nil {
		t.Error("expected error for invalid char '!@#', got nil")
	}
}

func TestInvalidAlphabetChars(t *testing.T) {
	// 'e', 'o', 'u', 't' are not in the nix base32 alphabet
	for _, ch := range []string{"e", "o", "u", "t"} {
		_, err := DecodeString(ch + "0000000")
		if err == nil {
			t.Errorf("expected error for char %q (not in alphabet), got nil", ch)
		}
	}
}

func TestKnownVector(t *testing.T) {
	// Nix encodes from the most-significant bit group first.
	// For {0xff}: bits [5:8)+padding = 00111 = 7 = '7' first, then bits [0:5) = 11111 = 31 = 'z'.
	got := EncodeToString([]byte{0xff})
	if got != "7z" {
		t.Errorf("EncodeToString({0xff}) = %q, want %q", got, "7z")
	}

	// Decode back
	dec, err := DecodeString("7z")
	if err != nil {
		t.Fatalf("DecodeString(%q): %v", "7z", err)
	}

	if len(dec) != 1 || dec[0] != 0xff {
		t.Errorf("DecodeString(%q) = %x, want ff", "7z", dec)
	}
}

func TestAllMax(t *testing.T) {
	// 20 bytes of 0xff should round-trip correctly.
	input := make([]byte, 20)
	for i := range input {
		input[i] = 0xff
	}

	encoded := EncodeToString(input)
	if len(encoded) != 32 {
		t.Errorf("encoded length = %d, want 32", len(encoded))
	}

	decoded, err := DecodeString(encoded)
	if err != nil {
		t.Fatalf("DecodeString: %v", err)
	}

	if !bytes.Equal(decoded, input) {
		t.Errorf("round-trip mismatch: got %x, want %x", decoded, input)
	}
}

func TestDecodeShortAndOddLengths(t *testing.T) {
	// Lengths where the leading character's byte position lands at or past the
	// end of the decoded output. These used to panic (1 character decodes to
	// zero bytes) or silently drop the surplus bits.
	tests := []struct {
		name    string
		input   string
		want    []byte
		wantErr error
	}{
		{"empty", "", []byte{}, nil},
		{"one zero char", "0", []byte{}, nil},
		{"one nonzero char", "z", nil, ErrNonCanonical},
		{"one nonzero char low", "1", nil, ErrNonCanonical},
		{"two chars", "7z", []byte{0xff}, nil},
		{"three chars canonical", "07z", []byte{0xff}, nil},
		{"three chars surplus in leading char", "1zz", nil, ErrNonCanonical},
		{"three chars surplus in carry", "0zz", nil, ErrNonCanonical},
		{"four chars canonical", "1zzz", []byte{0xff, 0xff}, nil},
		{"four chars low bits only", "0zzz", []byte{0xff, 0x7f}, nil},
		{"nine chars zero", "000000000", []byte{0, 0, 0, 0, 0}, nil},
		{"nine chars surplus", "z00000000", nil, ErrNonCanonical},
		{"invalid char beats canonicity", "e", nil, ErrInvalidChar},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeString(tc.input)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("DecodeString(%q) err = %v, want %v", tc.input, err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("DecodeString(%q): %v", tc.input, err)
			}

			if !bytes.Equal(got, tc.want) {
				t.Errorf("DecodeString(%q) = %x, want %x", tc.input, got, tc.want)
			}
		})
	}
}

func TestDecodeRejectsNonCanonicalHash(t *testing.T) {
	// A 52-character sha256 encoding carries 260 bits but only 32 bytes (256
	// bits) of payload. The leading character's lowest bit is real payload; its
	// top four are surplus and must be zero, else 16 distinct strings decode to
	// the same hash.
	canonical := EncodeToString(make([]byte, 32))
	if len(canonical) != 52 {
		t.Fatalf("test author error: encoded length %d", len(canonical))
	}

	// '1' flips the genuine top bit of the hash: still canonical, different hash.
	topBitSet, err := DecodeString("1" + canonical[1:])
	if err != nil {
		t.Fatalf("DecodeString of canonical top-bit encoding: %v", err)
	}

	if topBitSet[31] != 0x80 {
		t.Errorf("top-bit decode = %x, want last byte 0x80", topBitSet)
	}

	for _, lead := range alphabet[2:] {
		mutated := string(lead) + canonical[1:]

		_, err := DecodeString(mutated)
		if !errors.Is(err, ErrNonCanonical) {
			t.Errorf("DecodeString(%q) err = %v, want ErrNonCanonical", mutated, err)
		}
	}
}

func TestValidHashPart(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"real hash part", "00qn3vc7r4m32c072kjnrbxd86w9slzj", true},
		{"all zeroes", strings.Repeat("0", 32), true},
		{"too short", strings.Repeat("0", 31), false},
		{"too long", strings.Repeat("0", 33), false},
		{"empty", "", false},
		{"letter e not in alphabet", "e0qn3vc7r4m32c072kjnrbxd86w9slzj", false},
		{"sql wildcard", "%0qn3vc7r4m32c072kjnrbxd86w9slz%", false},
		{"path traversal", "../../etc/passwd0000000000000000", false},
		{"uppercase", "00QN3VC7R4M32C072KJNRBXD86W9SLZJ", false},
		{"nul byte", "00qn3vc7r4m32c072kjnrbxd86w9slz\x00", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidHashPart(tc.input)
			if got != tc.want {
				t.Errorf("ValidHashPart(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// FuzzDecodeString asserts DecodeString never panics on arbitrary input and
// that anything it accepts re-encodes to itself.
func FuzzDecodeString(f *testing.F) {
	for _, seed := range []string{"", "0", "z", "7z", "zzz", "000000000", "00qn3vc7r4m32c072kjnrbxd86w9slzj", "!@#"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		got, err := DecodeString(s)
		if err != nil {
			return
		}

		// Only strings of an encoding-produced length can round-trip; "0"
		// legitimately decodes to no bytes, which re-encodes to "".
		if len(s) != (len(got)*8+4)/5 {
			return
		}

		if reencoded := EncodeToString(got); reencoded != s {
			t.Errorf("DecodeString(%q) re-encodes to %q", s, reencoded)
		}
	})
}

func TestKnown32ZeroBytes(t *testing.T) {
	// 32 zero bytes → 52 '0' characters.
	input := make([]byte, 32)
	want := "0000000000000000000000000000000000000000000000000000"

	if len(want) != 52 {
		t.Fatalf("test author error: want length %d", len(want))
	}

	got := EncodeToString(input)
	if got != want {
		t.Errorf("EncodeToString(32×0x00) = %q, want %q", got, want)
	}
}
