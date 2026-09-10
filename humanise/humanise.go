// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package humanise formats byte counts and transfer rates for human-readable
// logs and CLI output. The standard library has no such formatter and a whole
// dependency isn't worth two small functions.
package humanise

import "fmt"

// Bytes formats a byte count with IEC binary units, e.g. 35089528 -> "33.5 MiB".
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	const prefixes = "KMGTPE"

	// Step up while the value would *print* as a full unit: %.1f rounds 1023.95
	// up to "1024.0", so the rollover point is the rounding boundary, not 1024.
	// Otherwise 1048575 renders as "1024.0 KiB" instead of "1.0 MiB".
	val, exp := float64(n)/unit, 0
	for val >= 1023.95 && exp < len(prefixes)-1 {
		val /= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", val, prefixes[exp])
}

// Bitrate formats a transfer rate, given in bytes per second, as SI bits per
// second, e.g. 30480 -> "243.8 kbit/s". Network throughput is conventionally
// quoted in bits, so callers pass bytes/s and get bit/s.
func Bitrate(bytesPerSec float64) string {
	const unit = 1000

	bits := bytesPerSec * 8
	// Rollover happens at the rounding boundary, not at the unit: %.0f prints
	// 999.5 as "1000", %.1f prints 999.95 as "1000.0". Stepping up only at
	// >= unit would render 999999 bit/s as "1000.0 kbit/s".
	if bits < 999.5 {
		return fmt.Sprintf("%.0f bit/s", bits)
	}

	units := []string{"kbit/s", "Mbit/s", "Gbit/s", "Tbit/s", "Pbit/s"}
	val := bits / unit

	i := 0
	for val >= 999.95 && i < len(units)-1 {
		val /= unit
		i++
	}

	return fmt.Sprintf("%.1f %s", val, units[i])
}
