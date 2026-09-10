// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package grafana builds the provisionable tsnixcache dashboard.
package grafana

import (
	"fmt"

	"github.com/grafana/grafana-foundation-sdk/go/common"
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	"github.com/grafana/grafana-foundation-sdk/go/prometheus"
	"github.com/grafana/grafana-foundation-sdk/go/stat"
	"github.com/grafana/grafana-foundation-sdk/go/timeseries"
	"github.com/grafana/grafana-foundation-sdk/go/units"
)

// datasourceVar is the name of the datasource template variable; panels point
// at "${datasource}" so the viewer's Grafana picks the concrete datasource.
const datasourceVar = "datasource"

// Row titles. Each names the question its panels answer, so an operator scans
// headings rather than panels. They are constants because dashboard_test.go
// selects panels by row — the Overview tests would silently pass over an empty
// set if a title here drifted from a string literal there.
const (
	rowOverview      = "Overview — is the cache healthy right now?"
	rowTraffic       = "Traffic — how much data is moving, and which way?"
	rowEffectiveness = "Cache effectiveness — are clients finding what they ask for?"
	rowStorage       = "Storage & GC — will there still be room tomorrow?"
)

// Range selectors. Every rate() and increase() in this file uses one of these
// three, and which one a panel takes follows from how often the thing it counts
// actually happens.
const (
	// rateWin covers live traffic: requests, bytes, imports. $__rate_interval
	// follows Grafana's step, so a panel zoomed out to 30 days keeps sampling
	// more than one scrape per step; a fixed [5m] silently under-reports there,
	// which is exactly when someone is reviewing an incident.
	rateWin = `[$__rate_interval]`

	// gcCountWin covers the GC counters. GC is threshold-triggered rather than
	// periodic, so its counters move in bursts and a step-sized window is empty
	// almost always; an hour is wide enough to still show the last burst and
	// narrow enough that a watcher which stops ticking (gc_checks_total) shows it
	// within the hour rather than decaying over a day.
	gcCountWin = `[1h]`

	// gcHistWin covers the GC histograms, and is wider than gcCountWin because
	// the failure mode is worse: rate() over a window containing no observation
	// is 0 in every bucket, histogram_quantile of that is NaN, and the panel is
	// blank forever rather than merely sparse. With realistic rules a collect is
	// hours or days apart, so a day is the shortest window that reliably holds
	// one.
	gcHistWin = `[24h]`
)

// Colour thresholds, as percentages. Shared with the descriptions below — the
// tooltips are formatted from these constants rather than repeating the numbers,
// because a tooltip promising amber at 75% while the tile turns amber at 85% is
// the same class of defect as a panel querying the wrong metric.
const (
	// hitRatioLowPct and hitRatioGoodPct: below 80% of narinfo GETs served,
	// clients are paying upstream latency for most of what they ask for, which is
	// a cold or misdirected cache rather than normal variation. Above 95% is the
	// steady state of a cache whose clients build against it.
	hitRatioLowPct  = 80.0
	hitRatioGoodPct = 95.0

	// diskWarnPct and diskCritPct bracket the range operators put in --gc-rule
	// thresholds. Amber is "GC is about to become the thing keeping this server
	// alive"; red is "it already is, and it is not winning".
	diskWarnPct = 75.0
	diskCritPct = 90.0
)

