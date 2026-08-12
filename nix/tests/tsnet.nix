# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

{ pkgs, tsnixcache, tsnixcacheModule, headscale }:

let
  # TLS cert for headscale, generated at eval time so all nodes can trust it.
  # The SAN includes IP 192.168.1.1 — headscale is the first node alphabetically
  # and therefore gets that address on the test VLAN.
  # Long validity is load-bearing: this derivation is cached, so a short-lived
  # cert would expire while the cached build is reused, and every run past expiry
  # hangs on the control-plane TLS handshake until the test's global timeout.
  tlsCert = pkgs.runCommand "headscale-test-cert" { } ''
    mkdir -p $out
    ${pkgs.openssl}/bin/openssl req -x509 -newkey rsa:2048 \
      -keyout $out/key.pem -out $out/cert.pem \
      -days 36500 -nodes \
      -subj '/CN=headscale' \
      -addext 'subjectAltName=DNS:headscale,IP:192.168.1.1'
  '';

  # Shared by every VM: trust the headscale cert and pin its hostname to the
  # test-VLAN IP.  tsnet prefers IPv6 on the test VLAN, so pinning to IPv4
  # prevents registration timeouts.
  sharedConfig = { ... }: {
    security.pki.certificateFiles = [ "${tlsCert}/cert.pem" ];
    networking.extraHosts = "192.168.1.1 headscale";
  };

  # Minimal Tailscale client for pusher/reader nodes.
  tailscaleClient = { ... }: {
    services.tailscale.enable = true;
    # Suppress logtail dials to public infrastructure (log.tailscale.io).
    systemd.services.tailscaled.environment.TS_NO_LOGS_NO_SUPPORT = "1";
  };
