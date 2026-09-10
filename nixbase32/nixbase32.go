// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package nixbase32 implements the Nix base32 encoding.
//
// Nix uses a custom base32 alphabet and encodes bytes in reversed order
// (the last byte of the input contributes to the leftmost characters of output).
package nixbase32

import (
	"errors"
	"fmt"
)

const alphabet = "0123456789abcdfghijklmnpqrsvwxyz"

// encTable is a [32]byte version of alphabet for faster indexed access.
var encTable = [32]byte([]byte(alphabet))

// decTable maps byte value → 5-bit nix-base32 digit; 0xff = invalid.
// Computed once at init to avoid rebuilding on every DecodeString call.
var decTable [256]byte

// HashPartLen is the length of the nix-base32 hash part of a store path
// basename (a 20-byte truncated hash, 32 characters).
const HashPartLen = 32

var (
	// ErrInvalidChar is returned when a character not in the Nix base32 alphabet is encountered.
	ErrInvalidChar = errors.New("nixbase32: invalid character")
	// ErrNonCanonical rejects nonzero bits beyond the decoded output.
	ErrNonCanonical = errors.New("nixbase32: non-canonical encoding, surplus bits set")
)

func init() {
	for i := range decTable {
		decTable[i] = 0xff
	}

	for i := range alphabet {
		decTable[alphabet[i]] = byte(i) // #nosec G115 -- i is always 0..31, safe to convert
	}
}

// EncodeToString encodes b using the Nix base32 encoding.
// Nix encodes bits from the most-significant end first: output[0] holds
// the highest bit group, output[len-1] holds the lowest.
func EncodeToString(b []byte) string {
	encodedLen := (len(b)*8 + 4) / 5
	out := make([]byte, encodedLen)
	n := len(b)

	for i := range encodedLen {
		// output[0] = highest bits, so invert index for bit position.
		bitPos := (encodedLen - 1 - i) * 5
		bPos := bitPos / 8
		bitOff := uint(bitPos % 8)

		low := b[bPos]

		var high byte
		if bPos+1 < n {
			high = b[bPos+1]
		}

		out[i] = encTable[(uint(high)<<8|uint(low))>>bitOff&0x1f]
	}

	return string(out)
}

// DecodeString decodes Nix base32, rejecting nonzero bits beyond the output.
// Leading-zero length aliases are accepted; EncodeToString canonicalizes them.
func DecodeString(s string) ([]byte, error) {
	outLen := len(s) * 5 / 8
	out := make([]byte, outLen)

	for i := range len(s) {
		ch := s[i]

		val := decTable[ch]
		if val == 0xff {
			return nil, fmt.Errorf("%w %q at position %d", ErrInvalidChar, ch, i)
		}

		// Inverse of EncodeToString: character i maps to bit position (len(s)-1-i)*5.
		bitPos := (len(s) - 1 - i) * 5
		bPos := bitPos / 8
		bitOff := uint(bitPos % 8)

		// The leading character can sit wholly past the end of the output
		// (a 1-character input decodes to no bytes at all).
		if bPos >= outLen {
			if val != 0 {
				return nil, fmt.Errorf("%w at position %d", ErrNonCanonical, i)
			}

			continue
		}

		out[bPos] |= val << bitOff

		carry := val >> (8 - bitOff) // 0 when bitOff is 0: shifting a byte by 8 yields 0
		if carry != 0 {
			if bPos+1 >= outLen {
				return nil, fmt.Errorf("%w at position %d", ErrNonCanonical, i)
			}

			out[bPos+1] |= carry
		}
	}

	return out, nil
}

// ValidHashPart reports whether s is a well-formed store path hash part: 32
// characters, all from the Nix base32 alphabet. At that length the encoding
// covers exactly 20 bytes, so every such string is canonical and there is
// nothing further to check.
func ValidHashPart(s string) bool {
	if len(s) != HashPartLen {
		return false
	}

	for i := range len(s) {
		if decTable[s[i]] == 0xff {
			return false
		}
	}

	return true
}