// PromQL expressions long enough to be worth naming (and, for the hit ratio,
// reused). rate()/increase() turn the monotonic counters into live rates; the
// gauges (disk bytes, paths) are read directly.
const (
	// Served plus received, because the question the headline tile answers is
	// "how much work is this cache doing", not which direction it went in. Both
	// counters advance once per request, when the transfer ends (cache.go adds
	// cw.n after nar.Write returns, and the received bytes after io.Copy), so
	// this is completed throughput and reads spiky when few transfers overlap.
	totalBandwidthExpr = `sum(rate(tsnixcache_nar_bytes_served_total` + rateWin + `)) + ` +
		`sum(rate(tsnixcache_nar_bytes_received_total` + rateWin + `))`

	// method="get" only: the counters are labelled by request method, and a
	// push does a presence HEAD for every path in the closure before it uploads
	// anything (upload.headNarInfo). Counting those probes would make a pushing
	// client's misses swamp the reads the ratio is meant to describe.
	hitsRateExpr   = `sum(rate(tsnixcache_narinfo_hits_total{method="get"}` + rateWin + `))`
	missesRateExpr = `sum(rate(tsnixcache_narinfo_misses_total{method="get"}` + rateWin + `))`

	// hits / (hits+misses) * 100. The denominator is deliberately unguarded: on
	// an idle cache it is 0, the division is NaN, and the panel reads "No data",
	// which is the truth. Flooring it (clamp_min(..., 1)) would make the panel
	// plot the numerator alone — a fictitious ratio — for any cache doing less
	// than one lookup per second.
	hitRatioExpr = hitsRateExpr + ` / (` + hitsRateExpr + ` + ` + missesRateExpr + `) * 100`

	// Import runs inside the narinfo PUT, so it happens on every push and a
	// step-sized window has data whenever anything is being pushed. The
	// histogram's buckets stop at ~2048s, just past the 30m import deadline, so
	// a timing-out import is visible rather than lost in +Inf.
	importP95Expr = `histogram_quantile(0.95, sum by (le) ` +
		`(rate(tsnixcache_import_duration_seconds_bucket` + rateWin + `)))`
	importP50Expr = `histogram_quantile(0.5, sum by (le) ` +
		`(rate(tsnixcache_import_duration_seconds_bucket` + rateWin + `)))`

	// gcHistWin, not a step-sized window: these histograms are observed only when
	// a disk threshold actually trips. See gcHistWin.
	prunePctlExpr = `histogram_quantile(0.95, sum by (le) ` +
		`(rate(tsnixcache_gc_prune_duration_seconds_bucket` + gcHistWin + `)))`
	collectPctlExpr = `histogram_quantile(0.95, sum by (le) ` +
		`(rate(tsnixcache_gc_collect_duration_seconds_bucket` + gcHistWin + `)))`
)

