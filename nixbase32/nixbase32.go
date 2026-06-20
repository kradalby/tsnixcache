// Package nixbase32 implements the Nix base32 encoding.
//
// Nix uses a custom base32 alphabet and encodes bytes in reversed order
// (the last byte of the input contributes to the leftmost characters of output).
package nixbase32

import "fmt"

const alphabet = "0123456789abcdfghijklmnpqrsvwxyz"

// encTable is a [32]byte version of alphabet for faster indexed access.
var encTable = [32]byte([]byte(alphabet))

// decTable maps byte value → 5-bit nix-base32 digit; 0xff = invalid.
// Computed once at init to avoid rebuilding on every DecodeString call.
var decTable [256]byte

func init() {
	for i := range decTable {
		decTable[i] = 0xff
	}
	for i := range alphabet {
		decTable[alphabet[i]] = byte(i)
	}
}

// EncodeToString encodes b using the Nix base32 encoding.
func EncodeToString(b []byte) string {
	encodedLen := (len(b)*8 + 4) / 5
	out := make([]byte, encodedLen)
	n := len(b)
	for i := range encodedLen {
		bitPos := i * 5
		bPos := bitPos / 8
		bitOff := uint(bitPos % 8)

		low := b[n-1-bPos]
		var high byte
		if bPos+1 < n {
			high = b[n-2-bPos]
		}

		out[i] = encTable[(uint(high)<<8|uint(low))>>bitOff&0x1f]
	}
	return string(out)
}

// DecodeString decodes a Nix base32 encoded string.
func DecodeString(s string) ([]byte, error) {
	outLen := len(s) * 5 / 8
	out := make([]byte, outLen)

	for i := range len(s) {
		ch := s[i]
		val := decTable[ch]
		if val == 0xff {
			return nil, fmt.Errorf("nixbase32: invalid character %q at position %d", ch, i)
		}

		bitPos := i * 5
		bPos := bitPos / 8
		bitOff := uint(bitPos % 8)

		lo := outLen - 1 - bPos
		out[lo] |= byte(uint(val) << bitOff)
		if bitOff+5 > 8 && lo > 0 {
			out[lo-1] |= byte(uint(val) >> (8 - bitOff))
		}
	}
	return out, nil
}
