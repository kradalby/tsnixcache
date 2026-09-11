# tsnixcache

A Nix binary cache for your tailnet. It serves and signs store paths, accepts
pushes from trusted Tailscale nodes, and includes modules for NixOS and
nix-darwin.

Build once, and every machine on your tailnet can fetch the result.

## Quick start

Add the flake:

```nix
inputs.tsnixcache = {
  url = "github:kradalby/tsnixcache";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

Generate a signing key on the cache host:

```sh
sudo mkdir -p /etc/tsnixcache
sudo sh -c 'umask 077; nix run \
  --extra-experimental-features "nix-command flakes" \
  github:kradalby/tsnixcache -- key generate --name cache.example.com \
  > /etc/tsnixcache/key'
```

The command writes the secret key to the file and prints the public key.

Enable the NixOS server module:

```nix
{ inputs, ... }:
{
  imports = [ inputs.tsnixcache.nixosModules.tsnixcache ];
  nixpkgs.overlays = [ inputs.tsnixcache.overlays.default ];

  services.tsnixcache = {
    enable = true;
    signKeyFile = "/etc/tsnixcache/key";
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

Create the tsnet node with a tagged auth key that gives it `tag:cache`, then
allow trusted builders to push:

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

Enable the NixOS client module with the public key printed above:

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

Check the server, push one path, and inspect its signature:

```sh
curl -fsS http://tsnixcache/health
p=$(nix build --no-link --print-out-paths nixpkgs#hello)
tsnixcache push --to http://tsnixcache "$p"
curl -fsS "http://tsnixcache/$(basename "$p" | cut -c1-32).narinfo"
```

The narinfo should contain a `Sig:` line for `cache.example.com`.

## Documentation

Read the [documentation index](docs/README.md) for full setup, nix-darwin,
retries, CI, security, monitoring, and garbage collection.

## Security

Cache reads are unauthenticated. Limit network access if store paths contain
private data. Grant push access only to machines you trust as builders.

## Licence

BSD 3-Clause. See [LICENSE](LICENSE).
