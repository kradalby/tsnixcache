{ config, lib, pkgs, ... }:

let
  cfg = config.services.tsnixcache;
  tsnetType = lib.types.submodule {
    options = {
      hostname = lib.mkOption { type = lib.types.str; description = "Tailscale hostname for this tsnet instance."; };
      controlUrl = lib.mkOption { type = lib.types.str; default = ""; description = "Tailscale control URL (empty = default)."; };
      authKeyFile = lib.mkOption { type = lib.types.path; description = "File containing the Tailscale auth key."; };
      dir = lib.mkOption { type = lib.types.str; description = "State directory for this tsnet instance."; };
      port = lib.mkOption { type = lib.types.port; default = 80; description = "Port to listen on."; };
      tls = lib.mkOption { type = lib.types.bool; default = false; description = "Enable TLS."; };
    };
  };
  tsnetToFlag = ts:
    "hostname=${ts.hostname}" +
    lib.optionalString (ts.controlUrl != "") ",control=${ts.controlUrl}" +
    ",authkey-file=${ts.authKeyFile}" +
    ",dir=${ts.dir}" +
    ",port=${toString ts.port}" +
    ",tls=${lib.boolToString ts.tls}";
in
{
  options.services.tsnixcache = {
    enable = lib.mkEnableOption "tsnixcache Nix binary cache server";

    package = lib.mkOption {
      type = lib.types.package;
      description = "tsnixcache package to use.";
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

    signKeyFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Path to the ed25519 signing key file (name:base64 format).";
    };

    priority = lib.mkOption {
      type = lib.types.int;
      default = 30;
      description = "Cache priority (lower = higher priority).";
    };

    listen = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ "127.0.0.1:5000" ];
      description = "Local listen addresses.";
    };

    unixSocket = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Optional Unix socket path.";
    };

    spoolDir = lib.mkOption {
      type = lib.types.str;
      default = "/var/cache/tsnixcache/spool";
      description = "Directory for spooling incoming NARs before import.";
    };

    gcrootDir = lib.mkOption {
      type = lib.types.str;
      default = "/nix/var/nix/gcroots/tsnixcache";
      description = "Directory for GC root symlinks of imported paths.";
    };

    serveCompression = lib.mkOption {
      type = lib.types.enum [ "none" "zstd" ];
      default = "none";
      description = "Compression to use when serving NARs.";
    };

    capName = lib.mkOption {
      type = lib.types.str;
      default = "dalby.cc/cap/tsnixcache";
      description = "Tailscale capability name for write access.";
    };

    tsnet = lib.mkOption {
      type = lib.types.listOf tsnetType;
      default = [ ];
      description = "List of tsnet (Tailscale network) instances to serve on.";
    };
  };

  config = lib.mkIf cfg.enable {
    # tsnixcache needs to be a trusted user to import into the system store.
    nix.settings.trusted-users = [ "tsnixcache" ];

    users.users.tsnixcache = {
      isSystemUser = true;
      group = "tsnixcache";
      description = "tsnixcache daemon user";
    };
    users.groups.tsnixcache = { };

    systemd.services.tsnixcache = {
      description = "tsnixcache Nix binary cache server";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];

      serviceConfig = {
        User = "tsnixcache";
        Group = "tsnixcache";
        ExecStart =
          let
            args = lib.concatStringsSep " " (
              [ "${cfg.package}/bin/tsnixcache serve" ]
              ++ [ "--db ${cfg.db}" ]
              ++ [ "--store-dir ${cfg.storeDir}" ]
              ++ lib.optional (cfg.signKeyFile != null) "--sign-key-file ${cfg.signKeyFile}"
              ++ [ "--priority ${toString cfg.priority}" ]
              ++ map (l: "--listen ${l}") cfg.listen
              ++ lib.optional (cfg.unixSocket != null) "--unix-socket ${cfg.unixSocket}"
              ++ [ "--spool-dir ${cfg.spoolDir}" ]
              ++ [ "--gcroot-dir ${cfg.gcrootDir}" ]
              ++ [ "--serve-compression ${cfg.serveCompression}" ]
              ++ [ "--cap-name ${cfg.capName}" ]
              ++ map (ts: "--tsnet ${tsnetToFlag ts}") cfg.tsnet
            );
          in
          args;
        StateDirectory = "tsnixcache";
        CacheDirectory = "tsnixcache";
        RuntimeDirectory = "tsnixcache";

        # Hardening
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        NoNewPrivileges = true;
        ReadWritePaths = [
          cfg.spoolDir
          cfg.gcrootDir
          "/nix/var/nix/gcroots/tsnixcache"
        ] ++ map (ts: ts.dir) cfg.tsnet;
        ReadOnlyPaths = [
          cfg.storeDir
          "/nix/var/nix/db"
        ];

        # Include xz and zstd in the PATH so external-binary codec path works.
        Environment = [
          "PATH=${lib.makeBinPath (with pkgs; [ xz zstd coreutils ])}:$PATH"
        ];

        Restart = "on-failure";
        RestartSec = "5s";
      };

      preStart = ''
        mkdir -p ${cfg.spoolDir} ${cfg.gcrootDir}
      '';
    };
  };
}
