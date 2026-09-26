package rules_test

import (
	"strings"
	"testing"

	"github.com/trishtzy/safe_sql/internal/lint"
	"github.com/trishtzy/safe_sql/internal/migrate"
	"github.com/trishtzy/safe_sql/internal/rules"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
	_ "github.com/trishtzy/safe_sql/internal/sqlparse/postgres"
)

// existing is the schema every case starts from, applied as an earlier
// migration so tables count as pre-existing.
const existing = `
CREATE TABLE users (id bigserial PRIMARY KEY, name text, email varchar(100), age int, amount numeric(10,2), settings text, ts timestamp(3), addr cidr, iv interval(3));
CREATE TABLE orders (id bigserial PRIMARY KEY, user_id bigint, status text);
CREATE INDEX users_email_idx ON users (email);
CREATE TYPE mood AS ENUM ('sad', 'ok');
`

type tc struct {
	name  string
	sql   string
	want  []string // rule IDs expected, in order; nil = clean
	opts  func(*lint.Options)
	tool  migrate.Tool
	fname string
}

// seen records every rule ID that produced a finding in any case, so the
// coverage test can prove each rule has at least one "bad" case.
var seen = map[string]bool{}

func run(t *testing.T, c tc) *lint.Result {
	t.Helper()
	opts := lint.Options{Engine: sqlparse.EnginePostgres, PlainInTransaction: true, StartAfter: "0001", Enabled: []string{"require-concurrent-index-drop"}}
	if c.opts != nil {
		c.opts(&opts)
	}
	name := c.fname
	if name == "" {
		name = "0002_change.sql"
	}
	base, _ := migrate.Parse("0001_base.sql", existing, migrate.ToolPlain)
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
	for _, fl := range res.Failures {
		t.Fatalf("%s: parse failure: %+v", c.name, fl)
	}
	for _, f := range res.Findings {
		seen[f.RuleID] = true
	}
	return res
}

func check(t *testing.T, cases []tc) {
	t.Helper()
	for _, c := range cases {
		res := run(t, c)
		var got []string
		for _, f := range res.Findings {
			if f.File == "0001_base.sql" {
				t.Errorf("%s: finding in base schema: %s", c.name, f.RuleID)
			}
			got = append(got, f.RuleID)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s:\n  got  %v\n  want %v", c.name, got, c.want)
		}
	}
}

var noTx = func(o *lint.Options) { o.PlainInTransaction = false }

func TestCommonRules(t *testing.T) {
	check(t, []tc{
		{name: "drop column", sql: "ALTER TABLE users DROP COLUMN name", want: []string{"ban-drop-column"}},
		{name: "drop column on new table", sql: "CREATE TABLE t (a int, b int); ALTER TABLE t DROP COLUMN b", want: nil},
		{name: "rename column", sql: "ALTER TABLE users RENAME COLUMN name TO full_name", want: []string{"ban-rename-column"}},
		{name: "rename table", sql: "ALTER TABLE users RENAME TO accounts", want: []string{"ban-rename-table"}},
		{name: "drop table", sql: "DROP TABLE orders", want: []string{"ban-drop-table"}},
		{name: "drop+create table", sql: "DROP TABLE orders; CREATE TABLE orders (id int)", want: []string{"ban-drop-table"}},
		{name: "serial column", sql: "ALTER TABLE users ADD COLUMN n bigserial", want: []string{"ban-add-serial-column"}},
		{name: "identity column", sql: "ALTER TABLE users ADD COLUMN n bigint GENERATED ALWAYS AS IDENTITY", want: []string{"ban-add-serial-column"}},
		{name: "stored generated", sql: "ALTER TABLE users ADD COLUMN n int GENERATED ALWAYS AS (age * 2) STORED", want: []string{"ban-add-stored-generated-column"}},
		{name: "fk validated", sql: "ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (user_id) REFERENCES users (id)", want: []string{"add-foreign-key-not-valid"}},
		{name: "fk not valid", sql: "ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (user_id) REFERENCES users (id) NOT VALID; ALTER TABLE orders VALIDATE CONSTRAINT fk", want: nil},
		{name: "fk inline add column", sql: "ALTER TABLE orders ADD COLUMN shop_id bigint REFERENCES shops (id)", want: []string{"add-foreign-key-not-valid"}},
		{name: "check validated", sql: "ALTER TABLE users ADD CONSTRAINT age_ck CHECK (age >= 0)", want: []string{"add-check-constraint-not-valid"}},
		{name: "check not valid", sql: "ALTER TABLE users ADD CONSTRAINT age_ck CHECK (age >= 0) NOT VALID", want: nil},
		{name: "dml with ddl", sql: "ALTER TABLE users ADD COLUMN x int; UPDATE users SET x = 1", want: []string{"ban-dml-in-migration"}},
		{name: "dml alone", sql: "UPDATE users SET age = 1", want: nil},
		{name: "seed data into new table", sql: "CREATE TABLE roles (id int); INSERT INTO roles VALUES (1)", want: nil},
		{name: "scratch table", sql: "CREATE TABLE tmp (id int); DROP TABLE tmp", want: nil},
		{name: "wide index", sql: "CREATE INDEX CONCURRENTLY i ON users (a, b, c, d)", want: []string{"index-too-many-columns"}, opts: noTx},
		{name: "wide unique index", sql: "CREATE UNIQUE INDEX CONCURRENTLY i ON users (a, b, c, d)", want: nil, opts: noTx},
	})
}

