{ config, lib, pkgs, ... }:

let
  cfg = config.services.tsnixcache-client;
in
{
  options.services.tsnixcache-client = {
    enable = lib.mkEnableOption "tsnixcache Nix binary cache client";

    package = lib.mkOption {
      type = lib.types.package;
      description = "tsnixcache package to use.";
    };

    substituter = lib.mkOption {
      type = lib.types.str;
      description = "URL of the tsnixcache server (e.g. http://cache:5000).";
    };

    publicKey = lib.mkOption {
      type = lib.types.str;
      description = "Signing public key for the cache (name:base64 format).";
    };

    watch = {
      enable = lib.mkEnableOption "tsnixcache watch daemon (auto-push new store paths)";

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
    nix.settings = {
      substituters = lib.mkAfter [ cfg.substituter ];
      trusted-public-keys = [ cfg.publicKey ];
    };

    systemd.services.tsnixcache-watch = lib.mkIf cfg.watch.enable {
      description = "tsnixcache watch — auto-push new Nix store paths";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" "nix-daemon.service" ];

      serviceConfig = {
        ExecStart = lib.concatStringsSep " " [
          "${cfg.package}/bin/tsnixcache watch"
          "--db ${cfg.watch.db}"
          "--store-dir ${cfg.watch.storeDir}"
          "--to ${cfg.substituter}"
        ];
        Restart = "on-failure";
        RestartSec = "10s";
        PrivateTmp = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        NoNewPrivileges = true;
        ReadOnlyPaths = [
          cfg.watch.storeDir
          "/nix/var/nix/db"
        ];
      };
    };
  };
}
