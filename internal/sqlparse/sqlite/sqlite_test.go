package sqlite

import (
	"reflect"
	"testing"

	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

func TestConvert(t *testing.T) {
	src := `-- header
CREATE TABLE users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  email TEXT NOT NULL UNIQUE,
  org_id INT REFERENCES orgs(id),
  ts TEXT DEFAULT CURRENT_TIMESTAMP,
  n VARCHAR(20) DEFAULT 'x',
  total INT GENERATED ALWAYS AS (id * 2) STORED,
  CONSTRAINT pk PRIMARY KEY (id),
  FOREIGN KEY (org_id) REFERENCES orgs (id)
);
PRAGMA foreign_keys = OFF;
-- safe_sql:disable ban-drop-column
ALTER TABLE users DROP COLUMN n;
ALTER TABLE users ADD COLUMN c TEXT NOT NULL DEFAULT 'a';
ALTER TABLE users RENAME COLUMN c TO d;
ALTER TABLE users RENAME TO people;
ALTER TABLE people ADD CONSTRAINT ck CHECK (id > 0);
CREATE UNIQUE INDEX i ON people (email, lower(d));
CREATE TRIGGER tr AFTER INSERT ON people BEGIN UPDATE people SET d = 'a'; END;
CREATE VIEW v AS SELECT email FROM people;
INSERT INTO people (email) SELECT email FROM old;
INSERT INTO people (email) VALUES ('a');
BEGIN; COMMIT;
DROP TABLE people;`
	stmts, err := Parser{}.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []sqlparse.Kind{}
	for _, s := range stmts {
		kinds = append(kinds, s.Kind)
	}
	want := []sqlparse.Kind{sqlparse.KindCreateTable, sqlparse.KindPragma, sqlparse.KindAlterTable, sqlparse.KindAlterTable, sqlparse.KindRenameColumn,
		sqlparse.KindRenameTable, sqlparse.KindAlterTable, sqlparse.KindCreateIndex, sqlparse.KindCreateTrigger, sqlparse.KindCreateView,
		sqlparse.KindDML, sqlparse.KindDML, sqlparse.KindTransaction, sqlparse.KindTransaction, sqlparse.KindDropTable}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("kinds = %v\nwant  %v", kinds, want)
	}
	ct := stmts[0].CreateTable
	cols := map[string]sqlparse.ColumnDef{}
	for _, c := range ct.Columns {
		cols[c.Name] = c
	}
	if c := cols["id"]; !c.PrimaryKey || !c.Serial || c.Type != "integer" {
		t.Errorf("id = %+v", c)
	}
	if c := cols["email"]; !c.NotNull || !c.Unique || c.Type != "text" {
		t.Errorf("email = %+v", c)
	}
	if c := cols["org_id"]; c.References == nil || c.References.Name != "orgs" || len(c.Constraints) != 1 {
		t.Errorf("org_id = %+v", c)
	}
	if c := cols["ts"]; c.Default == nil || c.Default.IsConstant || !reflect.DeepEqual(c.Default.Funcs, []string{"current_timestamp"}) {
		t.Errorf("ts = %+v %+v", c, c.Default)
	}
	if c := cols["n"]; c.Type != "varchar" || !reflect.DeepEqual(c.TypeMods, []int{20}) || !c.Default.IsConstant {
		t.Errorf("n = %+v", c)
	}
	if c := cols["total"]; c.Generated != "stored" || c.GeneratedExpr == nil || c.GeneratedExpr.Raw != "id * 2" {
		t.Errorf("total = %+v", c)
	}
	if len(ct.Constraints) != 2 || ct.Constraints[0].Kind != sqlparse.ConstraintPrimaryKey || ct.Constraints[0].Columns[0] != "id" ||
		ct.Constraints[1].Kind != sqlparse.ConstraintForeignKey || ct.Constraints[1].References.Name != "orgs" || ct.Constraints[1].RefColumns[0] != "id" {
		t.Errorf("constraints = %+v", ct.Constraints)
	}
	if p := stmts[1].Pragma; p.Name != "foreign_keys" || p.Value != "OFF" {
		t.Errorf("pragma = %+v", p)
	}
	if a := stmts[2].AlterTable.Actions[0]; a.Kind != sqlparse.ActionDropColumn || a.Column != "n" || stmts[2].Line != 14 ||
		!reflect.DeepEqual(stmts[2].LeadingComments, []string{"safe_sql:disable ban-drop-column"}) {
		t.Errorf("drop = %+v line=%d comments=%q", a, stmts[2].Line, stmts[2].LeadingComments)
	}
	if a := stmts[3].AlterTable.Actions[0]; a.Kind != sqlparse.ActionAddColumn || !a.ColumnDef.NotNull || a.ColumnDef.Default == nil {
		t.Errorf("add = %+v", a)
	}
	if r := stmts[4].RenameColumn; r.Old != "c" || r.New != "d" {
		t.Errorf("rename col = %+v", r)
	}
	if r := stmts[5].RenameTable; r.NewName != "people" {
		t.Errorf("rename table = %+v", r)
	}
	if a := stmts[6].AlterTable.Actions[0]; a.Kind != sqlparse.ActionUnsupported {
		t.Errorf("add constraint = %+v", a)
	}
	if ci := stmts[7].CreateIndex; !ci.Unique || ci.Table.Name != "people" || !reflect.DeepEqual(ci.Columns, []string{"email", "lower(d)"}) {
		t.Errorf("index = %+v", ci)
	}
	if tr := stmts[8].Trigger; tr.Name != "tr" || tr.Table.Name != "people" || tr.Definition == "" {
		t.Errorf("trigger = %+v", tr)
	}
	if v := stmts[9].View; v.Name != "v" || v.Definition == "" {
		t.Errorf("view = %+v", v)
	}
	if !stmts[10].DML.FromSelect || stmts[11].DML.FromSelect {
		t.Errorf("insert forms: %+v %+v", stmts[10].DML, stmts[11].DML)
	}
	if !stmts[12].Transaction.Begin || !stmts[13].Transaction.Commit {
		t.Error("transaction")
	}
	if stmts[14].Raw != "DROP TABLE people" {
		t.Errorf("raw = %q", stmts[14].Raw)
	}
}