// Panel descriptions — the "i" tooltip on each panel. Each says what the panel
// measures, in what units, and what to do when it looks wrong, so the dashboard
// is legible at 3am without reading PromQL or this source. Where a metric is
// subtler than its name suggests, the tooltip says so: a reader who does not
// know that the freed-bytes counter is a statfs delta will misread a flat zero
// as "GC is broken".
const (
	descTotalBW = "Live NAR throughput for the whole fleet in bytes/sec: served to " +
		"substituter clients plus received from pushers. Both counters advance once " +
		"per request, when the transfer finishes, so one multi-gigabyte NAR appears " +
		"as a single spike rather than a plateau — read this as work completed, not " +
		"as instantaneous link rate. Only NAR bodies move this counter, so flat zero " +
		"while clients are complaining is one of two things: no request is reaching " +
		"the server at all — check the tailnet and the listener — or every lookup is " +
		"missing, so nothing gets as far as a NAR body. The lookups panel tells them " +
		"apart."
	descHitRatio = "Percentage of narinfo GETs answered from the store, " +
		"hits/(hits+misses) over the panel's rate window. GETs only: a push probes " +
		"every path in its closure with HEAD before uploading, so counting those " +
		"would read as a collapsing hit ratio every time someone pushes. \"No data\" " +
		"means no GET traffic in the window — an idle cache, not a broken one; the " +
		"denominator is deliberately unfloored so this reads empty rather than " +
		"inventing a ratio. Sustained low means clients are asking for paths the " +
		"cache lacks: a cold cache, a client pointed at the wrong substituter, or a " +
		"GC sweep that took more than it should have."
	descStorePaths = "Valid paths in each server's Nix database. Rises with pushes, " +
		"falls after nix-collect-garbage. The gauge is dropped rather than zeroed when " +
		"the count query exceeds its 2-second deadline, so a tile that goes blank means " +
		"the store database is slow or unreachable, not that the store is empty — " +
		"confirm with GET /health, which reports store_reachable. " + descFreshness

	descBandwidth = "NAR bytes/sec leaving (served to substituter clients) against " +
		"entering (received from pushers), summed across the fleet. Which line " +
		"dominates says whether this cache is being read or written. Received counts " +
		"every byte that arrived even if the push then failed, so ingress with a flat " +
		"push rate on the panel beside this one means uploads are dying part-way " +
		"through."
	descPushRate = "Accepted NAR and narinfo PUTs per second against the ones that " +
		"were not accepted, so a push outage cannot look like an idle cache. " +
		"\"failed\" is by reason: nar_body/narinfo_body (the body broke, exceeded its " +
		"16 GiB NAR or 1 MiB narinfo limit, or the spool write failed), narinfo_parse " +
		"(unparseable, or a compression this server cannot decode), busy (no import " +
		"slot came free within 30s, so the client got a 503 with Retry-After unless " +
		"it had already hung up). \"refused\" is the " +
		"push-grant boundary: no_grant (the peer has no push capability), " +
		"unidentified (WhoIs could not attribute the connection), local_write (a " +
		"write on a --listen socket started without --local-write). The server log " +
		"names the peer and path behind each."

	descHitsMisses = "narinfo lookups per second that found (hit) or did not find " +
		"(miss) a path, split by HTTP method. GET is a substituter reading; HEAD is a " +
		"pusher asking what it still needs to upload, so a burst of HEAD misses is new " +
		"work arriving and is the healthy shape during a push. This is the only panel " +
		"that shows the HEAD traffic — both ratio panels drop it on purpose. Sustained " +
		"GET misses are the ones to act on: clients want paths this cache does not hold."
	descHitRatioTS = "The same GET-only hits/(hits+misses) percentage as the Overview " +
		"tile, over time, so a drop can be dated. Gaps are windows with no GET traffic, " +
		"not failures. A step down straight after a GC sweep means the sweep took paths " +
		"clients still want — lengthen the age on the matching --gc-rule; a step down " +
		"with no GC behind it usually means a new client or a new set of derivations."
	descImport = "nix-store --import operations per second by outcome. The failure " +
		"line also counts pushes shed because no import slot came free within 30 " +
		"seconds, which is load rather than corruption — tell the two apart by the " +
		"busy series on the push panel. Any sustained failure means pushed NARs are " +
		"not landing in the store: read the import error in the server log and check " +
		"the store's free space."
	descImportDur = "Time spent in verify+import of one pushed path, p50 and p95. " +
		"The server runs the import inside the final narinfo PUT and sends nothing " +
		"meanwhile, so this is the tail of what the client waits for — but not the " +
		"whole of it: the NAR upload that precedes it is not timed here, and over a " +
		"slow link a multi-gigabyte PUT dwarfs the import. " +
		"Failed imports are timed too, so an import that hits the 30-minute deadline " +
		"lands in the top bucket. A p95 climbing towards 30 minutes means pushes are " +
		"about to start timing out — look at store IO and at --import-concurrency."

	descDiskUsedTotal = "Absolute used and total bytes of each server's store " +
		"filesystem; the gap between the lines is the headroom left. Used is " +
		"Blocks-Bfree, matching the Overview tile and the GC rule, so the last few per " +
		"cent of that gap are root-reserved and unavailable to an unprivileged writer " +
		"— df counts that reserve against you and reads a little higher. Both series " +
		"disappear together if statfs fails. Read the slope rather than the value: " +
		"this is the panel that says how many days of headroom are left."
	descSpoolDisk = "Used and total bytes of the filesystem holding the upload spool, " +
		"on the same used-bytes arithmetic as the store panel beside it. The spool is " +
		"often a separate and much smaller filesystem, and it is the one that fills " +
		"first: a pushed NAR lands here whole — up to 16 GiB — before it is verified " +
		"and imported, and serving zstd keeps compressed copies here too, up to a 4 " +
		"GiB budget. When the gap between the lines falls below the largest NAR being " +
		"pushed, the next push fails with no space left. Startup removes abandoned " +
		"uploads; during normal operation the sweeper removes uploads idle for 24 " +
		"hours. Give the spool headroom of its own."
	descGCFreed = "Bytes reclaimed by nix-collect-garbage per second, averaged over " +
		"an hour, per server. A lower bound rather than an accounting figure: the " +
		"server measures it as the increase in statfs Bavail across each collect, so " +
		"concurrent writes and copy-on-write snapshots (btrfs, ZFS) hide reclaimed " +
		"space, a collect that ends with less free space than it started records zero, " +
		"and a collect that fails records nothing at all. Flat zero therefore does not " +
		"mean GC did nothing. Read it against the disk panels: a disk that stays full " +
		"is the symptom, this is only the clue."
	descGCRuns = "GC runs and errors over the last hour, fleet totals split by cause: " +
		"runs by the --gc-rule threshold that tripped, errors by the operation that " +
		"failed. A run always prunes eligible gcroots but may skip nix-collect-garbage " +
		"when nothing was unrooted and the previous collect freed nothing — that " +
		"cooldown is max(--gc-interval, 1h), so runs climbing with freed bytes flat is " +
		"expected rather than a fault. Errors should be zero: statfs means the store " +
		"filesystem could not be measured, so that check did nothing at all, collect means " +
		"nix-collect-garbage exited non-zero, and prune counts both a failed directory " +
		"read and each individual gcroot that could not be removed — usually " +
		"permissions on --gcroot-dir. The server log carries the specific failure."
	descGCRoots = "gcroots currently in --gcroot-dir per server, against how fast they " +
		"are pruned and how often the watcher checks the disk (both per second, " +
		"averaged over an hour). Roots climbing while pruned/s stays flat usually means " +
		"the disk sits below every --gc-rule threshold, so no rule selects and nothing " +
		"prunes — read it against the runs panel first. Roots climbing while runs climb " +
		"too is the one to act on: the rule's age outlasts the rate paths arrive, so GC " +
		"never unroots anything. Shorten the age or add a higher-threshold rule. " +
		"checks/s falling to zero means " +
		"the watcher itself has stopped, which otherwise looks exactly like a healthy " +
		"idle server. The roots count is orders of magnitude larger than either rate, " +
		"so read the rates from their legend values rather than from the shape of their " +
		"line. Zero roots before the first push is normal: the directory does not exist " +
		"yet."
	descGCDuration = "95th-percentile prune and collect durations in seconds, over a " +
		"24-hour window: GC is threshold-triggered and fires hours or days apart, and a " +
		"window with no observation in it makes histogram_quantile NaN, so a short one " +
		"would leave this blank forever rather than merely sparse. Prune is timed on " +
		"every run, collect only on the runs that actually collect, so the collect line " +
		"is the sparser of the two. Rising p95 means GC is getting slower — store " +
		"growth or IO contention — and a collect approaching an hour holds nix's global " +
		"GC lock for that long, blocking every push and local build behind it."
)

