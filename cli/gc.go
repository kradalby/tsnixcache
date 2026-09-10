// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/peterbourgon/ff/v4"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kradalby/tsnixcache/gcroot"
	"github.com/kradalby/tsnixcache/nixbase32"
)

// defaultStoreDir is the store nix-collect-garbage operates on.
const defaultStoreDir = "/nix/store"

// maxDurationDays is the largest whole-day count a time.Duration can hold.
// Past it the nanosecond product wraps to a negative duration, and a negative
// age is the most dangerous value in this package: it puts the prune cutoff in
// the future, so every gcroot — including one created a microsecond ago — is
// "old enough" and nix-collect-garbage then reaps the paths they held.
const maxDurationDays = int64(math.MaxInt64 / (24 * time.Hour))

// minSafeOlderThan preserves a grace period for newly imported paths under
// aggressive GC rules; ownership locks separately protect active imports.
const minSafeOlderThan = time.Minute

// collectCooldownFloor bounds how often a collection that has nothing to do
// may repeat: see gcRunner.apply.
const collectCooldownFloor = time.Hour

// gcRootEpoch rejects implausible mtimes from unsynchronized clocks, preventing
// a later clock correction from making newly created roots eligible for pruning.
var gcRootEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// GCRule pairs a disk-usage threshold with a minimum gcroot age before pruning.
type GCRule struct {
	ThresholdPct int           // 1-100
	OlderThan    time.Duration // gcroots older than this are eligible for removal
}

// String renders the rule the way it is written on the command line.
func (r GCRule) String() string {
	return fmt.Sprintf("%d:%s", r.ThresholdPct, formatDuration(r.OlderThan))
}

// gcRulesFlag implements flag.Value for a repeatable --gc-rule flag.
// Each value is "threshold:olderThan", e.g. "80:20d".
type gcRulesFlag []GCRule

func (f *gcRulesFlag) String() string {
	if len(*f) == 0 {
		return ""
	}

	parts := make([]string, len(*f))

	for i, r := range *f {
		parts[i] = r.String()
	}

	return strings.Join(parts, ",")
}

func (f *gcRulesFlag) Set(s string) error {
	rule, err := parseGCRule(s)
	if err != nil {
		return err
	}

	warnGCRuleShape(*f, rule)

	*f = append(*f, rule)

	return nil
}

// warnGCRuleShape warns about rules that parse but do not do what an operator
// writing them is likely to expect. Warn rather than reject: a short age is
// deliberate in the test suite, and a GC that refuses to start is worse than
// one that is surprising.
func warnGCRuleShape(existing []GCRule, rule GCRule) {
	if rule.OlderThan < minSafeOlderThan {
		slog.Warn("gc: rule keeps gcroots for less than a minute after import completes",
			"rule", rule.String(), "min_safe", formatDuration(minSafeOlderThan))
	}

	for _, e := range existing {
		higher, lower := rule, e
		if e.ThresholdPct > rule.ThresholdPct {
			higher, lower = e, rule
		}

		if higher.ThresholdPct == lower.ThresholdPct || higher.OlderThan <= lower.OlderThan {
			continue
		}

		// The fuller the disk, the more should go. A higher threshold paired
		// with a longer age means crossing it relaxes retention instead of
		// tightening it, which is what a transposed pair of rules looks like.
		slog.Warn("gc: a fuller disk selects a longer gcroot age, so GC gets less aggressive under pressure",
			"rule", higher.String(), "other", lower.String())
	}
}

var (
	errGCRuleMissingColon    = errors.New("gc-rule: expected \"threshold:duration\" e.g. \"80:20d\"")
	errGCRuleInvalidPct      = errors.New("gc-rule: threshold must be 1-100")
	errGCRuleEmptyDuration   = errors.New("gc-rule: duration must not be empty")
	errGCRuleNoRules         = errors.New("no rules configured; use --gc-rule threshold:duration")
	errGCBadDuration         = errors.New("gc: invalid duration: expected positive integer followed by d, e.g. \"30d\"")
	errGCNonPositiveDuration = errors.New("gc: duration must be positive")
	errGCDurationTooLarge    = errors.New("gc: duration is too large, at most " +
		strconv.FormatInt(maxDurationDays, 10) + "d")
	errGCNonDefaultStoreDir = errors.New("gc: --store-dir must be " + defaultStoreDir +
		"; nix-collect-garbage always collects the default store")
	errGCZeroBlocks      = errors.New("gc: statfs reports zero blocks, so disk usage is meaningless")
	errGCRootDirUnusable = errors.New("gc: --gcroot-dir is not a usable directory")
)

