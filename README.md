# safe_sql

**A language-agnostic [strong_migrations](https://github.com/ankane/strong_migrations) for raw SQL migrations, built for [sqlc](https://sqlc.dev) projects.**

`safe_sql lint` reads your migration files and reports operations that lock
tables or break code that is already running: dropping or renaming columns,
building indexes without `CONCURRENTLY`, adding validated constraints,
volatile defaults, `SET NOT NULL` without a check constraint, and more.
`safe_sql verify` is a local, no-cloud stand-in for `sqlc verify`: it applies
your migrations to a throwaway database and proves the queries that are
deployed *today* still prepare against the new schema.

If your project already has a `sqlc.yaml`, there is nothing to configure.

```
$ safe_sql lint
db/migrations/0002_unsafe.sql:2: error: ban-drop-column
  ALTER TABLE users DROP COLUMN name

  === Dangerous operation detected #safe_sql ===

  dropping column users.name

  Running application code (and ORM/sqlc-generated code with cached column lists)
  still references the column and fails once it is gone.
  ...
```

## Install

| Method | Command |
|---|---|
| Go | `go install github.com/trishtzy/safe_sql/cmd/safe_sql@latest` |
| Binaries | [GitHub Releases](https://github.com/trishtzy/safe_sql/releases) (macOS, Linux, Windows; amd64 and arm64) |
| Nix | `nix run github:trishtzy/safe_sql -- lint` |
| Docker | `docker run --rm -v "$PWD:/work" ghcr.io/trishtzy/safe_sql lint` |
| GitHub Action | see [CI](#ci) |
| pre-commit | see [pre-commit](#pre-commit) |

## Quick start

```sh
cd my-sqlc-project          # has sqlc.yaml with engine/schema/queries
safe_sql lint               # lints every migration in the sqlc schema paths
safe_sql lint db/migrations # or point it at files/directories directly
safe_sql init               # writes a commented safe_sql.yaml
safe_sql rules              # lists every rule
```

safe_sql follows sqlc's conventions: schema paths may be files, directories
(non-recursive) or lists; files are processed in lexicographic order; and the
up/down markers of goose, golang-migrate, dbmate, tern, sql-migrate and atlas
are understood, including each tool's directive for running a file outside a
transaction (which decides whether `CREATE INDEX CONCURRENTLY` is legal).

Exit codes: `0` clean, `1` findings (errors, or warnings with `--strict`),
`2` usage or parse error.

## Rules

Every rule mirrors a strong_migrations check, translated to plain SQL. Run
`safe_sql rules` for the same table with the current binary, and read the full
guidance for a rule by triggering it.

| Rule | Engines | Severity | Description |
|---|---|---|---|
| `add-check-constraint-not-valid` | postgresql | error | Adding a check constraint scans every row while blocking reads and writes |
| `add-column-volatile-default` | postgresql | error | Adding a column with a volatile default rewrites the whole table |
| `add-foreign-key-not-valid` | postgresql | error | Adding a foreign key validates every row while blocking writes on both tables |
| `add-unique-constraint-via-index` | postgresql | error | Adding a unique constraint builds its index while blocking reads and writes |
| `ban-add-serial-column` | postgresql | error | Adding an auto-incrementing column rewrites the whole table |
| `ban-add-stored-generated-column` | postgresql | error | Adding a stored generated column rewrites the whole table |
| `ban-change-column-type` | postgresql | error | Changing a column's type rewrites the table and breaks running code |
| `ban-dml-in-migration` | postgresql,sqlite | warning | Backfilling data inside a schema migration holds locks for the whole run |
| `ban-drop-column` | postgresql,sqlite | error | Dropping a column breaks running code that still reads it |
| `ban-drop-table` | postgresql,sqlite | warning | Dropping a table destroys data and breaks code that uses it |
| `ban-exclusion-constraint` | postgresql | error | Adding an exclusion constraint checks every row while blocking reads and writes |
| `ban-rename-column` | postgresql,sqlite | error | Renaming a column breaks running code that uses the old name |
| `ban-rename-enum-value` | postgresql | error | Renaming an enum value breaks running code that uses the old value |
| `ban-rename-schema` | postgresql | error | Renaming a schema breaks running code that uses the old name |
| `ban-rename-table` | postgresql,sqlite | error | Renaming a table breaks running code that uses the old name |
| `concurrent-index-in-transaction` | postgresql | error | CREATE/DROP INDEX CONCURRENTLY fails inside a transaction |
| `index-too-many-columns` | postgresql,sqlite | warning | Non-unique indexes with more than three columns are rarely useful |
| `prefer-jsonb` | postgresql | warning | json columns lack an equality operator; use jsonb |
| `require-concurrent-index-creation` | postgresql | error | Creating an index without CONCURRENTLY blocks writes for the whole build |
| `require-concurrent-index-drop` | postgresql | error (opt-in) | Dropping an index without CONCURRENTLY blocks reads and writes (opt-in) |
| `require-lock-timeout` | postgresql | warning (opt-in) | Migration files with DDL should set lock_timeout (opt-in) |
| `set-not-null-without-check` | postgresql | error | SET NOT NULL scans the table under an exclusive lock unless a validated check exists |
| `sqlite-add-column-restrictions` | sqlite | error | ADD COLUMN forms that SQLite rejects at run time |
| `sqlite-drop-column-restrictions` | sqlite | error | DROP COLUMN fails on old SQLite or when the column is indexed, referenced, or used by a view/trigger |
| `sqlite-pragma-in-transaction` | sqlite | error | PRAGMA foreign_keys / journal_mode are silently ignored inside a transaction |
| `sqlite-rebuild-locks-database` | sqlite | warning | A table rebuild holds SQLite's single write lock for the whole copy |
| `sqlite-rename-column-version` | sqlite | error | RENAME COLUMN needs SQLite 3.25+ |
| `sqlite-table-rebuild` | sqlite | error | Table rebuild is missing a step of SQLite's documented procedure |
| `sqlite-unsupported-alter` | sqlite | error | SQLite cannot execute this ALTER TABLE form; rebuild the table instead |

Notes:

- Rules that guard *existing* tables (locks, rewrites, validation scans) do not
  fire for tables created earlier in the same migration file: nothing can be
  using such a table yet.
- `ban-change-column-type` recognises the type changes Postgres performs
  without a rewrite (widening `varchar`, `varchar` to `text`, numeric precision
  increases, `cidr` to `inet`, precision increases on timestamp/time/interval).
  It needs the column's original definition, so with a partial migration
  history every type change is reported.
- `add-column-volatile-default` knows the volatility of common functions;
  an unknown function produces a warning rather than an error.
- Version-gated advice (constant defaults on Postgres < 11, NOT NULL via check
  constraint on Postgres >= 12, DROP COLUMN on SQLite >= 3.35) uses
  `target_version`; when unset, the latest release is assumed.
- SQLite has no strong_migrations equivalent, so its rules come from SQLite's
  own ALTER TABLE restrictions: forms the engine rejects at run time (which
  sqlc happens to accept for schema evolution), the documented table-rebuild
  procedure with its transaction, foreign-key and recreate-everything steps,
  and pragmas that are silently ignored inside a transaction.

### Skipping a check

Once an operation is actually safe (the column is no longer read, the table is
tiny, ...), acknowledge it above the statement, like strong_migrations'
`safety_assured`:

```sql
-- safe_sql:disable ban-drop-column
ALTER TABLE users DROP COLUMN legacy_name;
```

`-- safe_sql:disable` with no rule names skips every rule for that statement;
`-- safe_sql:disable-file [rules]` at the top of a file covers the whole file.
Rules can also be disabled project-wide in `safe_sql.yaml`, and `start_after`
trusts every migration up to the one you were already running in production
when you adopted safe_sql.

### Adopting safe_sql on an existing project: the todo file

Like RuboCop's `.rubocop_todo.yml`, safe_sql can baseline the findings that
already exist so the build is green from day one and the backlog is fixed
over time:

```sh
safe_sql lint --generate-todo   # writes .safe_sql_todo.yaml next to safe_sql.yaml
git add .safe_sql_todo.yaml
```

The file lists, per rule and file, how many findings are known:

```yaml
ban-drop-column:
  db/migrations/0002_cleanup.sql: 1
require-concurrent-index-creation:
  db/migrations/0007_indexes.sql: 2
```

`lint`, `verify` and `fix` load it automatically and skip those findings
(the summary says how many were suppressed). Only the listed *count* is
skipped: a baselined file that gains another `DROP COLUMN` is still reported.
Paths are relative to the todo file, so it works in CI checkouts. Delete
entries as you fix them, regenerate with `--generate-todo` when the list
drifts, and pass `--no-todo` to see everything. Set `todo:` in `safe_sql.yaml`
or `--todo <path>` to keep the file elsewhere.

Compared with `start_after`, which trusts every migration up to a version, the
todo file is per finding, so new mistakes in old files, and old files never
covered, still surface.

## Configuration

Configuration is optional. Put a `safe_sql.yaml` next to (or above) your
`sqlc.yaml`; sqlc rejects unknown keys in its own file, so safe_sql keeps its
settings separate. Every key is optional; `safe_sql init` writes this file
with comments.

```yaml
version: "1"
sqlc: sqlc.yaml            # default: auto-discovered upward from the working directory
# Override or replace what sqlc.yaml says:
# engine: postgresql       # postgresql | sqlite
# schema: [db/migrations]
# queries: [db/queries]    # used by verify
migration_tool: auto       # auto | plain | goose | golang-migrate | dbmate | tern | sql-migrate | atlas
target_version: "16"       # production Postgres major (or SQLite version, e.g. "3.45")
start_after: "20260101000000"
todo: .safe_sql_todo.yaml   # baseline from `safe_sql lint --generate-todo`; this is the default location
check_down: false
plain_in_transaction: true # assumption for files whose tool cannot be detected
rules:
  disable: [prefer-jsonb]
  enable: [require-lock-timeout, require-concurrent-index-drop]   # opt-in rules
  severity: { ban-dml-in-migration: error }
verify:
  database_url: ${SAFE_SQL_DATABASE_URL}   # else a Postgres container is started
  base_ref: main
```

Precedence is CLI flags, then `safe_sql.yaml`, then `sqlc.yaml`. A sqlc config
with several `sql:` packages is linted package by package (`--package` selects
one); packages for engines safe_sql does not support are skipped with a notice.

## verify: catch queries that the migration breaks

`sqlc verify` checks that queries already deployed keep working against a
proposed schema, but needs sqlc Cloud. `safe_sql verify` does the same locally:

1. Exports the query files as they exist at `--against` (default `main`), the
   queries that are actually deployed.
2. Applies every proposed migration, statement by statement, to a scratch
   database: `--database-url` (a throwaway database is created on that server),
   or a Postgres container started through Docker. SQLite projects need
   neither: a temporary file database is used.
3. Writes a temporary `sqlc.yaml` and runs `sqlc vet` with the built-in
   `sqlc/db-prepare` rule, so sqlc's own query parsing and parameter handling
   are used unchanged.
4. Reports every query that no longer compiles or prepares, attributed to the
   new migration most likely responsible, and lints the new migrations.

```sh
safe_sql verify --against origin/main
safe_sql verify --database-url postgres://localhost/dev   # no Docker needed
```

Requires `git` and the `sqlc` binary on `PATH` (`--sqlc-path` to override).
Like sqlc verify, this checks that queries prepare, not their runtime results.

## CI

```yaml
# .github/workflows/migrations.yml
on: pull_request
jobs:
  safe_sql:
    runs-on: ubuntu-latest
    permissions: { contents: read }
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: trishtzy/safe_sql@v1
        # with:
        #   verify: "true"          # run verify instead of lint (needs sqlc; starts Postgres in Docker)
        #   args: --target-version 16
```

Findings appear as annotations on the changed lines. By default only
migrations changed in the pull request are reported (every file still feeds
the schema model); set `changed-only: "false"` to report everything.

### AI auto-fix from a pull request comment

With `ai.enabled: true` in `safe_sql.yaml`, a maintainer can comment
`@safe_sql_ai` on a pull request and safe_sql rewrites the flagged migrations
in that PR: deterministic fixes first (`CONCURRENTLY`, `NOT VALID` plus a
validate migration, unique-via-index, `jsonb`), then a Claude model for the
patterns that need judgement (rename via a new column, batched backfills,
`NOT NULL` through a validated check constraint). Every proposal is validated
(only migration directories, only files with findings or new versioned files,
must parse, no smuggled `safe_sql:disable`) and re-linted before delivery.

```yaml
# .github/workflows/safe_sql_ai.yml
on:
  issue_comment:
    types: [created]
concurrency: safe_sql-ai-${{ github.event.issue.number }}
jobs:
  ai-fix:
    if: github.event.issue.pull_request
    runs-on: ubuntu-latest
    permissions: { contents: write, pull-requests: write }
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: trishtzy/safe_sql@v1
        with: { mode: ai-fix }
        env:
          ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
```

```yaml
# safe_sql.yaml
ai:
  enabled: true
  trigger: "@safe_sql_ai"
  mode: commit               # commit to the PR branch | suggest (review suggestions) | comment (diffs)
  model: claude-opus-5
  max_iterations: 3
  allowed_associations: [OWNER, MEMBER, COLLABORATOR]
  rules: []                  # restrict which findings the model may fix
```

Safety properties: the gate (`enabled`, `trigger`, `allowed_associations`) is
read from `safe_sql.yaml` on the base branch, so a pull request cannot enable
the feature or widen who may trigger it; the API key and any gateway URL come
only from the workflow environment; the model never executes anything, it
proposes file contents that safe_sql validates. The same flow runs locally
with `safe_sql fix --ai` (or `safe_sql fix` for deterministic fixes only,
`--dry-run` to preview). A commit pushed with `GITHUB_TOKEN` does not start
new workflow runs, so the result is re-linted in the same job and summarised
in a comment.

### pre-commit

```yaml
repos:
  - repo: https://github.com/trishtzy/safe_sql
    rev: v1.0.0
    hooks:
      - id: safe_sql
```

## How it relates to other tools

- **strong_migrations** works inside Rails migrations and can also enforce
  timeouts and retries at runtime. safe_sql is static: it reads SQL files, so
  it works with any language, but cannot inject `lock_timeout` for you (the
  opt-in `require-lock-timeout` rule reminds you to).
- **[squawk](https://github.com/sbdchd/squawk)** is an excellent Postgres-only
  migration linter with an overlapping rule set. safe_sql adds sqlc.yaml
  discovery and migration-tool conventions, the local `verify` mode, and SQLite
  rules.
- **sqlc vet** lints queries against the current schema; **sqlc verify** (cloud)
  checks deployed queries against a proposed schema. `safe_sql verify` is the
  local equivalent of the latter and uses `sqlc vet` underneath.

## Development

```sh
nix develop                 # go, sqlc, golangci-lint, goreleaser, go-licenses
go test ./...
devenv up                   # Postgres 16 on :54329 for the integration tests
SAFE_SQL_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:54329/safe_sql_test?sslmode=disable' \
  go test -tags integration ./...
```

The design and phase plan are in [docs/PLAN.md](docs/PLAN.md).

## License

MIT. Third-party licenses are listed in `THIRD_PARTY_LICENSES` in every
release archive; the Postgres parser is
[sqlc-dev/oliphant](https://github.com/sqlc-dev/oliphant) (MIT, with ported
PostgreSQL-licensed grammar) and the SQLite parser is
[sqlc-dev/meyer](https://github.com/sqlc-dev/meyer) (MIT).
