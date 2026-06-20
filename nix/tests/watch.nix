{ pkgs, tsnixcache, tsnixcacheModule }:

{
  name = "tsnixcache-watch";

  nodes = {
    server = { config, pkgs, ... }: {
      imports = [ tsnixcacheModule ];
      services.tsnixcache = {
        enable = true;
        package = tsnixcache;
        listen = [ "0.0.0.0:5000" ];
        priority = 30;
      };
      systemd.services.tsnixcache.preStart = pkgs.lib.mkBefore ''
        if [ ! -f /var/lib/tsnixcache/cache-key.secret ]; then
          ${tsnixcache}/bin/tsnixcache key generate --name cache \
            > /var/lib/tsnixcache/keygen.out
          grep "^private:" /var/lib/tsnixcache/keygen.out \
            | cut -d' ' -f2 \
            > /var/lib/tsnixcache/cache-key.secret
        fi
        mkdir -p /nix/var/nix/gcroots/tsnixcache /var/cache/tsnixcache/spool
      '';
      services.tsnixcache.signKeyFile = "/var/lib/tsnixcache/cache-key.secret";
      nix.settings.trusted-users = [ "root" "tsnixcache" ];
      networking.firewall.allowedTCPPorts = [ 5000 ];
    };

    watcher = { config, pkgs, ... }: {
      # The watcher watches the local store and pushes new paths to the server.
      environment.systemPackages = [ tsnixcache pkgs.nix ];
      nix.settings.trusted-users = [ "root" ];

      systemd.services.tsnixcache-watch = {
        description = "tsnixcache watch";
        wantedBy = [ "multi-user.target" ];
        after = [ "network.target" "nix-daemon.service" ];
        serviceConfig = {
          ExecStart = "${tsnixcache}/bin/tsnixcache watch --to http://server:5000";
          Restart = "on-failure";
        };
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

    watcher.wait_for_unit("tsnixcache-watch.service")

    # Build something on the watcher node to put it in the store.
    watcher.succeed("nix-build -E 'with import <nixpkgs> {}; hello' 2>&1")

    # Give the watcher time to detect the new path and push it.
    watcher.sleep(10)

    # Check that the server received the push.
    server.succeed(
      "curl -sf http://localhost:5000/metrics | grep -E 'push_nar_total.*[1-9]'"
    )

    # Consumer should be able to fetch the path from the server.
    consumer.succeed(
      "nix-build -E 'with import <nixpkgs> {}; hello' --substituters http://server:5000 2>&1"
    )
  '';
}
