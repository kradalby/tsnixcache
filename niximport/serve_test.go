// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package niximport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	modeNoAck     = "no acknowledgement"
	modeInherited = "inherited stdout"
	modeBadAck    = "bad acknowledgement"
	modeFailedAck = "failed after acknowledgement"
	modeTrailing  = "trailing stdout"
)

func TestServeExchange(t *testing.T) {
	for _, tt := range []struct {
		name                string
		magic, version, ack uint64
		want                error
	}{
		{name: "minimum", magic: serveMagicServer, version: 0x205, ack: 1},
		{name: "newer", magic: serveMagicServer, version: 0x207, ack: 1},
		{name: "wrong magic", magic: 1, version: 0x207, ack: 1, want: errServeProtocol},
		{name: "old version", magic: serveMagicServer, version: 0x204, ack: 1, want: errServeProtocol},
		{name: "wrong major", magic: serveMagicServer, version: 0x305, ack: 1, want: errServeProtocol},
		{name: modeNoAck, magic: serveMagicServer, version: 0x207, want: errServeAck},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte("bounded NAR fixture")
			ni := narInfoFor(testStorePath, testNarURL, raw)
			ni.References = []string{synthStorePath("z"), synthStorePath("a"), synthStorePath("a")}
			ni.Sigs = []string{"untrusted:signature"}
			ni.CA = "untrusted-content-address"

			var response, sent bytes.Buffer
			for _, word := range []uint64{tt.magic, tt.version, tt.ack} {
				require.NoError(t, binary.Write(&response, binary.LittleEndian, word))
			}

			err := serveExchange(&sent, &response, bytes.NewReader(append(raw, []byte("not part of the NAR")...)), ni)
			require.ErrorIs(t, err, tt.want)

			if tt.want != nil {
				return
			}

			for _, word := range []uint64{serveMagicClient, serveVersion, serveAddToStoreNar} {
				var got uint64
				require.NoError(t, binary.Read(&sent, binary.LittleEndian, &got))
				require.Equal(t, word, got)
			}

			require.Equal(t, ni.StorePath, readServeTestString(t, &sent))
			require.Empty(t, readServeTestString(t, &sent))

			sum := sha256.Sum256(raw)
			require.Equal(t, hex.EncodeToString(sum[:]), readServeTestString(t, &sent))

			var count uint64
			require.NoError(t, binary.Read(&sent, binary.LittleEndian, &count))
			require.EqualValues(t, 2, count)
			refs := []string{readServeTestString(t, &sent), readServeTestString(t, &sent)}
			require.ElementsMatch(t, []string{synthStorePath("a"), synthStorePath("z")}, refs)
			require.True(t, slices.IsSorted(refs))

			for _, want := range []uint64{0, ni.NarSize, 0, 0} {
				var got uint64
				require.NoError(t, binary.Read(&sent, binary.LittleEndian, &got))
				require.Equal(t, want, got)
			}

			require.Empty(t, readServeTestString(t, &sent))
			require.Equal(t, raw, sent.Bytes())
		})
	}
}

func TestServeExchangeTruncatedStreams(t *testing.T) {
	raw := []byte("fixture")
	ni := narInfoFor(testStorePath, testNarURL, raw)

	var response bytes.Buffer
	for _, word := range []uint64{serveMagicServer, serveVersion, 1} {
		require.NoError(t, binary.Write(&response, binary.LittleEndian, word))
	}

	for size := range response.Len() {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			err := serveExchange(io.Discard, bytes.NewReader(response.Bytes()[:size]), bytes.NewReader(raw), ni)
			require.Error(t, err)
		})
	}

	err := serveExchange(io.Discard, bytes.NewReader(response.Bytes()), bytes.NewReader(raw[:len(raw)-1]), ni)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

type finalErrorReader struct {
	data []byte
	err  error
}

func (r finalErrorReader) Read(dst []byte) (int, error) {
	return copy(dst, r.data), r.err
}

func TestServeExchangeFinalDecoderError(t *testing.T) {
	raw := []byte("fixture")
	ni := narInfoFor(testStorePath, testNarURL, raw)
	want := io.ErrNoProgress

	var response bytes.Buffer
	for _, word := range []uint64{serveMagicServer, serveVersion, 1} {
		require.NoError(t, binary.Write(&response, binary.LittleEndian, word))
	}

	err := serveExchange(io.Discard, &response, finalErrorReader{data: raw, err: want}, ni)
	require.ErrorIs(t, err, want)
	require.Equal(t, 8, response.Len(), "failed decoding must not consume acknowledgement")
}

func readServeTestString(t *testing.T, from io.Reader) string {
	t.Helper()

	var size uint64
	require.NoError(t, binary.Read(from, binary.LittleEndian, &size))
	require.LessOrEqual(t, size, uint64(1<<20))
	data := make([]byte, (size+7)&^7)
	_, err := io.ReadFull(from, data)
	require.NoError(t, err)
	require.Equal(t, make([]byte, uint64(len(data))-size), data[size:])

	return string(data[:size])
}

