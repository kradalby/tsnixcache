# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

{
  pkgs,
  tsnixcache,
  tsnixcacheModule,
  tsnixcacheClientModule,
}:
let
  common = import ./push-common.nix { inherit pkgs tsnixcache tsnixcacheModule; };
  paths = {
    key = "/etc/tsnixcache key'\"\\%d";
    spool = "/var/cache/tsnixcache space'\"\\%d/spool";
    roots = "/nix/var/nix/gcroots/tsnixcache space'\"\\%d";
    state = "/var/lib/tsnixcache-watch/state'\"\\%d";
    chroot = "/var/lib/tsnixcache chroot'\"\\%d";
  };
  node = chroot: {
    imports = [
      tsnixcacheModule
      tsnixcacheClientModule
    ];
    services.tsnixcache = {
      enable = true;
      package = tsnixcache;
      listen = [ "127.0.0.1:5000" ];
      localWrite = true;
      spoolDir = paths.spool;
      signKeyFile = paths.key;
      store = if chroot then paths.chroot else "";
      gcrootDir = if chroot then null else paths.roots;
      serveCompression = "zstd";
    };
    services.tsnixcache-client = {
      enable = true;
      package = tsnixcache;
      publicKey = "test:unused";
      substituters = [ ];
      watch = {
        enable = true;
        stateDir = paths.state;
        to = "http://127.0.0.1:5000";
        pollInterval = "1s";
      };
    };
    environment.etc.${pkgs.lib.removePrefix "/etc/" paths.key} = {
      text = common.secretKey;
      mode = "0400";
    };
    nix.settings.experimental-features = [ "nix-command" ];
    environment.systemPackages = [
      pkgs.nix
      pkgs.python3
      tsnixcache
    ];
  };
in
{
  name = "tsnixcache-module-paths";
  nodes = {
    system = node false;
    chroot = node true;
  };
  testScript = ''
    import json, shlex
    paths = json.loads(${builtins.toJSON (builtins.toJSON paths)})
    start_all()
    for machine, is_chroot in [(system, False), (chroot, True)]:
        machine.wait_for_unit("tsnixcache.service")
        machine.wait_for_unit("tsnixcache-watch.service")
        machine.wait_until_succeeds("curl -fsS http://127.0.0.1:5000/health | grep -F '\"status\":\"ok\"'", timeout=90)
        # tmpfiles must create writable mounts before systemd constructs its namespace.
        machine.succeed("systemd-tmpfiles --create")
        expected = [paths["spool"], paths["chroot"] if is_chroot else paths["roots"]]
        for path in expected:
            mode, owner, group = machine.succeed("stat -c '%a %U %G' " + shlex.quote(path)).strip().split()
            assert (mode, owner, group) == ("750", "tsnixcache", "tsnixcache"), (path, mode, owner, group)
        machine.succeed("test " + shlex.quote(machine.succeed("stat -Lc %a " + shlex.quote(paths["state"])).strip()) + " = 700")
        # Read actual argv; systemd must preserve quotes, backslashes and literal specifiers.
        pid = machine.succeed("systemctl show -p MainPID --value tsnixcache").strip()
        argv = machine.succeed(f"cat /proc/{pid}/cmdline").split("\x00")
        assert argv[argv.index("--spool-dir") + 1] == paths["spool"], argv
        credential = argv[argv.index("--sign-key-file") + 1]
        assert credential == "/run/credentials/tsnixcache.service/sign-key", argv
        machine.succeed("test " + shlex.quote(machine.succeed("stat -Lc '%a:%U' " + shlex.quote(paths["key"])).strip()) + " = 400:root")
        assert machine.succeed("readlink /run/tsnixcache-credentials/sign-key").rstrip("\n") == paths["key"]
        assert machine.succeed("stat -c '%a:%U' /run/tsnixcache-credentials").strip() == "700:root"
        machine.fail("runuser -u tsnixcache -- cat /run/tsnixcache-credentials/sign-key")
        flag, value = ("--store", paths["chroot"]) if is_chroot else ("--gcroot-dir", paths["roots"])
        assert argv[argv.index(flag) + 1] == value, argv
        machine.succeed("printf 'path fixture' >/tmp/path-fixture")
        store_path = machine.succeed("nix-store --add /tmp/path-fixture").strip()
        machine.succeed("tsnixcache push --to http://127.0.0.1:5000 " + shlex.quote(store_path))
        digest = store_path.rsplit("/", 1)[-1][:32]
        machine.succeed(f"curl -fsS http://127.0.0.1:5000/{digest}.narinfo")
        # A restart must reopen the same durable state through DynamicUser ownership.
        old_pid = machine.succeed("systemctl show -p MainPID --value tsnixcache-watch").strip()
        machine.succeed("systemctl restart tsnixcache-watch")
        machine.wait_for_unit("tsnixcache-watch.service")
        new_pid = machine.succeed("systemctl show -p MainPID --value tsnixcache-watch").strip()
        assert new_pid != old_pid
        machine.wait_until_succeeds(f"journalctl _PID={new_pid} | grep -F 'watch: starting'", timeout=60)
        machine.succeed("systemctl stop tsnixcache-watch")
        machine.succeed("test " + shlex.quote(machine.succeed("systemctl show -p Result --value tsnixcache-watch").strip()) + " = success")
        machine.succeed("find -H " + shlex.quote(paths["state"]) + " -name '*.sqlite' | grep .")
  '';
}
