# Push resilience

Choose the delivery method that matches the caller:

| Method             | Use it when                                             | Failure behaviour                                  |
| ------------------ | ------------------------------------------------------- | -------------------------------------------------- |
| Post-build hook    | A Nix build should never fail because the cache is down | Logs the failure and exits successfully            |
| `tsnixcache push`  | A person or script needs the result now                 | Returns non-zero when any path remains undelivered |
| `tsnixcache watch` | Delivery must survive an outage or restart              | Keeps pending work on disk and retries later       |

I recommend the post-build hook for exact build outputs and the watcher as a
fallback for paths added by other means.

## Retry a manual push

`push` resolves each requested closure and retries transient failures per path.
It skips a path already present on the server. A partial NAR upload restarts from
the beginning.

Independent paths continue after a failure. Paths that depend on a failed
closure member are also reported as failed because their references did not
arrive.

`--attempts` defaults to five per path. `--timeout` is one deadline for the whole
command and defaults to `0`, which disables that deadline.

The client retries server errors and HTTP `408`, `425`, and `429`. Other `4xx`
responses are permanent. This stops retries after errors such as a missing push
grant or an oversized request.

## Understand upload timeouts

| Phase                       |          Default | Behaviour                                        |
| --------------------------- | ---------------: | ------------------------------------------------ |
| NAR upload without progress |       60 seconds | Cancel and retry the path                        |
| Final narinfo request       | Up to 35 minutes | Stop that path without another immediate attempt |
| Response body read          | 60 seconds total | Retry unless the HTTP status is permanent        |
| Server import               |       30 minutes | Return an import failure                         |

The 35-minute narinfo limit is a ceiling. An earlier `--timeout`, watcher stop,
or other cancellation ends it sooner. The module's post-build hook sets a
60-second overall deadline by default.

The final narinfo request can be quiet while the server verifies and imports a
large path. It is not subject to NAR progress detection. Error response bodies
are read up to 4 KiB, with one extra byte used to detect truncation.

## Keep watcher retries across restarts

`watch` records discovered paths, retries, backoff, expiry, and drain progress
in a private state database. A process crash can cause a completed upload to be
sent again, but does not lose pending work.

State is separate for each combination of:

- source Nix database path;
- store directory;
- destination URL.

Keep `--state-dir` across restarts. A second watcher cannot open the same state
at the same time. Corrupt or unsupported state fails visibly instead of starting
with an empty queue.

### Backoff and batch size

Per-path backoff starts at 30 seconds, doubles up to 10 minutes, and adds 25%
jitter. Change these with `--retry-backoff-base` and `--retry-backoff-max`.

`--retry-queue-size` defaults to 512 and limits how much work is selected at
once. It is not a queue capacity: remaining work stays in the source or state
database. A value of `0` uses bounded internal batch sizes.

### Expiry

`--retry-max-age` defaults to two hours from the first failed attempt. `0`
disables expiry.

An expired path is no longer retried, and the next drain reports failure. It is
removed from retry state after it disappears from the source database. If Nix
registers the same path again later, that new registration can reset the expiry.
Changing the option to `0` does not revive work that already expired.

## Drain before shutdown

On shutdown, the watcher discovers and uploads work registered before the stop
request. `--drain-timeout` limits this to 30 seconds by default; `0` removes the
deadline. Each non-expired pending path that still exists gets one final upload
invocation even when its normal backoff is active. That invocation keeps the
configured per-upload attempt count, which defaults to two for `watch`.

If the drain does not finish, the watcher exits non-zero and keeps pending work
for the next start. The NixOS and nix-darwin services allow 90 seconds for the
watcher to stop.

### Wait for a named session

Pass `--pid-file` when another process must wait for delivery. The watcher then
creates a status file and a private authenticated control socket.

Wait for readiness and save the printed session token:

```sh
token=$(tsnixcache wait-for --ready --pid-file /path/to/watch.pid)
```

Request the final drain and wait for its result:

```sh
tsnixcache wait-for --stop --session "$token" \
  --pid-file /path/to/watch.pid
```

The terminal result remains in `<pid-file>.status.json` after the watcher exits.
Without `--pid-file`, `wait-for` cannot observe the result. Missing status is
bounded by `--pid-file-timeout`, which defaults to two minutes.

Use an explicit stop for CI and other bounded build sessions. `--idle-exit`
defaults to `0`, so the watcher otherwise keeps running.

`wait-for --timeout` bounds the whole readiness or stop wait. Set it and
`watch --drain-timeout` to `0` when the job or service manager already provides
the outer deadline. A second signal or the job's forced termination ends a stuck
drain.

## Observe failures

`tsnixcache push` is short-lived and exposes no client-side Prometheus metrics.
Use its exit status and logs. For persistent delivery, inspect the
`tsnixcache watch` service log.
