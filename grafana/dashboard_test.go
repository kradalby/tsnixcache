// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

package grafana

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Validate the provisioned model and its registered metrics.
func TestBuildDashboard(t *testing.T) {
	d, err := buildDashboard()
	if err != nil {
		require.FailNowf(t, "dashboard invariant", "buildDashboard: %v", err)
	}

	if d.Uid == nil || *d.Uid != "tsnixcache" {
		require.FailNowf(t, "dashboard invariant", "dashboard uid = %v, want \"tsnixcache\": provisioning files it by uid", d.Uid)
	}

	// Long enough to say what the panel measures and what to do about it; every
	// shipped description is several times this, so it only catches a stub.
	const minDescription = 120

	for _, p := range dashboardPanels(t) {
		if len(p.Targets) == 0 {
			require.FailNowf(t, "dashboard invariant", "panel %q has no targets, so it renders nothing", p.Title)
		}

		if len(p.Description) < minDescription {
			require.FailNowf(t, "dashboard invariant", "panel %q has a %d-character description; it needs to say what the panel "+
				"measures and what to do when it looks wrong: %q",
				p.Title, len(p.Description), p.Description)
		}
	}
}

// TestRowsGroupPanels: rows are how the dashboard is navigated, so every panel
// has to be under one — a panel emitted before the first row builder renders
// above the headings, outside the structure, and is the first thing an operator
// sees with no context for it.
func TestRowsGroupPanels(t *testing.T) {
	for _, p := range dashboardPanels(t) {
		if p.row == "" {
			require.FailNowf(t, "dashboard invariant", "panel %q sits above the first row, so it has no heading to explain it", p.Title)
		}
	}
}

// panel is the slice of the generated dashboard JSON these tests care about:
// what a panel is called, what unit it claims, and what it queries. Reading it
// back out of the marshalled JSON checks exactly what Grafana is handed.
type panel struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Targets     []struct {
		Expr    string `json:"expr"`
		Instant bool   `json:"instant"`
		Range   bool   `json:"range"`
	} `json:"targets"`
	FieldConfig struct {
		Defaults struct {
			Unit     string `json:"unit"`
			NoValue  string `json:"noValue"`
			Mappings []struct {
				Type    string `json:"type"`
				Options struct {
					Match  string `json:"match"`
					Result struct {
						Text  string `json:"text"`
						Color string `json:"color"`
					} `json:"result"`
				} `json:"options"`
			} `json:"mappings"`
		} `json:"defaults"`
	} `json:"fieldConfig"`

	Options struct {
		GraphMode     string `json:"graphMode"`
		ReduceOptions struct {
			Calcs []string `json:"calcs"`
		} `json:"reduceOptions"`
	} `json:"options"`

	row string // heading of the row this panel sits under
}

// dashboardPanels returns the generated panels, each tagged with its row. Rows
// are themselves panels of type "row"; the panels that follow one belong to it.
func dashboardPanels(t *testing.T) []panel {
	t.Helper()

	d, err := buildDashboard()
	if err != nil {
		require.FailNowf(t, "dashboard invariant", "buildDashboard: %v", err)
	}

	raw, err := json.Marshal(d)
	if err != nil {
		require.FailNowf(t, "dashboard invariant", "marshal dashboard: %v", err)
	}

	var model struct {
		Panels []panel `json:"panels"`
	}

	err = json.Unmarshal(raw, &model)
	if err != nil {
		require.FailNowf(t, "dashboard invariant", "unmarshal dashboard: %v", err)
	}

	var (
		out []panel
		row string
	)

	for _, p := range model.Panels {
		if p.Type == "row" {
			row = p.Title

			continue
		}

		p.row = row
		out = append(out, p)
	}

	if len(out) == 0 {
		require.FailNow(t, "no non-row panels in dashboard")
	}

	return out
}

var (
	// Grouping clauses hold label names, not metric names: `sum by (op) (...)`
	// must not be read as a query for a metric called "op". Stripped before the
	// rest, so the keyword and its label list go together.
	promGroupingRe = regexp.MustCompile(`\b(?:by|without|on|ignoring)\s*\([^)]*\)`)
	// Range selectors, label matchers and string literals hold no metric names.
	promNoiseRe = regexp.MustCompile(`\[[^\]]*\]|\{[^}]*\}|"[^"]*"`)
	promIdentRe = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)
)