func TestRecovery(t *testing.T) {
	src := "SELECT 1;\nALTER TABLE t ALTER COLUMN c TYPE int;\nSELECT 2;\nALTER TABLE t MODIFY c int;\nSELECT 3"
	stmts, err := Parser{}.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 5 {
		t.Fatalf("got %d statements: %+v", len(stmts), stmts)
	}
	if stmts[1].Kind != sqlparse.KindInvalid || stmts[1].Raw != "ALTER TABLE t ALTER COLUMN c TYPE int" || stmts[1].Line != 2 || stmts[1].Error == "" {
		t.Errorf("invalid = %+v", stmts[1])
	}
	if stmts[3].Kind != sqlparse.KindInvalid || stmts[3].Line != 4 {
		t.Errorf("invalid2 = %+v", stmts[3])
	}
	if stmts[2].Kind != sqlparse.KindDML || stmts[4].Kind != sqlparse.KindDML || stmts[4].Line != 5 {
		t.Errorf("selects = %+v %+v", stmts[2], stmts[4])
	}
	// Trigger bodies contain semicolons; they must not confuse recovery.
	src = "CREATE TRIGGER tr AFTER INSERT ON t BEGIN UPDATE t SET a = 1; DELETE FROM u; END;\nALTER TABLE t ALTER COLUMN c TYPE int;"
	stmts, err = Parser{}.Parse(src)
	if err != nil || len(stmts) != 2 || stmts[0].Kind != sqlparse.KindCreateTrigger || stmts[1].Kind != sqlparse.KindInvalid {
		t.Errorf("trigger recovery: %v %+v", err, stmts)
	}
}
