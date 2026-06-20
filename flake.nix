{
  description = "tsnixcache — Nix binary cache over local + tsnet";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    let
      tsnixcacheModule = import ./nix/module-server.nix;
      tsnixcacheClientModule = import ./nix/module-client.nix;
    in
    {
      nixosModules = {
        tsnixcache = tsnixcacheModule;
        tsnixcache-client = tsnixcacheClientModule;
        default = tsnixcacheModule;
      };
    } // flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        buildGo126Module = pkgs.buildGoModule.override { go = pkgs.go_1_26; };
        tsnixcache = buildGo126Module {
          pname = "tsnixcache";
          version = "0.1.0";
          src = ./.;
          vendorHash = (builtins.fromJSON (builtins.readFile ./flakehashes.json)).vendor.sri;
          subPackages = [ "cmd/tsnixcache" ];
          checkFlags = [ "-short" ];
          env.CGO_ENABLED = "0";
          meta = {
            description = "Nix binary cache that serves from /nix/store and accepts pushes via standard HTTP protocol";
            homepage = "https://github.com/kradalby/tsnixcache";
            license = pkgs.lib.licenses.mit;
            mainProgram = "tsnixcache";
          };
        };
      in
      {
        packages = {
          inherit tsnixcache;
          default = tsnixcache;
        };

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go_1_26
            golangci-lint
            gofumpt
            golines
            gotestsum
            nixpkgs-fmt
            nix
            xz
            zstd
            # for pre-commit
            pre-commit
          ];
        };

        checks = {
          build = tsnixcache;

          lint = pkgs.runCommandNoCCLocal "tsnixcache-lint"
            {
              src = ./.;
              nativeBuildInputs = [ pkgs.golangci-lint pkgs.go_1_26 ];
            } ''
            export HOME=$(mktemp -d)
            export GOPATH=$(mktemp -d)
            cd $src
            golangci-lint run ./...
            touch $out
          '';

          serve = pkgs.testers.nixosTest (import ./nix/tests/serve.nix { inherit pkgs tsnixcache tsnixcacheModule; });
          push = pkgs.testers.nixosTest (import ./nix/tests/push.nix { inherit pkgs tsnixcache tsnixcacheModule; });
          watch = pkgs.testers.nixosTest (import ./nix/tests/watch.nix { inherit pkgs tsnixcache tsnixcacheModule; });
        };
      });
}
