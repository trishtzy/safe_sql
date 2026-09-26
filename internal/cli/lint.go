package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/trishtzy/safe_sql/internal/migrate"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

func newLintCmd(code *int) *cobra.Command {
	var (
		engine string
		tool   string
		dump   bool
	)
	cmd := &cobra.Command{
		Use:   "lint [paths...]",
		Short: "Lint migration files for unsafe operations",
		RunE: func(cmd *cobra.Command, args []string) error {
			parser, err := sqlparse.ForEngine(sqlparse.Engine(engine))
			if err != nil {
				return err
			}
			files, err := migrate.Discover(args)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, path := range files {
				f, err := migrate.Load(path, migrate.Tool(tool))
				if err != nil {
					return err
				}
				stmts, err := parser.Parse(f.Up.SQL)
				if err != nil {
					if pe, ok := err.(*sqlparse.ParseError); ok {
						return fmt.Errorf("%s:%d: %s", path, f.Line(f.Up, pe.Offset), pe.Message)
					}
					return fmt.Errorf("%s: %w", path, err)
				}
				if dump {
					fmt.Fprintf(out, "%s (tool=%s tx=%v version=%s)\n", path, f.Tool, f.InTransaction(len(stmts), true), f.Version)
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
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&engine, "engine", "postgresql", "database engine (postgresql, sqlite)")
	cmd.Flags().StringVar(&tool, "migration-tool", "auto", "migration tool convention (auto, plain, goose, golang-migrate, dbmate, tern, sql-migrate, atlas)")
	cmd.Flags().BoolVar(&dump, "dump", false, "print the parsed statement model instead of linting")
	return cmd
}
