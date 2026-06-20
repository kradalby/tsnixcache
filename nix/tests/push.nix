{ pkgs, tsnixcache, tsnixcacheModule }:

{
  name = "tsnixcache-push";

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
          grep "^public:" /var/lib/tsnixcache/keygen.out \
            | cut -d' ' -f2 \
            > /var/lib/tsnixcache/cache-key.public
        fi
        mkdir -p /nix/var/nix/gcroots/tsnixcache /var/cache/tsnixcache/spool
      '';
      services.tsnixcache.signKeyFile = "/var/lib/tsnixcache/cache-key.secret";
      # Server must be a trusted user so it can import pushed paths.
      nix.settings.trusted-users = [ "root" "tsnixcache" ];
      networking.firewall.allowedTCPPorts = [ 5000 ];
    };

    pusher = { config, pkgs, ... }: {
      # The pusher needs nix copy and the hello package to push.
      environment.systemPackages = [ pkgs.hello pkgs.nix ];
      nix.settings.trusted-users = [ "root" ];
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

    # Push hello from the pusher node.
    pusher.succeed(
      "nix copy --to http://server:5000 ${pkgs.hello} 2>&1"
    )

    # Check push metrics.
    server.succeed(
      "curl -sf http://localhost:5000/metrics | grep -E 'push_nar_total.*[1-9]'"
    )
    server.succeed(
      "curl -sf http://localhost:5000/metrics | grep -E 'import_success_total.*[1-9]'"
    )
    server.succeed(
      "curl -sf http://localhost:5000/metrics | grep 'import_fail_total 0'"
    )

    # Consumer fetches the pushed path.
    consumer.succeed(
      "nix-store --realise ${pkgs.hello} --substituters http://server:5000 2>&1"
    )
  '';
}
