// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package narinfo

import (
	"bufio"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/require"

	"github.com/kradalby/tsnixcache/nixbase32"
)

// dep1Basename is fullNarinfo's first reference, used both as a bare basename
// and joined onto a store directory.
const dep1Basename = "d1p1hash000000000000000000000000-dep1-1.0"

// dep2Basename is fullNarinfo's second reference.
const dep2Basename = "d2p2hash000000000000000000000000-dep2-2.0"

// deriverBasename is fullNarinfo's Deriver.
const deriverBasename = "drv1hash000000000000000000000000-hello-2.12.1.drv"

// helloStorePath is the StorePath of both fixture narinfos.
const helloStorePath = "/nix/store/abc123df456ghi789jklmn0123456789-hello-2.12.1"

// testNarHash is a stand-in NarHash for fixtures that do not care about it. It
// still has to be a hash nix could have written: Parse re-encodes both hash
// fields, so an unparseable one is rejected.
const testNarHash = "sha256:1cahash000000000000000000000000000000000000000000000"

// testURL is the URL of both fixture narinfos.
const testURL = "nar/1abc123.nar"

// testFileHash is a stand-in FileHash, distinct from testNarHash.
const testFileHash = "sha256:1abc123df456ghi789jklmn0123456789abc123df456ghi789jk"

const minimalNarinfo = `StorePath: /nix/store/abc123df456ghi789jklmn0123456789-hello-2.12.1
URL: nar/1abc123.nar
Compression: none
FileHash: sha256:1abc123df456ghi789jklmn0123456789abc123df456ghi789jk
FileSize: 12345
NarHash: sha256:1cahash000000000000000000000000000000000000000000000
NarSize: 45678
References:
`

const fullNarinfo = `StorePath: /nix/store/abc123df456ghi789jklmn0123456789-hello-2.12.1
URL: nar/1abc123.nar
Compression: xz
FileHash: sha256:1abc123df456ghi789jklmn0123456789abc123df456ghi789jk
FileSize: 12345
NarHash: sha256:0nar123df456ghi789jklmn0123456789abc123df456ghi789jk
NarSize: 45678
References: d1p1hash000000000000000000000000-dep1-1.0 d2p2hash000000000000000000000000-dep2-2.0
Deriver: drv1hash000000000000000000000000-hello-2.12.1.drv
Sig: cache.example.org-1:AAABBBCCC==
Sig: other.cache.org-1:DDDEEEFFF==
CA: fixed:r:sha256:1cahash000000000000000000000000000000000000000000000
`

