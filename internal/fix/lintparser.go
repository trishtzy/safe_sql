package fix

import (
	"github.com/trishtzy/safe_sql/internal/lint"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

func lintParser(opts lint.Options) (sqlparse.Parser, error) {
	return sqlparse.ForEngine(opts.Engine)
}
