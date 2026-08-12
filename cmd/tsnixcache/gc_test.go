// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const (
	day    = 24 * time.Hour
	dur20d = 20 * day
	dur10d = 10 * day
	dur5d  = 5 * day
	dur30d = 30 * day
	dur1d  = 1 * day
)

// nixHash is a syntactically valid store-path hash part: the nix-base32
// alphabet is exactly 32 characters long, which is exactly the length of a
// hash part. Only entries named like this are tsnixcache gcroots.
const nixHash = "0123456789abcdfghijklmnpqrsvwxyz"

// nixHashN returns distinct valid hash parts by rotating the alphabet.
func nixHashN(i int) string {
	i %= len(nixHash)

	return nixHash[i:] + nixHash[:i]
}

func TestParseGCRule(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
		want    GCRule
	}{
		{"80:20d", false, GCRule{80, dur20d}},
		{"95:5d", false, GCRule{95, dur5d}},
		{"100:1d", false, GCRule{100, dur1d}},
		{"1:30d", false, GCRule{1, dur30d}},
		{"80:2h", false, GCRule{80, 2 * time.Hour}},
		{"80:1h30m", false, GCRule{80, 90 * time.Minute}},
		{"0:20d", true, GCRule{}},   // threshold < 1
		{"101:20d", true, GCRule{}}, // threshold > 100
		{"80", true, GCRule{}},      // missing colon
		{"abc:20d", true, GCRule{}}, // non-numeric threshold
		{"80:", true, GCRule{}},     // empty duration
		{":20d", true, GCRule{}},    // empty threshold
		// Durations must be rejected at parse time, not when the disk fills up.
		{"80:20days", true, GCRule{}}, // "days" is not a unit
		{"80:20", true, GCRule{}},     // no unit at all
		{"80:d", true, GCRule{}},      // no count
		{"80:0d", true, GCRule{}},     // zero age would prune everything
		{"80:-5d", true, GCRule{}},    // negative
		{"80:-5m", true, GCRule{}},    // negative, Go syntax
		{"80:0s", true, GCRule{}},     // zero, Go syntax
		// A day count that overflows int64 nanoseconds used to come back
		// negative, which puts the prune cutoff in the future and makes every
		// gcroot — including one written a microsecond ago — eligible.
		{"80:106751d", false, GCRule{80, 106751 * day}},
		{"80:106752d", true, GCRule{}},
		{"80:1000000d", true, GCRule{}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseGCRule(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseGCRule(%q) err=%v wantErr=%v", tt.input, err, tt.wantErr)
			}

			if err == nil && got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseDurationString(t *testing.T) {
	tests := []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"1d", 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"2h", 2 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"1h30m", 90 * time.Minute, false},
		{"500ms", 500 * time.Millisecond, false},
		{"0d", 0, true},   // zero days invalid
		{"-1d", 0, true},  // negative invalid
		{"xd", 0, true},   // non-numeric days
		{"", 0, true},     // empty
		{"5", 0, true},    // no unit
		{"0", 0, true},    // zero is not a usable interval or age
		{"0s", 0, true},   // zero, explicit unit
		{"-5m", 0, true},  // negative Go duration
		{"1d2h", 0, true}, // days only combine with nothing; "d" is not a Go unit
		{"1D", 0, true},   // unit is lower-case
		{"20dd", 0, true}, // suffix stripped once, "20d" is not a number
		{"0.5d", 0, true}, // whole days only
		{" 20d", 0, true}, // no surrounding whitespace
		{"20d ", 0, true}, // ditto
		// Overflow: n*24h wraps int64 nanoseconds past 106751 days. The
		// wrapped value is negative, and nothing downstream re-checks the
		// sign, so it has to be refused here.
		{"106751d", 106751 * day, false}, // the largest value that fits
		{"106752d", 0, true},             // one day past it
		{"1000000d", 0, true},            // a plausible typo for "1000000s"
		{"9223372036854775807d", 0, true},
		{"9223372036854775808d", 0, true}, // beyond int64 entirely
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseDurationString(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseDurationString(%q) err=%v wantErr=%v", tt.input, err, tt.wantErr)
			}

			if err == nil && got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}

			// The invariant every caller leans on: prune cutoffs and
			// time.NewTicker both misbehave on a non-positive duration.
			if err == nil && got <= 0 {
				t.Errorf("parseDurationString(%q) = %v (%d ns), want a positive duration", tt.input, got, got)
			}
		})
	}
}