func TestMinimalParse(t *testing.T) {
	n, err := Parse(strings.NewReader(minimalNarinfo), DefaultStoreDir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if n.StorePath != helloStorePath {
		t.Errorf("StorePath = %q", n.StorePath)
	}

	if n.URL != testURL {
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
	n, err := Parse(strings.NewReader(fullNarinfo), DefaultStoreDir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	wantRefs := []string{
		"/nix/store/" + dep1Basename,
		"/nix/store/" + dep2Basename,
	}

	if !reflect.DeepEqual(n.References, wantRefs) {
		t.Errorf("References = %v, want %v", n.References, wantRefs)
	}

	wantDeriver := "/nix/store/" + deriverBasename

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

	wantCA := "fixed:r:sha256:1cahash000000000000000000000000000000000000000000000"

	if n.CA != wantCA {
		t.Errorf("CA = %q, want %q", n.CA, wantCA)
	}
}

func TestRoundTrip(t *testing.T) {
	n, err := Parse(strings.NewReader(fullNarinfo), DefaultStoreDir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	marshaled := n.Marshal()

	n2, err := Parse(strings.NewReader(marshaled), DefaultStoreDir)
	if err != nil {
		t.Fatalf("Parse after Marshal: %v", err)
	}

	if !reflect.DeepEqual(n, n2) {
		t.Errorf("round-trip mismatch:\n original: %+v\nreparsed: %+v", n, n2)
	}
}

func TestFingerprint(t *testing.T) {
	n := &NarInfo{
		StorePath: "/nix/store/s66mzxpvicwklp6nvshjmcarrqdv3s7a-libssh2-1.10.0",
		NarHash:   "sha256:1l29f8r5z89560ndabhcj6yylaxihqadm0a37ibfwnr73x5m0b9p",
		NarSize:   294664,
		References: []string{
			"/nix/store/0jqd0rlxzra1rs38rdgwg20128y0f25r-libc-2.34",
			"/nix/store/s66mzxpvicwklp6nvshjmcarrqdv3s7a-libssh2-1.10.0",
		},
	}
	want := "1;/nix/store/s66mzxpvicwklp6nvshjmcarrqdv3s7a-libssh2-1.10.0;sha256:1l29f8r5z89560ndabhcj6yylaxihqadm0a37ibfwnr73x5m0b9p;294664;/nix/store/0jqd0rlxzra1rs38rdgwg20128y0f25r-libc-2.34,/nix/store/s66mzxpvicwklp6nvshjmcarrqdv3s7a-libssh2-1.10.0"

	got := n.Fingerprint()

	if got != want {
		t.Errorf("Fingerprint:\n got: %s\nwant: %s", got, want)
	}
}

func TestFingerprintEmptyRefs(t *testing.T) {
	n := &NarInfo{
		StorePath:  "/nix/store/abc123df456ghi789jklmn0123456789-foo-1.0",
		NarHash:    testNarHash,
		NarSize:    1000,
		References: []string{},
	}

	got := n.Fingerprint()

	want := "1;/nix/store/abc123df456ghi789jklmn0123456789-foo-1.0;" + testNarHash + ";1000;"

	if got != want {
		t.Errorf("Fingerprint:\n got: %s\nwant: %s", got, want)
	}
}

// narinfoWith renders a valid narinfo whose References line is refs, so a test
// can vary one field without repeating the required ones Parse now insists on.
func narinfoWith(refs string) string {
	return fmt.Sprintf(`StorePath: %s
URL: nar/main.nar
Compression: none
FileHash: %s
FileSize: 100
NarHash: %s
NarSize: 200
References: %s
`, helloStorePath, testFileHash, testNarHash, refs)
}

func TestManyReferences(t *testing.T) {
	const count = 50

	basenames := make([]string, count)
	for i := range count {
		basenames[i] = fmt.Sprintf("hash%028d-pkg-%d", i, i)
	}

	n, err := Parse(strings.NewReader(narinfoWith(strings.Join(basenames, " "))), DefaultStoreDir)
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

// TestReferencesLineOverScannerLimit covers a References line longer than
// bufio.Scanner's 64 KiB default, which a large closure easily exceeds.
func TestReferencesLineOverScannerLimit(t *testing.T) {
	const count = 2000 // ~2000 × 55 bytes ≈ 110 KiB on one line

	basenames := make([]string, count)
	for i := range count {
		basenames[i] = fmt.Sprintf("hash%028d-pkg-%d", i, i)
	}

	refLine := strings.Join(basenames, " ")
	if len(refLine) <= bufio.MaxScanTokenSize {
		t.Fatalf("test author error: References line is only %d bytes", len(refLine))
	}

	input := narinfoWith(refLine) + "Deriver: " + deriverBasename + "\n"

	n, err := Parse(strings.NewReader(input), DefaultStoreDir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(n.References) != count {
		t.Fatalf("len(References) = %d, want %d", len(n.References), count)
	}

	if n.References[count-1] != "/nix/store/"+basenames[count-1] {
		t.Errorf("last reference = %q", n.References[count-1])
	}

	// The Deriver line follows the long one; a truncated read would lose it.
	if n.Deriver != "/nix/store/"+deriverBasename {
		t.Errorf("Deriver = %q", n.Deriver)
	}
}

func TestParseFinalLineWithoutNewline(t *testing.T) {
	n, err := Parse(strings.NewReader(strings.TrimSuffix(minimalNarinfo, "\n")), DefaultStoreDir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if n.NarSize != 45678 {
		t.Errorf("NarSize = %d, want 45678", n.NarSize)
	}
}

// TestStoreDir checks References and Deriver survive a round trip through a
// non-default store directory: parsed against it, and marshalled back to bare
// basenames rather than absolute paths.
func TestStoreDir(t *testing.T) {
	tests := []struct {
		name     string
		storeDir string
		wantRef  string
	}{
		{"default", DefaultStoreDir, "/nix/store/" + dep1Basename},
		{"empty means default", "", "/nix/store/" + dep1Basename},
		{"trailing slash", "/nix/store/", "/nix/store/" + dep1Basename},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, err := Parse(strings.NewReader(fullNarinfo), tc.storeDir)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if n.References[0] != tc.wantRef {
				t.Errorf("References[0] = %q, want %q", n.References[0], tc.wantRef)
			}

			marshaled := n.Marshal()

			wantRefs := "References: " + dep1Basename + " " + dep2Basename + "\n"
			if !strings.Contains(marshaled, wantRefs) {
				t.Errorf("Marshal did not emit basenames:\n%s", marshaled)
			}

			wantDeriver := "Deriver: " + deriverBasename + "\n"
			if !strings.Contains(marshaled, wantDeriver) {
				t.Errorf("Marshal did not emit Deriver basename:\n%s", marshaled)
			}

			n2, err := Parse(strings.NewReader(marshaled), tc.storeDir)
			if err != nil {
				t.Fatalf("Parse after Marshal: %v", err)
			}

			if !reflect.DeepEqual(n, n2) {
				t.Errorf("round-trip mismatch:\n original: %+v\nreparsed: %+v", n, n2)
			}
		})
	}
}

// TestAlternativeStoreDir covers a store dir that is not /nix/store: the
// StorePath must be under it, and a StorePath under the default store dir is
// then not one this server can hold.
func TestAlternativeStoreDir(t *testing.T) {
	const altDir = "/mnt/nix/store"

	input := strings.ReplaceAll(fullNarinfo, "StorePath: /nix/store/", "StorePath: "+altDir+"/")

	n, err := Parse(strings.NewReader(input), altDir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if n.References[0] != altDir+"/"+dep1Basename {
		t.Errorf("References[0] = %q", n.References[0])
	}

	_, err = Parse(strings.NewReader(fullNarinfo), altDir)
	if !errors.Is(err, errInvalidStorePath) {
		t.Errorf("Parse of a /nix/store path against %s: err = %v, want errInvalidStorePath", altDir, err)
	}
}

// TestParseCRLF covers a narinfo written with DOS line endings. Nix and this
// package both emit bare \n, but a hand-edited or third-party pusher may not,
// and a trailing \r glued onto every value turns numeric fields into parse
// errors and store paths into unusable ones.
func TestParseCRLF(t *testing.T) {
	input := strings.ReplaceAll(fullNarinfo, "\n", "\r\n")

	n, err := Parse(strings.NewReader(input), DefaultStoreDir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if n.StorePath != helloStorePath {
		t.Errorf("StorePath = %q", n.StorePath)
	}

	if n.NarHash != "sha256:0nar123df456ghi789jklmn0123456789abc123df456ghi789jk" {
		t.Errorf("NarHash = %q", n.NarHash)
	}

	if n.NarSize != 45678 {
		t.Errorf("NarSize = %d, want 45678", n.NarSize)
	}

	if n.FileSize != 12345 {
		t.Errorf("FileSize = %d, want 12345", n.FileSize)
	}

	wantRefs := []string{
		"/nix/store/" + dep1Basename,
		"/nix/store/" + dep2Basename,
	}
	if !reflect.DeepEqual(n.References, wantRefs) {
		t.Errorf("References = %q, want %q", n.References, wantRefs)
	}

	if n.Deriver != "/nix/store/"+deriverBasename {
		t.Errorf("Deriver = %q", n.Deriver)
	}
}

// TestReferencesAllocationBound pins the References slice to one entry per 33
// bytes of line for every token shape, not just the all-spaces one this test
// was first written for. Spaces are the least interesting hostile input: a line
// of one-character tokens is a one-character edit away from it and, when the
// bound was a pre-size hint only, grew the slice by doubling well past the hint
// — ~54 bytes of allocation per input byte, against a 1 MiB body cap.
//
// A line carrying more entries than store path basenames could fit is not a
// reference list, so Parse must reject it rather than truncate it. Entry
// validation now turns most of these shapes away first, on the same grounds and
// a line earlier; the ceiling stays as the bound that does not depend on it.
func TestReferencesAllocationBound(t *testing.T) {
	const lineLen = 128 << 10

	realRef := dep1Basename + " " // 42 bytes
	realCount := lineLen / len(realRef)

	tests := []struct {
		name     string
		value    string
		wantRefs int // negative: the line must be rejected
	}{
		{"only spaces", strings.Repeat(" ", lineLen), 0},
		{"one-character tokens", strings.Repeat("a ", lineLen/2), -1},
		{"two-character tokens", strings.Repeat("ab ", lineLen/3), -1},
		{"tokens just under the hash length", strings.Repeat(strings.Repeat("a", 31)+" ", lineLen/32), -1},
		{"real basenames", strings.TrimSuffix(strings.Repeat(realRef, realCount), " "), realCount},
		{"single short token", "abc", -1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, err := Parse(strings.NewReader(narinfoWith(tc.value)), DefaultStoreDir)

			if tc.wantRefs < 0 {
				if err == nil {
					t.Fatalf("Parse accepted %d bytes: %d references, cap %d",
						len(tc.value), len(n.References), cap(n.References))
				}

				return
			}

			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if len(n.References) != tc.wantRefs {
				t.Errorf("len(References) = %d, want %d", len(n.References), tc.wantRefs)
			}

			if maxCap := maxReferences(len(tc.value)); cap(n.References) > maxCap {
				t.Errorf("cap(References) = %d for a %d-byte line, want <= %d",
					cap(n.References), len(tc.value), maxCap)
			}
		})
	}
}

// TestReferencesRejectsFullPaths pins the entry shape. A reference carrying its
// own store dir used to be prefixed a second time, so the fingerprint and the
// export stream saw "/nix/store//nix/store/…" while Marshal's path.Base put the
// basename back — parse and marshal disagreeing about the same narinfo.
func TestReferencesRejectsFullPaths(t *testing.T) {
	for _, ref := range []string{
		"/nix/store/" + dep1Basename,
		"../" + dep1Basename,
		".hidden",
		"nohashpart",
		"d1p1hash000000000000000000000000-dep1/1.0",
		"d1p1hash00000000000000000000000e-dep1-1.0", // 'e' is not in the nix alphabet
		"d1p1hash000000000000000000000000-",
	} {
		_, err := Parse(strings.NewReader(narinfoWith(ref)), DefaultStoreDir)
		if !errors.Is(err, errInvalidName) {
			t.Errorf("Parse(%q): err = %v, want errInvalidName", ref, err)
		}
	}
}

// TestMarshalSizeExact checks the Grow hint equals the output length, so the
// builder neither re-allocates nor over-reserves on the narinfo GET hot path.
// FileSize and NarSize are maximal uint64s because marshalSize budgets 20
// digits for them.
func TestMarshalSizeExact(t *testing.T) {
	n, err := Parse(strings.NewReader(fullNarinfo), DefaultStoreDir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	n.FileSize = math.MaxUint64
	n.NarSize = math.MaxUint64

	for _, tc := range []struct {
		name string
		ni   *NarInfo
	}{
		{"full", n},
		{"no refs, no deriver, no ca", &NarInfo{
			StorePath: helloStorePath,
			URL:       testURL,
			NarHash:   testNarHash,
			FileSize:  math.MaxUint64,
			NarSize:   math.MaxUint64,
		}},
		{"one ref", &NarInfo{
			StorePath:  helloStorePath,
			References: []string{"/nix/store/" + dep1Basename},
			FileSize:   math.MaxUint64,
			NarSize:    math.MaxUint64,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.ni.marshalSize()

			want := len(tc.ni.Marshal())
			if got != want {
				t.Errorf("marshalSize() = %d, Marshal() is %d bytes", got, want)
			}
		})
	}
}

func TestMarshalFieldOrder(t *testing.T) {
	n, err := Parse(strings.NewReader(fullNarinfo), DefaultStoreDir)
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

// TestMarshalEmptyReferencesTrailingSpace pins the single byte that decides
// whether nix can read the narinfo of a reference-free path: nix takes the value
// from colon+2, so "References:" without the space is unreadable to it.
func TestMarshalEmptyReferencesTrailingSpace(t *testing.T) {
	n := &NarInfo{
		StorePath:   helloStorePath,
		URL:         testURL,
		Compression: "none",
		NarHash:     testNarHash,
		NarSize:     200,
	}

	if !strings.Contains(n.Marshal(), "References: \n") {
		t.Errorf("Marshal dropped the trailing space after References:\n%q", n.Marshal())
	}
}

func TestParseBadNarSize(t *testing.T) {
	input := strings.ReplaceAll(minimalNarinfo, "NarSize: 45678", "NarSize: notanumber")

	_, err := Parse(strings.NewReader(input), DefaultStoreDir)
	if err == nil {
		t.Error("expected error for invalid NarSize, got nil")
	}
}

func TestParseBadFileSize(t *testing.T) {
	input := strings.ReplaceAll(minimalNarinfo, "FileSize: 12345", "FileSize: notanumber")

	_, err := Parse(strings.NewReader(input), DefaultStoreDir)
	if err == nil {
		t.Error("expected error for invalid FileSize, got nil")
	}
}

func TestParseUnknownField(t *testing.T) {
	input := minimalNarinfo + "UnknownField: somevalue\nAnotherUnknown: anothervalue\n"

	n, err := Parse(strings.NewReader(input), DefaultStoreDir)
	if err != nil {
		t.Fatalf("Parse: unexpected error: %v", err)
	}

	if n.StorePath != helloStorePath {
		t.Errorf("StorePath = %q", n.StorePath)
	}
}

// TestParseRequiredFields covers nix's own end-of-constructor check. Without it
// anything at all parses to a zero NarInfo with a nil error, and the server
// treats that as a push: an empty URL in particular resolves the importer's
// spool file to the spool directory itself, which it then deletes.
func TestParseRequiredFields(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty body", ""},
		{"prose", "hello world\n"},
		{"binary", "\x00\x01\x02"},
		{"only a URL", "URL: nar/zstd-cache\n"},
		{"no StorePath", strings.ReplaceAll(minimalNarinfo, "StorePath: "+helloStorePath+"\n", "")},
		{"no URL", strings.ReplaceAll(minimalNarinfo, "URL: "+testURL+"\n", "")},
		{"empty URL", strings.ReplaceAll(minimalNarinfo, "URL: "+testURL, "URL:")},
		{"no NarHash", strings.ReplaceAll(minimalNarinfo, "NarHash: "+testNarHash+"\n", "")},
		{"no NarSize", strings.ReplaceAll(minimalNarinfo, "NarSize: 45678\n", "")},
		{"zero NarSize", strings.ReplaceAll(minimalNarinfo, "NarSize: 45678", "NarSize: 0")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.input), DefaultStoreDir)
			if !errors.Is(err, errMissingField) {
				t.Errorf("Parse: err = %v, want errMissingField", err)
			}
		})
	}
}

// TestParseInvalidStorePath pins the shape of the one field the importer turns
// into a GC root symlink.
func TestParseInvalidStorePath(t *testing.T) {
	for _, sp := range []string{
		"abc123df456ghi789jklmn0123456789-hello",            // no store dir
		"/nix/store/hello-2.12.1",                           // no hash part
		"/nix/store/abc123df456ghi789jklmn012345678-hello",  // hash one character short
		"/nix/store/abc123df456ghi789jklmn0123456789-.evil", // leading dot in the name
		"/nix/store/abc123df456ghi789jklmn0123456789-a/b",   // a name that escapes the store
		"/nix/store/../etc/passwd",
		"/nix/store/abc123df456ghi789jklmn0123456789",
	} {
		input := strings.ReplaceAll(minimalNarinfo, helloStorePath, sp)

		_, err := Parse(strings.NewReader(input), DefaultStoreDir)
		if !errors.Is(err, errInvalidStorePath) {
			t.Errorf("Parse(StorePath: %q): err = %v, want errInvalidStorePath", sp, err)
		}
	}
}

// TestParseHashNormalisation covers the three encodings nix accepts plus its SRI
// separator. Keeping the incoming spelling makes a correct hash compare unequal
// to a re-encoding of itself, which the importer reports as "NarHash mismatch"
// on byte-identical content.
func TestParseHashNormalisation(t *testing.T) {
	const canonical = "sha256:1abc123df456ghi789jklmn0123456789abc123df456ghi789jk"

	tests := []struct {
		name string
		hash string
	}{
		{"nix32", canonical},
		{"base16", "sha256:532674227ca610d786086ca9848e296488006ca5532674227ca610d786086ca9"},
		{"base64", "sha256:UyZ0InymENeGCGyphI4pZIgAbKVTJnQifKYQ14YIbKk="},
		{"sri", "sha256-UyZ0InymENeGCGyphI4pZIgAbKVTJnQifKYQ14YIbKk="},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.ReplaceAll(minimalNarinfo, "NarHash: "+testNarHash, "NarHash: "+tc.hash)

			n, err := Parse(strings.NewReader(input), DefaultStoreDir)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if n.NarHash != canonical {
				t.Errorf("NarHash = %q, want %q", n.NarHash, canonical)
			}
		})
	}
}

func TestParseBadHash(t *testing.T) {
	for _, hash := range []string{
		"",
		"1abc",                 // no algorithm
		"sha256:1abc",          // too short for any encoding
		"md4:abcdef0123456789", // an algorithm nix does not have
		"sha256:zzzz123df456ghi789jklmn0123456789abc123df456ghi789jk", // not nix-base32
		"sha256:9abc123df456ghi789jklmn0123456789abc123df456ghi789jk", // surplus bits set
	} {
		input := strings.ReplaceAll(minimalNarinfo, "NarHash: "+testNarHash, "NarHash: "+hash)

		_, err := Parse(strings.NewReader(input), DefaultStoreDir)
		if err == nil {
			t.Errorf("Parse(NarHash: %q): expected an error", hash)
		}
	}
}

// TestParseCompressionDefault covers nix's `if (compression == "") compression =
// "bzip2"`. An absent field used to reach the decompressor as "" and fail there
// as an unknown compression.
func TestParseCompressionDefault(t *testing.T) {
	input := strings.ReplaceAll(minimalNarinfo, "Compression: none\n", "")

	n, err := Parse(strings.NewReader(input), DefaultStoreDir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if n.Compression != defaultCompression {
		t.Errorf("Compression = %q, want %q", n.Compression, defaultCompression)
	}

	// The converse: a zero Compression must not be emitted bare, because nix
	// reads that back as bzip2 rather than as "no compression".
	bare := &NarInfo{StorePath: helloStorePath, URL: "nar/x.nar", NarHash: testNarHash, NarSize: 1}

	if !strings.Contains(bare.Marshal(), "Compression: "+defaultCompression+"\n") {
		t.Errorf("Marshal emitted an empty Compression:\n%q", bare.Marshal())
	}
}

// TestParseUnknownDeriver covers the sentinel nix writes for a path with no
// known deriver. Taken literally it became /nix/store/unknown-deriver, which
// then failed the import of a narinfo nix itself accepts.
func TestParseUnknownDeriver(t *testing.T) {
	for _, value := range []string{"unknown-deriver", ""} {
		input := minimalNarinfo + "Deriver: " + value + "\n"

		n, err := Parse(strings.NewReader(input), DefaultStoreDir)
		if err != nil {
			t.Fatalf("Parse(Deriver: %q): %v", value, err)
		}

		if n.Deriver != "" {
			t.Errorf("Deriver = %q for %q, want empty", n.Deriver, value)
		}
	}
}

func TestParseBadDeriver(t *testing.T) {
	input := minimalNarinfo + "Deriver: /nix/store/" + deriverBasename + "\n"

	_, err := Parse(strings.NewReader(input), DefaultStoreDir)
	if !errors.Is(err, errInvalidName) {
		t.Errorf("Parse: err = %v, want errInvalidName", err)
	}
}

// TestParseTooManySigs pins the ceiling on the last repeated field that had
// none: a body of bare "Sig: " lines bought roughly four bytes of slice per
// byte of request.
func TestParseTooManySigs(t *testing.T) {
	const bodyLen = 1 << 20

	input := minimalNarinfo + strings.Repeat("Sig: \n", bodyLen/len("Sig: \n"))

	_, err := Parse(strings.NewReader(input), DefaultStoreDir)
	if !errors.Is(err, errTooManySigs) {
		t.Errorf("Parse: err = %v, want errTooManySigs", err)
	}
}

// TestParseDuplicateFields covers the two fields nix rejects a repeat of
// ("extra References", "extra CA") rather than letting the last one win.
func TestParseDuplicateFields(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"References", minimalNarinfo + "References: " + dep1Basename + "\n"},
		{"CA", minimalNarinfo + "CA: text:sha256:x\nCA: text:sha256:y\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.input), DefaultStoreDir)
			if !errors.Is(err, errDuplicateField) {
				t.Errorf("Parse: err = %v, want errDuplicateField", err)
			}
		})
	}
}

