# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

# Signature verification end to end: nix must ACCEPT what tsnixcache signs.
#
# Every other VM test sets require-sigs = false, so none of them would notice
# tsnixcache serving a narinfo nix refuses — or, worse, one nix accepts while
# the reference set it carries is wrong. This test turns signature checking on,
# trusts only the tsnixcache key, and substitutes both a pushed path and a path
# served straight from the server's own store.
#
# The pushed path deliberately references itself: that is the shape that broke
# signing before, and it is the shape of nearly every real package.
{
  pkgs,
  tsnixcache,
  tsnixcacheModule,
}:

let
  common = import ./push-common.nix { inherit pkgs tsnixcache tsnixcacheModule; };
in
{
  name = "tsnixcache-sigs";

  nodes = {
    server =
      {
        config,
        pkgs,
        lib,
        ...
      }:
      {
        imports = [ common.serverNode ];
        # hello is in the server's store, so it can be served without a push.
        environment.systemPackages = [ pkgs.hello ];
      };

    pusher = { config, pkgs, ... }: {
      environment.systemPackages = [ pkgs.nix ];
      system.extraDependencies = common.buildInputs;
      nix.settings = {
        # Nested VM cannot set up the build sandbox; build against the real
        # store (inputs pinned via extraDependencies above).
        sandbox = false;
        trusted-users = [ "root" ];
        # nix copy uses the nix-command experimental feature.
        experimental-features = [ "nix-command" ];
      };
    };

    consumer =
      {
        config,
        pkgs,
        lib,
        ...
      }:
      {
        nix.settings = {
          substituters = lib.mkForce [ "http://server:5000" ];
          # Only tsnixcache is trusted, and signatures are mandatory.
          trusted-public-keys = lib.mkForce [ common.publicKey ];
          require-sigs = true;
        };
      };
  };

  testScript = ''
    start_all()

    server.wait_for_unit("tsnixcache.service")
    server.wait_for_open_port(5000)
    # Wait for both client nodes: succeed()/fail() only wait for the machine to
    # boot, not for its resolver to be up, so without this the consumer's first
    # command below can fail on resolving "server" and prove nothing.
    pusher.wait_for_unit("multi-user.target")
    consumer.wait_for_unit("multi-user.target")

    # ── A self-referencing path, built on the pusher ──────────────────────────

    p = pusher.succeed("nix-build --no-out-link ${common.selfRefExpr}").strip()
    refs = pusher.succeed(f"nix-store --query --references {p}").split()
    assert p in refs, f"fixture is not self-referencing: {refs}"

    pusher.succeed(f"nix copy --to http://server:5000 {p} 2>&1")

    hashpart = p.split("/")[-1][:32]
    info = server.wait_until_succeeds(
      f"curl -sf http://localhost:5000/{hashpart}.narinfo", timeout=30
    )
    print(info)

    # Signed by the cache, and the self-reference is part of what is served.
    assert "Sig: ${common.keyName}:" in info, (
      f"narinfo carries no tsnixcache signature:\n{info}"
    )
    refline = next(l for l in info.splitlines() if l.startswith("References:"))
    assert p.split("/")[-1] in refline.split(), (
      f"served narinfo dropped the path's self-reference:\n{refline}"
    )

    # ── Signature checking is really on ───────────────────────────────────────

    # Control: the same fetch with no trusted key must be refused. Paired with
    # the successful fetches below — same path, same server, only the trusted
    # key differs — this proves the successes are not signature checking being
    # quietly off.
    # The empty shell argument below is written as three quotes: in a Nix
    # indented string that is the escape for a literal pair, which on its own
    # would close the string.
    refused = consumer.fail(
      "nix-store --realise ${pkgs.hello} --substituters http://server:5000"
      " --option trusted-public-keys ''' 2>&1"
    )
    print(refused)

    # A non-zero exit on its own proves nothing: an unreachable server or a name
    # that does not resolve yet would fail here too, and the control would pass
    # having tested nothing. Require nix to say it was the signature it refused.
    assert "signed" in refused or "signature" in refused, (
      f"control fetch failed, but not over signatures:\n{refused}"
    )

    # ── nix accepts what tsnixcache signs ─────────────────────────────────────

    # Served from the server's own store.
    consumer.succeed(
      "nix-store --realise ${pkgs.hello} --substituters http://server:5000 2>&1"
    )

    # Pushed, self-referencing.
    consumer.succeed(f"nix-store --realise {p} --substituters http://server:5000 2>&1")

    # A substituted path must keep the reference set it was pushed with —
    # including the self-reference.
    got = sorted(consumer.succeed(f"nix-store --query --references {p}").split())
    assert got == sorted(refs), f"reference set changed in transit: {got} != {sorted(refs)}"
  '';
}
