all: serve a Nix cache with durable uploads and coordinated garbage collection

Serve the local Nix store over HTTP and optional tsnet listeners, with signed cache metadata and writes gated by `kradalby.no/cap/tsnixcache`. NixOS server/client modules and a Darwin client module support hooks and a persistent watcher.

Uploads survive destination outages and watcher restarts. Readiness, authenticated stop and durable completion make CI drain failures visible. Imports and GC coordinate root ownership; compressed serving bounds active files and preserves reader lifetimes. Operational Grafana tiles stop displaying stale values after target loss.

Supports x86_64 Linux, aarch64 Linux and aarch64 Darwin. Native race tests, contributor hooks and the Darwin functional lifecycle run for each pushed revision. Local tests, 87.7% statement coverage under race detection, native hooks, dependency verification and 13 Linux VMs cover the remediation. Final per-revision native results are attached to the PR checks; performance measurements and tradeoffs are recorded in the remediation reports.

> Generated with the help of an AI assistant