// TestGCIntervalOverflowCannotPanicTicker pins the promise serve's
// parseGCInterval makes on this parser's behalf: --gc-interval is validated
// before anything is bound, so runGCWatcher's time.NewTicker can never be
// handed a value it panics on. An overflowing day count used to pass
// validation and take the process down from a goroutine, minutes after the
// server had announced it was listening.
func TestGCIntervalOverflowCannotPanicTicker(t *testing.T) {
	for _, s := range []string{"106752d", "1000000d", "9223372036854775807d"} {
		t.Run(s, func(t *testing.T) {
			d, err := parseGCInterval(s)
			if err == nil {
				t.Fatalf("parseGCInterval(%q) = %v, want an error; time.NewTicker would panic on it", s, d)
			}
		})
	}
}

func TestSelectRule(t *testing.T) {
	rules := []GCRule{
		{80, dur20d},
		{90, dur10d},
		{95, dur5d},
	}

	tests := []struct {
		usedPct  int
		wantNil  bool
		wantRule GCRule
	}{
		{0, true, GCRule{}},
		{79, true, GCRule{}},
		{80, false, GCRule{80, dur20d}},
		{85, false, GCRule{80, dur20d}},
		{90, false, GCRule{90, dur10d}},
		{93, false, GCRule{90, dur10d}},
		{95, false, GCRule{95, dur5d}},
		{99, false, GCRule{95, dur5d}},
		{100, false, GCRule{95, dur5d}},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("used=%d", tt.usedPct), func(t *testing.T) {
			got := selectRule(rules, tt.usedPct)
			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %+v", *got)
				}

				return
			}

			if got == nil {
				t.Fatal("expected non-nil rule, got nil")
			}

			if *got != tt.wantRule {
				t.Errorf("got %+v, want %+v", *got, tt.wantRule)
			}
		})
	}
}

func TestSelectRuleEmpty(t *testing.T) {
	if got := selectRule(nil, 99); got != nil {
		t.Errorf("expected nil for empty rules, got %+v", *got)
	}
}

func TestGCRulesFlagSet(t *testing.T) {
	var f gcRulesFlag

	err := f.Set("80:20d")
	if err != nil {
		t.Fatal(err)
	}

	err = f.Set("95:5d")
	if err != nil {
		t.Fatal(err)
	}

	want := gcRulesFlag{{80, dur20d}, {95, dur5d}}

	if !reflect.DeepEqual(f, want) {
		t.Errorf("got %v, want %v", f, want)
	}
}

func TestGCRulesFlagSetInvalid(t *testing.T) {
	// A rule the flag accepts but cannot act on would only blow up once the
	// disk threshold trips, so every part of it must be rejected here.
	for _, input := range []string{"notvalid", "80:20days", "80:1 d", "80:0d", "80:20", "101:5d", "80:1000000d"} {
		t.Run(input, func(t *testing.T) {
			var f gcRulesFlag

			err := f.Set(input)
			if err == nil {
				t.Errorf("Set(%q) = nil, want error", input)
			}

			if len(f) != 0 {
				t.Errorf("Set(%q) appended %v", input, f)
			}
		})
	}
}

func TestGCRulesFlagString(t *testing.T) {
	tests := []struct {
		name string
		f    gcRulesFlag
		want string
	}{
		{"days", gcRulesFlag{{80, dur20d}, {95, dur5d}}, "80:20d,95:5d"},
		{"sub-day", gcRulesFlag{{80, 90 * time.Minute}}, "80:1h30m0s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.f.String()
			if got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}

			// A flag.Value's String must round-trip through Set.
			var round gcRulesFlag

			for s := range strings.SplitSeq(got, ",") {
				err := round.Set(s)
				if err != nil {
					t.Fatalf("Set(%q) from String(): %v", s, err)
				}
			}

			if !reflect.DeepEqual(round, tt.f) {
				t.Errorf("round-trip got %v, want %v", round, tt.f)
			}
		})
	}
}

func TestGCRulesFlagStringEmpty(t *testing.T) {
	var f gcRulesFlag

	if got := f.String(); got != "" {
		t.Errorf("String() for empty flag = %q, want %q", got, "")
	}
}

// symlink creates path pointing at target, creating target as a plain file
// unless keepDangling is set.
func symlink(t *testing.T, target, path string, keepDangling bool) {
	t.Helper()

	if !keepDangling {
		err := os.WriteFile(target, nil, 0o600)
		if err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
	}

	err := os.Symlink(target, path)
	if err != nil {
		t.Fatal(err)
	}
}

// Ageing entries is done with minAge rather than by back-dating mtimes:
// os.Chtimes follows symlinks, so it would age a gcroot's target instead of the
// gcroot. minAge 0 puts the cutoff at "now", making every entry created before
// the call eligible; a non-zero minAge makes them all too young.
const (
	ageAll  = time.Duration(0)
	ageNone = time.Hour
)