func TestMultipleSigsRoundTrip(t *testing.T) {
	n, err := Parse(strings.NewReader(fullNarinfo), DefaultStoreDir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(n.Sigs) != 2 {
		t.Fatalf("expected 2 sigs, got %d", len(n.Sigs))
	}

	n2, err := Parse(strings.NewReader(n.Marshal()), DefaultStoreDir)
	if err != nil {
		t.Fatalf("Parse after Marshal: %v", err)
	}

	if !reflect.DeepEqual(n.Sigs, n2.Sigs) {
		t.Errorf("Sigs mismatch after round-trip: got %v, want %v", n2.Sigs, n.Sigs)
	}
}

func TestCanonicalHashNixCompatibility(t *testing.T) {
	zero := "sha256:" + strings.Repeat("0", 52)
	for _, tc := range []struct {
		name  string
		input string
		valid bool
	}{
		{"hex", "sha256:" + strings.Repeat("0", 64), true},
		{"SRI is base64", "sha256-" + strings.Repeat("0", 64), false},
		{"unpadded SRI", "sha256-" + strings.Repeat("A", 43), true},
		{"unpadded colon", "sha256:" + strings.Repeat("A", 43), false},
		{"short decoded digest", "sha256:" + strings.Repeat("A", 42) + "==", false},
		{"long decoded digest", "sha256:" + strings.Repeat("A", 44), false},
		{"nonzero padding bits", "sha256:" + strings.Repeat("A", 42) + "B=", true},
		{"newline counts for colon encoding selection", "sha256:" + strings.Repeat("A", 43) + "\n", true},
		{"carriage return rejected", "sha256:" + strings.Repeat("A", 43) + "\r", false},
		{"padding terminates SRI", "sha256-" + strings.Repeat("A", 43) + "=ignored", true},
		{"colon separator wins", "sha256-" + strings.Repeat("A", 43) + "=ignored:x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CanonicalHash(tc.input)
			if !tc.valid {
				require.ErrorIs(t, err, errInvalidHash)

				return
			}

			require.NoError(t, err)
			require.Equal(t, zero, got)
		})
	}
}