// parseGCRule parses a "threshold:duration" rule. The duration is parsed here,
// at flag-parse time, so a typo is rejected at startup rather than when the
// disk threshold trips — the one moment GC has to work.
func parseGCRule(s string) (GCRule, error) {
	pct, rest, ok := strings.Cut(s, ":")
	if !ok {
		return GCRule{}, fmt.Errorf("%w, got %q", errGCRuleMissingColon, s)
	}

	n, err := strconv.Atoi(pct)
	if err != nil || n < 1 || n > 100 {
		return GCRule{}, fmt.Errorf("%w, got %q in %q", errGCRuleInvalidPct, pct, s)
	}

	if rest == "" {
		return GCRule{}, fmt.Errorf("%w, in %q", errGCRuleEmptyDuration, s)
	}

	d, err := parseDurationString(rest)
	if err != nil {
		return GCRule{}, fmt.Errorf("gc-rule %q: %w", s, err)
	}

	return GCRule{ThresholdPct: n, OlderThan: d}, nil
}

// parseDurationString parses a Go duration ("2h", "30m", "1h30m") plus an
// optional "d" (days) suffix, which time.ParseDuration rejects. The NixOS
// module documents durations like "1d", so every duration this package takes
// from a flag goes through here rather than flag.Duration.
//
// On success the duration is always positive. Callers rely on that: a gcroot
// age must not put the prune cutoff in the future, and serve's --gc-interval
// goes straight into time.NewTicker, which panics on anything else.
func parseDurationString(s string) (time.Duration, error) {
	if before, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseInt(before, 10, 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%w, got %q", errGCBadDuration, s)
		}

		if n > maxDurationDays {
			return 0, fmt.Errorf("%w, got %q", errGCDurationTooLarge, s)
		}

		return time.Duration(n) * 24 * time.Hour, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}

	if d <= 0 {
		return 0, fmt.Errorf("%w, got %q", errGCNonPositiveDuration, s)
	}

	return d, nil
}

// formatDuration renders whole days as "20d" so a --gc-rule value prints back
// the way it was written; anything else uses time.Duration's own syntax.
func formatDuration(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}

	return d.String()
}

// selectRule returns the highest-threshold GCRule whose threshold <= usedPct,
// or nil if none applies. Rules whose ages do not tighten as their thresholds
// rise are warned about when they are parsed, not reordered here: which rule
// applies must stay something an operator can read off the flags.
func selectRule(rules []GCRule, usedPct int) *GCRule {
	var best *GCRule

	for i := range rules {
		if usedPct >= rules[i].ThresholdPct {
			if best == nil || rules[i].ThresholdPct > best.ThresholdPct {
				best = &rules[i]
			}
		}
	}

	return best
}

// gcMetrics holds Prometheus instruments for the GC subsystem.
// All methods are nil-safe so callers can pass nil when no registry is available.
type gcMetrics struct {
	checks      prometheus.Counter
	runs        *prometheus.CounterVec
	rootsPruned prometheus.Counter
	freedBytes  prometheus.Counter
	errors      *prometheus.CounterVec
	diskUsedPct prometheus.Gauge
	pruneDur    prometheus.Histogram
	collectDur  prometheus.Histogram
}

