# devenv.sh service definitions for local testing.
#
#   devenv up          # starts Postgres 16 on 127.0.0.1:54329 with db safe_sql_test
#   devenv shell -c 'go test -tags integration ./...'
#
# SQLite needs no service: the driver is pure Go and tests use a temp file.
{ pkgs, ... }:
{
  packages = with pkgs; [ go sqlc golangci-lint ];

  services.postgres = {
    enable = true;
    package = pkgs.postgresql_16;
    listen_addresses = "127.0.0.1";
    port = 54329;
    initialDatabases = [{ name = "safe_sql_test"; }];
    initialScript = "CREATE ROLE postgres SUPERUSER LOGIN;";
  };

  env.SAFE_SQL_TEST_DATABASE_URL = "postgres://postgres@127.0.0.1:54329/safe_sql_test?sslmode=disable";
}
