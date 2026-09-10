# tsnixcache initial PR review

Reviewed PR [#2](https://github.com/kradalby/tsnixcache/pull/2), branch `initial` against `origin/main`, at `da29eb6c28ee342a9d14b2be018a2a15d4b9406e` plus the dependency refresh in the working tree. Three specialist reviewers covered the full implementation, tests, modules, CI and documentation; the primary reviewer consolidated findings and reproduced GC races. Separate performance passes measured scheduling, eviction, cold reads and database filtering. Existing untracked development files were preserved. This document records the original findings. Current implementation and validation status live in [remediation verification](2026-09-09-remediation-verification.md).

The dependency refresh passes Go tests and builds, race tests, lint, package/dashboard Nix builds, supported-platform evaluation and macOS ARM64 cross-compilation. The implementation still has reliability and resource-accounting defects worth fixing before merging. Passing tests do not cover the interaction scenarios that exposed these findings.

## Dependency changes

- Refreshed all flake inputs; changed nixpkgs, flake-checks and Headscale. Headscale is now v0.29.3 and follows the root nixpkgs, removing the obsolete separate pin.
- Updated Go to 1.27.1 and 36 pinned modules. Upgrades were grouped by SQLite, compression, UI/observability, Go libraries, networking and gVisor; full `go test ./...` and `go build ./...` passed after every successful group.
- SQLite is v1.58.0 with **libc v1.75.6**, exactly the version declared by SQLite. The newer libc v1.75.7 is intentionally excluded. gVisor uses the latest generated `go` branch; its normal development branch cannot be built with standard Go tooling.
- Refreshed `flakehashes.json`; module checksums verified. All build helpers still use the latest Go toolchain. Shortened stale toolchain comments and set an explicit state version in module-evaluation fixtures.
- Current nixpkgs rejects Intel macOS. Flake outputs now cover `x86_64-linux`, `aarch64-linux`, and `aarch64-darwin`; README documents the change. Native Intel macOS packaging is no longer exposed.

## Findings requiring fixes

All findings below are **P2**: concrete failures under the stated conditions. Source links refer to the dependency-refresh snapshot, before remediation.

### 1. Compressed-cache eviction does not bound actual disk use

[cache/cache.go:581](https://github.com/kradalby/tsnixcache/blob/53c7d653/cache/cache.go#L581)

With zstd serving enabled, a reader can request a NAR, receive headers and then stop reading. Eviction unlinks the file and subtracts its bytes immediately, but the response still holds an open descriptor. Its disk blocks remain allocated until the response ends; there is no stalled-write deadline. Repeated downloads of evicted paths allocate additional copies while accounting remains within budget. No push capability is required. Encoding temp files also consume unaccounted space before completion.

**Evidence:** real TCP/compression paths, with only the test budget reduced to 8 MiB: four 6 MiB downloads left 6,291,725 tracked bytes plus 18,875,175 bytes in three open, deleted files. Each individual file fit the budget; compression concurrency was one. A separate concurrent-encoding test observed roughly 24 MiB before eviction. The host filesystem was not filled.

**Fix:** reserve space during compression, retain reservations while readers hold files, and release only when disk space can actually be reclaimed. Bound stalled response writes and decline/fall back when reservations are unavailable. The intentional oversized-single-entry exception should be documented separately.

### 2. Import verification ignores cancellation

[niximport/niximport.go:338](https://github.com/kradalby/tsnixcache/blob/53c7d653/niximport/niximport.go#L338)

The 30-minute import context does not bound in-process decoding and hashing: `io.Copy` and the none/zstd/bzip2 decoders never inspect it. The declared `NarSize` is supplied by the pusher, so it is not an independent work limit. Slow or oversized verification can retain CPU, I/O and an import slot beyond the deadline.

**Evidence:** an already-cancelled context still verified and accepted the full 17.8 MiB fixture for none and zstd. **Fix:** check cancellation during copying, propagate it before root/import work, and test both pre-cancelled and mid-stream cancellation. This is a robustness failure within the documented trusted-pusher model.

### 3. Response-body stalls bypass the upload watchdog

[upload/upload.go:676](https://github.com/kradalby/tsnixcache/blob/53c7d653/upload/upload.go#L676), [error-response drain:766](https://github.com/kradalby/tsnixcache/blob/53c7d653/upload/upload.go#L766)

The NAR watchdog stops when HTTP response headers arrive. Success and error response bodies are subsequently drained without their own deadline. A server/proxy that sends headers and then stalls can therefore freeze a default manual push or the watcher's synchronous poll loop indefinitely. Retries cannot begin until that attempt returns.

**Evidence:** a real test HTTP server sent `200`, declared a ten-byte body, and stopped. A 100 ms stall timeout did not release the upload after 500 ms; external cancellation did. The drain error was then ignored. **Fix:** keep a bound through response processing, cap or avoid drains, and propagate body/cancellation errors. Cover success and error responses.

### 4. Retry overflow starves later store paths and defeats backoff

[watch/watch.go:553](https://github.com/kradalby/tsnixcache/blob/53c7d653/watch/watch.go#L553)

The default 512-entry retry queue is smaller than the 1,000-row discovery page. When a page fails, eviction rewinds the discovery cursor. Subsequent polls repeatedly discover that same failing page, so later independent paths are never reached. Already-queued rows are treated as new work and submitted before their retry time; evicted rows also lose their first-failure age.

**Evidence:** 1,001 independent rows and three immediate polls produced 3,000 submissions for the first 1,000 discoveries, despite zero queued retries being due. Cursor repeatedly returned to zero; the final row was never attempted.

**Fix:** separate a monotonic discovery cursor from retry/replay state. Give overflow replay a bounded share of work or persist it, preserve failure ages, and exclude queued rows from fresh submissions until due. This is both a convergence defect and substantial outage-load amplification.

### 5. Shutdown silently drains only one database page

[watch/watch.go:348](https://github.com/kradalby/tsnixcache/blob/53c7d653/watch/watch.go#L348)

`finish` calls `run` once. If more than 1,000 independent paths arrived since the cursor, it uploads only the first page. Undiscovered rows are absent from the retry queue, so an empty queue causes a clean exit without warning. Restart establishes its baseline at the current maximum ID, permanently overlooking them.

**Evidence:** actual SQLite/poller code with 1,001 rows and successful uploads: 1,000 uploaded, one unseen, zero queued, well inside the shutdown timeout. **Fix:** capture a final discovery high-water mark and drain pages through it within the deadline; report/preserve undiscovered work as well as queued failures.

### 6. Watcher completion and the documented CI lifecycle do not compose

[cmd/tsnixcache/watch.go:93](https://github.com/kradalby/tsnixcache/blob/53c7d653/cmd/tsnixcache/watch.go#L93), [cmd/tsnixcache/waitfor.go:55](https://github.com/kradalby/tsnixcache/blob/53c7d653/cmd/tsnixcache/waitfor.go#L55)

Clean watcher exit deletes the PID file. A later `wait-for` cannot distinguish successful completion from failure to start, waits two minutes, then fails. The README's pre-build `--idle-exit 120s` can also stop the watcher during a long build that has not registered outputs yet.

**Evidence:** the real watch command exited successfully after a short idle window; subsequent `waitForPidFile` timed out. Existing wait-for tests retain a hand-written exited PID file, missing the real composition.

**Fix:** tie watcher lifetime to explicit build completion and retain a completion/status record until consumed. Account for startup failures and PID reuse; treating every missing file as success would mask failed starts.

### 7. Large macOS stores can exhaust watcher file descriptors before polling starts

[watch/watch.go:195](https://github.com/kradalby/tsnixcache/blob/53c7d653/watch/watch.go#L195)

fsnotify v1.10.1 uses **kqueue**, not the documented FSEvents backend. Adding `/nix/store` enumerates its immediate entries and opens a descriptor for each. Beyond the effective process limit, `Add` returns `EMFILE`; `Watch` exits before the DB polling loop exists, and launchd restarts it. Even below the limit, descriptor use scales with store size.

**Evidence:** traced through the exact pinned dependency's `backend_kqueue.go` (`Add`, `watchDirectoryFiles`, `internalWatch`, `unix.Open`). This finding is conditional on the descriptor limit and was not run on a Darwin host.

**Fix:** use a bounded event source/native FSEvents backend, or degrade gracefully to working DB polling when event setup fails. Add a Darwin test under a deliberately constrained descriptor limit and correct the backend claims.

### 8. A failed import can remove a concurrent successful import's root

[niximport/niximport.go:113](https://github.com/kradalby/tsnixcache/blob/53c7d653/niximport/niximport.go#L113)

First import creates the GC root and blocks. A second import of the same path refreshes that root and succeeds. The first import later fails and unconditionally removes the root by name. The successfully imported path can then be collected before its promised retention age.

**Evidence:** two actual `Importer.Import` calls, separate spools, shared root directory and a controlled nix-store test shim reproduced this ordering. The second returned success before the first failure removed its root. The implementation already acknowledges a related concurrent-import limitation; it remains an active reliability risk.

**Fix:** coordinate per-path import/root ownership and rollback across overlapping imports. Do not remove a root that another completed/in-flight import relies on; support multiple processes if independent serve/gc commands share the directory.

### 9. GC pruning removes roots refreshed after its stale snapshot

[cmd/tsnixcache/gc.go:414](https://github.com/kradalby/tsnixcache/blob/53c7d653/cmd/tsnixcache/gc.go#L414)

Pruning selects stale roots, then later removes their paths without coordinating with `addGCRoot`'s atomic replacement. A push between selection and deletion creates a fresh root which GC then unlinks. The following collection can reap a newly pushed path despite the configured age threshold.

**Evidence:** actual `pruneOldGCRoots`, an aged root, and deterministic replacement after stale selection: the replacement's fresh mtime was verified, yet pruning removed it. **Fix:** coordinate refresh/prune operations, including the standalone GC process. A separate last-second stat alone still leaves a check/unlink race.

### 10. Generated client hooks treat valid URLs as shell syntax

[nix/module-client.nix:33](https://github.com/kradalby/tsnixcache/blob/53c7d653/nix/module-client.nix#L33), [Darwin counterpart:34](https://github.com/kradalby/tsnixcache/blob/53c7d653/nix/module-client-darwin.nix#L34)

`postBuildHook.to` is interpolated unquoted into executable shell text and into a warning string. Valid URL path characters such as `&` or `;` truncate the invocation, dropping timeout and output paths. Parentheses can make the script invalid and return status 2, failing a build despite its best-effort contract.

**Evidence:** evaluated module hook plus harmless argv recorder: `http://cache/a&b` yielded only `push --to http://cache/a` and `b: command not found`; a parenthesised URL produced a shell syntax error. **Fix:** shell-escape all configured arguments and the complete warning text. Preserve intentional splitting of Nix-provided `OUT_PATHS`. This is trusted-configuration correctness, not a privilege-escalation claim.

### 11. Custom directories with spaces break generated tmpfiles rules

[nix/module-server.nix:352](https://github.com/kradalby/tsnixcache/blob/53c7d653/nix/module-server.nix#L352)

Server directory strings are quoted in commands but not in tmpfiles rules. `spoolDir = "/tmp/tsnixcache review/spool"` passes Nix evaluation yet generates an invalid rule. Missing directories then prevent the service namespace from starting before preStart can repair them.

**Evidence:** actual evaluated rule passed to `systemd-tmpfiles --dry-run --create` returned 65: `Failed to resolve user '0750'`. **Fix:** escape for tmpfiles syntax, including whitespace/specifiers, or use typed settings that do so. Test a fresh deployment with nontrivial directory names.

### 12. Pre-commit Nix checks require an x86_64 Linux builder on every host

[.pre-commit-config.yaml:43](https://github.com/kradalby/tsnixcache/blob/53c7d653/.pre-commit-config.yaml#L43)

The documented contributor hooks build `checks.x86_64-linux` unconditionally. Supported ARM Linux/macOS contributors without a matching remote builder cannot commit dependency/flake changes even when native checks pass. Darwin CI does not run these hooks.

**Fix:** select native checks for local hooks and leave cross-platform coverage to CI. Include `flake.lock` in the hook filter so lock-only changes trigger validation. This finding is statically established by the explicit target and conditional on builder availability.

## Performance findings

These are controlled comparisons, **not production throughput claims**. The fixtures and test-only alternatives are preserved in the linked specialist reports.

### 13. P2 — Topological-layer barriers leave upload capacity idle

[upload/upload.go:471](https://github.com/kradalby/tsnixcache/blob/53c7d653/upload/upload.go#L471)

Every path in a dependency layer must finish before the next layer starts. A long independent sibling therefore delays dependents of already-finished short paths even when a worker is free.

**Measured:** same two-worker cap, one independent 250 ms path and six 25 ms dependent paths. Current scheduler took **377–380 ms**, versus **250 ms** for a test-only ready queue. The small chain finished roughly **225 ms later** under the current scheduler, across three runs using the actual uploader and controlled HTTP delays.

**Fix:** schedule a path as soon as its own references complete, using pending-dependency counts and a ready queue. Retain bounded workers, deterministic failure propagation and self-reference handling.

### 14. P2 — LRU eviction repeatedly scans the map under the global cache lock

[cache/cache.go:590](https://github.com/kradalby/tsnixcache/blob/53c7d653/cache/cache.go#L590)

Each eviction scans every remaining entry for the oldest, giving O(N × K) work for K victims. Compression of a large new entry into a cache containing many small entries can therefore block all cache lookups while thousands of victims are selected and unlinked.

**Measured:** synthetic accounted sizes and real tiny files, evicting roughly half: **3 ms** at 501 entries, **32 ms** at 2,001, **387 ms** at 8,001. The production budget was unchanged in this experiment; these measure eviction bookkeeping/lock hold, not compression speed.

**Fix:** sort victims once per batch, or maintain an ordered LRU structure. Coordinate unlink/accounting changes with active-reader reservations from finding 1. Add a many-small-entries plus large-insertion benchmark.

### Lower-priority costs and deliberate tradeoffs

- **P3, watcher DB filtering:** [watch/watch.go:467](https://github.com/kradalby/tsnixcache/blob/53c7d653/watch/watch.go#L467) issues one SQLite query per path. For 1,000 rows: median **4.07 ms, 577 kB and 17,008 allocations**, versus a batched test control's **2.19 ms, 163 kB and 3,027 allocations**. Lower priority at a 30-second polling interval; batch if profiling shows it matters.
- **Cold zstd metadata latency:** GET narinfo serialises/compresses the entire NAR before replying. A page-cache-warm 32 MiB fixture measured **15–37 ms cold** versus roughly **10 µs warm**. HEAD correctly skips compression. This is an architectural cost, not a new correctness defect: accurate compressed size/hash require materialising bytes. Consider prewarming or the existing uncompressed serving mode when first-read latency matters.
- **Two-pass import:** verification and import each decode the stream. For a synthetic repeated-data 32 MiB NAR, the second decode added about **8 ms zstd / 38 ms xz**, excluding nix-store work. The design intentionally trades CPU for flat memory and verification before import. Do not remove verification to improve a benchmark; a verified uncompressed spool would instead trade disk writes/capacity for decode CPU.
- Retry amplification and macOS O(store-entry) descriptors are also material performance findings, described above. Per-import directory fsync preserves durability; no representative measurement supports weakening it.

## Documentation and maintainability

| Claim/location                                                               | Required correction                                                                                                                                                                                   |
| ---------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| README GC reserve/`df` arithmetic                                            | Code and dashboard use `(Blocks-Bfree)/Blocks`; reserved blocks count as free, and `df` has a different denominator.                                                                                  |
| README/Darwin module/test claim FSEvents                                     | Current dependency uses kqueue; describe actual backend and its limits until changed.                                                                                                                 |
| README says `serve --external-compression` enables external encoding         | Server encoding passes `false` to Encoder; the flag currently affects importer decoding.                                                                                                              |
| README/dashboard imply a hard 4 GiB cache ceiling                            | Document the oversized singleton exception and fix active/in-progress allocation accounting.                                                                                                          |
| Nix duration comments/checks say push/watch use `flag.Duration` without days | CLI now uses `dayDuration`. Nix types are deliberately narrower, and current checks compare duplicated expectations rather than executing both parsers.                                               |
| README/CI refer to `garnix.yaml`                                             | File was deleted. Actual PR checks prove Linux Garnix checks still run; correct the reference without claiming absent CI.                                                                             |
| Client modules say uploads shell out to `nix copy`                           | They call `nix path-info` for metadata and upload through HTTP themselves.                                                                                                                            |
| Import timeout comments disagree with server/client                          | Semaphore wait is bounded to 30 seconds; narinfo PUT is exempt from the client stall watchdog.                                                                                                        |
| Flake/sigs comments claim the only signature-checking test                   | Push-hook test also checks signatures; remove brittle exclusivity claims.                                                                                                                             |
| Dashboard promises blank values when metrics disappear                       | `lastNotNull` on range queries retains historical values. Use current-value/liveness queries or describe that retention. Grafana rendering was not executed.                                          |
| PR #2 description                                                            | Still names `dalby.cc/cap/tsnixcache`, describes watch as wrapping `nix copy`, and describes obsolete codec fallback. Current capability is `kradalby.no/cap/tsnixcache`. Refresh before publication. |

Simplify long historical comments, especially in upload, watch, store and dashboard. Keep durable invariants and the reason for limits/order; move measurements and past incident narratives into benchmark/design notes. Share generated hook construction between Linux and Darwin to avoid duplicating quoting fixes. Prefer focused composition tests over more assertions that mirror implementation internals.

## Validation and limits

- Full Go tests/build after each successful dependency group: passed.
- `go test -race -coverprofile=... -timeout=10m ./...`: passed; **86.4% statement coverage**. Auth, humanise and base32 reached 100%; this is not exhaustive branch/interleaving coverage.
- `golangci-lint run --new-from-rev=origin/main`: **zero issues**.
- `go mod verify`: passed; final module-update audit leaves only the intentional SQLite/libc pairing behind the newest patch.
- `treefmt --fail-on-change`: passed, zero changed files; `git diff --check`: passed.
- Nix default binary, dashboard generator and Grafana JSON builds: passed.
- Supported-platform flake evaluation and example Darwin system evaluation: passed. Darwin ARM64 Go cross-build: passed. Native Darwin and ARM Linux execution were unavailable on this host.
- `govulncheck`: zero reachable symbol vulnerabilities and zero affected imported packages. One module-only advisory, [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932), concerns unused `golang.org/x/crypto/openpgp`; it is not reported as an application vulnerability.
- Full `nix flake check --keep-going -L`: **passed**, including all ten NixOS VM tests (serve, push, store, signatures, GC, hook, watch, both, best-effort hook and tsnet). The final recheck after the supported-system list's formatting cleanup also passed; only the formatting derivation needed rebuilding.

The strongest existing tests compare NAR bytes and signatures against real Nix, exercise actual chroot imports, and cover parser invalid inputs and atomic spool replacement. Missing interaction tests explain most findings: response headers followed by stalled bodies; queue overflow with forward discovery; shutdown beyond one page; real watch completion followed by wait-for; active readers during cache eviction; import rollback/prune overlapping root refresh; and Darwin descriptor pressure.

Host Go coverage does not include VM subprocess coverage. Low percentages in tsnet setup/chroot setup and command main functions should therefore be interpreted alongside VM coverage, not as proof those paths were never tested. Parser fuzzing and real Nix-vs-Go duration contract tests would complement the existing table tests.

No authentication bypass, signature forgery or escaping spool import was established. Anonymous reads and fully trusted pushers are explicit product decisions, not additional findings. Rejected candidates included HEAD triggering compression and a lone oversized cache entry evicting itself; both were disproven by code/tests.

## Evidence artifacts

- [Reliability review and repro log](/tmp/tsnixcache-review-reliability.md), [repro output](/tmp/tsnixcache-review-reliability-repros.log).
- [Security/resource review](/tmp/tsnixcache-review-security.md); Go overlays/tests in `/tmp/tsnixcache-security-probes`.
- [Packaging/docs review](/tmp/tsnixcache-review-packaging-docs.md), including evaluated-hook/tmpfiles reproductions.
- [Upload/watch performance comparisons](/tmp/tsnixcache-review-performance.md) and [cache/import performance measurements](/tmp/tsnixcache-review-security-performance.md).
- GC reproductions: `/tmp/tsnixcache-review-gc-work`, `go test ./niximport ./cmd/tsnixcache -run '^TestReview' -count=1 -v`.
- [Race/coverage log](/tmp/tsnixcache-test-race.log), [coverage functions](/tmp/tsnixcache-coverage-summary.txt), [Nix check log with VM output](/tmp/tsnixcache-flake-check.log), [final Nix check](/tmp/tsnixcache-flake-check-final.log), [vulnerability scan](/tmp/tsnixcache-govulncheck-verbose.log).