func TestPruneOldGCRoots(t *testing.T) {
	// A stand-in for /nix/store: gcroots pointing in here are ours to prune,
	// anything else in the directory is not.
	storeDir := t.TempDir()
	elsewhere := t.TempDir()

	tests := []struct {
		name      string
		entryName string // defaults to a valid hash part
		make      func(t *testing.T, storeDir, path string)
		minAge    time.Duration
		wantCount int
	}{
		{
			name: "symlink into store",
			make: func(t *testing.T, storeDir, path string) {
				t.Helper()

				symlink(t, filepath.Join(storeDir, "abc-hello"), path, false)
			},
			minAge:    ageAll,
			wantCount: 1,
		},
		{
			name: "dangling symlink into store",
			// The store path is already gone; its gcroot must still be reapable.
			make: func(t *testing.T, storeDir, path string) {
				t.Helper()

				symlink(t, filepath.Join(storeDir, "gone-already"), path, true)
			},
			minAge:    ageAll,
			wantCount: 1,
		},
		{
			name: "interrupted import's temporary link",
			// addGCRoot renames <hash>.tmp.<pid>.<seq> into place; a process
			// killed in between leaves one behind, pinning its store path.
			entryName: nixHash + ".tmp.123.4",
			make: func(t *testing.T, storeDir, path string) {
				t.Helper()

				symlink(t, filepath.Join(storeDir, "abc-hello"), path, true)
			},
			minAge:    ageAll,
			wantCount: 1,
		},
		{
			name: "young symlink into store",
			make: func(t *testing.T, storeDir, path string) {
				t.Helper()

				symlink(t, filepath.Join(storeDir, "abc-hello"), path, false)
			},
			minAge: ageNone,
		},
		{
			name: "regular file",
			make: func(t *testing.T, _, path string) {
				t.Helper()

				err := os.WriteFile(path, []byte("not a gcroot"), 0o600)
				if err != nil {
					t.Fatal(err)
				}
			},
			minAge: ageAll,
		},
		{
			name: "directory",
			make: func(t *testing.T, _, path string) {
				t.Helper()

				err := os.Mkdir(path, 0o750)
				if err != nil {
					t.Fatal(err)
				}
			},
			minAge: ageAll,
		},
		{
			name: "symlink outside the store",
			make: func(t *testing.T, _, path string) {
				t.Helper()

				symlink(t, filepath.Join(elsewhere, "precious"), path, false)
			},
			minAge: ageAll,
		},
		{
			name: "symlink to a store-dir prefix sibling",
			// Shares a string prefix with the store but is not inside it.
			make: func(t *testing.T, storeDir, path string) {
				t.Helper()

				symlink(t, storeDir+"-evil", path, true)
			},
			minAge: ageAll,
		},
		{
			name: "relative symlink",
			make: func(t *testing.T, _, path string) {
				t.Helper()

				symlink(t, "../../etc/passwd", path, true)
			},
			minAge: ageAll,
		},
		{
			// The one that takes the machine down: nix's system generations
			// are direct store symlinks whose mtime is the deploy date, so an
			// operator pointing --gcroot-dir at /nix/var/nix/profiles would
			// unlink the running system and then collect its closure.
			name:      "nix system profile",
			entryName: "system-163-link",
			make: func(t *testing.T, storeDir, path string) {
				t.Helper()

				symlink(t, filepath.Join(storeDir, "abc-nixos-system"), path, false)
			},
			minAge: ageAll,
		},
		{
			name:      "nix booted-system link",
			entryName: "booted-system",
			make: func(t *testing.T, storeDir, path string) {
				t.Helper()

				symlink(t, filepath.Join(storeDir, "abc-nixos-system"), path, true)
			},
			minAge: ageAll,
		},
		{
			// /nix/var/nix/gcroots/auto/* is hash-named but points back out of
			// the store at an indirect root: the name filter alone would take
			// it, the target filter alone would take the profile above.
			name: "hash-named indirect root",
			make: func(t *testing.T, _, path string) {
				t.Helper()

				symlink(t, filepath.Join(elsewhere, "result"), path, false)
			},
			minAge: ageAll,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gcrootDir := t.TempDir()

			entryName := tt.entryName
			if entryName == "" {
				entryName = nixHash
			}

			path := filepath.Join(gcrootDir, entryName)

			tt.make(t, storeDir, path)

			res, err := pruneOldGCRoots(gcrootDir, storeDir, tt.minAge, false)
			if err != nil {
				t.Fatal(err)
			}

			if res.pruned != tt.wantCount {
				t.Errorf("pruned=%d, want %d", res.pruned, tt.wantCount)
			}

			if res.failed != 0 {
				t.Errorf("failed=%d, want 0", res.failed)
			}

			_, statErr := os.Lstat(path)
			if tt.wantCount == 1 && !os.IsNotExist(statErr) {
				t.Error("entry should have been pruned but is still there")
			}

			if tt.wantCount == 0 && statErr != nil {
				t.Errorf("entry should have survived: %v", statErr)
			}
		})
	}
}