func TestChangeColumnType(t *testing.T) {
	check(t, []tc{
		{name: "int to bigint", sql: "ALTER TABLE users ALTER COLUMN age TYPE bigint", want: []string{"ban-change-column-type"}},
		{name: "varchar widen", sql: "ALTER TABLE users ALTER COLUMN email TYPE varchar(200)", want: nil},
		{name: "varchar narrow", sql: "ALTER TABLE users ALTER COLUMN email TYPE varchar(50)", want: []string{"ban-change-column-type"}},
		{name: "varchar to text", sql: "ALTER TABLE users ALTER COLUMN email TYPE text", want: nil},
		{name: "text to varchar unlimited", sql: "ALTER TABLE users ALTER COLUMN name TYPE varchar", want: nil},
		{name: "text to varchar limited", sql: "ALTER TABLE users ALTER COLUMN name TYPE varchar(10)", want: []string{"ban-change-column-type"}},
		{name: "text to citext indexed", sql: "ALTER TABLE users ALTER COLUMN email TYPE citext", want: []string{"ban-change-column-type"}},
		{name: "text to citext unindexed", sql: "ALTER TABLE users ALTER COLUMN name TYPE citext", want: nil},
		{name: "numeric precision up", sql: "ALTER TABLE users ALTER COLUMN amount TYPE numeric(12,2)", want: nil},
		{name: "numeric scale change", sql: "ALTER TABLE users ALTER COLUMN amount TYPE numeric(12,3)", want: []string{"ban-change-column-type"}},
		{name: "numeric unconstrained", sql: "ALTER TABLE users ALTER COLUMN amount TYPE numeric", want: nil},
		{name: "timestamp precision up", sql: "ALTER TABLE users ALTER COLUMN ts TYPE timestamp(6)", want: nil},
		{name: "timestamp to tz", sql: "ALTER TABLE users ALTER COLUMN ts TYPE timestamptz", want: []string{"ban-change-column-type"}},
		{name: "interval precision up", sql: "ALTER TABLE users ALTER COLUMN iv TYPE interval(6)", want: nil},
		{name: "cidr to inet", sql: "ALTER TABLE users ALTER COLUMN addr TYPE inet", want: nil},
		{name: "unknown column", sql: "ALTER TABLE users ALTER COLUMN nope TYPE text", want: []string{"ban-change-column-type"}},
		{name: "new table", sql: "CREATE TABLE t (a int); ALTER TABLE t ALTER COLUMN a TYPE bigint", want: nil},
	})
}