// newGCMetrics registers the GC instruments on reg. rules is the configured
// rule set, used only to create the runs_total children up front.
func newGCMetrics(reg *prometheus.Registry, gcrootDir string, rules []GCRule) *gcMetrics {
	m := &gcMetrics{
		checks: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tsnixcache_gc_checks_total",
			Help: "Total disk-usage checks by the GC watcher.",
		}),
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tsnixcache_gc_runs_total",
			Help: "Total GC runs, labelled by the threshold_pct that triggered them.",
		}, []string{"threshold_pct"}),
		rootsPruned: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tsnixcache_gc_roots_pruned_total",
			Help: "Total gcroot symlinks removed across all GC runs.",
		}),
		freedBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tsnixcache_gc_freed_bytes_total",
			Help: "Lower bound on bytes reclaimed by nix-collect-garbage, measured as a " +
				"whole-filesystem statfs delta per run: concurrent writes and copy-on-write " +
				"snapshots hide reclaimed space, so 0 does not mean nothing was collected.",
		}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tsnixcache_gc_errors_total",
			Help: "Total GC errors, labelled by op (statfs, prune or collect).",
		}, []string{"op"}),
		diskUsedPct: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tsnixcache_gc_disk_used_pct",
			Help: "Disk usage percentage at the last GC check.",
		}),
		pruneDur: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "tsnixcache_gc_prune_duration_seconds",
			Help: "Duration of gcroot pruning runs in seconds.",
			// Same reasoning as the collect ladder below: a prune does an Lstat
			// and a Readlink per entry, and the directory holds one entry per
			// path pushed within the rule's age, so a busy cache reaches tens of
			// thousands. Stopping at 10s would saturate p95 at exactly the point
			// the dashboard tells the operator to watch it rise.
			Buckets: []float64{.001, .005, .01, .05, .1, .5, 1, 5, 10, 30, 60, 300},
		}),
		collectDur: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "tsnixcache_gc_collect_duration_seconds",
			Help: "Duration of nix-collect-garbage runs in seconds.",
			// Up to ~1h: a collect on a large store routinely runs for tens of
			// minutes, and a ladder that stops at 300s puts every slow run in
			// +Inf, so p95 goes blind exactly when GC starts getting slow.
			Buckets: prometheus.ExponentialBuckets(0.5, 2, 13), // 0.5s … ~2048s
		}),
	}

	// Create every child up front: a counter that has never fired must still be
	// scrapeable as 0, or the dashboard shows "no data" for a healthy server and
	// cannot distinguish it from a server whose watcher is wedged.
	for _, op := range []string{"statfs", "prune", "collect"} {
		m.errors.WithLabelValues(op)
	}

	for _, r := range rules {
		m.runs.WithLabelValues(strconv.Itoa(r.ThresholdPct))
	}

	reg.MustRegister(
		m.checks,
		m.runs,
		m.rootsPruned,
		m.freedBytes,
		m.errors,
		m.diskUsedPct,
		m.pruneDur,
		m.collectDur,
		newGCRootsCollector(gcrootDir),
	)

	return m
}

// gcRootsCollector is a prometheus.Collector that reports the current number
// of entries in the gcroot directory on each scrape.
type gcRootsCollector struct {
	gcrootDir string
	desc      *prometheus.Desc
}

func newGCRootsCollector(gcrootDir string) *gcRootsCollector {
	return &gcRootsCollector{
		gcrootDir: gcrootDir,
		desc: prometheus.NewDesc(
			"tsnixcache_gc_roots_current",
			"Current number of entries in the gcroot directory.",
			nil, nil,
		),
	}
}

func (c *gcRootsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *gcRootsCollector) Collect(ch chan<- prometheus.Metric) {
	entries, err := os.ReadDir(c.gcrootDir)

	// Before the first push niximport has not created the directory, and no
	// directory really is no roots — report it rather than dropping the series
	// and leaving a gap nobody can tell from a scrape failure. Any other error
	// (an unreadable directory, say) also stops the prune, which reports it on
	// errors_total{op="prune"}; reporting it here as well would have to be an
	// invalid metric, which fails the whole /metrics scrape.
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Debug("gc: read gcroot dir for roots_current", "dir", c.gcrootDir, "err", err)

		return
	}

	var count int

	for _, entry := range entries {
		if isGCRootName(entry.Name()) && entry.Type()&os.ModeSymlink != 0 {
			count++
		}
	}

	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(count))
}

// pointsIntoStore reports whether a symlink target names a path inside storeDir.
// The target is compared as written: a gcroot whose store path has already been
// collected still counts, and a relative target (which nothing here writes) is
// refused rather than resolved.
func pointsIntoStore(target, storeDir string) bool {
	store := filepath.Clean(storeDir)
	target = filepath.Clean(target)

	return target == store || strings.HasPrefix(target, store+string(filepath.Separator))
}

