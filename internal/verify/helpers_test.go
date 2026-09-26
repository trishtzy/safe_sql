package verify

import (
	"testing"

	"github.com/trishtzy/safe_sql/internal/config"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

func packageFor(t *testing.T) config.Package {
	t.Helper()
	return config.Package{Name: "app", Engine: sqlparse.EnginePostgres, Schema: []string{"db/migrations"}, Queries: []string{"db/queries"}}
}
