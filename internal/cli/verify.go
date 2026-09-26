package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/trishtzy/safe_sql/internal/config"
	"github.com/trishtzy/safe_sql/internal/lint"
	"github.com/trishtzy/safe_sql/internal/report"
	"github.com/trishtzy/safe_sql/internal/verify"
)

func newVerifyCmd(code *int, configPath *string) *cobra.Command {
	var (
		fl                          lintFlags
		against, dbURL, sqlcPath    string
		noDocker, keepTemp, verbose bool
	)
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Check that deployed queries still work against the proposed schema (local sqlc verify)",
		Long: `Verify applies every migration to a throwaway database (or --database-url),
then runs "sqlc vet" with the sqlc/db-prepare rule using the query files as
they exist at --against (default: main). Every query that fails to compile
or prepare is reported against the migration most likely responsible. New
migration files are also linted.

Requires git, the sqlc binary, and either Docker or a database URL.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			proj, err := config.Load(".", *configPath)
			if err != nil {
				return err
			}
			runs, err := resolveRuns(cmd, nil, &fl, *configPath)
			if err != nil {
				return err
			}
			pkgs := map[string]config.Package{}
			for _, p := range proj.Packages {
				pkgs[p.Name] = p
			}
			combined := &lint.Result{}
			for _, r := range runs {
				pkg := pkgs[r.name]
				if len(pkg.Queries) == 0 {
					return fmt.Errorf("package %s has no queries paths; verify needs sqlc `queries:` (or `queries:` in safe_sql.yaml)", pkg.Name)
				}
				opts := verify.Options{
					Package: pkg, Against: firstNonEmpty(against, proj.Verify.BaseRef, "main"),
					DatabaseURL: firstNonEmpty(dbURL, proj.Verify.DatabaseURL, pkg.DatabaseURL),
					Docker:      !noDocker && (proj.Verify.Docker == nil || *proj.Verify.Docker),
					SqlcPath:    firstNonEmpty(sqlcPath, proj.Verify.SqlcPath, os.Getenv("SAFE_SQL_SQLC")),
					Lint:        r.opts, Stderr: cmd.ErrOrStderr(), KeepTemp: keepTemp,
				}
				res, err := verify.Run(context.Background(), opts)
				if err != nil {
					return err
				}
				if verbose {
					fmt.Fprintln(cmd.ErrOrStderr(), res.SqlcOutput)
				}
				combined.Findings = append(combined.Findings, res.Lint.Findings...)
				combined.Failures = append(combined.Failures, res.Lint.Failures...)
				combined.Files = append(combined.Files, res.Lint.Files...)
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
	cmd.Flags().StringVar(&against, "against", "", "git ref whose queries are deployed (default: verify.base_ref or main)")
	cmd.Flags().StringVar(&dbURL, "database-url", "", "database to apply migrations to (default: start a throwaway container)")
	cmd.Flags().StringVar(&sqlcPath, "sqlc-path", "", "sqlc binary (default: sqlc on PATH)")
	cmd.Flags().BoolVar(&noDocker, "no-docker", false, "never start a container")
	cmd.Flags().BoolVar(&keepTemp, "keep-temp", false, "keep the temporary sqlc project for debugging")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "print sqlc vet's raw output")
	return cmd
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
