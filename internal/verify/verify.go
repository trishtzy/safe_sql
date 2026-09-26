// Package verify is a local, no-cloud stand-in for `sqlc verify`: it applies
// the proposed migrations to a throwaway database, then asks sqlc to prepare
// the queries that are deployed at a git ref against that schema and reports
// every query that breaks.
package verify

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/trishtzy/safe_sql/internal/catalog"
	"github.com/trishtzy/safe_sql/internal/config"
	"github.com/trishtzy/safe_sql/internal/lint"
	"github.com/trishtzy/safe_sql/internal/migrate"
	"github.com/trishtzy/safe_sql/internal/rules"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

// RuleID is the pseudo-rule under which query breakages are reported so the
// normal reporters and exit codes apply.
const RuleID = "verify/query-breaks"

// Options for one verify run.
type Options struct {
	Package config.Package
	// Against is the git ref whose queries are considered deployed.
	Against string
	// DatabaseURL, when set, is used instead of an ephemeral container.
	DatabaseURL string
	// Docker allows starting an ephemeral container when DatabaseURL is empty.
	Docker bool
	// SqlcPath is the sqlc binary (default: "sqlc" on PATH).
	SqlcPath string
	Lint     lint.Options
	Stderr   io.Writer
	// KeepTemp leaves the temp directory behind for debugging.
	KeepTemp bool
}

// Breakage is one query that no longer works against the proposed schema.
type Breakage struct {
	// QueryFile is the repo path of the query file at Against.
	QueryFile string
	QueryName string
	Line      int
	Message   string
	SQL       string
	// MigrationFile is the proposed migration most likely responsible ("" if unknown).
	MigrationFile string
	// Static is true for errors sqlc found by compiling the query against the
	// schema (no database needed); false for prepare failures.
	Static bool
}

// Result of a verify run.
type Result struct {
	Breakages []Breakage
	// NewFiles are proposed migrations that differ from Against.
	NewFiles []string
	// Lint holds findings for the new files plus one finding per breakage.
	Lint *lint.Result
	// SqlcOutput is sqlc vet's raw output for --verbose.
	SqlcOutput string
}

// Run executes a verify.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.SqlcPath == "" {
		opts.SqlcPath = "sqlc"
	}
	if opts.Against == "" {
		opts.Against = "main"
	}
	if _, err := exec.LookPath(opts.SqlcPath); err != nil {
		return nil, fmt.Errorf("sqlc binary not found (%s): verify delegates query preparation to `sqlc vet`; install sqlc or set verify.sqlc_path", opts.SqlcPath)
	}
	root, err := gitRoot()
	if err != nil {
		return nil, err
	}
	if err := gitOK(root, "rev-parse", "--verify", "--quiet", opts.Against+"^{commit}"); err != nil {
		return nil, fmt.Errorf("git ref %q not found (set verify.base_ref or --against)", opts.Against)
	}

	tmp, err := os.MkdirTemp("", "safe_sql-verify-")
	if err != nil {
		return nil, err
	}
	if !opts.KeepTemp {
		defer os.RemoveAll(tmp)
	} else {
		fmt.Fprintf(opts.Stderr, "safe_sql: keeping temp dir %s\n", tmp)
	}

	// 1. Deployed queries: export the query paths at Against.
	againstDir := filepath.Join(tmp, "against")
	queryRel, err := exportAt(root, opts.Against, opts.Package.Queries, againstDir)
	if err != nil {
		return nil, err
	}
	if len(queryRel) == 0 {
		return nil, fmt.Errorf("no query files found at %s under %v", opts.Against, opts.Package.Queries)
	}

	// 2. Proposed migrations, and which of them are new relative to Against.
	files, err := migrate.Discover(opts.Package.Schema)
	if err != nil {
		return nil, err
	}
	loaded := make([]*migrate.File, 0, len(files))
	var newFiles []string
	for _, p := range files {
		f, err := migrate.Load(p, opts.Lint.Tool)
		if err != nil {
			return nil, err
		}
		loaded = append(loaded, f)
		if changed, err := changedSince(root, opts.Against, p); err != nil {
			return nil, err
		} else if changed {
			newFiles = append(newFiles, p)
		}
	}

	// 3. A database with the proposed schema applied.
	db, cleanup, err := openDatabase(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	cat := catalog.New()
	if err := applyMigrations(ctx, db, loaded, opts, cat); err != nil {
		return nil, err
	}

	// 4. sqlc vet with db-prepare against it, using the deployed queries.
	cfg := tempSqlcConfig(tmp, opts.Package, queryRel, db.URI())
	cfgPath := filepath.Join(tmp, "sqlc.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		return nil, err
	}
	out, vetErr := runSqlcVet(ctx, opts.SqlcPath, cfgPath)
	res := &Result{NewFiles: newFiles, SqlcOutput: out}
	if vetErr != nil && !isFailedChecks(vetErr, out) {
		return nil, fmt.Errorf("sqlc vet: %v\n%s", vetErr, out)
	}
	res.Breakages = ParseVetOutput(out, "against/", root)
	for i := range res.Breakages {
		b := &res.Breakages[i]
		qfile := filepath.Join(againstDir, relToRoot(root, b.QueryFile))
		if b.QueryName == "" && b.Line > 0 {
			b.QueryName, b.SQL = queryAtLine(qfile, b.Line)
		} else {
			b.SQL = querySQL(qfile, b.QueryName)
		}
		b.MigrationFile = Attribute(b.SQL, newFiles, loaded, cat)
	}

	// 5. Lint the new files and fold breakages into the same result shape.
	lopts := opts.Lint
	lopts.Engine = opts.Package.Engine
	lopts.ReportOnly = newFiles
	res.Lint, err = lint.RunFiles(loaded, lopts)
	if err != nil {
		return nil, err
	}
	for _, b := range res.Breakages {
		res.Lint.Findings = append(res.Lint.Findings, b.finding())
	}
	sort.SliceStable(res.Lint.Findings, func(i, j int) bool {
		a, c := res.Lint.Findings[i], res.Lint.Findings[j]
		if a.File != c.File {
			return a.File < c.File
		}
		return a.Line < c.Line
	})
	return res, nil
}