func TestPostgresRules(t *testing.T) {
	pg10 := func(o *lint.Options) { o.TargetVersion, _ = rules.ParseVersion("10") }
	pg11 := func(o *lint.Options) { o.TargetVersion, _ = rules.ParseVersion("11") }
	check(t, []tc{
		{name: "index not concurrent", sql: "CREATE INDEX i ON users (name)", want: []string{"require-concurrent-index-creation"}},
		{name: "index concurrent outside tx", sql: "CREATE INDEX CONCURRENTLY i ON users (name)", want: nil,
			opts: func(o *lint.Options) { o.PlainInTransaction = false }},
		{name: "index concurrent in tx", sql: "CREATE INDEX CONCURRENTLY i ON users (name)", want: []string{"concurrent-index-in-transaction"}},
		{name: "index concurrent goose no tx", sql: "-- +goose NO TRANSACTION\n-- +goose Up\nCREATE INDEX CONCURRENTLY i ON users (name)", want: nil, tool: migrate.ToolGoose},
		{name: "index concurrent goose tx", sql: "-- +goose Up\nCREATE INDEX CONCURRENTLY i ON users (name)", want: []string{"concurrent-index-in-transaction"}, tool: migrate.ToolGoose},
		{name: "golang-migrate single stmt", sql: "CREATE INDEX CONCURRENTLY i ON users (name)", want: nil, tool: migrate.ToolGolangMigrate, fname: "000002_x.up.sql"},
		{name: "golang-migrate two stmts", sql: "CREATE INDEX CONCURRENTLY i ON users (name); SELECT 1", want: []string{"concurrent-index-in-transaction"}, tool: migrate.ToolGolangMigrate, fname: "000002_x.up.sql"},
		{name: "explicit begin", sql: "BEGIN; CREATE INDEX CONCURRENTLY i ON users (name); COMMIT", want: []string{"concurrent-index-in-transaction"},
			opts: func(o *lint.Options) { o.PlainInTransaction = false }},
		{name: "index on new table", sql: "CREATE TABLE t (a int); CREATE INDEX i ON t (a)", want: nil},
		{name: "unique constraint", sql: "ALTER TABLE users ADD CONSTRAINT u UNIQUE (email)", want: []string{"add-unique-constraint-via-index"}},
		{name: "unique using index", sql: "ALTER TABLE users ADD CONSTRAINT u UNIQUE USING INDEX users_email_idx", want: nil},
		{name: "exclusion", sql: "ALTER TABLE users ADD CONSTRAINT ex EXCLUDE USING gist (name WITH =)", want: []string{"ban-exclusion-constraint"}},
		{name: "json add", sql: "ALTER TABLE users ADD COLUMN j json", want: []string{"prefer-jsonb"}},
		{name: "json create", sql: "CREATE TABLE t (j json)", want: []string{"prefer-jsonb"}},
		{name: "jsonb", sql: "ALTER TABLE users ADD COLUMN j jsonb", want: nil},
		{name: "volatile default", sql: "ALTER TABLE users ADD COLUMN tok uuid DEFAULT gen_random_uuid()", want: []string{"add-column-volatile-default"}},
		{name: "random default", sql: "ALTER TABLE users ADD COLUMN r float DEFAULT random()", want: []string{"add-column-volatile-default"}},
		{name: "stable default", sql: "ALTER TABLE users ADD COLUMN c timestamptz DEFAULT now()", want: nil},
		{name: "current_timestamp default", sql: "ALTER TABLE users ADD COLUMN c timestamptz DEFAULT CURRENT_TIMESTAMP", want: nil},
		{name: "constant default pg11", sql: "ALTER TABLE users ADD COLUMN c int DEFAULT 0", want: nil, opts: pg11},
		{name: "constant default pg10", sql: "ALTER TABLE users ADD COLUMN c int DEFAULT 0", want: []string{"add-column-volatile-default"}, opts: pg10},
		{name: "unknown func default", sql: "ALTER TABLE users ADD COLUMN c text DEFAULT my_func()", want: []string{"add-column-volatile-default"}},
		{name: "default on new table", sql: "CREATE TABLE t (a int); ALTER TABLE t ADD COLUMN u uuid DEFAULT gen_random_uuid()", want: nil},
		{name: "set not null bare", sql: "ALTER TABLE users ALTER COLUMN name SET NOT NULL", want: []string{"set-not-null-without-check"}},
		{name: "set not null with validated check", sql: "ALTER TABLE users ADD CONSTRAINT nn CHECK (name IS NOT NULL) NOT VALID; ALTER TABLE users VALIDATE CONSTRAINT nn; ALTER TABLE users ALTER COLUMN name SET NOT NULL; ALTER TABLE users DROP CONSTRAINT nn", want: nil},
		{name: "set not null with unvalidated check", sql: "ALTER TABLE users ADD CONSTRAINT nn CHECK (name IS NOT NULL) NOT VALID; ALTER TABLE users ALTER COLUMN name SET NOT NULL", want: []string{"set-not-null-without-check"}},
		{name: "set not null pg11", sql: "ALTER TABLE users ADD CONSTRAINT nn CHECK (name IS NOT NULL) NOT VALID; ALTER TABLE users VALIDATE CONSTRAINT nn; ALTER TABLE users ALTER COLUMN name SET NOT NULL", want: []string{"set-not-null-without-check"}, opts: pg11},
		{name: "rename enum", sql: "ALTER TYPE mood RENAME VALUE 'sad' TO 'down'", want: []string{"ban-rename-enum-value"}},
		{name: "add enum", sql: "ALTER TYPE mood ADD VALUE 'meh'", want: nil},
		{name: "rename schema", sql: "ALTER SCHEMA public RENAME TO old", want: []string{"ban-rename-schema"}},
		{name: "drop index plain (opt-in enabled)", sql: "DROP INDEX users_email_idx", want: []string{"require-concurrent-index-drop"}},
		{name: "drop index concurrently", sql: "DROP INDEX CONCURRENTLY users_email_idx", want: nil, opts: func(o *lint.Options) { o.PlainInTransaction = false }},
		{name: "lock timeout missing", sql: "ALTER TABLE users DROP COLUMN name", want: []string{"ban-drop-column", "require-lock-timeout"},
			opts: func(o *lint.Options) { o.Enabled = []string{"require-lock-timeout"} }},
		{name: "lock timeout present", sql: "SET lock_timeout = '10s'; ALTER TABLE users DROP COLUMN name", want: []string{"ban-drop-column"},
			opts: func(o *lint.Options) { o.Enabled = []string{"require-lock-timeout"} }},
	})
}

