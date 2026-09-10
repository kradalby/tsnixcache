# Configuration

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

## Generate a signing key

### Create the key

Run the binary directly from the flake on a fresh cache host:

```
sudo mkdir -p /etc/tsnixcache
sudo sh -c 'umask 077; nix run --extra-experimental-features "nix-command flakes" \
  github:kradalby/tsnixcache -- key generate --name cache.example.com > /etc/tsnixcache/key'
```

The directory must exist before the shell opens the redirect. `umask 077` keeps
the secret private from the moment the file is created.

The command writes only `name:base64-secret` to the file and prints the public
key on stderr.

### Recover the public key

Once the server module is deployed, `tsnixcache` is available on `PATH`:

```
sudo tsnixcache key public /etc/tsnixcache/key
# cache.example.com:base64-public
```

> Back up the secret key. If it is lost, existing paths remain in the store but
> clients can no longer verify them. Generate a new key and trust it on every
> client.

## Server (NixOS)

```nix
{
  imports = [ inputs.tsnixcache.nixosModules.tsnixcache ];

  services.tsnixcache = {
    enable = true;
    package = inputs.tsnixcache.packages.${pkgs.system}.default;
    signKeyFile = "/etc/tsnixcache/key";

    listen = [ "127.0.0.1:5000" ];

    tsnet = [
      {
        authKeyFile = "/run/secrets/tailscale-authkey";
        dir = "/var/lib/tsnixcache/tsnet-cache";
      }
    ];
  };
}
```

The example exposes two listeners:

- `127.0.0.1:5000` serves unauthenticated reads and refuses writes.
- `http://tsnixcache` serves the tailnet; writes require the capability grant
  configured below.

The module also:

- installs `tsnixcache`, making commands such as `key` and `gc` available;
- adds its service user to `nix.settings.trusted-users` when using the system
  store;
- avoids trusted-user access when using a self-contained chroot store.

To omit the explicit `package`, add `inputs.tsnixcache.overlays.default` to
`nixpkgs.overlays`. All modules will then use `pkgs.tsnixcache` from the
consumer's nixpkgs.

### Server options

#### Core

| Option        | Default           | Purpose                                                                 |
| ------------- | ----------------- | ----------------------------------------------------------------------- |
| `enable`      | `false`           | Enable the service.                                                     |
| `package`     | `pkgs.tsnixcache` | Package to install and run. Requires the overlay unless set explicitly. |
| `signKeyFile` | `null`            | Secret signing key path. Use a string; Nix store paths are rejected.    |
| `priority`    | `30`              | Cache priority. Lower values win; `cache.nixos.org` uses 40.            |

#### Network

| Option       | Default                       | Purpose                                                                       |
| ------------ | ----------------------------- | ----------------------------------------------------------------------------- |
| `listen`     | Loopback, or empty with tsnet | Plain read-only listeners. Reads are unauthenticated.                         |
| `localWrite` | `false`                       | Allow writes on plain listeners. Review the [trust model](security.md) first. |
| `tsnet`      | `[ ]`                         | Tailnet listeners with grant-controlled writes.                               |

Each `tsnet` entry accepts `hostname`, `authKeyFile`, `dir`, `port`, `tls`, and
`controlUrl`. The hostname defaults to `tsnixcache`, the port to 80, and the
control URL to Tailscale's default. TLS is unsupported; setting `tls = true`
fails evaluation.

#### Storage and imports

| Option             | Default                           | Purpose                                               |
| ------------------ | --------------------------------- | ----------------------------------------------------- |
| `store`            | `""`                              | Root of a self-contained chroot store.                |
| `db`               | `/nix/var/nix/db/db.sqlite`       | Database opened read-only; incompatible with `store`. |
| `storeDir`         | `/nix/store`                      | Store directory; incompatible with `store`.           |
| `gcrootDir`        | `/nix/var/nix/gcroots/tsnixcache` | GC root directory; incompatible with `store`.         |
| `spoolDir`         | `/var/cache/tsnixcache/spool`     | Temporary location for NAR verification and import.   |
| `maxNarSize`       | 16 GiB (`17179869184`)            | Maximum decoded NAR size.                             |
| `serveCompression` | `"none"`                          | Served NAR compression: `"none"` or `"zstd"`.         |

When `store` is set, the module derives the database, store, import URI, and GC
root directory from that prefix. It runs `nix-store --init` on first start and
does not require a trusted service user.

`store` cannot be combined with `db`, `storeDir`, `gcrootDir`, or `gc.rules`.
These combinations fail during module evaluation.

#### Garbage collection

| Option        | Default | Purpose                                                                   |
| ------------- | ------- | ------------------------------------------------------------------------- |
| `gc.rules`    | `[ ]`   | Threshold and age rules such as `{ threshold = 80; olderThan = "20d"; }`. |
| `gc.interval` | `"5m"`  | How often to evaluate the rules.                                          |