// TestPruneOldGCRootsDryRun: the flag exists so an operator can point gc at a
// directory and find out what it would delete before it does.
func TestPruneOldGCRootsDryRun(t *testing.T) {
	storeDir := t.TempDir()
	gcrootDir := t.TempDir()

	root := filepath.Join(gcrootDir, nixHash)
	symlink(t, filepath.Join(storeDir, "abc-hello"), root, false)

	res, err := pruneOldGCRoots(gcrootDir, storeDir, ageAll, true)
	if err != nil {
		t.Fatal(err)
	}

	if res.pruned != 1 {
		t.Errorf("pruned=%d, want 1", res.pruned)
	}

	_, statErr := os.Lstat(root)
	if statErr != nil {
		t.Errorf("dry run removed the gcroot: %v", statErr)
	}
}

// TestPruneOldGCRootsCountsFailedRemovals: a gcroot dir that has lost write
// permission prunes nothing on every tick. That has to show up somewhere, or
// the collection keeps running against a cache that never shrinks while
// errors_total sits at zero.
func TestPruneOldGCRootsCountsFailedRemovals(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permissions")
	}

	storeDir := t.TempDir()
	gcrootDir := t.TempDir()

	symlink(t, filepath.Join(storeDir, "abc-hello"), filepath.Join(gcrootDir, nixHash), false)

	err := os.Chmod(gcrootDir, 0o500) // #nosec G302 -- an unwritable gcroot dir is the point
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(gcrootDir, 0o700) }) // #nosec G302 -- t.TempDir cleanup needs it back

	res, err := pruneOldGCRoots(gcrootDir, storeDir, ageAll, false)
	if err != nil {
		t.Fatal(err)
	}

	if res.pruned != 0 {
		t.Errorf("pruned=%d, want 0", res.pruned)
	}

	if res.failed != 1 {
		t.Errorf("failed=%d, want 1", res.failed)
	}
}

// TestPruneOldGCRootsIgnoresImplausibleMtimes: a VM with no battery-backed
// clock boots near the Unix epoch and writes roots with 1970 mtimes; when NTP
// then steps the clock forward every one of them is decades old at once, and
// one tick would prune the entire cache. Rather than back-date a symlink
// (os.Chtimes follows symlinks and would age the target instead), the epoch is
// moved forward so freshly written roots land on the wrong side of it.
func TestPruneOldGCRootsIgnoresImplausibleMtimes(t *testing.T) {
	storeDir := t.TempDir()
	gcrootDir := t.TempDir()

	prev := gcRootEpoch
	gcRootEpoch = time.Now().Add(time.Hour)

	t.Cleanup(func() { gcRootEpoch = prev })

	root := filepath.Join(gcrootDir, nixHash)
	symlink(t, filepath.Join(storeDir, "abc-hello"), root, false)

	res, err := pruneOldGCRoots(gcrootDir, storeDir, ageAll, false)
	if err != nil {
		t.Fatal(err)
	}

	if res.pruned != 0 {
		t.Errorf("pruned=%d, want 0 for a gcroot older than the project", res.pruned)
	}

	_, statErr := os.Lstat(root)
	if statErr != nil {
		t.Errorf("gcroot with an implausible mtime was removed: %v", statErr)
	}
}

