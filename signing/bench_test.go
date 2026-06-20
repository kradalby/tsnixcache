package signing

import "testing"

const benchFingerprint = "1;/nix/store/s66mzxpvicwklp6nvshjmctarrqdu3s7-libssh2-1.10.0;sha256:1l29f8r5z89560ndabhcj6yylaxihqadm0a37ibfwnr73x5m0b9p;294664;/nix/store/0jqd0rlxzra1rs38rdgwg20128y0f25r-libc-2.34,/nix/store/s66mzxpvicwklp6nvshjmctarrqdu3s7-libssh2-1.10.0"

var sinkStr string
var sinkBool bool

func BenchmarkSign(b *testing.B) {
	sk, err := ParseSecretKey(testSecretKeyStr)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(benchFingerprint)))
	b.ReportAllocs()
	for b.Loop() {
		sinkStr = sk.Sign(benchFingerprint)
	}
}

func BenchmarkVerify(b *testing.B) {
	sk, err := ParseSecretKey(testSecretKeyStr)
	if err != nil {
		b.Fatal(err)
	}
	pk := sk.Public()
	sig := sk.Sign(benchFingerprint)
	b.SetBytes(int64(len(benchFingerprint)))
	b.ReportAllocs()
	for b.Loop() {
		sinkBool = pk.Verify(benchFingerprint, sig)
	}
}
