// Package migrate discovers migration files and splits them the way the
// common migration tools (and sqlc) do: up vs down sections, lexicographic
// ordering, and per-file transaction mode.
package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Tool is a migration tool whose file conventions we understand.
type Tool string

const (
	ToolAuto          Tool = "auto"
	ToolPlain         Tool = "plain"
	ToolGoose         Tool = "goose"
	ToolGolangMigrate Tool = "golang-migrate"
	ToolDbmate        Tool = "dbmate"
	ToolTern          Tool = "tern"
	ToolSQLMigrate    Tool = "sql-migrate"
	ToolAtlas         Tool = "atlas"
)

// Tools lists every supported value for the migration_tool config key.
var Tools = []Tool{ToolAuto, ToolPlain, ToolGoose, ToolGolangMigrate, ToolDbmate, ToolTern, ToolSQLMigrate, ToolAtlas}

// Section is a slice of a migration file: the SQL and its byte offset within
// File.Source, so statement positions can be mapped back to file lines.
type Section struct {
	SQL    string
	Offset int
}

// File is one migration file.
type File struct {
	Path    string
	Name    string
	Version string
	Tool    Tool
	// Source is the file content with psql meta-command lines blanked out.
	Source string
	Up     Section
	Down   Section
	// IsDownFile is true for golang-migrate *.down.sql files (Up is empty).
	IsDownFile bool
	// NoTxDirective is true when the file carries the tool's directive that
	// disables the wrapping transaction.
	NoTxDirective bool
}

// InTransaction reports whether the tool would run this file's up section
// inside a transaction. stmtCount is the number of statements in the up
// section; it matters for golang-migrate, which sends the whole file as one
// multi-statement command that Postgres runs as an implicit transaction only
// when there is more than one statement. plainDefault is the assumption for
// files with no recognised tool.
func (f *File) InTransaction(stmtCount int, plainDefault bool) bool {
	if f.NoTxDirective {
		return false
	}
	switch f.Tool {
	case ToolGolangMigrate:
		return stmtCount > 1
	case ToolPlain:
		return plainDefault
	}
	return true
}

// Line maps a byte offset within a section to a 1-based line in the file.
func (f *File) Line(sec Section, off int) int {
	abs := sec.Offset + off
	if abs > len(f.Source) {
		abs = len(f.Source)
	}
	return 1 + strings.Count(f.Source[:abs], "\n")
}

// Discover expands sqlc-style schema paths (files, directories, or both) into
// a lexicographically sorted list of .sql files. Directories are not
// recursed, matching sqlc.
func Discover(paths []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if !seen[p] {
				out = append(out, p)
				seen[p] = true
			}
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		var files []string
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".sql") {
				continue
			}
			files = append(files, filepath.Join(p, e.Name()))
		}
		sort.Strings(files)
		for _, f := range files {
			if !seen[f] {
				out = append(out, f)
				seen[f] = true
			}
		}
	}
	return out, nil
}

// Load reads and parses one migration file. tool may be ToolAuto.
func Load(path string, tool Tool) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(path, string(b), tool)
}

var (
	versionRe   = regexp.MustCompile(`^\d+`)
	psqlMetaRe  = regexp.MustCompile(`(?m)^\\[A-Za-z!?;][^\n]*$`)
	gooseUpRe   = regexp.MustCompile(`(?im)^[ \t]*--\s*\+goose\s+up\b`)
	gooseDownRe = regexp.MustCompile(`(?im)^[ \t]*--\s*\+goose\s+down\b`)
	gooseNoTxRe = regexp.MustCompile(`(?im)^[ \t]*--\s*\+goose\s+no\s+transaction\b`)
	sqlmUpRe    = regexp.MustCompile(`(?im)^[ \t]*--\s*\+migrate\s+up\b([^\n]*)`)
	sqlmDownRe  = regexp.MustCompile(`(?im)^[ \t]*--\s*\+migrate\s+down\b`)
	dbmateUpRe  = regexp.MustCompile(`(?im)^[ \t]*--\s*migrate:up\b([^\n]*)`)
	dbmateDnRe  = regexp.MustCompile(`(?im)^[ \t]*--\s*migrate:down\b`)
	ternSplitRe = regexp.MustCompile(`(?m)^---- create above / drop below ----[ \t]*$`)
	ternNoTxRe  = regexp.MustCompile(`(?m)^---- tern: disable-tx ----[ \t]*$`)
	atlasDirRe  = regexp.MustCompile(`(?im)^[ \t]*--\s*atlas:(\w+)\s*([^\n]*)`)
)

