package todo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildWriteLoadFilter(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	path := filepath.Join(dir, "cfg", DefaultName)
	os.MkdirAll(filepath.Dir(path), 0o755)
	// Findings are named relative to the working directory, as lint does.
	entries := []Entry{
		{Rule: "ban-drop-column", File: "db/0002.sql"},
		{Rule: "ban-drop-column", File: "db/0002.sql"},
		{Rule: "prefer-jsonb", File: "db/0001.sql"},
	}
	built, err := Build(path, entries)
	if err != nil {
		t.Fatal(err)
	}
	if built.Len() != 3 || built.Files() != 2 {
		t.Errorf("len=%d files=%d", built.Len(), built.Files())
	}
	if err := built.Write("test"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	// Paths are relative to the todo file's directory, not the working directory.
	if !strings.Contains(string(b), "  ../db/0002.sql: 2\n") || !strings.Contains(string(b), "prefer-jsonb:\n  ../db/0001.sql: 1\n") {
		t.Errorf("written file:\n%s", b)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Empty() || loaded.Len() != 3 {
		t.Fatalf("loaded len=%d", loaded.Len())
	}
	f := loaded.NewFilter()
	got := []bool{
		f.Suppress("ban-drop-column", "db/0002.sql"),
		f.Suppress("ban-drop-column", "db/0002.sql"),
		f.Suppress("ban-drop-column", "db/0002.sql"), // third exceeds the count
		f.Suppress("prefer-jsonb", "db/0001.sql"),
		f.Suppress("prefer-jsonb", "db/0002.sql"),   // wrong file
		f.Suppress("ban-drop-table", "db/0002.sql"), // wrong rule
	}
	want := []bool{true, true, false, true, false, false}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("suppress[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	// A new filter starts counting again; the file itself is untouched.
	if !loaded.NewFilter().Suppress("prefer-jsonb", "db/0001.sql") {
		t.Error("fresh filter should suppress again")
	}
	// Absolute paths match too.
	abs, _ := filepath.Abs("db/0001.sql")
	if !loaded.NewFilter().Suppress("prefer-jsonb", abs) {
		t.Error("absolute path should match")
	}
}

func TestLoadMissingAndEmpty(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil || !f.Empty() || f.NewFilter().Suppress("x", "y") {
		t.Errorf("missing file: %v %v", f, err)
	}
	var nilFile *File
	if !nilFile.Empty() || nilFile.NewFilter().Suppress("x", "y") || nilFile.Len() != 0 {
		t.Error("nil file should suppress nothing")
	}
	p := filepath.Join(t.TempDir(), DefaultName)
	os.WriteFile(p, []byte("# only a header\n"), 0o644)
	f, err = Load(p)
	if err != nil || !f.Empty() {
		t.Errorf("header-only: %v %v", f, err)
	}
	os.WriteFile(p, []byte("ban-drop-column: [not, a, map]\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Error("malformed file should error")
	}
}

func TestWriteEmptyAndQuoting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, DefaultName)
	f, _ := Build(p, nil)
	if err := f.Write("v"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !strings.HasPrefix(string(b), "# safe_sql todo file") || strings.Count(string(b), "\n") > 7 {
		t.Errorf("empty file:\n%s", b)
	}
	f, _ = Build(p, []Entry{{Rule: "r", File: filepath.Join(dir, "odd: name.sql")}})
	f.Write("v")
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `  "odd: name.sql": 1`) {
		t.Errorf("quoting:\n%s", b)
	}
	loaded, err := Load(p)
	if err != nil || !loaded.NewFilter().Suppress("r", filepath.Join(dir, "odd: name.sql")) {
		t.Errorf("round trip of quoted key failed: %v", err)
	}
}
