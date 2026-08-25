# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

# GC under the module's own systemd hardening.
#
# The unit can write neither /nix/store nor /nix/var/nix, and does not need
# to: it runs unprivileged, so nix hands the collection to nix-daemon and the
# daemon does the deleting. The service writes only its own gcroot directory,
# to prune the roots that hold recently imported paths. Nothing else in the
# suite sets gc.rules, so both halves of that arrangement — a collection that
# really happens, and a fresh gcroot that really protects a path from it — are
# invisible outside this test.
{
  pkgs,
  tsnixcache,
  tsnixcacheModule,
}:

{
  name = "tsnixcache-gc";

  nodes = {
    server = { config, pkgs, ... }: {
      imports = [ tsnixcacheModule ];
      services.tsnixcache = {
        enable = true;
        package = tsnixcache;
        listen = [ "127.0.0.1:5000" ];
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
      environment.systemPackages = [ pkgs.nix ];
    };
  };

  testScript = ''
    start_all()

    server.wait_for_unit("tsnixcache.service")
    server.wait_for_open_port(5000)

    # A collectable path: in the store, reachable from no GC root. Added
    # directly rather than built, because the VM has no nixpkgs channel.
    #
    # It is deliberately large. The test VM's /nix/store is a fresh ~483 MiB
    # overlay, so used space rounds to 0% and no rule with a threshold of 1 or
    # more would ever fire; 40 MiB puts it around 8%.
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

    # freed_bytes is a statfs delta taken around nix-collect-garbage. Nothing
    # else exercises it, and a broken one reads 0 forever without failing
    # anything — the shape of bug this suite exists to catch. 40 MiB went away,
    # so it must be non-zero.
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
  '';
}
