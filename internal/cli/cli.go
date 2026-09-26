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
	return run(args, os.Stdout, os.Stderr)
}

func run(args []string, stdout, stderr io.Writer) int {
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

	var code int
	root.AddCommand(newLintCmd(&code))

	if err := root.Execute(); err != nil {
		fmt.Fprintln(stderr, "safe_sql:", err)
		if code == 0 {
			code = 2
		}
	}
	return code
}