// TestPruneOldGCRootsMixed checks one pass over a directory holding both
// gcroots and things that are not gcroots.
func TestPruneOldGCRootsMixed(t *testing.T) {
	storeDir := t.TempDir()
	gcrootDir := t.TempDir()

	root := filepath.Join(gcrootDir, nixHash)
	symlink(t, filepath.Join(storeDir, "aaa-pkg"), root, false)

	foreign := filepath.Join(gcrootDir, nixHashN(1))
	symlink(t, filepath.Join(t.TempDir(), "precious"), foreign, false)

	profile := filepath.Join(gcrootDir, "system-2-link")
	symlink(t, filepath.Join(storeDir, "aaa-pkg"), profile, false)

	stray := filepath.Join(gcrootDir, "stray")

	err := os.WriteFile(stray, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	res, err := pruneOldGCRoots(gcrootDir, storeDir, ageAll, false)
	if err != nil {
		t.Fatal(err)
	}

	if res.pruned != 1 {
		t.Errorf("pruned=%d, want 1", res.pruned)
	}

	for _, keep := range []string{foreign, profile, stray} {
		_, statErr := os.Lstat(keep)
		if statErr != nil {
			t.Errorf("%s should have survived: %v", keep, statErr)
		}
	}

	_, statErr := os.Lstat(root)
	if !os.IsNotExist(statErr) {
		t.Error("gcroot into the store was not pruned")
	}
}

// countWarns runs fn with the default logger replaced by one that keeps only
// warnings and above, and returns how many records it produced. It mutates the
// process-wide default logger, so no test in this package may run in parallel
// with it.
func countWarns(t *testing.T, fn func()) int {
	t.Helper()

	var buf bytes.Buffer

	prev := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	defer slog.SetDefault(prev)

	fn()

	return strings.Count(buf.String(), "level=WARN")
}

// TestPruneOldGCRootsLogsOncePerRun: this runs on every --gc-interval tick
// (5m by default), so a gcroot directory holding entries that are not ours
// must cost one line per run, not one line per entry per run. The line still
// has to be a warning: if nothing is ever pruned the store grows unbounded,
// and the usual cause is a --store-dir that does not match the store path in
// the narinfos being pushed.
func TestPruneOldGCRootsLogsOncePerRun(t *testing.T) {
	storeDir := t.TempDir()
	gcrootDir := t.TempDir()
	elsewhere := t.TempDir()

	const foreignEntries = 5

	for i := range foreignEntries {
		symlink(t,
			filepath.Join(elsewhere, fmt.Sprintf("precious-%d", i)),
			filepath.Join(gcrootDir, nixHashN(i)), false)

		err := os.WriteFile(filepath.Join(gcrootDir, fmt.Sprintf("stray-%d", i)), nil, 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	var (
		res      pruneResult
		pruneErr error
	)

	warns := countWarns(t, func() {
		res, pruneErr = pruneOldGCRoots(gcrootDir, storeDir, ageAll, false)
	})

	if pruneErr != nil {
		t.Fatal(pruneErr)
	}

	if res.pruned != 0 {
		t.Errorf("pruned=%d, want 0", res.pruned)
	}

	if warns != 1 {
		t.Errorf("logged %d warnings for %d kept entries, want exactly 1 summary line",
			warns, 2*foreignEntries)
	}
}

// TestPruneOldGCRootsMissingDir: serve starts before the first push creates
// the directory, so the watcher must treat a missing one as nothing to prune.
// The gc subcommand checks for it separately (TestGCCmdMissingGCRootDir).
func TestPruneOldGCRootsMissingDir(t *testing.T) {
	res, err := pruneOldGCRoots("/nonexistent/gcroots", "/nix/store", time.Hour, false)
	if err != nil {
		t.Errorf("expected no error for missing dir, got %v", err)
	}

	if res.pruned != 0 {
		t.Errorf("expected 0 pruned, got %d", res.pruned)
	}
}

func TestApplyGCRulesEmptyRules(t *testing.T) {
	// No rules → selectRule returns nil → no GC, no error.
	g := &gcRunner{storeDir: t.TempDir(), gcrootDir: t.TempDir()}

	ran, err := g.apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if ran {
		t.Error("expected ran=false with no rules")
	}
}

func TestApplyGCRulesBadPath(t *testing.T) {
	g := &gcRunner{
		storeDir:  "/nonexistent/tsnixcache/path",
		gcrootDir: t.TempDir(),
		rules:     []GCRule{{1, dur1d}},
	}

	_, err := g.apply(context.Background())
	if err == nil {
		t.Error("expected error for non-existent storeDir")
	}
}

// TestDiskUsedPct pins the two ways this number can lie. It must agree with
// df and the cache's store_disk_used_bytes, which count the root-reserved
// blocks as used, or the same /metrics page shows three different numbers for
// one filesystem and a threshold of 80 fires early. And a filesystem that
// reports no blocks at all must be an error: 100*x/0 is NaN or +Inf, and
// converting either to int is unspecified in Go — measured minint on amd64,
// saturating on arm64, i.e. "never GC" on one and "always GC" on the other.
func TestDiskUsedPct(t *testing.T) {
	pct, err := diskUsedPct(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if pct < 0 || pct > 100 {
		t.Errorf("diskUsedPct = %d, want 0-100", pct)
	}

	var st syscall.Statfs_t

	err = syscall.Statfs(t.TempDir(), &st)
	if err != nil {
		t.Fatal(err)
	}

	want := int(100 * (st.Blocks - st.Bfree) / st.Blocks) // #nosec G115 -- 0-100 fits
	if pct != want {
		t.Errorf("diskUsedPct = %d, want %d (blocks used, reserved included)", pct, want)
	}
}

func TestDiskUsedPctZeroBlocks(t *testing.T) {
	// procfs reports zero blocks, which is the shape that makes the division
	// undefined. Nothing sane points --store-dir here, but a bind mount or a
	// typo can.
	_, err := diskUsedPct("/proc")
	if !errors.Is(err, errGCZeroBlocks) {
		t.Fatalf("diskUsedPct(/proc) err = %v, want errGCZeroBlocks", err)
	}
}

// TestApplyGCRulesStatfsError: a store that cannot be measured must be
// reported. The gauge cannot be updated, so without the error counter it
// simply freezes at its last good value and looks like a healthy store.
func TestApplyGCRulesStatfsError(t *testing.T) {
	dir := t.TempDir()
	reg := prometheus.NewRegistry()
	m := newGCMetrics(reg, dir, nil)

	g := &gcRunner{
		storeDir:  "/nonexistent/tsnixcache/path",
		gcrootDir: dir,
		rules:     []GCRule{{1, dur1d}},
		metrics:   m,
	}

	_, err := g.apply(context.Background())
	if err == nil {
		t.Fatal("expected an error for an unmeasurable store")
	}

	if got := testutil.ToFloat64(m.checks); got != 1 {
		t.Errorf("checks_total = %v, want 1", got)
	}

	if got := testutil.ToFloat64(m.errors.WithLabelValues("statfs")); got != 1 {
		t.Errorf(`errors_total{op="statfs"} = %v, want 1`, got)
	}
}

// TestNewGCMetricsCreatesEveryChild: a CounterVec with no children exports no
// series at all, so a healthy server that has never errored is indistinguishable
// on /metrics from one that is not running the watcher. The dashboard's "GC runs
// & errors" panel says errors should be zero, and it can only say so if the
// series exists. Gather the registry rather than reading the children back:
// WithLabelValues would create whatever it was asked for and pass regardless.
func TestNewGCMetricsCreatesEveryChild(t *testing.T) {
	reg := prometheus.NewRegistry()
	newGCMetrics(reg, t.TempDir(), []GCRule{{80, dur20d}, {90, dur5d}})

	got := map[string][]string{}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if m.GetCounter().GetValue() != 0 {
					t.Errorf("%s{%s=%q} = %v, want 0",
						f.GetName(), l.GetName(), l.GetValue(), m.GetCounter().GetValue())
				}

				got[f.GetName()] = append(got[f.GetName()], l.GetValue())
			}
		}
	}

	want := map[string][]string{
		"tsnixcache_gc_errors_total": {"collect", "prune", "statfs"},
		"tsnixcache_gc_runs_total":   {"80", "90"},
	}

	for name, vals := range want {
		sort.Strings(got[name])

		if !reflect.DeepEqual(got[name], vals) {
			t.Errorf("%s children = %v, want %v", name, got[name], vals)
		}
	}
}

// alwaysTriggerPct is a threshold that fires whatever the host's disk usage is,
// since usedPct is never negative. --gc-rule rejects 0, so this value exists
// only to keep these tests off a property of the machine they run on.
const alwaysTriggerPct = 0

func TestApplyGCRulesExecArgs(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")

	// Fake nix-collect-garbage records its arguments for inspection.
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\n"

	err := os.WriteFile(filepath.Join(dir, "nix-collect-garbage"), []byte(script), 0o755) // #nosec G306 -- test helper needs execute bit
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	for _, tt := range []struct {
		name   string
		dryRun bool
		want   string
	}{
		// nix-collect-garbage is called without --delete-older-than; gcroot
		// pruning is handled by pruneOldGCRoots before the GC run.
		{"normal", false, ""},
		// A dry run that still collected would be worse than no dry run.
		{"dry run", true, "--dry-run"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := &gcRunner{
				storeDir:  dir,
				gcrootDir: t.TempDir(),
				rules:     []GCRule{{alwaysTriggerPct, dur20d}},
				dryRun:    tt.dryRun,
			}

			ran, err := g.apply(context.Background())
			if err != nil {
				t.Fatal(err)
			}

			if !ran {
				t.Fatal("expected GC to run")
			}

			got, err := os.ReadFile(argsFile) // #nosec G304 -- path is under t.TempDir()
			if err != nil {
				t.Fatal(err)
			}

			if strings.TrimSpace(string(got)) != tt.want {
				t.Errorf("nix-collect-garbage args = %q, want %q", strings.TrimSpace(string(got)), tt.want)
			}
		})
	}
}

