# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

# Option types shared by the tsnixcache modules.
#
# Every duration option ends up in a Go flag, and a value the type lets
# through but Go rejects is a service that exits at startup and restart-loops
# — or, for the post-build-hook, a push that silently never happens again —
# instead of an eval error. So each type mirrors exactly one Go parser, and
# checks.duration-types keeps them in step string by string.
{ lib }:

let
  # Go duration syntax, narrowed twice. Counts are whole numbers ("500ms", not
  # "0.5s") and at least one digit is non-zero, so "1h0m" is fine and "0s",
  # "0d" and "0m0s" are not. Fractions are what kept these types and Go out of
  # step: Go truncates a fraction below a nanosecond to zero, and a zero
  # duration is either rejected outright (parseDurationString) or panics the
  # ticker it is handed (watch --poll-interval). Only Go's "us" spelling of
  # microseconds is accepted, not "µs". Both narrowings err the safe way
  # round: an eval error, not a restart loop.
  # ponytail: the types still cannot catch a value that overflows int64
  # ("9999999999999999999h"); Go rejects that one at startup.
  unit = "(ns|us|ms|s|m|h)";
  num = "[0-9]+";
  numNonZero = "[0-9]*[1-9][0-9]*";
  duration = "(${num}${unit})*${numNonZero}${unit}(${num}${unit})*";
in
{
  # Values parsed by Go's flag.Duration (time.ParseDuration): push --timeout,
  # watch --poll-interval. time.ParseDuration has no day suffix, so "20d" has
  # to be refused here even though the gc flags take it.
  goDuration = lib.types.strMatching duration;

  # Values parsed by parseDurationString (cmd/tsnixcache/gc.go): the gc flags,
  # which additionally take a bare day count because the module documents that
  # form ("20d").
  goDurationOrDays = lib.types.strMatching "${numNonZero}d|${duration}";
}