See [Garbage collection](operations.md#garbage-collection) before enabling
rules.

### Tailscale write capability

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

### Compression

#### xz input

xz-compressed pushes use the external `xz` binary. The pure-Go decoder cannot
bound the dictionary declared by an xz stream.

Because `nix copy --to http://...` uses xz by default, `serve` refuses to start
when `xz` is missing. The NixOS module already puts `pkgs.xz` on the service
`PATH`.

For a cache that receives only zstd, pass `--allow-missing-codecs`. Startup then
logs a warning, and any later xz push fails with HTTP 500 after upload.

#### zstd input

zstd is decoded in-process by default. `--external-compression` tries the `zstd`
binary for import decoding and falls back to the in-process decoder when it is
missing.

Serving always uses the in-process NAR writer and zstd encoder.

Both decoders reject a zstd frame with a window larger than 8 MiB:

| Nix compression level | Declared window | Result   |
| --------------------: | --------------: | -------- |
|               Default |           2 MiB | Accepted |
|                    19 |           8 MiB | Accepted |
|          20 or higher |  32 MiB or more | Rejected |

Nix passes the compression level directly to libzstd without the `zstd(1)`
`--ultra` gate. Keep `compression-level` at 19 or lower. Rejected uploads log
`window size exceeded`; the client reports `import failed`.

#### Nix store protocol

Imports use `nix-store --serve --write` and require serve protocol 2.5 or newer.
This streams the decoded NAR; the legacy `--import` command buffers it in the Nix
subprocess.

Use matching recent Nix client and daemon versions. Older daemons may still
buffer forwarded imports.

The repository verifies the pinned client, daemon, and chroot store together.

### Running `serve` directly

The following flags are available only on the command line:

| Flag                     | Default or purpose                                      |
| ------------------------ | ------------------------------------------------------- |
| `--external-compression` | Use the external zstd decoder when available.           |
| `--allow-missing-codecs` | Start without every external decoder.                   |
| `--import-concurrency`   | One import per CPU; `0` removes the limit.              |
| `--nix-store-uri`        | Store URI passed to Nix; `auto` uses the system daemon. |

A plain `--listen` address serves reads only. Add `--local-write` to accept
pushes; otherwise write requests return `403`.

`--spool-dir` defaults to `/var/cache/tsnixcache/spool`, which an unprivileged
user may not be able to create. Choose a directory that user owns. The server
rejects a spool that:

- is a symlink;
- is group- or world-writable;
- belongs to another user.

These checks protect the unverified NAR before import. `--serve-compression`
accepts only `none` or `zstd` and is validated at startup.

## Client (NixOS)

```nix
{
  imports = [ inputs.tsnixcache.nixosModules.tsnixcache-client ];

  services.tsnixcache-client = {
    enable = true;
    package = inputs.tsnixcache.packages.${pkgs.system}.default;

    publicKey = "cache.example.com:base64-public";

    postBuildHook.enable = true;
    watch.enable = true;
  };
}
```

The module uses `http://tsnixcache` for substitution and uploads by default.
The post-build hook receives exact build outputs from Nix. The watcher catches
substituted, added, and imported paths and retries failed deliveries.

`publicKey` accepts one key. Add keys for other caches through Nix directly:

```nix
nix.settings.trusted-public-keys = [ "other-cache.example.com:base64-public" ];
```

### Client options

These options exist in both the NixOS and nix-darwin client modules.

#### Core

| Option         | Default                   | Purpose                             |
| -------------- | ------------------------- | ----------------------------------- |
| `enable`       | `false`                   | Enable client integration.          |
| `package`      | `pkgs.tsnixcache`         | Package to install and run.         |
| `substituters` | `[ "http://tsnixcache" ]` | Cache URLs added to Nix.            |
| `publicKey`    | Required                  | Signing key trusted for this cache. |

#### Post-build hook

| Option                  | Default               | Purpose                                       |
| ----------------------- | --------------------- | --------------------------------------------- |
| `postBuildHook.enable`  | `false`               | Push outputs handed to the hook by Nix.       |
| `postBuildHook.to`      | `"http://tsnixcache"` | Upload destination.                           |
| `postBuildHook.timeout` | `"60s"`               | Best-effort push deadline; `"0"` disables it. |

#### Watcher

| Option               | Default                        | Purpose                                               |
| -------------------- | ------------------------------ | ----------------------------------------------------- |
| `watch.enable`       | `false`                        | Discover and retry paths outside the post-build hook. |
| `watch.to`           | `"http://tsnixcache"`          | Upload destination.                                   |
| `watch.db`           | `/nix/var/nix/db/db.sqlite`    | Database to monitor.                                  |
| `watch.storeDir`     | `/nix/store`                   | Store containing discovered paths.                    |
| `watch.stateDir`     | `/var/lib/tsnixcache-watch`    | Persistent retry state.                               |
| `watch.pollInterval` | Linux: `"30s"`; Darwin: `"5m"` | Database polling fallback.                            |

### Watcher discovery and state

Notifications watch the Nix database directory, including WAL changes. Periodic
polling remains authoritative. Darwin uses kqueue. Notification failure falls
back to polling with bounded reattachment retries.

Watching the database directory keeps descriptor use independent of the number
of store paths.

On Linux, `StateDirectory` creates private persistent state. A custom
`watch.stateDir` must be `/var/lib/tsnixcache-watch` or a normalised child path.
Darwin creates persistent state for launchd.

Manual runs use:

| Platform | Default state directory                                     |
| -------- | ----------------------------------------------------------- |
| Linux    | `$XDG_STATE_HOME/tsnixcache` or `~/.local/state/tsnixcache` |
| Darwin   | The user's Application Support directory                    |

Keep durable retry state separate from ephemeral session files.

### Duration syntax

Module duration values accept:

- positive whole-number compounds through hours, such as `"1h30m"` or
  `"250ms"`;
- a whole-day value such as `"2d"`.

Fractions, signs, and mixed day compounds are rejected. Only
`postBuildHook.timeout` accepts `"0"`; it disables the overall deadline. The CLI
also accepts fractional Go durations.

### Module integration

When enabled, both client modules install `package`. They add `nix-command` to
`nix.settings.extra-experimental-features` only when the hook or watcher is
enabled, because closure discovery uses `nix path-info`.

The modules do not expose `--verbose`, `--stall-timeout`, `--attempts`,
`--idle-exit`, or the `--retry-*` flags. To change those defaults, override the
service command:

- NixOS: `systemd.services.tsnixcache-watch`
- Darwin: `launchd.daemons.tsnixcache-watch`

See [Push resilience](push-resilience.md) for the default retry behaviour.

### Watcher logs

| Platform | Location                                                    |
| -------- | ----------------------------------------------------------- |
| NixOS    | `journalctl -u tsnixcache-watch`                            |
| Darwin   | `/var/log/tsnixcache-watch.log` (not rotated by the module) |

## Manual push

### Prerequisites

Everything in this section needs the `nix-command` experimental feature, and
`push` needs `nix` itself on `PATH`: it resolves each closure with
`nix path-info -r --json`.

The client modules enable `nix-command` when the hook or watcher is active. On
other hosts, including a cache server without either client mechanism, enable it
for the command or set `NIX_CONFIG='experimental-features = nix-command'` for
the session.

### Push a system closure

```
nix --extra-experimental-features nix-command copy --to http://tsnixcache /run/current-system
```

On nix-darwin `/run/current-system` is the active generation too, so the same
command pushes a Mac's whole system closure.

### Push specific paths

```
tsnixcache push --to http://tsnixcache /nix/store/…
```

A single `-` argument reads store paths from stdin, one per line, so a query can
be piped straight in:

```
nix path-info --recursive /run/current-system | tsnixcache push --to http://tsnixcache -
```

### Listener permissions

These examples use the tsnet listener at `http://tsnixcache`, where the push
grant controls writes.

A plain listener such as `http://127.0.0.1:5000` rejects writes with `403`
unless `services.tsnixcache.localWrite` or `--local-write` is enabled. Reads need
no grant.

### Duration flags

Every duration flag on every subcommand (`--timeout`, `--stall-timeout`,
`--poll-interval`, `--retry-max-age`, `--gc-interval`, …) takes Go duration
syntax plus a whole-day `d` suffix, so `--retry-max-age 2d` works as well as
`48h`.

### Upload behaviour

`tsnixcache push` uploads each path's closure itself (no `nix copy`), streaming
and compressing NARs on the fly, skipping paths already present, and running
`--jobs` uploads in parallel. It logs per-path stats — full path, size, time,
average and peak throughput — and a batch summary:

```
2026/08/11 19:58:09 INFO push: uploaded path=/nix/store/…-glibc-2.42-67 size="33.5 MiB" dur=410ms avg_upload="205.8 Mbit/s" peak_upload="334.3 Mbit/s"
2026/08/11 19:58:09 INFO push: present path=/nix/store/…-hello-2.12.3 size="273.1 KiB"
2026/08/11 19:58:09 INFO push: done paths=5 uploaded=5 skipped=0 bytes="36.2 MiB" dur=544ms avg_upload="172.5 Mbit/s" peak_upload="334.3 Mbit/s"
```

The reported throughput measures compressed bytes sent over the network. The
reported size is the uncompressed NAR size.

Interactive terminals show one live progress bar per active upload. CI and
redirected output use structured log lines. `--no-progress` forces structured
logs on a terminal. The batch summary is always logged.

## Prove it works

Push a path and inspect its narinfo. A `Sig:` line naming your key proves that
signing and client trust are wired correctly:

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

Signature behaviour depends on how the path reached the server:

- **Already in the server store:** keeps signatures recorded by Nix and adds
  the cache signature.
- **Pushed to the server:** keeps path metadata, references, deriver, NAR hash,
  and size; discards uploader signatures and content-address trust metadata;
  adds the cache signature.

If no `Sig:` line appears, the server has no `signKeyFile`. Clients using the
default `require-sigs` setting will reject the path.

The `sigs` VM test verifies both cases with `require-sigs` enabled and only the
tsnixcache key trusted.
