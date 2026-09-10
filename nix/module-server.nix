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
  cfg = config.services.tsnixcache;

  types = import ./lib.nix { inherit lib; };

  # Credential names systemd exposes under %d for the service. Loading the key
  # and auth key as credentials means systemd reads them as root before
  # dropping to the tsnixcache user, so the files can stay 0400 root-owned.
  signKeyCred = "sign-key";
  tsnetCred = i: "tsnet-authkey-${toString i}";
  credentialDir = "/run/tsnixcache-credentials";
  credentials =
    lib.optional (cfg.signKeyFile != null) {
      name = signKeyCred;
      path = cfg.signKeyFile;
    }
    ++ lib.imap0 (i: ts: {
      name = tsnetCred i;
      path = ts.authKeyFile;
    }) cfg.tsnet;

  # Secret files are named by string, never by path literal: a path literal is
  # copied into the Nix store, where it is world-readable and travels with
  # every closure the machine's configuration is pushed to.
  inStore = p: lib.hasPrefix "${builtins.storeDir}/" p;
  secretLeak = option: p: ''
    services.tsnixcache.${option} points into the Nix store (${p}). The store is
    world-readable and its contents travel with every closure this configuration
    is copied to, so the secret would be published to every user on this machine
    and to any binary cache the closure reaches. Deploy the file out of band and
    give the option its path as a string, e.g. "/etc/tsnixcache/key".
  '';

  tsnetValue = lib.types.addCheck lib.types.str (value: !(lib.hasInfix "," value));
  tsnetType = lib.types.submodule {
    options = {
      hostname = lib.mkOption {
        type = tsnetValue;
        default = "tsnixcache";
        description = "Commas are unsupported in tsnet flag values. Tailscale hostname for this tsnet instance.";
      };
      controlUrl = lib.mkOption {
        type = tsnetValue;
        default = "";
        description = "Commas are unsupported in tsnet flag values. Tailscale control URL (empty = default).";
      };
      authKeyFile = lib.mkOption {
        type = lib.types.str;
        description = "Path to a file containing the Tailscale auth key. Must not be in the Nix store.";
      };
      dir = lib.mkOption {
        type = tsnetValue;
        description = "Commas are unsupported in tsnet flag values. State directory for this tsnet instance.";
      };
      port = lib.mkOption {
        type = lib.types.port;
        default = 80;
        description = "Port to listen on.";
      };
      tls = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Enable TLS. Not supported: tsnixcache serves plain HTTP over the tailnet, so true fails evaluation.";
      };
    };
  };
  # The auth key is read from the systemd credential (%d), not from its
  # original path, so the file itself never has to be readable by the service
  # user.
  tsnetToFlag =
    ts:
    "hostname=${ts.hostname}"
    + lib.optionalString (ts.controlUrl != "") ",control=${ts.controlUrl}"
    + ",dir=${ts.dir}"
    + ",port=${toString ts.port}"
    + ",tls=${lib.boolToString ts.tls}";

  # db/storeDir/gcrootDir default to null so "set alongside store" can be told
  # from "not set"; the CLI carries the real defaults. The unit still needs
  # concrete paths for the tmpfiles rules and the sandbox mounts, hence these.
  storeDir = if cfg.storeDir != null then cfg.storeDir else "/nix/store";
  gcrootDir = if cfg.gcrootDir != null then cfg.gcrootDir else "/nix/var/nix/gcroots/tsnixcache";

  # In chroot mode the CLI derives the gcroot directory inside the chroot, so
  # only the chroot itself has to exist and be writable.
  chroot = cfg.store != "";
  writableDirs = [
    cfg.spoolDir
  ]
  ++ lib.optional (!chroot) gcrootDir
  ++ lib.optional chroot cfg.store;

  # A "host:port", with the host optionally bracketed for IPv6, so the host is
  # everything before the last colon. Anything without one — including the
  # empty entry that asks for no local listener at all — has no host to judge.
  listenHost =
    l:
    let
      parts = lib.splitString ":" l;
    in
    if lib.length parts < 2 then
      null
    else
      lib.removeSuffix "]" (lib.removePrefix "[" (lib.concatStringsSep ":" (lib.init parts)));
  isLoopback =
    l:
    let
      h = listenHost l;
    in
    h == null || h == "localhost" || h == "::1" || lib.hasPrefix "127." h;

  # An empty entry asks the CLI for no local listener at all, so it is not a
  # listener anything can reach and must not trigger either warning below.
  localListeners = lib.filter (l: l != "") cfg.listen;
  exposed = lib.filter (l: !(isLoopback l)) localListeners;