func TestApplyGCRulesMetrics(t *testing.T) {
	dir := t.TempDir()

	script := "#!/bin/sh\n"

	err := os.WriteFile(filepath.Join(dir, "nix-collect-garbage"), []byte(script), 0o755) // #nosec G306 -- test helper
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	reg := prometheus.NewRegistry()
	m := newGCMetrics(reg, dir, nil)

	g := &gcRunner{
		storeDir:  dir,
		gcrootDir: dir,
		rules:     []GCRule{{alwaysTriggerPct, dur20d}},
		metrics:   m,
	}

	ran, err := g.apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if !ran {
		t.Fatal("expected GC to run")
	}

	if got := testutil.ToFloat64(m.checks); got != 1 {
		t.Errorf("checks_total = %v, want 1", got)
	}

	label := strconv.Itoa(alwaysTriggerPct)
	if got := testutil.ToFloat64(m.runs.WithLabelValues(label)); got != 1 {
		t.Errorf("runs_total{threshold_pct=%s} = %v, want 1", label, got)
	}

	// Verify histograms recorded at least one observation.
	mfs, gatherErr := reg.Gather()
	if gatherErr != nil {
		t.Fatal(gatherErr)
	}

	histCounts := map[string]uint64{}

	for _, mf := range mfs {
		if series := mf.GetMetric(); len(series) > 0 {
			if h := series[0].GetHistogram(); h != nil {
				histCounts[mf.GetName()] = h.GetSampleCount()
			}
		}
	}

	for _, name := range []string{
		"tsnixcache_gc_prune_duration_seconds",
		"tsnixcache_gc_collect_duration_seconds",
	} {
		if histCounts[name] == 0 {
			t.Errorf("%s histogram has no observations", name)
		}
	}
}

