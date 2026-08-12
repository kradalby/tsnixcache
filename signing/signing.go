// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package signing parses Nix ed25519 signing keys and signs narinfo fingerprints.
package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

var (
	errSecretKeyMissingSep  = errors.New("signing: secret key missing ':' separator")
	errPublicKeyMissingSep  = errors.New("signing: public key missing ':' separator")
	errSecretKeyWrongLength = errors.New("signing: secret key wrong length")
	errPublicKeyWrongLength = errors.New("signing: public key wrong length")
	errSecretKeyPubMismatch = errors.New("signing: secret key's stored public half does not match its seed")
	errKeyNameEmpty         = errors.New("signing: key name must not be empty")
	errKeyNameInvalid       = errors.New("signing: key name must not contain ':', whitespace or control characters")
)

// validateKeyName rejects names that would corrupt the formats the name is
// pasted into. A newline injects extra lines into every narinfo we sign, since
// a signature is written as "Sig: name:base64\n"; a colon breaks parsing back
// out, as both parsers cut at the first one; and nix rejects an empty name
// outright, so such a public key can never enter trusted-public-keys and every
// client silently rejects every path.
func validateKeyName(name string) error {
	if name == "" {
		return errKeyNameEmpty
	}

	for _, r := range name {
		if r == ':' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("%w: %q", errKeyNameInvalid, name)
		}
	}

	return nil
}

// SecretKey is a named ed25519 secret key in Nix wire format.
type SecretKey struct {
	Name string
	key  ed25519.PrivateKey // 64 bytes: seed || public
}

// PublicKey is a named ed25519 public key in Nix wire format.
type PublicKey struct {
	Name string
	key  ed25519.PublicKey // 32 bytes
}

// ParseSecretKey parses a "name:base64" secret key (64-byte seed+pub).
func ParseSecretKey(s string) (*SecretKey, error) {
	name, b64, ok := strings.Cut(s, ":")
	if !ok {
		return nil, errSecretKeyMissingSep
	}

	err := validateKeyName(name)
	if err != nil {
		return nil, err
	}

	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("signing: invalid base64 in secret key: %w", err)
	}

	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: got %d, want %d", errSecretKeyWrongLength, len(raw), ed25519.PrivateKeySize)
	}

	// Reconstruct from seed so Go's ed25519 internal state is consistent.
	// Nix stores seed || pub (libsodium format); we rebuild from the seed.
	key := ed25519.NewKeyFromSeed(raw[:ed25519.SeedSize])

	// The stored public half is what the operator published; if it disagrees
	// with the seed the file is corrupt or mis-assembled, and serving under the
	// derived key would have every client reject every path with no error
	// anywhere. Both halves are public, so no constant-time compare is needed.
	if !bytes.Equal(raw[ed25519.SeedSize:], key[ed25519.SeedSize:]) {
		return nil, errSecretKeyPubMismatch
	}

	return &SecretKey{Name: name, key: key}, nil
}

// ParsePublicKey parses a "name:base64" public key (32 bytes).
func ParsePublicKey(s string) (*PublicKey, error) {
	name, b64, ok := strings.Cut(s, ":")
	if !ok {
		return nil, errPublicKeyMissingSep
	}

	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("signing: invalid base64 in public key: %w", err)
	}

	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: got %d, want %d", errPublicKeyWrongLength, len(raw), ed25519.PublicKeySize)
	}

	return &PublicKey{Name: name, key: ed25519.PublicKey(raw)}, nil
}

// GenerateKey generates a new ed25519 keypair with the given name.
func GenerateKey(name string) (*SecretKey, *PublicKey, error) {
	err := validateKeyName(name)
	if err != nil {
		return nil, nil, err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("signing: generate key: %w", err)
	}

	sk := &SecretKey{Name: name, key: priv}
	pk := &PublicKey{Name: name, key: pub}

	return sk, pk, nil
}

// Public returns the corresponding public key derived from the secret key.
func (sk *SecretKey) Public() *PublicKey {
	return &PublicKey{
		Name: sk.Name,
		key:  sk.key.Public().(ed25519.PublicKey),
	}
}

// Sign signs the fingerprint string and returns "name:base64sig".
func (sk *SecretKey) Sign(fingerprint string) string {
	sig := ed25519.Sign(sk.key, []byte(fingerprint))

	return sk.Name + ":" + base64.StdEncoding.EncodeToString(sig)
}

// Verify verifies a "name:base64" signature against the fingerprint.
// Returns false (not error) if the sig is invalid or malformed.
//
// The name must match: a narinfo usually carries signatures from several keys,
// and accepting one labelled with another key's name would report the wrong
// signer for a signature that happens to verify.
func (pk *PublicKey) Verify(fingerprint, sig string) bool {
	name, b64, ok := strings.Cut(sig, ":")
	if !ok || name != pk.Name {
		return false
	}

	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return false
	}

	if len(raw) != ed25519.SignatureSize {
		return false
	}

	return ed25519.Verify(pk.key, []byte(fingerprint), raw)
}

// String returns the "name:base64" wire format for the secret key.
func (sk *SecretKey) String() string {
	return sk.Name + ":" + base64.StdEncoding.EncodeToString(sk.key)
}

// String returns the "name:base64" wire format for the public key.
func (pk *PublicKey) String() string {
	return pk.Name + ":" + base64.StdEncoding.EncodeToString(pk.key)
}
