# tsnixcache

Nix binary cache that serves reads from `/nix/store` and accepts pushes via the standard Nix HTTP cache protocol. Pushed paths are imported into `/nix/store` through Nix's streaming store protocol — no separate storage, GC is ordinary `nix-collect-garbage`. Optionally exposed on one or more Tailscale networks via tsnet (writes gated by capability grant).

## Setup

### 1. Generate a signing key

On a fresh cache host nothing is installed yet, so run the binary straight out
of the flake. The directory has to exist before the shell can open the
redirect, and the `umask` is what keeps the secret unreadable for the length of
the write (`key generate` also chmods a regular-file stdout to 0600, but only
after the shell has created it):

```
sudo mkdir -p /etc/tsnixcache
sudo sh -c 'umask 077; nix run --extra-experimental-features "nix-command flakes" \
  github:kradalby/tsnixcache -- key generate --name cache.example.com > /etc/tsnixcache/key'
```

Only the secret key goes to stdout, so the redirected file contains exactly
`name:base64-secret`; the matching public key is printed on stderr. Derive the
public key again at any time — once the server module is deployed, `tsnixcache`
is on the host's `PATH`:

```
sudo tsnixcache key public /etc/tsnixcache/key
# cache.example.com:base64-public
```

Back the key file up. Lose it and every path the cache already served stays put but
can no longer be verified by clients; you must generate a new key and re-trust it
everywhere.

### 2. Server (NixOS)

```nix
{
  imports = [ inputs.tsnixcache.nixosModules.tsnixcache ];

  services.tsnixcache = {
    enable = true;
    package = inputs.tsnixcache.packages.${pkgs.system}.default;
    signKeyFile = "/etc/tsnixcache/key";

    # Local listener. Nothing identifies a caller here, so it serves reads only
    # — enough to be a substituter — and refuses writes unless localWrite is
    # set. Configuring tsnet makes this default to [ ], i.e. tsnet only; with no
    # tsnet it falls back to 127.0.0.1:5000, since otherwise there'd be nothing
    # to serve on.
    listen = [ "127.0.0.1:5000" ];

    # One or more tsnet instances (reads open, writes need cap grant)
    # hostname defaults to "tsnixcache" → accessible as http://tsnixcache on the tailnet
    tsnet = [
      {
        authKeyFile = "/run/secrets/tailscale-authkey";
        dir = "/var/lib/tsnixcache/tsnet-cache";
      }
    ];
  };
}
```

The module puts `package` on `environment.systemPackages`, so `tsnixcache key`
and `tsnixcache gc` are on the cache host's `PATH` once it is deployed.

`package` can be left out entirely if you add `inputs.tsnixcache.overlays.default`
to `nixpkgs.overlays`: all three modules then default to `pkgs.tsnixcache`, built
from your own `pkgs` rather than the flake's pinned nixpkgs.

The service user is added to `nix.settings.trusted-users` **unless** a chroot
store (`store`, below) is configured: importing into the system `/nix/store`
needs that trust, importing into a store the service owns outright does not.

#### Options

