# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

# nix-darwin client module: substituters/keys plus auto-push on macOS.
#
# Battery: the post-build-hook has zero idle cost (it runs only when nix builds
# something) and is the recommended primary mechanism. The watch daemon is a
# fallback for paths that arrive outside the local daemon's builds; it is OFF by
# default, uses FSEvents (no busy loop), polls the DB only as a slow safety net,
# and runs as a launchd Background job so macOS throttles/coalesces its timers.
{ config, lib, pkgs, ... }:

let
  cfg = config.services.tsnixcache-client;

  types = import ./lib.nix { inherit lib; };

  # A non-zero exit here fails the build, so nothing in this script may abort:
  # under `set -u` an unset PATH would, hence ${PATH:-}.
  pushHook = pkgs.writeShellScript "tsnixcache-post-build-hook" ''
    set -u
    [ -n "''${OUT_PATHS:-}" ] || exit 0
    export PATH=${lib.makeBinPath [ pkgs.nix pkgs.coreutils ]}:''${PATH:-}
    if ! ${cfg.package}/bin/tsnixcache push --to ${cfg.postBuildHook.to} --timeout ${cfg.postBuildHook.timeout} $OUT_PATHS; then
      echo "tsnixcache: push to ${cfg.postBuildHook.to} failed after retries; not failing build (watch will retry)" >&2
    fi
    exit 0
  '';
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
        type = types.goDuration;
        default = "60s";
        description = ''
          Overall deadline for a best-effort push before the hook gives up (Go
          duration). A value push cannot parse makes it exit 2, which the hook's
          best-effort wrapper swallows — nothing would ever be pushed again — so
          the type only accepts what Go's flag.Duration does.
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

      pollInterval = lib.mkOption {
        type = types.goDuration;
        default = "5m";
        description = ''
          How often the watcher polls the DB as a safety net. FSEvents catches
          new paths promptly, so a long interval costs little and saves battery.
          A value watch cannot parse makes it exit 2 and launchd respawn it every
          few seconds, so the type only accepts what Go's flag.Duration does.
        '';
      };
    };
  };

  config = lib.mkIf cfg.enable {
    # Make the CLI available for manual `tsnixcache push` / `key` on the host.
    environment.systemPackages = [ cfg.package ];

    nix.settings.substituters = lib.mkAfter cfg.substituters;
    nix.settings.trusted-public-keys = [ cfg.publicKey ];

    # Both push paths shell out to `nix copy`, which needs the nix-command feature.
    nix.settings.post-build-hook = lib.mkIf cfg.postBuildHook.enable "${pushHook}";
    nix.settings.extra-experimental-features =
      lib.mkIf (cfg.postBuildHook.enable || cfg.watch.enable) [ "nix-command" ];

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
          "--poll-interval"
          cfg.watch.pollInterval
        ];
        RunAtLoad = true;
        KeepAlive = true;
        # Background: macOS may throttle and coalesce this job's timers/IO when
        # the machine is idle or on battery.
        ProcessType = "Background";
        LowPriorityIO = true;
        Nice = 10;
        EnvironmentVariables.PATH = lib.makeBinPath [ pkgs.nix pkgs.coreutils ];
        StandardOutPath = "/var/log/tsnixcache-watch.log";
        StandardErrorPath = "/var/log/tsnixcache-watch.log";
      };
    };
  };
}
