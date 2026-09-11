# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

{
  pkgs,
  tsnixcache,
  tsnixcacheModule,
}:
let
  common = import ./push-common.nix { inherit pkgs tsnixcache tsnixcacheModule; };
  blocks = pkgs.lib.splitString "```bash\n" (builtins.readFile ../../docs/github-actions.md);
  block = builtins.head (builtins.filter (pkgs.lib.hasInfix "session=$(mktemp") blocks);
  recipe =
    assert pkgs.lib.hasInfix "--drain-timeout 0" block;
    assert pkgs.lib.hasInfix "--timeout 0" block;
    pkgs.writeShellScript "readme-ci" (builtins.head (pkgs.lib.splitString "```" block));
  resolvePackage = pkgs.writeShellScriptBin "nix" ''
    if [ "$#" -eq 4 ] && [ "$1" = build ] && [ "$2" = --no-link ] &&
       [ "$3" = --print-out-paths ] && [ "$4" = 'github:kradalby/tsnixcache#default' ]; then
      printf '%s\n' '${tsnixcache}'
    else
      exec ${pkgs.nix}/bin/nix "$@"
    fi
  '';
in
{
  name = "tsnixcache-github-actions-lifecycle";
  nodes.server = common.serverNode;
  nodes.client = { ... }: {
    environment.systemPackages = [
      pkgs.nix
      pkgs.jq
      pkgs.sqlite
      tsnixcache
      common.builder
    ];
    nix.settings = {
      trusted-users = [ "root" ];
      substituters = [ ];
      experimental-features = [
        "nix-command"
        "flakes"
      ];
    };
  };
  testScript = ''
    import json, shlex

    start_all()
    server.wait_for_unit("tsnixcache.service")
    server.wait_for_open_port(5000)
    client.succeed("nix-store --add /etc/hostname")

    def scenario(name, build_ok, cache_ok):
        server.succeed("systemctl " + ("start" if cache_ok else "stop") + " tsnixcache")
        if cache_ok:
            server.wait_for_open_port(5000)
        base = "/root/recipe-" + name
        client.succeed("mkdir -p " + base + "/session " + base + "/work")
        client.succeed("cp ${common.builder}/bin/busybox " + base + "/work/busybox")
        expression = r"""
        {
          outputs = { self }: let
            dependency = derivation {
              name = "recipe-dependency-NAME";
              system = "${pkgs.stdenv.hostPlatform.system}";
              builder = ./busybox;
              args = [ "sh" "-c" "echo NAME > $out" ];
            };
          in { packages.${pkgs.stdenv.hostPlatform.system}.default = derivation {
            name = "recipe-build-NAME";
            system = "${pkgs.stdenv.hostPlatform.system}";
            builder = ./busybox;
            dep = dependency;
            args = [ "sh" "-c" "cat $dep > $out; exit BUILD_STATUS" ];
          }; };
        }
        """.replace("NAME", name).replace("BUILD_STATUS", "0" if build_ok else "17")
        client.succeed("printf %s " + shlex.quote(expression) + " > " + base + "/work/flake.nix")
        command = "cd " + base + "/work && env PATH=${resolvePackage}/bin:$PATH RUNNER_TEMP=" + base + "/session TSNIXCACHE_STATE_DIR=" + base + "/state TSNIXCACHE_URL=http://server:5000 ${pkgs.bash}/bin/bash ${recipe} > " + base + "/recipe.log 2>&1"
        code, output = client.execute(command, timeout=120)
        log = client.succeed("cat " + base + "/recipe.log")
        assert (code == 0) == (build_ok and cache_ok), (name, code, output, log)
        status_path = client.succeed("find " + base + "/session -name '*.status.json'").strip()
        status = json.loads(client.succeed("cat " + status_path))
        assert status["state"] == ("completed" if cache_ok else "failed"), (name, status, log)
        client.wait_until_fails("test -e /proc/" + str(status["pid"]), timeout=10)
        dependency = client.succeed("cd " + base + "/work && nix eval --raw .#packages.${pkgs.stdenv.hostPlatform.system}.default.dep").strip()
        if cache_ok:
            server.succeed("curl -fsS http://localhost:5000/" + dependency.split("/")[-1][:32] + ".narinfo")
        else:
            state = client.succeed("find " + base + "/state -name '*.sqlite'").strip()
            client.succeed("test $(sqlite3 -readonly " + state + " 'select count(*) from pending where expired = 0') -gt 0")
            client.fail("${tsnixcache}/bin/tsnixcache wait-for --pid-file " + status_path.removesuffix(".status.json") + " --timeout 5s")

    scenario("success", True, True)
    scenario("build-failure", False, True)
    scenario("drain-failure", True, False)
    scenario("both-fail", False, False)
  '';
}
