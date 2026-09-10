# Remediation verification

All original findings and implementation follow-ups are remediated in code through `3407098`. This record separates source validation, historical reproductions and the per-revision CI results linked below.

Baseline: `da29eb6c28ee342a9d14b2be018a2a15d4b9406e`. Work follows the local remediation plan and `/home/kradalby/git/dotfiles/docs/conventions`. User authorized commits and pushes to existing branch `initial`; plan documents remain uncommitted.

## Completed commits

| Commit    | Change                                                                         | Evidence                                                                                                                                                                              |
| --------- | ------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `7161442` | Supported flake systems and native contributor checks; lock-only trigger       | Mandatory hooks passed                                                                                                                                                                |
| `53c7d65` | Dependency manifests/locks                                                     | Mandatory hooks passed; pushed to `initial`; all 26 remote checks passed                                                                                                              |
| `b0032f0` | Context-aware decoding, decoded-size ceiling, bounded upload response handling | New cancellation/body-stall regressions fail before implementation; race tests pass across nixcompress, niximport, upload, cache, and command packages; lint and mandatory hooks pass |

## Cache and GC implementation

GC ownership coordination lives in root package `gcroot/`; no `internal/` package remains. Commit `c1ecbc5` adds process-death/cancellation tests, overlapping import ownership, stale-selection revalidation, busy-root pruning, and read-only dry-run coverage. All 26 remote checks passed at this revision.

Commit `4f6e76e` implements charged in-progress writes, pinned readers, bounded shared builds, list-based eviction, private directory ownership and cancellation-aware encoding. The cache constructor now reports initialization failures; serve closes the cache after shutdown. Failed unlink remains charged until cleanup succeeds. Native flake build/race/lint/formatting checks and whole-repository local race tests pass. Generated vendor hash is commit `d452e2e5`; both commits were pushed and all 26 remote checks passed.

The follow-up cache review found and fixed three additional paths before commit:

- Narinfo verification cleanup could unlink a root-spool lock. The lock now resides in the private cache subdirectory, unreachable through basename-only upload/import paths. Actual narinfo import and re-executed process tests cover ownership.
- Distinct cancelled misses could leave unlimited queued goroutines. New distinct flights are capped at twice the compression-slot count; existing-key requests still share work.
- HTTP/1 unread GET bodies bypass write deadlines and can block post-handler draining. GET/HEAD bodies are rejected before work begins, with connection closure and an expired read deadline. TCP tests with withheld fixed-length/chunked bodies assert server-side connection closure. The closure assertion failed before adding the read deadline.

The stalled TCP reader test records allocated blocks separately from charged file data. On the local filesystem the fixture occupied its data size; inside the Nix test sandbox it occupied an additional 4 KiB. The configured cache budget covers rounded file data, not filesystem metadata/allocation overhead.

## Eviction performance

Production eviction benchmark, identical fixtures (one-byte files with synthetic charged sizes; reclaim half the entries), ten samples each, `-benchtime=1x -benchmem`, Linux amd64, Go 1.27.1, Intel i9-13900K. The baseline includes its original per-victim logging; final code removes logging from the normal eviction path as well as the repeated map scans. These are synthetic filesystem measurements, not throughput promises.

| Entries | Before median | After median | Change  | Allocations before → after |
| ------- | ------------- | ------------ | ------- | -------------------------- |
| 500     | 2.584 ms      | 0.561 ms     | -78.29% | 763 → 250                  |
| 2,000   | 23.835 ms     | 2.214 ms     | -90.71% | 3,013 → 1,000              |
| 8,000   | 325.421 ms    | 9.584 ms     | -97.05% | 12,034 → 4,000             |

Benchstat reports p=0.000 for all three timing comparisons (n=10). After samples have filesystem noise of ±38–69%; the reduction remains substantial. Cold/warm serving and import resource measurements are recorded separately below. Scheduler and filtering comparisons follow.

## Upload scheduler

Commit `92c747a4` replaces layer barriers with a bounded completion queue. The coordinator owns dependency counts and final results, starts dependents after their references complete, preserves independent work after failure, reports cycles, and records each unique path once. Worker/result-channel capacity is capped by graph size. Tests cover a blocked unrelated sibling, diamond/self/out-of-graph references, duplicate metadata and roots, failed ancestry, cycles with independent work, cancellation, pre-cancellation and maximum concurrency. Follow-up review found no remaining scheduler correctness blocker.

Benchmarks use actual HTTP HEAD requests with deterministic service delays. Baseline source is `d452e2e5:upload/upload.go` selected through Go's overlay mechanism; both runs use the same committed benchmark fixture and environment. Ten one-iteration samples per fixture:

