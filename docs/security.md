# Security

Cache reads are unauthenticated. A node allowed to push is trusted as a Nix
builder.

| Listener       | Reads                      | Writes                    | `/debug/`                                      |
| -------------- | -------------------------- | ------------------------- | ---------------------------------------------- |
| tsnet          | Any reachable tailnet node | Nodes with the push grant | Debug gate plus the push grant                 |
| Plain `listen` | Any reachable client       | Denied by default         | Debug gate; force-GC also needs writes enabled |

## Restrict read access

A Nix substituter sends no credentials. Anyone who can reach a listener can:

- fetch a narinfo or NAR when they know its store hash;
- read `/health` and `/metrics`;
- use any other unauthenticated read endpoint.

There is no listing endpoint, but store hashes are not secrets. There is no
per-path authorisation. Restrict network access when closures contain private
artefacts or embedded secrets.

Read-only plain listeners still expose downloads. The NixOS module warns when
one listens beyond loopback.

## Grant push access carefully

A node with the push grant can add arbitrary paths to the server's Nix store.
The server checks that content matches its NAR hash; it cannot prove that the
claimed store path came from a legitimate build.

When signing is configured, the cache signs what it serves. Lower priority
numbers are preferred, and this cache defaults to 30. A malicious path can
shadow an upstream path for clients that trust this cache.

Grant push access only to machines you trust as remote builders or Nix trusted
users.

## Use plain listeners safely

A plain socket has no caller identity. It refuses writes with `403` by default
while continuing to serve reads.

`services.tsnixcache.localWrite` enables writes on every configured plain
listener. The CLI equivalent is `--local-write`. Enable it only when every
client that can reach the socket is trusted to build.

This includes all local users for a loopback listener and all reachable network
clients for a non-loopback listener. The server logs a startup warning, and the
NixOS module emits an evaluation warning.

Rejected plain-listener writes increment:

```text
tsnixcache_auth_rejects_total{reason="local_write"}
```

The log includes the remote address and method.

## Protect debug access

Every `/debug/` request first passes Tailscale's debug-access check. It admits
loopback and Tailscale-range addresses, `TS_ALLOW_DEBUG_IP`, trusted CIDRs, or a
valid debug key. Other clients receive `403`.

On tsnet, the push grant is also required. On a plain listener, `/debug/gc`
additionally requires local writes because it changes server state.

See [Debug endpoints](operations.md#debug-endpoints) for operational guidance.

## Remove the plain listener

Set this when you want tsnet only:

```nix
services.tsnixcache.listen = [ ];
```

The CLI defaults to `127.0.0.1:5000` only when neither `--listen` nor `--tsnet`
is supplied. Passing `--listen ""` explicitly creates no listener; without
tsnet, `serve` exits with `no listeners configured`.
