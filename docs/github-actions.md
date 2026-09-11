# GitHub Actions

## Prerequisites

Before running a build:

1. Join the runner to the tailnet. Do this before installing Nix when the cache
   will be a substituter.
2. Give the runner tag the `kradalby.no/cap/tsnixcache` push grant.
3. Configure Nix with `extra-substituters` and
   `extra-trusted-public-keys`.
4. Store tailnet credentials in GitHub secrets.
5. Pin the Tailscale and Nix installer actions to reviewed commits.

This repository does not provision runner credentials.

## Build lifecycle

Resolve the binary once, create a private session directory, wait for readiness,
and explicitly drain after the build. This shell sequence can be one Actions
step after tailnet and Nix setup:

```bash
set -euo pipefail
session=$(mktemp -d "${RUNNER_TEMP:-/tmp}/tsnixcache.XXXXXX")
chmod 700 "$session"
package=$(nix build --no-link --print-out-paths github:kradalby/tsnixcache#default)
cache="$package/bin/tsnixcache"
state="${TSNIXCACHE_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/tsnixcache-watch}"
token=""

finish() {
  build_status=$?
  trap - EXIT INT TERM
  set +e
  args=(--stop --pid-file "$session/watch.pid" --pid-file-timeout 10s --timeout 60s)
  if [ -n "$token" ]; then args+=(--session "$token"); fi
  "$cache" wait-for "${args[@]}"
  upload_status=$?
  if [ "$upload_status" -eq 0 ]; then
    wait "$watcher"
    upload_status=$?
  fi
  cat "$session/watch.log"
  printf 'Session logs and terminal status: %s\n' "$session"
  if [ "$build_status" -ne 0 ]; then exit "$build_status"; fi
  exit "$upload_status"
}

"$cache" watch --to "${TSNIXCACHE_URL:-http://tsnixcache}" \
  --state-dir "$state" --pid-file "$session/watch.pid" >"$session/watch.log" 2>&1 &
watcher=$!
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
token=$("$cache" wait-for --ready --timeout 60s --pid-file "$session/watch.pid")
nix build .
```

## What the recipe guarantees

- A long build with no registered outputs does not stop the watcher.
- The exit trap requests an authenticated final drain after success or failure.
- A build failure keeps its exit status.
- A successful build fails when the final upload fails.
- Session logs and terminal status remain available for diagnosis.

## State and recovery

| Data                         | Location                                                                          | Lifetime                |
| ---------------------------- | --------------------------------------------------------------------------------- | ----------------------- |
| PID, status, and log         | `$RUNNER_TEMP/tsnixcache.*`                                                       | Current job only        |
| Authenticated control socket | Private temporary directory                                                       | Watcher process         |
| Retry queue                  | `${TSNIXCACHE_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/tsnixcache-watch}` | Preserve across retries |

Hosted runners need explicit persistence for retry state between jobs.
`RUNNER_TEMP` is cleared at job boundaries.

If authenticated stop times out, keep the session directory and retry the stop
before reusing that state directory.

`TSNIXCACHE_URL` sets the destination for the recipe and is also the default URL
for manual `push` and `watch` commands.
