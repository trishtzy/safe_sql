package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/trishtzy/safe_sql/internal/lint"
	"github.com/trishtzy/safe_sql/internal/migrate"
	"github.com/trishtzy/safe_sql/internal/report"
	"github.com/trishtzy/safe_sql/internal/rules"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

type lintFlags struct {
	engine, tool, targetVersion, startAfter, format string
	checkDown, strict, dump                         bool
	disable, enable, only                           []string
}

func newLintCmd(code *int) *cobra.Command {
	var fl lintFlags
	cmd := &cobra.Command{
		Use:   "lint [paths...]",
		Short: "Lint migration files for unsafe operations",
		Long: `Lint parses each migration file (files or directories, in lexicographic
order, honouring goose/golang-migrate/dbmate/tern/sql-migrate/atlas up/down
markers) and reports operations that lock tables or break running code.

Exit codes: 0 clean, 1 findings (errors, or warnings with --strict), 2 usage
or parse error.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ver, err := rules.ParseVersion(fl.targetVersion)
			if err != nil {
				return err
			}
			opts := lint.Options{
				Engine: sqlparse.Engine(fl.engine), TargetVersion: ver, Tool: migrate.Tool(fl.tool),
				PlainInTransaction: true, StartAfter: fl.startAfter, CheckDown: fl.checkDown,
				Disabled: fl.disable, Enabled: fl.enable, Only: fl.only,
			}
			if fl.dump {
				return dump(cmd, args, opts)
			}
			res, err := lint.Run(args, opts)
			if err != nil {
				return err
			}
			if err := report.Write(cmd.OutOrStdout(), res, report.Format(fl.format)); err != nil {
				return err
			}
			switch {
			case len(res.Failures) > 0:
				*code = 2
			case res.Errors() > 0 || (fl.strict && res.Warnings() > 0):
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
	f.StringVar(&fl.engine, "engine", "postgresql", "database engine ("+strings.Join(sqlparse.Engines(), ", ")+")")
	f.StringVar(&fl.tool, "migration-tool", "auto", "migration tool convention (auto, plain, goose, golang-migrate, dbmate, tern, sql-migrate, atlas)")
	f.StringVar(&fl.targetVersion, "target-version", "", "production database version, e.g. 16 or 3.35; unset assumes latest")
	f.StringVar(&fl.startAfter, "start-after", "", "skip findings in files whose version prefix is <= this")
	f.StringVar(&fl.format, "format", "human", "output format (human, json, github)")
	f.BoolVar(&fl.checkDown, "check-down", false, "also lint down sections")
	f.BoolVar(&fl.strict, "strict", false, "exit 1 on warnings too")
	f.StringSliceVar(&fl.disable, "disable", nil, "rule IDs to disable")
	f.StringSliceVar(&fl.enable, "enable", nil, "opt-in rule IDs to enable")
	f.StringSliceVar(&fl.only, "only", nil, "run only these rule IDs")
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
		fmt.Fprintf(out, "%s (tool=%s tx=%v version=%q)\n", path, f.Tool, f.InTransaction(len(stmts), true), f.Version)
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