func (b Breakage) finding() lint.Finding {
	kind := "fails to prepare"
	if b.Static {
		kind = "no longer compiles"
	}
	file, line := b.MigrationFile, 0
	if file == "" {
		file, line = b.QueryFile, b.Line
	}
	msg := fmt.Sprintf("deployed query %s (%s) %s against the proposed schema: %s", b.QueryName, b.QueryFile, kind, b.Message)
	return lint.Finding{
		Finding: rules.Finding{
			RuleID: RuleID, Severity: rules.SeverityError, Message: msg,
			Guidance: "A query that is deployed right now would fail as soon as this migration\n" +
				"runs, before the new application code is out. Make the schema change\n" +
				"backward compatible (add before remove, rename via a new column, keep\n" +
				"the old column until the query is gone), or update and deploy the query\n" +
				"first.",
			Statement: &sqlparse.Statement{Raw: strings.TrimSpace(b.SQL)},
		},
		File: file, Line: line,
	}
}

var (
	staticRe  = regexp.MustCompile(`^(.+?):(\d+):(\d+): (.*)$`)
	prepareRe = regexp.MustCompile(`^(.+?): (\S+): sqlc/db-prepare: error preparing query: (.*)$`)
	ruleRe    = regexp.MustCompile(`^(.+?): (\S+): (\S+): (.*)$`)
)

// ParseVetOutput turns sqlc vet's stderr into Breakages. Query file paths in
// the output are relative to the temp config dir and prefixed with prefix;
// they are returned relative to the working directory.
func ParseVetOutput(out, prefix, root string) []Breakage {
	var bs []Breakage
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := prepareRe.FindStringSubmatch(line); m != nil {
			bs = append(bs, Breakage{QueryFile: display(root, strings.TrimPrefix(m[1], prefix)), QueryName: m[2], Message: m[3]})
			continue
		}
		if m := staticRe.FindStringSubmatch(line); m != nil {
			ln := 0
			fmt.Sscanf(m[2], "%d", &ln)
			bs = append(bs, Breakage{QueryFile: display(root, strings.TrimPrefix(m[1], prefix)), Line: ln, Message: m[4], Static: true})
			continue
		}
		if m := ruleRe.FindStringSubmatch(line); m != nil {
			bs = append(bs, Breakage{QueryFile: display(root, strings.TrimPrefix(m[1], prefix)), QueryName: m[2], Message: m[3] + ": " + m[4]})
		}
	}
	return bs
}

// Attribute picks the new migration most likely responsible for a query
// failure. A statement that drops, renames or retypes a column named in the
// query outranks one that merely touches a table named in the query; ties go
// to the later file.
func Attribute(querySQL string, newFiles []string, files []*migrate.File, cat *catalog.Catalog) string {
	q := strings.ToLower(querySQL)
	isNew := map[string]bool{}
	for _, f := range newFiles {
		isNew[f] = true
	}
	parser, err := sqlparse.ForEngine(sqlparse.EnginePostgres)
	if err != nil {
		return ""
	}
	best, bestScore := "", 0
	for _, f := range files {
		if !isNew[f.Path] {
			continue
		}
		stmts, err := parser.Parse(f.Up.SQL)
		if err != nil {
			continue
		}
		for _, s := range stmts {
			score := 0
			for _, t := range s.Tables() {
				if mentions(q, t.Name) {
					score = 1
				}
			}
			switch s.Kind {
			case sqlparse.KindRenameColumn:
				if mentions(q, s.RenameColumn.Old) {
					score = 3
				}
			case sqlparse.KindRenameTable, sqlparse.KindDropTable:
				if score > 0 {
					score = 3
				}
			case sqlparse.KindAlterTable:
				for _, a := range s.AlterTable.Actions {
					switch a.Kind {
					case sqlparse.ActionDropColumn, sqlparse.ActionAlterColumnType, sqlparse.ActionSetNotNull:
						if mentions(q, a.Column) {
							score = 3
						}
					}
				}
			}
			if score >= bestScore && score > 0 {
				best, bestScore = f.Path, score
			}
		}
	}
	if best == "" && len(newFiles) == 1 {
		return newFiles[0]
	}
	return best
}