func TestDisablesAndConfig(t *testing.T) {
	check(t, []tc{
		{name: "stmt disable one", sql: "-- safe_sql:disable ban-drop-column\nALTER TABLE users DROP COLUMN name", want: nil},
		{name: "stmt disable other", sql: "-- safe_sql:disable ban-rename-column\nALTER TABLE users DROP COLUMN name", want: []string{"ban-drop-column"}},
		{name: "stmt disable all", sql: "-- safe_sql:disable\nALTER TABLE users DROP COLUMN name", want: nil},
		{name: "stmt disable only applies to next", sql: "-- safe_sql:disable\nSELECT 1;\nALTER TABLE users DROP COLUMN name", want: []string{"ban-drop-column"}},
		{name: "file disable", sql: "-- safe_sql:disable-file ban-drop-column\nSELECT 1;\nALTER TABLE users DROP COLUMN name", want: nil},
		{name: "file disable all", sql: "-- safe_sql:disable-file\nALTER TABLE users DROP COLUMN name; ALTER TABLE users RENAME TO u", want: nil},
		{name: "config disable", sql: "ALTER TABLE users DROP COLUMN name", want: nil, opts: func(o *lint.Options) { o.Disabled = []string{"ban-drop-column"} }},
		{name: "start_after skips", sql: "ALTER TABLE users DROP COLUMN name", want: nil, opts: func(o *lint.Options) { o.StartAfter = "0002" }},
		{name: "start_after numeric compare", sql: "ALTER TABLE users DROP COLUMN name", want: []string{"ban-drop-column"}, fname: "10_x.sql", opts: func(o *lint.Options) { o.StartAfter = "0009" }},
		{name: "start_after passes", sql: "ALTER TABLE users DROP COLUMN name", want: []string{"ban-drop-column"}, opts: func(o *lint.Options) { o.StartAfter = "0001" }},
		{name: "only", sql: "ALTER TABLE users DROP COLUMN name; ALTER TABLE users RENAME TO u", want: []string{"ban-rename-table"}, opts: func(o *lint.Options) { o.Only = []string{"ban-rename-table"} }},
	})
	res := run(t, tc{sql: "ALTER TABLE users DROP COLUMN name", opts: func(o *lint.Options) {
		o.Severity = map[string]rules.Severity{"ban-drop-column": rules.SeverityWarning}
	}})
	if res.Errors() != 0 || res.Warnings() != 1 {
		t.Errorf("severity override: errors=%d warnings=%d", res.Errors(), res.Warnings())
	}
	res = run(t, tc{sql: "-- +goose Up\nSELECT 1;\n-- +goose Down\nALTER TABLE users DROP COLUMN name", tool: migrate.ToolGoose, opts: func(o *lint.Options) { o.CheckDown = true }})
	if len(res.Findings) != 1 || !res.Findings[0].Down || res.Findings[0].Line != 4 {
		t.Errorf("check_down: %+v", res.Findings)
	}
}

// TestZZEveryRuleCovered runs last (Go runs tests in source order) and
// fails if any Postgres rule never fired in the cases above.
func TestZZEveryRuleCovered(t *testing.T) {
	for _, r := range rules.ForEngine(sqlparse.EnginePostgres) {
		if r.Guidance == "" || r.Summary == "" {
			t.Errorf("%s: missing summary or guidance", r.ID)
		}
		if !seen[r.ID] {
			t.Errorf("%s: no test case produces this finding", r.ID)
		}
	}
}
