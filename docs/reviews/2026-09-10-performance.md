# Remediation performance

Comparisons below use the dependency-refresh revision `53c7d653` and the remediation implementation with identical fixtures. No elapsed-time thresholds were added to correctness tests.

## Method

Linux amd64, Go 1.27.1, Intel Core i9-13900K, `GOMAXPROCS=4`, local ext4 filesystem (`/dev/sda2`, `rw,relatime`). A private directory under `/home/kradalby` holds fixtures, compressed files and chroot stores; `/tmp` is tmpfs and is not used for measured storage. Filesystem pages remain warm after fixture preparation and cache eviction. These are compressed-cache misses, not cold physical-disk reads.

Ten paired samples alternate baseline/final order. Compiled test binaries exclude compilation from timing. Access logging is discarded in both zstd fixtures. Low-entropy content repeats a fixed block; high-entropy content uses a deterministic SHA-256 chain across the entire file. Distinct-path batches have distinct contents and NAR hashes. Tests verify actual metadata, hashes, decoded sizes and admission behavior, so a raw fallback cannot appear as a fast compressed result.

Baseline test adaptations only bridge APIs: use the original cache constructor, supply identical fixtures in the production package, and omit acquisition of the new GC-root lock around the root durability microbenchmark. Baseline production source and dependencies remain at `53c7d653`.

## Compressed metadata and concurrent misses

Each fixture contains 32 MiB of file content plus NAR framing. HEAD and warm GET use 100 ms calibration per sample; cold GET and concurrent batches use three iterations. A concurrent operation is eight requests released together, with four compression slots. Metadata requests are not labeled with NAR-byte throughput: HEAD and warm GET do not read a NAR.

| Request                                    | Baseline median | Final median |                             Change |
| ------------------------------------------ | --------------: | -----------: | ---------------------------------: |
| Low entropy HEAD                           |        2.193 µs |     2.284 µs | No significant difference, p=0.529 |
| High entropy HEAD                          |        2.244 µs |     2.359 µs |                    +5.13%, p=0.008 |
| Low entropy warm GET                       |        3.708 µs |     5.656 µs |                   +52.52%, p<0.001 |
| High entropy warm GET                      |        3.652 µs |     5.764 µs |                   +57.82%, p<0.001 |
| Low entropy cold GET                       |        11.11 ms |     14.36 ms |                   +29.34%, p<0.001 |
| High entropy cold GET                      |        31.77 ms |     34.60 ms |                    +8.92%, p=0.002 |
| Eight low-entropy requests, same NAR       |        11.02 ms |     14.29 ms |                   +29.73%, p<0.001 |
| Eight low-entropy requests, distinct NARs  |        30.59 ms |     31.52 ms | No significant difference, p=0.315 |
| Eight high-entropy requests, same NAR      |        31.96 ms |     35.08 ms |                    +9.75%, p<0.001 |
| Eight high-entropy requests, distinct NARs |        89.29 ms |     96.38 ms |                    +7.94%, p<0.001 |

The warm path now opens/closes a pinned file lease and participates in shutdown lifetime accounting. Its roughly 2 µs added cost is visible despite the small absolute latency. Cold paths additionally pay bounded context checks and incremental reservation/lifetime bookkeeping. These correctness costs remain; the comparison does not isolate each component's contribution.

Compressed sizes match before and after. Low-entropy output occupies about 3.7 KiB but reserves 64 KiB; high-entropy output occupies just over 32 MiB and reserves 32 MiB plus 64 KiB. Eight distinct high-entropy requests reserve 256 MiB plus 512 KiB. Reservation covers rounded file data; filesystem metadata and allocation overhead remain separate. The real stalled-reader regression measures allocated blocks independently.

Accurate compressed metadata still requires full materialization on a cold GET. HEAD avoids it. Uncompressed serving remains the documented choice when first-read latency matters more than transfer compression; speculative prewarming was not introduced.

## Lookups during eviction

Four workers each perform 128 production cache lookups while another task evicts half the seeded entries. The newest entry remains live throughout. Fixtures use one-byte files with synthetic byte charges to isolate eviction bookkeeping and real file operations. Logging is discarded in both revisions. Ten paired samples alternate order; each measures one batch. Per-lookup clock instrumentation is identical.

| Entries |     Batch baseline→final | Maximum lookup per batch, baseline→final |
| ------- | -----------------------: | ---------------------------------------: |
| 500     | 3.143→1.673 ms (-46.76%) |                 2.529→0.423 ms (-83.26%) |
| 2,000   | 24.25→5.645 ms (-76.73%) |                 23.61→0.341 ms (-98.55%) |
| 8,000   | 323.6→22.68 ms (-92.99%) |                 323.1→0.322 ms (-99.90%) |

The maximum-lookup rows are medians of each batch's slowest lookup, not percentile latency estimates. Final maximum-lookup spread is large (±50–79%), but each before/after comparison has p<0.001. The result supports moving scans and unlink work out of the lookup critical section. At 500 entries allocations increase 32.03%, while allocated bytes decrease 16.51%; larger fixtures reduce both. [Complete distributions](data/2026-09-10/contention-benchstat.txt).

