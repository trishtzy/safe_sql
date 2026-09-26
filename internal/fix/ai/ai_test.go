package ai

import (
	"context"
	"strings"
	"testing"

	"github.com/trishtzy/safe_sql/internal/lint"
	"github.com/trishtzy/safe_sql/internal/migrate"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
	_ "github.com/trishtzy/safe_sql/internal/sqlparse/postgres"
)

const base = "-- +goose Up\nCREATE TABLE users (id bigserial PRIMARY KEY, name text, email text);\n-- +goose Down\nDROP TABLE users;\n"
const rename = "-- +goose Up\nALTER TABLE users RENAME COLUMN name TO full_name;\n-- +goose Down\nALTER TABLE users RENAME COLUMN full_name TO name;\n"

func load(t *testing.T, tool migrate.Tool, files map[string]string) []*migrate.File {
	t.Helper()
	var out []*migrate.File
	for _, p := range []string{"db/0001_base.sql", "db/0002_rename.sql"} {
		if c, ok := files[p]; ok {
			f, err := migrate.Parse(p, c, tool)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, f)
		}
	}
	return out
}

func opts(p Provider) Options {
	return Options{
		Provider: p, MaxIterations: 3, SchemaDirs: []string{"db"}, DeterministicFirst: true, DryRun: true,
		Lint: lint.Options{Engine: sqlparse.EnginePostgres, Tool: migrate.ToolGoose, PlainInTransaction: true, StartAfter: "0001"},
	}
}

func TestLoopAppliesValidProposal(t *testing.T) {
	good := "-- +goose Up\nALTER TABLE users ADD COLUMN full_name text;\n-- +goose Down\nALTER TABLE users DROP COLUMN full_name;\n"
	backfill := "-- +goose Up\nUPDATE users SET full_name = name WHERE full_name IS NULL;\n-- +goose Down\nSELECT 1;\n"
	fake := &Fake{Responses: []*Response{{Model: "m", Proposal: &Proposal{Summary: "add column, backfill later", Files: []ProposedFile{
		{Path: "db/0002_rename.sql", Action: "modify", Content: good},
		{Path: "db/0003_backfill_full_name.sql", Action: "create", Content: backfill},
	}}}}}
	res, err := RunFiles(context.Background(), load(t, migrate.ToolGoose, map[string]string{"db/0001_base.sql": base, "db/0002_rename.sql": rename}), opts(fake))
	if err != nil {
		t.Fatal(err)
	}
	if res.Iterations != 1 || len(res.Modified) != 1 || len(res.Created) != 1 || len(res.Rejected) != 0 {
		t.Errorf("result = %+v", res)
	}
	if res.Files["db/0002_rename.sql"] != good || res.Files["db/0003_backfill_full_name.sql"] != backfill {
		t.Errorf("files = %v", res.Files)
	}
	if len(res.Remaining.Findings) != 0 {
		t.Errorf("remaining = %+v", res.Remaining.Findings)
	}
	req := fake.Requests[0]
	for _, want := range []string{"ban-rename-column", "<file path=\"db/0002_rename.sql\">", "users (id bigserial NOT NULL, name text, email text)", "0003, 0004, 0005", "+goose NO TRANSACTION"} {
		if !strings.Contains(req.Prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, req.Prompt)
		}
	}
}

func TestValidatorRejects(t *testing.T) {
	cases := []struct {
		name string
		file ProposedFile
		why  string
	}{
		{"path escape", ProposedFile{Path: "../evil.sql", Action: "create", Content: "SELECT 1;"}, "outside"},
		{"absolute path", ProposedFile{Path: "/etc/x.sql", Action: "create", Content: "SELECT 1;"}, "outside"},
		{"modify other file", ProposedFile{Path: "db/0001_base.sql", Action: "modify", Content: "SELECT 1;"}, "only allowed"},
		{"create existing", ProposedFile{Path: "db/0002_rename.sql", Action: "create", Content: "SELECT 1;"}, "existing"},
		{"version too low", ProposedFile{Path: "db/0002_other.sql", Action: "create", Content: "SELECT 1;"}, "version prefix"},
		{"no version", ProposedFile{Path: "db/new.sql", Action: "create", Content: "SELECT 1;"}, "version prefix"},
		{"disable smuggling", ProposedFile{Path: "db/0002_rename.sql", Action: "modify", Content: "-- safe_sql:disable\nALTER TABLE users RENAME COLUMN name TO n;"}, "safe_sql:disable"},
		{"unparsable", ProposedFile{Path: "db/0002_rename.sql", Action: "modify", Content: "-- +goose Up\nALTER TABLE users ADD COLUMN (;\n"}, "parse"},
		{"empty", ProposedFile{Path: "db/0002_rename.sql", Action: "modify", Content: "  \n"}, "empty"},
		{"bad action", ProposedFile{Path: "db/0002_rename.sql", Action: "delete", Content: "SELECT 1;"}, "unknown action"},
	}
	for _, c := range cases {
		fake := &Fake{Responses: []*Response{{Proposal: &Proposal{Files: []ProposedFile{c.file}}}}}
		res, err := RunFiles(context.Background(), load(t, migrate.ToolGoose, map[string]string{"db/0001_base.sql": base, "db/0002_rename.sql": rename}), opts(fake))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rejected) != 1 || !strings.Contains(res.Rejected[0], c.why) {
			t.Errorf("%s: rejected = %v (want %q)", c.name, res.Rejected, c.why)
		}
		if len(res.Files) != 0 {
			t.Errorf("%s: files written: %v", c.name, res.Files)
		}
	}
}

