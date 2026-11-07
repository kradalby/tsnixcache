# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

# A registered tsnet node must come back after a restart whatever its auth key
# file holds by then: the key only joins the node, as with tailscaled. headscale
# judges any key it is handed, and one that differs from the node's join key
# would retag the node, so a spent one keeps it logged out.
{
  pkgs,
  tsnixcache,
  tsnixcacheModule,
  headscaleTestkit,
}:

{
  name = "tsnixcache-tsnet-authkey";

  nodes = {
    headscale = {
      imports = [ headscaleTestkit ];
      environment.systemPackages = [ pkgs.jq ];
    };

    server =
      { lib, ... }:
      {
        imports = [ tsnixcacheModule ];

        environment.systemPackages = [ pkgs.curl ];

        services.tsnixcache = {
          enable = true;
          package = tsnixcache;
          listen = [ "127.0.0.1:5000" ];
          tsnet = [
            {
              controlUrl = "http://headscale";
              authKeyFile = "/var/lib/tsnixcache/tsnet-authkey";
              dir = "/var/lib/tsnixcache/tsnet";
            }
          ];
        };

        systemd.services.tsnixcache.wantedBy = lib.mkForce [ ];
        systemd.services.tsnixcache.environment.TS_NO_LOGS_NO_SUPPORT = "1";
      };
  };

  testScript = ''
    import json

    start_all()

    def key(*tags):
        out = headscale.succeed(f"hs-authkey cache --tags {','.join(tags)} -o json")
        k = json.loads(out)
        return k["id"], k["key"]

    def node():
        nodes = json.loads(headscale.succeed("headscale nodes list -o json"))
        return next((n for n in nodes if n["givenName"] == "tsnixcache"), None)

    def online(want=True):
        headscale.wait_until_succeeds(
            "headscale nodes list -o json | jq -e '.[] | select(.givenName == \"tsnixcache\")"
            f" | .online == {json.dumps(want)}'",
            timeout=120,
        )

    def start_with(k):
        server.succeed(f"echo -n '{k}' > /var/lib/tsnixcache/tsnet-authkey")
        server.systemctl("start tsnixcache")
        server.wait_for_unit("tsnixcache.service")

    # Waits out the old session so online() can only see the new one.
    def restart_with(k):
        server.systemctl("stop tsnixcache")
        online(False)
        start_with(k)
        online()

    headscale.wait_until_succeeds("headscale health")
    headscale.succeed("""
      cat > /tmp/policy.json << 'EOF'
    {
      "tagOwners": {"tag:server": ["cache@"], "tag:cache": ["cache@"], "tag:other": ["cache@"]},
      "acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}]
    }
    EOF
      headscale policy set -f /tmp/policy.json
    """)

    # As in production: join with one tagged key, then an admin retags the node.
    join_id, join_key = key("tag:server")
    server.succeed("mkdir -p /var/lib/tsnixcache")
    start_with(join_key)
    online()
    node_id = node()["id"]
    headscale.succeed(f"headscale nodes tag -i {node_id} -t tag:cache")

    with subtest("restart with its own join key, now expired"):
        headscale.succeed(f"headscale preauthkeys expire --id {join_id}")
        restart_with(join_key)

    with subtest("restart with a different tagged key, expired"):
        other_id, other_key = key("tag:other")
        headscale.succeed(f"headscale preauthkeys expire --id {other_id}")
        restart_with(other_key)
        assert node()["tags"] == ["tag:cache"], node()

    with subtest("restart with a different tagged key, valid"):
        _, valid_key = key("tag:other")
        restart_with(valid_key)
        assert node()["tags"] == ["tag:cache"], node()

    # Control that no longer knows the node asks it to log in, and only then
    # does the key go out.
    with subtest("deleted node offers its spent key"):
        server.systemctl("stop tsnixcache")
        online(False)
        headscale.succeed(f"headscale nodes delete -i {node_id} --force")
        start_with(other_key)
        server.wait_until_succeeds(
            "journalctl -u tsnixcache --no-pager | grep -F 'authkey expired'", timeout=120
        )

    with subtest("deleted node logs in again with a valid key"):
        _, fresh_key = key("tag:cache")
        server.systemctl("stop tsnixcache")
        start_with(fresh_key)
        online()
  '';
}
