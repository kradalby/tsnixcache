package narinfo

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const minimalNarinfo = `StorePath: /nix/store/abc123def456ghi789jklmn-hello-2.12.1
URL: nar/1abc123.nar
Compression: none
FileHash: sha256:1abc123def456ghi789jklmnoabcdefghijklmnopqrstuvwxyz012345678
FileSize: 12345
NarHash: sha256:1abc123def456ghi789jklmnoabcdefghijklmnopqrstuvwxyz012345678
NarSize: 45678
References:
`

const fullNarinfo = `StorePath: /nix/store/abc123def456ghi789jklmn-hello-2.12.1
URL: nar/1abc123.nar
Compression: xz
FileHash: sha256:1abc123def456ghi789jklmnoabcdefghijklmnopqrstuvwxyz012345678
FileSize: 12345
NarHash: sha256:1nar123def456ghi789jklmnoabcdefghijklmnopqrstuvwxyz012345678
NarSize: 45678
References: dep1hash0000000000000000000000000-dep1-1.0 dep2hash0000000000000000000000000-dep2-2.0
Deriver: drv1hash0000000000000000000000000-hello-2.12.1.drv
Sig: cache.example.org-1:AAABBBCCC==
Sig: other.cache.org-1:DDDEEEFFF==
CA: fixed:r:sha256:1cahash000000000000000000000000000000000000000000000000000
`

func TestMinimalParse(t *testing.T) {
	n, err := Parse(strings.NewReader(minimalNarinfo))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n.StorePath != "/nix/store/abc123def456ghi789jklmn-hello-2.12.1" {
		t.Errorf("StorePath = %q", n.StorePath)
	}
	if n.URL != "nar/1abc123.nar" {
		t.Errorf("URL = %q", n.URL)
	}
	if n.Compression != "none" {
		t.Errorf("Compression = %q", n.Compression)
	}
	if n.FileSize != 12345 {
		t.Errorf("FileSize = %d", n.FileSize)
	}
	if n.NarSize != 45678 {
		t.Errorf("NarSize = %d", n.NarSize)
	}
	if n.Deriver != "" {
		t.Errorf("Deriver = %q, want empty", n.Deriver)
	}
	if len(n.Sigs) != 0 {
		t.Errorf("Sigs = %v, want nil/empty", n.Sigs)
	}
	if n.CA != "" {
		t.Errorf("CA = %q, want empty", n.CA)
	}
	if len(n.References) != 0 {
		t.Errorf("References = %v, want empty", n.References)
	}
}

func TestFullParse(t *testing.T) {
	n, err := Parse(strings.NewReader(fullNarinfo))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wantRefs := []string{
		"/nix/store/dep1hash0000000000000000000000000-dep1-1.0",
		"/nix/store/dep2hash0000000000000000000000000-dep2-2.0",
	}
	if !reflect.DeepEqual(n.References, wantRefs) {
		t.Errorf("References = %v, want %v", n.References, wantRefs)
	}
	wantDeriver := "/nix/store/drv1hash0000000000000000000000000-hello-2.12.1.drv"
	if n.Deriver != wantDeriver {
		t.Errorf("Deriver = %q, want %q", n.Deriver, wantDeriver)
	}
	wantSigs := []string{
		"cache.example.org-1:AAABBBCCC==",
		"other.cache.org-1:DDDEEEFFF==",
	}
	if !reflect.DeepEqual(n.Sigs, wantSigs) {
		t.Errorf("Sigs = %v, want %v", n.Sigs, wantSigs)
	}
	wantCA := "fixed:r:sha256:1cahash000000000000000000000000000000000000000000000000000"
	if n.CA != wantCA {
		t.Errorf("CA = %q, want %q", n.CA, wantCA)
	}
}

func TestRoundTrip(t *testing.T) {
	n, err := Parse(strings.NewReader(fullNarinfo))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	marshaled := n.Marshal()
	n2, err := Parse(strings.NewReader(marshaled))
	if err != nil {
		t.Fatalf("Parse after Marshal: %v", err)
	}
	if !reflect.DeepEqual(n, n2) {
		t.Errorf("round-trip mismatch:\n original: %+v\nreparsed: %+v", n, n2)
	}
}

func TestFingerprint(t *testing.T) {
	n := &NarInfo{
		StorePath: "/nix/store/s66mzxpvicwklp6nvshjmctarrqdu3s7-libssh2-1.10.0",
		NarHash:   "sha256:1l29f8r5z89560ndabhcj6yylaxihqadm0a37ibfwnr73x5m0b9p",
		NarSize:   294664,
		References: []string{
			"/nix/store/0jqd0rlxzra1rs38rdgwg20128y0f25r-libc-2.34",
			"/nix/store/s66mzxpvicwklp6nvshjmctarrqdu3s7-libssh2-1.10.0",
		},
	}
	want := "1;/nix/store/s66mzxpvicwklp6nvshjmctarrqdu3s7-libssh2-1.10.0;sha256:1l29f8r5z89560ndabhcj6yylaxihqadm0a37ibfwnr73x5m0b9p;294664;/nix/store/0jqd0rlxzra1rs38rdgwg20128y0f25r-libc-2.34,/nix/store/s66mzxpvicwklp6nvshjmctarrqdu3s7-libssh2-1.10.0"
	got := n.Fingerprint()
	if got != want {
		t.Errorf("Fingerprint:\n got: %s\nwant: %s", got, want)
	}
}

