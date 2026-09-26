package verify

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/trishtzy/safe_sql/internal/catalog"
	"github.com/trishtzy/safe_sql/internal/migrate"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

// database is a connection safe_sql can apply migrations through, plus the
// URI sqlc should use to reach the same database.
type database interface {
	Exec(ctx context.Context, sql string) error
	URI() string
	Close() error
}

type pgDB struct {
	conn *pgx.Conn
	uri  string
}

func (p *pgDB) Exec(ctx context.Context, sql string) error {
	_, err := p.conn.Exec(ctx, sql)
	return err
}
func (p *pgDB) URI() string  { return p.uri }
func (p *pgDB) Close() error { return p.conn.Close(context.Background()) }

// openDatabase returns a database for the package's engine: the configured
// URL, or an ephemeral container.
func openDatabase(ctx context.Context, opts Options) (database, func(), error) {
	switch opts.Package.Engine {
	case sqlparse.EnginePostgres:
		return openPostgres(ctx, opts)
	default:
		return nil, nil, fmt.Errorf("verify does not support engine %q yet", opts.Package.Engine)
	}
}

func openPostgres(ctx context.Context, opts Options) (database, func(), error) {
	uri := opts.DatabaseURL
	cleanup := func() {}
	if uri == "" {
		if !opts.Docker {
			return nil, nil, errors.New("no database: set verify.database_url / --database-url, or allow Docker (verify.docker: true) to start a throwaway Postgres")
		}
		image := "postgres:17"
		if v := opts.Lint.TargetVersion; v.Known {
			image = fmt.Sprintf("postgres:%d", v.Major)
		}
		fmt.Fprintf(opts.Stderr, "safe_sql: starting %s container (set verify.database_url to skip)\n", image)
		c, err := tcpostgres.Run(ctx, image,
			tcpostgres.WithDatabase("safe_sql"), tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("postgres"),
			tcpostgres.BasicWaitStrategies())
		if err != nil {
			return nil, nil, fmt.Errorf("start postgres container: %w (is Docker running? or set verify.database_url)", err)
		}
		cleanup = func() { _ = testcontainers.TerminateContainer(c) }
		uri, err = c.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	conn, err := pgx.Connect(ctx, uri)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("connect to %s: %w", redact(uri), err)
	}

	// Work in a throwaway database on the server so repeated runs and shared
	// dev servers never see leftover schema. Fall back to the given database
	// when the role cannot CREATE DATABASE.
	name := fmt.Sprintf("safe_sql_verify_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		fmt.Fprintf(opts.Stderr, "safe_sql: cannot create a scratch database (%v); using %s as-is, it must be empty\n", err, redact(uri))
		db := &pgDB{conn: conn, uri: uri}
		return db, func() { db.Close(); cleanup() }, nil
	}
	scratch, err := withDatabase(uri, name)
	if err != nil {
		conn.Close(ctx)
		cleanup()
		return nil, nil, err
	}
	sconn, err := pgx.Connect(ctx, scratch)
	if err != nil {
		conn.Exec(ctx, "DROP DATABASE "+name)
		conn.Close(ctx)
		cleanup()
		return nil, nil, fmt.Errorf("connect to scratch database: %w", err)
	}
	db := &pgDB{conn: sconn, uri: scratch}
	return db, func() {
		db.Close()
		bg := context.Background()
		if _, err := conn.Exec(bg, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			conn.Exec(bg, "DROP DATABASE "+name)
		}
		conn.Close(bg)
		cleanup()
	}, nil
}

// withDatabase returns uri pointing at a different database name.
func withDatabase(uri, name string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

// applyMigrations executes every up statement of every file in order and
// replays them into cat for attribution.
func applyMigrations(ctx context.Context, db database, files []*migrate.File, opts Options, cat *catalog.Catalog) error {
	parser, err := sqlparse.ForEngine(opts.Package.Engine)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.IsDownFile {
			continue
		}
		stmts, err := parser.Parse(f.Up.SQL)
		if err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
		for _, st := range stmts {
			if st.Kind == sqlparse.KindTransaction {
				continue // each statement runs on its own; explicit BEGIN/COMMIT are not needed
			}
			if err := db.Exec(ctx, st.Raw); err != nil {
				return fmt.Errorf("%s:%d: applying migration: %w", f.Path, f.Line(f.Up, st.Offset), err)
			}
			cat.Apply(&st, f.Path)
		}
	}
	return nil
}

func redact(uri string) string {
	if i := strings.Index(uri, "@"); i > 0 {
		if j := strings.Index(uri, "//"); j >= 0 && j < i {
			return uri[:j+2] + "***" + uri[i:]
		}
	}
	return uri
}
