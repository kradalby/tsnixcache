package nixcompress

import (
	"bytes"
	"io"
	"time"
)

// ProbeCodecs benchmarks approximately 1 MB of data through zstd pure-Go vs
// the external zstd binary and returns true if the external binary is faster.
// Intended to be called once at startup to inform the useExternal decision.
func ProbeCodecs() bool {
	const size = 1 << 20 // 1 MB
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i)
	}

	pureGo := benchZstd(data, false)
	if !ExternalAvailable("zstd") {
		return false
	}
	external := benchZstd(data, true)
	return external < pureGo
}

// benchZstd returns the wall-clock duration for a single encode+decode cycle.
func benchZstd(data []byte, useExternal bool) time.Duration {
	start := time.Now()

	var buf bytes.Buffer
	enc, err := Encoder(&buf, "zstd", useExternal)
	if err != nil {
		return time.Duration(1<<63 - 1)
	}
	if _, err := enc.Write(data); err != nil {
		enc.Close()
		return time.Duration(1<<63 - 1)
	}
	if err := enc.Close(); err != nil {
		return time.Duration(1<<63 - 1)
	}

	dec, err := Decoder(&buf, "zstd", useExternal)
	if err != nil {
		return time.Duration(1<<63 - 1)
	}
	if _, err := io.Copy(io.Discard, dec); err != nil {
		dec.Close()
		return time.Duration(1<<63 - 1)
	}
	dec.Close()

	return time.Since(start)
}
