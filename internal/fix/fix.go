// Package fix rewrites migrations flagged by lint into their safe form.
//
// Tier 1 fixers are deterministic rewrites of the offending statement,
// sometimes splitting work into a new migration file (for example, moving a
// CREATE INDEX CONCURRENTLY out of a transactional file). Tier 2 (package ai)
// asks a model for the multi-step patterns a rewrite cannot express.
package fix

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/trishtzy/safe_sql/internal/lint"
	"github.com/trishtzy/safe_sql/internal/migrate"
)

// Edit replaces file bytes [Start, End) with Replacement.
type Edit struct {
	Path        string
	Start, End  int
	Replacement string
}

// NewFile is a migration file to create.
type NewFile struct {
	Path    string
	Content string
}

// Change is the outcome of fixing one finding.
type Change struct {
	Finding  lint.Finding
	Edits    []Edit
	NewFiles []NewFile
	// Note explains anything the developer must still do.
	Note string
}

// Fixer produces a Change for a finding, or ok=false when the finding is not
// fixable deterministically.
type Fixer func(c *Context, f lint.Finding) (Change, bool)

// Context is what a fixer may consult.
type Context struct {
	File *migrate.File
	// Section holds the statement (up, or down when the finding is Down).
	Section migrate.Section
	// InTransaction reports whether the tool runs the file in a transaction.
	InTransaction bool
	// Statements is the number of executable statements in the section.
	Statements int
	Versions   *Versioner
	Opts       lint.Options
}

// StmtStart returns the file byte offset of the finding's statement.
func (c *Context) StmtStart(f lint.Finding) int { return c.Section.Offset + f.Statement.Offset }

// StmtEnd returns the file byte offset just past the statement and its
// trailing semicolon and newline, so deleting [StmtStart, StmtEnd) removes
// the whole line.
func (c *Context) StmtEnd(f lint.Finding) int {
	end := c.Section.Offset + f.Statement.End
	src := c.File.Source
	if end < len(src) && src[end] == ';' {
		end++
	}
	for end < len(src) && (src[end] == ' ' || src[end] == '\t') {
		end++
	}
	if end < len(src) && src[end] == '\n' {
		end++
	}
	return end
}

var registry = map[string]Fixer{}

// Register adds a fixer for a rule ID.
func Register(ruleID string, f Fixer) { registry[ruleID] = f }

// Fixable reports whether a deterministic fixer exists for the rule.
func Fixable(ruleID string) bool { _, ok := registry[ruleID]; return ok }

// Options for a fix run.
type Options struct {
	Lint lint.Options
	// Only restricts fixing to these rule IDs.
	Only []string
	// DryRun computes changes without writing files.
	DryRun bool
}

// Result of a fix run.
type Result struct {
	Changes []Change
	// Unfixed are findings no deterministic fixer handled.
	Unfixed []lint.Finding
	// Before/After hold file contents keyed by path (After includes new files).
	Before, After map[string]string
	// Remaining is the lint result over After.
	Remaining *lint.Result
}

