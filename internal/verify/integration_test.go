//go:build integration

package verify

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trishtzy/safe_sql/internal/lint"
	"github.com/trishtzy/safe_sql/internal/migrate"
	_ "github.com/trishtzy/safe_sql/internal/sqlparse/postgres"
)

// TestVerifyEndToEnd builds a git repo with a deployed query on main, adds a
// migration that breaks it, and expects verify to attribute the breakage.
//
// Database: SAFE_SQL_TEST_DATABASE_URL if set (any empty Postgres database),
// else a Docker container via testcontainers. Requires sqlc on PATH.
func TestVerifyEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("sqlc"); err != nil {
		t.Skip("sqlc not on PATH")
	}
	dbURL := os.Getenv("SAFE_SQL_TEST_DATABASE_URL")
	if dbURL == "" {
		if _, err := exec.LookPath("docker"); err != nil || exec.Command("docker", "info").Run() != nil {
			t.Skip("no SAFE_SQL_TEST_DATABASE_URL and Docker not available")
		}
	}

	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "commit.gpgsign", "false")
	write("db/migrations/0001_init.sql", "-- +goose Up\nCREATE TABLE authors (id bigserial PRIMARY KEY, name text NOT NULL, bio text);\n-- +goose Down\nDROP TABLE authors;\n")
	write("db/queries/authors.sql", "-- name: GetAuthor :one\nSELECT id, name, bio FROM authors WHERE id = $1;\n\n-- name: CountAuthors :one\nSELECT count(*) FROM authors;\n")
	git("add", "-A")
	git("commit", "-q", "-m", "deployed")

	// Proposed change: rename a column the deployed query reads, plus an
	// unrelated safe migration.
	write("db/migrations/0002_rename.sql", "-- +goose Up\nALTER TABLE authors RENAME COLUMN bio TO biography;\n-- +goose Down\nALTER TABLE authors RENAME COLUMN biography TO bio;\n")
	write("db/migrations/0003_add.sql", "-- +goose Up\nALTER TABLE authors ADD COLUMN born int;\n-- +goose Down\nALTER TABLE authors DROP COLUMN born;\n")
	// The developer also updated the query in the working tree; verify must
	// still test the *deployed* version from main.
	write("db/queries/authors.sql", "-- name: GetAuthor :one\nSELECT id, name, biography FROM authors WHERE id = $1;\n")

	wd, _ := os.Getwd()
	os.Chdir(repo)
	defer os.Chdir(wd)

	pkg := packageFor(t)
	res, err := Run(context.Background(), Options{
		Package: pkg, Against: "main", DatabaseURL: dbURL, Docker: dbURL == "",
		Lint: lint.Options{Tool: migrate.ToolAuto, PlainInTransaction: true}, Stderr: os.Stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.NewFiles) != 2 {
		t.Errorf("new files = %v", res.NewFiles)
	}
	if len(res.Breakages) == 0 {
		t.Fatalf("expected GetAuthor to break; sqlc output:\n%s", res.SqlcOutput)
	}
	for _, b := range res.Breakages {
		if b.QueryName == "CountAuthors" {
			t.Errorf("CountAuthors should still work: %+v", b)
		}
		if !strings.Contains(b.QueryFile, "authors.sql") {
			t.Errorf("query file = %q", b.QueryFile)
		}
		if b.MigrationFile != filepath.Join("db", "migrations", "0002_rename.sql") {
			t.Errorf("attributed to %q, want 0002_rename.sql (%+v)", b.MigrationFile, b)
		}
	}
	var sawRename, sawBreak bool
	for _, f := range res.Lint.Findings {
		switch f.RuleID {
		case "ban-rename-column":
			sawRename = true
		case RuleID:
			sawBreak = true
		}
		if f.File == filepath.Join("db", "migrations", "0001_init.sql") {
			t.Errorf("finding in deployed migration: %+v", f)
		}
	}
	if !sawRename || !sawBreak {
		t.Errorf("findings = %+v", res.Lint.Findings)
	}
	t.Logf("sqlc output:\n%s", res.SqlcOutput)
}
