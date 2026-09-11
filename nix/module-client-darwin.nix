# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

# nix-darwin client module: substituters/keys plus auto-push on macOS.
#
# Battery: the post-build-hook has zero idle cost (it runs only when nix builds
# something) and is the recommended primary mechanism. The watch daemon is a
# fallback for paths that arrive outside the local daemon's builds; it is OFF by
# default, uses parent-directory notifications, polls the DB only as a slow safety net,
# and runs as a launchd Background job so macOS throttles/coalesces its timers.
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.tsnixcache-client;

  types = import ./lib.nix { inherit lib; };

  pushHook = types.postBuildHook {
    inherit pkgs;
    inherit (cfg) package;
    inherit (cfg.postBuildHook) to timeout;
  };
in
{
  options.services.tsnixcache-client = {
    enable = lib.mkEnableOption "tsnixcache Nix binary cache client";

    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.tsnixcache;
      defaultText = lib.literalExpression "pkgs.tsnixcache";
      description = "tsnixcache package to use; the flake's overlays.default provides pkgs.tsnixcache.";
    };

    substituters = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ "http://tsnixcache" ];
      description = "Cache server URLs to add as Nix substituters.";
    };

    publicKey = lib.mkOption {
      type = lib.types.str;
      description = "Signing public key for the cache (name:base64 format).";
    };

    postBuildHook = {
      enable = lib.mkEnableOption "auto-push built paths via nix post-build-hook (exact, recommended, zero idle cost)";

      to = lib.mkOption {
        type = lib.types.str;
        default = "http://tsnixcache";
        description = "URL to push freshly built paths to.";
      };

      timeout = lib.mkOption {
        type = types.goDurationOrZero;
        default = "60s";
        description = ''
          Best-effort push deadline. Whole-number duration components or a bare
          day count; "0" disables the deadline. Overflow is rejected at evaluation.
        '';
      };
    };

    watch = {
      enable = lib.mkEnableOption "tsnixcache watch daemon (fallback auto-push; off by default to save battery)";

      to = lib.mkOption {
        type = lib.types.str;
        default = "http://tsnixcache";
        description = "URL to push new store paths to.";
      };

      db = lib.mkOption {
        type = lib.types.str;
        default = "/nix/var/nix/db/db.sqlite";
        description = "Path to the Nix SQLite database.";
      };

      storeDir = lib.mkOption {
        type = lib.types.str;
        default = "/nix/store";
        description = "Path to the Nix store.";
      };

      stateDir = lib.mkOption {
        type = lib.types.str;
        default = "/var/db/tsnixcache-watch";
        description = "Private persistent retry state directory; retain across upgrades and restarts.";
      };

      pollInterval = lib.mkOption {
        type = types.goDuration;
        default = "5m";
        description = ''
          Database polling safety net. Parent-directory notifications use kqueue
          on macOS; polling remains authoritative when notifications fail.
        '';
      };
    };
  };

  config = lib.mkIf cfg.enable {
    # Make the CLI available for manual `tsnixcache push` / `key` on the host.
    environment.systemPackages = [ cfg.package ];

    nix.settings.substituters = lib.mkAfter cfg.substituters;
    nix.settings.trusted-public-keys = [ cfg.publicKey ];

    # Push resolves closure metadata with nix path-info, which needs nix-command.
    nix.settings.post-build-hook = lib.mkIf cfg.postBuildHook.enable "${pushHook}";
    nix.settings.extra-experimental-features = lib.mkIf (cfg.postBuildHook.enable || cfg.watch.enable) [
      "nix-command"
    ];

    launchd.daemons.tsnixcache-watch = lib.mkIf cfg.watch.enable {
      serviceConfig = {
        ProgramArguments = [
          "${cfg.package}/bin/tsnixcache"
          "watch"
          "--db"
          cfg.watch.db
          "--store-dir"
          cfg.watch.storeDir
          "--to"
          cfg.watch.to
          "--state-dir"
          cfg.watch.stateDir
          "--poll-interval"
          cfg.watch.pollInterval
        ];
        ExitTimeOut = 90;
        RunAtLoad = true;
        KeepAlive = true;
        # Background: macOS may throttle and coalesce this job's timers/IO when
        # the machine is idle or on battery.
        ProcessType = "Background";
        LowPriorityIO = true;
        Nice = 10;
        EnvironmentVariables.PATH = lib.makeBinPath [
          pkgs.nix
          pkgs.coreutils
        ];
        StandardOutPath = "/var/log/tsnixcache-watch.log";
        StandardErrorPath = "/var/log/tsnixcache-watch.log";
      };
    };
  };
}
