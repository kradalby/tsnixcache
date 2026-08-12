# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

# Post-build-hook (primary) and watch (fallback) enabled together.
{ pkgs, tsnixcache, tsnixcacheModule, tsnixcacheClientModule }:

let
  common = import ./push-common.nix { inherit pkgs tsnixcache tsnixcacheModule; };
in
{
  name = "tsnixcache-push-both";

  nodes = {
    server = common.serverNode;

    client = { config, pkgs, ... }: {
      imports = [ tsnixcacheClientModule ];
      environment.systemPackages = [ pkgs.nix ];
      system.extraDependencies = common.buildInputs;
      nix.settings = {
        trusted-users = [ "root" ];
        experimental-features = [ "nix-command" ];
        # Nested VM cannot set up the build sandbox; build against the real store.
        sandbox = false;
      };
      services.tsnixcache-client = {
        enable = true;
        package = tsnixcache;
        publicKey = common.publicKey;
        substituters = [ ];
        postBuildHook = {
          enable = true;
          to = "http://server:5000";
        };
        watch = {
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
    client.wait_for_unit("tsnixcache-watch.service")

    # See push-watch.nix: wait_for_unit only means the process was forked, and
    # the watcher ignores anything registered at or below the baseline cursor it
    # reads at startup. The nix-build below usually covers the gap, but relying
    # on a build being slow is not synchronisation.
    client.wait_until_succeeds(
      "journalctl -u tsnixcache-watch.service | grep -F 'watch: starting'",
      timeout=60,
    )

    # A built path is pushed synchronously by the post-build-hook: present at once.
    outp = client.succeed("nix-build --no-out-link ${common.buildExpr}").strip()
    hp_build = outp.split("/")[-1][:32]
    server.succeed(f"curl -sf http://localhost:5000/{hp_build}.narinfo")

    # An added (non-built) path is caught only by the watch fallback.
    p = client.succeed("echo tsnixcache-both-test > /tmp/wf && nix-store --add /tmp/wf").strip()
    hp_add = p.split("/")[-1][:32]
    server.wait_until_succeeds(f"curl -sf http://localhost:5000/{hp_add}.narinfo", timeout=60)
  '';
}
