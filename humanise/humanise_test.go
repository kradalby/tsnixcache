// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package humanise

import (
	"math"
	"testing"
)

func TestBytes(t *testing.T) {
	cases := map[int64]string{
		0:        "0 B",
		512:      "512 B",
		1024:     "1.0 KiB",
		1536:     "1.5 KiB",
		35089528: "33.5 MiB",
		1 << 30:  "1.0 GiB",

		// Unit boundaries: a value that rounds up to a full unit must roll over
		// to the next prefix rather than print "1024.0 KiB".
		1023 * 1024:   "1023.0 KiB",
		1<<20 - 52:    "1023.9 KiB", // just below the rounding boundary
		1<<20 - 1:     "1.0 MiB",
		1 << 20:       "1.0 MiB",
		1<<30 - 1:     "1.0 GiB",
		1<<40 - 1:     "1.0 TiB",
		1<<50 - 1:     "1.0 PiB",
		1<<60 - 1:     "1.0 EiB",
		math.MaxInt64: "8.0 EiB",
	}
	for in, want := range cases {
		if got := Bytes(in); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestBitrate(t *testing.T) {
	cases := map[float64]string{
		0:       "0 bit/s",
		100:     "800 bit/s",
		30480:   "243.8 kbit/s", // the motivating example
		255700:  "2.0 Mbit/s",
		1250000: "10.0 Mbit/s",

		// Unit boundaries, SI: same rollover rule as Bytes.
		124.9:     "999 bit/s",
		124.9375:  "1.0 kbit/s", // 999.5 bit/s rounds up to a full kbit/s
		124993.75: "1.0 Mbit/s", // 999.95 kbit/s rounds up to a full Mbit/s
		125000:    "1.0 Mbit/s",
		1e12:      "8.0 Tbit/s",

		// Bytes in, bits out: the README quotes push throughput in bit/s.
		1e6: "8.0 Mbit/s",
	}
	for in, want := range cases {
		if got := Bitrate(in); got != want {
			t.Errorf("Bitrate(%v) = %q, want %q", in, got, want)
		}
	}
}
