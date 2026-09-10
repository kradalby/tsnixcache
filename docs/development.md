# Development

## Common commands

```sh
nix develop
prek install
nix run .#test-race
nix run .#lint
nix fmt
prek run --all-files
```

## Repository layout

Packages live at the repository root by function; command mains are thin.
There are no project-owned `internal/`, `pkg/` or `pkgs/` directories.

- Generated SQL lives under `gen/`.
- Tests sit beside their source.
- Benchmarks live in `bench_test.go` files.

After changing SQL, regenerate and check for drift:

```sh
go generate ./db
nix run .#generate
```

## Checks

| Command or service                         | Coverage                                                                                    |
| ------------------------------------------ | ------------------------------------------------------------------------------------------- |
| `prek run --all-files`                     | Formatting, lint, tests, generated files, and hashes                                        |
| `nix build .#nativeChecks`                 | Host build, race tests, lint, formatting, generation, duration, and hook checks without VMs |
| `nix flake check`                          | Full checks, including NixOS VMs on Linux; requires KVM                                     |
| `nix flake check --all-systems --no-build` | Evaluation for every supported platform                                                     |
| `nix run .#coverage`                       | Go statement coverage                                                                       |
| Garnix                                     | x86 Linux packages and NixOS VMs                                                            |
| GitHub Actions                             | Native ARM Linux and Darwin checks; Darwin module and lifecycle tests                       |

Changes to `flake.lock` also trigger native checks. Fuzz targets and benchmarks
supplement deterministic ownership, cancellation, and lifecycle regressions.

## Formatting and dependencies

Treefmt runs gofumpt, goimports, nixfmt, prettier, and shfmt. Hooks invoke it
with `--fail-on-change`.

When the vendor tree changes, including through imported test packages, update
its hash:

```sh
go run ./cmd/vendorhash update
```

Hooks verify `flakehashes.json`.

`golangci-lint` enables all linters with explicit exceptions. Comments document
those exceptions. Prose and user-facing strings use British spelling.

## Licensing

The project uses the BSD 3-Clause licence. The NAR reader and writer in `nar/`
derive from Tailscale's implementation and carry their own BSD 3-Clause
headers.
