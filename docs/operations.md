# Operations

## Troubleshoot the server

Start with health and the service log:

```sh
curl -sS http://HOST/health
journalctl -u tsnixcache
```

A healthy server returns `"status":"ok"`. A `503` with
`"store_reachable":false` means the server cannot read the Nix database.

### Failed imports

Check these in order:

1. Search the server log for `import`. Import failures include `nix-store`
   stderr.
2. Check the available store and spool space in `/health`. They may be on
   different filesystems.
3. Inspect `tsnixcache_push_errors_total` by `reason`:

   | Reason          | Meaning                                                              |
   | --------------- | -------------------------------------------------------------------- |
   | `nar_body`      | The NAR request failed, exceeded its limit, or could not be spooled. |
   | `narinfo_body`  | The narinfo request failed or exceeded its limit.                    |
   | `narinfo_parse` | The narinfo was invalid or named an unsupported compression type.    |
   | `narinfo_size`  | The declared decoded NAR size exceeded the configured limit.         |
   | `busy`          | No import slot became available within 30 seconds.                   |

4. For the system store, confirm `nix-daemon` is running and the service user is
   still in `nix.settings.trusted-users`.

For repeated `busy` errors, reduce client parallelism or increase
`--import-concurrency`.

### Degraded startup

The server can start before the Nix database is readable. It binds its listeners
and returns:

```json
{
  "status": "degraded",
  "store_reachable": false
}
```

It keeps trying the database and logs
`nix store database opened, no longer degraded` after recovery. This commonly
happens when the server starts before `nix-daemon` has opened its WAL database.

## Monitor the cache

### HTTP endpoints

| Endpoint              | Purpose                                               | Failure                            |
| --------------------- | ----------------------------------------------------- | ---------------------------------- |
| `GET /health`         | Uptime, store access, available space, and path count | `503` when the store is unreadable |
| `GET /version`        | Build version                                         | —                                  |
| `GET /nix-cache-info` | Cache settings used by Nix                            | —                                  |
| `GET /metrics`        | Prometheus application, Go, and process metrics       | —                                  |
| `GET /debug/`         | pprof, expvar, varz, and force-GC tools               | Depends on the listener            |

### Prometheus

The exporter covers lookups, pushes, rejected pushes, imports, garbage
collection, and store and spool disk use.

These counters measure NAR body bytes transferred:

- `tsnixcache_nar_bytes_served_total`
- `tsnixcache_nar_bytes_received_total`

Received bytes include failed uploads. With zstd serving enabled, served bytes
count the compressed response. Use `rate(...[$__rate_interval])` for throughput
in Grafana.

### Grafana

Build dashboard JSON for file provisioning:

```sh
nix build .#grafanaDashboards
```

The result contains `tsnixcache.json`. Print the same JSON with:

```sh
nix run .#dashboard
```

Scrape at least every 30 seconds. The **Store paths** and **Store disk used**
tiles require a successful scrape and samples newer than 90 seconds; otherwise
they show **Unavailable**. The dashboard refreshes every 30 seconds. Historical
and rate panels keep older samples.

### Logs

```sh
journalctl -u tsnixcache
journalctl -u tsnixcache-watch
```

The watcher logs `watch: uploaded batch` with uploaded, skipped, and failed
counts. Failed paths are logged separately with their errors. `--verbose`
enables debug messages for polling and notification activity.

### Debug endpoints

Every `/debug/` request passes Tailscale's debug-access check. It admits
loopback and Tailscale-range addresses, `TS_ALLOW_DEBUG_IP`, trusted CIDRs, or a
valid debug key. Other clients receive `403`.

On tsnet, every debug request also requires the push grant. On a plain listener,
`/debug/gc` additionally requires `--local-write` because it changes server
state.

Keep plain listeners on loopback. CPU profiles do not have a server-side
duration limit.

## Garbage collection

### Add a rule

Use `services.tsnixcache.gc.rules` or repeat `--gc-rule threshold:age` on the
command line. This module rule:

```nix
services.tsnixcache.gc.rules = [
  {
    threshold = 80;
    olderThan = "20d";
  }
];
```

means: once store usage reaches 80%, remove eligible roots older than 20 days,
then run `nix-collect-garbage`.

The age is a minimum. A root remains until a disk threshold selects a rule that
can remove it. Imports create the root before writing the path and refresh its
age after a successful import, so GC does not reap an active or newly imported
path.

