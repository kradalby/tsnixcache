# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

{
  description = "tsnixcache — Nix binary cache over local + tsnet";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    flake-checks.url = "github:kradalby/flake-checks";
    flake-checks.inputs.nixpkgs.follows = "nixpkgs";
    # Headscale provides the control plane for the tsnet integration test.
    headscale.url = "github:juanfont/headscale/v0.29.3";
    headscale.inputs.nixpkgs.follows = "nixpkgs";
    # nix-darwin provides the launchd-based client module for macOS.
    nix-darwin.url = "github:nix-darwin/nix-darwin";
    nix-darwin.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      flake-checks,
      headscale,
      nix-darwin,
    }:
    let
      supportedSystems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      tsnixcacheModule = import ./nix/module-server.nix;
      tsnixcacheClientModule = import ./nix/module-client.nix;
      tsnixcacheClientDarwinModule = import ./nix/module-client-darwin.nix;

      fc = flake-checks.lib;

      # Formatters need the project's toolchain: a wrapped older Go would try
      # downloading its replacement inside the networkless formatting sandbox.
      goToolsOverlay = final: prev: {
        gofumpt = prev.gofumpt.override { buildGoModule = final.buildGoLatestModule; };
        gotools = prev.gotools.override {
          buildGoModule = final.buildGoLatestModule;
          go = final.go_latest;
        };
      };

      version = "0.1.0";
      # GET /version serves cache.Version, which stays "dev" unless the linker
      # sets it. A dirty tree has no shortRev, and dirtyShortRev changes on
      # every edit — which would relink the binary for a README typo — so a
      # working tree just says "dev".
      rev = self.shortRev or "dev";

      commonFor = pkgs: {
        inherit pkgs version;
        root = ./.;
        pname = "tsnixcache";
        vendorHash = (builtins.fromJSON (builtins.readFile ./flakehashes.json)).vendor.sri;
        goPkg = pkgs.go_latest;
        subPackages = [ "cmd/tsnixcache" ];
        embedDirs = [ ./db/schema.sql ];
        prettier = true;
        fmtExts = [
          "sh"
          "py"
        ];
        fmtInclude = [ ./pyproject.toml ];
        treefmtExtra = {
          programs.shfmt.enable = true;
          programs.ruff-format.enable = true;
          settings.global.excludes = [
            ".claude/**"
            "docs/plans/**"
            "docs/superpowers/**"
          ];
          settings.formatter.shfmt.options = [
            "-w"
            "-i"
            "2"
            "-ci"
          ];
        };
        env = {
          CGO_ENABLED = "0";
        };
        ldflags = [ "-X github.com/kradalby/tsnixcache/cache.Version=${version}-${rev}" ];
      };

      tsnixcacheFor =
        pkgs:
        (fc.goBuild (commonFor pkgs)).overrideAttrs (_: {
          meta = {
            description = "Nix binary cache that serves from /nix/store and accepts pushes via standard HTTP protocol";
            homepage = "https://github.com/kradalby/tsnixcache";
            license = pkgs.lib.licenses.bsd3;
            mainProgram = "tsnixcache";
          };
        });
    in
    {
      nixosModules = {
        tsnixcache = tsnixcacheModule;
        tsnixcache-client = tsnixcacheClientModule;
        default = tsnixcacheModule;
      };

      darwinModules = {
        tsnixcache-client = tsnixcacheClientDarwinModule;
        default = tsnixcacheClientDarwinModule;
      };

      # The modules default services.*.package to pkgs.tsnixcache, so a
      # consumer that adds this overlay can import them without threading a
      # package through by hand.
      overlays.default = final: _prev: {
        tsnixcache = tsnixcacheFor final;
      };

      # Example darwin system that exercises the client module end to end; the
      # macOS CI builds its toplevel to validate the launchd plist and nix.conf
      # the module generates. watch is enabled here so its plist is built too.
      darwinConfigurations.example = nix-darwin.lib.darwinSystem {
        system = "aarch64-darwin";
        modules = [
          tsnixcacheClientDarwinModule
          ({ ... }: {
            system.stateVersion = 6;
            nixpkgs.hostPlatform = "aarch64-darwin";
            services.tsnixcache-client = {
              enable = true;
              package = self.packages.aarch64-darwin.tsnixcache;
              publicKey = "tsnixcache-test:n6oyzsk5CB0YItBWUcWC4Crc0g9phL2nB7Tz/Q2QjtU=";
              postBuildHook.enable = true;
              watch.enable = true;
            };
          })
        ];
      };
    }
    // flake-utils.lib.eachSystem supportedSystems (
      system:
      let
        pkgs = import nixpkgs {
          inherit system;
          overlays = [ goToolsOverlay ];
        };
        common = commonFor pkgs;
        tsnixcache = tsnixcacheFor pkgs;

        # The dashboard generator is a separate binary (cmd/dashboard) so the
        # server build never pulls in the Grafana Foundation SDK. It emits the
        # tsnixcache Grafana dashboard as a bare JSON model on stdout.
        dashboard =
          (fc.goBuild (
            common
            // {
              pname = "tsnixcache-dashboard";
              subPackages = [ "cmd/dashboard" ];
              ldflags = [ ];
            }
          )).overrideAttrs
            (_: {
              meta = {
                description = "Generate the tsnixcache Grafana dashboard JSON";
                homepage = "https://github.com/kradalby/tsnixcache";
                license = pkgs.lib.licenses.bsd3;
                mainProgram = "dashboard";
              };
            });

        # Runs the generator and captures only the dashboard JSON, so a Nix
        # consumer (e.g. services.grafana provisioning) can point at this
        # derivation directly. Build() validates the schema, so an invalid
        # dashboard fails this build rather than shipping broken JSON.
        grafanaDashboards =
          pkgs.runCommand "tsnixcache-grafana-dashboards"
            {
              # The JSON is generated from this repository's sources, so it carries
              # the same licence; without a meta a consumer evaluating with
              # allowUnfree/licence filtering sees a package that declares nothing.
              meta = {
                description = "tsnixcache Grafana dashboard JSON for file-based provisioning";
                homepage = "https://github.com/kradalby/tsnixcache";
                license = pkgs.lib.licenses.bsd3;
              };
            }
            ''
              mkdir -p $out
              ${dashboard}/bin/dashboard > $out/tsnixcache.json
            '';
      in
      {
        packages = {
          inherit tsnixcache dashboard grafanaDashboards;
          default = tsnixcache;
          nativeChecks = pkgs.linkFarm "tsnixcache-native-checks" (
            map
              (name: {
                inherit name;
                path = self.checks.${system}.${name};
              })
              [
                "build"
                "gotest"
                "golangci-lint"
                "formatting"
                "duration-types"
                "hook-args"
                "generate"
              ]
          );
        };

        apps =
          pkgs.lib.mapAttrs
            (name: check: {
              type = "app";
              meta.description = "Run the tsnixcache ${name} check";
              program = toString (
                pkgs.writeShellScript "tsnixcache-${name}" ''
                  exec ${pkgs.nix}/bin/nix build --no-link -L .#checks.${system}.${check} "$@"
                ''
              );
            })
            {
              test = "gotest";
              test-race = "gotest";
              lint = "golangci-lint";
              coverage = "coverage";
              generate = "generate";
            };

        formatter = fc.formatter common;

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go_latest
            golangci-lint
            gofumpt
            gotools
            gotestsum
            treefmt
            nixfmt
            nix
            xz
            zstd
            bzip2
            prek
            sqlc
            gopls
            gnumake
            prettier
            shfmt
            govulncheck
            ruff
            pyright
            uv
            (python3.withPackages (ps: [ ps.playwright ]))
          ];
        };

        checks = {
          build = fc.goBuild common;

          hook-args = import ./nix/tests/hook-args.nix { inherit pkgs; };

          duration-types = import ./nix/duration-types.nix { inherit pkgs fc common; };

          # -race needs cgo, which it gets from stdenv's C compiler and Go's
          # default CGO_ENABLED=1: goTest builds a plain mkDerivation, so
          # common's env never reaches it. nix is on PATH because the NAR
          # differential against a real `nix-store --dump` — the best test in
          # the repo — otherwise skips itself and takes upload.Closure's
          # coverage with it.
          gotest = fc.goTest (
            common
            // {
              goRace = true;
              testFlags = [ "-short" ];
              nativeCheckInputs = with pkgs; [
                nix
                xz
                zstd
                bzip2
              ];
              # `nix path-info`, which upload.Closure resolves every push with,
              # is behind nix-command. Every real caller already has it — both
              # client modules turn it on whenever the hook or watch is enabled,
              # and each VM test sets it — so a sandbox without it is the odd one
              # out, and the tests it breaks only started running when nix landed
              # on PATH above.
              testEnv = "export NIX_CONFIG='experimental-features = nix-command'";
            }
          );
          generate = fc.goGenerate (
            common
            // {
              extraSrc = [
                ./sqlc.yaml
                ./db/queries
              ];
              nativeCheckInputs = [
                pkgs.sqlc
                pkgs.gofumpt
                pkgs.gotools
              ];
            }
          );
          coverage =
            (fc.goTest (
              common
              // {
                goRace = true;
                testFlags = [
                  "-short"
                  "-coverprofile=coverage.out"
                ];
                nativeCheckInputs = with pkgs; [
                  nix
                  xz
                  zstd
                  bzip2
                ];
                testEnv = "export NIX_CONFIG='experimental-features = nix-command'";
              }
            )).overrideAttrs
              (_: {
                installPhase = "cp coverage.out $out";
              });
          golangci-lint =
            (fc.goLint (
              common
              // {
                extraSrc = [
                  ./pyproject.toml
                  ./nix/tests/observability-browser.py
                  ./nix/tests/tsnet-revocation.py
                ];
              }
            )).overrideAttrs
              (old: {
                nativeBuildInputs = old.nativeBuildInputs ++ [
                  pkgs.ruff
                  pkgs.pyright
                  (pkgs.python3.withPackages (ps: [ ps.playwright ]))
                ];
                buildPhase = old.buildPhase + ''
                  ruff check nix/tests
                  pyright
                '';
              });
          formatting = fc.goFormat common;
        }
        // pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          readme-lifecycle = pkgs.testers.nixosTest (
            import ./nix/tests/readme-lifecycle.nix { inherit pkgs tsnixcache tsnixcacheModule; }
          );
          # NixOS VM tests: guest closures are Linux-only, so on darwin these
          # would ask for a full aarch64-linux system plus apple-virt and make
          # `nix flake check` unusable on the platform the darwin module ships
          # for.
          observability = pkgs.testers.nixosTest (
            import ./nix/tests/observability.nix {
              inherit
                pkgs
                tsnixcache
                tsnixcacheModule
                grafanaDashboards
                ;
            }
          );
          module-paths = pkgs.testers.nixosTest (
            import ./nix/tests/paths.nix {
              inherit
                pkgs
                tsnixcache
                tsnixcacheModule
                tsnixcacheClientModule
                ;
            }
          );
          serve = pkgs.testers.nixosTest (
            import ./nix/tests/serve.nix { inherit pkgs tsnixcache tsnixcacheModule; }
          );
          push = pkgs.testers.nixosTest (
            import ./nix/tests/push.nix { inherit pkgs tsnixcache tsnixcacheModule; }
          );
          store = pkgs.testers.nixosTest (
            import ./nix/tests/store.nix { inherit pkgs tsnixcache tsnixcacheModule; }
          );
          # Verify signed cache reads with Nix signature checking enabled.
          sigs = pkgs.testers.nixosTest (
            import ./nix/tests/sigs.nix { inherit pkgs tsnixcache tsnixcacheModule; }
          );
          # Drive a real collection under the module's own
          # hardening.
          gc = pkgs.testers.nixosTest (
            import ./nix/tests/gc.nix { inherit pkgs tsnixcache tsnixcacheModule; }
          );

          # Auto-push: post-build-hook (exact), watch (fallback), and both together.
          push-hook = pkgs.testers.nixosTest (
            import ./nix/tests/push-hook.nix {
              inherit
                pkgs
                tsnixcache
                tsnixcacheModule
                tsnixcacheClientModule
                ;
            }
          );
          push-watch = pkgs.testers.nixosTest (
            import ./nix/tests/push-watch.nix {
              inherit
                pkgs
                tsnixcache
                tsnixcacheModule
                tsnixcacheClientModule
                ;
            }
          );
          push-both = pkgs.testers.nixosTest (
            import ./nix/tests/push-both.nix {
              inherit
                pkgs
                tsnixcache
                tsnixcacheModule
                tsnixcacheClientModule
                ;
            }
          );
          push-hook-besteffort = pkgs.testers.nixosTest (
            import ./nix/tests/push-hook-besteffort.nix {
              inherit
                pkgs
                tsnixcache
                tsnixcacheModule
                tsnixcacheClientModule
                ;
            }
          );

          # Full tsnet integration test: headscale control plane + push/pull over Tailscale.
          tsnet = pkgs.testers.nixosTest (
            import ./nix/tests/tsnet.nix {
              inherit pkgs tsnixcache tsnixcacheModule;
              headscale = headscale.packages.${system}.headscale;
            }
          );

          # A VM test only ever runs configurations that work, and only ever
          # with one tsnet instance, so the module's refusals, its ExecStart
          # quoting and its index-keyed credentials have no coverage there.
          # All of it is pure evaluation, so it is checked here instead.
          module-eval =
            let
              lib = pkgs.lib;
              systemdUtils = import "${pkgs.path}/nixos/lib/utils.nix" {
                inherit lib pkgs;
                config = { };
              };
              eval =
                settings:
                (nixpkgs.lib.nixosSystem {
                  modules = [
                    tsnixcacheModule
                    {
                      nixpkgs.hostPlatform = "x86_64-linux";
                      system.stateVersion = "26.05";
                      services.tsnixcache = {
                        enable = true;
                        package = tsnixcache;
                      }
                      // settings;
                    }
                  ];
                }).config;
              # A config this minimal fails unrelated NixOS assertions (no boot
              # loader, no root filesystem), so only ours are looked at.
              ours = lib.filter (lib.hasInfix "tsnixcache");
              refusals = c: ours (map (a: a.message) (lib.filter (a: !a.assertion) c.assertions));
              execStart = c: c.systemd.services.tsnixcache.serviceConfig.ExecStart;

              refuses =
                name: settings: hint:
                let
                  got = refusals (eval settings);
                in
                lib.optional (
                  !(lib.any (lib.hasInfix hint) got)
                ) "${name}: expected a refusal mentioning ${builtins.toJSON hint}, got ${builtins.toJSON got}";
              expect = name: cond: lib.optional (!cond) name;

              key = "/etc/tsnixcache/key";
              inStore = "${builtins.storeDir}/0000000000000000000000000000000-key";
              tsnetOf = k: {
                authKeyFile = k;
                dir = "/var/lib/tsnixcache/${baseNameOf k}";
              };

              clean = eval { signKeyFile = key; };
              twoTsnet = eval {
                tsnet = [
                  (tsnetOf "/run/k0")
                  (tsnetOf "/run/k1")
                ];
              };
              emptyListen = eval {
                listen = [ "" ];
                serveCompression = "zstd";
              };
              public = eval { listen = [ "0.0.0.0:5000" ]; };
              localWrite = eval { localWrite = true; };
              localWriteNoListen = eval {
                localWrite = true;
                listen = [ "" ];
              };
              rejectsComma =
                option:
                !(builtins.tryEval (
                  builtins.deepSeq
                    (eval {
                      tsnet = [ (tsnetOf "/run/k" // { ${option} = "bad,injected=value"; }) ];
                    }).services.tsnixcache.tsnet
                    true
                )).success;

              problems = lib.concatLists [
                (refuses "tsnet.tls" { tsnet = [ (tsnetOf "/run/k" // { tls = true; }) ]; } "tls")
                (refuses "store + gc.rules" {
                  store = "/srv/c";
                  gc.rules = [
                    {
                      threshold = 80;
                      olderThan = "20d";
                    }
                  ];
                } "gc.rules")
                (refuses "store + db" {
                  store = "/srv/c";
                  db = "/srv/db.sqlite";
                } "db/storeDir/gcrootDir")
                (refuses "signKeyFile in the store" { signKeyFile = inStore; } "world-readable")
                (refuses "relative signing key" { signKeyFile = "relative-key"; } "absolute paths")
                (refuses "relative auth key" { tsnet = [ (tsnetOf "relative-key") ]; } "absolute paths")
                (refuses "authKeyFile in the store" { tsnet = [ (tsnetOf inStore) ]; } "world-readable")

                (expect "a valid config must not be refused" (refusals clean == [ ]))
                (expect "tsnet values must reject the flag delimiter" (
                  lib.all rejectsComma [
                    "hostname"
                    "controlUrl"
                    "dir"
                  ]
                ))
                (expect "the signing key must be a separate argument using the credential directory" (
                  lib.hasInfix " --sign-key-file \"%d/sign-key\"" (execStart clean)
                ))
                (expect "changing credential sources must restart the service" (
                  clean.systemd.services.tsnixcache.restartTriggers
                  != (eval { signKeyFile = "/etc/tsnixcache/other-key"; }).systemd.services.tsnixcache.restartTriggers
                ))
                (expect "a loopback listener must not warn" (ours clean.warnings == [ ]))
                (expect "a non-loopback listener must warn" (ours public.warnings != [ ]))

                # The write gate is one flag away from being off, and the only
                # thing that decides is this option reaching the command line.
                # A VM test proves the refusal on the default; nothing there
                # would notice --local-write being emitted unconditionally,
                # because every pushing test sets it.
                (expect "writes must be gated unless localWrite is set" (
                  !(lib.hasInfix "--local-write" (execStart clean))
                ))
                (expect "localWrite must reach the command line" (
                  lib.hasInfix "--local-write" (execStart localWrite)
                ))
                (expect "localWrite on a loopback listener must still warn" (ours localWrite.warnings != [ ]))
                # An empty entry emits no --listen, so there is no socket to
                # accept a push on and nothing to warn about; the flag is inert.
                (expect "localWrite with no local listener must not warn" (ours localWriteNoListen.warnings == [ ]))

                # Index-keyed credentials: the construct that breaks at two.
                (expect "two tsnet instances need two credentials" (
                  twoTsnet.systemd.services.tsnixcache.serviceConfig.LoadCredential == [
                    "tsnet-authkey-0:/run/tsnixcache-credentials/tsnet-authkey-0"
                    "tsnet-authkey-1:/run/tsnixcache-credentials/tsnet-authkey-1"
                  ]
                ))
                (expect "each tsnet spec must name its own credential" (
                  lib.hasInfix "%d/tsnet-authkey-0" (execStart twoTsnet)
                  && lib.hasInfix "%d/tsnet-authkey-1" (execStart twoTsnet)
                ))

                # An unauthenticated listener must be asked for. Configuring
                # tsnet is asking for the authenticated path, so the loopback
                # default must not be added on top of it; with no tsnet there is
                # nothing else to serve on, so it must still be there.
                (expect "tsnet alone must not add an unauthenticated local listener" (
                  !(lib.hasInfix "--listen" (execStart twoTsnet))
                ))
                (expect "without tsnet the loopback listener is the fallback" (
                  lib.hasInfix (systemdUtils.escapeSystemdExecArgs [
                    "--listen"
                    "127.0.0.1:5000"
                  ]) (execStart clean)
                ))

                # An unquoted empty value would swallow the next flag, and Go's
                # flag package stops parsing at the first non-flag argument.
                (expect "an empty listen entry must survive as an empty argument" (
                  lib.hasInfix (systemdUtils.escapeSystemdExecArgs [
                    "--listen"
                    ""
                  ]) (execStart emptyListen)
                ))
                (expect "flags after an empty value must still be there" (
                  lib.hasInfix (systemdUtils.escapeSystemdExecArgs [
                    "--serve-compression"
                    "zstd"
                  ]) (execStart emptyListen)
                ))
              ];
            in
            pkgs.runCommand "tsnixcache-module-eval" { } ''
              ${lib.optionalString (problems != [ ]) ''
                ${lib.concatMapStringsSep "\n" (p: "echo ${lib.escapeShellArg p} >&2") problems}
                exit 1
              ''}
              touch $out
            '';
        };
      }
    );
}
