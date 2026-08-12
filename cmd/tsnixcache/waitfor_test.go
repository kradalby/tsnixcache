// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestWaitForPidFileTimesOut covers the CI failure mode: the watcher never
// started, so the PID file never appears and the drain step must fail rather
// than hang until the job is killed.
func TestWaitForPidFileTimesOut(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "cache.pid")

	err := waitForPidFile(t.Context(), missing, 200*time.Millisecond)
	if !errors.Is(err, errWaitForPidFileTimeout) {
		t.Fatalf("waitForPidFile = %v, want errWaitForPidFileTimeout", err)
	}
}

func TestWaitForPidFileReturnsWhenProcessIsGone(t *testing.T) {
	// A process that has already exited and been reaped: its PID no longer
	// signals, which is exactly what wait-for is looking for.
	cmd := exec.CommandContext(t.Context(), "true")

	err := cmd.Run()
	if err != nil {
		t.Fatalf("run true: %v", err)
	}

	path := filepath.Join(t.TempDir(), "cache.pid")

	err = os.WriteFile(path, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = waitForPidFile(t.Context(), path, time.Minute)
	if err != nil {
		t.Fatalf("waitForPidFile: %v", err)
	}
}

// TestWaitForPidFileWaitsForUnparsableFile: watch's pid file is written by
// another process, so a reader can catch it mid-write (or find it empty from an
// older watch). That must be "not ready yet", not a hard failure — but it must
// still end at the timeout rather than hang.
func TestWaitForPidFileWaitsForUnparsableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.pid")

	err := os.WriteFile(path, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = waitForPidFile(t.Context(), path, 300*time.Millisecond)
	if !errors.Is(err, errWaitForPidFileTimeout) {
		t.Fatalf("waitForPidFile = %v, want errWaitForPidFileTimeout", err)
	}
}

// TestWaitForPidFileToleratesLateWrite: the empty file becomes a real PID while
// wait-for is polling, and wait-for must pick it up.
func TestWaitForPidFileToleratesLateWrite(t *testing.T) {
	// Already exited and reaped, so wait-for returns as soon as it reads the pid.
	cmd := exec.CommandContext(t.Context(), "true")

	err := cmd.Run()
	if err != nil {
		t.Fatalf("run true: %v", err)
	}

	path := filepath.Join(t.TempDir(), "cache.pid")

	err = os.WriteFile(path, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(200 * time.Millisecond)

		_ = os.WriteFile(path, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600)
	}()

	err = waitForPidFile(t.Context(), path, time.Minute)
	if err != nil {
		t.Fatalf("waitForPidFile: %v", err)
	}
}

// TestWritePidFileIsAtomic: wait-for polls this file every 100ms, so it must
// never be observable empty — it appears complete or not at all.
func TestWritePidFileIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.pid")

	err := writePidFile(path, 4242)
	if err != nil {
		t.Fatalf("writePidFile: %v", err)
	}

	data, err := os.ReadFile(path) // #nosec G304 -- path is inside the test's temp dir
	if err != nil {
		t.Fatal(err)
	}

	if strings.TrimSpace(string(data)) != "4242" {
		t.Errorf("pid file = %q, want 4242", data)
	}

	// No temp file left behind for a later reader (or wait-for) to trip over.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want just the pid file", len(entries))
	}
}

// TestProcessAliveForeignProcess: signal 0 answers EPERM for a live process
// owned by another uid (pid 1 unless the tests run as root). Reading that as
// "exited" made wait-for a no-op whenever the watcher ran under a different user
// — sudo in CI, a systemd unit — tearing the runner down mid-upload.
func TestProcessAliveForeignProcess(t *testing.T) {
	if !processAlive(1) {
		t.Error("processAlive(1) = false, want true (pid 1 always exists)")
	}

	if !processAlive(os.Getpid()) {
		t.Error("processAlive(os.Getpid()) = false, want true")
	}
}

// TestWaitForPidFileBlocksUntilExit: the point of the command — it must keep
// waiting while the watcher is still draining, and return once it is gone.
func TestWaitForPidFileBlocksUntilExit(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "sleep", "60")

	err := cmd.Start()
	if err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}

	path := filepath.Join(t.TempDir(), "cache.pid")

	err = os.WriteFile(path, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- waitForPidFile(t.Context(), path, time.Minute) }()

	select {
	case err := <-done:
		t.Fatalf("waitForPidFile returned %v while the process was still running", err)
	case <-time.After(500 * time.Millisecond):
	}

	_ = cmd.Process.Kill()
	// Reap it: a zombie still answers signal 0, so an unwaited child would keep
	// wait-for blocked.
	_ = cmd.Wait()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitForPidFile: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("waitForPidFile did not return after the process exited")
	}
}
