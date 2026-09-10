# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

{
  pkgs,
  fc,
  common,
}:
let
  inherit (pkgs) lib;
  types = import ./lib.nix { inherit lib; };
  corpus = builtins.fromJSON (builtins.readFile ../cli/testdata/durations.json);
  server = import ./module-server.nix;
  linux = import ./module-client.nix;
  darwin = import ./module-client-darwin.nix;
  options = [
    {
      module = server;
      path = [
        "services"
        "tsnixcache"
        "gc"
        "interval"
      ];
      command = "serve";
      flag = "gc-interval";
    }
    {
      module = server;
      path = [
        "services"
        "tsnixcache"
        "gc"
        "rules"
      ];
      command = "serve";
      flag = "gc-rule";
      rule = true;
    }
    {
      module = linux;
      path = [
        "services"
        "tsnixcache-client"
        "postBuildHook"
        "timeout"
      ];
      command = "push";
      flag = "timeout";
    }
    {
      module = darwin;
      path = [
        "services"
        "tsnixcache-client"
        "postBuildHook"
        "timeout"
      ];
      command = "push";
      flag = "timeout";
    }
    {
      module = linux;
      path = [
        "services"
        "tsnixcache-client"
        "watch"
        "pollInterval"
      ];
      command = "watch";
      flag = "poll-interval";
    }
    {
      module = darwin;
      path = [
        "services"
        "tsnixcache-client"
        "watch"
        "pollInterval"
      ];
      command = "watch";
      flag = "poll-interval";
    }
  ];
  cases = lib.concatMap (
    option:
    map (
      value:
      let
        evaluated = lib.evalModules {
          specialArgs = {
            inherit pkgs;
            utils = { };
          };
          modules = [
            option.module
            { _module.check = false; }
            (lib.setAttrByPath option.path (
              if option.rule or false then
                [
                  {
                    threshold = 80;
                    olderThan = value;
                  }
                ]
              else
                value
            ))
          ];
        };
        accepted =
          (builtins.tryEval (builtins.deepSeq (lib.getAttrFromPath option.path evaluated.config) true))
          .success;
      in
      {
        inherit (option) command flag;
        inherit value accepted;
        option = lib.concatStringsSep "." option.path;
        nanos = if value == "0" then 0 else types.parseDuration value;
      }
    ) corpus
  ) options;
  result = pkgs.writeText "tsnixcache-module-durations.json" (builtins.toJSON cases);
in
fc.goTest (
  common
  // {
    name = "tsnixcache-duration-contract";
    testPackages = "./cli";
    testFlags = [
      "-run"
      "^TestModuleDurationContract$"
    ];
    testEnv = "export TSNIXCACHE_DURATION_CASES=${result}";
  }
)