// isGCRootName reports whether name is one addGCRoot writes: the 32-character
// nix-base32 hash part of a store path, optionally with the ".tmp.<pid>.<seq>"
// suffix an interrupted import can leave behind.
func isGCRootName(name string) bool {
	hash, _, _ := strings.Cut(name, ".")

	return nixbase32.ValidHashPart(hash)
}

// staleGCRoot is a gcroot old enough to remove.
type staleGCRoot struct {
	path string
	age  time.Duration
}

// pruneResult counts what one pass over the gcroot directory did.
type pruneResult struct {
	pruned int // gcroots removed, or in dry-run mode that would have been
	failed int // gcroots that matched but could not be removed
}

// pruneOldGCRoots removes the gcroots in gcrootDir whose mtime is older than
// minAge, or reports what it would remove when dryRun is set.
//
// This function deletes what it matches and nix-collect-garbage then reaps
// whatever it unrooted, so the match has to be exact: an operator can point
// gcrootDir at a directory whose entries keep the running system alive.
// selectStaleGCRoots holds the filters that make it so.
func pruneOldGCRoots(gcrootDir, storeDir string, minAge time.Duration, dryRun bool) (pruneResult, error) {
	entries, err := os.ReadDir(gcrootDir)

	// serve starts before the first push and niximport creates the directory
	// with the first root it writes, so a missing one here means nothing to
	// prune. The gc subcommand checks the directory itself, where a missing
	// one means a --gcroot-dir that does not match serve's.
	if errors.Is(err, os.ErrNotExist) {
		return pruneResult{}, nil
	}

	if err != nil {
		return pruneResult{}, fmt.Errorf("gc: read gcroot dir %s: %w", gcrootDir, err)
	}

	cutoff := time.Now().Add(-minAge)
	stale := selectStaleGCRoots(gcrootDir, storeDir, cutoff, entries)

	var (
		res      pruneResult
		pruneErr error
	)

	for _, root := range stale {
		age := root.age.Round(time.Hour)

		if dryRun {
			slog.Info("gc: would prune gcroot", "path", root.path, "age", age)

			res.pruned++

			continue
		}

		removed, removeErr := pruneLockedRoot(gcrootDir, storeDir, root.path, cutoff)
		if removeErr != nil {
			slog.Warn("gc: remove gcroot", "path", root.path, "err", removeErr)

			res.failed++
			pruneErr = errors.Join(pruneErr, removeErr)

			continue
		}

		if !removed {
			continue
		}

		slog.Info("gc: pruned gcroot", "path", root.path, "age", age)

		res.pruned++
	}

	// Counted and reported, not just logged per entry: a gcroot dir that has
	// lost write permission otherwise prunes nothing on every tick while the
	// collection still runs, with errors_total flat at zero.
	if res.failed > 0 {
		slog.Error("gc: could not remove gcroots", "count", res.failed, "gcroot_dir", gcrootDir)
	}

	return res, pruneErr
}

// Revalidate under the same ownership lock used through import and rollback.
func pruneLockedRoot(dir, storeDir, path string, cutoff time.Time) (bool, error) {
	key, _, _ := strings.Cut(filepath.Base(path), ".")

	lock, err := gcroot.TryAcquire(dir, key)
	if errors.Is(err, gcroot.ErrBusy) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	defer lock.Close()

	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	selected := selectStaleGCRoots(dir, storeDir, cutoff, []os.DirEntry{fs.FileInfoToDirEntry(fi)})
	if len(selected) == 0 {
		return false, nil
	}

	err = os.Remove(path)
	if err != nil {
		return false, err
	}

	err = gcroot.SyncDir(dir)

	return err == nil, err
}