| Fixture                                                     | Total before → after       | Chain completion before → after |
| ----------------------------------------------------------- | -------------------------- | ------------------------------- |
| Two workers, one short chain plus long sibling              | 378.5 → 251.0 ms (-33.68%) | 378.3 → 152.6 ms (-59.67%)      |
| Eight workers, eight heterogeneous chains plus long sibling | 410.9 → 261.8 ms (-36.28%) | 410.7 → 198.1 ms (-51.76%)      |

Benchstat p=0.000 for these timing comparisons (n=10). These controlled workloads demonstrate removal of the barrier; they do not predict network-specific throughput. Larger fixture allocations fell from 5,651 to 3,909 per operation.

The review additionally confirmed an existing upload-producer bug: early HTTP 200 can race `h.Sum` against net/http request hashing and report a partial NAR as successful. A real TCP probe returned success after about 6 MiB of a 16 MiB random fixture; the race detector reported concurrent SHA-256 access. Commit `8b6ac027` moves hashing into the producer, joins producers on every response path, and rejects early success before complete request consumption. Real TCP tests cover early 200/400/503 responses with a large random payload under the race detector. All 26 remote checks passed.

## Durable watcher and lifecycle

Commits `661d3d4`, `963364b`, `2d00c1c`, `aa8aee1`, and `0290a470` establish SQLite persistence, generated queries, schema validation, exclusive ownership, and dependency packaging. All 26 remote checks passed on `0290a470`.

Commit `197858fa` retains discoveries, retry ages/backoff, acknowledgements, final allowances, expiry tombstones, and drain boundaries. Tests cover an outage exceeding 1,001 paths, bounded batches, recovery, source replacement and symlink repointing, expired-path re-registration, vanished final-attempt rows, and subprocess crashes around discovery/upload/acknowledgement. SQLite tests exercise actual FULL errors, corruption, truncated state, rollback, permissions, process ownership, and incremental vacuum.

Notification hints watch the database directory, with bounded setup retries and periodic polling after errors or closed channels. Backlog work has a separate signal from event debounce. Native Darwin descriptor-pressure coverage passed with the race suite and contributor hooks at `9fb8f41e`. The native Darwin functional lifecycle also passed at that revision.

CLI logic now lives in root package `cli/`, uses ff/v4, and exposes persistent `--state-dir`. Session status and an authenticated Unix control socket support `wait-for --ready`, `--stop`, and an expected `--session` token. Terminal results survive process exit. Recovery binds the session to an absolute state path and durable drain generation before final I/O. Tests cover old-token rejection, stop during upload, repeated stops, startup and delivery failures, long socket-workspace paths, crash after durable completion, cancellation/busy recovery, and missing recovery state.

A new stop request may extend an interrupted drain boundary to the newly captured source maximum, retaining its generation and consumed final allowances. Ordinary restart polling preserves the original boundary; arrivals after the new stop capture remain excluded. This refines the original plan's retained-boundary wording so a newly started build cannot be omitted from successful shutdown.

The CLI subprocess composition tests pass for readiness → stop → late wait, failed startup, and failed drain. Extended watcher, GC, tsnet and observability VMs passed; final aggregate checks are tracked below. Test files were consolidated next to their source files, with benchmarks in `bench_test.go`; bare test goroutines and sleeps were replaced with joined tasks and eventual assertions. No repository-owned `internal/`, `pkg/`, or `pkgs/` directories remain.

## Watch filtering performance

Actual production liveness calls, including statement construction, parameter allocation and SQLite work; ten samples each, Go 1.27.1, Linux amd64, Intel i9-13900K. Fixtures contain valid paths with Nix's unique path index. Missing/duplicate/error behavior is covered separately by tests.

| Paths | Before → after   | Time change | Allocated bytes change |
| ----- | ---------------- | ----------- | ---------------------- |
| 128   | 506.0 → 231.8 µs | -54.19%     | -68.83%                |
| 512   | 2.125 → 1.018 ms | -52.10%     | -68.56%                |
| 2,000 | 8.690 → 4.245 ms | -51.15%     | -68.40%                |

Benchstat reports p=0.000, n=10 for every timing comparison; timing spread is ±1–3%. Allocation counts fall by 82.67–83.32%. These fixtures isolate query batching; they do not estimate end-to-end upload throughput.

## Evidence logs

