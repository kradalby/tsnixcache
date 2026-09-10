// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package signing

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/kradalby/tsnixcache/narinfo"
)

// A real keypair, produced by `nix key generate-secret --key-name
// test.tsnixcache.example-1` and `nix key convert-secret-to-public`. Pinning
// nix's own output is the only way to know this package agrees with nix about
// the wire format; a made-up 64-byte blob would parse and sign happily while
// deriving a public key nix never issued.
const (
	testSecretKeyStr = "test.tsnixcache.example-1:B6/h63DPvuRF3J3d6Db74TCS1M4QlEFVGjlHhDQNP5JW+48zmSQOBodjiy+B6eDJM2aO9ZrNtG7wPVU9HrEbdQ==" // #nosec G101 -- throwaway fixture key, never used to sign anything real
	testPublicKeyStr = "test.tsnixcache.example-1:VvuPM5kkDgaHY4svgengyTNmjvWazbRu8D1VPR6xG3U="
	testKeyName      = "test.tsnixcache.example-1"
	testFingerprint  = "1;/nix/store/s66mzxpvicwklp6nvshjmctarrqdu3s7-libssh2-1.10.0;sha256:1l29f8r5z6bxi2ckjgajvy7kqhxqvq8j5k0m0s9f7dpzrm3qs7z5;294664;/nix/store/0jqd0rlxzra1rs38rdgwg20128y0f25r-libc-2.34,/nix/store/s66mzxpvicwklp6nvshjmctarrqdu3s7-libssh2-1.10.0"
)

// A real path from cache.nixos.org and the metadata nix recorded for it, taken
// verbatim from `nix path-info --json --sigs`. It carries two signatures over
// the same fingerprint: the upstream cache.nixos.org-1 one, and one nix itself
// produced with testSecretKeyStr via `nix store sign`. It references itself,
// which nix keeps in the fingerprint.
const (
	nixosCachePublicKey = "cache.nixos.org-1:6NCHdD59X431o0gWypbMrAURkbJ16ZPMQFGspcDShjY="
	nixosCacheSig       = "cache.nixos.org-1:DItxBWI+ETNDSv9B/J7CcSNfV5jXWkp6SO82IzgN3KiYclCXqvQY5rn6SlJImcTGearsEuHlLFKbE19GuQ33Cw=="
	nixSignedSig        = "test.tsnixcache.example-1:K9+m9D346ygFUgbvPL/X9clYuJWfwQr+kJB4Oj01W24o4BI/lzVceknW6uqPwbzZ3xVP1qX5rY0rofYEc7GuCw=="
	nixStorePath        = "/nix/store/6i6xl6bmcpxqd51m8nlva40d5c1bhndx-hello-2.12.3"
	nixNarHash          = "sha256:1pdbnpnmhllk4256s3ka335463dkb9pbwbmxzvpqg83d404bkgx3"
	nixNarSize          = 279624
)

// Deliberately unsorted, so Fingerprint's sorting is exercised.
var nixReferences = []string{
	"/nix/store/avld9cdn23zab2ssl30h2r6444rqh6ms-glibc-2.42-67",
	"/nix/store/6i6xl6bmcpxqd51m8nlva40d5c1bhndx-hello-2.12.3", // self-reference
}

// nixFingerprint is what nix signed, per its own definition:
// 1;storePath;narHash;narSize;comma-joined-sorted-references.
const nixFingerprint = "1;" + nixStorePath + ";" + nixNarHash + ";279624;" +
	"/nix/store/6i6xl6bmcpxqd51m8nlva40d5c1bhndx-hello-2.12.3," +
	"/nix/store/avld9cdn23zab2ssl30h2r6444rqh6ms-glibc-2.42-67"

func TestParseSecretKeyValid(t *testing.T) {
	sk, err := ParseSecretKey(testSecretKeyStr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sk.Name != testKeyName {
		t.Errorf("Name = %q, want %q", sk.Name, testKeyName)
	}

	if len(sk.key) != 64 {
		t.Errorf("key length = %d, want 64", len(sk.key))
	}
}

func TestParseSecretKeyInvalid(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"no colon", "nocachename"},
		{"bad base64", "name:not!valid!base64==="},
		{"wrong length 32 bytes", "name:" + base64.StdEncoding.EncodeToString(make([]byte, 32))},
		{"wrong length 16 bytes", "name:" + base64.StdEncoding.EncodeToString(make([]byte, 16))},
		{"empty name", strings.TrimPrefix(testSecretKeyStr, testKeyName)},
		{"name with a newline", "cache\nevil" + strings.TrimPrefix(testSecretKeyStr, testKeyName)},
		{"all-zero key: stored public half is not the seed's", "name:" + base64.StdEncoding.EncodeToString(make([]byte, 64))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSecretKey(tc.input)
			if err == nil {
				t.Errorf("expected error for input %q, got nil", tc.input)
			}
		})
	}
}

