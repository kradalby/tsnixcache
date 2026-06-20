package narinfo

import (
	"strings"
	"testing"
)

const benchNarinfo = `StorePath: /nix/store/s66mzxpvicwklp6nvshjmctarrqdu3s7-libssh2-1.10.0
URL: nar/0a1m7b1b3j19hpwslxbdlvnmkbn5yxw5bvkh1m0xqmvd73k7naw.nar.xz
Compression: xz
FileHash: sha256:0a1m7b1b3j19hpwslxbdlvnmkbn5yxw5bvkh1m0xqmvd73k7naw
FileSize: 98765
NarHash: sha256:1l29f8r5z89560ndabhcj6yylaxihqadm0a37ibfwnr73x5m0b9p
NarSize: 294664
References: 0jqd0rlxzra1rs38rdgwg20128y0f25r-libc-2.34 5qkm48bpgf7gzfm9hcp2cn2b7p4jqnvr-openssl-3.0.8 s66mzxpvicwklp6nvshjmctarrqdu3s7-libssh2-1.10.0
Deriver: i9bwyvs5b8p16ffc5g3l69b5s7b27wv9-libssh2-1.10.0.drv
Sig: cache.nixos.org-1:tH0Xg2PpBHNmJkB7yRQClhfRwkdtSSHJV88fy/oqAU7VlEFaJYaJKzZJpJkCFkEm5nfB3mzjKBYmVb0Mg3bCg==
Sig: cache.example.org-1:ZJui+kG6vPCSRD4+p1P4DyUVlASmp/zsaeN84PTFW28tj2/cZpP3VFkUTuHhwuE8TMGEXdORJEaSVONGHNJAZQ==
CA: fixed:r:sha256:1cahash000000000000000000000000000000000000000000000000000
`

var sinkNI *NarInfo
var sinkStr string

func BenchmarkParse(b *testing.B) {
	data := []byte(benchNarinfo)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		var err error
		sinkNI, err = Parse(strings.NewReader(benchNarinfo))
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMarshal(b *testing.B) {
	ni, err := Parse(strings.NewReader(benchNarinfo))
	if err != nil {
		b.Fatal(err)
	}
	marshaled := ni.Marshal()
	b.SetBytes(int64(len(marshaled)))
	b.ReportAllocs()
	for b.Loop() {
		sinkStr = ni.Marshal()
	}
}

func BenchmarkFingerprint(b *testing.B) {
	ni, err := Parse(strings.NewReader(benchNarinfo))
	if err != nil {
		b.Fatal(err)
	}
	fp := ni.Fingerprint()
	b.SetBytes(int64(len(fp)))
	b.ReportAllocs()
	for b.Loop() {
		sinkStr = ni.Fingerprint()
	}
}