## Import verification, decoding and durability

Import timing uses 4 MiB file payloads plus NAR framing, with ten paired one-iteration samples; verification uses three iterations per sample and root durability uses ten. Each import gets a fresh initialized chroot store and root directory; fixture preparation, store initialization, validation and cleanup are outside the timer. Initial root-lock publication and its fsyncs remain inside the measured import. This measures first publication, rather than a service reusing initialized lock stripes.

| Payload / decoder    | Baseline | Final streaming |  Change |
| -------------------- | -------: | --------------: | ------: |
| Low / none           | 59.71 ms |        81.56 ms | +36.58% |
| Low / zstd Go        | 65.40 ms |        81.86 ms | +25.18% |
| Low / zstd external  | 63.97 ms |        85.20 ms | +33.20% |
| Low / xz             | 67.98 ms |        81.26 ms | +19.54% |
| Low / bzip2          | 128.3 ms |        153.1 ms | +19.27% |
| High / none          | 61.03 ms |        80.19 ms | +31.38% |
| High / zstd Go       | 61.62 ms |        82.69 ms | +34.20% |
| High / zstd external | 62.36 ms |        79.79 ms | +27.94% |
| High / xz            | 66.95 ms |        81.76 ms | +22.12% |
| High / bzip2         | 741.5 ms |        785.9 ms |  +6.00% |

All comparisons have p≤0.023, n=10. These additional startup/durability costs remain. Verification alone changes little: low-entropy zstd Go increases 3.98%, high-entropy external zstd 11.62%; other verification timings have no statistically significant difference in these samples. [Verification distributions](data/2026-09-10/verify-benchstat.txt). Complete distributions, allocations and the intermediate legacy-import remediation comparison are preserved in the [raw results](data/2026-09-10/import-stream-benchstat.txt).

Root creation is 6.064→5.957 ms and refresh 5.992→5.955 ms, neither a significant change. Both revisions already fsync the root directory; no durability guarantee was removed to improve timing. Import preserves verification before Nix, a second decode on the same descriptor, bounded decoder windows and post-import root refresh.

## Full-pipeline memory