// TestParseSecretKeyRejectsCorruptedPublicHalf flips one byte of the stored
// public half. Nothing downstream would notice: signing only uses the seed, so
// the server would happily serve signatures under a key nobody published.
func TestParseSecretKeyRejectsCorruptedPublicHalf(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(testSecretKeyStr, testKeyName+":"))
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	raw[len(raw)-1] ^= 0x01

	_, err = ParseSecretKey(testKeyName + ":" + base64.StdEncoding.EncodeToString(raw))
	if !errors.Is(err, errSecretKeyPubMismatch) {
		t.Errorf("ParseSecretKey = %v, want errSecretKeyPubMismatch", err)
	}
}

// TestGenerateKeyRejectsBadNames covers the names that break the formats the
// name ends up in: a newline forges narinfo lines, a colon makes the key file
// unparseable, and an empty name is one nix will not load.
func TestGenerateKeyRejectsBadNames(t *testing.T) {
	names := []string{
		"",
		"cache:evil",
		"good\nStorePath: /nix/store/evil",
		"cache evil",
		"cache\tevil",
		"cache\x00evil",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			_, _, err := GenerateKey(name)
			if err == nil {
				t.Errorf("GenerateKey(%q) succeeded, want an error", name)
			}
		})
	}
}

func TestParsePublicKeyValid(t *testing.T) {
	sk, err := ParseSecretKey(testSecretKeyStr)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	pkStr := sk.Public().String()

	pk, err := ParsePublicKey(pkStr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if pk.Name != testKeyName {
		t.Errorf("Name = %q, want %q", pk.Name, testKeyName)
	}

	if len(pk.key) != 32 {
		t.Errorf("key length = %d, want 32", len(pk.key))
	}
}

func TestParsePublicKeyInvalid(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"no colon", "nocachename"},
		{"wrong length 64 bytes", "name:" + base64.StdEncoding.EncodeToString(make([]byte, 64))},
		{"wrong length 16 bytes", "name:" + base64.StdEncoding.EncodeToString(make([]byte, 16))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePublicKey(tc.input)
			if err == nil {
				t.Errorf("expected error for input %q, got nil", tc.input)
			}
		})
	}
}

func TestGenerateKeyRoundTrip(t *testing.T) {
	sk, pk, err := GenerateKey("test-cache-1")
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	fp := "1;/nix/store/abc;sha256:xyz;1234;/nix/store/dep"

	sig := sk.Sign(fp)
	if !pk.Verify(fp, sig) {
		t.Error("Verify returned false for a freshly signed fingerprint")
	}
}

func TestSignVerify(t *testing.T) {
	sk, err := ParseSecretKey(testSecretKeyStr)
	if err != nil {
		t.Fatalf("ParseSecretKey: %v", err)
	}

	pk := sk.Public()

	sig := sk.Sign(testFingerprint)
	if !pk.Verify(testFingerprint, sig) {
		t.Errorf("Verify returned false for sig=%q", sig)
	}
}

// TestVerifyRejectsWrongKey gives both keys the same name, so only the key
// material can make it fail.
func TestVerifyRejectsWrongKey(t *testing.T) {
	skA, _, err := GenerateKey("cache-1")
	if err != nil {
		t.Fatalf("GenerateKey A: %v", err)
	}

	_, pkB, err := GenerateKey("cache-1")
	if err != nil {
		t.Fatalf("GenerateKey B: %v", err)
	}

	fp := "some fingerprint"

	sig := skA.Sign(fp)
	if pkB.Verify(fp, sig) {
		t.Error("Verify returned true for wrong key")
	}
}

// TestVerifyRejectsWrongName pins that a signature is only accepted under the
// name it is labelled with, so a caller cannot be told the wrong key signed it.
func TestVerifyRejectsWrongName(t *testing.T) {
	sk, err := ParseSecretKey(testSecretKeyStr)
	if err != nil {
		t.Fatalf("ParseSecretKey: %v", err)
	}

	sig := sk.Sign(testFingerprint)
	relabelled := "other-cache-1:" + strings.TrimPrefix(sig, sk.Name+":")

	if sk.Public().Verify(testFingerprint, relabelled) {
		t.Error("Verify accepted a signature labelled with another key's name")
	}
}

func TestVerifyRejectsModifiedFingerprint(t *testing.T) {
	sk, err := ParseSecretKey(testSecretKeyStr)
	if err != nil {
		t.Fatalf("ParseSecretKey: %v", err)
	}

	pk := sk.Public()

	sig := sk.Sign("fingerprint-original")
	if pk.Verify("fingerprint-modified", sig) {
		t.Error("Verify returned true for modified fingerprint")
	}
}