| Option             | Default                                              | Notes                                                                                                                         |
| ------------------ | ---------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `enable`           | `false`                                              |                                                                                                                               |
| `package`          | `pkgs.tsnixcache`                                    | needs the flake's `overlays.default`, or set it explicitly                                                                    |
| `signKeyFile`      | `null`                                               | path _as a string_; a Nix-store path is refused, since the store is world-readable                                            |
| `listen`           | `[ "127.0.0.1:5000" ]`, or `[ ]` when `tsnet` is set | read-only unless `localWrite`; reads are unauthenticated, and a non-loopback entry only warns                                 |
| `localWrite`       | `false`                                              | accept pushes on `listen` addresses too — see [Security and trust model](#security-and-trust-model) before setting it         |
| `tsnet`            | `[ ]`                                                | each `{ hostname = "tsnixcache"; authKeyFile; dir; port = 80; tls = false; controlUrl = ""; }`; `tls = true` fails evaluation |
| `priority`         | `30`                                                 | lower wins; `cache.nixos.org` is 40, so this cache is preferred over it                                                       |
| `serveCompression` | `"none"`                                             | `"none"` or `"zstd"`                                                                                                          |
| `spoolDir`         | `/var/cache/tsnixcache/spool`                        | incoming NARs land here before they are verified and imported                                                                 |
| `db`               | `null` → `/nix/var/nix/db/db.sqlite`                 | opened read-only; mutually exclusive with `store`                                                                             |
| `storeDir`         | `null` → `/nix/store`                                | mutually exclusive with `store`                                                                                               |
| `gcrootDir`        | `null` → `/nix/var/nix/gcroots/tsnixcache`           | mutually exclusive with `store`                                                                                               |
| `store`            | `""`                                                 | serve a self-contained chroot store rooted here instead                                                                       |
| `maxNarSize`       | `17179869184` (16 GiB)                               | decoded NAR byte limit, independent of the compressed request limit                                                           |
| `gc.rules`         | `[ ]`                                                | `[ { threshold = 80; olderThan = "20d"; } ]`; see [Garbage collection](#garbage-collection)                                   |
| `gc.interval`      | `"5m"`                                               | how often the thresholds are checked                                                                                          |

`store` is the one mode that needs no trusted user: it derives the database,
store directory, import URI and gcroot directory from the prefix, runs
`nix-store --init` on first start, and imports into a store nobody else shares.
It cannot be combined with `db`/`storeDir`/`gcrootDir` (it derives all three) or
with `gc.rules` (`nix-collect-garbage` only ever collects `/nix/store`). Both
restrictions are module assertions, so they fail evaluation rather than at
runtime.

#### Tailscale write capability

To allow a node to push, add this to its ACL grants:

```json
{
  "grants": [
    {
      "src": ["tag:builder"],
      "dst": ["tag:cache"],
      "app": {
        "kradalby.no/cap/tsnixcache": [{ "push": true }]
      }
    }
  ]
}
```

#### Compression, and running `serve` by hand

xz-compressed pushes are always decompressed by the `xz` binary, which must be in
the service's `PATH`; the pure-Go xz decoder cannot bound the dictionary a stream
declares. Because that is what `nix copy --to http://...` compresses with by
default, `serve` refuses to start when `xz` is not on its `PATH` rather than
letting every such push fail after the client has uploaded the whole NAR. Pass
`--allow-missing-codecs` to start a cache that only ever receives zstd (one fed
solely by `tsnixcache push`, say); it then logs a warning at startup, and each xz
push fails with a 500 once its NAR has been uploaded. The NixOS module already
puts `pkgs.xz` on the service `PATH`, so a module deployment never hits this.
Everything else is decompressed in-process. `tsnixcache serve
--external-compression` additionally spawns the `zstd` binary for decoding and
uses the in-process decoder when `zstd` is missing. This flag controls import
decoding. Serving always uses the in-process NAR writer and zstd encoder.

Imports use `nix-store --serve --write` and require serve protocol 2.5 or newer.
The legacy `--import` command buffers the decoded NAR in the Nix subprocess;
the streaming command avoids that allocation. Use matching recent Nix client
and daemon versions: old daemon implementations can still buffer forwarded
imports. The pinned Nix client, daemon and chroot store are verified together.

Decoder memory is bounded from the pushed stream's own header, so a zstd frame
declaring a window over 8 MiB is refused whichever decoder runs. nix's default
lands on 2 MiB and `compression-level=19` on exactly 8 MiB, but nix passes
`compression-level` to libzstd unfiltered — there is no `--ultra` gate as in
`zstd(1)` — so `nix copy --to "http://...?compression=zstd&compression-level=20"`
and above declare 32 MiB or more and are rejected, after the NAR has been
uploaded: the server logs `window size exceeded` and the client sees `import
failed`. Keep `compression-level` at 19 or below.

Four `serve` flags have no module option and so apply only to a hand-written
command line: `--external-compression`, `--allow-missing-codecs`,
`--import-concurrency` (default: one per CPU; `0` = unlimited) and
`--nix-store-uri` (what `nix`'s own `--store` is given; `auto` = the system
daemon).

Running `tsnixcache serve` by hand: a `--listen` address serves reads only, so
pass `--local-write` if you mean to push to it — otherwise every write comes back
`403` naming the flag. `--spool-dir` defaults to
`/var/cache/tsnixcache/spool` (what the module uses), which an unprivileged
operator cannot create — pass `--spool-dir` at a directory you own. The spool is
refused if it is a symlink, is group- or world-writable, or belongs to another
uid: a pushed NAR is written there before it is verified, so anything that can
edit it can choose what gets imported. `--serve-compression` accepts only `none`
or `zstd`, and is rejected at startup rather than at the first request.

### 3. Client (NixOS)

```nix
{
  imports = [ inputs.tsnixcache.nixosModules.tsnixcache-client ];

  services.tsnixcache-client = {
    enable = true;
    package = inputs.tsnixcache.packages.${pkgs.system}.default;

    # Defaults to ["http://tsnixcache"]; add more entries for multiple servers.
    # substituters = [ "http://tsnixcache" ];
    publicKey = "cache.example.com:base64-public";

    # Auto-push. The post-build-hook is the exact mechanism — nix hands it the
    # paths it just built — and watch is the fallback that catches everything
    # else (substituted paths, nix-store --add, imports) and retries what the
    # hook could not deliver. Enable both; see "Push resilience" below.
    postBuildHook.enable = true;
    watch.enable = true;
    # postBuildHook.to and watch.to both default to "http://tsnixcache"
  };
}
```

`publicKey` is a single string. A second cache with a _different_ key is
expressed by adding it to Nix's own option, which the module appends to rather
than owns:

```nix
nix.settings.trusted-public-keys = [ "other-cache.example.com:base64-public" ];
```

#### Options

`enable`, `package`, `substituters` (default `[ "http://tsnixcache" ]`),
`publicKey`, `postBuildHook.{enable,to,timeout}` (timeout default `"60s"`) and
`watch.{enable,to,db,storeDir,stateDir,pollInterval}` are available in both client
modules. Polling defaults to `"30s"` on Linux and `"5m"` on Darwin. Notifications
watch the Nix database directory, including WAL changes; periodic database
polling remains authoritative. Darwin uses kqueue. Watching the small database
directory keeps descriptor use independent of the number of store paths.
Notification failure falls back to polling and bounded reattachment retries.

`watch.stateDir` defaults to `/var/lib/tsnixcache-watch`. Linux creates private
persistent state with `StateDirectory`; a custom value must be that directory
or a normalised child path. Darwin creates a persistent directory for launchd.
Manual invocation uses `$XDG_STATE_HOME/tsnixcache` or
`~/.local/state/tsnixcache` on Linux, and the user's Application Support
directory on Darwin. Keep retry state separate from ephemeral session files.

Module duration strings accept positive whole-number compounds through hours
(`"1h30m"`, `"250ms"`) or a whole-day count (`"2d"`), with overflow rejected.
Fractions, signs and mixed day compounds are outside this Nix subset.
`postBuildHook.timeout = "0"` disables the overall push deadline; other module
durations must be positive. The CLI also accepts fractional Go durations.

Both modules put `package` on `environment.systemPackages` whenever they are
enabled, and both add `nix-command` to `nix.settings.extra-experimental-features`
**only** when `postBuildHook.enable` or `watch.enable` is set — `push` resolves
closures with `nix path-info`, which lives behind that feature.

Neither module exposes `--verbose`, `--stall-timeout`, `--attempts`,
`--idle-exit` or any of the four `--retry-*` flags. Their defaults are the ones described under
[Push resilience](#push-resilience), and changing them means overriding the
unit's `ExecStart` (`systemd.services.tsnixcache-watch` /
`launchd.daemons.tsnixcache-watch`) by hand.

On NixOS the watcher logs to the journal (`journalctl -u tsnixcache-watch`). The
darwin launchd job writes both streams to `/var/log/tsnixcache-watch.log`
instead, which nothing rotates.

### 4. Manual push

Everything in this section needs the `nix-command` experimental feature, and
`push` needs `nix` itself on `PATH`: it resolves each closure with
`nix path-info -r --json`. The client modules enable `nix-command` only when the
hook or the watcher is on, and the server module never does, so on a host with
neither — including the cache server itself — pass it explicitly (or set
`NIX_CONFIG='experimental-features = nix-command'` for the session):

```
nix --extra-experimental-features nix-command copy --to http://tsnixcache /run/current-system
```

On nix-darwin `/run/current-system` is the active generation too, so the same
command pushes a Mac's whole system closure.

Or push specific paths:

```
tsnixcache push --to http://tsnixcache /nix/store/…
```

A single `-` argument reads store paths from stdin, one per line, so a query can
be piped straight in:

```
nix path-info --recursive /run/current-system | tsnixcache push --to http://tsnixcache -
```

Every example here pushes to `http://tsnixcache`, the tsnet listener, where the
push grant decides. Pushing to a `listen` address instead — `http://127.0.0.1:5000`
— is refused with `403` unless that server was started with
`services.tsnixcache.localWrite` (or `--local-write`); reads from it need
nothing.

Every duration flag on every subcommand (`--timeout`, `--stall-timeout`,
`--poll-interval`, `--retry-max-age`, `--gc-interval`, …) takes Go duration
syntax plus a whole-day `d` suffix, so `--retry-max-age 2d` works as well as
`48h`.

`tsnixcache push` uploads each path's closure itself (no `nix copy`), streaming
and compressing NARs on the fly, skipping paths already present, and running
`--jobs` uploads in parallel. It logs per-path stats — full path, size, time,
average and peak throughput — and a batch summary:

```
2026/08/11 19:58:09 INFO push: uploaded path=/nix/store/…-glibc-2.42-67 size="33.5 MiB" dur=410ms avg_upload="205.8 Mbit/s" peak_upload="334.3 Mbit/s"
2026/08/11 19:58:09 INFO push: present path=/nix/store/…-hello-2.12.3 size="273.1 KiB"
2026/08/11 19:58:09 INFO push: done paths=5 uploaded=5 skipped=0 bytes="36.2 MiB" dur=544ms avg_upload="172.5 Mbit/s" peak_upload="334.3 Mbit/s"
```

Throughput is measured on the compressed bytes actually sent (what the link
sees); size is the uncompressed NAR size.

On an interactive terminal push draws a live progress bar per in-flight upload
(name, percentage, size, current rate, elapsed) instead of the per-path log
lines; the batch summary is still logged when it finishes. Off a terminal (CI,
redirected output) it falls back to the structured logs above. `--no-progress`
forces the plain logs even on a terminal.

### 5. Prove it works

Signing and trust are only really wired up if a client can verify what the
server serves, and the one artefact that shows it is a `Sig:` line in the
served narinfo naming your key. Push a path and look:

```
p=$(nix path-info nixpkgs#hello)   # any store path will do
tsnixcache push --to http://tsnixcache "$p"
curl -s "http://tsnixcache/$(basename $p | cut -c1-32).narinfo"
```

```
StorePath: /nix/store/6i6xl6bmcpxqd51m8nlva40d5c1bhndx-hello-2.12.3
…
Sig: cache.example.com:cXDAk/lqU7r5fXCAQG88BeylzZXqFgVh3Xj/VLXh0y7XwCUITTD2WdZcv1jk1bkb9rhmqjI3EwolCCmJq1vTBg==
```

A path already in the server's own store is served with the signatures its Nix
database recorded _plus_ the cache's own, so one substituted from
`cache.nixos.org` and re-served here carries both. A _pushed_ path is served
with the cache's signature alone: the streaming importer retains the path,
references, deriver, NAR hash and size while discarding uploader signatures
and content-address trust metadata. Either way, no `Sig:` line at all means
the server has no `signKeyFile`, and every client with `require-sigs` on (the
default) will refuse the path.

`nix/tests/sigs.nix` is this check as a VM test: it turns `require-sigs` on,
trusts _only_ the tsnixcache key, and substitutes both a pushed path and one
served straight from the server's store.

### Push resilience

The post-build hook is best-effort and always exits successfully. Explicit
`push` and `wait-for` commands return failure when delivery is incomplete:

- `tsnixcache push` retries each path with exponential backoff under an overall
  deadline (`--attempts`, `--timeout`), so a brief outage (restart, deploy, load
  spike) self-heals. Retries are per path, not per byte: a path that already
  landed is skipped after a cheap narinfo `HEAD`, but a path that failed
  part-way is re-PUT in full — nothing resumes mid-NAR. One path failing doesn't
  abort the others, and a path the store garbage-collected between resolving the
  closure and uploading it is reported as a single failed path rather than
  failing the whole batch. A `4xx` is not retried — a revoked push grant or an
  over-size NAR is not going to succeed on the fifth attempt — so only `408`,
  `425`, `429` and server errors come back.
- **Stall detection:** a NAR transfer that makes no progress for `--stall-timeout`
  (default 60s) is cancelled and retried, so a laptop dropping offline mid-upload
  fails fast instead of hanging. The final `.narinfo` PUT is exempt — the server
  runs the whole verify + import inside it and sends nothing meanwhile, so a
  stall watchdog would kill every healthy import of a large path. It is bounded
  by a fixed 35-minute whole-request deadline, just above the server's own
  30-minute import deadline. Response bodies use the stall deadline (60s by default) and bounded
  diagnostics (4 KiB plus one byte to detect truncation). Expiring the 35-minute
  deadline ends that push without another attempt; the watcher can retry it
  later. Body-read failures remain subject to the configured upload retries.
- The `post-build-hook` runs push best-effort — on a persistent failure it logs
  a warning and exits 0, so the build still succeeds.
- **Convergence:** `watch` transactionally records discoveries, the poll cursor,
  retries, first-failure times and acknowledgements in private SQLite state.
  An outage or process crash retains pending work. Per-path backoff runs from
  `--retry-backoff-base` (30s) to `--retry-backoff-max` (10m), with 25%
  jitter around each interval.
  `--retry-queue-size` (512) limits discovery and selection batches; overflow
  stays on disk. It is no longer a lossy queue capacity. Zero selects bounded
  defaults. A destination and source database have their own state identity;
  competing owners, corrupt state and unknown schemas fail instead of resetting
  the queue. Keep the state directory across restarts.
- **Expiry:** `--retry-max-age` defaults to 2h; zero disables expiry. Expired work
  is logged and retained as a tombstone until it can be retired. New expiry makes that drain unsuccessful. Later sessions can succeed while
  old expired tombstones remain; disabling expiry does not revive them.
  Tombstones retire when the source path disappears, and a new registration
  of that path can reset expiry. Source replacement is
  reconciled conservatively; unrelated database files never inherit a cursor.
- **Shutdown:** capture a source boundary and drain every discovery page through
  it within 30s. Each pending path gets at most one recorded final upload invocation for
  that drain generation, even if its backoff lies ahead. The configured
  per-upload retries (`--attempts`, default 2) still apply. Failure retains the
  boundary, work and consumed allowances. Restart resumes ordinary retries with
  their saved ages/backoff; repeatedly stopping immediately does not grant extra
  attempts. Incomplete drain exits non-zero and remains visible to a late
  `wait-for`. The service managers allow 90s for shutdown.
- **Lifecycle:** `--pid-file` also maintains an atomic status record and private
  authenticated control socket. `wait-for --ready` prints the session token;
  `wait-for --stop --session TOKEN` requests that session's final drain. A late
  waiter reads the recorded terminal result. Missing status is a bounded error.
  Legacy PID-only files are rejected after the startup timeout; restart the
  watcher with the matching CLI. `--idle-exit` defaults to zero; explicit
  stop is the reliable end of a build session.

Client-side push metrics are intentionally not implemented: `tsnixcache push`
is a short-lived process with nothing to scrape. Persistent failures surface in
the hook's log (and, for the daemon, in `tsnixcache watch`'s logs).

## Security and trust model

**Reads are entirely unauthenticated.** On the tsnet listener the capability
grant gates writes only; safe methods are exempt, so _any_ node on the tailnet
can enumerate hash parts, fetch every narinfo and download every NAR the
server's `/nix/store` holds — which on a build host is every private artefact
ever built there, secrets baked into a closure included. It can also scrape
`/metrics` and read `/health`. If the store holds anything you would not hand to
every tailnet peer, the ACL that decides who can reach the cache node at all is
the control you have; there is no per-path authorisation. (`/debug/` is the one
exception on tsnet: it needs the push grant, because pprof can dump the
process's memory.) This has not changed and is deliberate — a substituter that
demanded credentials would be no use to `nix`, which is given none.

A `listen` address is the same bargain, and being read-only does not soften it:
what that closes is writes, not reads. Anything that can open the socket can
still enumerate and download every path the cache serves, which is why the module
warns about a non-loopback entry.

**Writes are gated by the push grant, and a pusher is fully trusted.** A node
that can push writes arbitrary paths into the server's `/nix/store`. The server
verifies that uploaded content matches its NAR hash, but not that the claimed
store path was legitimately built, and it re-signs everything it serves with its
own key. Worse, the default `priority` of 30 beats `cache.nixos.org`'s 40, so a
poisoned path does not merely coexist with the upstream one — it _shadows_ it
for every client that trusts this cache. Grant push only to machines you trust
to build — the same trust you'd give a Nix remote builder or trusted-user.

**That grant is the whole write boundary, because a `listen` address cannot be
written to at all.** There is nothing to authenticate a request arriving on a plain
socket against — no node identity, no uid, nothing — so rather than pretend
otherwise, those listeners serve reads and refuse every unsafe method with `403`.
That keeps the substituter use case, which is what loopback is for, and takes
away the local-uid path into the store: without it, any account able to open the
socket could have a NAR of its choosing imported and re-signed with the cache's
key, at a priority that beats the upstream cache.

`services.tsnixcache.localWrite` (CLI: `--local-write`) turns writes back on for
those listeners, and gives that boundary up: pushes are then accepted from every
local uid, and from the whole network if the address is not loopback. Set it only
where the socket is genuinely reachable only by things you would trust to build —
a single-user builder, a firewalled host — and understand it as granting them the
cache's signing key by proxy. The server logs a warning at startup naming each
such listener, and the module warns at evaluation time.

Each refusal is counted as `tsnixcache_auth_rejects_total{reason="local_write"}`
and logged with the remote address and method, so a pusher that stopped working
across an upgrade shows up as a number rather than a mystery.

`listen = [ ]` in the module leaves no local listener at all: the module maps
over the list, so an empty one emits no `--listen` flag, and the CLI's
`127.0.0.1:5000` fallback applies only when no `--listen` was passed _and_ no
`--tsnet` was configured — so a server with neither still has something to serve
on. Passing `--listen ""` on the command line is not the same thing: it is a
listen address that resolves to nothing, so with no `--tsnet` the server exits
with `no listeners configured` rather than falling back.

## Failure and recovery

- **Compressed NAR requests are capped at 16 GiB** (and narinfo at 1 MiB),
  enforced on the request body: past that the server replies `413` and logs the limit. `413` is a
  `4xx`, so the client does not retry it — the path simply never lands. Nothing
  in Nix stops a closure member from being that large; it just means this cache
  cannot carry it. Decoded NARs have an independent 16 GiB default limit
  (`--max-nar-size` / `maxNarSize`), checked against actual decoded bytes as well
  as metadata. Verification and import decode the same open spool descriptor in
  two passes; unverified data is never imported. Cancellation interrupts stream
  work and codec subprocesses, though a blocked filesystem syscall must return.
  Narinfo parsing accepts at most 1,024 signature lines.
- **Import runs inside the narinfo PUT, under a 30-minute deadline.** The client
  allows 35, so a genuinely slow import completes and only a wedged `nix-store`
  is cut off. Imports are also bounded in number (`--import-concurrency`); a push
  that waits more than 30s for a slot gets `503` with `Retry-After` and backs
  off, rather than holding a connection open.
- **A restart can truncate an in-flight import.** Shutdown waits 60s for
  in-flight requests; an import still running then is killed. The transfer is
  wasted, not corrupted — Nix store import is atomic and the path is either
  fully there or not — and the client's `watch` queue re-pushes it. Deploying the
  cache during a large push costs bandwidth, nothing else.
- **The spool is swept every 10 minutes.** Unpaired NARs expire after a day;
  completed compressed-cache entries expire after an hour. Startup removes
  abandoned spool data under exclusive ownership. Two servers cannot share the
  same compressed-cache directory.
- **The compressed cache charges file data against a 4 GiB budget.** Charges
  round up to 64 KiB and include in-progress files, pinned readers and files
  whose removal failed. Eviction uses an LRU list and cannot reclaim a file
  still being read. Filesystem metadata and allocation overhead are additional
  disk use. A cold zstd narinfo GET materialises the compressed NAR to report
  accurate hash and size; HEAD does not compress. Use uncompressed serving when
  first-read latency matters. If an object cannot be admitted, narinfo advertises
  an uncompressed URL; an explicit `.nar.zstd` request gets retryable `503`,
  never an uncompressed body under compressed metadata.
- **Degraded start is normal, not a fault.** Nix keeps its DB in WAL mode, which
  cannot be read without writing a wal-index, so before `nix-daemon` has opened
  the database the server cannot either. It binds anyway, `/health` reports
  `degraded` with `store_reachable: false`, and it retries until the open
  succeeds (logging `nix store database opened, no longer degraded`).

When imports fail repeatedly, in order:

1. `journalctl -u tsnixcache | grep import` — the server logs `ERROR import
path=… err=…` with `nix-store`'s own stderr attached, which usually names the
   cause outright.
2. `curl -s http://…/health` — a full store or spool filesystem is the common
   one, and the spool is often a separate filesystem that fills first.
3. `tsnixcache_push_errors_total` by `reason` (`nar_body`, `narinfo_body`,
   `narinfo_parse`, `busy`) separates "the client sent something we refused"
   from "we ran out of import slots"; `busy` means raise
   `--import-concurrency` or push less concurrently.
4. Check the service is still a trusted user (`nix.settings.trusted-users`) and
   that `nix-daemon` is running — every import goes through it. A chroot store
   (`services.tsnixcache.store`) needs neither.

## Monitoring

- `GET /health` — JSON: store reachable, uptime, store and spool free space,
  path count. `503` when the store is unreadable.
- `GET /version` — the build's version string as plain text; `dev` unless the
  linker set it.
- `GET /nix-cache-info` — `StoreDir`, `WantMassQuery` and `Priority`, i.e. what
  a Nix client reads before deciding to use this cache at all.
- `GET /metrics` — Prometheus (cache hits/misses by request method, pushes,
  push errors by reason, imports and their duration, GC, store and spool disk),
  plus the standard Go and process collectors.
  `tsnixcache_nar_bytes_served_total` and `tsnixcache_nar_bytes_received_total`
  are byte counters; `rate(...[$__rate_interval])` gives read/write throughput.
  Prefer `$__rate_interval` over a fixed `[5m]` in Grafana: once the zoom takes
  the step past the window, a fixed window samples gaps and under-reports —
  which is exactly when you are looking. Served bytes are wire bytes, so under
  `serveCompression = "zstd"` they count the compressed body.
- A Grafana dashboard ships with the flake: `nix build .#grafanaDashboards`
  writes `tsnixcache.json` for file-based provisioning, and `nix run .#dashboard`
  prints the same JSON. A test greps every metric name out of the server sources
  and fails the build if a panel queries one that no longer exists, so the
  dashboard stays checked against the exporter. Operational Store paths and
  Store disk used tiles require a successful matching scrape and gauge/up samples
  younger than 90 seconds. Scrape at least every 30 seconds. Missing, failed or
  stale series render neutral **Unavailable**; the default dashboard refresh adds
  up to 30 seconds of display delay. Historical/rate panels retain earlier data.
  The VM suite tests actual Prometheus queries, Grafana responses and browser
  rendering through scrape failure, target removal and recovery.
- Serving `zstd` keeps compressed NARs in a spool cache that is bounded by a
  total byte budget with LRU eviction, and the number of concurrent
  compressions is capped: NAR reads need no push grant, so neither disk nor
  memory may grow with the number of readers.
- `tsnixcache watch` logs a per-batch summary at info level — `watch: uploaded
batch` with counts and humanised `bytes`, `avg_upload` and `peak_upload`
  (bit/s) — with or without `--verbose`. What `--verbose` adds is a debug line
  per _discovered_ path (`watch: path`), logged when the poll finds it and
  before anything is uploaded.
- `GET /debug/` — tsweb debug index (pprof, expvar, `/debug/varz`, force-GC).
  Over the tsnet listener it requires the **push grant**, whatever the method:
  pprof can dump the process's memory, so it is not something every tailnet peer
  should be able to read. On a `listen` address the introspection endpoints are
  ungated, so treat them as available to anything that can open the socket —
  unbounded CPU profiles included — and keep the address on loopback.
  `/debug/gc` is the exception: it collects a stop-the-world GC, and tsweb's
  index links it with a plain `<a href>`, so the button is a `GET`. It is
  refused by path rather than by method, and `--local-write` turns it back on
  along with everything else.
- `journalctl -u tsnixcache` for server logs, `-u tsnixcache-watch` for the client watcher.

## Garbage collection

The server can run GC itself on a disk-usage threshold via repeatable `gc.rules`
(module) / `--gc-rule` (CLI), each `threshold:age`. `80:20d` means "once the store
filesystem is ≥ 80% full, prune tsnixcache gcroots older than 20 days, then run
`nix-collect-garbage`". Imported paths are held by a gcroot until they age out, so
active imports retain their roots, and completed imports refresh their age.

The same rules can be applied once, out of band, by the standalone subcommand —
useful from a timer on a host that does not run `serve`, or to try a rule set
before wiring it into the module:

```
tsnixcache gc --gc-rule 80:20d --dry-run
```

It refuses to run against a `--gcroot-dir` that does not exist, since a typo
there would prune nothing and still collect the real store.

`nix-collect-garbage` takes no store argument and always collects `/nix/store`, so
GC is refused for any other store: neither `--store` nor a `--store-dir` other than
`/nix/store` can be combined with `--gc-rule`. Otherwise the threshold would be
measured, and the gcroots pruned, on one store while another one is collected.

Things worth knowing before pointing this at a full disk:

- **Threshold usage is `(Blocks-Bfree)/Blocks`.** Root-reserved free blocks
  remain free in this calculation. `df` instead reports
  `used/(used+Bavail)`, where `Bavail` excludes the reserve. The store disk gauge
  and operational dashboard use the GC formula.
- **Imports and pruning share persistent ownership locks.** A stale candidate
  is rechecked under its stripe before removal. A failed re-push retains an
  existing root; rollback removes only a root that import created. Root-run GC
  and service imports share the configured root directory's ownership. Keep
  `.locks` and its inodes intact across upgrades.
- **`tsnixcache gc --dry-run`** applies the rules and logs what it _would_ prune
  without unlinking anything, and passes `--dry-run` through to
  `nix-collect-garbage`. Run it once before trusting a new rule set.
- **Repeat collections back off.** A run that prunes no gcroot and frees no
  space does not start `nix-collect-garbage` again until `max(--gc-interval, 1h)`
  has passed, so a store that is simply full of live paths is not re-walked every
  interval forever.
- **`tsnixcache_gc_freed_bytes_total` is a lower bound.** It is a whole-filesystem
  `statfs` delta measured around each collect, so concurrent writes and
  copy-on-write snapshots (btrfs, ZFS) hide reclaimed space; a zero does not mean
  nothing was collected. On such filesystems the backoff above also means
  collections settle to one per cooldown rather than one per interval.
- Only tsnixcache's own gcroots are pruned — entries named after a store path's
  hash part, which is what the importer writes. `system-*-link`, `booted-system`
  and anything else `nix` owns in the gcroot directory is left alone, whatever
  its age.
- A `--gc-rule` set whose ages do not tighten as thresholds rise (say
  `80:5d 90:20d`, which _relaxes_ retention as the disk fills) is accepted but
  warned about at startup, as is an age under a minute — a gcroot exists to
  provide useful retention after the import completes.

## Flake input

The flake builds for `x86_64-linux`, `aarch64-linux`, and `aarch64-darwin`.
Current nixpkgs no longer supports `x86_64-darwin`, so Intel macOS packages
are not exposed by this flake.

```nix
inputs.tsnixcache = {
  url = "github:kradalby/tsnixcache";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

## Using as a GitHub Actions cache

Join the runner to the tailnet before installing Nix if the cache will also be
its substituter. Give its tag the `kradalby.no/cap/tsnixcache` push grant, then
configure `extra-substituters` and `extra-trusted-public-keys`. Pin the Tailscale
and Nix installer actions to reviewed commits and supply credentials through
GitHub secrets. This repository does not provision those credentials.

Resolve the binary once, create a private session directory, wait for readiness,
and explicitly drain after the build. This shell sequence can be one Actions
step after tailnet and Nix setup:

```bash
set -euo pipefail
session=$(mktemp -d "${RUNNER_TEMP:-/tmp}/tsnixcache.XXXXXX")
chmod 700 "$session"
package=$(nix build --no-link --print-out-paths github:kradalby/tsnixcache#default)
cache="$package/bin/tsnixcache"
state="${TSNIXCACHE_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/tsnixcache-watch}"
token=""

finish() {
  build_status=$?
  trap - EXIT INT TERM
  set +e
  args=(--stop --pid-file "$session/watch.pid" --pid-file-timeout 10s --timeout 60s)
  if [ -n "$token" ]; then args+=(--session "$token"); fi
  "$cache" wait-for "${args[@]}"
  upload_status=$?
  if [ "$upload_status" -eq 0 ]; then
    wait "$watcher"
    upload_status=$?
  fi
  cat "$session/watch.log"
  printf 'Session logs and terminal status: %s\n' "$session"
  if [ "$build_status" -ne 0 ]; then exit "$build_status"; fi
  exit "$upload_status"
}

"$cache" watch --to "${TSNIXCACHE_URL:-http://tsnixcache}" \
  --state-dir "$state" --pid-file "$session/watch.pid" >"$session/watch.log" 2>&1 &
watcher=$!
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
token=$("$cache" wait-for --ready --timeout 60s --pid-file "$session/watch.pid")
nix build .
```

A long build with no registered outputs keeps its watcher alive. The exit trap
runs the authenticated stop even after build failure and preserves the build or
upload failure status. Keep the session files when diagnosing a failed drain;
keep the state directory to resume retries. Hosted runners need explicit preservation of retry state between jobs.
`RUNNER_TEMP` holds only session files and is cleared at job boundaries. A
control timeout can leave the watcher running; retain the session and retry
its authenticated stop before reusing that state directory. `TSNIXCACHE_URL` is
also the default URL for manual `push` and `watch` commands.

## Upgrading and rollback

Stop old watchers, servers and standalone GC jobs before installing matching
CLI and module versions. Old binaries do not honour the new root locks. Keep
GC roots, `.locks` inodes and persistent retry state; then restart the services.
The queue schema is versioned and unknown newer versions are rejected without
resetting state. Old in-memory retries cannot be reconstructed perfectly: run
an explicit closure push for known affected builds after upgrading.

Before rollback, stop the new processes and preserve state and roots. An old
watcher cannot resume the new durable queue. Do not run old and new root writers
against the same directory concurrently.

## Contributing

```sh
nix develop
prek install
nix run .#test-race
nix run .#lint
nix fmt
prek run --all-files
```

Packages live at the repository root by function; command mains are thin.
There are no project-owned `internal/`, `pkg/` or `pkgs/` directories.
Generated SQL lives under `gen/`. Run `go generate ./db` after changing SQL, then `nix run .#generate`
to check for drift. Tests sit beside their source and
benchmarks in `bench_test.go`.

- Treefmt runs gofumpt, goimports, nixfmt, prettier and shfmt. Hooks use
  `treefmt --fail-on-change`.
- Run `go run ./cmd/vendorhash update` when the vendor tree changes, including
  changes in imported test packages. `flakehashes.json` is verified by hooks.
- `nix build .#nativeChecks` selects the host platform and runs build, race
  tests, lint, formatting, generation, duration and hook checks without VMs.
  `flake.lock` changes trigger it too.
- Garnix runs x86 Linux packages and NixOS VMs. GitHub Actions runs native ARM
  Linux and ARM Darwin checks; Darwin also builds the example module and runs
  the real hook/watcher lifecycle test.
- `nix flake check` includes NixOS VMs on Linux and requires KVM. Use
  `nix build .#nativeChecks` for a host-only contributor check. Use
  `nix flake check --all-systems --no-build` for supported-platform evaluation.
- `nix run .#coverage` records statement coverage. Fuzz targets and benchmarks
  supplement deterministic ownership, cancellation and lifecycle regressions.

`golangci-lint` enables all linters with explicit exceptions. Comments explain
why; prose and user-facing strings use British spelling.

## Licence

BSD 3-Clause. See [`LICENSE`](LICENSE). The NAR reader and writer in `nar/`
derive from Tailscale's, which carry their own BSD 3-Clause headers.
