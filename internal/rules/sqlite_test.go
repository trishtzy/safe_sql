package rules_test

import (
	"strings"
	"testing"

	"github.com/trishtzy/safe_sql/internal/lint"
	"github.com/trishtzy/safe_sql/internal/migrate"
	"github.com/trishtzy/safe_sql/internal/rules"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
	_ "github.com/trishtzy/safe_sql/internal/sqlparse/sqlite"
)

const sqliteBase = `
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE, name TEXT, age INT, code TEXT, total INT GENERATED ALWAYS AS (age * 2) VIRTUAL);
CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INT REFERENCES users(id), note TEXT);
CREATE INDEX users_code_idx ON users (code);
CREATE VIEW user_names AS SELECT name FROM users;
CREATE TRIGGER orders_note AFTER INSERT ON orders BEGIN UPDATE orders SET note = 'x' WHERE id = NEW.id; END;
CREATE TABLE plain (id INTEGER PRIMARY KEY, a TEXT);
`

var seenSQLite = map[string]bool{}

func runSQLite(t *testing.T, c tc) *lint.Result {
	t.Helper()
	opts := lint.Options{Engine: sqlparse.EngineSQLite, PlainInTransaction: true, StartAfter: "0001"}
	if c.opts != nil {
		c.opts(&opts)
	}
	name := c.fname
	if name == "" {
		name = "0002_change.sql"
	}
	base, _ := migrate.Parse("0001_base.sql", sqliteBase, migrate.ToolPlain)
	tool := c.tool
	if tool == "" {
		tool = migrate.ToolPlain
	}
	f, err := migrate.Parse(name, c.sql, tool)
	if err != nil {
		t.Fatal(err)
	}
	res, err := lint.RunFiles([]*migrate.File{base, f}, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range res.Findings {
		seenSQLite[fd.RuleID] = true
	}
	return res
}

func checkSQLite(t *testing.T, cases []tc) {
	t.Helper()
	for _, c := range cases {
		res := runSQLite(t, c)
		var got []string
		for _, f := range res.Findings {
			got = append(got, f.RuleID)
		}
		for _, fl := range res.Failures {
			got = append(got, "FAIL:"+fl.Message)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s:\n  got  %v\n  want %v", c.name, got, c.want)
		}
	}
}

func TestSQLiteRules(t *testing.T) {
	old := func(v string) func(*lint.Options) {
		return func(o *lint.Options) { o.TargetVersion, _ = rules.ParseVersion(v) }
	}
	noTx := func(o *lint.Options) { o.PlainInTransaction = false }
	rebuild := func(extra string) string {
		return "PRAGMA foreign_keys = OFF;\nBEGIN;\nCREATE TABLE users_new (id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE, name TEXT, age INT, code TEXT, total INT GENERATED ALWAYS AS (age * 2) VIRTUAL);\n" +
			"INSERT INTO users_new (id, email, name, age, code) SELECT id, email, name, age, code FROM users;\nDROP TABLE users;\nALTER TABLE users_new RENAME TO users;\n" + extra + "COMMIT;\nPRAGMA foreign_keys = ON;\n"
	}
	full := "CREATE INDEX users_code_idx ON users (code);\nCREATE VIEW user_names AS SELECT name FROM users;\nPRAGMA foreign_key_check;\n"
	checkSQLite(t, []tc{
		{name: "alter column type", sql: "ALTER TABLE users ALTER COLUMN age TYPE TEXT", want: []string{"sqlite-unsupported-alter"}},
		{name: "add constraint (sqlc form)", sql: "ALTER TABLE users ADD CONSTRAINT ck CHECK (age > 0)", want: []string{"sqlite-unsupported-alter"}},
		{name: "set not null (sqlc form)", sql: "ALTER TABLE users ALTER COLUMN name SET NOT NULL", want: []string{"sqlite-unsupported-alter"}},
		{name: "garbage is a parse failure", sql: "SELEC 1", want: []string{"FAIL:near \"SELEC\": syntax error"}},
		{name: "add column pk", sql: "ALTER TABLE users ADD COLUMN k INT PRIMARY KEY", want: []string{"sqlite-add-column-restrictions"}},
		{name: "add column unique", sql: "ALTER TABLE users ADD COLUMN k INT UNIQUE", want: []string{"sqlite-add-column-restrictions"}},
		{name: "add column current_timestamp", sql: "ALTER TABLE users ADD COLUMN ts TEXT DEFAULT CURRENT_TIMESTAMP", want: []string{"sqlite-add-column-restrictions"}},
		{name: "add column expr default", sql: "ALTER TABLE users ADD COLUMN ts TEXT DEFAULT (datetime('now'))", want: []string{"sqlite-add-column-restrictions"}},
		{name: "add column not null no default", sql: "ALTER TABLE users ADD COLUMN k TEXT NOT NULL", want: []string{"sqlite-add-column-restrictions"}},
		{name: "add column not null default null", sql: "ALTER TABLE users ADD COLUMN k TEXT NOT NULL DEFAULT NULL", want: []string{"sqlite-add-column-restrictions"}},
		{name: "add column stored", sql: "ALTER TABLE users ADD COLUMN k INT GENERATED ALWAYS AS (age) STORED", want: []string{"sqlite-add-column-restrictions"}},
		{name: "add column fk with default", sql: "ALTER TABLE users ADD COLUMN o INT REFERENCES orders(id) DEFAULT 1", want: []string{"sqlite-add-column-restrictions"}},
		{name: "add column ok", sql: "ALTER TABLE users ADD COLUMN k TEXT NOT NULL DEFAULT 'x'", want: nil},
		{name: "add column virtual ok", sql: "ALTER TABLE users ADD COLUMN k INT GENERATED ALWAYS AS (age) VIRTUAL", want: nil},
		{name: "drop column old sqlite", sql: "-- safe_sql:disable ban-drop-column\nALTER TABLE users DROP COLUMN name", want: []string{"sqlite-drop-column-restrictions"}, opts: old("3.34")},
		{name: "drop column plain ok", sql: "-- safe_sql:disable ban-drop-column\nALTER TABLE plain DROP COLUMN a", want: nil},
		{name: "drop indexed column", sql: "-- safe_sql:disable ban-drop-column\nALTER TABLE users DROP COLUMN code", want: []string{"sqlite-drop-column-restrictions"}},
		{name: "drop unique column", sql: "-- safe_sql:disable ban-drop-column\nALTER TABLE users DROP COLUMN email", want: []string{"sqlite-drop-column-restrictions"}},
		{name: "drop referenced column", sql: "-- safe_sql:disable ban-drop-column\nALTER TABLE users DROP COLUMN id", want: []string{"sqlite-drop-column-restrictions"}},
		{name: "drop column in view", sql: "-- safe_sql:disable ban-drop-column\nALTER TABLE users DROP COLUMN name", want: []string{"sqlite-drop-column-restrictions"}},
		{name: "drop column used by generated", sql: "-- safe_sql:disable ban-drop-column\nALTER TABLE users DROP COLUMN age", want: []string{"sqlite-drop-column-restrictions"}},
		{name: "drop column in trigger", sql: "-- safe_sql:disable ban-drop-column\nALTER TABLE orders DROP COLUMN note", want: []string{"sqlite-drop-column-restrictions"}},
		{name: "drop column app-breaking still reported", sql: "ALTER TABLE plain DROP COLUMN a", want: []string{"ban-drop-column"}},
		{name: "rename column old", sql: "-- safe_sql:disable ban-rename-column\nALTER TABLE users RENAME COLUMN name TO n", want: []string{"sqlite-rename-column-version"}, opts: old("3.24")},
		{name: "rename column new", sql: "-- safe_sql:disable ban-rename-column\nALTER TABLE users RENAME COLUMN name TO n", want: nil},
		{name: "pragma in tool tx", sql: "PRAGMA foreign_keys = OFF;\nSELECT 1", want: []string{"sqlite-pragma-in-transaction"}},
		{name: "pragma outside tx", sql: "PRAGMA foreign_keys = OFF;\nSELECT 1", want: nil, opts: noTx},
		{name: "pragma inside explicit tx", sql: "BEGIN;\nPRAGMA journal_mode = WAL;\nCOMMIT", want: []string{"sqlite-pragma-in-transaction"}, opts: noTx},
		{name: "pragma goose no tx", sql: "-- +goose NO TRANSACTION\n-- +goose Up\nPRAGMA foreign_keys = OFF;\nSELECT 1;\n-- +goose Down\nSELECT 1;", want: nil, tool: migrate.ToolGoose},
		{name: "rebuild complete", sql: rebuild(full), want: []string{"sqlite-rebuild-locks-database"}, opts: noTx},
		{name: "rebuild missing index and view", sql: rebuild("PRAGMA foreign_key_check;\n"), want: []string{"sqlite-rebuild-locks-database", "sqlite-table-rebuild"}, opts: noTx},
		{name: "rebuild without fk check", sql: rebuild("CREATE INDEX users_code_idx ON users (code);\nCREATE VIEW user_names AS SELECT name FROM users;\n"), want: []string{"sqlite-rebuild-locks-database", "sqlite-table-rebuild"}, opts: noTx},
		{name: "rebuild inside tool tx has ignored pragma", sql: rebuild(full), want: []string{"sqlite-pragma-in-transaction", "sqlite-rebuild-locks-database", "sqlite-pragma-in-transaction"}},
		{name: "rebuild no tx at all", sql: strings.ReplaceAll(strings.ReplaceAll(rebuild(full), "BEGIN;\n", ""), "COMMIT;\n", ""), want: []string{"sqlite-rebuild-locks-database", "sqlite-table-rebuild"}, opts: noTx},
		{name: "rebuild without fk off", sql: strings.ReplaceAll(rebuild(full), "PRAGMA foreign_keys = OFF;\n", ""), want: []string{"sqlite-rebuild-locks-database", "sqlite-table-rebuild"}, opts: noTx},
	})
	for _, r := range rules.ForEngine(sqlparse.EngineSQLite) {
		if strings.HasPrefix(r.ID, "sqlite-") && !seenSQLite[r.ID] {
			t.Errorf("%s: no test case produces this finding", r.ID)
		}
	}
	// Postgres-only rules must not register for sqlite.
	for _, r := range rules.ForEngine(sqlparse.EngineSQLite) {
		if r.ID == "require-concurrent-index-creation" || r.ID == "prefer-jsonb" {
			t.Errorf("%s registered for sqlite", r.ID)
		}
	}
}
