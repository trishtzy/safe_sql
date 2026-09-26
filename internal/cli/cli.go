// Package cli wires the cobra commands.
package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	// Register engine parsers.
	_ "github.com/trishtzy/safe_sql/internal/sqlparse/postgres"
)

// Version is set by the linker at release time.
var Version = "dev"

// Main runs the CLI and returns the process exit code:
// 0 clean, 1 findings, 2 usage or parse error.
func Main(args []string) int {
	return Run(args, os.Stdout, os.Stderr)
}

// Run executes args with the given streams and returns the exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	var (
		code       int
		configPath string
	)
	root := &cobra.Command{
		Use:           "safe_sql",
		Short:         "Catch unsafe SQL migrations before they reach production",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "", "path to safe_sql.yaml (default: search upward from the working directory)")

	root.AddCommand(newLintCmd(&code, &configPath), newVerifyCmd(&code, &configPath), newRulesCmd(), newInitCmd(&configPath))

	if err := root.Execute(); err != nil {
		fmt.Fprintln(stderr, "safe_sql:", err)
		if code == 0 {
			code = 2
		}
	}
	return code
}