// promKeywords are PromQL words that look like identifiers but name no metric:
// the matching keywords and the aggregation operators, which are not caught by
// the "followed by (" rule when they take a modifier, as in `sum by (le) (...)`.
// Ordinary functions need no listing.
var promKeywords = map[string]bool{
	"group_left": true, "group_right": true,
	"and": true, "or": true, "unless": true, "offset": true, "bool": true,
	"sum": true, "min": true, "max": true, "avg": true, "group": true,
	"count": true, "count_values": true, "stddev": true, "stdvar": true,
	"topk": true, "bottomk": true, "quantile": true,
}

// metricsIn extracts the metric names a PromQL expression selects.
func metricsIn(expr string) []string {
	clean := promNoiseRe.ReplaceAllString(promGroupingRe.ReplaceAllString(expr, " "), " ")

	var out []string

	for _, loc := range promIdentRe.FindAllStringIndex(clean, -1) {
		name := clean[loc[0]:loc[1]]
		if promKeywords[name] {
			continue
		}
		// A call, not a selector.
		if loc[1] < len(clean) && clean[loc[1]] == '(' {
			continue
		}

		out = append(out, name)
	}

	return out
}

// metricNameRe matches the metric-name string literals in the server sources.
var metricNameRe = regexp.MustCompile(`"tsnixcache_[a-z0-9_]+"`)

// declaredMetrics scans Go sources for the metric names they declare. Every
// "tsnixcache_…" string literal in the server packages is a metric name (a
// CounterOpts/GaugeOpts/HistogramOpts Name or a prometheus.NewDesc fqName), so
// grepping beats a hand-written list: the list cannot drift out of date.
// Paths are files or directories; directories are walked recursively and
// _test.go files are ignored.
func declaredMetrics(t *testing.T, paths ...string) map[string]bool {
	t.Helper()

	out := map[string]bool{}

	for _, root := range paths {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if d.IsDir() {
				// Dotdirs, the nix build symlink and vendor/ hold no tsnixcache
				// sources — buildGoModule materialises vendor/ before running
				// this check, and walking its ~4700 files costs seconds — and
				// this package must be skipped: scanning the dashboard's own
				// PromQL would let a panel certify its own metric name. The root
				// itself is exempt — it is spelled "..", which reads as a
				// dotdir.
				if path != root &&
					(strings.HasPrefix(d.Name(), ".") || d.Name() == "result" ||
						d.Name() == "vendor" || path == dashboardDir) {
					return filepath.SkipDir
				}

				return nil
			}

			if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}

			src, err := os.ReadFile(path) //nolint:gosec // reads this repo's own sources
			if err != nil {
				return err
			}

			for _, lit := range metricNameRe.FindAllString(string(src), -1) {
				out[strings.Trim(lit, `"`)] = true
			}

			return nil
		})
		if err != nil {
			require.FailNowf(t, "dashboard invariant", "scan %s: %v", root, err)
		}
	}

	if len(out) == 0 {
		require.FailNowf(t, "dashboard invariant", "no metric names found in %v — the scan is broken", paths)
	}

	return out
}

// Source of truth for what the server actually exports. Metrics are declared
// wherever they are used — cache/, auth/, cli/ — so the scan starts
// at the repo root rather than a list of packages that goes stale the moment
// someone adds one. gcFile's metrics only exist once GC rules are configured
// (newGCMetrics), so a panel using them is blank on a default deployment.
const (
	repoRoot     = ".."
	dashboardDir = "../grafana"
	gcFile       = "../cli/gc.go"
	cacheFile    = "../cache/cache.go"
)

// base strips the derived-series suffixes Prometheus generates for a histogram,
// which are queried but never declared.
func base(metric string) string {
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if trimmed, ok := strings.CutSuffix(metric, suffix); ok {
			return trimmed
		}
	}

	return metric
}

