#!/usr/bin/env bash
# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

# Functional macOS auto-push test: exercises the watch daemon (directory notifications) and the
# post-build-hook against a chroot-store cache, the two mechanisms the darwin
# client module configures. Run on a macOS CI runner with nix installed.
set -euo pipefail

PORT=5000
URL="http://127.0.0.1:${PORT}"

BIN="$(nix build --no-link --print-out-paths .#default)/bin/tsnixcache"
echo "binary: ${BIN}"

# Resolve symlinks: on macOS mktemp returns /var/folders/... and /var is a
# symlink, which nix refuses as a chroot store parent.
CHROOT="$(cd "$(mktemp -d)" && pwd -P)"
SESSION_DIR="$(cd "$(mktemp -d)" && pwd -P)"
# The spool default lives under /var/cache, which an unprivileged CI user
# cannot create; keep it inside the chroot we already own.
#
# --allow-missing-codecs because serve otherwise refuses to start without xz(1),
# and a macOS runner is not guaranteed to have it: nothing here pushes xz, since
# both mechanisms under test go through `tsnixcache push`, which sends zstd. That
# makes this the zstd-only deployment the flag exists for, and CI's only coverage
# of it.
#
# --local-write because a --listen address carries no identity, so writes on one
# are refused by default; both mechanisms under test push over this loopback
# socket, so without it every push here comes back 403.
"${BIN}" serve --store "${CHROOT}" --spool-dir "${CHROOT}/spool" \
  --allow-missing-codecs --local-write --listen "127.0.0.1:${PORT}" >"${SESSION_DIR}/serve.log" 2>&1 &
SERVE_PID=$!
cleanup() {
  result=$?
  trap - EXIT
  if [ "$result" -ne 0 ]; then
    cat "${SESSION_DIR}/serve.log" "${SESSION_DIR}/watch.log" 2>/dev/null || true
  fi
  if [ -n "${WATCH_PID:-}" ]; then
    kill "${WATCH_PID}" 2>/dev/null || true
    wait "${WATCH_PID}" || true
  fi
  kill "${SERVE_PID}" 2>/dev/null || true
  wait "${SERVE_PID}" || true
  if [ -f "${SESSION_DIR}/nix.conf" ]; then
    sudo cp "${SESSION_DIR}/nix.conf" /etc/nix/nix.conf
    sudo launchctl kill SIGTERM system/org.nixos.nix-daemon || true
  fi
  chmod -R u+w "${CHROOT}" 2>/dev/null || true
  rm -rf "${CHROOT}" "${SESSION_DIR}"
  exit "$result"
}
trap cleanup EXIT

for _ in $(seq 1 30); do
  curl -sf "${URL}/nix-cache-info" >/dev/null && break
  sleep 1
done
curl -sf "${URL}/nix-cache-info" >/dev/null || {
  echo "serve never came up"
  cat "${SESSION_DIR}/serve.log"
  exit 1
}

wait_narinfo() { # $1 = 32-char hash
  for _ in $(seq 1 40); do
    curl -sf "${URL}/$1.narinfo" >/dev/null && return 0
    sleep 1
  done
  return 1
}

# --- watch / directory notifications ---------------------------------------------------------
"${BIN}" watch --to "${URL}" --poll-interval 5s \
  --state-dir "${SESSION_DIR}/state" --pid-file "${SESSION_DIR}/watch.pid" \
  >"${SESSION_DIR}/watch.log" 2>&1 &
WATCH_PID=$!
SESSION="$("${BIN}" wait-for --pid-file "${SESSION_DIR}/watch.pid" --ready --timeout 60s)"
printf '%s\n' "tsnixcache-darwin-watch-test-${SESSION_DIR}" >"${SESSION_DIR}/wf"
WPATH="$(nix-store --add "${SESSION_DIR}/wf")"
if wait_narinfo "$(basename "${WPATH}" | cut -c1-32)"; then
  echo "watch (directory notifications) OK"
else
  echo "watch FAILED"
  cat "${SESSION_DIR}/watch.log"
  exit 1
fi
"${BIN}" wait-for --pid-file "${SESSION_DIR}/watch.pid" --session "${SESSION}" --stop --timeout 60s
wait "${WATCH_PID}"
WATCH_PID=
"${BIN}" wait-for --pid-file "${SESSION_DIR}/watch.pid" --session "${SESSION}" --timeout 5s

# --- post-build-hook ----------------------------------------------------------
# The hook under test is the one the darwin module generates, not a copy of it:
# a copy is byte-identical right up until it isn't, and then the module's own
# script is built by CI and never run.
cat >"${SESSION_DIR}/hook.nix" <<'EOF'
{ url, root }:
let
  self = builtins.getFlake root;
  system = builtins.currentSystem;
  pkgs = self.inputs.nixpkgs.legacyPackages.${system};
  darwin = self.inputs.nix-darwin.lib.darwinSystem {
    modules = [
      self.darwinModules.tsnixcache-client
      {
        system.stateVersion = 6;
        nixpkgs.hostPlatform = system;
        services.tsnixcache-client = {
          enable = true;
          package = self.packages.${system}.default;
          publicKey = "tsnixcache-test:n6oyzsk5CB0YItBWUcWC4Crc0g9phL2nB7Tz/Q2QjtU=";
          postBuildHook = { enable = true; to = url; };
        };
      }
    ];
  };
in
# post-build-hook is a plain string, so the symlink is what pulls the script's
# context in and makes building this realise it.
pkgs.runCommand "tsnixcache-post-build-hook" { }
  "ln -s ${darwin.config.nix.settings.post-build-hook} $out"
EOF
HOOK="$(nix build --no-link --print-out-paths --impure \
  -f "${SESSION_DIR}/hook.nix" --argstr url "${URL}" --argstr root "$PWD")"
echo "hook: ${HOOK} -> $(readlink "${HOOK}")"

sudo cp /etc/nix/nix.conf "${SESSION_DIR}/nix.conf"
echo "post-build-hook = ${HOOK}" | sudo tee -a /etc/nix/nix.conf >/dev/null
sudo launchctl kill SIGTERM system/org.nixos.nix-daemon
for _ in $(seq 1 30); do
  nix --store daemon store ping >/dev/null 2>&1 && break
  sleep 1
done
nix --store daemon store ping

OUT="$(nix-build --no-out-link -E \
  "derivation { name = \"darwinhook-${SESSION_DIR##*/}\"; system = builtins.currentSystem; builder = \"/bin/bash\"; args = [ \"-c\" \"echo built > \$out\" ]; }")"
if wait_narinfo "$(basename "${OUT}" | cut -c1-32)"; then
  echo "post-build-hook OK"
else
  echo "post-build-hook FAILED"
  cat "${SESSION_DIR}/serve.log"
  exit 1
fi

echo "darwin auto-push: all mechanisms OK"
