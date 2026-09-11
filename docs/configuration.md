# Configuration

## Add the flake

```nix
inputs.tsnixcache = {
  url = "github:kradalby/tsnixcache";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

Add the overlay to each system configuration as shown below. It provides
`pkgs.tsnixcache`, which is the default package used by all modules.

## Generate a signing key

Run this on the cache host:

```sh
sudo mkdir -p /etc/tsnixcache
sudo sh -c 'umask 077; nix run --extra-experimental-features "nix-command flakes" \
  github:kradalby/tsnixcache -- key generate --name cache.example.com \
  > /etc/tsnixcache/key'
```

The command writes the secret key to the file and prints the public key. Back
up the secret key and keep it outside the Nix store.

Recover the public key later with:

```sh
sudo tsnixcache key public /etc/tsnixcache/key
```

## Configure the server

Import the NixOS module:

```nix
{ inputs, ... }:
{
  imports = [ inputs.tsnixcache.nixosModules.tsnixcache ];
  nixpkgs.overlays = [ inputs.tsnixcache.overlays.default ];

  services.tsnixcache = {
    enable = true;
    signKeyFile = "/etc/tsnixcache/key";

    listen = [ "127.0.0.1:5000" ];
    tsnet = [
      {
        hostname = "tsnixcache";
        authKeyFile = "/run/secrets/tailscale-authkey";
        dir = "/var/lib/tsnixcache/tsnet";
      }
    ];
  };
}
```

The loopback listener serves reads and refuses writes. The tsnet listener is
available at `http://tsnixcache`; writes require the Tailscale grant below.

Create the tsnet node with a tagged auth key that gives it `tag:cache`. Grant
push access only to trusted builders:

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

## Configure a NixOS client

Use the public key printed during server setup:

```nix
{ inputs, ... }:
{
  imports = [ inputs.tsnixcache.nixosModules.tsnixcache-client ];
  nixpkgs.overlays = [ inputs.tsnixcache.overlays.default ];

  services.tsnixcache-client = {
    enable = true;
    publicKey = "cache.example.com:base64-public";
    postBuildHook.enable = true;
    watch.enable = true;
  };
}
```

The post-build hook pushes outputs built by Nix. The watcher also catches paths
added through substitution, import, or other tools, and keeps failed work on
disk for later retries.

## Configure a nix-darwin client

Import the Darwin client module instead:

```nix
{ inputs, ... }:
{
  imports = [ inputs.tsnixcache.darwinModules.tsnixcache-client ];
  nixpkgs.overlays = [ inputs.tsnixcache.overlays.default ];

  services.tsnixcache-client = {
    enable = true;
    publicKey = "cache.example.com:base64-public";
    postBuildHook.enable = true;
  };
}
```

This example leaves the watcher off because the post-build hook has no idle
cost. If enabled, the polling fallback runs every five minutes and launchd
writes to `/var/log/tsnixcache-watch.log`. The module does not rotate that file.

## Prove it works

Build one path locally, push it, and inspect its narinfo:

```sh
p=$(nix build --no-link --print-out-paths nixpkgs#hello)
tsnixcache push --to http://tsnixcache "$p"
curl -fsS "http://tsnixcache/$(basename "$p" | cut -c1-32).narinfo"
```

Look for a `Sig:` line naming your key:

```text
StorePath: /nix/store/6i6xl6bmcpxqd51m8nlva40d5c1bhndx-hello-2.12.3
Sig: cache.example.com:base64-signature
```

If the signature is missing, check `services.tsnixcache.signKeyFile`. Clients
using Nix's default signature policy will reject an unsigned path.

## Push paths manually

Push a path and its closure with:

```sh
tsnixcache push --to http://tsnixcache /run/current-system
```

Read paths from standard input with `-`:

```sh
nix path-info --recursive /run/current-system \
  | tsnixcache push --to http://tsnixcache -
```

`push` skips paths already present and uploads independent paths in parallel.
If one path fails, anything in the closure that depends on it also fails. See
[Push resilience](push-resilience.md) for retries and timeouts.

These examples use tsnet. A plain listener returns `403` for writes unless you
enable `services.tsnixcache.localWrite` or pass `--local-write` to `serve`.

## Choose a server layout

### Listeners

With no tsnet entry, the module listens on `127.0.0.1:5000`. Adding tsnet removes
that default; set `listen` explicitly when you want both.

Plain listeners cannot identify callers. They serve unauthenticated reads and
refuse writes by default. Keep non-loopback listeners behind a firewall. Read
[Security](security.md) before enabling local writes.

### Chroot store

Set `services.tsnixcache.store` to use a self-contained store instead of the
system store. The module initialises it and does not need trusted-user access.

`store` cannot be combined with `db`, `storeDir`, `gcrootDir`, or `gc.rules`.

### Served compression

`serveCompression = "none"` avoids first-read compression latency.
`serveCompression = "zstd"` saves bandwidth and keeps generated compressed NARs
in a bounded on-disk cache. A cold narinfo `GET` must finish compression before
it can report the correct hash and size; `HEAD` does not compress.

## Watcher state

The watcher uses database-directory notifications and periodic polling. Keep
its state directory across restarts so failed uploads can resume.

| Platform   | Module state                | Poll fallback |
| ---------- | --------------------------- | ------------: |
| NixOS      | `/var/lib/tsnixcache-watch` |    30 seconds |
| nix-darwin | `/var/db/tsnixcache-watch`  |     5 minutes |

