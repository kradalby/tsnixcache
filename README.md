# tsnixcache

A Nix binary cache for your tailnet. It serves and signs store paths, accepts
pushes from authorised Tailscale nodes, and includes modules for NixOS and
nix-darwin.

## Add the flake

```nix
inputs.tsnixcache = {
  url = "github:kradalby/tsnixcache";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

## Configure the server

Generate a signing key on the cache host:

```sh
sudo mkdir -p /etc/tsnixcache
sudo sh -c 'umask 077; nix run \
  --extra-experimental-features "nix-command flakes" \
  github:kradalby/tsnixcache -- key generate --name cache.example.com \
  > /etc/tsnixcache/key'
```

The command writes the secret key to the file and prints the public key.

Enable the NixOS module:

```nix
{ inputs, pkgs, ... }:
{
  imports = [ inputs.tsnixcache.nixosModules.tsnixcache ];

  services.tsnixcache = {
    enable = true;
    package = inputs.tsnixcache.packages.${pkgs.system}.default;
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

See [server configuration](docs/configuration.md#server-nixos) for all module
options and standalone server settings.

Allow trusted builders to push with a Tailscale capability grant:

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

## Configure clients

Use the public key printed during server setup:

```nix
{ inputs, pkgs, ... }:
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

The default cache URL is `http://tsnixcache`. `postBuildHook` uploads new builds;
`watch` retries failed uploads and catches paths added by other means.

See [client configuration](docs/configuration.md#client-nixos) for module options
on NixOS and nix-darwin.

## Use the cache

Push a path manually:

```sh
tsnixcache push --to http://tsnixcache /run/current-system
```

Check the service:

```sh
curl http://tsnixcache/health
```

Run `tsnixcache --help` and `tsnixcache <command> --help` for command options.

## Documentation

The [documentation index](docs/README.md) covers module options, push
resilience, CI, security, operations, and development.

## Security

Cache reads are unauthenticated. Any node that can reach the service can download
stored paths and view health and metrics data. Limit network access if the store
contains private artefacts.

A node allowed to push is trusted to add paths that the cache will sign and serve.
Grant the push capability only to trusted builders. Keep the signing key private
and backed up.

## Development

See the [development guide](docs/development.md) for checks and contribution
conventions.

## Licence

BSD 3-Clause. See [`LICENSE`](LICENSE).