// selectStaleGCRoots returns the entries that are tsnixcache gcroots into
// storeDir with an mtime at or before cutoff. Everything it leaves alone it
// summarises in one log line per run.
//
// Both filters are load-bearing. The name must be one addGCRoot writes, which
// excludes nix's own system-N-link, booted-system and current-system profiles:
// they are direct store symlinks whose mtime is the deploy date, so the target
// check alone would unlink the running system's root. The target must point
// into the store, which excludes /nix/var/nix/gcroots/auto/*: those are
// hash-named but point back out at an indirect root.
func selectStaleGCRoots(
	gcrootDir, storeDir string, cutoff time.Time, entries []os.DirEntry,
) []staleGCRoot {
	var (
		stale       []staleGCRoot
		kept        int
		keptExample string
		skewed      int
		linkErr     error
		linkErrPath string
	)

	keep := func(path string) {
		kept++

		if keptExample == "" {
			keptExample = path
		}
	}

	for _, e := range entries {
		path := filepath.Join(gcrootDir, e.Name())

		if e.Name() == gcroot.LockDir {
			continue
		}

		if !isGCRootName(e.Name()) {
			slog.Debug("gc: keeping entry that tsnixcache did not write", "path", path)

			keep(path)

			continue
		}

		fi, statErr := os.Lstat(path)
		if statErr != nil {
			continue
		}

		mtime := fi.ModTime()

		// A clock that is behind the mtimes lands here and prunes nothing,
		// which is the safe direction.
		if mtime.After(cutoff) {
			continue // too young; keep it
		}

		if mtime.Before(gcRootEpoch) {
			skewed++

			continue
		}

		if fi.Mode()&os.ModeSymlink == 0 {
			slog.Debug("gc: keeping non-symlink in gcroot dir", "path", path)

			keep(path)

			continue
		}

		target, readErr := os.Readlink(path)
		if readErr != nil {
			// Not a foreign entry but an I/O error: the Lstat above ran on this
			// same path and said "symlink", so every path-level failure (ENOENT,
			// EACCES, ELOOP, ENAMETOOLONG) has already been absorbed by the
			// continue above. What is left is EIO, ESTALE, or another process
			// replacing the entry mid-run. Reported below with its path and error
			// rather than folded into the kept summary, which carries neither.
			if linkErr == nil {
				linkErr, linkErrPath = readErr, path
			}

			keep(path)

			continue
		}

		if !pointsIntoStore(target, storeDir) {
			slog.Debug("gc: keeping gcroot pointing outside the store",
				"path", path, "target", target, "store_dir", storeDir)

			keep(path)

			continue
		}

		stale = append(stale, staleGCRoot{path: path, age: time.Since(mtime)})
	}

	// One line per run, not one per entry: this runs every --gc-interval, and a
	// gcroot dir full of foreign entries would otherwise dominate the log. It
	// still has to be a warning — if every entry is kept, GC reclaims nothing,
	// which most often means --gcroot-dir or --store-dir does not match what
	// the imports write.
	if kept > 0 {
		slog.Warn("gc: kept entries that are not tsnixcache gcroots into the store",
			"count", kept, "example", keptExample, "store_dir", storeDir, "stale", len(stale))
	}

	// Once per run for the same reason, but separate from the summary: a foreign
	// symlink is expected, a symlink that cannot be read is not, and only this
	// line carries the error.
	if linkErr != nil {
		slog.Warn("gc: read gcroot symlink", "path", linkErrPath, "err", linkErr)
	}

	if skewed > 0 {
		slog.Warn("gc: kept gcroots whose mtime predates the project, so the clock has jumped forward",
			"count", skewed, "epoch", gcRootEpoch.Format(time.DateOnly))
	}

	return stale
}

// diskUsedPct shares the dashboard's occupied-block fraction, (Blocks-Bfree)/Blocks.
// Bfree keeps reserved blocks free so GC thresholds agree with the disk gauges.
func diskUsedPct(storeDir string) (int, error) {
	var st syscall.Statfs_t

	err := syscall.Statfs(storeDir, &st)
	if err != nil {
		return 0, fmt.Errorf("gc: statfs %s: %w", storeDir, err)
	}

	// Pseudo-filesystems can report no blocks; reject them before division.
	if st.Blocks == 0 {
		return 0, fmt.Errorf("%w: %s", errGCZeroBlocks, storeDir)
	}

	return int(100 * (st.Blocks - st.Bfree) / st.Blocks), nil // #nosec G115 -- 0-100 fits
}

