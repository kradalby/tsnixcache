# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

# Auto-push via the watch daemon only (fallback for non-built paths).
{
  pkgs,
  tsnixcache,
  tsnixcacheModule,
  tsnixcacheClientModule,
}:

let
  common = import ./push-common.nix { inherit pkgs tsnixcache tsnixcacheModule; };
  metadataGate = pkgs.writeShellScriptBin "nix" ''
    control=/run/watch-final-gate
    case " $* " in
      *" path-info "*)
        if [ -e "$control/armed" ]; then
          rm "$control/armed"
          touch "$control/entered"
          read -r decision <"$control/gate"
        fi
        ;;
    esac
    exec ${pkgs.nix}/bin/nix "$@"
  '';
in
{
  name = "tsnixcache-push-watch";

  nodes = {
    server = common.serverNode;

    client = { config, pkgs, ... }: {
      imports = [ tsnixcacheClientModule ];
      environment.systemPackages = [
        pkgs.nix
        pkgs.sqlite
        pkgs.python3
      ];
      nix.settings = {
        trusted-users = [ "root" ];
        experimental-features = [ "nix-command" ];
      };
      services.tsnixcache-client = {
        enable = true;
        package = tsnixcache;
        publicKey = common.publicKey;
        substituters = [ ];
        watch = {
          enable = true;
          to = "http://server:5000";
          pollInterval = "1s";
        };
      };
    };
  };

  testScript = ''
    import json, shlex

    start_all()

    server.wait_for_unit("tsnixcache.service")
    server.wait_for_open_port(5000)
    client.wait_for_unit("tsnixcache-watch.service")

    # wait_for_unit only means systemd forked the process, not that the watcher
    # has read its baseline cursor (the highest ValidPaths id at startup). It
    # pushes what is registered *after* that read, so a path added in the gap
    # lands at or below the baseline and is correctly ignored — the test then
    # waits for a push that is never coming. Wait for the watcher to say it has
    # taken the baseline before adding anything.
    client.wait_until_succeeds(
      "journalctl -u tsnixcache-watch.service | grep -F 'watch: starting'",
      timeout=60,
    )

    # nix-store --add does not trigger a build, so only the watcher catches it.
    p = client.succeed("echo tsnixcache-watch-test > /tmp/wf && nix-store --add /tmp/wf").strip()
    hashpart = p.split("/")[-1][:32]

    # The watcher detects the new path and pushes it asynchronously.
    server.wait_until_succeeds(f"curl -sf http://localhost:5000/{hashpart}.narinfo", timeout=60)
    # Persist more registrations than the discovery batch while the cache is offline.
    server.succeed("systemctl stop tsnixcache")
    client.succeed("mkdir /tmp/offline; for n in $(seq 1 520); do echo offline-$n > /tmp/offline/$n; done")
    paths = client.succeed("nix-store --add /tmp/offline/*").splitlines()
    assert len(paths) == 520
    state = client.succeed("find -H /var/lib/tsnixcache-watch -name '*.sqlite'").strip()
    assert len(state.splitlines()) == 1, state

    def pending(count, timeout=180):
        client.wait_until_succeeds(
            "test $(sqlite3 -readonly " + shlex.quote(state) +
            " 'select count(*) from pending where expired = 0') = " + str(count), timeout=timeout)

    pending(520)
    before = client.succeed("sqlite3 -readonly " + shlex.quote(state) + " 'select cursor from checkpoint'").strip()
    client.wait_until_succeeds("test $(sqlite3 -readonly " + shlex.quote(state) + " 'select count(*) from pending where attempts > 0') -ge 16")
    failed = json.loads(client.succeed("sqlite3 -json -readonly " + shlex.quote(state) + " 'select path,attempts,first_fail,next_at from pending where attempts > 0 order by path limit 16'"))
    old_pid = client.succeed("systemctl show -p MainPID --value tsnixcache-watch").strip()
    # Kill the process to bypass shutdown flushing; WAL state must already be durable.
    client.succeed("systemctl kill --signal=SIGKILL tsnixcache-watch")
    client.wait_until_succeeds("test $(systemctl show -p MainPID --value tsnixcache-watch) != " + old_pid)
    client.wait_for_unit("tsnixcache-watch.service")
    new_pid = client.succeed("systemctl show -p MainPID --value tsnixcache-watch").strip()
    client.wait_until_succeeds(f"journalctl _PID={new_pid} | grep -F 'watch: starting'")
    pending(520)
    assert client.succeed("sqlite3 -readonly " + shlex.quote(state) + " 'select cursor from checkpoint'").strip() == before

    after = {row["path"]: row for row in json.loads(client.succeed("sqlite3 -json -readonly " + shlex.quote(state) + " 'select path,attempts,first_fail,next_at from pending'"))}
    for row in failed:
        resumed = after[row["path"]]
        assert resumed["first_fail"] == row["first_fail"] > 0, (row, resumed)
        assert resumed["attempts"] >= row["attempts"] and resumed["next_at"] >= row["next_at"], (row, resumed)

    server.succeed("systemctl start tsnixcache")
    server.wait_for_open_port(5000)
    pending(0, timeout=600)
    for path in paths:
        digest = path.rsplit("/", 1)[-1][:32]
        server.succeed(f"curl -fsS http://localhost:5000/{digest}.narinfo > /dev/null")
    client.succeed("systemctl stop tsnixcache-watch")
    assert client.succeed("systemctl show -p Result --value tsnixcache-watch").strip() == "success"

    # Compose the actual readiness/stop commands with the same durable source.
    def session(name, gated=False):
        directory = "/run/" + name
        client.succeed("install -d -m 700 " + directory)
        pidfile = directory + "/watch.pid"
        client.succeed(
            "systemd-run --unit=" + name + " --property=Type=exec --setenv=PATH=" + ("${metadataGate}/bin:" if gated else "") + "${
              pkgs.lib.makeBinPath [
                pkgs.nix
                pkgs.xz
                pkgs.zstd
                pkgs.coreutils
              ]
            } " +
            "${tsnixcache}/bin/tsnixcache watch --to http://server:5000 " +
            "--state-dir /var/lib/tsnixcache-watch --poll-interval 1s " +
            "--retry-queue-size 2 --attempts 1 --retry-backoff-base 1s --retry-backoff-max 2s --pid-file " + pidfile)
        token = client.succeed("tsnixcache wait-for --ready --timeout 60s --pid-file " + pidfile).strip()
        return "tsnixcache wait-for --timeout 60s --pid-file " + pidfile + " --session " + token, pidfile

    server.succeed("systemctl stop tsnixcache")
    wait, pidfile = session("watch-failed-drain")
    client.succeed("mkdir /tmp/late; for n in $(seq 1 10); do echo late-$n >/tmp/late/$n; done")
    late = client.succeed("nix-store --add /tmp/late/*").splitlines()
    assert len(late) == 10
    pending(10)
    client.fail(wait + " --stop")
    client.fail(wait)
    client.succeed("journalctl -u watch-failed-drain | grep -F 'connection refused'")
    status = json.loads(client.succeed("cat " + pidfile + ".status.json"))
    assert status["state"] == "failed", status
    assert "watch: incomplete drain" in status["error"], status
    assert status["progress"]["pending"] == 10 and status["progress"]["state_file"] == state, status
    pending(10)

    server.succeed("systemctl start tsnixcache")
    server.wait_for_open_port(5000)
    wait, pidfile = session("watch-resumed-drain")
    # Restart preserves consumed final allowances; ordinary retries honor backoff.
    pending(0)
    client.succeed(wait + " --stop")
    client.succeed(wait)
    status = json.loads(client.succeed("cat " + pidfile + ".status.json"))
    assert status["state"] == "completed", status
    pending(0)
    for path in late:
        digest = path.rsplit("/", 1)[-1][:32]
        server.succeed(f"curl -fsS http://localhost:5000/{digest}.narinfo")

    # Hold the ordinary metadata call after one discovery page. Stop must cancel it
    # and discover every remaining page through the captured boundary.
    client.succeed("install -d -m 700 /run/watch-final-gate; mkfifo /run/watch-final-gate/gate; touch /run/watch-final-gate/armed")
    wait, pidfile = session("watch-multipage-drain", gated=True)
    client.succeed("mkdir /tmp/final; for n in $(seq 1 10); do echo final-$n >/tmp/final/$n; done")
    final_paths = client.succeed("nix-store --add /tmp/final/*").splitlines()
    assert len(final_paths) == 10
    client.wait_until_succeeds("test -f /run/watch-final-gate/entered")
    pending(2)
    cursor = int(client.succeed("sqlite3 -readonly " + shlex.quote(state) + " 'select cursor from checkpoint'").strip())
    maximum = int(client.succeed("sqlite3 -readonly /nix/var/nix/db/db.sqlite 'select max(id) from ValidPaths'").strip())
    assert cursor < maximum, (cursor, maximum)
    client.succeed(wait + " --stop")
    client.succeed(wait)
    status = json.loads(client.succeed("cat " + pidfile + ".status.json"))
    assert status["state"] == "completed" and status["progress"]["pending"] == 0, status
    pending(0)
    for path in final_paths:
        digest = path.rsplit("/", 1)[-1][:32]
        server.succeed(f"curl -fsS http://localhost:5000/{digest}.narinfo > /dev/null")
  '';
}
