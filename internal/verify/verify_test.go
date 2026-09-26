package verify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/trishtzy/safe_sql/internal/catalog"
	"github.com/trishtzy/safe_sql/internal/migrate"
	_ "github.com/trishtzy/safe_sql/internal/sqlparse/postgres"
)

func TestParseVetOutput(t *testing.T) {
	out := `# package app
against/db/queries/authors.sql:12:8: column "name" does not exist
against/db/queries/authors.sql: GetAuthor: sqlc/db-prepare: error preparing query: ERROR: column "name" does not exist (SQLSTATE 42703)
against/db/queries/authors.sql: ListAuthors: no-delete: don't use delete
random noise
`
	root, _ := os.Getwd()
	bs := ParseVetOutput(out, "against/", root)
	if len(bs) != 3 {
		t.Fatalf("got %d breakages: %+v", len(bs), bs)
	}
	if !bs[0].Static || bs[0].Line != 12 || bs[0].QueryFile != filepath.Join("db", "queries", "authors.sql") {
		t.Errorf("static = %+v", bs[0])
	}
	if bs[1].QueryName != "GetAuthor" || bs[1].Static || bs[1].Message == "" {
		t.Errorf("prepare = %+v", bs[1])
	}
	if bs[2].QueryName != "ListAuthors" || bs[2].Message != "no-delete: don't use delete" {
		t.Errorf("rule = %+v", bs[2])
	}
}

func TestAttributeAndQuerySQL(t *testing.T) {
	f1, _ := migrate.Parse("m/0001.sql", "CREATE TABLE authors (id int, name text); CREATE TABLE books (id int);", migrate.ToolPlain)
	f2, _ := migrate.Parse("m/0002.sql", "ALTER TABLE books ADD COLUMN title text;", migrate.ToolPlain)
	f3, _ := migrate.Parse("m/0003.sql", "ALTER TABLE authors DROP COLUMN name;", migrate.ToolPlain)
	files := []*migrate.File{f1, f2, f3}
	newFiles := []string{"m/0002.sql", "m/0003.sql"}
	got := Attribute("SELECT name FROM authors WHERE id = $1", newFiles, files, catalog.New())
	if got != "m/0003.sql" {
		t.Errorf("attribute = %q", got)
	}
	f4, _ := migrate.Parse("m/0004.sql", "ALTER TABLE authors ADD COLUMN x int;", migrate.ToolPlain)
	got = Attribute("SELECT name FROM authors", []string{"m/0003.sql", "m/0004.sql"}, append(files, f4), catalog.New())
	if got != "m/0003.sql" {
		t.Errorf("column-level match should outrank later table-level touch: %q", got)
	}
	if got := Attribute("SELECT 1", []string{"m/0003.sql"}, files, catalog.New()); got != "m/0003.sql" {
		t.Errorf("single new file fallback = %q", got)
	}
	if got := Attribute("SELECT 1", newFiles, files, catalog.New()); got != "" {
		t.Errorf("no match = %q", got)
	}

	dir := t.TempDir()
	q := filepath.Join(dir, "q.sql")
	os.WriteFile(q, []byte("-- name: A :one\nSELECT 1;\n\n-- name: B :many\nSELECT 2\nFROM t;\n"), 0o644)
	if s := querySQL(q, "B"); s != "-- name: B :many\nSELECT 2\nFROM t;" {
		t.Errorf("querySQL = %q", s)
	}
	if n, s := queryAtLine(q, 6); n != "B" || s == "" {
		t.Errorf("queryAtLine(6) = %q %q", n, s)
	}
	if n, _ := queryAtLine(q, 2); n != "A" {
		t.Errorf("queryAtLine(2) = %q", n)
	}
}

func TestTempConfig(t *testing.T) {
	cfg := tempSqlcConfig("/tmp/x", packageFor(t), []string{"db/queries/a.sql"}, "postgres://u:p@h/db")
	for _, want := range []string{"engine: postgresql", `- "against/db/queries/a.sql"`, "sqlc/db-prepare", `uri: "postgres://u:p@h/db"`} {
		if !contains(cfg, want) {
			t.Errorf("config missing %q:\n%s", want, cfg)
		}
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
