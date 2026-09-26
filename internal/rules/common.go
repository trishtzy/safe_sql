package rules

import (
	"fmt"
	"strings"

	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

// Rules in this file apply to every engine. Each mirrors a strong_migrations
// check; the guidance is that check's advice translated to raw SQL.

func init() {
	Register(&Rule{
		ID: "ban-drop-column", Engines: common, Severity: SeverityError,
		Summary: "Dropping a column breaks running code that still reads it",
		Guidance: `Running application code (and ORM/sqlc-generated code with cached column lists)
still references the column and fails once it is gone.

Safe sequence:
  1. Remove every read and write of the column from the application and deploy.
  2. Then, in a later migration, drop the column:

     ALTER TABLE users DROP COLUMN name;

If the column is already unused, add "-- safe_sql:disable ban-drop-column"
above the statement to acknowledge this.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			return forActions(ctx, st, sqlparse.ActionDropColumn, func(r *Rule, a sqlparse.AlterAction) Finding {
				return ctx.finding(r, st, fmt.Sprintf("dropping column %s.%s", st.AlterTable.Table, a.Column))
			}, "ban-drop-column")
		},
	})

	Register(&Rule{
		ID: "ban-rename-column", Engines: common, Severity: SeverityError,
		Summary: "Renaming a column breaks running code that uses the old name",
		Guidance: `Renaming a column that is in use causes errors in the application until the
new code is deployed, and there is no moment where both old and new code work.

Safe sequence:
  1. Add the new column:
       ALTER TABLE users ADD COLUMN full_name text;
  2. Deploy code that writes to both columns and reads from the new one.
  3. Backfill the new column from the old one in batches.
  4. Deploy code that stops writing the old column.
  5. Drop the old column in a later migration.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindRenameColumn || ctx.TableCreatedHere(st.RenameColumn.Table) {
				return nil
			}
			r, _ := Get("ban-rename-column")
			return []Finding{ctx.finding(r, st, fmt.Sprintf("renaming column %s.%s to %s", st.RenameColumn.Table, st.RenameColumn.Old, st.RenameColumn.New))}
		},
	})

	Register(&Rule{
		ID: "ban-rename-table", Engines: common, Severity: SeverityError,
		Summary: "Renaming a table breaks running code that uses the old name",
		Guidance: `Renaming a table that is in use causes errors in the application.

Safer approaches:
  - Create a view with the old name that selects from the renamed table, deploy
    code that uses the new name, then drop the view:
       ALTER TABLE users RENAME TO accounts;
       CREATE VIEW users AS SELECT * FROM accounts;
  - Or create the new table, dual-write, backfill, switch reads, drop the old one.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindRenameTable || ctx.TableCreatedHere(st.RenameTable.Table) {
				return nil
			}
			r, _ := Get("ban-rename-table")
			return []Finding{ctx.finding(r, st, fmt.Sprintf("renaming table %s to %s", st.RenameTable.Table, st.RenameTable.NewName))}
		},
	})

	Register(&Rule{
		ID: "ban-drop-table", Engines: common, Severity: SeverityWarning,
		Summary: "Dropping a table destroys data and breaks code that uses it",
		Guidance: `Dropping a table loses its data and breaks any code still reading it.
DROP TABLE followed by CREATE TABLE of the same name (the equivalent of Rails'
"force: true") silently discards production data and is reported as an error.

Make sure nothing reads the table, take a backup if the data matters, then drop
it in its own migration. Add "-- safe_sql:disable ban-drop-table" to acknowledge.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindDropTable {
				return nil
			}
			r, _ := Get("ban-drop-table")
			var out []Finding
			for _, t := range st.DropTable.Tables {
				if ctx.TableCreatedHere(t) || ctx.renamedIntoLater(t) {
					continue // scratch table, or a table rebuild (DROP old; RENAME new TO old)
				}
				f := ctx.finding(r, st, fmt.Sprintf("dropping table %s", t))
				if ctx.recreatedLater(t) {
					f.Severity = SeverityError
					f.Message = fmt.Sprintf("dropping table %s and re-creating it in the same migration discards its data", t)
				}
				out = append(out, f)
			}
			return out
		},
	})

	Register(&Rule{
		ID: "ban-change-column-type", Engines: pgOnly, Severity: SeverityError,
		Summary: "Changing a column's type rewrites the table and breaks running code",
		Guidance: `Changing the type of a column causes the entire table to be rewritten while
reads and writes are blocked, and running code may not handle the new type.

Some changes do not rewrite the table and are allowed: widening varchar(n),
varchar -> text, numeric precision increase with the same scale, cidr -> inet,
timestamp/time/interval precision increase. safe_sql can only recognise these
when the column's original definition is in the migrations it was given; with
a partial history every type change is reported.

Safe sequence for everything else:
  1. Add a new column with the new type:
       ALTER TABLE users ADD COLUMN amount_v2 bigint;
  2. Deploy code that writes to both columns.
  3. Backfill the new column in batches.
  4. Switch reads to the new column, then drop the old one in a later migration.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			return forActions(ctx, st, sqlparse.ActionAlterColumnType, func(r *Rule, a sqlparse.AlterAction) Finding {
				return ctx.finding(r, st, fmt.Sprintf("changing type of %s.%s to %s", st.AlterTable.Table, a.Column, describeType(a.NewType)))
			}, "ban-change-column-type", func(ctx *Context, st *sqlparse.Statement, a sqlparse.AlterAction) bool {
				return typeChangeSafe(ctx, st.AlterTable.Table, a)
			})
		},
	})

	Register(&Rule{
		ID: "ban-add-serial-column", Engines: pgOnly, Severity: SeverityError,
		Summary: "Adding an auto-incrementing column rewrites the whole table",
		Guidance: `Adding a serial, bigserial or GENERATED AS IDENTITY column to an existing
table fills every row, which rewrites the table while reads and writes are
blocked.

If you need a new surrogate key, add a plain bigint column, backfill it in
batches from a sequence, then attach the identity/default afterwards:
  ALTER TABLE users ADD COLUMN new_id bigint;
  CREATE SEQUENCE users_new_id_seq OWNED BY users.new_id;
  -- backfill in batches, then:
  ALTER TABLE users ALTER COLUMN new_id SET DEFAULT nextval('users_new_id_seq');`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			return forActions(ctx, st, sqlparse.ActionAddColumn, func(r *Rule, a sqlparse.AlterAction) Finding {
				return ctx.finding(r, st, fmt.Sprintf("adding auto-incrementing column %s.%s", st.AlterTable.Table, a.Column))
			}, "ban-add-serial-column", func(ctx *Context, st *sqlparse.Statement, a sqlparse.AlterAction) bool {
				return a.ColumnDef == nil || (!a.ColumnDef.Serial && !a.ColumnDef.Identity)
			})
		},
	})

	Register(&Rule{
		ID: "ban-add-stored-generated-column", Engines: pgOnly, Severity: SeverityError,
		Summary: "Adding a stored generated column rewrites the whole table",
		Guidance: `Adding a GENERATED ALWAYS AS (...) STORED column computes the value for every
existing row, rewriting the table while reads and writes are blocked.

Use a virtual generated column (Postgres 18+), compute the value in the
application, or add a plain column and backfill it in batches.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			return forActions(ctx, st, sqlparse.ActionAddColumn, func(r *Rule, a sqlparse.AlterAction) Finding {
				return ctx.finding(r, st, fmt.Sprintf("adding stored generated column %s.%s", st.AlterTable.Table, a.Column))
			}, "ban-add-stored-generated-column", func(ctx *Context, st *sqlparse.Statement, a sqlparse.AlterAction) bool {
				return a.ColumnDef == nil || a.ColumnDef.Generated != "stored"
			})
		},
	})

	Register(&Rule{
		ID: "add-foreign-key-not-valid", Engines: pgOnly, Severity: SeverityError,
		Summary: "Adding a foreign key validates every row while blocking writes on both tables",
		Guidance: `Adding a foreign key checks every existing row, holding locks that block
writes on both the referencing and the referenced table for the duration.

Add the constraint without validating existing rows, then validate in a
separate migration (validation only takes a SHARE UPDATE EXCLUSIVE lock):

  ALTER TABLE orders ADD CONSTRAINT orders_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users (id) NOT VALID;

  -- next migration:
  ALTER TABLE orders VALIDATE CONSTRAINT orders_user_id_fkey;

Inline "REFERENCES" on ADD COLUMN cannot be marked NOT VALID; add the column
first, then the constraint as above.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindAlterTable || ctx.TableCreatedHere(st.AlterTable.Table) {
				return nil
			}
			r, _ := Get("add-foreign-key-not-valid")
			var out []Finding
			for _, a := range st.AlterTable.Actions {
				switch {
				case a.Kind == sqlparse.ActionAddConstraint && a.Constraint != nil &&
					a.Constraint.Kind == sqlparse.ConstraintForeignKey && !a.Constraint.NotValid:
					out = append(out, ctx.finding(r, st, fmt.Sprintf("adding foreign key on %s without NOT VALID", st.AlterTable.Table)))
				case a.Kind == sqlparse.ActionAddColumn && a.ColumnDef != nil && a.ColumnDef.References != nil:
					out = append(out, ctx.finding(r, st, fmt.Sprintf("adding column %s.%s with an inline foreign key (validated immediately)", st.AlterTable.Table, a.Column)))
				}
			}
			return out
		},
	})

	Register(&Rule{
		ID: "add-check-constraint-not-valid", Engines: pgOnly, Severity: SeverityError,
		Summary: "Adding a check constraint scans every row while blocking reads and writes",
		Guidance: `Adding a CHECK constraint verifies every existing row while holding an
ACCESS EXCLUSIVE lock, blocking reads and writes.

Add it without validation, then validate in a separate migration:

  ALTER TABLE users ADD CONSTRAINT users_age_check CHECK (age >= 0) NOT VALID;

  -- next migration:
  ALTER TABLE users VALIDATE CONSTRAINT users_age_check;`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			return forActions(ctx, st, sqlparse.ActionAddConstraint, func(r *Rule, a sqlparse.AlterAction) Finding {
				return ctx.finding(r, st, fmt.Sprintf("adding check constraint on %s without NOT VALID", st.AlterTable.Table))
			}, "add-check-constraint-not-valid", func(ctx *Context, st *sqlparse.Statement, a sqlparse.AlterAction) bool {
				return a.Constraint == nil || a.Constraint.Kind != sqlparse.ConstraintCheck || a.Constraint.NotValid
			})
		},
	})

	Register(&Rule{
		ID: "ban-dml-in-migration", Engines: common, Severity: SeverityWarning,
		Summary: "Backfilling data inside a schema migration holds locks for the whole run",
		Guidance: `An UPDATE/INSERT/DELETE that touches many rows in the same migration (and
transaction) as schema changes keeps the DDL locks held until the data change
finishes, and a single large statement can take a long time on its own.

Backfill in a separate step, outside the schema migration, in batches:

  UPDATE users SET status = 'active' WHERE id BETWEEN $1 AND $2;  -- loop over ranges

If the table is small or the migration is a one-row seed, add
"-- safe_sql:disable ban-dml-in-migration" above the statement.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindDML || st.DML.Op == "select" || !ctx.HasDDL() || ctx.TableCreatedHere(st.DML.Table) {
				return nil
			}
			r, _ := Get("ban-dml-in-migration")
			return []Finding{ctx.finding(r, st, fmt.Sprintf("%s on %s in a migration that also changes schema", strings.ToUpper(st.DML.Op), st.DML.Table))}
		},
	})

	Register(&Rule{
		ID: "index-too-many-columns", Engines: common, Severity: SeverityWarning,
		Summary: "Non-unique indexes with more than three columns are rarely useful",
		Guidance: `Adding a non-unique index with more than three columns rarely improves
performance; the leading columns do most of the work and the rest bloat the
index. Keep non-unique indexes to three columns or fewer, or make it a
covering index with INCLUDE for the extra columns:

  CREATE INDEX CONCURRENTLY idx ON orders (user_id, status) INCLUDE (created_at, total);`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindCreateIndex || st.CreateIndex.Unique || len(st.CreateIndex.Columns) <= 3 {
				return nil
			}
			r, _ := Get("index-too-many-columns")
			return []Finding{ctx.finding(r, st, fmt.Sprintf("index on %s has %d columns", st.CreateIndex.Table, len(st.CreateIndex.Columns)))}
		},
	})
}