- `/tmp/tsnixcache-cancellation-red.log`: codec cancellation before the fix; initial upload test import was corrected separately.
- `/tmp/tsnixcache-response-red.log`: all six success/error NAR/narinfo body-stall cases failed before the fix.
- `/tmp/tsnixcache-streaming-race.log`: passing streaming package race tests.
- `/tmp/tsnixcache-streaming-lint.log`: zero lint issues.
- `/tmp/tsnixcache-ci-baseline.json`: remote baseline check results.
- `/tmp/tsnixcache-gc-red.log`: original stale-snapshot deletion reproduced.
- `/tmp/tsnixcache-gc-prek.log`: passing GC hook run.
- `/tmp/tsnixcache-ci-gc.json`: 26 successful checks on `c1ecbc5`.
- `/tmp/tsnixcache-cache-all-race.log`: passing whole-repository race tests before the final read-drain regression; affected cache/compression packages rerun afterward.
- `/tmp/tsnixcache-read-body-red.log`: server connection remained open before the expired read deadline fix.
- `/tmp/tsnixcache-cache-race.log`: passing cache/compression race tests including connection closure.
- `/tmp/tsnixcache-cache-prek.log`: passing complete native contributor checks.
- `/tmp/tsnixcache-scheduling-before.txt`, `-after.txt`, `-benchstat.txt`: scheduler benchmark evidence.
- `/tmp/tsnixcache-scheduling-red.log`: old layer barrier failed the ready-child regression.
- `/tmp/tsnixcache-upload-followup-probe.log`: confirmed partial-success and hash-race reproduction.
- `/tmp/tsnixcache-live-before.txt`, `-after.txt`, `-benchstat.txt`: paired liveness query benchmark evidence.
- `/tmp/tsnixcache-state-db-prek.log`: state foundation native checks.
- `/tmp/tsnixcache-durable-watch-race.log`: durable watcher/database/CLI race tests before lifecycle integration.
- `/tmp/tsnixcache-session-recovery-race.log`: focused session and recovery race tests (rerun after every recovery fix).
- `/tmp/tsnixcache-cli-conventions-race.log`: passing CLI race tests after ff/v4 migration and task joining.
- `/tmp/tsnixcache-eviction-before.txt`, `-before-clean.txt`, `-after.txt`, `-benchstat.txt`: raw/normalized eviction evidence.

## Finding disposition

| Original finding                        | Implementation and regression evidence                                                                                                                                                                                                                                                                  |
| --------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1. Compressed-cache disk accounting     | `4f6e76eb`: charged partial files, pinned readers, failed-unlink accounting and bounded shared builds. HTTP acceptance verifies oversized raw fallback, exact hashes/sizes, and retry of an already-advertised zstd URL while another object is pinned.                                                 |
| 2. Import cancellation                  | `b0032f03`: context checks between bounded reads, external process cancellation, decoded-byte and decoder-window limits. Cancellation and oversized-stream regressions pass under race detection.                                                                                                       |
| 3. Upload response stalls               | `b0032f03`: bounded body reads with a deadline after headers; both successful and error responses covered. `8b6ac027` also joins upload producers and rejects premature success.                                                                                                                        |
| 4. Retry starvation/backoff             | `824091af`, `197858fa`: transactional discoveries and bounded durable batches; saved ages/backoff survive restarts. More than 1,001 paths, crash boundaries, database replacement and actual disk-full/corruption tests pass.                                                                           |
| 5. Incomplete final discovery           | `197858fa`: persistent boundary and final allowances; discovery spans every page through that boundary. VM stops during a gated two-path batch, then verifies all ten new paths delivered.                                                                                                              |
| 6. Watch/wait lifecycle                 | `197858fa`: readiness token, authenticated explicit stop, durable terminal results and late wait. Actual binary remains alive through 125 seconds without new outputs. README shell recipe passes real build/drain success and failure combinations.                                                    |
| 7. Darwin descriptor growth             | `197858fa`: watch the database directory; polling remains authoritative after notification failure. Native Darwin race tests include reduced descriptor limits and polling-only convergence.                                                                                                            |
| 8–9. Import/GC root races               | `c1ecbc51`, `eb8dee44`, `bd8f3d53`: persistent striped process locks, rollback ownership, post-import freshness, locked pruning and durable directory updates. Real Nix VM pauses import while standalone root GC runs, verifies root retention, then exercises failed re-push and new-import rollback. |
| 10–11. Generated argument/path escaping | `d4b49ecb`: shared hooks, systemd argument escaping, tmpfiles encoding, literal-percent handling and private credential aliases. Hook argv tests and fresh services with spaces, quotes, backslashes and percent signs pass.                                                                            |
| 12. Host-independent contributor checks | `71614428`, `d4b49ecb`: native aggregate, lock-only trigger, duration contract and generation checks. ARM Linux and Darwin execute native checks and actual contributor hooks.                                                                                                                          |
| 13. Upload barriers                     | `92c747a4`: completion-driven scheduler; deterministic blocked-sibling regression and real HTTP latency comparisons above.                                                                                                                                                                              |
| 14. Eviction scans                      | `4f6e76eb`: ordered LRU; bounded bookkeeping and eviction comparisons above.                                                                                                                                                                                                                            |
| P3. Watch database filtering            | `197858fa`: bounded parameterized batches preserving order, duplicates and failure policy; performance comparison above.                                                                                                                                                                                |
| Documentation and operational gauges    | README now documents actual limits, state, expiry, lifecycle, GC arithmetic and rollback. Grafana current-value tiles require fresh raw samples and a fresh successful scrape; provisioned browser tests cover disappearance and recovery.                                                              |

