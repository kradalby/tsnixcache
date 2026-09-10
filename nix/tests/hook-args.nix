# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

{ pkgs }:
let
  inherit (pkgs) lib;
  recorder = pkgs.writeScriptBin "tsnixcache" ''
    #!${pkgs.python3}/bin/python3
    import json, os, sys
    with open(os.environ["TSNIXCACHE_HOOK_ARGS"], "w") as output:
        json.dump(sys.argv[1:], output)
    sys.exit(int(os.environ.get("TSNIXCACHE_HOOK_EXIT", "0")))
  '';
  urls = [
    "http://cache/path?a=1&b=2"
    "http://cache/;touch INJECTED;$(touch INJECTED)`touch INJECTED`(x)'\"%d\\tail"
  ];
  hook =
    module: to: timeout:
    (lib.evalModules {
      specialArgs = {
        inherit pkgs;
        utils = { };
      };
      modules = [
        module
        {
          options =
            lib.genAttrs [ "environment" "systemd" "launchd" ] (
              _:
              lib.mkOption {
                type = lib.types.attrs;
                default = { };
              }
            )
            // {
              nix = lib.mkOption {
                type = lib.types.attrsOf (lib.types.attrsOf lib.types.anything);
                default = { };
              };
              assertions = lib.mkOption {
                type = lib.types.listOf lib.types.attrs;
                default = [ ];
              };
            };
          config.services.tsnixcache-client = {
            enable = true;
            package = recorder;
            publicKey = "test:unused";
            postBuildHook = {
              enable = true;
              inherit to timeout;
            };
          };
        }
      ];
    }).config.nix.settings.post-build-hook;
  cases =
    lib.concatMap
      (
        module:
        lib.concatMap (
          to:
          map
            (timeout: {
              inherit to timeout;
              script = hook module to timeout;
            })
            [
              "1d"
              "1ns"
              "0"
            ]
        ) urls
      )
      [
        ../module-client.nix
        ../module-client-darwin.nix
      ];
  manifest = pkgs.writeText "tsnixcache-hook-cases.json" (builtins.toJSON cases);
in
pkgs.runCommand "tsnixcache-hook-arguments" { nativeBuildInputs = [ pkgs.python3 ]; } ''
  python3 - ${manifest} <<'PY'
  import json, os, pathlib, subprocess, sys
  record = pathlib.Path.cwd() / "args.json"
  for case in json.load(open(sys.argv[1])):
      for output_paths, expected in [
          ("", []),
          ("/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-one", ["/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-one"]),
          (" /nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-one\n/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-two ",
           ["/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-one", "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-two"]),
          ("*", ["*"]),
      ]:
          for exit_code in [0, 1, 124]:
              record.unlink(missing_ok=True)
              env = {"OUT_PATHS": output_paths, "TSNIXCACHE_HOOK_ARGS": str(record), "TSNIXCACHE_HOOK_EXIT": str(exit_code)}
              result = subprocess.run([case["script"]], env=env, text=True, capture_output=True, timeout=10)
              assert result.returncode == 0, result
              assert not pathlib.Path("INJECTED").exists()
              if not expected:
                  assert not record.exists()
                  assert result.stderr == ""
                  continue
              assert json.loads(record.read_text()) == ["push", "--to", case["to"], "--timeout", case["timeout"], *expected]
              warning = "tsnixcache: push to " + case["to"] + " failed after retries; not failing build\n"
              assert result.stderr == (warning if exit_code else ""), result.stderr
  PY
  touch $out
''
