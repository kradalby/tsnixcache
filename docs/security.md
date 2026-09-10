# Security and trust model

The core trust rules are simple:

1. Anyone who can reach a cache listener can read cached paths.
2. Anyone allowed to push is trusted as a builder.

| Listener | Reads                      | Writes                    | `/debug/`                                                                  |
| -------- | -------------------------- | ------------------------- | -------------------------------------------------------------------------- |
| tsnet    | Any reachable tailnet node | Nodes with the push grant | Nodes with the push grant                                                  |
| `listen` | Any reachable client       | Denied by default         | Any reachable client; see [debug endpoints](operations.md#debug-endpoints) |

## Read access

### Reads are unauthenticated

A Nix substituter receives no credentials, so cache reads cannot require
authentication. Any client that can reach the listener can:

- enumerate hash parts;
- fetch narinfo files and NARs;
- read `/health` and `/metrics`.

This includes every tailnet node that can reach a tsnet listener. If the store
contains private build artefacts or secrets embedded in a closure, restrict
network access to the cache node. There is no per-path authorisation.

The same rule applies to a plain `listen` address. Making that listener
read-only prevents writes, but it does not restrict downloads. The NixOS module
warns when a plain listener uses a non-loopback address.

### Debug access

`/debug/` is the exception on tsnet. It requires the push grant because pprof can
expose process memory. Plain listeners have different behaviour; see
[debug endpoints](operations.md#debug-endpoints).

## Write access

### Pushers are trusted builders

A node with the push grant can write arbitrary paths into the server's Nix
store. The server verifies that the uploaded content matches its NAR hash, but
it does not prove that the claimed store path came from a legitimate build.

The cache signs everything it serves with its own key. Its default priority is
30, ahead of `cache.nixos.org` at 40, so a poisoned path can shadow the upstream
path for every client that trusts this cache.

Grant push access only to machines you would trust as Nix remote builders or
trusted users.

### Plain listeners refuse writes

A plain socket provides no caller identity. By default, every unsafe method is
therefore rejected with `403`, while reads remain available for substituter use.

This removes the path where any local account able to open the socket could
import arbitrary content and have the cache re-sign it.

### Enabling local writes

`services.tsnixcache.localWrite` enables writes on plain listeners. The CLI
equivalent is `--local-write`.

Enabling it grants write access to:

- every local user that can reach a loopback listener;
- every network client that can reach a non-loopback listener.

Use it only when every reachable client is trusted to build, such as on a
single-user builder or behind a strict firewall. Treat that access as indirect
use of the cache signing key.

The server logs a startup warning for each affected listener, and the NixOS
module emits an evaluation warning.

## Auditing rejected writes

Each rejected plain-listener write increments:

```text
tsnixcache_auth_rejects_total{reason="local_write"}
```

The server also logs the remote address and HTTP method.

## Listener edge cases

### No local listener

Set `services.tsnixcache.listen = [ ];` to run with tsnet only. The module emits
no `--listen` flag in this case.

The CLI falls back to `127.0.0.1:5000` only when neither `--listen` nor `--tsnet`
was supplied. A server configured with tsnet therefore does not gain an implicit
plain listener.

### Empty CLI listener

Passing `--listen ""` explicitly is different from omitting `--listen`. The
empty address resolves to no listener, so a command without tsnet exits with
`no listeners configured` instead of using the loopback fallback.
