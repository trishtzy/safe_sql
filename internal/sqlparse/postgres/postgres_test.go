package postgres

import (
	"errors"
	"reflect"
	"testing"

	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

func parseOne(t *testing.T, sql string) sqlparse.Statement {
	t.Helper()
	stmts, err := Parser{}.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	if len(stmts) != 1 {
		t.Fatalf("want 1 statement, got %d", len(stmts))
	}
	return stmts[0]
}

func TestCreateTable(t *testing.T) {
	st := parseOne(t, `CREATE TABLE IF NOT EXISTS app.users (
		id bigserial PRIMARY KEY,
		email varchar(255) NOT NULL UNIQUE,
		org_id integer REFERENCES orgs(id),
		meta json,
		price numeric(10,2) DEFAULT 0,
		tags text[],
		created_at timestamptz DEFAULT now(),
		total int GENERATED ALWAYS AS (price * 2) STORED,
		CONSTRAINT price_pos CHECK (price >= 0),
		FOREIGN KEY (org_id) REFERENCES orgs (id)
	)`)
	if st.Kind != sqlparse.KindCreateTable {
		t.Fatalf("kind = %v", st.Kind)
	}
	ct := st.CreateTable
	if ct.Table != (sqlparse.TableName{Schema: "app", Name: "users"}) || !ct.IfNotExists {
		t.Errorf("table = %+v", ct.Table)
	}
	cols := map[string]sqlparse.ColumnDef{}
	for _, c := range ct.Columns {
		cols[c.Name] = c
	}
	if c := cols["id"]; !c.Serial || !c.PrimaryKey || c.Type != "bigserial" {
		t.Errorf("id = %+v", c)
	}
	if c := cols["email"]; c.Type != "varchar" || !reflect.DeepEqual(c.TypeMods, []int{255}) || !c.NotNull || !c.Unique {
		t.Errorf("email = %+v", c)
	}
	if c := cols["org_id"]; c.Type != "integer" || c.References == nil || c.References.Name != "orgs" {
		t.Errorf("org_id = %+v", c)
	}
	if c := cols["meta"]; c.Type != "json" {
		t.Errorf("meta = %+v", c)
	}
	if c := cols["price"]; !reflect.DeepEqual(c.TypeMods, []int{10, 2}) || c.Default == nil || !c.Default.IsConstant {
		t.Errorf("price = %+v", c)
	}
	if c := cols["tags"]; !c.IsArray || c.Type != "text" {
		t.Errorf("tags = %+v", c)
	}
	if c := cols["created_at"]; c.Type != "timestamptz" || c.Default == nil || c.Default.IsConstant || !reflect.DeepEqual(c.Default.Funcs, []string{"now"}) {
		t.Errorf("created_at = %+v default=%+v", c, c.Default)
	}
	if c := cols["total"]; c.Generated != "stored" {
		t.Errorf("total = %+v", c)
	}
	if len(ct.Constraints) != 2 || ct.Constraints[0].Kind != sqlparse.ConstraintCheck || ct.Constraints[0].Name != "price_pos" ||
		ct.Constraints[1].Kind != sqlparse.ConstraintForeignKey || ct.Constraints[1].References.Name != "orgs" {
		t.Errorf("constraints = %+v", ct.Constraints)
	}
}

func TestAlterTableActions(t *testing.T) {
	cases := []struct {
		sql  string
		kind sqlparse.AlterActionKind
		chk  func(a sqlparse.AlterAction) bool
	}{
		{"ALTER TABLE t ADD COLUMN c uuid DEFAULT gen_random_uuid()", sqlparse.ActionAddColumn,
			func(a sqlparse.AlterAction) bool {
				return a.ColumnDef.Type == "uuid" && a.ColumnDef.Default != nil && a.ColumnDef.Default.Funcs[0] == "gen_random_uuid"
			}},
		{"ALTER TABLE t ADD COLUMN c timestamp DEFAULT CURRENT_TIMESTAMP", sqlparse.ActionAddColumn,
			func(a sqlparse.AlterAction) bool { return a.ColumnDef.Default.Funcs[0] == "current_timestamp" }},
		{"ALTER TABLE t ADD COLUMN c int DEFAULT 5 NOT NULL", sqlparse.ActionAddColumn,
			func(a sqlparse.AlterAction) bool { return a.ColumnDef.Default.IsConstant && a.ColumnDef.NotNull }},
		{"ALTER TABLE t ADD COLUMN c bigint GENERATED ALWAYS AS IDENTITY", sqlparse.ActionAddColumn,
			func(a sqlparse.AlterAction) bool { return a.ColumnDef.Identity }},
		{"ALTER TABLE t DROP COLUMN c", sqlparse.ActionDropColumn, func(a sqlparse.AlterAction) bool { return a.Column == "c" }},
		{"ALTER TABLE t ALTER COLUMN c TYPE varchar(100) USING c::varchar", sqlparse.ActionAlterColumnType,
			func(a sqlparse.AlterAction) bool {
				return a.NewType.Type == "varchar" && a.NewType.TypeMods[0] == 100 && a.Using != nil
			}},
		{"ALTER TABLE t ALTER COLUMN c SET NOT NULL", sqlparse.ActionSetNotNull, nil},
		{"ALTER TABLE t ALTER COLUMN c DROP NOT NULL", sqlparse.ActionDropNotNull, nil},
		{"ALTER TABLE t ALTER COLUMN c SET DEFAULT now()", sqlparse.ActionSetDefault,
			func(a sqlparse.AlterAction) bool { return a.Default.Funcs[0] == "now" }},
		{"ALTER TABLE t ALTER COLUMN c DROP DEFAULT", sqlparse.ActionDropDefault, nil},
		{"ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (a) REFERENCES u (id) NOT VALID", sqlparse.ActionAddConstraint,
			func(a sqlparse.AlterAction) bool {
				return a.Constraint.Kind == sqlparse.ConstraintForeignKey && a.Constraint.NotValid && a.Constraint.References.Name == "u"
			}},
		{"ALTER TABLE t ADD CONSTRAINT ck CHECK (a > 0)", sqlparse.ActionAddConstraint,
			func(a sqlparse.AlterAction) bool {
				return a.Constraint.Kind == sqlparse.ConstraintCheck && !a.Constraint.NotValid && a.Constraint.Expr.Raw == "a > 0"
			}},
		{"ALTER TABLE t ADD CONSTRAINT uq UNIQUE (a, b)", sqlparse.ActionAddConstraint,
			func(a sqlparse.AlterAction) bool {
				return a.Constraint.Kind == sqlparse.ConstraintUnique && reflect.DeepEqual(a.Constraint.Columns, []string{"a", "b"})
			}},
		{"ALTER TABLE t ADD CONSTRAINT uq UNIQUE USING INDEX idx", sqlparse.ActionAddConstraint,
			func(a sqlparse.AlterAction) bool { return a.Constraint.UsingIndex == "idx" }},
		{"ALTER TABLE t ADD CONSTRAINT ex EXCLUDE USING gist (a WITH =)", sqlparse.ActionAddConstraint,
			func(a sqlparse.AlterAction) bool { return a.Constraint.Kind == sqlparse.ConstraintExclusion }},
		{"ALTER TABLE t VALIDATE CONSTRAINT fk", sqlparse.ActionValidateConstraint, func(a sqlparse.AlterAction) bool { return a.ConstraintName == "fk" }},
		{"ALTER TABLE t DROP CONSTRAINT fk", sqlparse.ActionDropConstraint, nil},
		{"ALTER TABLE t ALTER COLUMN c ADD GENERATED ALWAYS AS IDENTITY", sqlparse.ActionAddIdentity, nil},
	}
	for _, c := range cases {
		st := parseOne(t, c.sql)
		if st.Kind != sqlparse.KindAlterTable || len(st.AlterTable.Actions) != 1 {
			t.Errorf("%s: kind=%v actions=%d", c.sql, st.Kind, len(st.AlterTable.Actions))
			continue
		}
		a := st.AlterTable.Actions[0]
		if a.Kind != c.kind {
			t.Errorf("%s: action kind = %v, want %v", c.sql, a.Kind, c.kind)
			continue
		}
		if c.chk != nil && !c.chk(a) {
			t.Errorf("%s: check failed: %+v", c.sql, a)
		}
	}
}

func TestOtherStatements(t *testing.T) {
	cases := []struct {
		sql  string
		kind sqlparse.Kind
		chk  func(s sqlparse.Statement) bool
	}{
		{"CREATE INDEX CONCURRENTLY IF NOT EXISTS i ON t USING btree (a, lower(b)) WHERE a > 0", sqlparse.KindCreateIndex,
			func(s sqlparse.Statement) bool {
				ci := s.CreateIndex
				return ci.Concurrent && ci.IfNotExists && ci.Name == "i" && ci.Partial && reflect.DeepEqual(ci.Columns, []string{"a", "lower(b)"})
			}},
		{"CREATE UNIQUE INDEX i ON t (a)", sqlparse.KindCreateIndex, func(s sqlparse.Statement) bool { return s.CreateIndex.Unique && !s.CreateIndex.Concurrent }},
		{"DROP INDEX CONCURRENTLY IF EXISTS i", sqlparse.KindDropIndex, func(s sqlparse.Statement) bool { return s.DropIndex.Concurrent && s.DropIndex.Names[0] == "i" }},
		{"DROP TABLE IF EXISTS a, s.b", sqlparse.KindDropTable, func(s sqlparse.Statement) bool {
			return s.DropTable.IfExists && len(s.DropTable.Tables) == 2 && s.DropTable.Tables[1].Schema == "s"
		}},
		{"ALTER TABLE t RENAME TO u", sqlparse.KindRenameTable, func(s sqlparse.Statement) bool { return s.RenameTable.NewName == "u" }},
		{"ALTER TABLE t RENAME COLUMN a TO b", sqlparse.KindRenameColumn, func(s sqlparse.Statement) bool { return s.RenameColumn.Old == "a" && s.RenameColumn.New == "b" }},
		{"ALTER TABLE t RENAME a TO b", sqlparse.KindRenameColumn, nil},
		{"ALTER SCHEMA a RENAME TO b", sqlparse.KindRenameSchema, func(s sqlparse.Statement) bool { return s.RenameSchema.New == "b" }},
		{"ALTER TYPE mood RENAME VALUE 'sad' TO 'ok'", sqlparse.KindAlterEnum, func(s sqlparse.Statement) bool {
			return s.AlterEnum.RenameOld == "sad" && s.AlterEnum.RenameNew == "ok"
		}},
		{"ALTER TYPE mood ADD VALUE 'meh'", sqlparse.KindAlterEnum, func(s sqlparse.Statement) bool { return s.AlterEnum.AddValue == "meh" }},
		{"SET lock_timeout = '10s'", sqlparse.KindSet, func(s sqlparse.Statement) bool {
			return s.Set.Name == "lock_timeout" && s.Set.Value == "'10s'" && !s.Set.Local
		}},
		{"SET LOCAL statement_timeout TO 0", sqlparse.KindSet, func(s sqlparse.Statement) bool { return s.Set.Local && s.Set.Value == "0" }},
		{"BEGIN", sqlparse.KindTransaction, func(s sqlparse.Statement) bool { return s.Transaction.Begin }},
		{"COMMIT", sqlparse.KindTransaction, func(s sqlparse.Statement) bool { return s.Transaction.Commit }},
		{"UPDATE t SET a = 1 WHERE b IS NULL", sqlparse.KindDML, func(s sqlparse.Statement) bool { return s.DML.Op == "update" && s.DML.Table.Name == "t" }},
		{"INSERT INTO t (a) SELECT a FROM u", sqlparse.KindDML, func(s sqlparse.Statement) bool { return s.DML.Op == "insert" && s.DML.FromSelect }},
		{"INSERT INTO t (a) VALUES (1)", sqlparse.KindDML, func(s sqlparse.Statement) bool { return !s.DML.FromSelect }},
		{"DELETE FROM t", sqlparse.KindDML, func(s sqlparse.Statement) bool { return s.DML.Op == "delete" }},
		{"CREATE VIEW v AS SELECT a FROM t", sqlparse.KindCreateView, func(s sqlparse.Statement) bool { return s.View.Name == "v" && s.View.Definition != "" }},
		{"DROP VIEW v", sqlparse.KindDropView, func(s sqlparse.Statement) bool { return s.View.Name == "v" }},
		{"CREATE TRIGGER tr BEFORE INSERT ON t FOR EACH ROW EXECUTE FUNCTION f()", sqlparse.KindCreateTrigger, func(s sqlparse.Statement) bool { return s.Trigger.Name == "tr" && s.Trigger.Table.Name == "t" }},
		{"DROP TRIGGER tr ON s.t", sqlparse.KindDropTrigger, func(s sqlparse.Statement) bool {
			return s.Trigger.Name == "tr" && s.Trigger.Table.Name == "t" && s.Trigger.Table.Schema == "s"
		}},
		{"CREATE EXTENSION IF NOT EXISTS citext", sqlparse.KindOther, nil},
		{"ALTER INDEX i RENAME TO j", sqlparse.KindOther, nil},
	}
	for _, c := range cases {
		st := parseOne(t, c.sql)
		if st.Kind != c.kind {
			t.Errorf("%s: kind = %v, want %v", c.sql, st.Kind, c.kind)
			continue
		}
		if c.chk != nil && !c.chk(st) {
			t.Errorf("%s: check failed: %+v", c.sql, st)
		}
	}
}

func TestPositionsAndComments(t *testing.T) {
	src := "-- header\n\n-- safe_sql:disable ban-drop-column\nALTER TABLE users DROP COLUMN name;\n\nCREATE INDEX idx ON users (a); -- trailing\n/* block\n * safe_sql:disable-file\n */\nSELECT 1"
	stmts, err := Parser{}.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 3 {
		t.Fatalf("got %d statements", len(stmts))
	}
	if stmts[0].Line != 4 || stmts[1].Line != 6 || stmts[2].Line != 10 {
		t.Errorf("lines = %d %d %d", stmts[0].Line, stmts[1].Line, stmts[2].Line)
	}
	if stmts[0].Raw != "ALTER TABLE users DROP COLUMN name" {
		t.Errorf("raw = %q", stmts[0].Raw)
	}
	if !reflect.DeepEqual(stmts[0].LeadingComments, []string{"header", "safe_sql:disable ban-drop-column"}) {
		t.Errorf("comments[0] = %q", stmts[0].LeadingComments)
	}
	if len(stmts[1].LeadingComments) != 0 {
		t.Errorf("comments[1] = %q", stmts[1].LeadingComments)
	}
	if !reflect.DeepEqual(stmts[2].LeadingComments, []string{"block", "safe_sql:disable-file"}) {
		t.Errorf("comments[2] = %q", stmts[2].LeadingComments)
	}
}

func TestParseError(t *testing.T) {
	_, err := Parser{}.Parse("SELECT 1;\nALTER TABLE t ALTER COLUMN c TYPE bogus(;")
	var pe *sqlparse.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T %v", err, err)
	}
	if pe.Line != 2 {
		t.Errorf("line = %d (%v)", pe.Line, pe)
	}
}
