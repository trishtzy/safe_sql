package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/trishtzy/safe_sql/internal/config"
)

func newInitCmd(configPath *string) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a starter safe_sql.yaml (detects sqlc.yaml)",
		RunE: func(cmd *cobra.Command, args []string) error {
			target := *configPath
			if target == "" {
				target = config.FileNames[0]
			}
			if _, err := os.Stat(target); err == nil && !force {
				return fmt.Errorf("%s already exists (use --force to overwrite)", target)
			}
			// Ignore any existing safe_sql.yaml so discovery reflects the sqlc project only.
			proj, err := config.LoadWithoutConfig(filepath.Dir(target))
			if err != nil {
				return err
			}
			proj.ConfigPath, _ = filepath.Abs(target)
			if err := os.WriteFile(target, []byte(config.Starter(proj)), 0o644); err != nil {
				return err
			}
			if proj.SqlcPath != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s (using %s for engine and paths)\n", target, proj.SqlcPath)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s; edit engine and schema to point at your migrations\n", target)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing file")
	return cmd
}