// runCollectGarbage runs nix-collect-garbage and returns how many bytes the
// filesystem gained across the run. m may be nil.
func runCollectGarbage(ctx context.Context, storeDir string, dryRun bool, m *gcMetrics) (uint64, error) {
	var beforeSt syscall.Statfs_t

	haveBefore := syscall.Statfs(storeDir, &beforeSt) == nil

	var args []string
	if dryRun {
		args = append(args, "--dry-run")
	}

	collectStart := time.Now()
	cmd := exec.CommandContext(ctx, "nix-collect-garbage", args...) // #nosec G204
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	runErr := cmd.Run()

	if m != nil {
		m.collectDur.Observe(time.Since(collectStart).Seconds())
	}

	if runErr != nil {
		if m != nil {
			m.errors.WithLabelValues("collect").Inc()
		}

		return 0, fmt.Errorf("gc: nix-collect-garbage: %w", runErr)
	}

	var freed uint64

	if haveBefore {
		var afterSt syscall.Statfs_t

		if syscall.Statfs(storeDir, &afterSt) == nil && afterSt.Bavail > beforeSt.Bavail {
			bsize := uint64(afterSt.Bsize) // #nosec G115
			freed = (afterSt.Bavail - beforeSt.Bavail) * bsize
		}
	}

	if m != nil {
		m.freedBytes.Add(float64(freed))
	}

	return freed, nil
}

// gcRunner applies the GC rules. It carries state rather than being a plain
// function because deciding whether to collect needs to know what the previous
// collection achieved.
type gcRunner struct {
	storeDir  string
	gcrootDir string
	rules     []GCRule
	dryRun    bool
	metrics   *gcMetrics // may be nil

	// cooldown is the shortest gap between two collections that have nothing
	// new to reap; zero collects on every applicable check.
	cooldown time.Duration

	lastCollect time.Time
	lastFreed   uint64
}

// apply checks disk usage at storeDir, selects the most aggressive applicable
// rule, prunes the gcroots it makes eligible and then runs
// nix-collect-garbage. Returns true if a collection ran.
func (g *gcRunner) apply(ctx context.Context) (bool, error) {
	usedPct, err := diskUsedPct(g.storeDir)

	if g.metrics != nil {
		g.metrics.checks.Inc()
	}

	if err != nil {
		// Counted: without this the gauge simply freezes at its last good
		// value and a store that cannot be measured looks like one that is
		// comfortably empty.
		if g.metrics != nil {
			g.metrics.errors.WithLabelValues("statfs").Inc()
		}

		return false, err
	}

	if g.metrics != nil {
		g.metrics.diskUsedPct.Set(float64(usedPct))
	}

	rule := selectRule(g.rules, usedPct)
	if rule == nil {
		slog.Debug("gc: no rule triggered", "used_pct", usedPct)

		return false, nil
	}

	if g.metrics != nil {
		g.metrics.runs.WithLabelValues(strconv.Itoa(rule.ThresholdPct)).Inc()
	}

	slog.Info(
		"gc: pruning old gcroots",
		"used_pct", usedPct,
		"threshold_pct", rule.ThresholdPct,
		"min_age", formatDuration(rule.OlderThan),
		"gcroot_dir", g.gcrootDir,
		"dry_run", g.dryRun,
	)

	pruneStart := time.Now()
	res, pruneErr := pruneOldGCRoots(g.gcrootDir, g.storeDir, rule.OlderThan, g.dryRun)

	if g.metrics != nil {
		g.metrics.pruneDur.Observe(time.Since(pruneStart).Seconds())
		g.metrics.rootsPruned.Add(float64(res.pruned))
		g.metrics.errors.WithLabelValues("prune").Add(float64(res.failed))
	}

	if pruneErr != nil {
		if g.metrics != nil {
			g.metrics.errors.WithLabelValues("prune").Inc()
		}

		return false, pruneErr
	}

	if g.skipCollect(res.pruned) {
		return false, nil
	}

	slog.Info("gc: running nix-collect-garbage", "pruned_roots", res.pruned, "dry_run", g.dryRun)

	freed, collectErr := runCollectGarbage(ctx, g.storeDir, g.dryRun, g.metrics)
	g.lastCollect = time.Now()
	g.lastFreed = freed

	if collectErr != nil {
		return false, collectErr
	}

	return true, nil
}