// The shell receives only the framed NAR and therefore observes EOF before acknowledgement.
func TestServeProcessHelper(t *testing.T) {
	body := os.Getenv("TSNIXCACHE_NIX_BODY")
	if body == "" {
		t.Skip("subprocess helper")
	}

	mode := os.Getenv("TSNIXCACHE_SERVE_MODE")
	if mode == "silent" {
		_, _ = io.Copy(io.Discard, os.Stdin)

		os.Exit(0)
	}

	if mode == modeInherited {
		child := exec.CommandContext(t.Context(), "sleep", "30")
		child.Stdout = os.Stdout
		require.NoError(t, child.Start())
		require.NoError(t, os.WriteFile(os.Getenv("TSNIXCACHE_SERVE_CHILD"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)) // #nosec G703 -- parent-selected test control file.
		// The parent test owns cleanup; deliberately leave stdout inherited.
		os.Exit(0)
	}

	var hello [2]uint64
	require.NoError(t, binary.Read(os.Stdin, binary.LittleEndian, &hello))
	require.Equal(t, [2]uint64{serveMagicClient, serveVersion}, hello)
	require.NoError(t, binary.Write(os.Stdout, binary.LittleEndian, [2]uint64{serveMagicServer, serveVersion}))

	var opcode uint64
	require.NoError(t, binary.Read(os.Stdin, binary.LittleEndian, &opcode))
	require.EqualValues(t, serveAddToStoreNar, opcode)

	for range 3 {
		readServeTestString(t, os.Stdin)
	}

	var refs uint64
	require.NoError(t, binary.Read(os.Stdin, binary.LittleEndian, &refs))
	require.LessOrEqual(t, refs, uint64(1<<16))

	for range refs {
		readServeTestString(t, os.Stdin)
	}

	var metadata [4]uint64
	require.NoError(t, binary.Read(os.Stdin, binary.LittleEndian, &metadata))
	require.Zero(t, metadata[2])
	require.Zero(t, metadata[3])
	require.Empty(t, readServeTestString(t, os.Stdin))

	args := []string{body}
	if index := slices.Index(os.Args, "--"); index >= 0 {
		args = append(args, os.Args[index+1:]...)
	}

	cmd := exec.CommandContext(t.Context(), "sh", args...)   // #nosec G204 G702 -- test-owned shell fixture.
	cmd.Stdin = io.LimitReader(os.Stdin, int64(metadata[1])) // #nosec G115 -- fixture size is bounded by the parent test.
	cmd.Stdout = os.Stderr

	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			os.Exit(exitErr.ExitCode())
		}

		os.Exit(1)
	}

	if mode == modeNoAck {
		_, _ = io.Copy(io.Discard, os.Stdin)

		os.Exit(0)
	}

	ack := uint64(1)
	if mode == modeBadAck {
		ack = 0
	}

	require.NoError(t, binary.Write(os.Stdout, binary.LittleEndian, ack))

	if mode == modeTrailing {
		_, _ = os.Stdout.Write(make([]byte, 64<<10))
	}

	if mode == modeFailedAck {
		os.Exit(7)
	}

	os.Exit(0)
}

func TestImportStreamProcessCompletion(t *testing.T) {
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")

	for _, mode := range []string{"silent", modeNoAck, modeBadAck, modeFailedAck, modeInherited, modeTrailing} {
		t.Run(mode, func(t *testing.T) {
			fakeNixStore(t, "")
			t.Setenv("TSNIXCACHE_SERVE_MODE", mode)
			childPath := filepath.Join(t.TempDir(), "child")
			t.Setenv("TSNIXCACHE_SERVE_CHILD", childPath)

			if mode == modeInherited {
				t.Cleanup(func() {
					data, err := os.ReadFile(childPath) // #nosec G304 -- child control file under t.TempDir.
					require.NoError(t, err)
					pid, err := strconv.Atoi(string(data))
					require.NoError(t, err)
					child, err := os.FindProcess(pid)
					require.NoError(t, err)

					_ = child.Kill()
				})
			}

			raw := []byte("NAR payload")
			ni := narInfoFor(testStorePath, testNarURL, raw)

			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			started := time.Now()
			err := (&Importer{}).importStream(ctx, bytes.NewReader(raw), ni)

			switch mode {
			case modeBadAck:
				require.ErrorIs(t, err, errServeAck)
			case modeFailedAck:
				var exitErr *exec.ExitError
				require.ErrorAs(t, err, &exitErr)
				require.Equal(t, 7, exitErr.ExitCode())
			case modeTrailing:
				require.Error(t, err)
				require.NotErrorIs(t, err, context.DeadlineExceeded)
			default:
				require.ErrorIs(t, err, context.DeadlineExceeded)
			}

			require.Less(t, time.Since(started), waitDelay+5*time.Second)
		})
	}
}

func TestImportStreamImmediateSuccessfulExit(t *testing.T) {
	fakeNixStore(t, "")
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")

	raw := []byte("NAR payload")
	ni := narInfoFor(testStorePath, testNarURL, raw)

	for range 30 {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		err := (&Importer{}).importStream(ctx, bytes.NewReader(raw), ni)

		cancel()
		require.NoError(t, err)
	}
}
