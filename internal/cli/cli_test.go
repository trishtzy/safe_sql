package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite expected.json golden files")

// Golden tests: each testdata/<case> directory is a project; the CLI runs
// inside it with the arguments in args.txt (default: "lint --format json")
// and its JSON output must match expected.json.
func TestGolden(t *testing.T) {
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata"))
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			args := []string{"lint", "--format", "json"}
			if b, err := os.ReadFile(filepath.Join(dir, "args.txt")); err == nil {
				args = strings.Fields(strings.TrimSpace(string(b)))
			}
			wd, _ := os.Getwd()
			if err := os.Chdir(dir); err != nil {
				t.Fatal(err)
			}
			defer os.Chdir(wd)

			if _, err := os.Stat(filepath.Join(dir, "responses.json")); err == nil {
				t.Setenv("SAFE_SQL_AI_FAKE", filepath.Join(dir, "responses.json"))
			}
			var stdout, stderr bytes.Buffer
			code := Run(args, &stdout, &stderr)
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, stdout.Bytes(), "", "  "); err != nil {
				t.Fatalf("output is not JSON (exit %d): %s\n%s", code, stdout.String(), stderr.String())
			}
			got := strings.TrimSpace(pretty.String()) + "\n"
			golden := filepath.Join(dir, "expected.json")
			if *update {
				os.WriteFile(golden, []byte(got), 0o644)
				os.WriteFile(filepath.Join(dir, "expected.exit"), []byte(fmt.Sprintf("%d\n", code)), 0o644)
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("missing golden: run with -update")
			}
			if got != string(want) {
				t.Errorf("output differs from %s\n--- got ---\n%s--- want ---\n%s", golden, got, want)
			}
			if b, err := os.ReadFile(filepath.Join(dir, "expected.exit")); err == nil {
				if strings.TrimSpace(string(b)) != fmt.Sprint(code) {
					t.Errorf("exit code = %d, want %s", code, strings.TrimSpace(string(b)))
				}
			}
		})
	}
}

func TestInitAndRulesCommands(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "sqlc.yaml"), []byte("version: \"2\"\nsql:\n  - engine: postgresql\n    schema: m\n    queries: q\n"), 0o644)
	wd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(wd)

	var out, errb bytes.Buffer
	if code := Run([]string{"init"}, &out, &errb); code != 0 {
		t.Fatalf("init: %d %s", code, errb.String())
	}
	b, err := os.ReadFile("safe_sql.yaml")
	if err != nil || !strings.Contains(string(b), "sqlc: sqlc.yaml") {
		t.Errorf("starter = %q (%v)", b, err)
	}
	if code := Run([]string{"init"}, &out, &errb); code != 2 {
		t.Errorf("second init should refuse: %d", code)
	}
	out.Reset()
	if code := Run([]string{"rules", "--format", "markdown"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "| `ban-drop-column` |") {
		t.Errorf("rules: %d %s", code, out.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"lint"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "no such file") {
		t.Errorf("lint with missing schema dir: %d %s", code, errb.String())
	}
}

func TestGenerateTodo(t *testing.T) {
	src, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "goose_basic", "migrations"))
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "db", "migrations"), 0o755)
	entries, _ := os.ReadDir(src)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(src, e.Name()))
		os.WriteFile(filepath.Join(dir, "db", "migrations", e.Name()), b, 0o644)
	}
	os.WriteFile(filepath.Join(dir, "safe_sql.yaml"), []byte("version: \"1\"\nengine: postgresql\nschema: [db/migrations]\n"), 0o644)
	wd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(wd)

	var out, errb bytes.Buffer
	if code := Run([]string{"lint"}, &out, &errb); code != 1 {
		t.Fatalf("lint before baseline: %d %s%s", code, out.String(), errb.String())
	}
	out.Reset()
	if code := Run([]string{"lint", "--generate-todo"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "Wrote .safe_sql_todo.yaml: 6 finding(s) in 2 file(s)") {
		t.Fatalf("generate: %d %s%s", code, out.String(), errb.String())
	}
	b, err := os.ReadFile(".safe_sql_todo.yaml")
	if err != nil || !strings.Contains(string(b), "ban-drop-column:\n  db/migrations/0002_unsafe.sql: 1\n") {
		t.Errorf("todo file: %v\n%s", err, b)
	}
	out.Reset()
	if code := Run([]string{"lint"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "No unsafe operations found in 3 file(s). (6 known finding(s) suppressed by the todo file)") {
		t.Errorf("lint after baseline: %d %s%s", code, out.String(), errb.String())
	}
	out.Reset()
	if code := Run([]string{"lint", "--no-todo"}, &out, &errb); code != 1 {
		t.Errorf("--no-todo should report again: %d", code)
	}
	// verify and fix share the flag; fix must not rewrite baselined migrations.
	out.Reset()
	if code := Run([]string{"fix", "--dry-run", "--format", "json"}, &out, &errb); code != 0 || strings.Contains(out.String(), "0002_unsafe") || !strings.Contains(out.String(), `"remaining_findings": 0`) {
		t.Errorf("fix with baseline: %d %s%s", code, out.String(), errb.String())
	}
	out.Reset()
	if code := Run([]string{"fix", "--dry-run", "--no-todo", "--format", "json"}, &out, &errb); code != 1 || !strings.Contains(out.String(), "0002_unsafe") {
		t.Errorf("fix --no-todo should propose changes: %d %s%s", code, out.String(), errb.String())
	}
	// Regenerating starts from scratch: the same six entries, not zero.
	out.Reset()
	if code := Run([]string{"lint", "--generate-todo"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "6 finding(s)") {
		t.Errorf("regenerate: %d %s%s", code, out.String(), errb.String())
	}
	// A custom location via --todo; a config `todo:` key is covered in config tests.
	out.Reset()
	if code := Run([]string{"lint", "--generate-todo", "--todo", "ci/baseline.yaml"}, &out, &errb); code != 0 {
		t.Errorf("custom path: %d %s%s", code, out.String(), errb.String())
	}
	if b, err := os.ReadFile("ci/baseline.yaml"); err != nil || !strings.Contains(string(b), "  ../db/migrations/0002_unsafe.sql: 1\n") {
		t.Errorf("custom path file: %v\n%s", err, b)
	}
	out.Reset()
	if code := Run([]string{"lint", "--todo", "ci/baseline.yaml"}, &out, &errb); code != 0 {
		t.Errorf("lint with custom todo: %d %s%s", code, out.String(), errb.String())
	}
}
