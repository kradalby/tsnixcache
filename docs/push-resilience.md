# Push resilience

tsnixcache has three delivery mechanisms:

| Mechanism          | Failure behaviour                                                               |
| ------------------ | ------------------------------------------------------------------------------- |
| `post-build-hook`  | Logs the failure and exits successfully so the Nix build can continue.          |
| `tsnixcache push`  | Retries within its deadline and exits non-zero if any path remains undelivered. |
| `tsnixcache watch` | Stores pending work on disk and continues retrying across restarts.             |

## Upload retries

`tsnixcache push` retries each path with exponential backoff under the
`--attempts` and `--timeout` limits. A brief restart, deployment, or load spike
therefore heals without intervention.

Retries operate per path. An uploaded path is skipped after a cheap narinfo
`HEAD`, while a partially uploaded path is sent again in full. Uploads do not
resume part-way through a NAR.

One failed path does not stop the rest of the batch. If a path is garbage
collected after closure resolution, only that path is reported as failed.

Most `4xx` responses are permanent and are not retried. The retryable exceptions
are `408`, `425`, and `429`; server errors are also retried. This prevents a
revoked grant or oversized NAR from being attempted repeatedly.

## Timeouts

### Stalled uploads

`--stall-timeout` defaults to 60 seconds. A NAR transfer that makes no progress
for that long is cancelled and retried, allowing an interrupted network
connection to fail promptly.

### Imports

The final `.narinfo` request is exempt from stall detection. The server verifies
and imports the path before responding, so a healthy large import may be silent
for a long time.

That request has a fixed 35-minute deadline, just above the server's 30-minute
import deadline. Reaching it ends the current push without another immediate
attempt; the watcher can retry later.

Response bodies retain the 60-second stall deadline. Error diagnostics are
limited to 4 KiB plus one byte used to detect truncation. Body-read failures
remain eligible for the configured upload retries.

## Durable watcher state

`watch` records the following data transactionally in a private SQLite database:

- discovered paths and the polling cursor;
- pending retries and their backoff;
- first-failure times and expiry;
- drain acknowledgements and terminal results.

An outage or process crash therefore preserves pending work.

### Backoff and batching

Per-path backoff starts at `--retry-backoff-base` (30 seconds) and grows to
`--retry-backoff-max` (10 minutes), with 25% jitter.

`--retry-queue-size` defaults to 512 and limits discovery and selection batch
sizes. Overflow remains on disk; this setting is not a lossy queue capacity. A
zero value selects bounded defaults.

Each destination and source database pair has its own state identity. Competing
owners, corrupt state, and unknown schemas fail visibly instead of resetting the
queue. Keep the state directory across restarts.

### Expiry

`--retry-max-age` defaults to two hours; zero disables expiry. When work expires:

- the current drain fails;
- the path remains as a tombstone until it can be retired;
- later sessions may still succeed;
- disabling expiry does not revive an existing tombstone.

A tombstone retires when its source path disappears. Registering that path again
can reset expiry. Source replacement is reconciled conservatively, and unrelated
database files never inherit another source's cursor.

## Draining and shutdown

Shutdown captures a source boundary and gives the watcher 30 seconds to discover
and drain every page through it.

Each pending path receives at most one recorded final upload invocation for that
drain generation, even when its normal backoff is in the future. The configured
per-upload attempts still apply; the default is two.

If draining fails, the boundary, pending work, and consumed attempts remain in
state. Restarting resumes the saved ages and backoff. Repeated immediate stops do
not grant extra attempts.

An incomplete drain exits non-zero and remains visible to a later `wait-for`.
Service managers allow 90 seconds for shutdown.

## Session lifecycle

When `--pid-file` is set, the watcher also creates:

- an atomic status record;
- a private authenticated control socket.

`wait-for --ready` prints the session token. Use that token to request the final
drain:

```sh
tsnixcache wait-for --stop --session TOKEN --pid-file /path/to/watch.pid
```

A late waiter reads the recorded terminal result. Missing status produces a
bounded error. Legacy PID-only files are rejected after the startup timeout; in
that case, restart the watcher with the matching CLI.

`--idle-exit` defaults to zero. Explicit stop is the reliable way to finish a
build session.

## Observing failures

Client-side push metrics are intentionally absent because `tsnixcache push` is
short-lived and has nothing persistent to scrape. Check the post-build hook log
or the `tsnixcache watch` service log for persistent failures.
