package rules

import (
	"fmt"
	"strings"

	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

// noTxHint maps a migration tool to the directive that runs a file outside a
// transaction. Verified against each tool's documentation.
var noTxHint = map[string]string{
	"goose":          "-- +goose NO TRANSACTION",
	"sql-migrate":    "-- +migrate Up notransaction",
	"dbmate":         "-- migrate:up transaction:false",
	"tern":           "---- tern: disable-tx ----",
	"atlas":          "-- atlas:txmode none",
	"golang-migrate": "put the statement alone in its own migration file (golang-migrate runs a multi-statement file as one implicit transaction)",
}

func txHint(tool string) string {
	if h, ok := noTxHint[tool]; ok {
		return h
	}
	return "run this migration outside a transaction (check your migration tool's documentation)"
}

// Postgres volatility classes for functions commonly used in defaults.
var (
	volatileFuncs = set("random", "gen_random_uuid", "uuid_generate_v1", "uuid_generate_v1mc",
		"uuid_generate_v4", "uuidv4", "uuidv7", "clock_timestamp", "timeofday", "nextval",
		"gen_random_bytes", "setval", "pg_backend_pid", "txid_current", "lastval")
	stableFuncs = set("now", "current_timestamp", "current_date", "current_time", "localtime",
		"localtimestamp", "transaction_timestamp", "statement_timestamp", "current_user",
		"session_user", "user", "current_role", "current_catalog", "current_schema",
		"current_setting", "to_char", "to_date", "to_timestamp", "date_trunc", "age",
		"uuid_generate_v3", "uuid_generate_v5", "uuid_nil", "md5", "lower", "upper", "concat",
		"coalesce", "nullif", "length", "substr", "substring", "trim", "abs", "round", "floor",
		"ceil", "array_to_string", "string_to_array", "jsonb_build_object", "json_build_object",
		"to_jsonb", "to_json", "encode", "decode", "sha256", "date", "interval")
)

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func init() {
	Register(&Rule{
		ID: "require-concurrent-index-creation", Engines: pgOnly, Severity: SeverityError,
		Summary: "Creating an index without CONCURRENTLY blocks writes for the whole build",
		Guidance: `CREATE INDEX takes a SHARE lock on the table, blocking writes until the index
is built. Build it concurrently instead:

  CREATE INDEX CONCURRENTLY idx_users_email ON users (email);

CONCURRENTLY cannot run inside a transaction, so the migration file must opt
out of the tool's transaction wrapper (see concurrent-index-in-transaction).
Note that a concurrent build that fails leaves an INVALID index behind that
must be dropped before retrying.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindCreateIndex || st.CreateIndex.Concurrent || ctx.TableCreatedHere(st.CreateIndex.Table) {
				return nil
			}
			r, _ := Get("require-concurrent-index-creation")
			return []Finding{ctx.finding(r, st, fmt.Sprintf("creating index %s on %s without CONCURRENTLY", st.CreateIndex.Name, st.CreateIndex.Table))}
		},
	})

	Register(&Rule{
		ID: "concurrent-index-in-transaction", Engines: pgOnly, Severity: SeverityError,
		Summary: "CREATE/DROP INDEX CONCURRENTLY fails inside a transaction",
		Guidance: `Postgres refuses CONCURRENTLY inside a transaction block ("cannot run inside
a transaction block"), and most migration tools wrap each file in one by
default. Put the statement in its own migration file and disable the
transaction for that file with the tool's directive.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if !ctx.InTransaction {
				return nil
			}
			conc := (st.Kind == sqlparse.KindCreateIndex && st.CreateIndex.Concurrent) ||
				(st.Kind == sqlparse.KindDropIndex && st.DropIndex.Concurrent)
			if !conc {
				return nil
			}
			r, _ := Get("concurrent-index-in-transaction")
			f := ctx.finding(r, st, "CONCURRENTLY cannot run inside a transaction, and this file runs in one")
			f.Guidance = r.Guidance + "\n\nFor " + ctx.Tool + ": " + txHint(ctx.Tool)
			return []Finding{f}
		},
	})

	Register(&Rule{
		ID: "add-unique-constraint-via-index", Engines: pgOnly, Severity: SeverityError,
		Summary: "Adding a unique constraint builds its index while blocking reads and writes",
		Guidance: `ALTER TABLE ... ADD CONSTRAINT ... UNIQUE (cols) creates the backing index
under an ACCESS EXCLUSIVE lock. Build the index concurrently first, then
attach it:

  CREATE UNIQUE INDEX CONCURRENTLY users_email_key ON users (email);
  -- next migration (or same file, outside a transaction):
  ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE USING INDEX users_email_key;`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			return forActions(ctx, st, sqlparse.ActionAddConstraint, func(r *Rule, a sqlparse.AlterAction) Finding {
				return ctx.finding(r, st, fmt.Sprintf("adding unique constraint on %s (%s) without USING INDEX", st.AlterTable.Table, strings.Join(a.Constraint.Columns, ", ")))
			}, "add-unique-constraint-via-index", func(ctx *Context, st *sqlparse.Statement, a sqlparse.AlterAction) bool {
				return a.Constraint == nil || a.Constraint.Kind != sqlparse.ConstraintUnique || a.Constraint.UsingIndex != ""
			})
		},
	})

	Register(&Rule{
		ID: "ban-exclusion-constraint", Engines: pgOnly, Severity: SeverityError,
		Summary: "Adding an exclusion constraint checks every row while blocking reads and writes",
		Guidance: `Exclusion constraints cannot be added NOT VALID or built concurrently; adding
one to an existing table scans it under an ACCESS EXCLUSIVE lock.

Create the table with the constraint from the start, enforce the rule in the
application, or accept the lock in a maintenance window and add
"-- safe_sql:disable ban-exclusion-constraint" above the statement.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			return forActions(ctx, st, sqlparse.ActionAddConstraint, func(r *Rule, a sqlparse.AlterAction) Finding {
				return ctx.finding(r, st, fmt.Sprintf("adding exclusion constraint on %s", st.AlterTable.Table))
			}, "ban-exclusion-constraint", func(ctx *Context, st *sqlparse.Statement, a sqlparse.AlterAction) bool {
				return a.Constraint == nil || a.Constraint.Kind != sqlparse.ConstraintExclusion
			})
		},
	})

	Register(&Rule{
		ID: "prefer-jsonb", Engines: pgOnly, Severity: SeverityWarning,
		Summary: "json columns lack an equality operator; use jsonb",
		Guidance: `The json type has no equality operator, so existing queries that use
SELECT DISTINCT, GROUP BY, or comparisons on the column start failing. jsonb
is also faster to process and indexable.

  ALTER TABLE users ADD COLUMN settings jsonb;`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			r, _ := Get("prefer-jsonb")
			var out []Finding
			switch st.Kind {
			case sqlparse.KindCreateTable:
				for _, c := range st.CreateTable.Columns {
					if c.Type == "json" {
						out = append(out, ctx.finding(r, st, fmt.Sprintf("column %s.%s uses json", st.CreateTable.Table, c.Name)))
					}
				}
			case sqlparse.KindAlterTable:
				for _, a := range st.AlterTable.Actions {
					if a.Kind == sqlparse.ActionAddColumn && a.ColumnDef != nil && a.ColumnDef.Type == "json" {
						out = append(out, ctx.finding(r, st, fmt.Sprintf("column %s.%s uses json", st.AlterTable.Table, a.Column)))
					}
				}
			}
			return out
		},
	})

	Register(&Rule{
		ID: "add-column-volatile-default", Engines: pgOnly, Severity: SeverityError,
		Summary: "Adding a column with a volatile default rewrites the whole table",
		Guidance: `Since Postgres 11, adding a column with a constant default is instant. A
volatile default (random(), gen_random_uuid(), clock_timestamp(), nextval(),
...) must be evaluated per row, so the whole table is rewritten while reads
and writes are blocked. Stable functions such as now() are fine.

Add the column without the default, set the default for new rows, then
backfill existing rows in batches:

  ALTER TABLE users ADD COLUMN token uuid;
  ALTER TABLE users ALTER COLUMN token SET DEFAULT gen_random_uuid();
  -- backfill in batches:
  UPDATE users SET token = gen_random_uuid() WHERE token IS NULL AND id BETWEEN $1 AND $2;`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindAlterTable || ctx.TableCreatedHere(st.AlterTable.Table) {
				return nil
			}
			r, _ := Get("add-column-volatile-default")
			var out []Finding
			for _, a := range st.AlterTable.Actions {
				if a.Kind != sqlparse.ActionAddColumn || a.ColumnDef == nil || a.ColumnDef.Default == nil {
					continue
				}
				d := a.ColumnDef.Default
				col := fmt.Sprintf("%s.%s", st.AlterTable.Table, a.Column)
				switch {
				case d.IsConstant || len(d.Funcs) == 0:
					if !ctx.TargetVersion.AtLeast(11, 0) {
						f := ctx.finding(r, st, fmt.Sprintf("adding column %s with a default rewrites the table on Postgres %s (< 11)", col, ctx.TargetVersion))
						out = append(out, f)
					}
				default:
					var volatile, unknown []string
					for _, fn := range d.Funcs {
						switch {
						case volatileFuncs[fn]:
							volatile = append(volatile, fn+"()")
						case !stableFuncs[fn]:
							unknown = append(unknown, fn+"()")
						}
					}
					if len(volatile) > 0 {
						out = append(out, ctx.finding(r, st, fmt.Sprintf("adding column %s with volatile default %s", col, strings.Join(volatile, ", "))))
					} else if len(unknown) > 0 {
						f := ctx.finding(r, st, fmt.Sprintf("adding column %s with default calling %s, whose volatility safe_sql cannot determine", col, strings.Join(unknown, ", ")))
						f.Severity = SeverityWarning
						out = append(out, f)
					}
				}
			}
			return out
		},
	})

	Register(&Rule{
		ID: "set-not-null-without-check", Engines: pgOnly, Severity: SeverityError,
		Summary: "SET NOT NULL scans the table under an exclusive lock unless a validated check exists",
		Guidance: `ALTER COLUMN ... SET NOT NULL verifies every row while holding an ACCESS
EXCLUSIVE lock, blocking reads and writes.

On Postgres 12+, add a check constraint without validation, validate it
separately (a much weaker lock), then SET NOT NULL is instant and the check
can be dropped:

  ALTER TABLE users ADD CONSTRAINT users_email_not_null CHECK (email IS NOT NULL) NOT VALID;
  -- next migration:
  ALTER TABLE users VALIDATE CONSTRAINT users_email_not_null;
  -- next migration:
  ALTER TABLE users ALTER COLUMN email SET NOT NULL;
  ALTER TABLE users DROP CONSTRAINT users_email_not_null;`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindAlterTable || ctx.TableCreatedHere(st.AlterTable.Table) {
				return nil
			}
			r, _ := Get("set-not-null-without-check")
			var out []Finding
			for _, a := range st.AlterTable.Actions {
				if a.Kind != sqlparse.ActionSetNotNull {
					continue
				}
				col := fmt.Sprintf("%s.%s", st.AlterTable.Table, a.Column)
				if !ctx.TargetVersion.AtLeast(12, 0) {
					out = append(out, ctx.finding(r, st, fmt.Sprintf("SET NOT NULL on %s scans the table on Postgres %s (< 12 cannot use a check constraint to skip the scan)", col, ctx.TargetVersion)))
					continue
				}
				if tbl := ctx.Catalog.Table(st.AlterTable.Table); tbl != nil && tbl.HasValidatedNotNullCheck(a.Column) {
					continue
				}
				out = append(out, ctx.finding(r, st, fmt.Sprintf("SET NOT NULL on %s without a validated CHECK (%s IS NOT NULL) constraint", col, a.Column)))
			}
			return out
		},
	})

	Register(&Rule{
		ID: "ban-rename-enum-value", Engines: pgOnly, Severity: SeverityError,
		Summary: "Renaming an enum value breaks running code that uses the old value",
		Guidance: `Renaming an enum value that is in use causes errors in the application.

Safer sequence:
  1. Add the new value:
       ALTER TYPE status ADD VALUE 'archived' AFTER 'inactive';
  2. Deploy code that handles both values and writes the new one.
  3. Backfill rows from the old value to the new one in batches.
  (Postgres cannot remove enum values without recreating the type.)`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindAlterEnum || st.AlterEnum.RenameOld == "" {
				return nil
			}
			r, _ := Get("ban-rename-enum-value")
			return []Finding{ctx.finding(r, st, fmt.Sprintf("renaming enum value %q to %q on %s", st.AlterEnum.RenameOld, st.AlterEnum.RenameNew, st.AlterEnum.Type))}
		},
	})

	Register(&Rule{
		ID: "ban-rename-schema", Engines: pgOnly, Severity: SeverityError,
		Summary: "Renaming a schema breaks running code that uses the old name",
		Guidance: `Renaming a schema that is in use causes errors in the application.

Safer sequence: create the new schema, write to both, backfill, move reads to
the new schema, stop writing the old one, then drop it.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindRenameSchema {
				return nil
			}
			r, _ := Get("ban-rename-schema")
			return []Finding{ctx.finding(r, st, fmt.Sprintf("renaming schema %s to %s", st.RenameSchema.Old, st.RenameSchema.New))}
		},
	})

	Register(&Rule{
		ID: "require-concurrent-index-drop", Engines: pgOnly, Severity: SeverityError, OptIn: true,
		Summary: "Dropping an index without CONCURRENTLY blocks reads and writes (opt-in)",
		Guidance: `DROP INDEX takes an ACCESS EXCLUSIVE lock on the table. For most
applications this is brief and acceptable, which is why this check is opt-in.

  DROP INDEX CONCURRENTLY idx_users_email;`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindDropIndex || st.DropIndex.Concurrent {
				return nil
			}
			r, _ := Get("require-concurrent-index-drop")
			return []Finding{ctx.finding(r, st, fmt.Sprintf("dropping index %s without CONCURRENTLY", strings.Join(st.DropIndex.Names, ", ")))}
		},
	})

	Register(&Rule{
		ID: "require-lock-timeout", Engines: pgOnly, Severity: SeverityWarning, OptIn: true,
		Summary: "Migration files with DDL should set lock_timeout (opt-in)",
		Guidance: `A DDL statement waiting for a lock makes every later query on that table
queue behind it. A short lock_timeout turns a stuck migration into a quick,
retryable failure instead of an outage:

  SET lock_timeout = '10s';
  SET statement_timeout = '1h';

Add these at the top of each migration file, or configure them on the role
that runs migrations. If your migration runner sets them itself, disable this
check.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if !st.Kind.IsDDL() {
				return nil
			}
			// Report once, on the first DDL statement.
			for i := 0; i < ctx.Index; i++ {
				if ctx.Statements[i].Kind.IsDDL() {
					return nil
				}
			}
			for i := range ctx.Statements {
				s := &ctx.Statements[i]
				if s.Kind == sqlparse.KindSet && s.Set.Name == "lock_timeout" {
					return nil
				}
			}
			r, _ := Get("require-lock-timeout")
			return []Finding{ctx.finding(r, st, "migration changes schema without setting lock_timeout")}
		},
	})
}
