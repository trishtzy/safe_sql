package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSqlcDiscoveryV2(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "sqlc.yaml"), `version: "2"
sql:
  - name: app
    engine: postgresql
    schema: db/migrations
    queries: [db/queries, db/more]
    database:
      uri: postgres://x
    gen: { go: { package: db } }
  - engine: mysql
    schema: my/schema.sql
    queries: my/q
  - engine: sqlite
    schema: [lite.sql]
    queries: q.sql
`)
	sub := filepath.Join(dir, "cmd", "app")
	os.MkdirAll(sub, 0o755)
	p, err := Load(sub, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.SqlcPath != filepath.Join(dir, "sqlc.yaml") || p.ConfigPath != "" {
		t.Errorf("paths: sqlc=%s cfg=%s", p.SqlcPath, p.ConfigPath)
	}
	if len(p.Packages) != 2 || len(p.Skipped) != 1 {
		t.Fatalf("packages=%+v skipped=%v", p.Packages, p.Skipped)
	}
	want := Package{Name: "app", Engine: sqlparse.EnginePostgres,
		Schema: []string{filepath.Join(dir, "db/migrations")}, Queries: []string{filepath.Join(dir, "db/queries"), filepath.Join(dir, "db/more")},
		DatabaseURL: "postgres://x"}
	if !reflect.DeepEqual(p.Packages[0], want) {
		t.Errorf("pkg0 = %+v\nwant %+v", p.Packages[0], want)
	}
	if p.Packages[1].Name != "sql[2]" || p.Packages[1].Engine != sqlparse.EngineSQLite {
		t.Errorf("pkg1 = %+v", p.Packages[1])
	}
}

func TestSqlcV1AndJSON(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "sqlc.json"), `{"version":"1","packages":[{"name":"db","engine":"postgresql","schema":"schema.sql","queries":"query.sql","path":"db"}]}`)
	p, err := Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Packages) != 1 || p.Packages[0].Schema[0] != filepath.Join(dir, "schema.sql") {
		t.Errorf("packages = %+v", p.Packages)
	}
}

func TestSafeSQLOverridesAndValidation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SS_TEST_TOOL", "goose")
	write(t, filepath.Join(dir, "sqlc.yaml"), "version: \"2\"\nsql:\n  - engine: postgresql\n    schema: a\n    queries: b\n")
	write(t, filepath.Join(dir, "safe_sql.yaml"), `version: "1"
schema: [migrations]
migration_tool: ${SS_TEST_TOOL}
target_version: "15"
start_after: "0010"
check_down: true
plain_in_transaction: false
rules:
  disable: [prefer-jsonb]
  enable: [require-lock-timeout]
  severity: { ban-dml-in-migration: error }
verify:
  base_ref: develop
ai:
  enabled: true
  model: claude-opus-5
`)
	p, err := Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Packages) != 1 || p.Packages[0].Engine != sqlparse.EnginePostgres ||
		p.Packages[0].Schema[0] != filepath.Join(dir, "migrations") || p.Packages[0].Queries[0] != filepath.Join(dir, "b") {
		t.Errorf("packages = %+v", p.Packages)
	}
	if string(p.Tool) != "goose" || !p.TargetVersion.Known || p.TargetVersion.Major != 15 || p.StartAfter != "0010" || !p.CheckDown || p.PlainInTransaction {
		t.Errorf("project = %+v", p)
	}
	if p.Disabled[0] != "prefer-jsonb" || p.Enabled[0] != "require-lock-timeout" || p.Severity["ban-dml-in-migration"] != "error" {
		t.Errorf("rules = %v %v %v", p.Disabled, p.Enabled, p.Severity)
	}
	if p.Verify.BaseRef != "develop" || !p.AI.Enabled || p.AI.Model != "claude-opus-5" || p.AI.Trigger != "@safe_sql_ai" || p.AI.MaxIterations != 3 {
		t.Errorf("verify/ai = %+v %+v", p.Verify, p.AI)
	}

	write(t, filepath.Join(dir, "safe_sql.yaml"), "rules:\n  disable: [no-such-rule]\n")
	if _, err := Load(dir, ""); err == nil {
		t.Error("expected unknown rule error")
	}
	write(t, filepath.Join(dir, "safe_sql.yaml"), "bogus_key: 1\n")
	if _, err := Load(dir, ""); err == nil {
		t.Error("expected unknown key error")
	}
	write(t, filepath.Join(dir, "safe_sql.yaml"), "")
	if _, err := Load(dir, ""); err != nil {
		t.Errorf("empty config: %v", err)
	}
}

func TestNoConfigAtAll(t *testing.T) {
	dir := t.TempDir()
	p, err := Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Packages) != 0 || p.SqlcPath != "" {
		t.Errorf("project = %+v", p)
	}
	if s := Starter(p); s == "" {
		t.Error("empty starter")
	}
}
