# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

{ lib }:
let
  maxDuration = 9223372036854775807;
  factors = {
    ns = 1;
    us = 1000;
    ms = 1000000;
    s = 1000000000;
    m = 60000000000;
    h = 3600000000000;
    d = 86400000000000;
  };
  # Reject overflow before converting or multiplying an untrusted count.
  boundedCount =
    count: limit:
    let
      digits = builtins.head (builtins.match "0*([1-9][0-9]*|0)" count);
      ceiling = toString limit;
    in
    if
      builtins.stringLength digits > builtins.stringLength ceiling
      || (builtins.stringLength digits == builtins.stringLength ceiling && digits > ceiling)
    then
      null
    else
      lib.toInt digits;
  addPart =
    total: part:
    if total == null then
      null
    else
      let
        factor = factors.${builtins.elemAt part 1};
        count = boundedCount (builtins.elemAt part 0) (builtins.div (maxDuration - total) factor);
      in
      if count == null then null else total + count * factor;
  parseDuration =
    value:
    let
      days = builtins.match "([0-9]+)(d)" value;
      pieces = builtins.split "([0-9]+)(ns|us|ms|s|m|h)" value;
      valid = value != "" && lib.all (p: builtins.isList p || p == "") pieces;
    in
    if days != null then
      addPart 0 days
    else if valid then
      lib.foldl' addPart 0 (lib.filter builtins.isList pieces)
    else
      null;
  durationType =
    allowZero:
    lib.types.addCheck lib.types.str (
      value:
      let
        parsed = parseDuration value;
      in
      (allowZero && value == "0") || (parsed != null && parsed > 0)
    );
in
{
  inherit parseDuration;
  # Whole-number CLI subset: compounds through hours, or a bare day count.
  goDuration = durationType false;
  goDurationOrDays = durationType false;
  goDurationOrZero = durationType true;

  # tmpfiles and systemd path lists use C quoting and expand percent specifiers.
  systemdPath = value: lib.replaceStrings [ "%" ] [ "%%" ] (builtins.toJSON value);

  postBuildHook =
    {
      pkgs,
      package,
      to,
      timeout,
    }:
    pkgs.writeShellScript "tsnixcache-post-build-hook" ''
      set -u
      set -f
      [ -n "''${OUT_PATHS:-}" ] || exit 0
      export PATH=${
        lib.escapeShellArg (
          lib.makeBinPath [
            pkgs.nix
            pkgs.coreutils
          ]
        )
      }:"''${PATH:-}"
      if ! ${
        lib.escapeShellArgs [
          "${package}/bin/tsnixcache"
          "push"
          "--to"
          to
          "--timeout"
          timeout
        ]
      } $OUT_PATHS; then
        printf '%s\n' ${lib.escapeShellArg "tsnixcache: push to ${to} failed after retries; not failing build"} >&2
      fi
      exit 0
    '';
}