// forActions runs mk for each ALTER TABLE action of the given kind on a
// table that was not created in this file. An optional safe predicate
// suppresses the finding when it returns true.
func forActions(ctx *Context, st *sqlparse.Statement, kind sqlparse.AlterActionKind,
	mk func(*Rule, sqlparse.AlterAction) Finding, ruleID string,
	safe ...func(*Context, *sqlparse.Statement, sqlparse.AlterAction) bool) []Finding {
	if st.Kind != sqlparse.KindAlterTable || ctx.TableCreatedHere(st.AlterTable.Table) {
		return nil
	}
	r, _ := Get(ruleID)
	var out []Finding
	for _, a := range st.AlterTable.Actions {
		if a.Kind != kind {
			continue
		}
		skip := false
		for _, s := range safe {
			if s(ctx, st, a) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, mk(r, a))
		}
	}
	return out
}

// recreatedLater reports whether a CREATE TABLE for t follows the current
// statement in the same section.
func (c *Context) recreatedLater(t sqlparse.TableName) bool {
	for i := c.Index + 1; i < len(c.Statements); i++ {
		s := &c.Statements[i]
		if s.Kind == sqlparse.KindCreateTable && s.CreateTable.Table.Key() == t.Key() {
			return true
		}
	}
	return false
}