func TestIterationAndDecline(t *testing.T) {
	// First proposal still renames (finding remains), second fixes it.
	still := "-- +goose Up\nALTER TABLE users RENAME COLUMN name TO n2;\n-- +goose Down\nSELECT 1;\n"
	good := "-- +goose Up\nALTER TABLE users ADD COLUMN n2 text;\n-- +goose Down\nALTER TABLE users DROP COLUMN n2;\n"
	fake := &Fake{Responses: []*Response{
		{Proposal: &Proposal{Files: []ProposedFile{{Path: "db/0002_rename.sql", Action: "modify", Content: still}}}},
		{Proposal: &Proposal{Files: []ProposedFile{{Path: "db/0002_rename.sql", Action: "modify", Content: good}}}},
	}}
	res, err := RunFiles(context.Background(), load(t, migrate.ToolGoose, map[string]string{"db/0001_base.sql": base, "db/0002_rename.sql": rename}), opts(fake))
	if err != nil {
		t.Fatal(err)
	}
	if res.Iterations != 2 || len(res.Remaining.Findings) != 0 {
		t.Errorf("iterations=%d remaining=%+v", res.Iterations, res.Remaining.Findings)
	}
	if !strings.Contains(fake.Requests[1].Prompt, "previous proposal") {
		t.Error("second prompt should mention the previous attempt")
	}

	// Prose-only answer: recorded as declined, nothing written, loop stops.
	fake = &Fake{Responses: []*Response{{Text: "The column is still read by the app; deploy first."}}}
	res, err = RunFiles(context.Background(), load(t, migrate.ToolGoose, map[string]string{"db/0001_base.sql": base, "db/0002_rename.sql": rename}), opts(fake))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Declined) != 1 || len(res.Files) != 0 || res.Iterations != 1 {
		t.Errorf("declined=%v files=%v iterations=%d", res.Declined, res.Files, res.Iterations)
	}

	// Max iterations bound.
	fake = &Fake{Responses: []*Response{
		{Proposal: &Proposal{Files: []ProposedFile{{Path: "db/0002_rename.sql", Action: "modify", Content: still}}}},
		{Proposal: &Proposal{Files: []ProposedFile{{Path: "db/0002_rename.sql", Action: "modify", Content: still}}}},
	}}
	o := opts(fake)
	o.MaxIterations = 2
	res, err = RunFiles(context.Background(), load(t, migrate.ToolGoose, map[string]string{"db/0001_base.sql": base, "db/0002_rename.sql": rename}), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Iterations != 2 || len(res.Remaining.Findings) == 0 {
		t.Errorf("bound: iterations=%d remaining=%d", res.Iterations, len(res.Remaining.Findings))
	}
}

func TestDeterministicFirst(t *testing.T) {
	idx := "-- +goose Up\nCREATE INDEX i ON users (email);\n-- +goose Down\nDROP INDEX i;\n"
	fake := &Fake{}
	res, err := RunFiles(context.Background(), load(t, migrate.ToolGoose, map[string]string{"db/0001_base.sql": base, "db/0002_rename.sql": idx}), opts(fake))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deterministic) != 1 || len(fake.Requests) != 0 || res.Iterations != 0 {
		t.Errorf("deterministic fix should leave nothing for the model: %+v requests=%d", res.Deterministic, len(fake.Requests))
	}
	if !strings.Contains(res.Files["db/0002_rename.sql"], "CONCURRENTLY") {
		t.Errorf("file = %q", res.Files["db/0002_rename.sql"])
	}
}