// TestPanelsQueryRegisteredMetrics is the guard that matters: a panel querying a
// metric nobody registers renders empty forever and nobody notices. Cross-check
// every metric name in the generated PromQL against the names the server code
// declares.
func TestPanelsQueryRegisteredMetrics(t *testing.T) {
	declared := declaredMetrics(t, repoRoot)

	var seen int

	for _, p := range dashboardPanels(t) {
		for _, target := range p.Targets {
			for _, m := range metricsIn(target.Expr) {
				seen++

				if m == "up" && (p.Title == "Store disk used" || p.Title == "Store paths") {
					continue
				}

				if !declared[m] && !declared[base(m)] {
					require.FailNowf(t, "dashboard invariant", "panel %q queries %q, which no package registers\nexpr: %s",
						p.Title, m, target.Expr)
				}
			}
		}
	}

	// Guard against a parser that silently matches nothing.
	if seen < len(declared)/2 {
		require.FailNowf(t, "dashboard invariant", "only %d metric references extracted from the dashboard; extraction is broken", seen)
	}
}

// sourceContains reports whether path's text contains sub. It lets this package
// check the arithmetic used by the exporter and GC.
func sourceContains(t *testing.T, path, sub string) bool {
	t.Helper()

	src, err := os.ReadFile(path) //nolint:gosec // reads this repo's own sources
	if err != nil {
		require.FailNowf(t, "dashboard invariant", "read %s: %v", path, err)
	}

	return strings.Contains(string(src), sub)
}

// GC and the dashboard must use the same definition of occupied blocks.
func TestDiskTileTracksGCThreshold(t *testing.T) {
	// The pairing the tile's expression is chosen from: gc.go's formula, and the
	// gauge in cache.go carrying the same statfs field.
	var (
		gcUsesBfree  = sourceContains(t, gcFile, "(st.Blocks - st.Bfree) / st.Blocks")
		usedIsBfree  = sourceContains(t, cacheFile, "c.diskUsed, prometheus.GaugeValue, float64((st.Blocks-st.Bfree)")
		availIsBavai = sourceContains(t, cacheFile, "c.diskAvail, prometheus.GaugeValue, float64(st.Bavail")
	)

	// collectDisk emits the gauges through a shared helper, so the descs above
	// are named at the call site rather than in the arithmetic; fall back to the
	// helper's own form.
	if !usedIsBfree {
		usedIsBfree = sourceContains(t, cacheFile, "used, prometheus.GaugeValue, float64((st.Blocks-st.Bfree)")
	}

	if !availIsBavai {
		availIsBavai = sourceContains(t, cacheFile, "avail, prometheus.GaugeValue, float64(st.Bavail")
	}

	if !usedIsBfree || !availIsBavai {
		require.FailNowf(t, "dashboard invariant", "cannot tell which statfs field each disk gauge carries (used=%v avail=%v); "+
			"the tile's calibration against GC is unverifiable", usedIsBfree, availIsBavai)
	}

	if !gcUsesBfree {
		require.FailNowf(t, "dashboard invariant", "gc.go no longer derives its high-water percentage from (Blocks-Bfree)/Blocks; "+
			"the Overview disk tile in %s is calibrated against that formula and must move with it", dashboardDir)
	}

	// Naming the right gauge is not enough: available/total is the percentage
	// *free*, which reads 10%% on a disk that is 90%% full and inverts the 75/90
	// colour thresholds. Pin the whole fraction.
	usedFraction := liveGauge("tsnixcache_store_disk_used_bytes") + " / " + liveGauge("tsnixcache_store_disk_total_bytes")

	var found bool

	for _, p := range dashboardPanels(t) {
		if p.row != rowOverview || !strings.Contains(strings.ToLower(p.Title), "disk") ||
			p.FieldConfig.Defaults.Unit != "percent" {
			continue
		}

		found = true

		for _, target := range p.Targets {
			if !strings.Contains(strings.Join(strings.Fields(target.Expr), " "), usedFraction) {
				require.FailNowf(t, "dashboard invariant", "panel %q is a disk percentage but does not compute %s, so it does not plot the used fraction GC prunes on\nexpr: %s",
					p.Title, usedFraction, target.Expr)
			}
		}
	}

	if !found {
		require.FailNow(t, "no percentage disk tile in the Overview row")
	}
}

