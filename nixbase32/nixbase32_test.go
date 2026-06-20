package nixbase32

import (
	"bytes"
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
	// {0xff} encodes to "z7": first 5 bits = 11111 = 31 = 'z', next 3 bits = 111 padded = 7 = '7'.
	got := EncodeToString([]byte{0xff})
	if got != "z7" {
		t.Errorf("EncodeToString({0xff}) = %q, want %q", got, "z7")
	}
	// Decode back
	dec, err := DecodeString("z7")
	if err != nil {
		t.Fatalf("DecodeString(%q): %v", "z7", err)
	}
	if len(dec) != 1 || dec[0] != 0xff {
		t.Errorf("DecodeString(%q) = %x, want ff", "z7", dec)
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
