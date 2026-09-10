# Operations

## Failure and recovery

### Request limits

| Data                   | Default limit | Failure                                            |
| ---------------------- | ------------: | -------------------------------------------------- |
| Compressed NAR request |        16 GiB | HTTP `413`; the client does not retry              |
| Decoded NAR            |        16 GiB | Import rejected by `--max-nar-size` / `maxNarSize` |
| Narinfo request        |         1 MiB | HTTP `413`                                         |
| Narinfo signatures     |   1,024 lines | Narinfo rejected                                   |

The compressed and decoded NAR limits are independent. A closure member larger
than either limit cannot be stored in this cache.

Verification and import decode the same open spool descriptor in two passes, so
unverified data is never imported. Cancellation interrupts stream work and
codec subprocesses after any blocked filesystem call returns.

### Import deadlines and concurrency

Import runs inside the narinfo `PUT` and has a 30-minute server deadline. The
client allows 35 minutes, giving a slow healthy import time to finish.

`--import-concurrency` limits simultaneous imports. A push that waits more than
30 seconds for a slot receives `503` with `Retry-After` and backs off.

### Restarts during import

Shutdown waits up to 60 seconds for active requests. If an import is still
running, the process stops and the client sends that path again later.

Nix store imports are atomic, so the path is either complete or absent. A
restart can waste transfer bandwidth, but it does not leave a partial path.

### Spool maintenance

The spool is swept every 10 minutes:

- unpaired NARs expire after one day;
- completed compressed-cache entries expire after one hour;
- startup removes abandoned spool data under exclusive ownership.

Two servers cannot share one compressed-cache directory.

### Compressed cache behaviour

The compressed cache has a 4 GiB file-data budget. Charges round up to 64 KiB
and include:

- files being created;
- files held by active readers;
- files whose removal failed.

Eviction uses LRU and cannot reclaim a file while it is being read. Filesystem
metadata and allocation overhead consume additional space outside the budget.

A cold zstd narinfo `GET` materialises the compressed NAR to report its exact
hash and size. `HEAD` does not compress. Use uncompressed serving when first-read
latency matters more than transfer size.

When an object cannot enter the compressed cache, narinfo advertises an
uncompressed URL. A direct `.nar.zstd` request receives retryable `503`; it never
receives an uncompressed body under compressed metadata.

### Degraded startup

Nix opens its database in WAL mode. Before `nix-daemon` has opened the database,
tsnixcache may be unable to read it.

The server still binds its listeners and reports:

```json
{
  "status": "degraded",
  "store_reachable": false
}
```

It retries until the database becomes available, then logs
`nix store database opened, no longer degraded`.

### Troubleshooting failed imports

Check these in order:

1. **Server log:** `journalctl -u tsnixcache | grep import`
   includes `nix-store` stderr and usually names the cause.
2. **Disk and health:** `curl -s http://HOST/health`
   shows store and spool capacity. The spool may be on a separate filesystem.
3. **Failure reason:** inspect `tsnixcache_push_errors_total` by `reason`.
   `nar_body`, `narinfo_body`, and `narinfo_parse` indicate rejected input;
   `busy` indicates exhausted import slots.
4. **Nix access:** confirm `nix-daemon` is running and the service remains in
   `nix.settings.trusted-users`. A chroot store needs neither trusted-user
   access nor the system daemon; configure it with `services.tsnixcache.store`.

For repeated `busy` failures, increase `--import-concurrency` or reduce client
parallelism.

## Monitoring

### HTTP endpoints

| Endpoint              | Purpose                                                     | Unhealthy response                 |
| --------------------- | ----------------------------------------------------------- | ---------------------------------- |
| `GET /health`         | Uptime, store reachability, free space, and path count      | `503` when the store is unreadable |
| `GET /version`        | Build version; `dev` when unset by the linker               | —                                  |
| `GET /nix-cache-info` | `StoreDir`, `WantMassQuery`, and `Priority` for Nix clients | —                                  |
| `GET /metrics`        | Prometheus application, Go, and process metrics             | —                                  |
| `GET /debug/`         | pprof, expvar, varz, and force-GC tools                     | Depends on listener type           |

### Prometheus metrics

The exporter covers cache hits and misses, pushes, push errors, imports, import
duration, garbage collection, and store/spool disk use.

The NAR byte counters measure bytes on the wire:

- `tsnixcache_nar_bytes_served_total`
- `tsnixcache_nar_bytes_received_total`

Use `rate(...[$__rate_interval])` for throughput. Prefer Grafana's
`$__rate_interval` to a fixed window such as `[5m]`, which can under-report when
the dashboard step grows beyond that window.

With `serveCompression = "zstd"`, served bytes count the compressed response.

### Grafana dashboard

Build the dashboard JSON for file-based provisioning:

```sh
nix build .#grafanaDashboards
```

The result contains `tsnixcache.json`.

Or print the same JSON:

