// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kradalby/tsnixcache/signing"
)

// writeKeyFile runs `key generate > path`, the documented setup, and returns
// what the terminal (stderr) saw.
func writeKeyFile(t *testing.T, name, path string) string {
	t.Helper()

	// #nosec G304,G302 -- test-owned path, mode mimics shell redirection
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}

	t.Cleanup(func() { _ = file.Close() })

	var info bytes.Buffer

	err = generateKey(name, file, &info)
	if err != nil {
		t.Fatalf("generateKey: %v", err)
	}

	return info.String()
}

// TestKeyGenerateWritesOnlyTheSecretKey pins the documented contract:
// `tsnixcache key generate > /etc/tsnixcache/key` must produce a file the
// server can parse, with nothing else mixed in.
func TestKeyGenerateWritesOnlyTheSecretKey(t *testing.T) {
	var out, info bytes.Buffer

	err := generateKey("cache.example.com", &out, &info)
	if err != nil {
		t.Fatalf("generateKey: %v", err)
	}

	sk, err := signing.ParseSecretKey(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatalf("stdout %q is not a usable key file: %v", out.String(), err)
	}

	if sk.Name != "cache.example.com" {
		t.Errorf("key name = %q, want cache.example.com", sk.Name)
	}

	if lines := strings.Count(strings.TrimSpace(out.String()), "\n"); lines != 0 {
		t.Errorf("stdout has %d extra lines, want a single key line: %q", lines, out.String())
	}

	if !strings.Contains(info.String(), sk.Public().String()) {
		t.Errorf("public key %q not reported on stderr: %q", sk.Public().String(), info.String())
	}
}

// TestKeyPublicRoundTrip is the README's two-command setup end to end.
func TestKeyPublicRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")

	writeKeyFile(t, "cache.example.com", path)

	var out bytes.Buffer

	err := printPublicKey(path, &out)
	if err != nil {
		t.Fatalf("printPublicKey: %v", err)
	}

	pk, err := signing.ParsePublicKey(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatalf("key public printed %q, which is not a public key: %v", out.String(), err)
	}

	data, err := os.ReadFile(path) // #nosec G304 -- test-owned path
	if err != nil {
		t.Fatal(err)
	}

	sk, err := signing.ParseSecretKey(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}

	if pk.String() != sk.Public().String() {
		t.Errorf("key public = %q, want %q", pk.String(), sk.Public().String())
	}
}

// TestKeyGenerateTightensFileMode covers the redirection case: the shell
// creates the file world-readable, and it holds a secret.
func TestKeyGenerateTightensFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")

	writeKeyFile(t, "cache.example.com", path)

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %o, want 600", st.Mode().Perm())
	}
}

// runKeyCmd runs `tsnixcache key <args…>` through the real command tree with
// stdout and stderr redirected to files, the way the README's shell redirection
// does. Calling generateKey and printPublicKey directly leaves the command
// surface untested: whether `public` is registered at all, and which stream each
// half of a generated keypair goes to.
func runKeyCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	dir := t.TempDir()
	stdoutPath := filepath.Join(dir, "stdout")
	stderrPath := filepath.Join(dir, "stderr")

	// 0644 mimics a shell redirection under a default umask.
	// #nosec G304,G302 -- test-owned path, mode mimics shell redirection
	outFile, err := os.OpenFile(stdoutPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open stdout: %v", err)
	}

	defer func() { _ = outFile.Close() }()

	// #nosec G304 -- test-owned path
	errFile, err := os.OpenFile(stderrPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open stderr: %v", err)
	}

	defer func() { _ = errFile.Close() }()

	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outFile, errFile

	defer func() { os.Stdout, os.Stderr = origOut, origErr }()

	runErr := newKeyCmd().ParseAndRun(t.Context(), args)

	stderr, err := os.ReadFile(stderrPath) // #nosec G304 -- test-owned path
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}

	return stdoutPath, string(stderr), runErr
}

// TestKeyGenerateCommandSplitsStreams runs the documented redirection through
// the subcommand itself: swap the two streams in its Exec and the key file gets
// the public key while the secret lands on the terminal.
func TestKeyGenerateCommandSplitsStreams(t *testing.T) {
	keyPath, stderr, err := runKeyCmd(t, "generate", "--name", "cache.example.com")
	if err != nil {
		t.Fatalf("key generate: %v", err)
	}

	data, err := os.ReadFile(keyPath) // #nosec G304 -- test-owned path
	if err != nil {
		t.Fatal(err)
	}

	sk, err := signing.ParseSecretKey(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("redirected stdout %q is not a usable key file: %v", data, err)
	}

	if !strings.Contains(stderr, sk.Public().String()) {
		t.Errorf("public key %q not reported on stderr: %q", sk.Public().String(), stderr)
	}

	if strings.Contains(stderr, sk.String()) {
		t.Errorf("secret key leaked to stderr: %q", stderr)
	}

	st, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	if st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %o, want 600", st.Mode().Perm())
	}
}

// TestKeyGenerateRejectsBadNames: a name is pasted verbatim into every Sig
// line, so a newline in it forges narinfo fields and a colon makes the key file
// unparseable. The operator has to hear about it here, not when the server
// refuses to start or when clients silently reject every path.
func TestKeyGenerateRejectsBadNames(t *testing.T) {
	names := []string{"", "cache:evil", "good\nStorePath: /nix/store/evil"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			var out, info bytes.Buffer

			err := generateKey(name, &out, &info)
			if err == nil {
				t.Fatalf("generateKey(%q) succeeded and wrote %q", name, out.String())
			}

			if out.Len() != 0 {
				t.Errorf("generateKey(%q) wrote %q to stdout despite failing", name, out.String())
			}
		})
	}
}

func TestKeyPublicCommandRequiresOneArgument(t *testing.T) {
	_, _, err := runKeyCmd(t, "public")
	if !errors.Is(err, errKeyPublicArgs) {
		t.Fatalf("key public (no args) = %v, want errKeyPublicArgs", err)
	}
}

func TestKeyPublicRejectsBadInput(t *testing.T) {
	dir := t.TempDir()

	garbage := filepath.Join(dir, "garbage")

	err := os.WriteFile(garbage, []byte("not a key\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	publicOnly := filepath.Join(dir, "public")

	_, pk, err := signing.GenerateKey("cache.example.com")
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(publicOnly, []byte(pk.String()+"\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
	}{
		{"missing file", filepath.Join(dir, "nope")},
		{"not a key", garbage},
		{"public key is not a secret key", publicOnly},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer

			err := printPublicKey(tt.path, &out)
			if err == nil {
				t.Fatalf("printPublicKey(%s) = %q, want error", tt.path, out.String())
			}
		})
	}
}
