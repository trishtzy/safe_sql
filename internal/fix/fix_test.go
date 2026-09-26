package fix

import (
	"sort"
	"strings"
	"testing"

	"github.com/trishtzy/safe_sql/internal/lint"
	"github.com/trishtzy/safe_sql/internal/migrate"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
	_ "github.com/trishtzy/safe_sql/internal/sqlparse/postgres"
)

const base = "-- +goose Up\nCREATE TABLE users (id bigserial PRIMARY KEY, email text, name text, meta text);\nCREATE TABLE orders (id bigserial PRIMARY KEY, user_id bigint);\n-- +goose Down\nDROP TABLE orders; DROP TABLE users;\n"

type fixCase struct {
	name    string
	tool    migrate.Tool
	file    string
	content string
	// want maps path -> expected content after fixing ("" = file unchanged).
	want map[string]string
	rule string
}

func runCase(t *testing.T, c fixCase) *Result {
	t.Helper()
	tool := c.tool
	if tool == "" {
		tool = migrate.ToolGoose
	}
	b, _ := migrate.Parse("db/0001_base.sql", base, tool)
	if tool == migrate.ToolGolangMigrate {
		b, _ = migrate.Parse("db/000001_base.up.sql", strings.SplitN(strings.TrimPrefix(base, "-- +goose Up\n"), "-- +goose Down", 2)[0], tool)
	}
	f, err := migrate.Parse(c.file, c.content, tool)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{DryRun: true, Lint: lint.Options{Engine: sqlparse.EnginePostgres, Tool: tool, PlainInTransaction: true, StartAfter: "0001", Enabled: []string{"require-concurrent-index-drop"}}}
	res, err := RunFiles([]*migrate.File{b, f}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func check(t *testing.T, cases []fixCase) {
	t.Helper()
	for _, c := range cases {
		res := runCase(t, c)
		for path, want := range c.want {
			got, ok := res.After[path]
			if !ok {
				var have []string
				for p := range res.After {
					have = append(have, p)
				}
				sort.Strings(have)
				t.Errorf("%s: missing %s; have %v", c.name, path, have)
				continue
			}
			if got != want {
				t.Errorf("%s: %s\n--- got ---\n%s--- want ---\n%s", c.name, path, got, want)
			}
		}
		for _, fd := range res.Remaining.Findings {
			if fd.RuleID == c.rule {
				t.Errorf("%s: %s still reported after fix at %s:%d: %s", c.name, c.rule, fd.File, fd.Line, fd.Message)
			}
			if fd.RuleID == "concurrent-index-in-transaction" {
				t.Errorf("%s: fix produced CONCURRENTLY inside a transaction: %s:%d", c.name, fd.File, fd.Line)
			}
		}
		if len(res.Changes) == 0 {
			t.Errorf("%s: no change produced", c.name)
		}
	}
}

func TestConcurrentIndex(t *testing.T) {
	check(t, []fixCase{
		{name: "goose sole statement gets directive", rule: "require-concurrent-index-creation",
			file: "db/0002_idx.sql", content: "-- +goose Up\nCREATE INDEX users_email_idx ON users (email);\n-- +goose Down\nDROP INDEX users_email_idx;\n",
			want: map[string]string{"db/0002_idx.sql": "-- +goose NO TRANSACTION\n-- +goose Up\nCREATE INDEX CONCURRENTLY users_email_idx ON users (email);\n-- +goose Down\nDROP INDEX users_email_idx;\n"}},
		{name: "goose multi statement moves to new file", rule: "require-concurrent-index-creation",
			file: "db/0002_idx.sql", content: "-- +goose Up\nALTER TABLE users ADD COLUMN age int;\nCREATE UNIQUE INDEX users_email_idx ON users (email);\n-- +goose Down\nDROP INDEX users_email_idx;\n",
			want: map[string]string{
				"db/0002_idx.sql": "-- +goose Up\nALTER TABLE users ADD COLUMN age int;\n-- +goose Down\nDROP INDEX users_email_idx;\n",
				"db/0003_create_users_email_idx_concurrently.sql": "-- +goose NO TRANSACTION\n-- +goose Up\nCREATE UNIQUE INDEX CONCURRENTLY users_email_idx ON users (email);\n\n-- +goose Down\nDROP INDEX CONCURRENTLY IF EXISTS users_email_idx;\n"}},
		{name: "sql-migrate directive on up line", tool: migrate.ToolSQLMigrate, rule: "require-concurrent-index-creation",
			file: "db/2_idx.sql", content: "-- +migrate Up\nCREATE INDEX i ON users (email);\n-- +migrate Down\nDROP INDEX i;\n",
			want: map[string]string{"db/2_idx.sql": "-- +migrate Up notransaction\nCREATE INDEX CONCURRENTLY i ON users (email);\n-- +migrate Down\nDROP INDEX i;\n"}},
		{name: "dbmate directive", tool: migrate.ToolDbmate, rule: "require-concurrent-index-creation",
			file: "db/20240101000002_idx.sql", content: "-- migrate:up\nCREATE INDEX i ON users (email);\n-- migrate:down\nDROP INDEX i;\n",
			want: map[string]string{"db/20240101000002_idx.sql": "-- migrate:up transaction:false\nCREATE INDEX CONCURRENTLY i ON users (email);\n-- migrate:down\nDROP INDEX i;\n"}},
		{name: "golang-migrate multi moves to new pair", tool: migrate.ToolGolangMigrate, rule: "require-concurrent-index-creation",
			file: "db/000002_idx.up.sql", content: "ALTER TABLE users ADD COLUMN age int;\nCREATE INDEX i ON users (email);\n",
			want: map[string]string{
				"db/000002_idx.up.sql":                     "ALTER TABLE users ADD COLUMN age int;\n",
				"db/000003_create_i_concurrently.up.sql":   "CREATE INDEX CONCURRENTLY i ON users (email);\n",
				"db/000003_create_i_concurrently.down.sql": "DROP INDEX CONCURRENTLY IF EXISTS i;\n"}},
		{name: "tern sole statement", tool: migrate.ToolTern, rule: "require-concurrent-index-creation",
			file: "db/002_idx.sql", content: "CREATE INDEX i ON users (email);\n---- create above / drop below ----\nDROP INDEX i;\n",
			want: map[string]string{"db/002_idx.sql": "---- tern: disable-tx ----\nCREATE INDEX CONCURRENTLY i ON users (email);\n---- create above / drop below ----\nDROP INDEX i;\n"}},
		{name: "drop index opt-in", rule: "require-concurrent-index-drop",
			file: "db/0002_idx.sql", content: "-- +goose Up\nDROP INDEX users_email_idx;\n-- +goose Down\nSELECT 1;\n",
			want: map[string]string{"db/0002_idx.sql": "-- +goose NO TRANSACTION\n-- +goose Up\nDROP INDEX CONCURRENTLY users_email_idx;\n-- +goose Down\nSELECT 1;\n"}},
	})
}

func TestConstraints(t *testing.T) {
	check(t, []fixCase{
		{name: "fk named", rule: "add-foreign-key-not-valid",
			file: "db/0002_fk.sql", content: "-- +goose Up\nALTER TABLE orders ADD CONSTRAINT orders_user_fk FOREIGN KEY (user_id) REFERENCES users (id);\n-- +goose Down\nALTER TABLE orders DROP CONSTRAINT orders_user_fk;\n",
			want: map[string]string{
				"db/0002_fk.sql":                      "-- +goose Up\nALTER TABLE orders ADD CONSTRAINT orders_user_fk FOREIGN KEY (user_id) REFERENCES users (id) NOT VALID;\n-- +goose Down\nALTER TABLE orders DROP CONSTRAINT orders_user_fk;\n",
				"db/0003_validate_orders_user_fk.sql": "-- +goose Up\nALTER TABLE orders VALIDATE CONSTRAINT orders_user_fk;\n\n-- +goose Down\nSELECT 1 -- nothing to undo;\n"}},
		{name: "fk unnamed gets a name", rule: "add-foreign-key-not-valid",
			file: "db/0002_fk.sql", content: "-- +goose Up\nALTER TABLE orders ADD FOREIGN KEY (user_id) REFERENCES users (id);\n-- +goose Down\nSELECT 1;\n",
			want: map[string]string{"db/0002_fk.sql": "-- +goose Up\nALTER TABLE orders ADD CONSTRAINT orders_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (id) NOT VALID;\n-- +goose Down\nSELECT 1;\n"}},
		{name: "check", rule: "add-check-constraint-not-valid",
			file: "db/0002_ck.sql", content: "-- +goose Up\nALTER TABLE users ADD CONSTRAINT email_ck CHECK (email <> '');\n-- +goose Down\nSELECT 1;\n",
			want: map[string]string{"db/0002_ck.sql": "-- +goose Up\nALTER TABLE users ADD CONSTRAINT email_ck CHECK (email <> '') NOT VALID;\n-- +goose Down\nSELECT 1;\n"}},
		{name: "unique in tx splits into two files", rule: "add-unique-constraint-via-index",
			file: "db/0002_uq.sql", content: "-- +goose Up\nALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);\nALTER TABLE users ADD COLUMN x int;\n-- +goose Down\nSELECT 1;\n",
			want: map[string]string{
				"db/0002_uq.sql": "-- +goose Up\nALTER TABLE users ADD COLUMN x int;\n-- +goose Down\nSELECT 1;\n",
				"db/0003_create_users_email_key_concurrently.sql": "-- +goose NO TRANSACTION\n-- +goose Up\nCREATE UNIQUE INDEX CONCURRENTLY users_email_key ON users (email);\n\n-- +goose Down\nDROP INDEX CONCURRENTLY IF EXISTS users_email_key;\n",
				"db/0004_attach_users_email_key.sql":              "-- +goose Up\nALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE USING INDEX users_email_key;\n\n-- +goose Down\nALTER TABLE users DROP CONSTRAINT users_email_key;\n"}},
		{name: "unique outside tx inline", rule: "add-unique-constraint-via-index",
			file: "db/0002_uq.sql", content: "-- +goose NO TRANSACTION\n-- +goose Up\nALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);\n-- +goose Down\nSELECT 1;\n",
			want: map[string]string{"db/0002_uq.sql": "-- +goose NO TRANSACTION\n-- +goose Up\nCREATE UNIQUE INDEX CONCURRENTLY users_email_key ON users (email);\nALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE USING INDEX users_email_key;\n-- +goose Down\nSELECT 1;\n"}},
		{name: "jsonb", rule: "prefer-jsonb",
			file: "db/0002_j.sql", content: "-- +goose Up\nALTER TABLE users ADD COLUMN settings json;\nCREATE TABLE t (\n  id int,\n  \"json\" json,\n  payload json NOT NULL\n);\n-- +goose Down\nSELECT 1;\n",
			want: map[string]string{"db/0002_j.sql": "-- +goose Up\nALTER TABLE users ADD COLUMN settings jsonb;\nCREATE TABLE t (\n  id int,\n  \"json\" jsonb,\n  payload jsonb NOT NULL\n);\n-- +goose Down\nSELECT 1;\n"}},
	})
}

func TestUnfixableAndPlain(t *testing.T) {
	res := runCase(t, fixCase{tool: migrate.ToolPlain, file: "db/schema2.sql", content: "ALTER TABLE users ADD COLUMN age int;\nCREATE INDEX i ON users (email);\nALTER TABLE users DROP COLUMN name;\n"})
	if len(res.Changes) != 0 {
		t.Errorf("plain files with unknown numbering must not get new files: %+v", res.Changes)
	}
	ids := map[string]bool{}
	for _, u := range res.Unfixed {
		ids[u.RuleID] = true
	}
	if !ids["ban-drop-column"] || !ids["require-concurrent-index-creation"] {
		t.Errorf("unfixed = %v", ids)
	}
}
