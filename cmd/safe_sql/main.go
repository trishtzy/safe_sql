// Command safe_sql lints SQL migration files for operations that lock tables
// or break running applications, in the spirit of strong_migrations.
package main

import (
	"os"

	"github.com/trishtzy/safe_sql/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
