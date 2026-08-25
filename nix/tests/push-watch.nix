# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

# Auto-push via the watch daemon only (fallback for non-built paths).
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
  name = "tsnixcache-push-watch";

  nodes = {
    server = common.serverNode;

    client = { config, pkgs, ... }: {
      imports = [ tsnixcacheClientModule ];
      environment.systemPackages = [ pkgs.nix ];
      nix.settings = {
        trusted-users = [ "root" ];
        experimental-features = [ "nix-command" ];
      };
      services.tsnixcache-client = {
        enable = true;
        package = tsnixcache;
        publicKey = common.publicKey;
        substituters = [ ];
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

    # wait_for_unit only means systemd forked the process, not that the watcher
    # has read its baseline cursor (the highest ValidPaths id at startup). It
    # pushes what is registered *after* that read, so a path added in the gap
    # lands at or below the baseline and is correctly ignored — the test then
    # waits for a push that is never coming. Wait for the watcher to say it has
    # taken the baseline before adding anything.
    client.wait_until_succeeds(
      "journalctl -u tsnixcache-watch.service | grep -F 'watch: starting'",
      timeout=60,
    )

    # nix-store --add does not trigger a build, so only the watcher catches it.
    p = client.succeed("echo tsnixcache-watch-test > /tmp/wf && nix-store --add /tmp/wf").strip()
    hashpart = p.split("/")[-1][:32]

    # The watcher detects the new path and pushes it asynchronously.
    server.wait_until_succeeds(f"curl -sf http://localhost:5000/{hashpart}.narinfo", timeout=60)
  '';
}