### Test a rule

Run the rule once without changing the store:

```sh
tsnixcache gc --gc-rule 80:20d --dry-run
```

This logs roots it would remove and passes `--dry-run` to
`nix-collect-garbage`. The command refuses a missing `--gcroot-dir`.

### Know what can be removed

The collector considers an entry eligible when it:

- has a hash-shaped name used by tsnixcache;
- is a symlink into the configured store;
- is old enough for the selected rule.

Ownership is inferred from that shape. Keep unrelated hash-named store symlinks
out of the configured tsnixcache root directory. Names such as
`system-*-link` and `booted-system` do not match and are left alone.

Rules should get more aggressive as the disk fills. A rule set such as
`80:5d 90:20d` is accepted but warns because the higher threshold keeps paths
longer. Ages below one minute also warn.

### Understand the threshold

GC calculates store usage as:

```text
(Blocks - Bfree) / Blocks
```

The store disk gauge and dashboard use the same calculation. `df` uses
`used / (used + Bavail)`, so it can show a different percentage when the
filesystem reserves blocks for root.

### Collection cooldown

After a collection frees no space, the next eligible check skips collection
when it also removes no roots and the cooldown has not elapsed. The cooldown is
`max(--gc-interval, 1h)`. Removing any root bypasses it.

`tsnixcache_gc_freed_bytes_total` measures the increase in filesystem-available
blocks around a successful collection. Treat it as a lower bound: concurrent
writes and copy-on-write filesystems can hide reclaimed space.

### Store restrictions

`nix-collect-garbage` targets the default `/nix/store`. GC rules are rejected
with `--store` or a different `--store-dir`, which avoids pruning one store and
collecting another.

Imports and pruning coordinate access to each root. Preserve the configured
root directory and its `.locks` directory during upgrades. A failed re-push
keeps a root that existed before that attempt.

## Upgrade or roll back

Stop watchers, servers, and standalone GC jobs before replacing the binaries.
Preserve:

- the configured GC root directory, including `.locks`;
- watcher retry state;
- the signing key and tsnet state.

Install matching CLI and module versions, then restart the services. Unknown
newer watcher state fails visibly instead of being reset.

Before rolling back, stop every newer process. An older watcher cannot resume a
newer queue, and old and new processes must not write the same root directory at
the same time.

## Limits and recovery reference

### Request limits

| Data                   | Default limit | Response                                            |
| ---------------------- | ------------: | --------------------------------------------------- |
| Compressed NAR request |        16 GiB | `413`; the client does not retry                    |
| Decoded NAR            |        16 GiB | Import rejected by `--max-nar-size` or `maxNarSize` |
| Narinfo request        |         1 MiB | `413`                                               |
| Narinfo signatures     |   1,024 lines | Narinfo rejected                                    |

The compressed and decoded NAR limits are independent.

The server verifies the complete decoded NAR before importing it. Import has a
30-minute deadline. Clients allow up to 35 minutes for the final narinfo request,
unless an earlier overall deadline or cancellation applies.

`--import-concurrency` limits simultaneous imports. Waiting 30 seconds for a
slot returns `503` with `Retry-After: 30`.

Shutdown gives active HTTP requests 60 seconds to finish. If an import is still
running when the process exits, the client can send the path again. Nix imports
leave the path complete or absent.

### Spool and compressed cache

The server checks the spool every 10 minutes:

- unpaired NAR uploads become eligible for removal after 24 hours without a
  write;
- unused compressed NARs become eligible after one hour;
- startup removes abandoned uploads and compressed NARs.

The compressed cache has a 4 GiB file-data budget and evicts the least recently
used files. Files being created or read still count against the budget and
cannot be evicted. Filesystem metadata uses additional space. Only compression
work is concurrency-limited; NAR readers are not capped.

When narinfo compression fails, the server advertises an uncompressed NAR. An
explicit `.nar.zstd` request returns:

| Condition                             | Response                     |
| ------------------------------------- | ---------------------------- |
| Compression busy or cache budget full | `503` with `Retry-After: 10` |
| Other compression or cache failure    | `500`                        |
| zstd serving disabled                 | `404`                        |

The server never sends an uncompressed body under zstd metadata. Two servers
cannot share one compressed-cache directory.
