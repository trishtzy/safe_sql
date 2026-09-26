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
          # Update after changing go.mod: set to "" (or lib.fakeHash), run
          # `nix build`, and copy the hash from the "got:" line.
          vendorHash = "sha256-LKDVO/OnoxBMf9LM/948JINGH8twMCpYxNqtARrzQIk=";
          ldflags = [ "-s" "-w" "-X github.com/trishtzy/safe_sql/internal/cli.Version=${self.shortRev or "dirty"}" ];
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
          default = {
            type = "app";
            program = "${safe_sql}/bin/safe_sql";
            meta.description = "Lint SQL migrations for unsafe operations";
          };
        };

        formatter = pkgs.nixfmt;
      });
}