func TestFingerprintEmptyRefs(t *testing.T) {
	n := &NarInfo{
		StorePath:  "/nix/store/abc123-foo-1.0",
		NarHash:    "sha256:1abc",
		NarSize:    1000,
		References: []string{},
	}
	got := n.Fingerprint()
	want := "1;/nix/store/abc123-foo-1.0;sha256:1abc;1000;"
	if got != want {
		t.Errorf("Fingerprint:\n got: %s\nwant: %s", got, want)
	}
}

func TestManyReferences(t *testing.T) {
	const count = 50
	basenames := make([]string, count)
	for i := range count {
		basenames[i] = fmt.Sprintf("hash%040d-pkg-%d", i, i)
	}
	input := fmt.Sprintf(`StorePath: /nix/store/main000000000000000000000000000000-main
URL: nar/main.nar
Compression: none
FileHash: sha256:1abc
FileSize: 100
NarHash: sha256:1abc
NarSize: 200
References: %s
`, strings.Join(basenames, " "))

	n, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(n.References) != count {
		t.Errorf("len(References) = %d, want %d", len(n.References), count)
	}
	for i, ref := range n.References {
		want := "/nix/store/" + basenames[i]
		if ref != want {
			t.Errorf("References[%d] = %q, want %q", i, ref, want)
		}
	}
}

func TestMarshalFieldOrder(t *testing.T) {
	n, err := Parse(strings.NewReader(fullNarinfo))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	marshaled := n.Marshal()
	lines := strings.Split(strings.TrimRight(marshaled, "\n"), "\n")

	expectedPrefixes := []string{
		"StorePath:",
		"URL:",
		"Compression:",
		"FileHash:",
		"FileSize:",
		"NarHash:",
		"NarSize:",
		"References:",
	}
	for i, prefix := range expectedPrefixes {
		if i >= len(lines) {
			t.Errorf("line %d missing, expected prefix %q", i, prefix)
			continue
		}
		if !strings.HasPrefix(lines[i], prefix) {
			t.Errorf("line %d = %q, want prefix %q", i, lines[i], prefix)
		}
	}
	// Deriver must come after References (index 8 for this narinfo with Deriver set).
	if len(lines) <= 8 || !strings.HasPrefix(lines[8], "Deriver:") {
		t.Errorf("line 8 = %q, want Deriver:", lines[8])
	}
}

func TestParseBadNarSize(t *testing.T) {
	input := `StorePath: /nix/store/abc-test
URL: nar/abc.nar
Compression: none
FileHash: sha256:abc
FileSize: 100
NarHash: sha256:abc
NarSize: notanumber
References:
`
	_, err := Parse(strings.NewReader(input))
	if err == nil {
		t.Error("expected error for invalid NarSize, got nil")
	}
}

func TestParseBadFileSize(t *testing.T) {
	input := `StorePath: /nix/store/abc-test
URL: nar/abc.nar
Compression: none
FileHash: sha256:abc
FileSize: notanumber
NarHash: sha256:abc
NarSize: 100
References:
`
	_, err := Parse(strings.NewReader(input))
	if err == nil {
		t.Error("expected error for invalid FileSize, got nil")
	}
}

func TestParseUnknownField(t *testing.T) {
	input := `StorePath: /nix/store/abc-test
URL: nar/abc.nar
Compression: none
FileHash: sha256:abc
FileSize: 100
NarHash: sha256:abc
NarSize: 200
References:
UnknownField: somevalue
AnotherUnknown: anothervalue
`
	n, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}
	if n.StorePath != "/nix/store/abc-test" {
		t.Errorf("StorePath = %q", n.StorePath)
	}
}

func TestMultipleSigsRoundTrip(t *testing.T) {
	n, err := Parse(strings.NewReader(fullNarinfo))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(n.Sigs) != 2 {
		t.Fatalf("expected 2 sigs, got %d", len(n.Sigs))
	}
	n2, err := Parse(strings.NewReader(n.Marshal()))
	if err != nil {
		t.Fatalf("Parse after Marshal: %v", err)
	}
	if !reflect.DeepEqual(n.Sigs, n2.Sigs) {
		t.Errorf("Sigs mismatch after round-trip: got %v, want %v", n2.Sigs, n.Sigs)
	}
}
