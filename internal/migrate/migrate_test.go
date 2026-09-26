package migrate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseTools(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		content string
		tool    Tool
		up      string
		down    string
		noTx    bool
		version string
	}{
		{"goose", "00012_add.sql", "-- +goose NO TRANSACTION\n-- +goose Up\nCREATE INDEX CONCURRENTLY i ON t (a);\n-- +goose Down\nDROP INDEX i;\n",
			ToolGoose, "-- +goose NO TRANSACTION\n-- +goose Up\nCREATE INDEX CONCURRENTLY i ON t (a);\n", "-- +goose Down\nDROP INDEX i;\n", true, "00012"},
		{"goose no down", "2_x.sql", "-- +goose Up\nSELECT 1;\n", ToolGoose, "-- +goose Up\nSELECT 1;\n", "", false, "2"},
		{"sql-migrate", "3_x.sql", "-- +migrate Up notransaction\nSELECT 1;\n-- +migrate Down\nSELECT 2;\n",
			ToolSQLMigrate, "-- +migrate Up notransaction\nSELECT 1;\n", "-- +migrate Down\nSELECT 2;\n", true, "3"},
		{"dbmate", "20240101120000_x.sql", "-- migrate:up transaction:false\nSELECT 1;\n\n-- migrate:down\nSELECT 2;\n",
			ToolDbmate, "-- migrate:up transaction:false\nSELECT 1;\n\n", "-- migrate:down\nSELECT 2;\n", true, "20240101120000"},
		{"dbmate tx", "4_x.sql", "-- migrate:up\nSELECT 1;\n-- migrate:down\n", ToolDbmate, "-- migrate:up\nSELECT 1;\n", "-- migrate:down\n", false, "4"},
		{"tern", "005_x.sql", "---- tern: disable-tx ----\nSELECT 1;\n---- create above / drop below ----\nSELECT 2;\n",
			ToolTern, "---- tern: disable-tx ----\nSELECT 1;\n", "---- create above / drop below ----\nSELECT 2;\n", true, "005"},
		{"golang-migrate up", "000006_x.up.sql", "SELECT 1;\n", ToolGolangMigrate, "SELECT 1;\n", "", false, "000006"},
		{"golang-migrate down", "000006_x.down.sql", "SELECT 2;\n", ToolGolangMigrate, "", "SELECT 2;\n", false, "000006"},
		{"atlas", "20240101.sql", "-- atlas:txmode none\nSELECT 1;\n", ToolAtlas, "-- atlas:txmode none\nSELECT 1;\n", "", true, "20240101"},
		{"plain", "schema.sql", "SELECT 1;\n", ToolPlain, "SELECT 1;\n", "", false, ""},
	}
	for _, c := range cases {
		f, err := Parse(c.path, c.content, ToolAuto)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if f.Tool != c.tool {
			t.Errorf("%s: tool = %s, want %s", c.name, f.Tool, c.tool)
		}
		if f.Up.SQL != c.up {
			t.Errorf("%s: up = %q, want %q", c.name, f.Up.SQL, c.up)
		}
		if f.Down.SQL != c.down {
			t.Errorf("%s: down = %q, want %q", c.name, f.Down.SQL, c.down)
		}
		if f.NoTxDirective != c.noTx {
			t.Errorf("%s: noTx = %v, want %v", c.name, f.NoTxDirective, c.noTx)
		}
		if f.Version != c.version {
			t.Errorf("%s: version = %q, want %q", c.name, f.Version, c.version)
		}
		if f.Down.SQL != "" && f.Source[f.Down.Offset:] != f.Down.SQL {
			t.Errorf("%s: down offset %d does not map to down section", c.name, f.Down.Offset)
		}
	}
}

func TestInTransaction(t *testing.T) {
	gm := &File{Tool: ToolGolangMigrate}
	if gm.InTransaction(1, true) || !gm.InTransaction(2, true) {
		t.Error("golang-migrate: single statement runs outside tx, multiple inside")
	}
	if (&File{Tool: ToolGoose, NoTxDirective: true}).InTransaction(5, true) {
		t.Error("directive should disable tx")
	}
	if !(&File{Tool: ToolGoose}).InTransaction(1, false) {
		t.Error("goose default is tx")
	}
	if (&File{Tool: ToolPlain}).InTransaction(3, false) {
		t.Error("plain honours default")
	}
}

func TestLineAndPsqlMeta(t *testing.T) {
	src := "\\restrict abc\nSELECT 1;\n-- +goose Down\nSELECT\n2;\n"
	f, _ := Parse("1_x.sql", src, ToolGoose)
	if len(f.Source) != len(src) {
		t.Errorf("length changed: %d vs %d", len(f.Source), len(src))
	}
	if f.Source[:13] != strings.Repeat(" ", 13) {
		t.Errorf("meta line not blanked: %q", f.Source[:14])
	}
	// "2;" is on line 5 of the file; find its offset within Down.
	off := len("-- +goose Down\nSELECT\n")
	if got := f.Line(f.Down, off); got != 5 {
		t.Errorf("line = %d, want 5", got)
	}
}

func TestDiscover(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"10_b.sql", "9_a.sql", "notes.txt", "2_c.SQL"} {
		os.WriteFile(filepath.Join(dir, n), []byte("select 1"), 0o644)
	}
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "1_z.sql"), []byte("select 1"), 0o644)
	extra := filepath.Join(dir, "sub", "1_z.sql")
	got, err := Discover([]string{dir, extra, extra})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "10_b.sql"), filepath.Join(dir, "2_c.SQL"), filepath.Join(dir, "9_a.sql"), extra}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}
