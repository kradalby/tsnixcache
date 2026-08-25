# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

# Auto-push via nix post-build-hook only (the exact, recommended mechanism).
{
  pkgs,
  tsnixcache,
  tsnixcacheModule,
  tsnixcacheClientModule,
}:

let
  common = import ./push-common.nix { inherit pkgs tsnixcache tsnixcacheModule; };
in
{
  name = "tsnixcache-push-hook";

  nodes = {
    server = { config, pkgs, ... }: {
      imports = [ common.serverNode ];
      # Only the server has hello, so the client can only get it by
      # substituting from the cache.
      environment.systemPackages = [ pkgs.hello ];
    };

    client = { config, pkgs, ... }: {
      imports = [ tsnixcacheClientModule ];
      environment.systemPackages = [ pkgs.nix ];
      system.extraDependencies = common.buildInputs;
      # Nested VM cannot set up the build sandbox; build against the real store
      # (inputs pinned via extraDependencies above).
      nix.settings.sandbox = false;
      nix.settings.trusted-users = [ "root" ];
      services.tsnixcache-client = {
        enable = true;
        package = tsnixcache;
        publicKey = common.publicKey;
        # Every other client-module test blanks this and configures nix by
        # hand, so the module's own substituter and trusted-key wiring — its
        # headline feature — is proven here or nowhere.
        substituters = [ "http://server:5000" ];
        postBuildHook = {
          enable = true;
          to = "http://server:5000";
        };
      };
    };
  };

  testScript = ''
    start_all()

    server.wait_for_unit("tsnixcache.service")
    server.wait_for_open_port(5000)
    client.wait_for_unit("multi-user.target")

    # Build a fresh derivation; the post-build-hook runs synchronously, so the
    # path is already on the server by the time nix-build returns.
    outp = client.succeed("nix-build --no-out-link ${common.buildExpr}").strip()
    hashpart = outp.split("/")[-1][:32]

    server.succeed(f"curl -sf http://localhost:5000/{hashpart}.narinfo")
    server.succeed("curl -sf http://localhost:5000/metrics | grep -E '^tsnixcache_import_success_total [1-9]'")
    server.succeed("curl -sf http://localhost:5000/metrics | grep -Fx 'tsnixcache_import_fail_total 0'")

    # Substitute purely through the nix.conf the module wrote: no --substituters
    # here, and hello is valid only on the server, so this fails unless the
    # module's substituters and trusted-public-keys both landed. Signature
    # checking is on (nix's default), so the key wiring is load-bearing too.
    #
    # Validity, not "test -e": every node mounts the build host's /nix/store,
    # and naming ${pkgs.hello} in this script is itself enough to build it there,
    # so the path's *files* are visible on the client no matter what. Only the
    # node's own database says whether nix considers it realised.
    client.fail("nix-store --check-validity ${pkgs.hello}")
    client.succeed("nix-store --realise ${pkgs.hello} 2>&1")
    client.succeed("nix-store --check-validity ${pkgs.hello}")
  '';
}
