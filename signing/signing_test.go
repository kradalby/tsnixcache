package signing

import (
	"encoding/base64"
	"strings"
	"testing"
)

const (
	testSecretKeyStr = "cache.example.org-1:ZJui+kG6vPCSRD4+p1P4DyUVlASmp/zsaeN84PTFW28tj2/cZpP3VFkUTuHhwuE8TMGEXdORJEaSVONGHNJAZQ=="
	testKeyName      = "cache.example.org-1"
	testFingerprint  = "1;/nix/store/s66mzxpvicwklp6nvshjmctarrqdu3s7-libssh2-1.10.0;sha256:1l29f8r5z6bxi2ckjgajvy7kqhxqvq8j5k0m0s9f7dpzrm3qs7z5;294664;/nix/store/0jqd0rlxzra1rs38rdgwg20128y0f25r-libc-2.34,/nix/store/s66mzxpvicwklp6nvshjmctarrqdu3s7-libssh2-1.10.0"
)

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

func TestVerifyRejectsWrongKey(t *testing.T) {
	skA, _, err := GenerateKey("keyA")
	if err != nil {
		t.Fatalf("GenerateKey A: %v", err)
	}
	_, pkB, err := GenerateKey("keyB")
	if err != nil {
		t.Fatalf("GenerateKey B: %v", err)
	}
	fp := "some fingerprint"
	sig := skA.Sign(fp)
	if pkB.Verify(fp, sig) {
		t.Error("Verify returned true for wrong key")
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

func TestPublicKeyMatchesSecretKeyLastBytes(t *testing.T) {
	sk, err := ParseSecretKey(testSecretKeyStr)
	if err != nil {
		t.Fatalf("ParseSecretKey: %v", err)
	}
	pk := sk.Public()
	// The last 32 bytes of the ed25519 private key are the public key.
	wantPub := sk.key[32:]
	for i, b := range pk.key {
		if b != wantPub[i] {
			t.Errorf("public key byte %d: got %x, want %x", i, b, wantPub[i])
		}
	}
}