in
{
  options.services.tsnixcache = {
    enable = lib.mkEnableOption "tsnixcache Nix binary cache server";

    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.tsnixcache;
      defaultText = lib.literalExpression "pkgs.tsnixcache";
      description = "tsnixcache package to use; the flake's overlays.default provides pkgs.tsnixcache.";
    };

    db = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      defaultText = lib.literalExpression ''"/nix/var/nix/db/db.sqlite"'';
      description = "Path to the Nix SQLite database. Mutually exclusive with store.";
    };

    storeDir = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      defaultText = lib.literalExpression ''"/nix/store"'';
      description = "Path to the Nix store. Mutually exclusive with store.";
    };

    store = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = ''
        Serve and import into a self-contained chroot Nix store rooted at this
        path, instead of the system /nix/store. Derives the database, store
        directory, import URI and gcroot directory from the prefix, initialises
        the store on first start, and needs no trusted-user privileges.
        Mutually exclusive with db/storeDir/gcrootDir and with gc.rules.
      '';
    };

    signKeyFile = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = ''
        Path to the ed25519 signing key file (name:base64 format). Must not be
        in the Nix store: the store is world-readable, so a path literal here
        would publish the key that every consumer of this cache trusts.
      '';
    };

    priority = lib.mkOption {
      type = lib.types.int;
      default = 30;
      description = "Cache priority (lower = higher priority).";
    };

    listen = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      # An unauthenticated listener should be something the operator asked for.
      # Configuring tsnet is asking for the authenticated path, so the loopback
      # one is not added on top of it — set this explicitly to get both, as the
      # in-VM metric checks in nix/tests/tsnet.nix do. With no tsnet instance
      # there is nothing else to serve on, so the loopback default stands.
      default = if cfg.tsnet == [ ] then [ "127.0.0.1:5000" ] else [ ];
      defaultText = lib.literalExpression ''if tsnet == [ ] then [ "127.0.0.1:5000" ] else [ ]'';
      description = ''
        Local listen addresses. These carry no identity — there is nothing to
        authenticate a request against, for the network or for other local uids
        — so they serve reads only and refuse every write, unless localWrite is
        set. Reads are open to anything that can reach the socket, so keep them
        on loopback and use tsnet (which gates writes on a capability grant) for
        anything else.
      '';
    };

    localWrite = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = ''
        Accept pushes on the listen addresses as well as reads. Nothing
        identifies a request arriving there, so this grants every local uid —
        and, on a non-loopback address, everything that can reach the socket —
        the power to import arbitrary paths into the store. Those paths are then
        re-signed with the cache's own key and, at the cache's priority, are
        preferred over the upstream cache by every client that trusts it. Leave
        this off and push over tsnet, where writes need a capability grant,
        unless the listener is genuinely reachable only by things you would
        trust to build.
      '';
    };

    spoolDir = lib.mkOption {
      type = lib.types.str;
      default = "/var/cache/tsnixcache/spool";
      description = "Directory for spooling incoming NARs before import.";
    };

    maxNarSize = lib.mkOption {
      type = lib.types.ints.between 1 9223372036854775806;
      default = 17179869184;
      description = "Maximum decoded NAR size in bytes, independent of compressed upload size.";
    };

    gcrootDir = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      defaultText = lib.literalExpression ''"/nix/var/nix/gcroots/tsnixcache"'';
      description = "Directory for GC root symlinks of imported paths. Mutually exclusive with store.";
    };

    serveCompression = lib.mkOption {
      type = lib.types.enum [
        "none"
        "zstd"
      ];
      default = "none";
      description = "Compression to use when serving NARs.";
    };

    tsnet = lib.mkOption {
      type = lib.types.listOf tsnetType;
      default = [ ];
      description = "List of tsnet (Tailscale network) instances to serve on.";
    };

    gc = {
      rules = lib.mkOption {
        type = lib.types.listOf (
          lib.types.submodule {
            options = {
              threshold = lib.mkOption {
                type = lib.types.ints.between 1 100;
                description = "Disk usage percentage that triggers this rule.";
              };

              olderThan = lib.mkOption {
                type = types.goDurationOrDays;
                description = ''
                  Minimum age of a pushed path before it may be garbage-collected (e.g. "20d").
                  When the rule triggers, gcroot symlinks older than this are removed, then
                  nix-collect-garbage runs to reclaim the now-unrooted store paths.
                  Paths imported more recently than this duration are always kept.
                '';
              };
            };
          }
        );
        default = [ ];
        example = [
          {
            threshold = 80;
            olderThan = "20d";
          }
          {
            threshold = 90;
            olderThan = "10d";
          }
          {
            threshold = 95;
            olderThan = "5d";
          }
        ];
        description = ''
          GC rules: when disk usage exceeds threshold%, prune gcroot symlinks for paths
          imported more than olderThan ago, then run nix-collect-garbage.
          Paths newer than olderThan are always kept regardless of disk pressure.
        '';
      };

      interval = lib.mkOption {
        type = types.goDurationOrDays;
        default = "5m";
        description = "How often to check disk usage for GC rules.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = !(chroot && cfg.gc.rules != [ ]);
        message = "services.tsnixcache: gc.rules is not supported together with a chroot store (services.tsnixcache.store).";
      }
      {
        # serve rejects the combination outright (errStoreFlagConflict), so
        # without this the module would quietly drop the flags and serve a
        # store the operator did not configure.
        assertion = !(chroot && (cfg.db != null || cfg.storeDir != null || cfg.gcrootDir != null));
        message = "services.tsnixcache: db/storeDir/gcrootDir cannot be combined with a chroot store (services.tsnixcache.store); it derives all three.";
      }
      {
        # serve's parseTsnetSpec makes tls=true fatal, so without this the unit
        # evaluates cleanly and then restart-loops on every start.
        assertion = !(lib.any (ts: ts.tls) cfg.tsnet);
        message = "services.tsnixcache: tsnet.*.tls is not supported; tsnixcache serves plain HTTP over the tailnet.";
      }
      {
        assertion = cfg.signKeyFile == null || !(inStore cfg.signKeyFile);
        message = secretLeak "signKeyFile" (toString cfg.signKeyFile);
      }
      {
        assertion = lib.all (c: lib.hasPrefix "/" c.path) credentials;
        message = "services.tsnixcache signing and auth key files must have absolute paths.";
      }
    ]
    ++ lib.imap0 (i: ts: {
      assertion = !(inStore ts.authKeyFile);
      message = secretLeak "tsnet.${toString i}.authKeyFile" ts.authKeyFile;
    }) cfg.tsnet;

    # Two separate hazards, so two separate warnings: who can read the store,
    # and who can write to it. Both are legitimate behind a firewall or on a
    # single-user host, hence warnings rather than assertions.
    warnings =
      # Reads are unauthenticated on every listener, by design. On loopback that
      # is the operator's own machine; off it, it is whoever can reach the port.
      map (l: ''
        services.tsnixcache.listen has a non-loopback address (${l}). Reads there are
        unauthenticated, so anything that can reach it can enumerate and download every
        path this cache serves. Restrict it at the firewall, or serve over tsnet instead.
      '') exposed
      # Writes are refused there by default because nothing identifies the
      # caller; localWrite is the way back to a listener anything can push to,
      # and the service user is a nix trusted user, so it deserves saying out
      # loud what that hands out.
      ++ lib.optional (cfg.localWrite && localListeners != [ ]) (
        ''
          services.tsnixcache.localWrite accepts unauthenticated pushes on ${lib.concatStringsSep ", " localListeners}.
          Every local uid can import arbitrary paths, which are re-signed with the cache
          key and, at priority ${toString cfg.priority}, shadow the upstream cache for every client that
          trusts it. Serve over tsnet instead, where writes need the
          kradalby.no/cap/tsnixcache push grant.
        ''
        + lib.optionalString (exposed != [ ]) ''
          Worse, not all of those are loopback (${lib.concatStringsSep ", " exposed}), so it is not
          only local uids but anything on the network that can reach the port.
        ''
      );

    # `tsnixcache key generate` and `key public` are the first thing an
    # operator runs on a cache host.
    environment.systemPackages = [ cfg.package ];

    # Importing into the system store needs trust; a chroot store does not.
    nix.settings.trusted-users = lib.mkIf (!chroot) [ "tsnixcache" ];

    # Pre-create directories before the service namespace is set up.
    systemd.tmpfiles.rules =
      map (d: "d ${types.systemdPath d} 0750 tsnixcache tsnixcache -") writableDirs
      ++ map (ts: "d ${types.systemdPath ts.dir} 0750 tsnixcache tsnixcache -") cfg.tsnet
      # systemd's executor serialization splits credential source paths on spaces.
      ++ lib.optional (credentials != [ ]) "d ${credentialDir} 0700 root root -"
      ++ map (
        c:
        "L+ ${credentialDir}/${c.name} - - - - ${
          lib.replaceStrings [ "%" ] [ "%%" ] (lib.strings.escapeC [ "\t" "\n" "\r" " " "\\" ] c.path)
        }"
      ) credentials;

    users.users.tsnixcache = {
      isSystemUser = true;
      group = "tsnixcache";
      description = "tsnixcache daemon user";
    };
    users.groups.tsnixcache = { };

    systemd.services.tsnixcache = {
      description = "tsnixcache Nix binary cache server";
      wantedBy = [ "multi-user.target" ];
      # Nix keeps its database in WAL mode and SQLite cannot read one without
      # writing a wal-index, which this unit is not allowed to do. It works
      # only while another process holds the database open, so the daemon has
      # to be up first — otherwise a fresh boot restart-loops. The daemon also
      # does every import and collection on the service's behalf.
      after = [
        "network.target"
        "nix-daemon.service"
      ];
      wants = [ "nix-daemon.service" ];
      # Stable aliases must still restart the service when their sources change.
      restartTriggers = lib.optional (credentials != [ ]) (
        pkgs.writeText "tsnixcache-credential-sources.json" (builtins.toJSON credentials)
      );

      serviceConfig = {
        User = "tsnixcache";
        Group = "tsnixcache";
        # Escape user values before appending trusted credential specifiers.
        ExecStart =
          utils.escapeSystemdExecArgs (
            [
              "${cfg.package}/bin/tsnixcache"
              "serve"
            ]
            ++ lib.optionals chroot [
              "--store"
              cfg.store
            ]
            ++ lib.optionals (cfg.db != null) [
              "--db"
              cfg.db
            ]
            ++ lib.optionals (cfg.storeDir != null) [
              "--store-dir"
              cfg.storeDir
            ]
            ++ lib.optionals (cfg.gcrootDir != null) [
              "--gcroot-dir"
              cfg.gcrootDir
            ]
            ++ [
              "--priority"
              (toString cfg.priority)
            ]
            ++ lib.concatMap (l: [
              "--listen"
              l
            ]) cfg.listen
            ++ lib.optional cfg.localWrite "--local-write"
            ++ [
              "--spool-dir"
              cfg.spoolDir
            ]
            ++ [
              "--serve-compression"
              cfg.serveCompression
              "--max-nar-size"
              (toString cfg.maxNarSize)
            ]
            ++ lib.concatMap (r: [
              "--gc-rule"
              "${toString r.threshold}:${r.olderThan}"
            ]) cfg.gc.rules
            ++ lib.optionals (cfg.gc.rules != [ ]) [
              "--gc-interval"
              cfg.gc.interval
            ]
          )
          + lib.optionalString (cfg.signKeyFile != null) " --sign-key-file \"%d/${signKeyCred}\""
          + lib.concatStrings (
            lib.imap0 (
              i: ts:
              " --tsnet "
              + lib.removeSuffix "\"" (utils.escapeSystemdExecArg (tsnetToFlag ts))
              + ",authkey-file=%d/${tsnetCred i}\""
            ) cfg.tsnet
          );
        StateDirectory = "tsnixcache";
        CacheDirectory = "tsnixcache";
        RuntimeDirectory = "tsnixcache";

        # Secrets are read by systemd as root and handed to the service as
        # credentials under %d, owned by the service user and mode 0400. The
        # source files can therefore stay root-only — nothing here depends on
        # the operator having picked a mode that happens to work.
        LoadCredential = map (c: "${c.name}:${credentialDir}/${c.name}") credentials;

        # Hardening. Nothing here needs to write to the store: every import and
        # every collection is carried out by nix-daemon on the service's
        # behalf, so the service only writes its own spool and gcroot dirs.
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        NoNewPrivileges = true;
        CapabilityBoundingSet = "";
        AmbientCapabilities = "";
        LockPersonality = true;
        # AF_NETLINK is tsnet's: its link monitor watches route changes.
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
          "AF_NETLINK"
        ];
        SystemCallFilter = [ "@system-service" ];
        ReadWritePaths = map types.systemdPath (writableDirs ++ map (ts: ts.dir) cfg.tsnet);
        ReadOnlyPaths = map types.systemdPath [
          storeDir
          "/nix/var/nix/db"
        ];

        # systemd does not expand $PATH, so the list must be complete: xz is
        # mandatory (every xz-compressed push is decoded by the binary, because
        # only it can bound the declared dictionary size), zstd for optional
        # external compression, nix for nix-store --serve and
        # nix-collect-garbage, coreutils for the preStart script.
        Environment = [
          "PATH=${
            lib.makeBinPath [
              pkgs.xz
              pkgs.zstd
              pkgs.coreutils
              pkgs.nix
            ]
          }"
        ];

        Restart = "on-failure";
        RestartSec = "5s";
      };

      preStart = ''
        mkdir -p ${lib.escapeShellArgs writableDirs}
      '';
    };
  };
}
