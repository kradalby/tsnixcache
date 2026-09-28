# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

{
  pkgs,
  tsnixcache,
  tsnixcacheModule,
  headscaleTestkit,
  headscaleTestkitPeer,
}:

{
  name = "tsnixcache-tsnet";

  nodes = {
    headscale.imports = [ headscaleTestkit ];

    server =
      {
        config,
        pkgs,
        lib,
        ...
      }:
      {
        imports = [ tsnixcacheModule ];

        environment.systemPackages = [ pkgs.curl ];

        services.tsnixcache = {
          enable = true;
          package = tsnixcache;
          # Local listener for in-VM metric checks (no auth).
          listen = [ "127.0.0.1:5000" ];
          tsnet = [
            {
              hostname = "tsnixcache";
              controlUrl = "http://headscale";
              # Written by the test script after headscale issues the auth key.
              authKeyFile = "/var/lib/tsnixcache/tsnet-authkey";
              dir = "/var/lib/tsnixcache/tsnet";
              port = 80;
              tls = false;
            }
          ];
        };

        nix.settings.trusted-users = [
          "root"
          "tsnixcache"
        ];

        # Don't auto-start: the test writes the auth key first, then starts it.
        systemd.services.tsnixcache.wantedBy = lib.mkForce [ ];
        systemd.services.tsnixcache.environment.TS_NO_LOGS_NO_SUPPORT = "1";
      };

    pusher = { config, pkgs, ... }: {
      imports = [ headscaleTestkitPeer ];

      # hello lives here so the pusher can push it to the server.
      environment.systemPackages = [
        pkgs.hello
        pkgs.nix
        pkgs.python3
        pkgs.jq
      ];
      nix.settings = {
        trusted-users = [ "root" ];
        experimental-features = [ "nix-command" ];
      };
    };

    reader = { config, pkgs, ... }: {
      imports = [ headscaleTestkitPeer ];

      nix.settings = {
        # No pre-configured substituters; we'll pass the tsnet URL explicitly.
        substituters = pkgs.lib.mkForce [ ];
        require-sigs = false;
        experimental-features = [ "nix-command" ];
      };
    };
  };

  testScript = ''
    import json, shlex

    start_all()

    # ── Headscale bootstrap ────────────────────────────────────────────────────

    # hs-authkey also creates each user.
    server_key = headscale.succeed("hs-authkey server").strip()
    pusher_key = headscale.succeed("hs-authkey pusher").strip()
    reader_key = headscale.succeed("hs-authkey reader").strip()

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

    # ── Start tsnixcache ───────────────────────────────────────────────────────

    # Root-owned and mode 0400: the tsnixcache user cannot read this file, so
    # tsnet only comes up if the module loads it as a systemd credential.
    server.succeed("mkdir -p /var/lib/tsnixcache")
    server.succeed(f"echo -n '{server_key}' > /var/lib/tsnixcache/tsnet-authkey")
    server.succeed("chmod 400 /var/lib/tsnixcache/tsnet-authkey")
    server.systemctl("start tsnixcache")
    server.wait_for_unit("tsnixcache.service")

    # ── Enrol Tailscale clients ────────────────────────────────────────────────

    pusher.succeed(f"hs-join {pusher_key}")
    reader.succeed(f"hs-join {reader_key}")

    # ── Discover tsnixcache tsnet IP ───────────────────────────────────────────

    # An active unit only means tsnet started; it registers in the background.
    # Both clients' requests below are single-shot, so wait until each sees it.
    tsnet_ip = pusher.wait_until_succeeds("tailscale ip -4 tsnixcache").strip()
    reader.wait_until_succeeds("tailscale ip -4 tsnixcache")

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
    # Read-only peers must not reach debugger payloads or its GET-shaped GC action.
    for path in ["/debug/vars", "/debug/pprof/goroutine?debug=1", "/debug/gc"]:
        body = reader.succeed(
            f"curl -sS --max-time 10 -o /tmp/denied -w '%{{http_code}}' http://{tsnet_ip}" + shlex.quote(path))
        assert body == "403", (path, body)
        assert "push grant" in reader.succeed("cat /tmp/denied")
    for path in ["/nix-cache-info", "/metrics", "/health"]:
        reader.succeed(f"curl -fsS --max-time 10 http://{tsnet_ip}" + path)

    # Retain one actual TCP connection while only the application grant changes.
    pusher.succeed("echo initial >/run/revocation-phase")
    pusher.succeed(f"systemd-run --unit=revocation --property=Type=exec python3 ${./tsnet-revocation.py} {tsnet_ip}")

    def probe(phase, codes):
        pusher.wait_until_succeeds(
            "jq -e " + shlex.quote(".phase == " + json.dumps(phase) + " and .statuses == " + json.dumps(codes)) +
            " /run/revocation.json", timeout=120)
        result = json.loads(pusher.succeed("cat /run/revocation.json"))
        print("same-connection grant probe:", result)
        return result["port"]

    port = probe("initial", [200, 200, 200])
    original = json.loads(headscale.succeed("cat /tmp/policy.json"))
    revoked = json.loads(json.dumps(original))
    revoked["grants"][0]["src"] = ["reader@"]
    headscale.succeed("printf '%s' " + shlex.quote(json.dumps(revoked)) + " >/tmp/revoked.json")
    headscale.succeed("headscale policy set -f /tmp/revoked.json")
    pusher.succeed("echo revoked >/run/revocation-phase")
    assert probe("revoked", [403, 403, 200]) == port
    reader.wait_until_succeeds(f"curl -fsS --max-time 10 http://{tsnet_ip}/debug/vars", timeout=120)
    reader.succeed(f"curl -fsS --max-time 10 -X PUT --data @/dev/null http://{tsnet_ip}/nar/authorized.nar")

    headscale.succeed("headscale policy set -f /tmp/policy.json")
    pusher.succeed("echo restored >/run/revocation-phase")
    assert probe("restored", [200, 200, 200]) == port
    pusher.succeed("systemctl stop revocation")
  '';
}
