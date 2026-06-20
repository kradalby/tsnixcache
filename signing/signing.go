package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

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
		return nil, fmt.Errorf("signing: secret key missing ':' separator")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("signing: invalid base64 in secret key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing: secret key is %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}
	// Reconstruct from seed so Go's ed25519 internal state is consistent.
	// Nix stores seed || pub (libsodium format); we rebuild from the seed.
	key := ed25519.NewKeyFromSeed(raw[:ed25519.SeedSize])
	return &SecretKey{Name: name, key: key}, nil
}

// ParsePublicKey parses a "name:base64" public key (32 bytes).
func ParsePublicKey(s string) (*PublicKey, error) {
	name, b64, ok := strings.Cut(s, ":")
	if !ok {
		return nil, fmt.Errorf("signing: public key missing ':' separator")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("signing: invalid base64 in public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("signing: public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return &PublicKey{Name: name, key: ed25519.PublicKey(raw)}, nil
}

// GenerateKey generates a new ed25519 keypair with the given name.
func GenerateKey(name string) (*SecretKey, *PublicKey, error) {
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
func (pk *PublicKey) Verify(fingerprint, sig string) bool {
	_, b64, ok := strings.Cut(sig, ":")
	if !ok {
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
