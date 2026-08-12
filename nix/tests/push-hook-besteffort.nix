# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

# Best-effort post-build-hook: a build must succeed even when the cache is
# unreachable. No server node — the hook points at a dead address.
{ pkgs, tsnixcache, tsnixcacheModule, tsnixcacheClientModule }:

let
  common = import ./push-common.nix { inherit pkgs tsnixcache tsnixcacheModule; };
in
{
  name = "tsnixcache-push-hook-besteffort";

  nodes = {
    client = { config, pkgs, ... }: {
      imports = [ tsnixcacheClientModule ];
      environment.systemPackages = [ pkgs.nix ];
      system.extraDependencies = common.buildInputs;
      nix.settings.sandbox = false;
      nix.settings.trusted-users = [ "root" ];
      services.tsnixcache-client = {
        enable = true;
        package = tsnixcache;
        publicKey = common.publicKey;
        substituters = [ ];
        postBuildHook = {
          enable = true;
          # Nothing listens here; push exhausts retries fast, hook must exit 0.
          to = "http://127.0.0.1:1";
          timeout = "3s";
        };
      };
    };
  };

  testScript = ''
    start_all()
    client.wait_for_unit("multi-user.target")

    # The cache is unreachable, but the best-effort hook must still let the build
    # succeed. Capturing stderr too lets us assert the hook actually ran and gave
    # up deliberately — otherwise a regression that silently skips the push, or
    # one that starts failing the build, would pass unnoticed.
    output = client.succeed("nix-build --no-out-link ${common.buildExpr} 2>&1")
    assert "/nix/store/" in output, f"build produced no store path:\n{output}"
    assert "not failing build" in output, (
        f"best-effort hook did not log its give-up warning:\n{output}"
    )
  '';
}
