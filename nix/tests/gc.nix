# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

# Exercise collection and import ownership under the module sandbox.
{
  pkgs,
  tsnixcache,
  tsnixcacheModule,
}:

let
  importGate = pkgs.writeShellScriptBin "nix-store" ''
    control=/var/cache/tsnixcache/spool/control
    case " $* " in
      *" --serve "*)
        if [ -p "$control/gate" ]; then
          touch "$control/entered"
          read -r decision <"$control/gate"
          [ "$decision" != fail ] || exit 7
        fi
        ;;
    esac
    exec ${pkgs.nix}/bin/nix-store "$@"
  '';
in

{
  name = "tsnixcache-gc";

  nodes = {
    server =
      {
        config,
        pkgs,
        lib,
        ...
      }:
      {
        imports = [ tsnixcacheModule ];
        services.tsnixcache = {
          enable = true;
          package = tsnixcache;
          listen = [ "127.0.0.1:5000" ];
          localWrite = true;
          gc = {
            # A VM's disk is always more than 1% full, so the rule fires on the
            # first tick instead of waiting for a full disk. olderThan keeps its
            # production shape: a value short enough to expire during the test
            # would race a live import and quietly defeat the whole
            # gcroot-before-import design, so nothing here may depend on a root
            # ageing out.
            rules = [
              {
                threshold = 1;
                olderThan = "20d";
              }
            ];
            interval = "5s";
          };
        };
        environment.systemPackages = [
          pkgs.nix
          pkgs.python3
        ];
        nix.settings.experimental-features = [ "nix-command" ];
        systemd.services.tsnixcache.serviceConfig.Environment = lib.mkForce [
          "PATH=${
            lib.makeBinPath [
              importGate
              pkgs.xz
              pkgs.zstd
              pkgs.coreutils
              pkgs.nix
            ]
          }"
        ];
      };
  };

  testScript = ''
    import json, shlex

    start_all()

    server.wait_for_unit("tsnixcache.service")
    server.wait_for_open_port(5000)

    # A collectable path: in the store, reachable from no GC root. Added
    # directly rather than built, because the VM has no nixpkgs channel.
    #
    # Fill enough of the writable overlay to cross the integer-percent GC threshold.
    server.succeed("dd if=/dev/urandom of=/tmp/victim bs=1M count=40 2>&1")
    garbage = server.succeed("nix-store --add /tmp/victim").strip()
    server.succeed(f"test -e {garbage}")

    # The same thing, but held by a gcroot of the shape an import leaves
    # behind: named after the hash part, created just now. It is younger than
    # olderThan, so the prune must leave it and the collection must spare the
    # path it points at — the guarantee that a just-pushed path is never
    # reaped.
    server.succeed("dd if=/dev/urandom of=/tmp/keeper bs=1M count=40 2>&1")
    keeper = server.succeed("nix-store --add /tmp/keeper").strip()
    root = "/nix/var/nix/gcroots/tsnixcache/" + keeper.split("/")[-1][:32]
    server.succeed(f"ln -s {keeper} {root}")

    # The watcher must be ticking at all before anything below means something.
    server.wait_until_succeeds(
      "curl -sf http://localhost:5000/metrics | grep -E '^tsnixcache_gc_checks_total [1-9]'",
      timeout=60,
    )

    # Wait for the watcher to complete a collection.
    server.wait_until_succeeds(
      "curl -sf http://localhost:5000/metrics | grep -E '^tsnixcache_gc_runs_total\\{[^}]*\\} [1-9]'",
      timeout=90,
    )

    # It must have actually collected: the unrooted path is gone. Note that
    # gc_runs_total is incremented when the rule is selected, not when the
    # collection finishes, so this is the point at which the run is complete.
    server.wait_until_succeeds(f"test ! -e {garbage}", timeout=60)

    # …and it must have spared the rooted one, root and all.
    server.succeed(f"test -L {root}")
    server.succeed(f"test -e {keeper}")

    metrics = server.succeed("curl -sf http://localhost:5000/metrics")
    print("\n".join(l for l in metrics.splitlines() if l.startswith("tsnixcache_gc_")))

    # The unit is hardened; a collection that cannot write to the store reports
    # an error here rather than failing loudly, so assert on it directly.
    #
    # Presence first: a CounterVec with no children exports no series at all, so
    # "no line is non-zero" would also hold for a server that never created
    # them, and this assertion would pass by absence.
    ops = {
      l.split("op=\"")[1].split("\"")[0]
      for l in metrics.splitlines() if l.startswith("tsnixcache_gc_errors_total{")
    }
    assert ops == {"statfs", "prune", "collect"}, \
      f"gc_errors_total must export a zero series per op, got {sorted(ops)}"

    errs = [
      l for l in metrics.splitlines()
      if l.startswith("tsnixcache_gc_errors_total{") and not l.endswith(" 0")
    ]
    assert not errs, "GC reported errors under the module's hardening:\n" + "\n".join(errs)

    # Actual collection must advance the freed-byte counter.
    freed = next(
      float(l.split()[1]) for l in metrics.splitlines()
      if l.startswith("tsnixcache_gc_freed_bytes_total ")
    )
    assert freed > 0, f"collection freed 40 MiB but gc_freed_bytes_total is {freed}"

    # The backoff. This rule is above the threshold permanently (a VM disk does
    # not empty itself), so without it nix-collect-garbage would run every 5s
    # forever, taking nix's global GC lock and walking the whole store each
    # time — every push and local build on the host queues behind that. Once a
    # collection prunes nothing and frees nothing, the next is held off for
    # max(interval, 1h), which outlasts this test.
    #
    # Wait for the runner to say it has settled rather than guessing at tick
    # counts: it takes two collections to get there, since the first freed
    # 40 MiB and only the second one comes back empty-handed.
    server.wait_until_succeeds(
      "journalctl -u tsnixcache.service | grep -F 'skipping nix-collect-garbage'",
      timeout=120,
    )

    def counter(name):
        line = next(
          l for l in server.succeed("curl -sf http://localhost:5000/metrics").splitlines()
          if l.startswith(name + " ")
        )
        return float(line.split()[1])

    collects, checks = counter("tsnixcache_gc_collect_duration_seconds_count"), counter("tsnixcache_gc_checks_total")

    # Four more ticks at a 5s interval.
    server.sleep(25)

    # The watcher must still be running, or "no new collections" means nothing.
    assert counter("tsnixcache_gc_checks_total") > checks, \
      "watcher stopped checking; the collection count below proves nothing"
    assert counter("tsnixcache_gc_collect_duration_seconds_count") == collects, \
      "nix-collect-garbage ran again inside the cooldown despite pruning and freeing nothing"

    server.succeed("systemctl is-active tsnixcache.service")
    # Standalone root GC initializes metadata that later service imports must use.
    server.succeed("test ! -e /nix/var/nix/gcroots/tsnixcache/.locks")
    server.succeed(f"touch -h -d '2 days ago' {root}")
    server.succeed("tsnixcache gc --gc-rule 1:1d")
    assert server.succeed("stat -c '%U:%G' /nix/var/nix/gcroots/tsnixcache/.locks").strip() == "tsnixcache:tsnixcache"

    # The real import subprocess pauses only after verification and root locking.
    control = "/var/cache/tsnixcache/spool/control"
    server.succeed("install -d -o tsnixcache -g tsnixcache -m 750 " + control)
    server.succeed("mkfifo -m 644 " + control + "/gate")

    def import_fixture(name):
        server.succeed(f"dd if=/dev/urandom of=/tmp/{name} bs=1M count=40 2>&1")
        path = server.succeed(f"nix-store --add /tmp/{name}").strip()
        digest = path.rsplit("/", 1)[-1][:32]
        server.succeed(f"nix-store --dump {path} >/tmp/{name}.nar")
        metadata = json.loads(server.succeed(f"nix path-info --json {path}"))
        info = metadata[path] if isinstance(metadata, dict) else metadata[0]
        text = (f"StorePath: {path}\nURL: nar/{name}.nar\nCompression: none\n"
                f"NarHash: {info['narHash']}\nNarSize: {info['narSize']}\n"
                f"FileHash: {info['narHash']}\nFileSize: {info['narSize']}\nReferences: \n")
        server.succeed("printf '%s' " + shlex.quote(text) + f" >/tmp/{name}.narinfo")
        return path, digest, f"/nix/var/nix/gcroots/tsnixcache/{digest}"

    def start_import(name, digest, unit):
        server.succeed("rm -f " + control + "/entered")
        server.succeed(f"curl -fsS -X PUT --data-binary @/tmp/{name}.nar http://localhost:5000/nar/{name}.nar")
        server.succeed(f"systemd-run --unit={unit} --property=RemainAfterExit=yes ${pkgs.curl}/bin/curl -fsS -X PUT "
                       f"--data-binary @/tmp/{name}.narinfo http://localhost:5000/{digest}.narinfo")
        server.wait_until_succeeds("test -f " + control + "/entered", timeout=60)

    def release_import(decision, unit, success):
        server.succeed("echo " + decision + " >" + control + "/gate")
        server.wait_until_succeeds(f"test $(systemctl show -p ExecMainExitTimestampMonotonic --value {unit}) -gt 0", timeout=60)
        result = server.succeed(f"systemctl show -p Result --value {unit}").strip()
        assert result == ("success" if success else "exit-code"), result

    path, digest, root = import_fixture("overlap")
    start_import("overlap", digest, "import-overlap")
    assert server.succeed(f"readlink {root}").strip() == path
    # A candidate selected as old must be rechecked under its ownership stripe.
    server.succeed(f"touch -h -d '2 days ago' {root}")
    inode = dict(line.split() for line in server.succeed("stat -c '%n %i' /nix/var/nix/gcroots/tsnixcache/.locks/*").splitlines())
    server.succeed("tsnixcache gc --gc-rule 1:1d")
    server.succeed(f"test -L {root}; test -e {path}")
    release_import("import", "import-overlap", True)
    server.succeed(f"nix-store --verify-path {path}")
    server.succeed(f"test $(date +%s) -lt $(( $(stat -c %Y {root}) + 60 ))")

    # A failed re-push may not remove a root owned by the earlier success.
    start_import("overlap", digest, "import-failed-repush")
    server.succeed(f"touch -h -d '2 days ago' {root}")
    server.succeed("tsnixcache gc --gc-rule 1:1d")
    server.succeed(f"test -L {root}; test -e {path}")
    release_import("fail", "import-failed-repush", False)
    server.succeed(f"test -L {root}; nix-store --verify-path {path}")
    after = dict(line.split() for line in server.succeed("stat -c '%n %i' /nix/var/nix/gcroots/tsnixcache/.locks/*").splitlines())
    assert all(after.get(path) == value for path, value in inode.items()), (inode, after)

    # Rollback removes a newly created root, making its unreferenced path collectible.
    failed_path, failed_digest, failed_root = import_fixture("failed-new")
    start_import("failed-new", failed_digest, "import-failed-new")
    release_import("fail", "import-failed-new", False)
    server.succeed(f"test ! -L {failed_root}")
    server.succeed(f"touch -h {root}")
    server.succeed("tsnixcache gc --gc-rule 1:1d")
    server.succeed(f"test ! -e {failed_path}; test -e {path}")
  '';
}