// TestSpoolDiskPanelMatchesStorePanel: the spool filesystem is the one that
// fills first — a pushed NAR sits there whole before it is imported — so it gets
// a panel of its own, and that panel has to be readable against the store panel
// beside it. Two ways it could quietly stop being: plotting the available gauge
// (Bavail, which excludes the root reserve) where the store plots the used one,
// or claiming a rate unit for a plain byte gauge. Both render plausibly and lie.
func TestSpoolDiskPanelMatchesStorePanel(t *testing.T) {
	// The store and spool gauges agree only because one helper fills both; if
	// they stop sharing it, this test can no longer claim the panels are
	// comparable and says so rather than passing.
	storeShared := sourceContains(t, cacheFile, "collectDisk(ch, c.storeDir, c.diskTotal, c.diskUsed, c.diskAvail)")
	spoolShared := sourceContains(t, cacheFile, "collectDisk(ch, c.spoolDir, c.spoolTotal, c.spoolUsed, c.spoolAvail)")

	if !storeShared || !spoolShared {
		require.FailNowf(t, "dashboard invariant", "store and spool disk gauges no longer come from the same collectDisk helper "+
			"(store=%v spool=%v); the two disk panels may be measuring different statfs fields",
			storeShared, spoolShared)
	}

	var found int

	for _, p := range dashboardPanels(t) {
		var spool bool

		for _, target := range p.Targets {
			for _, m := range metricsIn(target.Expr) {
				if strings.HasPrefix(m, "tsnixcache_spool_disk_") {
					spool = true
				}

				if m == "tsnixcache_spool_disk_available_bytes" {
					require.FailNowf(t, "dashboard invariant", "panel %q plots %q, which is Bavail; the store panel plots used bytes (Blocks-Bfree), "+
						"so the two disk panels would not be on the same scale", p.Title, m)
				}
			}
		}

		if !spool {
			continue
		}

		found++

		// "bytes" (BytesIEC), as the store panel uses: these are byte gauges, not
		// a rate, and Bps would silently divide by the scrape interval.
		if got := p.FieldConfig.Defaults.Unit; got != "bytes" {
			require.FailNowf(t, "dashboard invariant", "spool panel %q has unit %q, want \"bytes\": it plots byte gauges, not a rate", p.Title, got)
		}

		for _, want := range []string{"tsnixcache_spool_disk_used_bytes", "tsnixcache_spool_disk_total_bytes"} {
			var plotted bool

			for _, target := range p.Targets {
				if strings.Contains(target.Expr, want) {
					plotted = true
				}
			}

			if !plotted {
				require.FailNowf(t, "dashboard invariant", "spool panel %q does not plot %s, so the headroom the spool has left is not readable",
					p.Title, want)
			}
		}
	}

	if found == 0 {
		require.FailNow(t, "no panel plots the spool filesystem, so the disk that fills first is invisible")
	}
}

// TestOverviewAvoidsOptionalMetrics keeps the headline tiles honest: the
// Overview row must work on a default deployment, so it may not depend on
// metrics that only exist when GC rules are configured.
func TestOverviewAvoidsOptionalMetrics(t *testing.T) {
	optional := declaredMetrics(t, gcFile)

	for _, p := range dashboardPanels(t) {
		if p.row != rowOverview {
			continue
		}

		for _, target := range p.Targets {
			for _, m := range metricsIn(target.Expr) {
				if optional[base(m)] {
					require.FailNowf(t, "dashboard invariant", "Overview panel %q queries %q, which is only registered when GC rules are configured",
						p.Title, m)
				}
			}
		}
	}
}

// byteUnits are the Grafana unit ids that render a value as bytes, and
// secondUnits those that render it as a duration. A panel claiming one of them
// makes a checkable promise about the metric family underneath: the axis label,
// the tooltip and the automatic scaling (KiB/MiB, ms/m/h) are all derived from
// it, so a mismatched unit does not look broken — it looks like a plausible
// number of the wrong kind.
var (
	byteUnits = map[string]bool{
		"bytes": true, "decbytes": true, "Bps": true, "binBps": true,
	}
	secondUnits = map[string]bool{
		"s": true, "dtdurations": true,
	}
)