in
{
  name = "tsnixcache-tsnet";

  nodes = {
    headscale = { config, pkgs, lib, ... }: {
      imports = [ sharedConfig ];

      environment.systemPackages = [ pkgs.jq ];

      services.headscale = {
        enable = true;
        package = headscale;
        address = "127.0.0.1";
        port = 8080;
        settings = {
          server_url = "https://headscale";
          policy.mode = "database";
          dns = {
            magic_dns = false;
            override_local_dns = false;
          };
          # Built-in DERP so nodes can relay without public infrastructure.
          derp = {
            server = {
              enabled = true;
              region_id = 999;
              region_code = "test";
              region_name = "Test DERP";
              stun_listen_addr = "0.0.0.0:3478";
            };
            urls = [ ];
            auto_update_enabled = false;
          };
        };
      };

      # nginx TLS proxy — tsnet's control client requires HTTPS.
      services.nginx = {
        enable = true;
        virtualHosts."headscale" = {
          onlySSL = true;
          sslCertificate = "${tlsCert}/cert.pem";
          sslCertificateKey = "${tlsCert}/key.pem";
          locations."/" = {
            proxyPass = "http://127.0.0.1:8080";
            proxyWebsockets = true;
          };
        };
      };

      networking.firewall = {
        allowedTCPPorts = [ 443 ];
        allowedUDPPorts = [ 3478 ];
      };
    };

    server = { config, pkgs, lib, ... }: {
      imports = [ sharedConfig tsnixcacheModule ];

      environment.systemPackages = [ pkgs.curl ];

      services.tsnixcache = {
        enable = true;
        package = tsnixcache;
        # Local listener for in-VM metric checks (no auth).
        listen = [ "127.0.0.1:5000" ];
        tsnet = [{
          hostname = "tsnixcache";
          controlUrl = "https://headscale";
          # Written by the test script after headscale issues the auth key.
          authKeyFile = "/var/lib/tsnixcache/tsnet-authkey";
          dir = "/var/lib/tsnixcache/tsnet";
          port = 80;
          tls = false;
        }];
      };

      nix.settings.trusted-users = [ "root" "tsnixcache" ];

      # Don't auto-start: the test writes the auth key first, then starts it.
      systemd.services.tsnixcache.wantedBy = lib.mkForce [ ];
      systemd.services.tsnixcache.environment.TS_NO_LOGS_NO_SUPPORT = "1";
      # Force tsnet to treat network as up (avoids pause-until-link-change race on a stable test VLAN).
      systemd.services.tsnixcache.environment.TS_ASSUME_NETWORK_UP_FOR_TEST = "1";
      systemd.services.tsnixcache.environment.TS_DEBUG_REGISTER = "1";
    };

    pusher = { config, pkgs, ... }: {
      imports = [ sharedConfig tailscaleClient ];

      # hello lives here so the pusher can push it to the server.
      environment.systemPackages = [ pkgs.hello pkgs.nix ];
      nix.settings = {
        trusted-users = [ "root" ];
        experimental-features = [ "nix-command" ];
      };
    };

    reader = { config, pkgs, ... }: {
      imports = [ sharedConfig tailscaleClient ];

      nix.settings = {
        # No pre-configured substituters; we'll pass the tsnet URL explicitly.
        substituters = pkgs.lib.mkForce [ ];
        require-sigs = false;
        experimental-features = [ "nix-command" ];
      };
    };
  };

  testScript = ''
    import json

    start_all()

    headscale.wait_for_unit("headscale.service")
    headscale.wait_for_unit("nginx.service")
    headscale.wait_for_open_port(443)

    # ── Headscale bootstrap ────────────────────────────────────────────────────

    headscale.succeed("headscale users create server")
    headscale.succeed("headscale users create pusher")
    headscale.succeed("headscale users create reader")

    # Policy: accept all traffic; grant push capability only to pusher→server.
    headscale.succeed("""
      cat > /tmp/policy.json << 'EOF'
    {
      "acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}],
      "grants": [{
        "src": ["pusher@"],
        "dst": ["server@"],
        "app": {
          "kradalby.no/cap/tsnixcache": [{"push": true}]
        }
      }]
    }
    EOF
      headscale policy set -f /tmp/policy.json
    """)

    def user_id(name):
        return headscale.succeed(
          f"headscale users list -o json | jq -r '.[] | select(.name==\"{name}\") | .id'"
        ).strip()

    server_id = user_id("server")
    pusher_id = user_id("pusher")
    reader_id = user_id("reader")

    server_key = headscale.succeed(
      f"headscale preauthkeys create --user {server_id} --reusable -o json | jq -r .key"
    ).strip()
    pusher_key = headscale.succeed(
      f"headscale preauthkeys create --user {pusher_id} --reusable -o json | jq -r .key"
    ).strip()
    reader_key = headscale.succeed(
      f"headscale preauthkeys create --user {reader_id} --reusable -o json | jq -r .key"
    ).strip()

    # ── Start tsnixcache ───────────────────────────────────────────────────────

    # Root-owned and mode 0400: the tsnixcache user cannot read this file, so
    # tsnet only comes up if the module loads it as a systemd credential.
    server.succeed("mkdir -p /var/lib/tsnixcache")
    server.succeed(f"echo -n '{server_key}' > /var/lib/tsnixcache/tsnet-authkey")
    server.succeed("chmod 400 /var/lib/tsnixcache/tsnet-authkey")
    server.systemctl("start tsnixcache")
    server.wait_for_unit("tsnixcache.service")

    # ── Diagnostics: verify headscale is reachable from server VM ─────────────
    # Dump tsnixcache logs to stdout so they appear in nix log output.
    rc, curl_out = server.execute("curl -sk --head --max-time 10 https://headscale/ 2>&1")
    print(f"curl headscale from server (rc={rc}): {curl_out}")
    rc, jctl_out = server.execute("journalctl -u tsnixcache --no-pager 2>&1")
    print(f"tsnixcache journal:\n{jctl_out}")

    # ── Enrol Tailscale clients ────────────────────────────────────────────────

    pusher.wait_for_unit("tailscaled.service")
    pusher.succeed(
      f"tailscale up --login-server https://headscale --auth-key {pusher_key} --accept-routes"
    )

    reader.wait_for_unit("tailscaled.service")
    reader.succeed(
      f"tailscale up --login-server https://headscale --auth-key {reader_key} --accept-routes"
    )

    # ── Discover tsnixcache tsnet IP ───────────────────────────────────────────

    def get_tsnet_ip():
        out = headscale.succeed("headscale nodes list -o json")
        nodes = json.loads(out)
        names = [n.get("givenName", n.get("name", "")) for n in nodes]
        print("headscale nodes: " + str(names))
        for node in nodes:
            name = node.get("givenName", node.get("name", ""))
            if "tsnixcache" in name:
                for addr in node.get("ipAddresses", node.get("ip_addresses", [])):
                    if "." in addr:   # IPv4 preferred; tsnet may emit IPv6 first
                        return addr
        return None

    # Dump journal before long retry so we can see if tsnet connected.
    rc, jctl_out = server.execute("journalctl -u tsnixcache --no-pager 2>&1")
    print(f"tsnixcache journal before retry:\n{jctl_out}")

    retry(lambda _: get_tsnet_ip() is not None)
    tsnet_ip = get_tsnet_ip()

    # ── Push (authenticated write) ─────────────────────────────────────────────

    # pusher has the push capability — nix copy must succeed.
    pusher.succeed(f"nix copy --to http://{tsnet_ip} ${pkgs.hello} 2>&1")

    # Verify via the local no-auth listener that the import succeeded.
    server.succeed(
      "curl -sf http://localhost:5000/metrics"
      " | grep -E '^tsnixcache_push_nar_total [1-9]'"
    )
    server.succeed(
      "curl -sf http://localhost:5000/metrics"
      " | grep -E '^tsnixcache_import_success_total [1-9]'"
    )
    server.succeed(
      "curl -sf http://localhost:5000/metrics"
      " | grep -Fx 'tsnixcache_import_fail_total 0'"
    )

    # ── Pull (open read) ───────────────────────────────────────────────────────

    # reader has no push cap but reads are always open.
    reader.succeed(
      f"nix-store --realise ${pkgs.hello} --substituters http://{tsnet_ip} 2>&1"
    )

    # ── Deny write without capability ─────────────────────────────────────────

    # reader must get HTTP 403 on any write; use curl to avoid nix copy's
    # skip-if-present optimisation hiding the rejection.
    reader.succeed(
      f"curl -s -o /dev/null -w '%{{http_code}}'"
      f" -X PUT http://{tsnet_ip}/nar/test.nar --data 'x'"
      f" | grep -Fx 403"
    )
  '';
}
