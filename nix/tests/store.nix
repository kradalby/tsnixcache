# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

{ pkgs, tsnixcache, tsnixcacheModule }:

let
  chroot = "/var/lib/tsnixcache/chroot";
  helloName = builtins.baseNameOf pkgs.hello;
  helloHash = builtins.substring 0 32 helloName;
in
{
  name = "tsnixcache-store";

  nodes = {
    # Server imports into a self-contained chroot store and is deliberately
    # NOT a trusted user: a separate store needs no machine-wide privilege.
    server = { config, pkgs, ... }: {
      imports = [ tsnixcacheModule ];
      services.tsnixcache = {
        enable = true;
        package = tsnixcache;
        listen = [ "0.0.0.0:5000" ];
        # The pusher below writes over that plain listener, which has no
        # identity to check and so refuses writes unless asked.
        localWrite = true;
        store = chroot;
      };
      environment.systemPackages = [ pkgs.nix ];
      networking.firewall.allowedTCPPorts = [ 5000 ];
    };

    pusher = { config, pkgs, ... }: {
      environment.systemPackages = [ pkgs.hello pkgs.nix ];
      nix.settings = {
        trusted-users = [ "root" ];
        experimental-features = [ "nix-command" ];
      };
    };

    consumer = { config, pkgs, ... }: {
      nix.settings = {
        substituters = pkgs.lib.mkForce [ "http://server:5000" ];
        require-sigs = false;
      };
    };
  };

  testScript = ''
    start_all()

    server.wait_for_unit("tsnixcache.service")
    server.wait_for_open_port(5000)
    # Wait for the pushing node too: succeed() only waits for the machine to
    # boot, not for its resolver to be up, and nix copy gives DNS one second
    # before it fails the whole transfer.
    pusher.wait_for_unit("multi-user.target")
    consumer.wait_for_unit("multi-user.target")

    # The chroot store is initialised on first start, separate from /nix/store.
    server.succeed("test -e ${chroot}/nix/var/nix/db/db.sqlite")

    # The served store dir is the logical /nix/store, not the chroot path.
    server.succeed("curl -sf http://localhost:5000/nix-cache-info | grep -Fx 'StoreDir: /nix/store'")

    # tsnixcache serves only the chroot store: hello is unknown before the push
    # even though the path already exists in the server's system /nix/store.
    server.succeed(
      "test $(curl -s -o /dev/null -w '%{http_code}' http://localhost:5000/${helloHash}.narinfo) = 404"
    )

    # Push hello from the pusher.
    pusher.succeed("nix copy --to http://server:5000 ${pkgs.hello} 2>&1")

    server.succeed("curl -sf http://localhost:5000/metrics | grep -E '^tsnixcache_import_success_total [1-9]'")
    server.succeed("curl -sf http://localhost:5000/metrics | grep -Fx 'tsnixcache_import_fail_total 0'")

    # The path landed in the chroot store and is now known to tsnixcache.
    server.succeed("test -e ${chroot}/nix/store/${helloName}")
    server.succeed("curl -sf http://localhost:5000/${helloHash}.narinfo")

    # Fetch the NAR itself: this streams from the chroot store via storeRoot.
    nar_url = server.succeed(
      "curl -s http://localhost:5000/${helloHash}.narinfo | sed -n 's/^URL: //p'"
    ).strip()
    server.succeed(f"curl -sf http://localhost:5000/{nar_url} -o /tmp/h.nar && test -s /tmp/h.nar")

    # Consumer pulls it back, exercising the chroot read path end to end.
    consumer.succeed("nix-store --realise ${pkgs.hello} --substituters http://server:5000 2>&1")
  '';
}