```sh
nix run .#dashboard
```

The dashboard is checked against metric names in the server source. Operational
tiles require a successful matching scrape and gauge/up samples newer than 90
seconds. Scrape at least every 30 seconds.

Missing, failed, or stale series display **Unavailable**. The default dashboard
refresh can add up to 30 seconds of display delay, while historical and rate
panels retain older data.

The VM suite tests Prometheus queries, Grafana responses, and browser rendering
during scrape failure, target removal, and recovery.

### Compression resource bounds

zstd responses use the bounded spool cache described above. Concurrent
compression is also capped, so unauthenticated NAR reads cannot make disk or
memory grow with the number of readers.

### Watcher logs

`tsnixcache watch` logs `watch: uploaded batch` at info level for each batch. It
includes counts, human-readable bytes, average upload rate, and peak upload
rate.

`--verbose` adds a `watch: path` debug line for each discovered path before
upload. It does not change the batch summary.

### Debug endpoints

`GET /debug/` exposes pprof, expvar, `/debug/varz`, and force-GC tools.

- **tsnet:** every debug request requires the push grant because pprof can
  expose process memory.
- **Plain listener:** debug endpoints are available to every client that can
  open the socket. Keep the listener on loopback. CPU profiles can be requested
  without a duration bound.
- **`/debug/gc`:** this stop-the-world operation is refused on a plain listener
  unless `--local-write` is enabled. The restriction is based on the path
  because the debug index invokes it with `GET`.

### Service logs

```sh
journalctl -u tsnixcache
journalctl -u tsnixcache-watch
```

The first command shows server logs; the second shows client watcher logs.

## Garbage collection

### Configure threshold rules

The server accepts repeatable `gc.rules` module entries or `--gc-rule` CLI
flags. Each rule uses `threshold:age`.

For example, `80:20d` means:

1. wait until the store filesystem reaches 80% usage;
2. prune tsnixcache gcroots older than 20 days;
3. run `nix-collect-garbage`.

Imported paths remain rooted until their age limit. Active imports retain their
roots, and completed imports refresh the root age.

### Test rules with a dry run

Apply rules once without changing the store:

```sh
tsnixcache gc --gc-rule 80:20d --dry-run
```

The command logs which roots it would prune and passes `--dry-run` to
`nix-collect-garbage`. Run it before enabling a new rule set.

The command refuses a missing `--gcroot-dir`; a typo must not trigger collection
without pruning the intended roots.

### Store restrictions

`nix-collect-garbage` always targets `/nix/store` and accepts no store argument.
GC rules are therefore rejected with either:

- `--store`; or
- a `--store-dir` other than `/nix/store`.

This prevents measuring and pruning one store while collecting another.

### Disk usage calculation

The threshold is calculated as:

```text
(Blocks - Bfree) / Blocks
```

Root-reserved blocks remain free in this calculation. `df` instead uses
`used / (used + Bavail)`, where `Bavail` excludes the reserve. The store disk
gauge and dashboard use the GC calculation.

### Root ownership and locking

Imports and pruning share persistent ownership locks. A stale candidate is
checked again while holding its stripe lock.

A failed re-push retains an existing root; rollback removes only a root created
by that import. Root-run GC and service imports share the configured root
directory's ownership.

Keep the `.locks` directory and its inodes intact during upgrades.

### Collection backoff

When a run prunes no roots and frees no space, another `nix-collect-garbage`
does not start until `max(--gc-interval, 1h)` has elapsed. A store filled with
live paths is therefore not scanned every interval.

### Freed-space metric

`tsnixcache_gc_freed_bytes_total` is a lower bound. It records a whole-filesystem
`statfs` delta around collection.

Concurrent writes and copy-on-write filesystems such as btrfs and ZFS can hide
reclaimed space. A zero value does not prove that nothing was collected. The
backoff still limits repeated collection to one attempt per cooldown.

### What can be pruned

Only tsnixcache roots named after a store path hash are eligible. Entries owned
by Nix, including `system-*-link` and `booted-system`, are left untouched
regardless of age.

Rules whose retention ages become looser at higher thresholds are accepted with
a warning. For example, `80:5d 90:20d` keeps paths longer as the disk fills.
Ages under one minute also produce a warning because roots should provide
useful retention after import.

## Upgrading and rollback

### Upgrade

1. Stop old watchers, servers, and standalone GC jobs.
2. Preserve GC roots, `.locks` inodes, and persistent retry state.
3. Install matching CLI and module versions.
4. Restart the services.
5. Push known affected build closures explicitly if old in-memory retries may
   have been lost.

Old binaries do not honour the current root locks. The queue schema is
versioned; an unknown newer schema fails without resetting state.

### Roll back

1. Stop all newer processes.
2. Preserve retry state and roots for later recovery.
3. Install the older version.

An old watcher cannot resume the newer durable queue. Never run old and new root
writers against the same directory at the same time.