// DetectTool infers the migration tool from the file name and content.
func DetectTool(path, content string) Tool {
	lower := strings.ToLower(filepath.Base(path))
	switch {
	case gooseUpRe.MatchString(content) || gooseDownRe.MatchString(content):
		return ToolGoose
	case sqlmUpRe.MatchString(content) || sqlmDownRe.MatchString(content):
		return ToolSQLMigrate
	case dbmateUpRe.MatchString(content) || dbmateDnRe.MatchString(content):
		return ToolDbmate
	case ternSplitRe.MatchString(content) || ternNoTxRe.MatchString(content):
		return ToolTern
	case strings.HasSuffix(lower, ".up.sql") || strings.HasSuffix(lower, ".down.sql"):
		return ToolGolangMigrate
	case atlasDirRe.MatchString(content):
		return ToolAtlas
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "atlas.sum")); err == nil {
		return ToolAtlas
	}
	return ToolPlain
}

// Parse splits content according to tool's conventions. tool may be ToolAuto.
func Parse(path, content string, tool Tool) (*File, error) {
	if tool == "" || tool == ToolAuto {
		tool = DetectTool(path, content)
	}
	src := blankPsqlMetaCommands(content)
	f := &File{
		Path:    path,
		Name:    filepath.Base(path),
		Version: versionRe.FindString(filepath.Base(path)),
		Tool:    tool,
		Source:  src,
	}
	whole := Section{SQL: src, Offset: 0}
	switch tool {
	case ToolGoose:
		f.Up, f.Down = splitAt(src, gooseDownRe)
		f.NoTxDirective = gooseNoTxRe.MatchString(src)
	case ToolSQLMigrate:
		f.Up, f.Down = splitAt(src, sqlmDownRe)
		if m := sqlmUpRe.FindStringSubmatch(src); m != nil && strings.Contains(strings.ToLower(m[1]), "notransaction") {
			f.NoTxDirective = true
		}
	case ToolDbmate:
		f.Up, f.Down = splitAt(src, dbmateDnRe)
		if m := dbmateUpRe.FindStringSubmatch(src); m != nil && strings.Contains(strings.ToLower(m[1]), "transaction:false") {
			f.NoTxDirective = true
		}
	case ToolTern:
		f.Up, f.Down = splitAt(src, ternSplitRe)
		f.NoTxDirective = ternNoTxRe.MatchString(src)
	case ToolGolangMigrate:
		if strings.HasSuffix(strings.ToLower(f.Name), ".down.sql") {
			f.IsDownFile = true
			f.Down = whole
		} else {
			f.Up = whole
		}
	case ToolAtlas:
		f.Up = whole
		for _, m := range atlasDirRe.FindAllStringSubmatch(src, -1) {
			if strings.EqualFold(m[1], "txmode") && strings.EqualFold(strings.TrimSpace(m[2]), "none") {
				f.NoTxDirective = true
			}
		}
	case ToolPlain:
		f.Up = whole
	default:
		return nil, fmt.Errorf("%s: unknown migration tool %q", path, tool)
	}
	return f, nil
}

// splitAt returns everything before the first match of re as the up section
// (this mirrors sqlc, which keeps the header and the up marker comment) and
// everything from the marker onward as the down section.
func splitAt(src string, re *regexp.Regexp) (up, down Section) {
	loc := re.FindStringIndex(src)
	if loc == nil {
		return Section{SQL: src}, Section{}
	}
	return Section{SQL: src[:loc[0]], Offset: 0}, Section{SQL: src[loc[0]:], Offset: loc[0]}
}

// blankPsqlMetaCommands replaces psql meta-command lines (\restrict,
// \connect, ...) that pg_dump emits with empty lines so byte offsets and
// line numbers are preserved. sqlc strips the same lines.
func blankPsqlMetaCommands(s string) string {
	return psqlMetaRe.ReplaceAllStringFunc(s, func(m string) string { return strings.Repeat(" ", len(m)) })
}