// skipCollect reports whether this check should leave nix-collect-garbage
// alone. A collection that unrooted nothing and freed nothing last time will
// free nothing now either, but it still takes nix's global GC lock and walks
// the whole store, so every push and every local build blocks behind it. Disk
// usage above the threshold does not fall on its own, so without this a store
// that is 82% full with everything younger than the rule's age would collect
// on every tick forever.
func (g *gcRunner) skipCollect(pruned int) bool {
	if pruned > 0 || g.lastCollect.IsZero() {
		return false
	}

	since := time.Since(g.lastCollect)
	if g.lastFreed > 0 || since >= g.cooldown {
		return false
	}

	slog.Info("gc: skipping nix-collect-garbage, nothing was pruned and the last collection freed nothing",
		"since_last_collect", since.Round(time.Second), "cooldown", g.cooldown)

	return true
}

// runGCWatcher periodically applies GC rules until ctx is cancelled. m may be nil.
func runGCWatcher(
	ctx context.Context, storeDir, gcrootDir string, rules []GCRule, interval time.Duration, m *gcMetrics,
) {
	runner := &gcRunner{
		storeDir:  storeDir,
		gcrootDir: gcrootDir,
		rules:     rules,
		metrics:   m,
		// Long enough that a store sitting over the threshold with nothing to
		// reap stops holding the GC lock every few minutes, short enough that
		// a real backlog is picked up within the hour.
		cooldown: max(interval, collectCooldownFloor),
	}

	check := func() {
		_, err := runner.apply(ctx)
		if err != nil {
			slog.Error("gc: apply rules failed", "err", err)
		}
	}

	// Before the ticker, not after it: a server restarting at 99% full must
	// not wait a whole --gc-interval — a day, if that is what it is set to —
	// for its first check.
	check()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

func newGCCmd() *ff.Command {
	fs := ff.NewFlagSet("tsnixcache gc")

	var rules gcRulesFlag

	fs.Value(0, "gc-rule", &rules, `GC rule as "threshold:duration" e.g. "80:20d"; repeatable`)

	storeDir := fs.StringLong("store-dir", defaultStoreDir,
		"path to Nix store directory (only "+defaultStoreDir+"; nix-collect-garbage collects no other store)")
	gcrootDir := fs.StringLong("gcroot-dir", "/nix/var/nix/gcroots/tsnixcache", "directory for GC root symlinks")
	dryRun := fs.BoolLongDefault("dry-run", false,
		"delete nothing: log the gcroots that would be pruned and pass --dry-run to nix-collect-garbage")

	return &ff.Command{
		Name: "gc",
		Usage: "tsnixcache gc --gc-rule threshold:duration [--gc-rule ...] " +
			"[--store-dir path] [--gcroot-dir path] [--dry-run]",
		ShortHelp: "Prune old gcroot symlinks and run nix-collect-garbage based on disk-usage thresholds.",
		Flags:     fs,
		Exec: func(ctx context.Context, _ []string) error {
			if len(rules) == 0 {
				return errGCRuleNoRules
			}

			// The threshold is measured on --store-dir and the gcroots are
			// pruned for it, but nix-collect-garbage takes no store argument
			// and always collects the default store. Refuse rather than prune
			// one store's roots and collect another's, which is what serve
			// does for --store (errStoreGCConflict).
			if filepath.Clean(*storeDir) != defaultStoreDir {
				return fmt.Errorf("%w, got %q", errGCNonDefaultStoreDir, *storeDir)
			}

			// Run from cron against a --gcroot-dir that does not match the
			// server's, this would prune nothing at all and still collect the
			// real store. Silence is the wrong answer to that.
			err := checkGCRootDir(*gcrootDir)
			if err != nil {
				return err
			}

			runner := &gcRunner{
				storeDir:  *storeDir,
				gcrootDir: *gcrootDir,
				rules:     []GCRule(rules),
				dryRun:    *dryRun,
			}

			ran, err := runner.apply(ctx)
			if err != nil {
				return err
			}

			if !ran {
				slog.Info("gc: no rule triggered, disk usage is below all thresholds")
			}

			return nil
		},
	}
}

// checkGCRootDir fails unless dir exists and is a directory.
func checkGCRootDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("%w: %w (does it match the server's --gcroot-dir?)", errGCRootDirUnusable, err)
	}

	if !fi.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", errGCRootDirUnusable, dir)
	}

	return nil
}
