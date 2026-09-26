// Package lint runs the rule set over migration files and collects findings.
package lint

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/trishtzy/safe_sql/internal/catalog"
	"github.com/trishtzy/safe_sql/internal/migrate"
	"github.com/trishtzy/safe_sql/internal/rules"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

// Options controls a lint run.
type Options struct {
	Engine        sqlparse.Engine
	TargetVersion rules.Version
	Tool          migrate.Tool
	// PlainInTransaction is the transaction assumption for files whose tool
	// could not be detected.
	PlainInTransaction bool
	// StartAfter suppresses findings in files whose version prefix is <= it.
	// Those files still feed the catalog.
	StartAfter string
	// ReportOnly, when non-empty, limits findings to these file paths (as
	// passed in); other files still feed the catalog.
	ReportOnly []string
	CheckDown  bool
	Disabled   []string
	Enabled    []string
	Only       []string
	Severity   map[string]rules.Severity
}

// Finding is a rule finding located in a file.
type Finding struct {
	rules.Finding
	File string
	// Line is the 1-based line in File.
	Line int
	// Down is true when the finding is in a down section (CheckDown).
	Down bool
}

// Failure is a file that could not be parsed.
type Failure struct {
	File    string
	Line    int
	Message string
}

// Result of a lint run.
type Result struct {
	Findings []Finding
	Failures []Failure
	Files    []*migrate.File
}

// Errors counts error-severity findings.
func (r *Result) Errors() int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == rules.SeverityError {
			n++
		}
	}
	return n
}

// Warnings counts warning-severity findings.
func (r *Result) Warnings() int { return len(r.Findings) - r.Errors() }

// Run discovers files under paths and lints them.
func Run(paths []string, opts Options) (*Result, error) {
	names, err := migrate.Discover(paths)
	if err != nil {
		return nil, err
	}
	files := make([]*migrate.File, 0, len(names))
	for _, n := range names {
		f, err := migrate.Load(n, opts.Tool)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return RunFiles(files, opts)
}

// RunFiles lints already-loaded files in the given order.
func RunFiles(files []*migrate.File, opts Options) (*Result, error) {
	parser, err := sqlparse.ForEngine(opts.Engine)
	if err != nil {
		return nil, err
	}
	active := activeRules(opts)
	cat := catalog.New()
	res := &Result{Files: files}
	reportOnly := map[string]bool{}
	for _, p := range opts.ReportOnly {
		reportOnly[p] = true
	}

	for _, f := range files {
		report := true
		if opts.StartAfter != "" && f.Version != "" && compareVersions(f.Version, opts.StartAfter) <= 0 {
			report = false
		}
		if len(reportOnly) > 0 && !reportOnly[f.Path] {
			report = false
		}
		if !f.IsDownFile {
			lintSection(f, f.Up, false, parser, active, cat, opts, report, res)
		}
		if opts.CheckDown && f.Down.SQL != "" {
			lintSection(f, f.Down, true, parser, active, cat, opts, report, res)
		}
	}
	sort.SliceStable(res.Findings, func(i, j int) bool {
		if res.Findings[i].File != res.Findings[j].File {
			return res.Findings[i].File < res.Findings[j].File
		}
		return res.Findings[i].Line < res.Findings[j].Line
	})
	return res, nil
}

func lintSection(f *migrate.File, sec migrate.Section, down bool, parser sqlparse.Parser, active []*rules.Rule,
	cat *catalog.Catalog, opts Options, report bool, res *Result) {
	stmts, err := parser.Parse(sec.SQL)
	if err != nil {
		var pe *sqlparse.ParseError
		if errors.As(err, &pe) {
			res.Failures = append(res.Failures, Failure{File: f.Path, Line: f.Line(sec, max(pe.Offset, 0)), Message: pe.Message})
		} else {
			res.Failures = append(res.Failures, Failure{File: f.Path, Message: err.Error()})
		}
		return
	}
	fileTx := f.InTransaction(len(stmts), opts.PlainInTransaction)
	fileDisabled, fileAll := fileDisables(stmts)
	ctx := &rules.Context{
		Engine: opts.Engine, TargetVersion: opts.TargetVersion, Tool: string(f.Tool),
		Filename: f.Path, Catalog: cat, Statements: stmts,
	}
	explicitTx := false
	for i := range stmts {
		st := &stmts[i]
		if st.Kind == sqlparse.KindTransaction {
			explicitTx = st.Transaction.Begin
		}
		ctx.Index = i
		ctx.InTransaction = fileTx || explicitTx
		if report && !fileAll {
			disabled, all := stmtDisables(st)
			if !all {
				for _, r := range active {
					if fileDisabled[r.ID] || disabled[r.ID] {
						continue
					}
					for _, fd := range r.Check(ctx, st) {
						if sev, ok := opts.Severity[r.ID]; ok {
							fd.Severity = sev
						}
						res.Findings = append(res.Findings, Finding{Finding: fd, File: f.Path, Line: f.Line(sec, st.Offset), Down: down})
					}
				}
			}
		}
		if !down {
			cat.Apply(st, f.Path)
		}
	}
}

func activeRules(opts Options) []*rules.Rule {
	disabled := set(opts.Disabled)
	enabled := set(opts.Enabled)
	only := set(opts.Only)
	var out []*rules.Rule
	for _, r := range rules.ForEngine(opts.Engine) {
		if disabled[r.ID] || (r.OptIn && !enabled[r.ID]) || (len(only) > 0 && !only[r.ID]) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func set(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

const (
	disablePrefix     = "safe_sql:disable"
	disableFilePrefix = "safe_sql:disable-file"
)

// stmtDisables reads "safe_sql:disable [rule ...]" from a statement's leading
// comments. A bare directive disables every rule (all == true).
func stmtDisables(st *sqlparse.Statement) (map[string]bool, bool) {
	return parseDisables(st.LeadingComments, disablePrefix, disableFilePrefix)
}

// fileDisables reads "safe_sql:disable-file [rule ...]" from the comments
// before the first statement.
func fileDisables(stmts []sqlparse.Statement) (map[string]bool, bool) {
	if len(stmts) == 0 {
		return nil, false
	}
	return parseDisables(stmts[0].LeadingComments, disableFilePrefix, "")
}

func parseDisables(comments []string, prefix, exclude string) (map[string]bool, bool) {
	out := map[string]bool{}
	all := false
	for _, c := range comments {
		if exclude != "" && strings.HasPrefix(c, exclude) {
			continue
		}
		if !strings.HasPrefix(c, prefix) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(c, prefix))
		if rest == "" {
			all = true
			continue
		}
		for _, id := range strings.FieldsFunc(rest, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' }) {
			out[id] = true
		}
	}
	return out, all
}

// compareVersions compares numeric version prefixes of possibly different
// lengths ("00012" vs "9"), ignoring leading zeros.
func compareVersions(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// Summary is a one-line human summary.
func (r *Result) Summary() string {
	return fmt.Sprintf("%d error(s), %d warning(s), %d parse failure(s) in %d file(s)", r.Errors(), r.Warnings(), len(r.Failures), len(r.Files))
}