// renamedIntoLater reports whether a later statement renames some table to t
// (the tail of SQLite's rebuild procedure).
func (c *Context) renamedIntoLater(t sqlparse.TableName) bool {
	for i := c.Index + 1; i < len(c.Statements); i++ {
		s := &c.Statements[i]
		if s.Kind == sqlparse.KindRenameTable && strings.EqualFold(s.RenameTable.NewName, t.Name) {
			return true
		}
	}
	return false
}

func describeType(c *sqlparse.ColumnDef) string {
	if c == nil {
		return "?"
	}
	s := c.Type
	if len(c.TypeMods) > 0 {
		parts := make([]string, len(c.TypeMods))
		for i, m := range c.TypeMods {
			parts[i] = fmt.Sprint(m)
		}
		s += "(" + strings.Join(parts, ",") + ")"
	}
	if c.IsArray {
		s += "[]"
	}
	return s
}

// typeChangeSafe ports strong_migrations' Postgres change_type_safe?: the
// type changes Postgres performs without rewriting the table.
func typeChangeSafe(ctx *Context, table sqlparse.TableName, a sqlparse.AlterAction) bool {
	if a.NewType == nil || ctx.Catalog == nil {
		return false
	}
	tbl := ctx.Catalog.Table(table)
	if tbl == nil {
		return false
	}
	col := tbl.Column(a.Column)
	if col == nil {
		return false
	}
	old, nw := col.Def, *a.NewType
	if old.IsArray != nw.IsArray {
		return false
	}
	indexed := tbl.IndexedColumns(a.Column)
	mod := func(c sqlparse.ColumnDef, i, def int) int {
		if len(c.TypeMods) > i {
			return c.TypeMods[i]
		}
		return def
	}
	switch nw.Type {
	case "varchar":
		switch old.Type {
		case "varchar":
			return len(nw.TypeMods) == 0 || (len(old.TypeMods) > 0 && nw.TypeMods[0] >= old.TypeMods[0])
		case "text":
			return len(nw.TypeMods) == 0
		case "citext":
			return len(nw.TypeMods) == 0 && !indexed
		}
	case "text":
		return old.Type == "varchar" || old.Type == "text" || (old.Type == "citext" && !indexed)
	case "citext":
		return (old.Type == "varchar" || old.Type == "text") && !indexed
	case "numeric", "decimal":
		if old.Type != "numeric" && old.Type != "decimal" {
			return false
		}
		if len(nw.TypeMods) == 0 {
			return true
		}
		return len(old.TypeMods) > 0 && nw.TypeMods[0] >= old.TypeMods[0] && mod(nw, 1, 0) == mod(old, 1, 0)
	case "timestamp", "timestamptz", "time", "interval":
		// Same type with equal or higher precision. Switching between
		// timestamp and timestamptz is only free when the session time zone is
		// UTC, which static analysis cannot see, so it is always reported.
		return old.Type == nw.Type && mod(nw, 0, 6) >= mod(old, 0, 6)
	case "inet":
		return old.Type == "cidr"
	}
	return false
}