// TestGCCmdNonDefaultStoreDir: nix-collect-garbage always collects the default
// store, so gc must refuse a --store-dir it cannot actually collect rather than
// prune one store's gcroots and collect another's.
func TestGCCmdNonDefaultStoreDir(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	// A fake nix-collect-garbage: if the guard fails, this records the fact
	// instead of the test collecting the real store.
	dir := t.TempDir()
	ranFile := filepath.Join(dir, "ran")
	script := "#!/bin/sh\ntouch " + ranFile + "\n"

	err := os.WriteFile(filepath.Join(dir, "nix-collect-garbage"), []byte(script), 0o755) // #nosec G306 -- test helper needs execute bit
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	cmd := newGCCmd()

	err = cmd.ParseAndRun(ctx, []string{
		"--gc-rule", "1:1d",
		"--store-dir", dir,
		"--gcroot-dir", t.TempDir(),
	})
	if !errors.Is(err, errGCNonDefaultStoreDir) {
		t.Fatalf("err = %v, want errGCNonDefaultStoreDir", err)
	}

	_, statErr := os.Stat(ranFile)
	if statErr == nil {
		t.Error("nix-collect-garbage ran against the default store for a non-default --store-dir")
	}
}

func TestGCCmdNoRules(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	err := newGCCmd().ParseAndRun(ctx, nil)
	if !errors.Is(err, errGCRuleNoRules) {
		t.Fatalf("err = %v, want errGCRuleNoRules", err)
	}
}

// TestGCCmdMissingGCRootDir: run from cron with a --gcroot-dir that does not
// match the server's, gc would prune nothing at all and still collect the real
// store. Treating "no such directory" as "nothing to prune" makes that silent.
func TestGCCmdMissingGCRootDir(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	dir := t.TempDir()
	ranFile := filepath.Join(dir, "ran")
	script := "#!/bin/sh\ntouch " + ranFile + "\n"

	err := os.WriteFile(filepath.Join(dir, "nix-collect-garbage"), []byte(script), 0o755) // #nosec G306 -- test helper
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	err = newGCCmd().ParseAndRun(ctx, []string{
		"--gc-rule", "1:1d",
		"--gcroot-dir", filepath.Join(dir, "not-there"),
	})
	if !errors.Is(err, errGCRootDirUnusable) {
		t.Fatalf("err = %v, want errGCRootDirUnusable", err)
	}

	_, statErr := os.Stat(ranFile)
	if statErr == nil {
		t.Error("nix-collect-garbage ran despite a gcroot dir that could not be pruned")
	}
}