## Follow-up defects fixed during implementation

- Root-run standalone GC originally published metadata owned only by root, excluding the service account. `bd8f3d53` inherits ownership from the managed root directory, rejects symlinks/hardlinks/nonregular files, repairs legacy metadata without replacing held lock inodes, and recovers interrupted publication. Actual root/service tests cover both creation orders and held-inode repair. The regression fails against the previous implementation.
- Darwin resolves `/tmp` through a symlink and gives `/dev/fd/N` path stats a different filesystem identity. `4c5df1cf` canonicalizes test-store ancestors; `9fb8f41e` enumerates descriptor names and calls `fstat` on the descriptors. Native race tests and hooks passed with both corrections.
- Hash parsing now follows Nix canonical forms, validates decoded lengths, retains optional metadata through round trips, and bounds signatures independently of ignored lines (`26307b22`, vendor hash `b4181233`). Dead parser accounting was removed afterward.
- Tool logic moved from command mains into root packages `grafana/` and `flakehash/`. Dashboard emission validates the model and propagates writer errors. Vendor-hash commands use ff/v4, propagate context cancellation, and recompute the vendor tree before comparing it.

- Performance validation exposed full-NAR buffering inside Nix's legacy import subprocess despite bounded Go RSS. The importer now uses serve protocol `AddToStoreNar`, preserves verification on the same descriptor and legacy trust metadata, and joins protocol/process completion. Real imports pass for references, derivers, duplicates, all codecs and concurrent stores. Process tests cover no handshake, no acknowledgement, malformed acknowledgement, nonzero exit after acknowledgement, trailing stdout, inherited descriptors, cancellation and final-byte decoder errors. A reviewer reproduced the trailing-output hang and suppressed decoder error before their fixes; both regressions now pass.
- Python VM fixtures now have complete annotations, Ruff formatting/lint and strict Pyright analysis. These run through the existing flake-checks lint gate; both local checks pass.

## Provisioned acceptance

| Check                                      | Result and evidence                                                                                                                                                                                                                                                                                                                                    |
| ------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Real Nix import versus standalone GC       | Passed, 69.74 seconds; `/tmp/tsnixcache-final-flake-check.log`. Root/service ownership, active import, failed refresh, rollback and collection.                                                                                                                                                                                                        |
| Durable watcher outage/crash/restart/drain | Passed, 144.04 seconds; `/tmp/tsnixcache-final-flake-check.log`. 520 pending paths across SIGKILL/restart, stable retry history, recovery and deterministic multi-page final drain.                                                                                                                                                                    |
| Live tsnet authorization                   | Passed, 34.08 seconds; `/tmp/tsnixcache-final-flake-check.log`. Debug denial, live grant revocation on the same TCP connection, continued authorized reads and restored grant.                                                                                                                                                                         |
| Grafana/Prometheus/browser                 | Passed, 92.48 seconds; `/tmp/tsnixcache-final-flake-check.log`. Real generated dashboard, instant queries and rendered values; neutral unavailable display after failed scrape/removal; both recovery transitions; preserved history and explicit 90-second freshness cutoff while raw series remain in lookback.                                      |
| Exact README CI shell block                | Passed, 42.96 seconds; `/tmp/tsnixcache-final-flake-check.log`. Real Nix builds with successful dependencies; success, build failure, unavailable cache and both failures. Verifies terminal state, child exit, remote dependency metadata, retained pending state and late wait failure. Package resolution alone is redirected to the branch binary. |
| Long idle build session                    | Passed; `/tmp/tsnixcache-lifecycle-idle-result.json`. Actual command stayed alive for 125.000166 seconds; explicit stop and late wait both exited zero with completed generation and no pending work.                                                                                                                                                  |