func mentions(q, table string) bool {
	if table == "" {
		return false
	}
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(table) + `\b`)
	return re.MatchString(q)
}

var nameRe = regexp.MustCompile(`(?m)^\s*--\s*name:\s*(\S+)`)

// queryAtLine returns the name and SQL of the sqlc query block that contains
// the given 1-based line (sqlc's static errors carry a position, not a name).
func queryAtLine(path string, line int) (string, string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	src := string(b)
	locs := nameRe.FindAllStringSubmatchIndex(src, -1)
	for i, loc := range locs {
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		startLine := 1 + strings.Count(src[:loc[0]], "\n")
		endLine := 1 + strings.Count(src[:end], "\n")
		if line >= startLine && line <= endLine {
			return src[loc[2]:loc[3]], strings.TrimSpace(src[loc[0]:end])
		}
	}
	return "", ""
}

// querySQL extracts the SQL for a named sqlc query from a query file.
func querySQL(path, name string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	src := string(b)
	locs := nameRe.FindAllStringSubmatchIndex(src, -1)
	for i, loc := range locs {
		if src[loc[2]:loc[3]] != name {
			continue
		}
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		return strings.TrimSpace(src[loc[0]:end])
	}
	return ""
}

func tempSqlcConfig(tmp string, pkg config.Package, queryRel []string, uri string) string {
	var b strings.Builder
	b.WriteString("version: \"2\"\nsql:\n  - engine: ")
	b.WriteString(string(pkg.Engine))
	b.WriteString("\n    schema:\n")
	for _, s := range pkg.Schema {
		abs, _ := filepath.Abs(s)
		rel, err := filepath.Rel(tmp, abs)
		if err != nil {
			rel = abs
		}
		fmt.Fprintf(&b, "      - %q\n", rel)
	}
	b.WriteString("    queries:\n")
	for _, q := range queryRel {
		fmt.Fprintf(&b, "      - %q\n", filepath.ToSlash(filepath.Join("against", q)))
	}
	fmt.Fprintf(&b, "    database:\n      uri: %q\n    rules:\n      - sqlc/db-prepare\n", uri)
	return b.String()
}

func runSqlcVet(ctx context.Context, sqlc, cfg string) (string, error) {
	cmd := exec.CommandContext(ctx, sqlc, "vet", "-f", cfg)
	cmd.Dir = filepath.Dir(cfg)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

// isFailedChecks distinguishes "queries failed" (exit 1 with per-query
// lines) from sqlc being unable to run at all.
func isFailedChecks(err error, out string) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if prepareRe.MatchString(line) || staticRe.MatchString(line) || ruleRe.MatchString(line) {
			return true
		}
	}
	return false
}

// --- git helpers ---

func gitRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("verify needs a git repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func gitOK(root string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	return cmd.Run()
}

func relToRoot(root, p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}

// display converts a repo-relative path to a working-directory-relative one.
func display(root, repoRel string) string {
	abs := filepath.Join(root, filepath.FromSlash(repoRel))
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, abs); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return abs
}

// changedSince reports whether path differs from its content at ref (or does
// not exist there).
func changedSince(root, ref, path string) (bool, error) {
	rel := relToRoot(root, path)
	cmd := exec.Command("git", "cat-file", "-p", ref+":"+rel)
	cmd.Dir = root
	old, err := cmd.Output()
	if err != nil {
		return true, nil // not present at ref
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return !bytes.Equal(old, cur), nil
}

// exportAt writes the .sql files under paths (as of ref) into dst, preserving
// repo-relative paths, and returns those relative paths.
func exportAt(root, ref string, paths []string, dst string) ([]string, error) {
	args := []string{"archive", "--format=tar", ref, "--"}
	for _, p := range paths {
		args = append(args, relToRoot(root, p))
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git archive %s: %s", ref, strings.TrimSpace(stderr.String()))
	}
	var files []string
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg || !strings.HasSuffix(strings.ToLower(h.Name), ".sql") {
			continue
		}
		target := filepath.Join(dst, filepath.FromSlash(h.Name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, b, 0o644); err != nil {
			return nil, err
		}
		files = append(files, h.Name)
	}
	sort.Strings(files)
	return files, nil
}
