package rules

import (
	"fmt"
	"strings"

	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

// SQLite rules. strong_migrations has none; these come from SQLite's own
// documented ALTER TABLE restrictions (https://sqlite.org/lang_altertable.html)
// and its single-writer locking model.

var sqliteOnly = []sqlparse.Engine{sqlparse.EngineSQLite}

// rebuild is the "12-step" table rebuild pattern found in a section.
type rebuild struct {
	old, tmp                sqlparse.TableName
	createIdx, dropIdx      int
	renameIdx               int
	hasInsertSelect         bool
	fkOff, fkCheck, inTx    bool
	newIndexes, newTriggers map[string]bool
	newViews                map[string]bool
}

// findRebuild detects CREATE TABLE tmp; INSERT INTO tmp SELECT ... FROM old;
// DROP TABLE old; ALTER TABLE tmp RENAME TO old. It is keyed by the DROP
// TABLE statement, the last point at which the catalog still describes the
// old table's indexes, triggers and views.
func findRebuild(c *Context, dropAt int) *rebuild {
	dr := c.Statements[dropAt]
	if dr.Kind != sqlparse.KindDropTable || len(dr.DropTable.Tables) != 1 {
		return nil
	}
	old := dr.DropTable.Tables[0]
	renameAt := -1
	for i := dropAt + 1; i < len(c.Statements); i++ {
		s := &c.Statements[i]
		if s.Kind == sqlparse.KindRenameTable && strings.EqualFold(s.RenameTable.NewName, old.Name) {
			renameAt = i
			break
		}
	}
	if renameAt < 0 {
		return nil
	}
	rn := c.Statements[renameAt]
	r := &rebuild{tmp: rn.RenameTable.Table, old: old,
		createIdx: -1, dropIdx: dropAt, renameIdx: renameAt,
		newIndexes: map[string]bool{}, newTriggers: map[string]bool{}, newViews: map[string]bool{}}
	explicitTx := false
	for i := range c.Statements {
		s := &c.Statements[i]
		switch s.Kind {
		case sqlparse.KindCreateTable:
			if i < renameAt && s.CreateTable.Table.Key() == r.tmp.Key() {
				r.createIdx = i
			}
		case sqlparse.KindDML:
			if i < renameAt && s.DML.Op == "insert" && s.DML.FromSelect && s.DML.Table.Key() == r.tmp.Key() {
				r.hasInsertSelect = true
			}
		case sqlparse.KindPragma:
			if s.Pragma.Name == "foreign_keys" && strings.EqualFold(strings.Trim(s.Pragma.Value, " ='\""), "off") || s.Pragma.Value == "0" && s.Pragma.Name == "foreign_keys" {
				if i < renameAt {
					r.fkOff = true
				}
			}
			if s.Pragma.Name == "foreign_key_check" {
				r.fkCheck = true
			}
		case sqlparse.KindTransaction:
			if s.Transaction.Begin && i < renameAt {
				explicitTx = true
			}
		case sqlparse.KindCreateIndex:
			if s.CreateIndex.Table.Key() == r.old.Key() || s.CreateIndex.Table.Key() == r.tmp.Key() {
				r.newIndexes[strings.ToLower(s.CreateIndex.Name)] = true
			}
		case sqlparse.KindCreateTrigger:
			r.newTriggers[strings.ToLower(s.Trigger.Name)] = true
		case sqlparse.KindCreateView:
			r.newViews[strings.ToLower(s.View.Name)] = true
		}
	}
	r.inTx = c.InTransaction || explicitTx
	if r.createIdx < 0 || !r.hasInsertSelect {
		return nil
	}
	return r
}

func init() {
	Register(&Rule{
		ID: "sqlite-unsupported-alter", Engines: sqliteOnly, Severity: SeverityError,
		Summary: "SQLite cannot execute this ALTER TABLE form; rebuild the table instead",
		Guidance: `SQLite's ALTER TABLE supports only RENAME TO, RENAME COLUMN, ADD COLUMN and
DROP COLUMN. Changing a column's type or constraints, or adding a table
constraint, needs the documented rebuild procedure inside one transaction:

  PRAGMA foreign_keys = OFF;         -- before BEGIN; a no-op inside a transaction
  BEGIN;
  CREATE TABLE users_new (... new definition ...);
  INSERT INTO users_new (cols) SELECT cols FROM users;
  DROP TABLE users;
  ALTER TABLE users_new RENAME TO users;
  -- recreate every index, trigger and view that referenced users
  PRAGMA foreign_key_check;
  COMMIT;
  PRAGMA foreign_keys = ON;

sqlc understands these ALTER forms for schema evolution in its own catalog,
which is why they parse, but the database rejects them at run time.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			r, _ := Get("sqlite-unsupported-alter")
			switch st.Kind {
			case sqlparse.KindInvalid:
				if strings.HasPrefix(strings.ToUpper(st.Raw), "ALTER TABLE") {
					return []Finding{ctx.finding(r, st, "ALTER TABLE form SQLite does not support: "+st.Error)}
				}
			case sqlparse.KindAlterTable:
				var out []Finding
				for _, a := range st.AlterTable.Actions {
					if a.Kind == sqlparse.ActionUnsupported {
						out = append(out, ctx.finding(r, st, fmt.Sprintf("ALTER TABLE %s %s cannot run on SQLite", st.AlterTable.Table, a.Raw)))
					}
				}
				return out
			}
			return nil
		},
	})

	Register(&Rule{
		ID: "sqlite-add-column-restrictions", Engines: sqliteOnly, Severity: SeverityError,
		Summary: "ADD COLUMN forms that SQLite rejects at run time",
		Guidance: `SQLite's ADD COLUMN cannot: declare PRIMARY KEY or UNIQUE; use a
non-constant default (CURRENT_TIMESTAMP, expressions in parentheses); be NOT
NULL without a non-NULL default; be a STORED generated column; or REFERENCE
another table while having a non-NULL default when foreign keys are on.

Either relax the definition (add the column nullable, backfill, then treat
NOT NULL in the application) or rebuild the table with the column in its
CREATE TABLE (see sqlite-unsupported-alter for the procedure).`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindAlterTable {
				return nil
			}
			r, _ := Get("sqlite-add-column-restrictions")
			var out []Finding
			for _, a := range st.AlterTable.Actions {
				if a.Kind != sqlparse.ActionAddColumn || a.ColumnDef == nil {
					continue
				}
				c := a.ColumnDef
				col := fmt.Sprintf("%s.%s", st.AlterTable.Table, c.Name)
				var why []string
				if c.PrimaryKey {
					why = append(why, "PRIMARY KEY")
				}
				if c.Unique {
					why = append(why, "UNIQUE")
				}
				nullDefault := c.Default != nil && strings.EqualFold(strings.TrimSpace(c.Default.Raw), "null")
				if c.Default != nil && !nullDefault && !c.Default.IsConstant {
					why = append(why, "non-constant DEFAULT "+c.Default.Raw)
				}
				if c.NotNull && (c.Default == nil || nullDefault) {
					why = append(why, "NOT NULL without a non-NULL default")
				}
				if c.Generated == "stored" {
					why = append(why, "STORED generated column")
				}
				if c.References != nil && c.Default != nil && !nullDefault {
					why = append(why, "REFERENCES with a non-NULL default")
				}
				if len(why) > 0 {
					out = append(out, ctx.finding(r, st, fmt.Sprintf("adding column %s fails on SQLite: %s", col, strings.Join(why, "; "))))
				}
			}
			return out
		},
	})

	Register(&Rule{
		ID: "sqlite-drop-column-restrictions", Engines: sqliteOnly, Severity: SeverityError,
		Summary: "DROP COLUMN fails on old SQLite or when the column is indexed, referenced, or used by a view/trigger",
		Guidance: `SQLite added DROP COLUMN in 3.35.0, and even then refuses to drop a column
that is a PRIMARY KEY or UNIQUE, has an index, is referenced by a foreign key
or a generated column, or appears in a view or trigger. The statement fails
at run time with the migration half applied.

Drop the dependent index, view or trigger first (and recreate it), or rebuild
the table without the column (see sqlite-unsupported-alter).`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindAlterTable || ctx.TableCreatedHere(st.AlterTable.Table) {
				return nil
			}
			r, _ := Get("sqlite-drop-column-restrictions")
			var out []Finding
			for _, a := range st.AlterTable.Actions {
				if a.Kind != sqlparse.ActionDropColumn {
					continue
				}
				col := fmt.Sprintf("%s.%s", st.AlterTable.Table, a.Column)
				if !ctx.TargetVersion.AtLeast(3, 35) {
					out = append(out, ctx.finding(r, st, fmt.Sprintf("DROP COLUMN %s needs SQLite 3.35+, target is %s", col, ctx.TargetVersion)))
					continue
				}
				var why []string
				if tbl := ctx.Catalog.Table(st.AlterTable.Table); tbl != nil {
					if tbl.IndexedColumns(a.Column) {
						why = append(why, "it is a key or indexed")
					}
					if g := tbl.GeneratedColumnsUsing(a.Column); len(g) > 0 {
						why = append(why, "generated column(s) "+strings.Join(g, ", ")+" use it")
					}
				}
				if by := ctx.Catalog.ReferencedBy(st.AlterTable.Table, a.Column); len(by) > 0 {
					why = append(why, "foreign keys from "+strings.Join(by, ", ")+" reference it")
				}
				if v := ctx.Catalog.ViewsMentioning(a.Column); len(v) > 0 {
					why = append(why, "view(s) "+strings.Join(v, ", ")+" mention it")
				}
				if t := ctx.Catalog.TriggersMentioning(st.AlterTable.Table, a.Column); len(t) > 0 {
					why = append(why, "trigger(s) "+strings.Join(t, ", ")+" mention it")
				}
				if len(why) > 0 {
					out = append(out, ctx.finding(r, st, fmt.Sprintf("DROP COLUMN %s fails on SQLite: %s", col, strings.Join(why, "; "))))
				}
			}
			return out
		},
	})

	Register(&Rule{
		ID: "sqlite-rename-column-version", Engines: sqliteOnly, Severity: SeverityError,
		Summary: "RENAME COLUMN needs SQLite 3.25+",
		Guidance: `ALTER TABLE ... RENAME COLUMN was added in SQLite 3.25.0. On older versions
rebuild the table (see sqlite-unsupported-alter), or raise target_version if
your driver ships a newer SQLite.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindRenameColumn || ctx.TargetVersion.AtLeast(3, 25) {
				return nil
			}
			r, _ := Get("sqlite-rename-column-version")
			return []Finding{ctx.finding(r, st, fmt.Sprintf("RENAME COLUMN needs SQLite 3.25+, target is %s", ctx.TargetVersion))}
		},
	})

	Register(&Rule{
		ID: "sqlite-pragma-in-transaction", Engines: sqliteOnly, Severity: SeverityError,
		Summary: "PRAGMA foreign_keys / journal_mode are silently ignored inside a transaction",
		Guidance: `PRAGMA foreign_keys is a no-op inside a transaction and PRAGMA journal_mode
cannot change there, so a migration that relies on them (the table-rebuild
procedure, for one) runs with foreign keys still enforced. Most migration
tools wrap each file in a transaction.

Run the file outside the tool's transaction (its no-transaction directive)
and manage the transaction explicitly:

  PRAGMA foreign_keys = OFF;
  BEGIN;
  ...
  COMMIT;
  PRAGMA foreign_keys = ON;`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			if st.Kind != sqlparse.KindPragma || !ctx.InTransaction {
				return nil
			}
			switch st.Pragma.Name {
			case "foreign_keys", "journal_mode":
			default:
				return nil
			}
			r, _ := Get("sqlite-pragma-in-transaction")
			f := ctx.finding(r, st, fmt.Sprintf("PRAGMA %s inside a transaction has no effect", st.Pragma.Name))
			f.Guidance = r.Guidance + "\n\nFor " + ctx.Tool + ": " + txHint(ctx.Tool)
			return []Finding{f}
		},
	})

	Register(&Rule{
		ID: "sqlite-table-rebuild", Engines: sqliteOnly, Severity: SeverityError,
		Summary: "Table rebuild is missing a step of SQLite's documented procedure",
		Guidance: `A table rebuild (CREATE new, INSERT ... SELECT, DROP old, RENAME) must run
inside one transaction, with foreign keys disabled beforehand when other
tables reference it, and must recreate every index, trigger and view that
existed on the old table, then run PRAGMA foreign_key_check before COMMIT.
Anything missed is lost silently or fails half way.

  PRAGMA foreign_keys = OFF;
  BEGIN;
  CREATE TABLE users_new (...);
  INSERT INTO users_new (...) SELECT ... FROM users;
  DROP TABLE users;
  ALTER TABLE users_new RENAME TO users;
  CREATE INDEX users_email_idx ON users (email);   -- every former index
  CREATE TRIGGER ...;                              -- every former trigger
  CREATE VIEW ...;                                 -- every view over users
  PRAGMA foreign_key_check;
  COMMIT;
  PRAGMA foreign_keys = ON;`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			rb := findRebuild(ctx, ctx.Index)
			if rb == nil {
				return nil
			}
			r, _ := Get("sqlite-table-rebuild")
			var out []Finding
			if !rb.inTx {
				out = append(out, ctx.finding(r, st, fmt.Sprintf("rebuild of %s does not run inside a transaction", rb.old)))
			}
			tbl := ctx.Catalog.Table(rb.old)
			if tbl != nil {
				var missing []string
				for _, ix := range tbl.Indexes {
					if !rb.newIndexes[strings.ToLower(ix.Name)] {
						missing = append(missing, "index "+ix.Name)
					}
				}
				for _, tr := range ctx.Catalog.TriggersOn(rb.old) {
					if !rb.newTriggers[strings.ToLower(tr.Name)] {
						missing = append(missing, "trigger "+tr.Name)
					}
				}
				for _, v := range ctx.Catalog.Views() {
					if mentions(v.Definition, rb.old.Name) && !rb.newViews[strings.ToLower(v.Name)] {
						missing = append(missing, "view "+v.Name)
					}
				}
				if len(missing) > 0 {
					out = append(out, ctx.finding(r, st, fmt.Sprintf("rebuild of %s does not recreate: %s", rb.old, strings.Join(missing, ", "))))
				}
			}
			referenced := len(ctx.Catalog.ReferencedBy(rb.old, "")) > 0 || anyFKTo(ctx, rb.old)
			if referenced && !rb.fkOff {
				f := ctx.finding(r, st, fmt.Sprintf("other tables reference %s; disable foreign keys (PRAGMA foreign_keys = OFF, before BEGIN) or the DROP fails / cascades", rb.old))
				f.Severity = SeverityWarning
				out = append(out, f)
			}
			if !rb.fkCheck {
				f := ctx.finding(r, st, "rebuild does not run PRAGMA foreign_key_check before COMMIT")
				f.Severity = SeverityWarning
				out = append(out, f)
			}
			return out
		},
	})

	Register(&Rule{
		ID: "sqlite-rebuild-locks-database", Engines: sqliteOnly, Severity: SeverityWarning,
		Summary: "A table rebuild holds SQLite's single write lock for the whole copy",
		Guidance: `SQLite has one writer at a time. Copying a large table into its
replacement holds the write lock (and, without WAL, blocks readers) until
the transaction commits. Schedule the migration for a quiet period, and
prefer WAL mode so readers keep working.`,
		Check: func(ctx *Context, st *sqlparse.Statement) []Finding {
			rb := findRebuild(ctx, ctx.Index)
			if rb == nil {
				return nil
			}
			r, _ := Get("sqlite-rebuild-locks-database")
			return []Finding{ctx.finding(r, st, fmt.Sprintf("rebuilding %s copies every row under the write lock", rb.old))}
		},
	})
}

func anyFKTo(ctx *Context, t sqlparse.TableName) bool {
	for _, tbl := range ctx.Catalog.Tables() {
		for _, cn := range tbl.Constraints {
			if cn.Kind == sqlparse.ConstraintForeignKey && cn.References != nil && cn.References.Key() == t.Key() {
				return true
			}
		}
	}
	return false
}

func mentions(text, word string) bool {
	lower, w := strings.ToLower(text), strings.ToLower(word)
	for i := 0; i+len(w) <= len(lower); i++ {
		if lower[i:i+len(w)] != w {
			continue
		}
		before := i == 0 || !identByte(lower[i-1])
		after := i+len(w) == len(lower) || !identByte(lower[i+len(w)])
		if before && after {
			return true
		}
	}
	return false
}

func identByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}