// TestPanelUnitsMatchQueriedMetrics catches a panel that labels unrelated series
// with one axis unit — a bytes/sec rate and a run count both drawn as bytes, or
// a duration panel repointed at a counter so "12" is rendered as 12 seconds.
// Grafana will not complain about either: it formats whatever it is given.
//
// Only the units that name a physical dimension are checkable this way. percent
// and short say nothing about the metric family (the hit ratio is a percentage
// built from two request counters), so they are left to the panel-specific tests
// below rather than asserted loosely here.
func TestPanelUnitsMatchQueriedMetrics(t *testing.T) {
	for _, p := range dashboardPanels(t) {
		unit := p.FieldConfig.Defaults.Unit

		var wantSuffix string

		switch {
		case byteUnits[unit]:
			wantSuffix = "_bytes"
		case secondUnits[unit]:
			wantSuffix = "_seconds"
		default:
			continue
		}

		for _, target := range p.Targets {
			for _, m := range metricsIn(target.Expr) {
				if !strings.Contains(base(m), wantSuffix) {
					require.FailNowf(t, "dashboard invariant", "panel %q renders %q as unit %q, but its name does not carry %q",
						p.Title, m, unit, wantSuffix)
				}
			}
		}
	}
}

// unplottedGauges are exported metrics that no panel may query, and the reason
// each is excluded. Both are statfs Bavail, which excludes the root reserve,
// while every disk panel and GC's own high-water rule work in used bytes
// (Blocks-Bfree) over total. Plotting a Bavail series beside them would put two
// irreconcilable definitions of "free" on one axis — which is precisely the
// shape of the drift that once had the Overview tile reading ~5 points high and
// red while GC still saw headroom.
//
// This is a deliberate editorial decision recorded in dashboard.go, not an
// oversight waiting to be tidied up: used-vs-total already answers "how much
// headroom is left", because the gap between those two lines is that headroom.
// /health serves the one question that genuinely wants Bavail — whether the next
// NAR fits on this server right now.
var unplottedGauges = map[string]string{
	"tsnixcache_store_disk_available_bytes": "Bavail excludes the root reserve; " +
		"the store panels and GC both work in used bytes over total",
	"tsnixcache_spool_disk_available_bytes": "Bavail excludes the root reserve; " +
		"the spool panel plots used vs total to stay on the store panel's scale",
}

// TestPanelsAvoidUnplottedGauges keeps the decision above enforced rather than
// merely written down. Adding one of these gauges to a panel renders perfectly
// and reads as an improvement, which is why only a test catches it.
func TestPanelsAvoidUnplottedGauges(t *testing.T) {
	// A metric that stopped existing would make this test pass vacuously, so
	// check the exclusion list still names real metrics.
	declared := declaredMetrics(t, repoRoot)

	for m := range unplottedGauges {
		if !declared[m] {
			require.FailNowf(t, "dashboard invariant", "%q is on the do-not-plot list but no package declares it; "+
				"either the metric was renamed or the list is stale", m)
		}
	}

	for _, p := range dashboardPanels(t) {
		for _, target := range p.Targets {
			for _, m := range metricsIn(target.Expr) {
				if why, excluded := unplottedGauges[m]; excluded {
					require.FailNowf(t, "dashboard invariant", "panel %q plots %q, which is deliberately not plotted: %s\nexpr: %s",
						p.Title, m, why, target.Expr)
				}
			}
		}
	}
}