func TestVerifyRejectsTruncatedSig(t *testing.T) {
	sk, err := ParseSecretKey(testSecretKeyStr)
	if err != nil {
		t.Fatalf("ParseSecretKey: %v", err)
	}

	pk := sk.Public()

	truncated := sk.Name + ":" + base64.StdEncoding.EncodeToString([]byte("tooshort"))
	if pk.Verify(testFingerprint, truncated) {
		t.Error("Verify returned true for truncated sig")
	}
}

func TestSecretKeyStringRoundTrip(t *testing.T) {
	sk, err := ParseSecretKey(testSecretKeyStr)
	if err != nil {
		t.Fatalf("ParseSecretKey: %v", err)
	}

	sk2, err := ParseSecretKey(sk.String())
	if err != nil {
		t.Fatalf("ParseSecretKey round-trip: %v", err)
	}

	if sk2.Name != sk.Name {
		t.Errorf("Name mismatch: %q != %q", sk2.Name, sk.Name)
	}

	if string(sk2.key) != string(sk.key) {
		t.Error("key bytes differ after round-trip")
	}
}

func TestSigFormat(t *testing.T) {
	sk, err := ParseSecretKey(testSecretKeyStr)
	if err != nil {
		t.Fatalf("ParseSecretKey: %v", err)
	}

	sig := sk.Sign(testFingerprint)

	prefix := sk.Name + ":"
	if !strings.HasPrefix(sig, prefix) {
		t.Errorf("sig %q does not start with %q", sig, prefix)
	}

	b64part := strings.TrimPrefix(sig, prefix)

	raw, err := base64.StdEncoding.DecodeString(b64part)
	if err != nil {
		t.Errorf("sig base64 part is invalid: %v", err)
	}

	if len(raw) != 64 {
		t.Errorf("sig raw length = %d, want 64", len(raw))
	}
}

// TestPublicKeyMatchesNixDerived checks the public key we derive from a nix
// secret key is byte-for-byte the one `nix key convert-secret-to-public`
// produced. Comparing against the tail of the same key we just reconstructed
// would prove nothing: ParseSecretKey rebuilds the key from its seed, so the
// tail is derived by the very code under test.
func TestPublicKeyMatchesNixDerived(t *testing.T) {
	sk, err := ParseSecretKey(testSecretKeyStr)
	if err != nil {
		t.Fatalf("ParseSecretKey: %v", err)
	}

	got := sk.Public().String()
	if got != testPublicKeyStr {
		t.Errorf("Public() = %q, want %q", got, testPublicKeyStr)
	}
}

// TestFingerprintMatchesNix checks narinfo.Fingerprint reproduces nix's
// definition — including the self-reference, which nix keeps.
func TestFingerprintMatchesNix(t *testing.T) {
	ni := &narinfo.NarInfo{
		StorePath:  nixStorePath,
		NarHash:    nixNarHash,
		NarSize:    nixNarSize,
		References: nixReferences,
	}

	got := ni.Fingerprint()
	if got != nixFingerprint {
		t.Errorf("Fingerprint:\n got: %s\nwant: %s", got, nixFingerprint)
	}
}

// TestVerifyNixProducedSignatures verifies signatures nix itself produced over
// a fingerprint this package builds from narinfo metadata. It pins down the
// whole path at once: if the fingerprint's field order, separators, reference
// sorting or self-reference handling drift from nix's definition, or if key
// parsing mangles the seed, these stop verifying. The cache.nixos.org-1 case
// is the pass-through path, where a live bug was found.
func TestVerifyNixProducedSignatures(t *testing.T) {
	ni := &narinfo.NarInfo{
		StorePath:  nixStorePath,
		NarHash:    nixNarHash,
		NarSize:    nixNarSize,
		References: nixReferences,
	}
	fp := ni.Fingerprint()

	tests := []struct {
		name   string
		pubKey string
		sig    string
	}{
		{"upstream cache.nixos.org-1", nixosCachePublicKey, nixosCacheSig},
		{"nix store sign with our test key", testPublicKeyStr, nixSignedSig},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pk, err := ParsePublicKey(tc.pubKey)
			if err != nil {
				t.Fatalf("ParsePublicKey: %v", err)
			}

			if !pk.Verify(fp, tc.sig) {
				t.Error("nix-produced signature did not verify against our fingerprint")
			}

			// A single altered byte anywhere in the fingerprint must break it.
			if pk.Verify(fp+" ", tc.sig) {
				t.Error("Verify accepted a modified fingerprint")
			}
		})
	}
}

// TestSignMatchesNix checks our Sign reproduces, byte for byte, the signature
// `nix store sign` wrote with the same key. ed25519 is deterministic, so any
// difference means we are signing different bytes or using a different key.
func TestSignMatchesNix(t *testing.T) {
	sk, err := ParseSecretKey(testSecretKeyStr)
	if err != nil {
		t.Fatalf("ParseSecretKey: %v", err)
	}

	got := sk.Sign(nixFingerprint)
	if got != nixSignedSig {
		t.Errorf("Sign:\n got: %s\nwant: %s", got, nixSignedSig)
	}
}
