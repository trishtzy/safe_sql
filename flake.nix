{
  description = "safe_sql — a language-agnostic strong_migrations for raw SQL migrations, sqlc-aware";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        lib = pkgs.lib;

        # The Go package only exists once go.mod is present (Phase 1 of the plan).
        # Until then `nix flake check` and `nix develop` still work.
        hasGoMod = builtins.pathExists ./go.mod;

        safe_sql = pkgs.buildGoModule {
          pname = "safe_sql";
          version = "0.0.0-dev";
          src = lib.cleanSource ./.;
          subPackages = [ "cmd/safe_sql" ];
          # Replace with the real hash once go.sum exists:
          #   nix build 2>&1 | grep 'got:'  -> copy the sha256 here
          vendorHash = null;
          ldflags = [ "-s" "-w" "-X main.version=${self.shortRev or "dirty"}" ];
          meta = with lib; {
            description = "Lint SQL migrations for operations that lock tables or break running apps";
            homepage = "https://github.com/trishtzy/safe_sql";
            license = licenses.mit;
            mainProgram = "safe_sql";
          };
        };
      in
      {
        devShells.default = pkgs.mkShell {
          name = "safe_sql-dev";
          packages = with pkgs; [
            go
            gopls
            gotools
            golangci-lint
            goreleaser
            go-licenses
            sqlc          # required by `safe_sql verify`
            pre-commit
            docker-client # testcontainers-go in verify integration tests
            git
          ];
          shellHook = ''
            export GOFLAGS="-mod=mod"
            echo "safe_sql dev shell: $(go version)"
          '';
        };

        packages = lib.optionalAttrs hasGoMod {
          default = safe_sql;
          safe_sql = safe_sql;
        };

        apps = lib.optionalAttrs hasGoMod {
          default = flake-utils.lib.mkApp { drv = safe_sql; };
        };

        formatter = pkgs.nixfmt;
      });
}