func TestParseMarshalOptionalFields(t *testing.T) {
	for _, text := range []string{
		strings.ReplaceAll(minimalNarinfo, "FileHash: "+testFileHash+"\n", ""),
		"Ignored: " + strings.Repeat("x", 10000) + "\n" + minimalNarinfo + strings.Repeat("Sig: x\n", 50),
	} {
		n, err := Parse(strings.NewReader(text), DefaultStoreDir)
		require.NoError(t, err)
		roundtrip, err := Parse(strings.NewReader(n.Marshal()), DefaultStoreDir)
		require.NoError(t, err)
		require.Equal(t, n, roundtrip)
	}
}

func FuzzCanonicalHash(f *testing.F) {
	for _, seed := range []string{
		testNarHash, "sha256:" + strings.Repeat("A", 44), "sha256:" + strings.Repeat("A", 42) + "==",
		"sha256-" + strings.Repeat("A", 43), "sha512:" + strings.Repeat("z", 103), "md5:00", "sha1:\x00",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 1<<16 {
			t.Skip()
		}

		rawHash := sha512.Sum512([]byte(text))
		for algorithm, size := range hashSizes {
			raw := rawHash[:size]

			want := algorithm + ":" + nixbase32.EncodeToString(raw)
			for _, encoded := range []string{
				want, algorithm + ":" + hex.EncodeToString(raw), algorithm + ":" + strings.ToUpper(hex.EncodeToString(raw)),
				algorithm + ":" + base64.StdEncoding.EncodeToString(raw), algorithm + "-" + base64.RawStdEncoding.EncodeToString(raw),
			} {
				got, err := CanonicalHash(encoded)
				require.NoError(t, err)
				require.Equal(t, want, got)
			}
		}

		canonical, err := CanonicalHash(text)
		if err != nil {
			return
		}

		again, err := CanonicalHash(canonical)
		require.NoError(t, err)
		require.Equal(t, canonical, again)
		algorithm, digest, ok := strings.Cut(canonical, ":")
		require.True(t, ok)

		raw, err := nixbase32.DecodeString(digest)
		require.NoError(t, err)
		require.Len(t, raw, hashSizes[algorithm])
	})
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		minimalNarinfo, fullNarinfo, "", "NarSize: 18446744073709551616\n", "References: ../../etc/passwd\n",
		strings.ReplaceAll(minimalNarinfo, "FileHash: "+testFileHash+"\n", ""),
		minimalNarinfo + strings.Repeat("Sig: \n", 2000),
		"Ignored: " + strings.Repeat("x", 10000) + "\n" + minimalNarinfo + strings.Repeat("Sig: x\n", 50),
		strings.ReplaceAll(minimalNarinfo, "\n", "\r\n"),
		minimalNarinfo + "CA: " + strings.Repeat("x", 1<<17) + "\n",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 1<<20 {
			t.Skip()
		}

		n, err := Parse(strings.NewReader(text), DefaultStoreDir)
		if err != nil {
			return
		}

		rendered := n.Marshal()
		// Normalization adds field labels but cannot grow input without bound.
		require.LessOrEqual(t, len(rendered), 2*len(text)+1024)
		roundtrip, err := Parse(strings.NewReader(rendered), DefaultStoreDir)
		require.NoError(t, err)
		require.Empty(t, cmp.Diff(n, roundtrip, cmpopts.EquateEmpty()))
		require.Equal(t, n.Fingerprint(), roundtrip.Fingerprint())
	})
}

func TestParseSignatureLimit(t *testing.T) {
	input := minimalNarinfo + strings.Repeat("Sig: key:AA==\n", maxSignatures)
	n, err := Parse(strings.NewReader(input), DefaultStoreDir)
	require.NoError(t, err)
	require.Len(t, n.Sigs, maxSignatures)
	roundtrip, err := Parse(strings.NewReader(n.Marshal()), DefaultStoreDir)
	require.NoError(t, err)
	require.Equal(t, n, roundtrip)

	_, err = Parse(strings.NewReader(input+"Sig: key:AA==\n"), DefaultStoreDir)
	require.ErrorIs(t, err, errTooManySigs)
}