// Descriptions whose numbers come from the threshold constants, so the tooltip
// and the colour it explains cannot drift apart.
var (
	descHitRatioTile = descHitRatio + fmt.Sprintf(
		" Colour thresholds: red below %g%%, amber %g–%g%%, green above.",
		hitRatioLowPct, hitRatioLowPct, hitRatioGoodPct,
	)

	descDiskUsed = fmt.Sprintf("Store filesystem usage per server, as used bytes over "+
		"total bytes — the same arithmetic gc.go's high-water rule uses, so this tile "+
		"and the rule that prunes on it cannot disagree. Root-reserved blocks count as "+
		"free here; df uses used/(used+available), which excludes the root reserve. "+
		"Amber at %g%%, red at %g%%: above the highest "+
		"--gc-rule threshold, watch GC decisions and the freed-bytes "+
		"panel to see whether it is winning. A tile with no value at all means statfs "+
		"failed — the gauges are not emitted then. "+descFreshness,
		diskWarnPct, diskCritPct)
)

// Operational samples expire before Prometheus's default lookback does.
const freshnessSeconds = 90

func liveGauge(metric string) string {
	return fmt.Sprintf("(%s and (time() - timestamp(%s) < %d) and on (job, instance) "+
		"((up == 1) and (time() - timestamp(up) < %d)))", metric, metric, freshnessSeconds, freshnessSeconds)
}

func diskUsedPctQuery() string {
	return "max by (instance) (" + liveGauge("tsnixcache_store_disk_used_bytes") +
		" / " + liveGauge("tsnixcache_store_disk_total_bytes") + ") * 100"
}

const descFreshness = "Unavailable also means the target is down or either its gauge or up sample " +
	"is at least 90 seconds old. " +
	"Scrape at least every 30 seconds. The dashboard refreshes every 30 seconds; historical panels retain earlier samples."

// promDS references the datasource selected by the $datasource variable.
func promDS() common.DataSourceRef {
	return common.DataSourceRef{
		Type: new("prometheus"),
		Uid:  new("${" + datasourceVar + "}"),
	}
}

// query is a Prometheus target: an expression and the legend format naming its
// series. The legend is not optional in practice — Grafana falls back to the
// full label set, which for these expressions is an unreadable brace soup — so
// every call site passes an explicit label, using {{instance}} wherever a series
// is per server and a bare word where it is a fleet total.
func query(expr, legend string) *prometheus.DataqueryBuilder {
	return prometheus.NewDataqueryBuilder().Expr(expr).LegendFormat(legend).Range()
}