// TestSkipCollect: above the threshold, disk usage does not fall on its own,
// so a collection that unroots nothing and frees nothing would otherwise
// repeat every --gc-interval forever — taking nix's global GC lock and walking
// the store, with every push and local build blocked behind it.
func TestSkipCollect(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name     string
		runner   gcRunner
		pruned   int
		wantSkip bool
	}{
		{
			name:   "never collected",
			runner: gcRunner{cooldown: time.Hour},
		},
		{
			name:     "nothing pruned, last collect freed nothing",
			runner:   gcRunner{cooldown: time.Hour, lastCollect: now.Add(-5 * time.Minute)},
			wantSkip: true,
		},
		{
			name:   "roots were pruned",
			runner: gcRunner{cooldown: time.Hour, lastCollect: now.Add(-5 * time.Minute)},
			pruned: 1,
		},
		{
			name:   "last collect freed something",
			runner: gcRunner{cooldown: time.Hour, lastCollect: now.Add(-5 * time.Minute), lastFreed: 4096},
		},
		{
			name:   "cooldown elapsed",
			runner: gcRunner{cooldown: time.Hour, lastCollect: now.Add(-2 * time.Hour)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.runner.skipCollect(tt.pruned)
			if got != tt.wantSkip {
				t.Errorf("skipCollect(%d) = %v, want %v", tt.pruned, got, tt.wantSkip)
			}
		})
	}
}

// TestApplyGCRulesSkipsRepeatCollect drives the same decision through apply:
// the rule still triggers, the prune still runs, but nix-collect-garbage is
// left alone. lastFreed is set here rather than measured because the statfs
// delta of a live filesystem is not deterministic.
func TestApplyGCRulesSkipsRepeatCollect(t *testing.T) {
	dir := t.TempDir()
	ranFile := filepath.Join(dir, "ran")
	script := "#!/bin/sh\ntouch " + ranFile + "\n"

	err := os.WriteFile(filepath.Join(dir, "nix-collect-garbage"), []byte(script), 0o755) // #nosec G306 -- test helper
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	g := &gcRunner{
		storeDir:    dir,
		gcrootDir:   t.TempDir(), // empty: nothing to prune
		rules:       []GCRule{{alwaysTriggerPct, dur20d}},
		cooldown:    time.Hour,
		lastCollect: time.Now().Add(-time.Minute),
	}

	ran, err := g.apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if ran {
		t.Error("expected the collection to be skipped")
	}

	_, statErr := os.Stat(ranFile)
	if statErr == nil {
		t.Error("nix-collect-garbage ran again with nothing pruned and nothing freed last time")
	}
}

// TestRunGCWatcherChecksBeforeTicking: a server restarting at 99% full must
// not wait a whole --gc-interval, which an operator may well have set to a day.
func TestRunGCWatcherChecksBeforeTicking(t *testing.T) {
	dir := t.TempDir()
	ranFile := filepath.Join(dir, "ran")
	script := "#!/bin/sh\ntouch " + ranFile + "\n"

	err := os.WriteFile(filepath.Join(dir, "nix-collect-garbage"), []byte(script), 0o755) // #nosec G306 -- test helper
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go runGCWatcher(ctx, dir, t.TempDir(), []GCRule{{alwaysTriggerPct, dur20d}}, 24*time.Hour, nil)

	deadline := time.Now().Add(10 * time.Second)

	for {
		_, statErr := os.Stat(ranFile)
		if statErr == nil {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal("watcher did not check before the first tick")
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// TestGCRulesFlagWarnsOnSuspectRules: both shapes parse and both are accepted,
// so the only way an operator finds out is the log.
func TestGCRulesFlagWarnsOnSuspectRules(t *testing.T) {
	tests := []struct {
		name      string
		rules     []GCRule
		wantWarns int
	}{
		{"tightening with pressure", []GCRule{{80, dur20d}, {95, dur5d}}, 0},
		{"same age throughout", []GCRule{{80, dur5d}, {95, dur5d}}, 0},
		// A fuller disk must not relax retention: 95:20d after 80:5d is a
		// transposition, and GC would get less aggressive under more pressure.
		{"transposed", []GCRule{{80, dur5d}, {95, dur20d}}, 1},
		{"transposed, written the other way round", []GCRule{{95, dur20d}, {80, dur5d}}, 1},
		// An age shorter than an import can expire the root of a path still
		// being written, which is the one thing the gcroot exists to prevent.
		{"age shorter than an import", []GCRule{{80, time.Second}}, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f gcRulesFlag

			warns := countWarns(t, func() {
				for _, r := range tt.rules {
					err := f.Set(r.String())
					if err != nil {
						t.Errorf("Set(%q): %v", r, err)
					}
				}
			})

			if warns != tt.wantWarns {
				t.Errorf("logged %d warnings, want %d", warns, tt.wantWarns)
			}

			if len(f) != len(tt.rules) {
				t.Errorf("kept %d rules, want %d: a warning must not drop the rule", len(f), len(tt.rules))
			}
		})
	}
}

func TestRunGCWatcherCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		runGCWatcher(ctx, t.TempDir(), t.TempDir(), nil, time.Hour, nil)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runGCWatcher did not return after context cancellation")
	}
}