// Run lints paths, applies every available fixer, re-lints the result, and
// (unless DryRun) writes the files.
func Run(paths []string, opts Options) (*Result, error) {
	names, err := migrate.Discover(paths)
	if err != nil {
		return nil, err
	}
	var files []*migrate.File
	for _, n := range names {
		f, err := migrate.Load(n, opts.Lint.Tool)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return RunFiles(files, opts)
}

// RunFiles is Run over already-loaded files.
func RunFiles(files []*migrate.File, opts Options) (*Result, error) {
	res, err := lint.RunFiles(files, opts.Lint)
	if err != nil {
		return nil, err
	}
	out := &Result{Before: map[string]string{}, After: map[string]string{}}
	byPath := map[string]*migrate.File{}
	for _, f := range files {
		byPath[f.Path] = f
		out.Before[f.Path] = f.Source
	}
	only := map[string]bool{}
	for _, id := range opts.Only {
		only[id] = true
	}
	versions := NewVersioner(files)
	parser, _ := lintParser(opts.Lint)

	edits := map[string][]Edit{}
	for _, fd := range res.Findings {
		fixer, ok := registry[fd.RuleID]
		if !ok || (len(only) > 0 && !only[fd.RuleID]) {
			out.Unfixed = append(out.Unfixed, fd)
			continue
		}
		f := byPath[fd.File]
		sec := f.Up
		if fd.Down {
			sec = f.Down
		}
		n := 0
		if parser != nil {
			if stmts, err := parser.Parse(sec.SQL); err == nil {
				for _, s := range stmts {
					if s.Kind.String() != "transaction" {
						n++
					}
				}
			}
		}
		c := &Context{File: f, Section: sec, InTransaction: f.InTransaction(n, opts.Lint.PlainInTransaction), Statements: n, Versions: versions, Opts: opts.Lint}
		ch, ok := fixer(c, fd)
		if !ok {
			out.Unfixed = append(out.Unfixed, fd)
			continue
		}
		if overlaps(edits, ch.Edits) {
			out.Unfixed = append(out.Unfixed, fd)
			continue
		}
		for _, e := range ch.Edits {
			edits[e.Path] = append(edits[e.Path], e)
		}
		for _, nf := range ch.NewFiles {
			out.After[nf.Path] = nf.Content
		}
		out.Changes = append(out.Changes, ch)
	}

	for path, src := range out.Before {
		out.After[path] = Apply(src, edits[path])
	}

	// Re-lint the result in memory.
	var after []*migrate.File
	var afterPaths []string
	for p := range out.After {
		afterPaths = append(afterPaths, p)
	}
	sort.Strings(afterPaths)
	for _, p := range afterPaths {
		f, err := migrate.Parse(p, out.After[p], opts.Lint.Tool)
		if err != nil {
			return nil, err
		}
		after = append(after, f)
	}
	out.Remaining, err = lint.RunFiles(after, opts.Lint)
	if err != nil {
		return nil, err
	}

	if !opts.DryRun {
		for _, p := range afterPaths {
			if out.After[p] == out.Before[p] {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(p, []byte(out.After[p]), 0o644); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// Apply applies edits to src. Edits must not overlap.
func Apply(src string, edits []Edit) string {
	// Descending by Start so earlier offsets stay valid; at equal Start apply
	// the wider edit first so a zero-width insert lands before the replacement.
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].Start != edits[j].Start {
			return edits[i].Start > edits[j].Start
		}
		return edits[i].End > edits[j].End
	})
	for _, e := range edits {
		src = src[:e.Start] + e.Replacement + src[e.End:]
	}
	return src
}

func overlaps(existing map[string][]Edit, next []Edit) bool {
	for _, n := range next {
		for _, e := range existing[n.Path] {
			if n.Start < e.End && e.Start < n.End {
				return true
			}
		}
	}
	return false
}

// Versioner allocates migration file names that sort after every existing
// file, following the scheme the directory already uses.
type Versioner struct {
	dir       string
	next      int64
	width     int
	timestamp bool
	tool      migrate.Tool
	ok        bool
}

// NewVersioner inspects existing files to learn the naming scheme.
func NewVersioner(files []*migrate.File) *Versioner {
	v := &Versioner{}
	var maxV int64
	for _, f := range files {
		if f.Version == "" {
			continue
		}
		v.ok = true
		v.dir = filepath.Dir(f.Path)
		v.tool = f.Tool
		var n int64
		fmt.Sscanf(f.Version, "%d", &n)
		if n > maxV {
			maxV = n
			v.width = len(f.Version)
		}
	}
	v.timestamp = v.width == 14
	if v.timestamp {
		v.next = mustInt(time.Now().UTC().Format("20060102150405"))
		if v.next <= maxV {
			v.next = maxV + 1
		}
	} else {
		v.next = maxV + 1
	}
	return v
}

func mustInt(s string) int64 {
	var n int64
	fmt.Sscanf(s, "%d", &n)
	return n
}

// Next returns the paths for a new migration named slug. golang-migrate
// yields an up and a down file; other tools a single file. ok is false when
// the directory's scheme is unknown (plain files).
func (v *Versioner) Next(slug string) (up, down string, ok bool) {
	if !v.ok || v.tool == migrate.ToolPlain {
		return "", "", false
	}
	num := fmt.Sprintf("%0*d", v.width, v.next)
	v.next++
	switch v.tool {
	case migrate.ToolGolangMigrate:
		return filepath.Join(v.dir, num+"_"+slug+".up.sql"), filepath.Join(v.dir, num+"_"+slug+".down.sql"), true
	case migrate.ToolAtlas:
		return filepath.Join(v.dir, num+"_"+slug+".sql"), "", true
	default:
		return filepath.Join(v.dir, num+"_"+slug+".sql"), "", true
	}
}

// NewMigration renders a migration for the tool with the given up and down
// statements (each without trailing semicolon). noTx adds the tool's
// directive for running outside a transaction. Returns the files to create.
func NewMigration(v *Versioner, slug string, up, down []string, noTx bool) ([]NewFile, bool) {
	upPath, downPath, ok := v.Next(slug)
	if !ok {
		return nil, false
	}
	stmts := func(xs []string) string {
		var b strings.Builder
		for _, x := range xs {
			b.WriteString(x)
			b.WriteString(";\n")
		}
		return b.String()
	}
	var b strings.Builder
	switch v.tool {
	case migrate.ToolGoose:
		if noTx {
			b.WriteString("-- +goose NO TRANSACTION\n")
		}
		b.WriteString("-- +goose Up\n" + stmts(up) + "\n-- +goose Down\n" + stmts(down))
	case migrate.ToolSQLMigrate:
		tx := ""
		if noTx {
			tx = " notransaction"
		}
		b.WriteString("-- +migrate Up" + tx + "\n" + stmts(up) + "\n-- +migrate Down" + tx + "\n" + stmts(down))
	case migrate.ToolDbmate:
		tx := ""
		if noTx {
			tx = " transaction:false"
		}
		b.WriteString("-- migrate:up" + tx + "\n" + stmts(up) + "\n-- migrate:down" + tx + "\n" + stmts(down))
	case migrate.ToolTern:
		if noTx {
			b.WriteString("---- tern: disable-tx ----\n")
		}
		b.WriteString(stmts(up) + "\n---- create above / drop below ----\n\n" + stmts(down))
	case migrate.ToolAtlas:
		if noTx {
			b.WriteString("-- atlas:txmode none\n")
		}
		b.WriteString(stmts(up))
	case migrate.ToolGolangMigrate:
		out := []NewFile{{Path: upPath, Content: stmts(up)}}
		if len(down) > 0 {
			out = append(out, NewFile{Path: downPath, Content: stmts(down)})
		}
		return out, true
	default:
		return nil, false
	}
	return []NewFile{{Path: upPath, Content: b.String()}}, true
}

// NoTxDirectiveEdit returns an edit that makes the whole file run outside a
// transaction, when the tool has a directive for that.
func NoTxDirectiveEdit(f *migrate.File) (Edit, bool) {
	src := f.Source
	switch f.Tool {
	case migrate.ToolGoose:
		return Edit{Path: f.Path, Start: 0, End: 0, Replacement: "-- +goose NO TRANSACTION\n"}, true
	case migrate.ToolTern:
		return Edit{Path: f.Path, Start: 0, End: 0, Replacement: "---- tern: disable-tx ----\n"}, true
	case migrate.ToolAtlas:
		return Edit{Path: f.Path, Start: 0, End: 0, Replacement: "-- atlas:txmode none\n"}, true
	case migrate.ToolSQLMigrate:
		if m := regexp.MustCompile(`(?im)^[ \t]*--[ \t]*\+migrate[ \t]+up\b`).FindStringIndex(src); m != nil {
			return Edit{Path: f.Path, Start: m[1], End: m[1], Replacement: " notransaction"}, true
		}
	case migrate.ToolDbmate:
		if m := regexp.MustCompile(`(?im)^[ \t]*--[ \t]*migrate:up\b`).FindStringIndex(src); m != nil {
			return Edit{Path: f.Path, Start: m[1], End: m[1], Replacement: " transaction:false"}, true
		}
	}
	return Edit{}, false
}