The first matrix uncovered a previously missed full-NAR allocation in the Nix child: legacy `--import` parses through a `StringSink`. Go's own streaming RSS had hidden that cost. The importer now negotiates serve protocol ≥2.5 and uses `AddToStoreNar`, following Nix's own client implementation. It retains the legacy trust metadata policy and requires both acknowledgement and successful process exit. Protocol errors, premature exit, trailing output, cancellation, inherited stdout and final-byte decoder errors have regressions. [Nix import implementation](https://github.com/NixOS/nix/blob/2.34.8/src/libstore/export-import.cc#L45-L82), [Nix streaming client](https://github.com/NixOS/nix/blob/2.34.8/src/libstore/legacy-ssh-store.cc#L147-L175).

Resource measurements use Nix 2.34.8, zstd 1.5.7, xz 5.8.3 and bzip2 1.0.8; exact command output and source/binary identities are in [environment.json](data/2026-09-10/environment.json). There are 180 fresh-process cases: 4/64 MiB, low/high entropy, every supported decoder, verify/import, three repetitions; import alternates intermediate legacy and final streaming binaries. Every actual import is hash-checked afterward, with its root target and spool removal verified. Separate preparation processes and a small C fork supervisor avoid carrying Python's earlier RSS peak into the worker's `ru_maxrss`.

Medians below are descriptive, three repetitions per cell. All figures are MiB. “Go” is worker maximum RSS; “Nix” is largest-child maximum RSS. The last column is sampled process RSS summed across Go and descendants, excluding the supervisor; shared pages may be counted more than once. It is not physical memory usage, and approximately 2 ms sampling can miss short peaks.

| Payload / decoder    | Go final, 4→64 MiB | Nix legacy, 4→64 MiB | Nix final, 4→64 MiB | Process RSS at 64 MiB, legacy→final |
| -------------------- | -----------------: | -------------------: | ------------------: | ----------------------------------: |
| Low / none           |            9.2→9.3 |           38.8→100.4 |           36.6→36.5 |                          109.3→46.3 |
| Low / zstd Go        |          15.8→16.0 |           38.8→100.5 |           36.5→36.6 |                          116.2→52.5 |
| Low / zstd external  |            9.7→9.5 |           38.8→100.5 |           36.5→36.5 |                          115.3→51.5 |
| Low / xz             |            9.7→9.5 |           38.8→100.4 |           36.6→36.8 |                          119.4→56.2 |
| Low / bzip2          |          17.0→17.1 |           39.0→100.4 |           36.6→36.5 |                          117.5→53.8 |
| High / none          |            9.0→9.1 |           39.0→100.5 |           36.6→36.5 |                          109.2→45.3 |
| High / zstd Go       |          16.1→16.1 |           38.8→100.4 |           36.6→36.5 |                          116.5→52.8 |
| High / zstd external |            9.6→9.6 |           38.7→100.4 |           36.8→36.8 |                          117.0→52.7 |
| High / xz            |            9.5→9.7 |           38.8→100.5 |           36.6→36.8 |                          120.7→56.4 |
| High / bzip2         |          17.5→18.2 |           38.8→100.3 |           36.4→36.7 |                          118.8→54.8 |

The Nix child's former increase tracks the decoded NAR size; streaming removes it on the pinned chroot implementation. Integration VMs verify the pinned daemon path too. Ancient daemons can still buffer forwarded imports, so README requires matching recent client/daemon versions. Measurements at two sizes support the bounded implementation; they do not establish a universal RSS ceiling for every Nix version or input.

## Decode CPU and disk I/O

The following final 64 MiB cases report CPU milliseconds for verification and full import. Each cell is Go / waited children; full import includes both decoder passes and Nix. These are CPU counters, not the resource probe's sampled elapsed time. The unsampled benchmark matrix above supplies latency comparisons.

| Payload / decoder    | Verify CPU, Go / children | Import CPU, Go / children |
| -------------------- | ------------------------: | ------------------------: |
| Low / none           |                44.1 / 0.0 |               78.6 / 90.3 |
| Low / zstd Go        |                52.3 / 0.0 |               88.0 / 89.0 |
| Low / zstd external  |               69.6 / 28.3 |             116.7 / 143.3 |
| Low / xz             |               63.3 / 50.2 |             122.4 / 195.5 |
| Low / bzip2          |               573.1 / 0.0 |             1126.1 / 90.0 |
| High / none          |                41.2 / 0.0 |               74.0 / 81.6 |
| High / zstd Go       |                62.0 / 0.0 |              111.4 / 81.9 |
| High / zstd external |               85.7 / 47.7 |             161.7 / 183.6 |
| High / xz            |               89.7 / 43.5 |             176.2 / 179.6 |
| High / bzip2         |              5486.9 / 0.0 |            10806.7 / 98.7 |

High-entropy bzip2 remains CPU-expensive. Two-pass verification is retained; no extra decoded spool or unverified store import was introduced. Decoder CPU and RSS explain why cumulative Go allocation counts alone cannot describe the full pipeline.

Raw [resource counters](data/2026-09-10/resource-stream.json) preserve `rchar`, `wchar`, `read_bytes`, `write_bytes` and `cancelled_write_bytes` independently. `/proc/self/io` includes waited children; adding their I/O again would double count. Fixture copies leave warm, potentially dirty pages. Removing the spool can cancel preparation-process writeback, so `write_bytes - cancelled_write_bytes` is not a valid estimate of this import's disk writes. Filesystem cache and metadata overhead are outside the RSS figures. [Linux process I/O](https://man7.org/linux/man-pages/man5/proc_pid_io.5.html), [resource usage](https://man7.org/linux/man-pages/man2/getrusage.2.html).

Warm high-entropy 64 MiB full imports, median counters in MiB. Logical character counts include pipe transfers and store work; `read_bytes` is zero for these warm fixtures. Written and canceled-write counters remain separate.

| Decoder       |  rchar |  wchar | write_bytes | cancelled_write_bytes |
| ------------- | -----: | -----: | ----------: | --------------------: |
| none          | 192.18 | 128.02 |       64.07 |                 64.04 |
| zstd Go       | 192.18 | 128.02 |       64.07 |                 64.04 |
| zstd external | 448.19 | 384.03 |       64.07 |                 64.04 |
| xz            | 448.31 | 384.03 |       64.07 |                 64.04 |
| bzip2         | 192.75 | 128.02 |       64.07 |                 64.32 |

## Reproduction and provenance

Run the committed Go benchmarks inside the flake dev shell, using a private directory on the measured filesystem as `TMPDIR` and `GOMAXPROCS=4`. Use ten samples, `-benchmem`, and the iteration counts above; alternate baseline/final binaries rather than running all samples of one first. Extract `.patch` from the [baseline fixture adaptations](data/2026-09-10/baseline-fixtures.json) with `jq -r`, then apply it to an archive of `53c7d653`. The legacy and stream source patches plus binary hashes preserve the intermediate implementations.

`TestImportResourceProbe` runs only with `TSNIXCACHE_RESOURCE_PROBE=1`. In a fresh directory, prepare `resource-config.json` with operation `prepare`, payload bytes, entropy (`high`) and compression; preparation writes the compressed spool and narinfo. For each measured worker, copy these to a separate fresh directory, initialize its store outside timing, then select operation `verify` or `import` and `external`. Run the compiled test with `-test.run=^TestImportResourceProbe$`; it writes `resource.json`. Post-check imported Nix hashes, root links and spool removal. Exclude preparation and validation processes from resource accounting.

## Earlier improvements

The [remediation verification report](2026-09-09-remediation-verification.md) records separate paired comparisons for ordered eviction, completion-driven uploads and batched liveness queries. Those improvements do not cancel the additional per-request correctness costs shown here: each measurement describes its own workload.