// panelLegend configures the legend for a time-series panel: a table with
// last/max/mean columns, so current and peak values are readable without
// hovering, and so a series too small to see against its neighbours (the GC
// rates beside the roots count) still has a readable number. Every panel gets
// one — panels that look single-series here still fan out to one series per
// instance, method, reason or op.
func panelLegend() *common.VizLegendOptionsBuilder {
	return common.NewVizLegendOptionsBuilder().
		Placement(common.LegendPlacementBottom).
		ShowLegend(true).
		DisplayMode(common.LegendDisplayModeTable).
		Calcs([]string{"lastNotNull", "max", "mean"})
}

// timeseriesPanel is the common shape for the graph panels: half-width so two
// sit side by side, a unit that must match what the expression returns (a ratio
// times 100 is percent, a rate of bytes is Bps, a quantile of a _seconds
// histogram is seconds), a description tooltip, and one or more Prometheus
// targets. unit is a Grafana unit id from the units package, never a free string.
func timeseriesPanel(title, desc, unit string, targets ...*prometheus.DataqueryBuilder) *timeseries.PanelBuilder {
	p := timeseries.NewPanelBuilder().
		Title(title).
		Description(desc).
		Unit(unit).
		Datasource(promDS()).
		Span(12).
		Height(8).
		FillOpacity(10).
		Legend(panelLegend())
	for _, t := range targets {
		p = p.WithTarget(t)
	}

	return p
}

// Traffic tiles retain historical rates; operational gauges use currentStatPanel.
func statPanel(title, desc, unit string, target *prometheus.DataqueryBuilder) *stat.PanelBuilder {
	return stat.NewPanelBuilder().
		Title(title).
		Description(desc).
		Unit(unit).
		Datasource(promDS()).
		Span(6).
		Height(4).
		ColorMode(common.BigValueColorModeValue).
		GraphMode(common.BigValueGraphModeArea).
		ReduceOptions(common.NewReduceDataOptionsBuilder().Calcs([]string{"lastNotNull"})).
		WithTarget(target)
}

func currentStatPanel(title, desc, unit string, target *prometheus.DataqueryBuilder) *stat.PanelBuilder {
	return statPanel(title, desc, unit, target.Instant()).
		GraphMode(common.BigValueGraphModeNone).
		ReduceOptions(common.NewReduceDataOptionsBuilder().Calcs([]string{"last"})).
		NoValue("Unavailable").
		Mappings([]dashboard.ValueMapping{{SpecialValueMap: &dashboard.SpecialValueMap{
			Type: dashboard.MappingTypeSpecialValue,
			Options: dashboard.DashboardSpecialValueMapOptions{
				Match:  dashboard.SpecialValueMatchNullAndNan,
				Result: dashboard.ValueMappingResult{Text: new("Unavailable"), Color: new("gray")},
			},
		}}})
}

// hitRatioThresholds colours the hit-ratio tile by health, worst-first as
// Grafana expects: red is the base, amber from hitRatioLowPct, green from
// hitRatioGoodPct. A cold or misconfigured cache therefore reads red at a
// glance rather than needing the number to be interpreted.
func hitRatioThresholds() *dashboard.ThresholdsConfigBuilder {
	return dashboard.NewThresholdsConfigBuilder().
		Mode(dashboard.ThresholdsModeAbsolute).
		Steps([]dashboard.Threshold{
			{Value: nil, Color: "red"},
			{Value: new(hitRatioLowPct), Color: "yellow"},
			{Value: new(hitRatioGoodPct), Color: "green"},
		})
}

// diskThresholds runs the other way round — green is the base and the colour
// worsens as the number rises — because for disk the large value is the bad
// one. The steps are the same percentages an operator writes into --gc-rule, so
// the tile turning amber and GC starting to prune are the same event.
func diskThresholds() *dashboard.ThresholdsConfigBuilder {
	return dashboard.NewThresholdsConfigBuilder().
		Mode(dashboard.ThresholdsModeAbsolute).
		Steps([]dashboard.Threshold{
			{Value: nil, Color: "green"},
			{Value: new(diskWarnPct), Color: "yellow"},
			{Value: new(diskCritPct), Color: "red"},
		})
}

