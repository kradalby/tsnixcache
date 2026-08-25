# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

{
  pkgs,
  tsnixcache,
  tsnixcacheModule,
}:

let
  common = import ./push-common.nix { inherit pkgs tsnixcache tsnixcacheModule; };
in
{
  name = "tsnixcache-serve";

  nodes = {
    server = { config, pkgs, ... }: {
      imports = [
        tsnixcacheModule
        common.keyFileConfig
      ];
      services.tsnixcache = {
        enable = true;
        package = tsnixcache;
        listen = [ "0.0.0.0:5000" ];
        priority = 30;
        signKeyFile = common.keyFile;
        # The only coverage of the zstd serve path: the client fetch below
        # proves real nix accepts a NAR compressed on the fly out of the spool.
        serveCompression = "zstd";
      };
      # Ensure hello is in the server's store so it can be served.
      environment.systemPackages = [ pkgs.hello ];
      networking.firewall.allowedTCPPorts = [ 5000 ];
    };

    client = { config, pkgs, ... }: {
      # Allow the client to use the server as a substituter.
      nix.settings = {
        substituters = pkgs.lib.mkForce [ "http://server:5000" ];
        trusted-public-keys = [ ];
      };
      # Needed to fetch without signature check in tests.
      nix.settings.require-sigs = false;
    };
  };

  testScript = ''
    start_all()

    server.wait_for_unit("tsnixcache.service")
    server.wait_for_open_port(5000)

    # The module runs the cache unprivileged. Nothing else notices if it does
    # not: drop User= and every test in the suite still passes with the server
    # running as root.
    pid = server.succeed("systemctl show -p MainPID --value tsnixcache.service").strip()
    server.succeed(f"test $(ps -o uid= -p {pid}) -eq $(id -u tsnixcache)")

    # Verify /nix-cache-info is served.
    server.succeed("curl -sf http://localhost:5000/nix-cache-info | grep StoreDir")

    # Verify /health returns ok.
    server.succeed("curl -sf http://localhost:5000/health | grep ok")

    # Fetch pkgs.hello from the server (it is in the server's store via systemPackages).
    client.succeed("nix-store --realise ${pkgs.hello} --substituters http://server:5000 2>&1")

    # The fetch above must show up as a hit. Anchored to the start of the line
    # and to a non-zero value: unanchored, the pattern matches the "# HELP"
    # line and passes with the counter still at zero.
    server.succeed(
      "curl -sf http://localhost:5000/metrics"
      " | grep -E '^tsnixcache_narinfo_hits_total\\{[^}]*\\} [1-9]'"
    )
  '';
}
