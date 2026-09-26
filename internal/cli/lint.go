package cli

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/trishtzy/safe_sql/internal/config"
	"github.com/trishtzy/safe_sql/internal/lint"
	"github.com/trishtzy/safe_sql/internal/migrate"
	"github.com/trishtzy/safe_sql/internal/report"
	"github.com/trishtzy/safe_sql/internal/rules"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

type lintFlags struct {
	engine, tool, targetVersion, startAfter, format, pkg, changedSince string
	checkDown, strict, dump                                            bool
	disable, enable, only                                              []string
}

func newLintCmd(code *int, configPath *string) *cobra.Command {
	var fl lintFlags
	cmd := &cobra.Command{
		Use:   "lint [paths...]",
		Short: "Lint migration files for unsafe operations",
		Long: `Lint parses each migration file (files or directories, in lexicographic
order, honouring goose/golang-migrate/dbmate/tern/sql-migrate/atlas up/down
markers) and reports operations that lock tables or break running code.

With no paths, the schema paths come from safe_sql.yaml or, failing that,
from sqlc.yaml (every supported sql[] package). Flags override both.

Exit codes: 0 clean, 1 findings (errors, or warnings with --strict), 2 usage
or parse error.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			runs, err := resolveRuns(cmd, args, &fl, *configPath)
			if err != nil {
				return err
			}
			if fl.dump {
				for _, r := range runs {
					if err := dump(cmd, r.paths, r.opts); err != nil {
						return err
					}
				}
				return nil
			}
			combined := &lint.Result{}
			for _, r := range runs {
				res, err := lint.Run(r.paths, r.opts)
				if err != nil {
					return err
				}
				combined.Findings = append(combined.Findings, res.Findings...)
				combined.Failures = append(combined.Failures, res.Failures...)
				combined.Files = append(combined.Files, res.Files...)
			}
			if err := report.Write(cmd.OutOrStdout(), combined, report.Format(fl.format)); err != nil {
				return err
			}
			switch {
			case len(combined.Failures) > 0:
				*code = 2
			case combined.Errors() > 0 || (fl.strict && combined.Warnings() > 0):
				*code = 1
			}
			return nil
		},
	}
	addLintFlags(cmd, &fl)
	cmd.Flags().BoolVar(&fl.dump, "dump", false, "print the parsed statement model instead of linting")
	return cmd
}

func addLintFlags(cmd *cobra.Command, fl *lintFlags) {
	f := cmd.Flags()
	f.StringVar(&fl.engine, "engine", "", "database engine ("+strings.Join(sqlparse.Engines(), ", ")+"); default from config, else postgresql")
	f.StringVar(&fl.tool, "migration-tool", "", "migration tool convention (auto, plain, goose, golang-migrate, dbmate, tern, sql-migrate, atlas)")
	f.StringVar(&fl.targetVersion, "target-version", "", "production database version, e.g. 16 or 3.35; unset assumes latest")
	f.StringVar(&fl.startAfter, "start-after", "", "skip findings in files whose version prefix is <= this")
	f.StringVar(&fl.format, "format", "human", "output format (human, json, github)")
	f.StringVar(&fl.pkg, "package", "", "only lint this sqlc package (by name)")
	f.BoolVar(&fl.checkDown, "check-down", false, "also lint down sections")
	f.BoolVar(&fl.strict, "strict", false, "exit 1 on warnings too")
	f.StringSliceVar(&fl.disable, "disable", nil, "rule IDs to disable (adds to config)")
	f.StringSliceVar(&fl.enable, "enable", nil, "opt-in rule IDs to enable (adds to config)")
	f.StringSliceVar(&fl.only, "only", nil, "run only these rule IDs")
	f.StringVar(&fl.changedSince, "changed-since", "", "only report files added or modified since this git ref (all files still build the schema model)")
}

// lintRun is one lint invocation: an engine and its paths.
type lintRun struct {
	name  string
	paths []string
	opts  lint.Options
}

// resolveRuns merges config and flags into one run per package (or a single
// run for explicit paths).
func resolveRuns(cmd *cobra.Command, args []string, fl *lintFlags, configPath string) ([]lintRun, error) {
	proj, err := config.Load(".", configPath)
	if err != nil {
		return nil, err
	}
	base := lint.Options{
		Tool: proj.Tool, TargetVersion: proj.TargetVersion, StartAfter: proj.StartAfter,
		CheckDown: proj.CheckDown, PlainInTransaction: proj.PlainInTransaction,
		Disabled: append([]string{}, proj.Disabled...), Enabled: append([]string{}, proj.Enabled...),
		Severity: proj.Severity, Only: fl.only,
	}
	if fl.tool != "" {
		base.Tool = migrate.Tool(fl.tool)
	}
	if base.Tool == "" {
		base.Tool = migrate.ToolAuto
	}
	if fl.targetVersion != "" {
		v, err := rules.ParseVersion(fl.targetVersion)
		if err != nil {
			return nil, err
		}
		base.TargetVersion = v
	}
	if fl.startAfter != "" {
		base.StartAfter = fl.startAfter
	}
	if cmd.Flags().Changed("check-down") {
		base.CheckDown = fl.checkDown
	}
	base.Disabled = append(base.Disabled, fl.disable...)
	base.Enabled = append(base.Enabled, fl.enable...)

	if fl.changedSince != "" {
		changed, err := gitChangedFiles(fl.changedSince)
		if err != nil {
			return nil, err
		}
		base.ReportOnly = changed
	}

	if len(args) > 0 {
		eng := sqlparse.Engine(fl.engine)
		if eng == "" {
			eng = sqlparse.EnginePostgres
			if len(proj.Packages) == 1 {
				eng = proj.Packages[0].Engine
			}
		}
		base.Engine = eng
		return []lintRun{{name: "args", paths: args, opts: base}}, nil
	}

	if len(proj.Packages) == 0 {
		return nil, fmt.Errorf("no paths given and no schema configured: pass migration paths, add `schema:` to safe_sql.yaml, or run inside a sqlc project (run `safe_sql init` to get started)")
	}
	for _, s := range proj.Skipped {
		fmt.Fprintf(cmd.ErrOrStderr(), "safe_sql: skipping sqlc package %s: unsupported engine\n", s)
	}
	var runs []lintRun
	for _, p := range proj.Packages {
		if fl.pkg != "" && p.Name != fl.pkg {
			continue
		}
		opts := base
		opts.Engine = p.Engine
		if fl.engine != "" {
			opts.Engine = sqlparse.Engine(fl.engine)
		}
		runs = append(runs, lintRun{name: p.Name, paths: p.Schema, opts: opts})
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("no package named %q in %s", fl.pkg, proj.SqlcPath)
	}
	return runs, nil
}

// gitChangedFiles lists files added, modified or untracked relative to ref,
// as paths relative to the working directory.
func gitChangedFiles(ref string) ([]string, error) {
	var out []string
	for _, args := range [][]string{
		{"diff", "--name-only", "--diff-filter=AMR", "--relative", ref},
		{"ls-files", "--others", "--exclude-standard"},
	} {
		b, err := exec.Command("git", args...).Output()
		if err != nil {
			return nil, fmt.Errorf("git %s: %w (is %q a valid ref?)", strings.Join(args, " "), err, ref)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line != "" {
				out = append(out, filepath.Clean(line))
			}
		}
	}
	if out == nil {
		out = []string{"\x00none"} // nothing changed: report nothing, but keep ReportOnly non-empty
	}
	return out, nil
}

func dump(cmd *cobra.Command, paths []string, opts lint.Options) error {
	parser, err := sqlparse.ForEngine(opts.Engine)
	if err != nil {
		return err
	}
	files, err := migrate.Discover(paths)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	for _, path := range files {
		f, err := migrate.Load(path, opts.Tool)
		if err != nil {
			return err
		}
		stmts, err := parser.Parse(f.Up.SQL)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		fmt.Fprintf(out, "%s (tool=%s tx=%v version=%q)\n", path, f.Tool, f.InTransaction(len(stmts), opts.PlainInTransaction), f.Version)
		for _, s := range stmts {
			fmt.Fprintf(out, "  %d: %s", f.Line(f.Up, s.Offset), s.Kind)
			if s.Kind == sqlparse.KindAlterTable {
				for _, a := range s.AlterTable.Actions {
					fmt.Fprintf(out, " [%s]", a.Raw)
				}
			}
			for _, t := range s.Tables() {
				fmt.Fprintf(out, " %s", t)
			}
			if len(s.LeadingComments) > 0 {
				fmt.Fprintf(out, " comments=%q", s.LeadingComments)
			}
			fmt.Fprintln(out)
		}
	}
	return nil
}
