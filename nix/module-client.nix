# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

{
  config,
  lib,
  pkgs,
  utils,
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
      enable = lib.mkEnableOption "auto-push built paths via nix post-build-hook (exact, recommended)";

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
      enable = lib.mkEnableOption "tsnixcache watch daemon (fallback auto-push for non-built paths)";

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
        default = "/var/lib/tsnixcache-watch";
        description = "Private persistent retry state directory within /var/lib/tsnixcache-watch, managed by StateDirectory; retain across upgrades and restarts.";
      };

      pollInterval = lib.mkOption {
        type = types.goDuration;
        default = "30s";
        description = "Database polling safety net alongside directory notifications.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = lib.optional cfg.watch.enable {
      assertion =
        (
          cfg.watch.stateDir == "/var/lib/tsnixcache-watch"
          || lib.hasPrefix "/var/lib/tsnixcache-watch/" cfg.watch.stateDir
        )
        && lib.all (
          part:
          !(builtins.elem part [
            ""
            "."
            ".."
          ])
        ) (lib.splitString "/" (lib.removePrefix "/" cfg.watch.stateDir));
      message = "services.tsnixcache-client.watch.stateDir must be a normalized path within /var/lib/tsnixcache-watch.";
    };

    # Make the CLI available for manual `tsnixcache push` / `key` on the host.
    environment.systemPackages = [ cfg.package ];

    nix.settings.substituters = lib.mkAfter cfg.substituters;
    nix.settings.trusted-public-keys = [ cfg.publicKey ];

    # Push resolves closure metadata with nix path-info, which needs nix-command.
    nix.settings.post-build-hook = lib.mkIf cfg.postBuildHook.enable "${pushHook}";
    nix.settings.extra-experimental-features = lib.mkIf (cfg.postBuildHook.enable || cfg.watch.enable) [
      "nix-command"
    ];

    systemd.services.tsnixcache-watch = lib.mkIf cfg.watch.enable {
      description = "tsnixcache watch — auto-push new Nix store paths";
      wantedBy = [ "multi-user.target" ];
      after = [
        "network.target"
        "nix-daemon.service"
      ];

      serviceConfig = {
        ExecStart = utils.escapeSystemdExecArgs [
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
        # The watcher only reads the store and uploads over HTTP, so it needs
        # no privilege at all. Running unprivileged also makes nix route
        # through the daemon by identity rather than by accident: as root it
        # would open the store directly if the sandbox happened to leave
        # /nix/var/nix writable.
        DynamicUser = true;
        # tsnixcache watch shells out to `nix`, which needs nix on PATH and a
        # writable cache dir (ProtectHome makes the default ~/.cache read-only).
        CacheDirectory = "tsnixcache-watch";
        # Keep the managed parent simple; child paths pass through the CLI unchanged.
        StateDirectory = "tsnixcache-watch";
        StateDirectoryMode = "0700";
        Environment = [
          "PATH=${
            lib.makeBinPath [
              pkgs.nix
              pkgs.coreutils
            ]
          }"
          "XDG_CACHE_HOME=/var/cache/tsnixcache-watch"
        ];
        KillMode = "mixed";
        TimeoutStopSec = "90s";
        Restart = "on-failure";
        RestartSec = "10s";
        PrivateTmp = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        NoNewPrivileges = true;
        CapabilityBoundingSet = "";
        AmbientCapabilities = "";
        LockPersonality = true;
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
          "AF_NETLINK"
        ];
        SystemCallFilter = [ "@system-service" ];
        ReadOnlyPaths = map types.systemdPath [
          cfg.watch.storeDir
          "/nix/var/nix/db"
        ];
      };
    };
  };
}
