# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.tsnixcache-client;

  types = import ./lib.nix { inherit lib; };

  # Post-build hook: nix runs this after each successful build with $OUT_PATHS
  # set to the freshly built paths. This is the canonical, exact auto-push
  # mechanism (the only way to populate harmonia; attic/cachix's recommended CI
  # path); the watch daemon below is the fallback for paths that arrive outside
  # the local daemon's builds (substituted, nix-store --add, imported).
  #
  # A non-zero exit here fails the build, so nothing in this script may abort:
  # under `set -u` an unset PATH would, hence ${PATH:-}.
  pushHook = pkgs.writeShellScript "tsnixcache-post-build-hook" ''
    set -u
    [ -n "''${OUT_PATHS:-}" ] || exit 0
    export PATH=${
      lib.makeBinPath [
        pkgs.nix
        pkgs.coreutils
      ]
    }:''${PATH:-}
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
      enable = lib.mkEnableOption "auto-push built paths via nix post-build-hook (exact, recommended)";

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
    };
  };

  config = lib.mkIf cfg.enable {
    # Make the CLI available for manual `tsnixcache push` / `key` on the host.
    environment.systemPackages = [ cfg.package ];

    nix.settings.substituters = lib.mkAfter cfg.substituters;
    nix.settings.trusted-public-keys = [ cfg.publicKey ];

    # Both push paths shell out to `nix copy`, which needs the nix-command feature.
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
        ExecStart = lib.escapeShellArgs [
          "${cfg.package}/bin/tsnixcache"
          "watch"
          "--db"
          cfg.watch.db
          "--store-dir"
          cfg.watch.storeDir
          "--to"
          cfg.watch.to
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
        Environment = [
          "PATH=${
            lib.makeBinPath [
              pkgs.nix
              pkgs.coreutils
            ]
          }"
          "XDG_CACHE_HOME=/var/cache/tsnixcache-watch"
        ];
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
        ReadOnlyPaths = [
          cfg.watch.storeDir
          "/nix/var/nix/db"
        ];
      };
    };
  };
}
