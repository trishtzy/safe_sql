package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/trishtzy/safe_sql/internal/rules"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

func newRulesCmd() *cobra.Command {
	var engine, format string
	cmd := &cobra.Command{
		Use:   "rules",
		Short: "List the available rules",
		RunE: func(cmd *cobra.Command, args []string) error {
			var list []*rules.Rule
			if engine == "" {
				list = rules.All()
			} else {
				list = rules.ForEngine(sqlparse.Engine(engine))
			}
			out := cmd.OutOrStdout()
			if format == "markdown" {
				fmt.Fprintln(out, "| Rule | Engines | Severity | Description |")
				fmt.Fprintln(out, "|---|---|---|---|")
				for _, r := range list {
					sev := string(r.Severity)
					if r.OptIn {
						sev += " (opt-in)"
					}
					fmt.Fprintf(out, "| `%s` | %s | %s | %s |\n", r.ID, engines(r), sev, r.Summary)
				}
				return nil
			}
			for _, r := range list {
				flag := ""
				if r.OptIn {
					flag = " (opt-in)"
				}
				fmt.Fprintf(out, "%-36s %-8s %-18s %s\n", r.ID, r.Severity, engines(r)+flag, r.Summary)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&engine, "engine", "", "only rules for this engine")
	cmd.Flags().StringVar(&format, "format", "text", "text or markdown")
	return cmd
}

func engines(r *rules.Rule) string {
	xs := make([]string, len(r.Engines))
	for i, e := range r.Engines {
		xs[i] = string(e)
	}
	return strings.Join(xs, ",")
}