// TestHitRatioDenominatorIsReal pins every shipped ratio panel to
// hits/(hits+misses). Flooring the denominator (clamp_min, or an `or vector(1)`
// fallback) turns the panel into a plot of the numerator whenever traffic is
// below the floor, which reads as a plausible-looking but invented ratio on any
// quiet cache. It reads the generated panels rather than hitRatioExpr, because
// nothing obliges a panel to use that constant: inlining the fudged expression
// into one of them ships the defect with the constant left pristine.
func TestHitRatioDenominatorIsReal(t *testing.T) {
	// Spelled out rather than built from the package's own constants, so a
	// change to those does not quietly move the goalposts.
	// method="get" is part of the pin, not incidental: a push HEADs every path
	// in its closure, so a ratio counting HEADs collapses during a healthy push.
	const want = `sum(rate(tsnixcache_narinfo_hits_total{method="get"}[$__rate_interval])) / ` +
		`(sum(rate(tsnixcache_narinfo_hits_total{method="get"}[$__rate_interval])) + ` +
		`sum(rate(tsnixcache_narinfo_misses_total{method="get"}[$__rate_interval])))`

	var found int

	for _, p := range dashboardPanels(t) {
		if !strings.Contains(strings.ToLower(p.Title), "ratio") ||
			p.FieldConfig.Defaults.Unit != "percent" {
			continue
		}

		found++

		for _, target := range p.Targets {
			expr := strings.Join(strings.Fields(target.Expr), " ")

			for _, fudge := range []string{"clamp_min", "clamp_max", "or vector("} {
				if strings.Contains(expr, fudge) {
					require.FailNowf(t, "dashboard invariant", "panel %q fudges the hit-ratio denominator with %s: %s", p.Title, fudge, expr)
				}
			}

			if !strings.Contains(expr, want) {
				require.FailNowf(t, "dashboard invariant", "panel %q is not hits/(hits+misses):\n got %s\nwant it to contain %s",
					p.Title, expr, want)
			}
		}
	}

	// The Overview tile and the "Hit ratio over time" graph. A rename that
	// hides one from this check must fail rather than pass vacuously.
	if found < 2 {
		require.FailNowf(t, "dashboard invariant", "found %d percentage ratio panels, want the Overview tile and the hit-ratio timeseries", found)
	}
}

// TestDescriptionsPromiseNothingThatDoesNotExist: tsnixcache ships no alerting
// rules, so a tooltip telling an operator that an alert fires is a lie they only
// discover during an incident.
func TestDescriptionsPromiseNothingThatDoesNotExist(t *testing.T) {
	for _, p := range dashboardPanels(t) {
		if strings.Contains(strings.ToLower(p.Description), "alert") {
			require.FailNowf(t, "dashboard invariant", "panel %q describes an alert, but tsnixcache ships no alerting rules: %s",
				p.Title, p.Description)
		}
	}
}

func TestOperationalGaugesExpire(t *testing.T) {
	var operational, historical int

	for _, p := range dashboardPanels(t) {
		current := p.Title == "Store disk used" || p.Title == "Store paths"
		if !current {
			if p.Type == "timeseries" {
				historical++

				for _, target := range p.Targets {
					require.False(t, target.Instant, p.Title)
					require.True(t, target.Range, p.Title)
				}
			}

			continue
		}

		operational++

		require.Equal(t, "none", p.Options.GraphMode, p.Title)
		require.Equal(t, []string{"last"}, p.Options.ReduceOptions.Calcs, p.Title)
		require.Equal(t, "Unavailable", p.FieldConfig.Defaults.NoValue, p.Title)
		require.Len(t, p.FieldConfig.Defaults.Mappings, 1)
		mapping := p.FieldConfig.Defaults.Mappings[0]
		require.Equal(t, "special", mapping.Type)
		require.Equal(t, "null+nan", mapping.Options.Match)
		require.Equal(t, "Unavailable", mapping.Options.Result.Text)
		require.Equal(t, "gray", mapping.Options.Result.Color)

		for _, target := range p.Targets {
			require.True(t, target.Instant, p.Title)
			require.False(t, target.Range, p.Title)
			require.Contains(t, target.Expr, "and on (job, instance)")
			require.Contains(t, target.Expr, "(up == 1)")
			require.Contains(t, target.Expr, "time() - timestamp(up) < 90")

			for _, metric := range metricsIn(target.Expr) {
				if metric == "up" {
					continue
				}

				require.Contains(t, target.Expr, "time() - timestamp("+metric+") < 90")
			}
		}
	}

	require.Equal(t, 2, operational)
	require.Positive(t, historical)
}
