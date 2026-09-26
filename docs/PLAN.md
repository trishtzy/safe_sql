# safe_sql — language-agnostic strong_migrations, sqlc-aware

## Context

strong_migrations (Ruby) catches migrations that lock tables or break running app code, but it only
works inside Rails. Teams using sqlc (Go/Kotlin/Python/TS) write plain SQL migrations and have no
equivalent guard. `sqlc verify` covers one slice of the problem (existing queries breaking against a
new schema) but requires sqlc Cloud.

`safe_sql` is a standalone CLI that (1) statically lints raw SQL migration files with the
strong_migrations rule set, and (2) offers a local, no-cloud stand-in for `sqlc verify`. It reads an
existing `sqlc.yaml` so sqlc projects need zero extra setup.

**Prior art**: [squawk](https://github.com/sbdchd/squawk) is a Rust Postgres-only migration linter that
already covers much of the static-lint half. This project differentiates on: sqlc.yaml discovery and
migration-tool conventions, SQLite rules (no existing linter covers SQLite's ALTER TABLE
restrictions and the table-rebuild procedure), the local `verify` command, and AI auto-fix. README will say so.

## Decisions (confirmed with user)

| Decision | Choice |
|---|---|
| Language | Go |
| Engines | Postgres full rule set in v1; SQLite rules as the last phase of this plan; MySQL/MariaDB out of scope (user changed from MySQL to SQLite) |
| sqlc compat | sqlc.yaml discovery + migration-tool conventions + local `verify` mode |
| Distribution | GitHub Releases binaries, GitHub Action, pre-commit hook, Docker image, Nix flake |
| AI auto-fix | `@safe_sql_ai` PR-comment trigger fixes findings in the same PR; fully configurable under `ai:` in safe_sql.yaml; off by default |

## Licensing of reused code (checked 2026-09-26)

| Dependency | License | How we use it | Obligation |
|---|---|---|---|
| `github.com/sqlc-dev/oliphant` | MIT (© The sqlc Authors) + bundled `LICENSE.POSTGRESQL` (PostgreSQL License) and `LICENSE.LIBPG_QUERY` (BSD-3) for the ported grammar/test corpora | Go module import, unmodified | Ship the copyright + permission notices with binaries |
| `github.com/sqlc-dev/meyer` | MIT (© The sqlc Authors) | Go module import, unmodified | Ship the notice |
| `github.com/ncruces/go-sqlite3` | MIT (© Nuno Cruces); embeds SQLite itself, which is public domain | Go module import, unmodified | Ship the notice |
| `github.com/sqlc-dev/sqlc` | MIT (© Riza, Inc.) | Not imported in v1. `verify` shells out to the user's own `sqlc` binary; only its up/down marker logic is re-implemented (a few lines, fine under MIT with attribution) | Attribute in THIRD_PARTY_LICENSES |
| `github.com/anthropics/anthropic-sdk-go` | MIT | Go module import for the AI fix feature | Same |

All are permissive and compatible with each other and with releasing safe_sql under **MIT**
(recommended; Apache-2.0 would also work). Concretely: add `LICENSE` (MIT), generate
`THIRD_PARTY_LICENSES` with `go-licenses` in the release workflow, and include it in the release
archives, the Docker image, and the Nix package. GitHub labels oliphant "Other" only because its
LICENSE file appends the two ported-code notices after the MIT text.

## Defaults set by me (not asked; stated here so they can be overridden)

- **Parsers**: `github.com/sqlc-dev/oliphant` (pure-Go, no-cgo drop-in for pg_query_go, pinned to
  libpg_query 18) for Postgres; `github.com/sqlc-dev/meyer` (pure-Go hand-written SQLite parser,
  returns SQLite's exact error messages) for SQLite. Both are what sqlc itself now uses. No cgo
  anywhere, so goreleaser cross-compiles cleanly. Both libraries are young: pin exact versions in
  go.mod. oliphant is a declared drop-in for `pganalyze/pg_query_go/v6`, so the fallback if it
  misbehaves is a one-line import swap (at the cost of cgo).
- **SQLite driver** (verify mode only): `github.com/ncruces/go-sqlite3` (pure Go via wazero, MIT,
  also sqlc's choice). SQLite verify therefore needs no Docker and no external service.
- **sqlc internals are not importable** (`internal/`). Re-implement the tiny pieces we need: sqlc.yaml
  v2 struct (subset), `Paths` file-or-list unmarshalling, down-marker stripping (mirror of
  `internal/migrations/migrations.go`), lexicographic file ordering.
- **Config**: standalone `safe_sql.yaml`. sqlc's v2 loader uses `yaml.Decoder.KnownFields(true)`
  (verified in `internal/config/v_two.go`), so extra keys in sqlc.yaml would break `sqlc generate`.
- **Disable annotation**: `-- safe_sql:disable <rule> [<rule>...]` comment immediately before a
  statement (mirrors `@sqlc-vet-disable`). Bare `-- safe_sql:disable` disables all rules for that
  statement. `-- safe_sql:disable-file` at top of file for the whole file. This is the
  `safety_assured` equivalent.
- **v1 is lint-only**: no migration-runner wrapper that injects `lock_timeout`/retries. A static tool
  cannot do that portably across migration tools. Instead, an opt-in rule `require-lock-timeout`
  warns when a migration file that contains DDL has no `SET lock_timeout` / `SET LOCAL lock_timeout`.
- **Exit codes**: 0 clean, 1 findings, 2 usage/parse error. Output: human (default, grouped by file
  with `file:line: rule: message` header plus the strong_migrations-style guidance block), `--format
  json`, `--format github` (workflow annotations).
- **Severity**: `error` (blocks) and `warning` (does not affect exit code unless `--strict`).

## Architecture

```
safe_sql/
  cmd/safe_sql/main.go              # cobra root: lint, verify, fix, rules, init, version
  internal/config/                   # safe_sql.yaml + sqlc.yaml discovery, merged Project struct
  internal/fix/                      # deterministic fixers (one per fixable rule) + AI fixer loop
    ai/                              #   Provider interface, anthropic impl, prompt builder, output validation
  internal/github/                   # PR comment / review-suggestion / commit helpers for the Action
  internal/migrate/                  # file discovery, ordering, tool detection, up/down split,
                                     #   transaction-mode detection, start_after filtering
  internal/sqlparse/                 # engine-neutral Statement model + per-engine parsers
    model.go                         #   Statement{Kind, Table, Columns, Raw, Line, Comments, ...}
    postgres/                        #   oliphant AST -> model
    sqlite/                          #   meyer AST -> model
  internal/rules/                    # Rule interface, registry, per-engine rule files
    rule.go                          #   type Rule struct{ ID, Engines, Severity, Check func(ctx, stmt) []Finding }
    postgres_*.go, common_*.go, sqlite_*.go
  internal/lint/                     # runs rules over parsed migrations, applies disables, collects Findings
  internal/verify/                   # local verify mode (ephemeral DB + prepare queries)
  internal/report/                   # human / json / github formatters
  testdata/                          # golden tests: <case>/{migrations/, sqlc.yaml, safe_sql.yaml, expected.json}
  action.yml                         # GitHub Action (composite, downloads release binary)
  .pre-commit-hooks.yaml
  Dockerfile, flake.nix, .goreleaser.yaml
```

### Statement model (engine-neutral)

Rules never touch raw parser ASTs. Each parser produces `[]Statement` with a small typed union:
`CreateTable`, `AlterTable{Actions []AlterAction}`, `CreateIndex`, `DropIndex`, `RenameTable`,
`RenameColumn`, `AlterType` (enum), `AlterSchema`, `DML{Kind: update|insert|delete}`, `Set`,
`Pragma{Name, Value}`, `CreateView`, `CreateTrigger`, `DropView`, `DropTrigger`,
`Transaction{Begin|Commit}`, `Other{Raw}`. `AlterAction` covers `AddColumn`, `DropColumn`,
`AlterColumnType`, `SetNotNull`, `DropNotNull`, `SetDefault`, `AddConstraint{Kind, NotValid}`,
`ValidateConstraint`, `Unsupported{Raw}` (forms the engine cannot execute, used by SQLite rules).
Each carries `Line` and the raw SQL span. The catalog tracks views and triggers per table because
SQLite's DROP COLUMN and rebuild rules depend on them.

### Rule context

`Context{Engine, TargetVersion, InTransaction bool, MigrationTool, Filename, Version string,
SchemaSoFar *Catalog}`. `SchemaSoFar` is a lightweight catalog built from earlier migration files
(tables + columns + whether a table was created in the *same* file). Several rules are only dangerous
on *existing* tables; creating a table then adding an index to it in the same migration is fine.

## Rule table (strong_migrations check → safe_sql rule)

Rule IDs are kebab-case, grouped by engine. "PG≥N" marks a rule whose behaviour depends on
`target_version`: the safe alternative only exists on Postgres N or later, so below N the rule fires
differently or the suggested fix changes. When `target_version` is unset, assume the latest release.

### Common (Postgres + SQLite)

| strong_migrations check | Rule ID | Trigger | Guidance |
|---|---|---|---|
| removing a column | `ban-drop-column` | `ALTER TABLE .. DROP COLUMN` | Deploy code that stops reading the column first; then drop. |
| changing column type | `ban-change-column-type` | `ALTER COLUMN .. TYPE` (PG). SQLite has no such statement; the equivalent table-rebuild pattern is handled by `sqlite-table-rebuild` below. PG exceptions that don't rewrite: varchar(n)→text/wider varchar, numeric precision increase, cidr→inet, interval/timestamp precision increase, text↔citext etc. Encode the strong_migrations safe-list, minus the timestamp→timestamptz case (depends on server timezone, which static analysis cannot see; document it as always flagged). | Add new column, backfill, swap. |
| renaming a column | `ban-rename-column` | `RENAME COLUMN` | Add new column, dual-write, backfill, drop old. |
| renaming a table | `ban-rename-table` | `ALTER TABLE .. RENAME TO` | Create new table + view, or dual-write. |
| creating a table with force | `ban-drop-table` | `DROP TABLE x` followed by `CREATE TABLE x` in the same file is the exact `force: true` equivalent → `error`. A bare `DROP TABLE` is **stricter than strong_migrations** (squawk bans it too) → `warning`. | |
| adding auto-increment column | `ban-add-serial-column` | `ADD COLUMN .. serial/bigserial/GENERATED .. AS IDENTITY` on existing table (PG). SQLite: `ADD COLUMN .. PRIMARY KEY` is a runtime error, reported by `sqlite-add-column-restrictions`. | Rewrites whole table. |
| adding stored generated column | `ban-add-stored-generated-column` | `ADD COLUMN .. GENERATED ALWAYS AS (..) STORED` on existing table | Rewrites whole table. |
| adding a foreign key | `add-foreign-key-not-valid` | `ADD CONSTRAINT .. FOREIGN KEY` without `NOT VALID` (PG). SQLite cannot add constraints via ALTER; see `sqlite-unsupported-alter`. | PG: add `NOT VALID`, then `VALIDATE CONSTRAINT` in a later migration. |
| adding a check constraint | `add-check-constraint-not-valid` | `ADD CONSTRAINT .. CHECK` without `NOT VALID` (PG). SQLite: same as above. | Same pattern. |
| executing SQL directly | *(dropped)* | Every statement is raw SQL here; not meaningful. | |
| backfilling data | `ban-dml-in-migration` | `UPDATE`/`INSERT..SELECT`/`DELETE` in a migration file that also contains DDL, or any DML wrapped in a transaction with DDL | Backfill in batches outside the migration/transaction. `warning` by default. |
| non-unique index > 3 cols (best practice) | `index-too-many-columns` | `CREATE INDEX` (non-unique) with >3 columns | `warning`. |

### Postgres

| strong_migrations check | Rule ID | Trigger |
|---|---|---|
| adding index non-concurrently | `require-concurrent-index-creation` | `CREATE INDEX` without `CONCURRENTLY` on a table not created in this file. Companion `concurrent-index-in-transaction` (error) when `CONCURRENTLY` is used while the file runs in a transaction. |
| adding a reference | *(covered)* | = FK rule + index rule. |
| adding a unique constraint | `add-unique-constraint-via-index` | `ADD CONSTRAINT .. UNIQUE (cols)`; suggest `CREATE UNIQUE INDEX CONCURRENTLY` then `ADD CONSTRAINT .. UNIQUE USING INDEX`. |
| adding an exclusion constraint | `ban-exclusion-constraint` | `ADD CONSTRAINT .. EXCLUDE` on existing table. |
| adding a json column | `prefer-jsonb` | column type `json` in ADD COLUMN or CREATE TABLE (`warning`). |
| column with volatile default | `add-column-volatile-default` | `ADD COLUMN .. DEFAULT <expr>` where expr is volatile (`gen_random_uuid()`, `random()`, `now()` is *stable* so allowed, `clock_timestamp()` volatile). Constant defaults: safe on PG≥11, error on <11 (`target_version`). Maintain a volatile-function list; unknown functions → warning. |
| setting NOT NULL | `set-not-null-without-check` | `ALTER COLUMN .. SET NOT NULL` unless a validated `CHECK (col IS NOT NULL)` constraint exists in the catalog (PG≥12). |
| renaming enum value | `ban-rename-enum-value` | `ALTER TYPE .. RENAME VALUE`. |
| renaming schema | `ban-rename-schema` | `ALTER SCHEMA .. RENAME`. |
| removing index non-concurrently (opt-in) | `require-concurrent-index-drop` | `DROP INDEX` without `CONCURRENTLY`; disabled by default. |
| *(new, static-only)* | `require-lock-timeout` | opt-in; file with DDL has no `SET [LOCAL] lock_timeout`. |

### SQLite (final phase)

strong_migrations has no SQLite checks, so this set is derived from SQLite's own documented
restrictions (sqlite.org/lang_altertable.html, "Making Other Kinds Of Table Schema Changes", the
12-step procedure) and its single-writer locking model. "SQLite≥x.y" gates on `target_version`,
which for SQLite is the version linked into the application's driver.

| Hazard | Rule ID | Trigger | Guidance |
|---|---|---|---|
| ALTER form SQLite does not support | `sqlite-unsupported-alter` | `ALTER COLUMN`, `ADD/DROP CONSTRAINT`, `MODIFY`, `ALTER TABLE .. ADD PRIMARY KEY`, etc. (meyer rejects these; we report the parse error as a finding, not a crash) | Use the 12-step rebuild; `fix` can scaffold it. |
| ADD COLUMN forms that fail at runtime | `sqlite-add-column-restrictions` | `ADD COLUMN` with `PRIMARY KEY`, `UNIQUE`, non-constant `DEFAULT` (`CURRENT_TIMESTAMP`, expressions), `NOT NULL` without a non-NULL default, `GENERATED .. STORED`, or `REFERENCES` with a non-NULL default | Rebuild, or relax the column definition. |
| DROP COLUMN that fails at runtime | `sqlite-drop-column-restrictions` | SQLite<3.35 at all; or the column is PK/UNIQUE, indexed, FK-referenced, or used in a view, trigger, or generated column (from the catalog) | Drop dependents first or rebuild. |
| RENAME COLUMN on old SQLite | `sqlite-rename-column-version` | `RENAME COLUMN` with SQLite<3.25 | Rebuild. |
| PRAGMA inside a transaction is silently ignored | `sqlite-pragma-in-transaction` | `PRAGMA foreign_keys` / `journal_mode` inside `BEGIN..COMMIT` or in a file the migration tool wraps in a transaction (goose default, etc.) | Move it before `BEGIN`, or use the tool's no-transaction directive with explicit `BEGIN`/`COMMIT`. `error`. |
| Incomplete table rebuild | `sqlite-table-rebuild` | Pattern `CREATE TABLE new_x` + `INSERT INTO new_x SELECT .. FROM x` + `DROP TABLE x` + `ALTER TABLE new_x RENAME TO x` in one file. Fires when: not in a transaction; `PRAGMA foreign_keys=OFF` missing or misplaced; indexes/triggers/views that existed on `x` are not recreated; no `PRAGMA foreign_key_check` before commit | Emit the missing steps. `error`. |
| Rebuild blocks all writers | `sqlite-rebuild-locks-database` | Same pattern detected, well-formed | `warning`: SQLite is single-writer; the copy holds the write lock for its duration. Consider doing it during a maintenance window. |
| Common rules with SQLite triggers | `ban-drop-column`, `ban-rename-column`, `ban-rename-table`, `ban-drop-table`, `ban-dml-in-migration`, `index-too-many-columns` | Same app-breaking rationale as Postgres; DROP COLUMN additionally gated on SQLite≥3.35 | |

Postgres-only rules (`require-concurrent-*`, `set-not-null-without-check`, `prefer-jsonb`, enum and
schema renames, volatile defaults) do not register for the sqlite engine.

`safe_sql rules [--engine postgresql]` prints this table with descriptions and default severities.

## Configuration

### `safe_sql.yaml` (all keys optional)

```yaml
version: "1"
sqlc: sqlc.yaml            # path to sqlc config; default: auto-find sqlc.yaml|sqlc.json in cwd/parents
# when there is no sqlc.yaml, or to override it:
engine: postgresql         # postgresql | sqlite
schema: [db/migrations]    # same file-or-dir-or-list semantics as sqlc `schema`
queries: [db/queries]      # only used by `verify`
migration_tool: auto       # auto | goose | golang-migrate | dbmate | tern | sql-migrate | atlas | plain
target_version: "16"       # PG major, or SQLite "3.35"-style version linked into your driver
start_after: "20260101000000"   # filenames whose leading version <= this are skipped
check_down: false          # lint down sections too (strong_migrations default false)
rules:
  disable: [prefer-jsonb]
  enable: [require-concurrent-index-drop, require-lock-timeout]
  severity: { ban-dml-in-migration: error }
verify:
  database_url: ${SAFE_SQL_DATABASE_URL}   # ${ENV} substitution like sqlc
  docker: true             # postgres: start postgres:<target_version> via testcontainers-go
                           # sqlite: ignored; a temp file database is always used
  base_ref: main           # git ref whose migrations define the "deployed" schema
ai:
  enabled: false           # master switch; the Action refuses to run AI fixes when false
  trigger: "@safe_sql_ai"  # PR-comment mention that starts a fix run
  auto: false              # true = also run after every failing lint, without a mention
  mode: commit             # commit | suggest | comment  (see AI section)
  provider: anthropic      # only provider in v1
  model: claude-opus-5     # any Claude model id; ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL come from the environment, never from this file
  max_iterations: 3        # fix -> re-lint loop bound
  rules: []                # restrict AI fixing to these rule IDs; empty = all fixable rules
  deterministic_first: true  # apply non-AI fixers before asking the model
  allow_disable_annotations: false  # forbid the model from "fixing" by adding safe_sql:disable
  allowed_associations: [OWNER, MEMBER, COLLABORATOR]  # who may trigger it from a PR comment
  commit_message: "safe_sql: rewrite migration using a safer pattern"
```

### sqlc.yaml discovery

Parse only what we need with a lenient decoder: `version`, `sql[]{engine, schema, queries, database.uri}`.
Multiple `sql[]` packages → lint each; `--package <name>` selects one. Engines other than
postgresql/sqlite are skipped with a notice.

## Migration file handling (`internal/migrate`)

1. Discover files exactly like sqlc: file, directory (non-recursive, `*.sql`), or list. Sort
   lexicographically. Skip `*.down.sql`.
2. Detect tool per file from markers (`-- +goose Up`, `-- +migrate Up`, `-- migrate:up`,
   `---- create above / drop below ----`, atlas `-- Create "x" table` comments, else `plain`).
3. Split into up/down sections. Lint up only unless `check_down`.
4. Extract the version prefix (`^\d+`) for `start_after` and for `verify`'s "new vs deployed" split.
   **`start_after` and `--changed-since` gate reporting, not parsing**: every file is always parsed
   and fed into `SchemaSoFar`, because rules like `set-not-null-without-check` and the
   "table created in this file" exemption depend on earlier files. Only files after the cutoff
   produce findings.
5. Determine `InTransaction` per file. **Verify each of these against the tool's docs during
   implementation; do not trust memory**: goose (`-- +goose NO TRANSACTION`), dbmate
   (`-- migrate:up transaction:false`), tern (`---- tern: disable-tx ----`), sql-migrate
   (`-- +migrate Up notransaction`), atlas (`-- atlas:txmode none`), golang-migrate (no wrapping
   directive; whole file sent as one multi-statement exec, which Postgres runs as an implicit
   transaction unless it is a single statement). `plain` → assume transaction, overridable in config.
6. Strip psql meta-commands like sqlc does.

## `verify` command (local stand-in for `sqlc verify`)

```
safe_sql verify [--against <git-ref>] [--database-url ...]
```

1. Resolve migration files at `--against` (default `verify.base_ref`, default `main`) using
   `git show <ref>:<path>` — these are the "deployed" migrations. Current working tree = proposed.
2. Get a database. Postgres: `--database-url` if given, else start an ephemeral container with
   `testcontainers-go` (`postgres:<target_version>`); fail with a clear message if neither is
   available. SQLite: create a temp file database with `ncruces/go-sqlite3`, always; the sqlc
   `database.uri` becomes `file:<path>`.
3. Apply *all* proposed up migrations in order (raw exec per statement, using our up/down split).
   This is the step sqlc itself does not do ("sqlc does not apply schema migrations to your database").
4. **Delegate query preparation to sqlc instead of re-implementing it.** sqlc's query splitting and
   parameter rewriting (`$1`, `?`, `@name`, `sqlc.arg/narg/slice/embed`, `:copyfrom`/`:batch*`
   handling) is its hardest code and not importable. But `sqlc vet` with the built-in
   `sqlc/db-prepare` rule prepares every query against `database.uri` (verified in
   `internal/cmd/vet.go`). So: check out the `--against` `queries` paths into a temp dir (`git
   archive <ref> -- <paths>`), write a temp `sqlc.yaml` whose `queries:` points there, `schema:`
   points at the proposed migrations, `database.uri` at the ephemeral DB, and `rules: [sqlc/db-prepare]`;
   then run `sqlc vet -f <temp>`.
   - Invocation: shell out to a `sqlc` binary on `PATH` (the tool targets sqlc users, so this is a
     fair requirement for `verify` only; `lint` never needs it). `--sqlc-path` overrides.
   - Alternative kept in reserve: `import "github.com/sqlc-dev/sqlc/pkg/cli"` and call
     `cli.Run([]string{"vet","-f",tmp})` — exported and verified, but drags in sqlc's full dependency
     tree (grpc, protobuf, wazero, cel, every engine parser). Not in v1.
5. Parse sqlc's vet output (one line per failing query: file, query name, error) and re-emit it in
   safe_sql's formats, adding the migration file most likely responsible (the last proposed file
   touching a table named in the query text). Exit non-zero on any failure.
6. Also run lint on the *new* files only (diff of `--against` vs working tree) so `verify` is a
   superset for CI.

Scope limit (stated in README): `verify` checks prepare-ability, not runtime semantics; same as
sqlc verify's own limit. `sqlc vet` also honors `@sqlc-vet-disable` annotations in query files, which
carries over for free.

## `fix` command and the `@safe_sql_ai` PR flow

### Two tiers of fixes

**Tier 1, deterministic (no API key, always available via `safe_sql fix`):** rules whose safe form
is a mechanical rewrite of the same statement:
`require-concurrent-index-creation` (add `CONCURRENTLY`, and if the file runs in a transaction, move
the statement into a new migration file with the tool's no-transaction directive),
`require-concurrent-index-drop`, `add-foreign-key-not-valid` and `add-check-constraint-not-valid`
(append `NOT VALID`, create a follow-up migration with `VALIDATE CONSTRAINT`),
`add-unique-constraint-via-index` (split into `CREATE UNIQUE INDEX CONCURRENTLY` + `ADD CONSTRAINT
... UNIQUE USING INDEX`), `prefer-jsonb`. SQLite: `sqlite-pragma-in-transaction` (move the PRAGMA
out of the transaction using the tool's directive), `sqlite-table-rebuild` (append the missing
index/trigger/view recreations and `PRAGMA foreign_key_check`, all derivable from the catalog),
`sqlite-unsupported-alter` for `ADD CONSTRAINT`/`ALTER COLUMN TYPE` (scaffold the full 12-step
rebuild from the catalog's table definition).
Each fixer is a function next to its rule: `Fix(ctx, stmt, catalog) ([]FileEdit, bool)`.

**Tier 2, AI (`safe_sql fix --ai`):** rules whose fix is a multi-step expansion that needs judgement
about the schema and naming: `ban-rename-column`, `ban-rename-table`, `ban-change-column-type`,
`set-not-null-without-check`, `add-column-volatile-default`, `sqlite-add-column-restrictions`,
`sqlite-drop-column-restrictions`, `ban-add-serial-column`, `ban-add-stored-generated-column`, `ban-rename-enum-value`,
`ban-dml-in-migration` (batching), `ban-drop-column` (comment-only: it cannot know whether the app
still reads the column, so it explains the deploy-then-drop sequence and does not remove the DROP).

### AI fixer loop (`internal/fix/ai`)

1. Build the prompt: engine + target_version; migration tool and its markers; the version-number
   scheme inferred from existing filenames (so new files sort correctly); the offending file; the
   `SchemaSoFar` summary for tables it touches; each finding with its rule guidance (the same text
   the human report shows); the strong_migrations "Good" pattern for that rule as SQL.
   User-controlled SQL is wrapped in a delimited block and labelled as data, not instructions.
2. Call the model with a single tool `propose_migration_files` whose schema is
   `{files: [{path, action: modify|create, content}], summary: string}` so the output is structured,
   never free text. Anthropic SDK: `github.com/anthropics/anthropic-sdk-go`.
3. Validate: every path must be under a configured `schema` directory; `modify` only for the
   offending file; `create` only with a version prefix greater than the current max; content must
   parse with our parser; `safe_sql:disable` annotations rejected unless `allow_disable_annotations`.
4. Apply to a temp copy, re-run lint on the affected files. If findings remain and iterations are
   left, send the remaining findings back and repeat. Stop at `max_iterations`.
5. Return the edits and a summary; `--dry-run` prints the diff, otherwise writes files.
   `Provider` is an interface so tests use a scripted fake; a `record`/`replay` fixture set covers
   the prompt builder and validator without network.

### GitHub flow (`action.yml`)

- The Action gains a second job, `on: issue_comment (created)`, guarded by:
  the issue is a PR, the body contains `ai.trigger`, the commenter's `author_association` is in
  `ai.allowed_associations`, and `ai.enabled` is true in the repo's `safe_sql.yaml`. Requires
  `ANTHROPIC_API_KEY` and a `GITHUB_TOKEN` with `contents: write` + `pull-requests: write`.
  Concurrency group per PR so two mentions do not race.
- Steps: react 👀 on the comment → check out the PR head → `safe_sql lint --changed-since <base>`
  → `safe_sql fix --ai` → deliver according to `ai.mode`:
  - `commit`: commit to the PR head branch as `safe_sql[bot]`. If the PR is from a fork (no push
    access) fall back to `suggest`.
  - `suggest`: post a PR review with GitHub suggestion blocks for modified lines, and one review
    comment per new file containing its full contents.
  - `comment`: one PR comment with the unified diff and the summary.
- Always finish with a comment listing what was fixed, what remains, and the model used. Never
  merges, never bypasses branch protection; the fix commit re-triggers the normal lint job.
- `ai.auto: true` runs the same job from the `pull_request` lint job when it fails, without a
  mention. Default off because it spends API tokens on every push.

## Phases

### Phase 0 — flake.nix first (user request)
- Create `flake.nix` before any Go code. Inputs: `nixpkgs` (unstable), `flake-utils`. Outputs:
  - `devShells.default`: `go` (1.26 line), `gopls`, `golangci-lint`, `goreleaser`, `go-licenses`,
    `sqlc` (needed by `verify` and not on this machine's PATH), `pre-commit`, `docker-client`.
  - `packages.default`: `buildGoModule` for `cmd/safe_sql`, `vendorHash = null` placeholder until
    `go.sum` exists, then filled with the real hash in Phase 1.
  - `apps.default` pointing at the binary; `formatter = nixpkgs-fmt`.
- `.envrc` with `use flake` for direnv users; `.gitignore` (`result`, `.direnv/`).
- Validate: `nix flake check` and `nix develop -c go version` (nix 2.32.4 with flakes is enabled
  locally); commit as the first commit on `main`.

### Phase 1 — Skeleton, parsing, model
- `go mod init github.com/trishtzy/safe_sql`; cobra CLI; `lint` reads files, parses, prints statement
  kinds (debug `--dump`).
- `internal/sqlparse/model.go`; Postgres converter from oliphant AST (`RawStmt` → model).
- `internal/migrate`: discovery, ordering, tool detection, up/down split, transaction detection.
- Tests: table-driven parse tests per statement kind; migrate tests copied in spirit from sqlc's
  `migrations_test.go`.

### Phase 2 — Rule engine + Postgres/common rules
- `Rule` interface, registry, `Context`, `SchemaSoFar` catalog (tables/columns/constraints per file).
- Implement every rule in the Common and Postgres tables. Each rule: one file, one test file with
  `bad` and `good` SQL cases plus version-gated cases.
- Disable annotations, `start_after`, `check_down`, severity overrides.
- `internal/report`: human, json, github formats. Human output mirrors strong_migrations'
  "=== Dangerous operation detected ===" block followed by the "Good" pattern as SQL.

### Phase 3 — Config + sqlc discovery
- `safe_sql.yaml` loader with `${ENV}` substitution; sqlc.yaml lenient loader; merge precedence:
  CLI flags > safe_sql.yaml > sqlc.yaml.
- `safe_sql init` writes a starter `safe_sql.yaml` (detects sqlc.yaml, fills engine/schema).
- Golden end-to-end tests in `testdata/<case>/` run through the CLI.

### Phase 4 — verify
- `internal/verify` per the design above: git checkout of `--against` queries, migration apply,
  temp sqlc.yaml generation, `sqlc vet` shell-out, output parsing. Integration test behind a build
  tag `integration` using testcontainers and a `sqlc` binary (skipped when either is absent).
  Postgres only in this phase.

### Phase 5 — Distribution
- `.goreleaser.yaml` (darwin/linux/windows, amd64/arm64), GitHub Actions workflows for CI + release.
- `action.yml` composite action: installs the release binary, runs `lint` (or `verify` with
  `with: verify: true`), `--format github` annotations, `--changed-since` to limit to files changed
  in the PR.
- `.pre-commit-hooks.yaml` (`language: golang` so pre-commit builds it, plus a `system` variant).
- `Dockerfile` (distroless, multi-arch) published to GHCR by goreleaser.
- `flake.nix` (created in Phase 0) gets its real `vendorHash`, a `nix flake check` job in CI, and
  `nix run github:trishtzy/safe_sql` documented in the README.
- README: install, quickstart for sqlc projects, rule reference (generated by `safe_sql rules
  --markdown`), comparison with squawk and sqlc verify.

### Phase 6 — fix: deterministic fixers + AI loop + PR flow
- `internal/fix`: `FileEdit` model, tier-1 fixers with bad→good golden tests per rule.
- `internal/fix/ai`: `Provider` interface, Anthropic implementation, prompt builder, validator,
  iteration loop, fake provider for tests, record/replay fixtures.
- `safe_sql fix [--ai] [--dry-run] [--only rule,...]`.
- `internal/github`: comment/reaction/review-suggestion/commit helpers over the REST API using
  `GITHUB_TOKEN`; `action.yml` gains the `issue_comment` job and the `ai-*` inputs.
- Repo CI exercises the flow end-to-end on a fixture PR using the fake provider (`SAFE_SQL_AI_FAKE=1`).

### Phase 7 — SQLite
- meyer converter into the model, including views/triggers/pragmas and mapping meyer's rejection of
  unsupported ALTER forms into `Unsupported` actions rather than parse failures.
- Catalog extensions (views, triggers, indexes per table) needed by the SQLite rules.
- The SQLite rule table above, each with bad/good tests and version-gated cases; SQLite triggers for
  the common rules.
- `verify` SQLite support: temp file DB via `ncruces/go-sqlite3`, `sqlc vet` with `file:` URI.
  Fast enough to run in the default unit-test suite, no build tag.
- SQLite tier-1 fixers (pragma placement, rebuild completion, rebuild scaffold); goldens.
- Dogfood against sqlc's `examples/` SQLite schemas.
  (Phases 6 and 7 are independent and can be swapped.)

## Verification (how we know it works)

- `go test ./...` — unit tests per rule (bad/good/version-gated), parser conversion tests, migrate
  split/ordering tests, golden CLI tests.
- `go test -tags integration ./internal/verify/...` — spins up Postgres via testcontainers; fixture
  repo with a `main` ref (git init in a temp dir) where a new migration renames a column used by a
  deployed query → verify must report it; a compatible migration → clean.
- Dogfood: run `safe_sql lint` against sqlc's own `examples/` schema dirs and a fixture set
  hand-ported from strong_migrations' README "Bad"/"Good" examples (each Bad must fire exactly its
  rule; each Good must be clean).
- `golangci-lint`, `go vet`; `goreleaser release --snapshot` locally; `nix build`; run the action
  in this repo's own CI on a PR that adds a deliberately bad migration.
- Fix: tier-1 golden tests (each Bad fixture → expected Good files, then re-lint must be clean).
  AI: validator unit tests (path escape, disable-annotation smuggling, unparsable output, version
  collision all rejected); loop tests with the fake provider; one manual run against the real API
  documented in CONTRIBUTING with the expected outcome on the fixture set.

## Out of scope for this plan (call out in README)

- Runtime features of strong_migrations: automatic lock/statement timeout injection, lock-timeout
  retries, `safe_by_default` rewriting, `auto_analyze`, `remove_invalid_indexes`.
- MySQL/MariaDB (strong_migrations' COPY-algorithm, LOCK=, and expression-default checks) and the
  other sqlc engines. The `Statement` model and rule registry are engine-keyed so MySQL can be added
  later with `sqlc-dev/marino` (Apache-2.0) without restructuring.
- Custom user rules (CEL like sqlc vet) — a natural v2 once the statement model is stable.