Module validation additionally covers fresh services with nontrivial paths and a shared duration corpus evaluated by both Nix and the actual Go parser. Linux uses private persistent `StateDirectory`, Darwin retains state for launchd; both allow 90 seconds for shutdown. Module and CLI binaries must be upgraded together.

## Parser and vulnerability verification

All three fuzz targets passed separate 60-second runs and longer 300-second runs. Longer executions: hash parsing 29,208,062 cases, narinfo 54,631,801, base32 91,074,550. Logs: `/tmp/tsnixcache-fuzz-{hash,narinfo,base32}-{60,300}.log`. No failing corpus entries were discovered.

`govulncheck ./...` and verbose analysis found no reachable vulnerabilities or affected imported packages. Module-only GO-2026-5932 concerns unimported, unmaintained `golang.org/x/crypto/openpgp`; it is not classified as an application vulnerability. The final source recheck also passed with zero reachable vulnerabilities or affected imported packages.

## Final performance evidence

The [performance report](2026-09-10-performance.md) preserves baseline/final source identity, raw distributions and all 180 resource cases. Additional cache-hit/lifetime and first-publication durability costs are explicit. Streaming reduces the Nix child's 64 MiB import RSS from about 100.4 MiB to 36.5–36.8 MiB; Go RSS remains independent of NAR size within each codec. Parallel eviction/lookups, complete codec CPU/I/O, cold/warm/HEAD metadata and root durability are measured. Statement coverage and synthetic benchmarks do not prove every concurrent interleaving or predict production throughput.

## Final validation and revision identity

Runtime and acceptance implementation ends at `3407098`; subsequent commits preserve performance evidence, improve CI diagnostics and simplify comments. The [environment record](data/2026-09-10/environment.json) contains the full implementation revision, baseline/stream source adaptations, compiled benchmark identities and production-source hashes. Generated measurements are committed separately from code. Planning and unrelated development files remain local.

| Gate                        | Evidence                                                                                                                                                                                                                                                                                                                                                                                                       |
| --------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Full Go tests and build     | `go test -timeout=10m ./...` and `go build ./...` passed.                                                                                                                                                                                                                                                                                                                                                      |
| Race and coverage           | `go test -race -coverprofile=/tmp/tsnixcache-remediation.cover -timeout=15m ./...` passed; aggregate **87.7% statement coverage**.                                                                                                                                                                                                                                                                             |
| Lint and fixture types      | Full Go lint plus change-relative lint passed; Ruff and strict Pyright passed locally and in the native Nix lint derivation.                                                                                                                                                                                                                                                                                   |
| Dependencies                | `go mod verify`, recomputed vendor hash and final `govulncheck ./...` passed.                                                                                                                                                                                                                                                                                                                                  |
| Contributor hooks           | Complete `prek run --all-files` and mandatory commit hooks passed for each committed group.                                                                                                                                                                                                                                                                                                                    |
| Formatting and generation   | Treefmt, generated SQL drift and native duration/hook contracts passed.                                                                                                                                                                                                                                                                                                                                        |
| Nix packages and VMs        | All 13 Linux VMs and the clean aggregate passed. Logs: `/tmp/tsnixcache-final-flake-check{,-clean}.log`; package/dashboard/artifact builds also passed.                                                                                                                                                                                                                                                        |
| Supported systems           | Full three-system evaluation passed.                                                                                                                                                                                                                                                                                                                                                                           |
| Native ARM Linux and Darwin | All 34 checks passed at `bdd5f541`, including native checks, contributor hooks and Darwin functional lifecycle in both [push](https://github.com/kradalby/tsnixcache/actions/runs/34477391979) and [PR](https://github.com/kradalby/tsnixcache/actions/runs/34477396775) workflows. Subsequent revisions retain their own acceptance results on [PR #2](https://github.com/kradalby/tsnixcache/pull/2/checks). |

One initial sandboxed lint invocation was killed while heavy Nix evaluations/builds overlapped. The native lint aggregate and complete flake rerun subsequently passed. The final rerun used one build job and four cores; no failed or interrupted invocation is counted as successful.

The final conventions follow-up removes stale benchmark numbers and incident narratives from production comments. Comments retain the reasons for resource limits, authorization boundaries and GC accounting; the performance report remains the measurement record. It also clarifies that ownership locks protect active imports independently of the minimum root-age grace period.

The [local PR description](initial-pr-description.md) reflects final behavior and remains unpublished. The [performance report](2026-09-10-performance.md) explicitly retains additional cache-hit and first-publication durability costs, alongside measured memory, eviction, scheduling and query improvements. Coverage and synthetic measurements do not establish exhaustive interleaving coverage or production throughput.
