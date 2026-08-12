# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

# Shared pieces for the auto-push tests (post-build-hook, watch, and both).
{ pkgs, tsnixcache, tsnixcacheModule }:
rec {
  # Matching keypair for the test cache.
  keyName = "tsnixcache-test";
  secretKey = "${keyName}:C/9c4zl/Apai/V8C00Rp8nIr6kvMrzPP/YpU5epc00efqjLOyTkIHRgi0FZRxYLgKtzSD2mEvacHtPP9DZCO1Q==";
  publicKey = "${keyName}:n6oyzsk5CB0YItBWUcWC4Crc0g9phL2nB7Tz/Q2QjtU=";

  # An expression file (not a prebuilt derivation) for a trivial path that
  # builds locally at test time, so building it fires the post-build-hook. The
  # builder is a static busybox (no dynamic loader), which builds reliably in
  # the sandboxed VM; pin it and this file into the store via buildInputs.
  builder = pkgs.pkgsStatic.busybox;
  buildExpr = pkgs.writeText "hooktest.nix" ''
    derivation {
      name = "hooktest";
      system = builtins.currentSystem;
      builder = "${builder}/bin/busybox";
      args = [ "sh" "-c" "echo built > $out" ];
    }
  '';

  # Same idea, but the output contains its own store path, so nix records a
  # self-reference. Self-referencing paths are the case that broke signing
  # before, and every real package is one.
  selfRefExpr = pkgs.writeText "selfref.nix" ''
    derivation {
      name = "selfref";
      system = builtins.currentSystem;
      builder = "${builder}/bin/busybox";
      args = [ "sh" "-c" "echo $out > $out" ];
    }
  '';

  # Store paths the client must hold so the builds above run offline.
  buildInputs = [ buildExpr selfRefExpr builder ];

  # The signing key as an operator would deploy it: root-owned and mode 0400,
  # unreadable by the tsnixcache user. The service only gets at it because the
  # module loads it as a systemd credential.
  keyFile = "/etc/tsnixcache/key";
  keyFileConfig = {
    environment.etc."tsnixcache/key" = {
      text = secretKey;
      mode = "0400";
    };
  };

  # A signed cache server with the service user trusted so it can import pushes.
  serverNode = { config, pkgs, lib, ... }: {
    imports = [ tsnixcacheModule keyFileConfig ];
    services.tsnixcache = {
      enable = true;
      package = tsnixcache;
      listen = [ "0.0.0.0:5000" ];
      # Every test built on this node pushes over that plain listener, which
      # carries no identity and so refuses writes unless asked. The refusal
      # itself is what nix/tests/push.nix asserts.
      localWrite = true;
      signKeyFile = keyFile;
    };
    nix.settings.trusted-users = [ "root" "tsnixcache" ];
    networking.firewall.allowedTCPPorts = [ 5000 ];
  };
}
