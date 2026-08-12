# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

{ pkgs, tsnixcache, tsnixcacheModule }:

let
  common = import ./push-common.nix { inherit pkgs tsnixcache tsnixcacheModule; };
in
{
  name = "tsnixcache-push";

  nodes = {
    server = { config, pkgs, lib, ... }: {
      imports = [ common.serverNode ];
      services.tsnixcache.priority = 30;

      # The same server with localWrite back at its default, so the second half
      # of the test can show a real nix client being refused on a real listener.
      # A specialisation rather than a second node: it switches in place, so the
      # gate costs a system closure instead of another VM boot.
      specialisation.readonly.configuration = {
        services.tsnixcache.localWrite = lib.mkForce false;
      };
    };

    pusher = { config, pkgs, ... }: {
      # The pusher needs nix copy and the hello package to push.
      environment.systemPackages = [ pkgs.hello pkgs.nix ];
      nix.settings = {
        trusted-users = [ "root" ];
        # nix copy uses the nix-command experimental feature.
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

  testScript = { nodes, ... }: ''
    start_all()

    server.wait_for_unit("tsnixcache.service")
    server.wait_for_open_port(5000)
    # succeed() waits for the machine to boot, not for its resolver, and nix
    # copy gives DNS one second before it fails the whole transfer.
    pusher.wait_for_unit("multi-user.target")

    # Push hello from the pusher node.
    pusher.succeed(
      "nix copy --to http://server:5000 ${pkgs.hello} 2>&1"
    )

    # Check push metrics. Anchored to the start of the line and to a non-zero
    # value: unanchored, these match the "# HELP" line of the same metric and
    # pass with the counter still at zero.
    server.succeed(
      "curl -sf http://localhost:5000/metrics | grep -E '^tsnixcache_push_nar_total [1-9]'"
    )
    server.succeed(
      "curl -sf http://localhost:5000/metrics | grep -E '^tsnixcache_import_success_total [1-9]'"
    )
    server.succeed(
      "curl -sf http://localhost:5000/metrics | grep -Fx 'tsnixcache_import_fail_total 0'"
    )

    # Consumer fetches the pushed path.
    consumer.succeed(
      "nix-store --realise ${pkgs.hello} --substituters http://server:5000 2>&1"
    )

    # ── The write gate on a listener with no identity ─────────────────────────
    #
    # Everything above ran with localWrite = true, which is the only reason it
    # worked. Switch to the module's default and show the same push refused:
    # nothing identifies a caller on a plain listener, so it serves reads only.
    # Only a real nix client against a real listener can show that — a Go test
    # can prove the handler returns 403, not that nix's uploader hits it.
    server.succeed(
      "${nodes.server.system.build.toplevel}/specialisation/readonly/bin/switch-to-configuration test >&2"
    )

    # The unit restarts with the new command line; wait for the refusal itself
    # rather than for the port, which is briefly still the old process's.
    pusher.wait_until_succeeds(
      "curl -s -o /dev/null -w '%{http_code}'"
      " -X PUT http://server:5000/nar/poison.nar --data 'x' | grep -Fx 403",
      timeout=60,
    )

    # The refusal has to name the way back, or an operator who meant to push
    # here has a 403 and no idea what to do about it.
    body = pusher.succeed("curl -s -X PUT http://server:5000/nar/poison.nar --data 'x'")
    assert "--local-write" in body, f"refusal does not name the flag:\n{body}"

    # It is counted where the dashboard looks for it.
    server.succeed(
      "curl -sf http://localhost:5000/metrics"
      " | grep -E '^tsnixcache_auth_rejects_total\\{reason=\"local_write\"\\} [1-9]'"
    )

    # Now the same thing through nix itself, with a path the server has never
    # seen so nothing can be skipped as already present.
    gated = pusher.succeed("echo gated > /tmp/gated && nix-store --add /tmp/gated").strip()
    hashpart = gated.split("/")[-1][:32]

    out = pusher.fail(f"nix copy --to http://server:5000 {gated} 2>&1")
    assert "403" in out, f"nix copy failed, but not over the write gate:\n{out}"

    # …and it really did not land: a 403 that still imported would be worse
    # than no gate at all.
    server.succeed(
      f"test $(curl -s -o /dev/null -w '%{{http_code}}'"
      f" http://localhost:5000/{hashpart}.narinfo) = 404"
    )

    # Reads are untouched. Serving substituters over loopback is what a plain
    # listener is for, and closing writes must not cost that.
    pusher.succeed("curl -sf http://server:5000/nix-cache-info | grep -F StoreDir")
    pusher.succeed(
      "curl -sf http://server:5000/${builtins.substring 0 32 (builtins.baseNameOf pkgs.hello)}.narinfo"
    )
  '';
}
