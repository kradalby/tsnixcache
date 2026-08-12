# Copyright (c) 2026 Kristoffer Dalby
# SPDX-License-Identifier: BSD-3-Clause

{
  description = "tsnixcache — Nix binary cache over local + tsnet";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    flake-checks.url = "github:kradalby/flake-checks";
    flake-checks.inputs.nixpkgs.follows = "nixpkgs";
    # Headscale is used as the Tailscale control plane in tsnet integration
    # tests. Deliberately NOT following our nixpkgs, unlike every other input
    # here: headscale v0.29.2's go.mod requires a newer Go than nixpkgs-unstable
    # currently ships, so the follows builds it against go 1.26.3 and it dies
    # with "go.mod requires go >= 1.26.4", taking checks.tsnet with it. It pins
    # a nixpkgs that can build it, and it is the one input nothing outside
    # checks.tsnet evaluates, so the cost of the extra lock entry falls on this
    # repo rather than on consumers. Revisit once nixpkgs catches up.
    headscale.url = "github:juanfont/headscale/v0.29.2";
    # nix-darwin provides the launchd-based client module for macOS.
    nix-darwin.url = "github:nix-darwin/nix-darwin";
    nix-darwin.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs = { nixpkgs, flake-utils, flake-checks, ... }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        fc = flake-checks.lib;
        common = {
          inherit pkgs;
          version = "0.1.0";
          root = ./.;
          pname = "tsnixcache";
          vendorHash = (builtins.fromJSON (builtins.readFile ./flakehashes.json)).vendor.sri;
          goPkg = pkgs.go_1_26;
          env = { CGO_ENABLED = "0"; };
        };
      in
      {
        formatter = fc.formatter common;

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go_1_26
            golangci-lint
            gofumpt
            gotools
            gotestsum
            treefmt
            nixpkgs-fmt
            nix
            xz
            zstd
            pre-commit
          ];
        };

        checks = {
          build = fc.goBuild common;
          gotest = fc.goTest (common // { goRace = true; testFlags = [ "-short" ]; });
          golangci-lint = fc.goLint common;
          formatting = fc.goFormat common;
        };
      });
}