For a manual run, Linux uses `$XDG_STATE_HOME/tsnixcache` or
`~/.local/state/tsnixcache`. Darwin uses the user's Application Support
directory.

On NixOS, a custom `watch.stateDir` must be
`/var/lib/tsnixcache-watch` or a normalised child of it.

## Option reference

### Server

| Option             | Default                                 | Purpose                                                   |
| ------------------ | --------------------------------------- | --------------------------------------------------------- |
| `enable`           | `false`                                 | Enable the server.                                        |
| `package`          | `pkgs.tsnixcache`                       | Package to run. Add the overlay before using the default. |
| `signKeyFile`      | `null`                                  | Secret signing key path. Nix store paths are rejected.    |
| `priority`         | `30`                                    | Cache priority. Lower values win.                         |
| `listen`           | Loopback without tsnet; otherwise `[ ]` | Plain listeners.                                          |
| `localWrite`       | `false`                                 | Accept unauthenticated writes on plain listeners.         |
| `tsnet`            | `[ ]`                                   | Tailnet listeners with grant-controlled writes.           |
| `store`            | `""`                                    | Root of a self-contained store.                           |
| `db`               | `/nix/var/nix/db/db.sqlite`             | Nix database opened read-only.                            |
| `storeDir`         | `/nix/store`                            | Store directory.                                          |
| `gcrootDir`        | `/nix/var/nix/gcroots/tsnixcache`       | Roots for imported paths.                                 |
| `spoolDir`         | `/var/cache/tsnixcache/spool`           | Incoming NAR and compressed-cache storage.                |
| `maxNarSize`       | 16 GiB                                  | Maximum decoded NAR size.                                 |
| `serveCompression` | `"none"`                                | Serve NARs as `"none"` or `"zstd"`.                       |
| `gc.rules`         | `[ ]`                                   | Disk threshold and minimum root-age rules.                |
| `gc.interval`      | `"5m"`                                  | How often to check GC rules.                              |

Each `tsnet` entry accepts `hostname`, `authKeyFile`, `dir`, `port`, `tls`, and
`controlUrl`. The hostname defaults to `tsnixcache` and the port to 80. TLS is
unsupported; `tls = true` fails evaluation.

See [Garbage collection](operations.md#garbage-collection) before adding rules.

### Client

These options exist in both client modules.

| Option                  | Default                        | Purpose                                          |
| ----------------------- | ------------------------------ | ------------------------------------------------ |
| `enable`                | `false`                        | Enable cache substitution.                       |
| `package`               | `pkgs.tsnixcache`              | Package to install and run.                      |
| `substituters`          | `[ "http://tsnixcache" ]`      | Cache URLs added to Nix.                         |
| `publicKey`             | Required                       | Signing key trusted for this cache.              |
| `postBuildHook.enable`  | `false`                        | Push exact build outputs.                        |
| `postBuildHook.to`      | `"http://tsnixcache"`          | Hook destination.                                |
| `postBuildHook.timeout` | `"60s"`                        | Overall best-effort deadline; `"0"` disables it. |
| `watch.enable`          | `false`                        | Discover and retry other new paths.              |
| `watch.to`              | `"http://tsnixcache"`          | Watcher destination.                             |
| `watch.db`              | `/nix/var/nix/db/db.sqlite`    | Database to monitor.                             |
| `watch.storeDir`        | `/nix/store`                   | Store containing discovered paths.               |
| `watch.stateDir`        | Platform default above         | Persistent retry state.                          |
| `watch.pollInterval`    | Linux: `"30s"`; Darwin: `"5m"` | Polling fallback.                                |

Add additional trusted keys through Nix:

```nix
nix.settings.trusted-public-keys = [ "other-cache.example.com:base64-public" ];
```

Module durations accept positive whole-number Go duration components, such as
`"250ms"` and `"1h30m"`, or a whole-day value such as `"2d"`. Only
`postBuildHook.timeout` accepts `"0"`. CLI duration flags also accept fractional
Go durations.

The modules do not expose the watcher's retry, attempt, stall, idle-exit, or
verbose flags. Override the systemd or launchd command if you need them.

## Compression and protocol reference

Incoming xz NARs require the external `xz` binary. The NixOS module adds it to
the service `PATH`. A zstd-only server may pass `--allow-missing-codecs`; later
xz uploads then fail after transfer.

zstd imports use the in-process decoder by default. Both available decoders
reject frames with a window larger than 8 MiB. Keep Nix's zstd compression level
at 19 or lower.

Imports use `nix-store --serve --write` and require protocol 2.5 or newer. Use
matching recent Nix client and daemon versions.

When running `serve` directly:

- `--external-compression` uses the external zstd decoder when available;
- `--allow-missing-codecs` starts without every external decoder;
- `--import-concurrency` defaults to one import per CPU; `0` removes the limit;
- `--nix-store-uri` selects the Nix store; `auto` uses the system daemon.

The default spool directory is `/var/cache/tsnixcache/spool`. For an
unprivileged process, choose a directory it owns. The server rejects a spool
that is a symlink, belongs to another user, or is group- or world-writable.

The flake provides packages for `x86_64-linux`, `aarch64-linux`, and
`aarch64-darwin`.
