{ pkgs, tsnixcache, tsnixcacheModule }:

{
  name = "tsnixcache-serve";

  nodes = {
    server = { config, pkgs, ... }: {
      imports = [ tsnixcacheModule ];
      services.tsnixcache = {
        enable = true;
        package = tsnixcache;
        listen = [ "0.0.0.0:5000" ];
        priority = 30;
      };
      # Generate a signing key before the service starts.
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

    # Verify /nix-cache-info is served.
    server.succeed("curl -sf http://localhost:5000/nix-cache-info | grep StoreDir")

    # Verify /health returns ok.
    server.succeed("curl -sf http://localhost:5000/health | grep ok")

    # Fetch a package from the server on the client via nix substitution.
    # pkgs.hello is in the server's /nix/store.
    client.succeed("nix-env -iA nixpkgs.hello --substituters http://server:5000 2>&1")
    client.succeed("hello")

    # Check metrics endpoint incremented.
    server.succeed("curl -sf http://localhost:5000/metrics | grep narinfo_hits_total")
  '';
}