// buildDashboard assembles the full tsnixcache dashboard. Build() schema-validates,
// so a malformed panel fails here (and thus fails the generating derivation).
//
// Panels are ordered by the question they answer, not by which subsystem emits
// them: the Overview row is what an operator reads first and must work on a
// default deployment, then traffic, then whether the cache is earning its keep,
// then whether it will still have room tomorrow.
//
// We emit the v1 dashboard model on purpose: it is what file-based Grafana
// provisioning consumes and works on Grafana >= 10. dashboardv2 is the Grafana
// 12 app-platform schema and is not portable to the generic instances this
// dashboard targets — hence the staticcheck deprecation is knowingly ignored.
//
//nolint:staticcheck // v1 model required for portable file-based provisioning
func buildDashboard() (dashboard.Dashboard, error) {
	return dashboard.NewDashboardBuilder("tsnixcache").
		Uid("tsnixcache").
		Tags([]string{"tsnixcache", "nix", "generated"}).
		Refresh("30s").
		Time("now-6h", "now").
		Timezone(common.TimeZoneBrowser).
		WithVariable(
			dashboard.NewDatasourceVariableBuilder(datasourceVar).
				Label("Data source").
				Type("prometheus"),
		).

		// Row 1 — Overview: headline stat tiles, and the only row that has to be
		// readable on every deployment, so nothing here may query a metric that
		// exists only when GC rules are configured (TestOverviewAvoidsOptionalMetrics).
		// Traffic and hit ratio are fleet totals (the question is "how much");
		// disk and paths are per instance (the question is "which server").
		WithRow(dashboard.NewRowBuilder(rowOverview)).
		WithPanel(
			statPanel("Total NAR bandwidth",
				descTotalBW,
				units.BytesPerSecondSI, query(totalBandwidthExpr, "served + received")),
		).
		WithPanel(
			statPanel("narinfo hit ratio (GET)",
				descHitRatioTile,
				units.Percent, query(hitRatioExpr, "hit ratio")).
				Thresholds(hitRatioThresholds()),
		).
		WithPanel(
			// Background colouring, not just the number: this is the tile that has
			// to be legible from across a room when the disk is filling.
			currentStatPanel("Store disk used",
				descDiskUsed,
				units.Percent, query(diskUsedPctQuery(), "{{instance}}")).
				Thresholds(diskThresholds()).
				ColorMode(common.BigValueColorModeBackground),
		).
		WithPanel(
			// No sparkline: the path count moves in steps on push and GC, and an
			// area chart of it reads as a trend that is not there.
			currentStatPanel("Store paths",
				descStorePaths,
				units.Short, query("max by (instance) ("+liveGauge("tsnixcache_store_paths")+")", "{{instance}}")).
				GraphMode(common.BigValueGraphModeNone),
		).

		// Row 2 — Traffic: what is on the wire, and whether pushes are landing.
		WithRow(dashboard.NewRowBuilder(rowTraffic)).
		WithPanel(
			timeseriesPanel("NAR bandwidth: served vs received",
				descBandwidth,
				units.BytesPerSecondSI,
				query(`sum(rate(tsnixcache_nar_bytes_served_total`+rateWin+`))`, "served (egress)"),
				query(`sum(rate(tsnixcache_nar_bytes_received_total`+rateWin+`))`, "received (ingress)")),
		).
		WithPanel(
			timeseriesPanel("Push rate: accepted vs failed vs refused",
				descPushRate,
				units.OpsPerSecond,
				query(`sum(rate(tsnixcache_push_nar_total`+rateWin+`))`, "accepted: NAR"),
				query(`sum(rate(tsnixcache_push_narinfo_total`+rateWin+`))`, "accepted: narinfo"),
				// Failed and refused writes belong next to accepted ones: with
				// only the accepted lines, a push outage draws the same picture
				// as an idle cache. They come from different subsystems — the
				// cache handler and the auth middleware — and the legend prefixes
				// keep the two reason label-sets apart in one table.
				query(`sum by (reason) (rate(tsnixcache_push_errors_total`+rateWin+`))`, "failed: {{reason}}"),
				query(`sum by (reason) (rate(tsnixcache_auth_rejects_total`+rateWin+`))`, "refused: {{reason}}")),
		).

		// Row 3 — Cache effectiveness: is the cache answering what is asked of it,
		// and does what gets pushed actually make it into the store.
		WithRow(dashboard.NewRowBuilder(rowEffectiveness)).
		WithPanel(
			// All four series: the ratio panels drop HEADs on purpose, so this
			// is the one place the push-side probe traffic is visible, and the
			// HEAD-miss rate is how much new work pushers are bringing.
			timeseriesPanel("narinfo lookups: hits vs misses by method",
				descHitsMisses,
				units.OpsPerSecond,
				query(`sum by (method) (rate(tsnixcache_narinfo_hits_total`+rateWin+`))`, "hit ({{method}})"),
				query(`sum by (method) (rate(tsnixcache_narinfo_misses_total`+rateWin+`))`, "miss ({{method}})")),
		).
		WithPanel(
			// Pinned to 0–100 so a quiet period cannot autoscale a 2-point wobble
			// into a cliff.
			timeseriesPanel("narinfo hit ratio over time (GET)",
				descHitRatioTS,
				units.Percent,
				query(hitRatioExpr, "hit ratio")).
				Min(0).Max(100),
		).
		WithPanel(
			timeseriesPanel("Import outcome: success vs failure",
				descImport,
				units.OpsPerSecond,
				query(`sum(rate(tsnixcache_import_success_total`+rateWin+`))`, "imported"),
				query(`sum(rate(tsnixcache_import_fail_total`+rateWin+`))`, "failed (incl. shed)")),
		).
		WithPanel(
			timeseriesPanel("Import duration (p50/p95)",
				descImportDur,
				units.Seconds,
				query(importP50Expr, "p50"),
				query(importP95Expr, "p95")),
		).

		// Row 4 — Storage & GC: the operational-safety layer. Two filesystems and
		// the machinery that keeps them from filling.
		WithRow(dashboard.NewRowBuilder(rowStorage)).
		WithPanel(
			timeseriesPanel("Store filesystem: used vs total",
				descDiskUsedTotal,
				units.BytesIEC,
				query(`max by (instance) (tsnixcache_store_disk_used_bytes)`, "{{instance}} used"),
				query(`max by (instance) (tsnixcache_store_disk_total_bytes)`, "{{instance}} total")),
		).
		WithPanel(
			// Share collectDisk's accounting so spool and store panels remain comparable.
			timeseriesPanel("Spool filesystem: used vs total",
				descSpoolDisk,
				units.BytesIEC,
				query(`max by (instance) (tsnixcache_spool_disk_used_bytes)`, "{{instance}} used"),
				query(`max by (instance) (tsnixcache_spool_disk_total_bytes)`, "{{instance}} total")),
		).
		WithPanel(
			// Freed bytes gets a panel to itself: a bytes/sec rate and a run
			// count cannot share one axis unit without one of them lying.
			timeseriesPanel("GC bytes freed (1h rate, lower bound)",
				descGCFreed,
				units.BytesPerSecondSI,
				query(`sum by (instance) (rate(tsnixcache_gc_freed_bytes_total`+gcCountWin+`))`, "{{instance}}")),
		).
		WithPanel(
			// Split by the labels the tooltip tells the operator to read: which
			// high-water rule tripped, and which operation failed. increase()
			// rather than rate(), because "three collect failures last hour" is
			// the number to act on and 0.00083/s is not.
			timeseriesPanel("GC runs & errors (1h)",
				descGCRuns,
				units.Short,
				query(`sum by (threshold_pct) (increase(tsnixcache_gc_runs_total`+gcCountWin+`))`,
					"runs ≥{{threshold_pct}}%"),
				query(`sum by (op) (increase(tsnixcache_gc_errors_total`+gcCountWin+`))`, "errors: {{op}}")),
		).
		WithPanel(
			// Per instance throughout, including the two rates: a fleet-total
			// prune rate under a per-instance roots count would look like one
			// server pruning another's roots.
			timeseriesPanel("GC roots & watcher liveness",
				descGCRoots,
				units.Short,
				query(`max by (instance) (tsnixcache_gc_roots_current)`, "{{instance}} roots"),
				query(`sum by (instance) (rate(tsnixcache_gc_roots_pruned_total`+gcCountWin+`))`,
					"{{instance}} pruned/s"),
				query(`sum by (instance) (rate(tsnixcache_gc_checks_total`+gcCountWin+`))`,
					"{{instance}} checks/s")),
		).
		WithPanel(
			timeseriesPanel("GC duration p95 (24h)",
				descGCDuration,
				units.Seconds,
				query(prunePctlExpr, "prune p95"),
				query(collectPctlExpr, "collect p95")),
		).
		Build()
}
