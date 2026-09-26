// Package config loads safe_sql.yaml and discovers sqlc.yaml so sqlc projects
// need no extra setup.
//
// Precedence: CLI flags > safe_sql.yaml > sqlc.yaml. Paths in either file are
// resolved relative to that file's directory, as sqlc does.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/trishtzy/safe_sql/internal/migrate"
	"github.com/trishtzy/safe_sql/internal/rules"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

// FileNames are the config files looked for, in order, in each directory
// from the working directory upward.
var FileNames = []string{"safe_sql.yaml", "safe_sql.yml"}

// SqlcFileNames mirrors sqlc's own lookup order.
var SqlcFileNames = []string{"sqlc.yaml", "sqlc.yml", "sqlc.json"}

// File is the on-disk shape of safe_sql.yaml. Every key is optional.
type File struct {
	Version       string `yaml:"version"`
	Sqlc          string `yaml:"sqlc"`
	Engine        string `yaml:"engine"`
	Schema        Paths  `yaml:"schema"`
	Queries       Paths  `yaml:"queries"`
	MigrationTool string `yaml:"migration_tool"`
	TargetVersion string `yaml:"target_version"`
	StartAfter    string `yaml:"start_after"`
	CheckDown     bool   `yaml:"check_down"`
	// PlainInTransaction is the transaction assumption for files whose tool
	// cannot be detected (nil = true).
	PlainInTransaction *bool  `yaml:"plain_in_transaction"`
	Rules              Rules  `yaml:"rules"`
	Verify             Verify `yaml:"verify"`
	AI                 AI     `yaml:"ai"`
}

// Rules configures which rules run and at what severity.
type Rules struct {
	Disable  []string          `yaml:"disable"`
	Enable   []string          `yaml:"enable"`
	Severity map[string]string `yaml:"severity"`
}

// Verify configures the verify command.
type Verify struct {
	DatabaseURL string `yaml:"database_url"`
	Docker      *bool  `yaml:"docker"`
	BaseRef     string `yaml:"base_ref"`
	SqlcPath    string `yaml:"sqlc_path"`
}

// AI configures the AI fix flow.
type AI struct {
	Enabled                 bool     `yaml:"enabled"`
	Trigger                 string   `yaml:"trigger"`
	Auto                    bool     `yaml:"auto"`
	Mode                    string   `yaml:"mode"`
	Provider                string   `yaml:"provider"`
	Model                   string   `yaml:"model"`
	MaxIterations           int      `yaml:"max_iterations"`
	Rules                   []string `yaml:"rules"`
	DeterministicFirst      *bool    `yaml:"deterministic_first"`
	AllowDisableAnnotations bool     `yaml:"allow_disable_annotations"`
	AllowedAssociations     []string `yaml:"allowed_associations"`
	CommitMessage           string   `yaml:"commit_message"`
}

// Paths accepts a single string or a list, like sqlc's schema/queries keys.
type Paths []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (p *Paths) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		*p = Paths{s}
		return nil
	}
	var list []string
	if err := value.Decode(&list); err != nil {
		return err
	}
	*p = list
	return nil
}

// UnmarshalJSON implements json.Unmarshaler.
func (p *Paths) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*p = Paths{s}
		return nil
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	*p = list
	return nil
}

// Package is one lintable unit: an engine plus its schema (and queries) paths.
// A sqlc.yaml with several sql[] entries yields several packages.
type Package struct {
	Name    string
	Engine  sqlparse.Engine
	Schema  []string
	Queries []string
	// DatabaseURL is sqlc's database.uri, if any (used by verify).
	DatabaseURL string
}

// Project is the merged, resolved configuration.
type Project struct {
	// ConfigPath is the safe_sql.yaml that was loaded ("" when none).
	ConfigPath string
	// SqlcPath is the sqlc config that was loaded ("" when none).
	SqlcPath string
	Packages []Package
	// Skipped lists sqlc packages whose engine safe_sql does not support.
	Skipped []string

	Tool               migrate.Tool
	TargetVersion      rules.Version
	StartAfter         string
	CheckDown          bool
	PlainInTransaction bool
	Disabled, Enabled  []string
	Severity           map[string]rules.Severity
	Verify             Verify
	AI                 AI
	File               File
}

// Load resolves configuration. explicitConfig, when non-empty, is the
// safe_sql.yaml to use; otherwise FileNames are searched upward from dir.
func Load(dir, explicitConfig string) (*Project, error) {
	return load(dir, explicitConfig, false)
}

// LoadWithoutConfig ignores any safe_sql.yaml and only discovers sqlc
// configuration from dir upward. `safe_sql init` uses it.
func LoadWithoutConfig(dir string) (*Project, error) {
	return load(dir, "", true)
}

func load(dir, explicitConfig string, skipConfig bool) (*Project, error) {
	p := &Project{PlainInTransaction: true, Tool: migrate.ToolAuto, Severity: map[string]rules.Severity{}}
	p.AI = defaultAI()

	cfgPath := explicitConfig
	if cfgPath == "" && !skipConfig {
		cfgPath = findUp(dir, FileNames)
	}
	var f File
	if cfgPath != "" {
		b, err := os.ReadFile(cfgPath)
		if err != nil {
			return nil, err
		}
		dec := yaml.NewDecoder(strings.NewReader(os.ExpandEnv(string(b))))
		dec.KnownFields(true)
		if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: %w", cfgPath, err)
		}
		p.ConfigPath = cfgPath
		p.File = f
	}
	cfgDir := dir
	if cfgPath != "" {
		cfgDir = filepath.Dir(cfgPath)
	}

	// sqlc discovery: explicit `sqlc:` key, else search upward from the
	// safe_sql.yaml directory (or dir when there is no safe_sql.yaml).
	sqlcPath := ""
	switch {
	case f.Sqlc != "":
		sqlcPath = resolve(cfgDir, f.Sqlc)
		if _, err := os.Stat(sqlcPath); err != nil {
			return nil, fmt.Errorf("sqlc config %s: %w", sqlcPath, err)
		}
	default:
		sqlcPath = findUp(cfgDir, SqlcFileNames)
	}
	if sqlcPath != "" {
		pkgs, skipped, err := loadSqlc(sqlcPath)
		if err != nil {
			return nil, err
		}
		p.SqlcPath, p.Packages, p.Skipped = sqlcPath, pkgs, skipped
	}

	// safe_sql.yaml overrides: engine/schema/queries replace the sqlc packages.
	if f.Engine != "" || len(f.Schema) > 0 {
		pkg := Package{Name: "default"}
		if len(p.Packages) == 1 {
			pkg = p.Packages[0]
		}
		if f.Engine != "" {
			pkg.Engine = sqlparse.Engine(f.Engine)
		}
		if len(f.Schema) > 0 {
			pkg.Schema = resolveAll(cfgDir, f.Schema)
		}
		if len(f.Queries) > 0 {
			pkg.Queries = resolveAll(cfgDir, f.Queries)
		}
		if pkg.Engine == "" {
			return nil, fmt.Errorf("%s: engine is required when schema is set and no sqlc config is found", cfgPath)
		}
		p.Packages = []Package{pkg}
	}

	if f.MigrationTool != "" {
		p.Tool = migrate.Tool(f.MigrationTool)
		if !validTool(p.Tool) {
			return nil, fmt.Errorf("%s: unknown migration_tool %q", cfgPath, f.MigrationTool)
		}
	}
	v, err := rules.ParseVersion(f.TargetVersion)
	if err != nil {
		return nil, fmt.Errorf("%s: target_version: %w", cfgPath, err)
	}
	p.TargetVersion = v
	p.StartAfter = f.StartAfter
	p.CheckDown = f.CheckDown
	if f.PlainInTransaction != nil {
		p.PlainInTransaction = *f.PlainInTransaction
	}
	p.Disabled, p.Enabled = f.Rules.Disable, f.Rules.Enable
	for id, sev := range f.Rules.Severity {
		s := rules.Severity(sev)
		if s != rules.SeverityError && s != rules.SeverityWarning {
			return nil, fmt.Errorf("%s: rules.severity.%s: want error or warning, got %q", cfgPath, id, sev)
		}
		p.Severity[id] = s
	}
	for _, id := range append(append([]string{}, p.Disabled...), p.Enabled...) {
		if _, ok := rules.Get(id); !ok {
			return nil, fmt.Errorf("%s: unknown rule %q", cfgPath, id)
		}
	}
	p.Verify = f.Verify
	mergeAI(&p.AI, f.AI)
	return p, nil
}

func defaultAI() AI {
	t := true
	return AI{
		Trigger: "@safe_sql_ai", Mode: "commit", Provider: "anthropic", Model: "claude-opus-5",
		MaxIterations: 3, DeterministicFirst: &t,
		AllowedAssociations: []string{"OWNER", "MEMBER", "COLLABORATOR"},
		CommitMessage:       "safe_sql: rewrite migration using a safer pattern",
	}
}

func mergeAI(dst *AI, src AI) {
	dst.Enabled, dst.Auto, dst.AllowDisableAnnotations = src.Enabled, src.Auto, src.AllowDisableAnnotations
	if src.Trigger != "" {
		dst.Trigger = src.Trigger
	}
	if src.Mode != "" {
		dst.Mode = src.Mode
	}
	if src.Provider != "" {
		dst.Provider = src.Provider
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.MaxIterations > 0 {
		dst.MaxIterations = src.MaxIterations
	}
	if len(src.Rules) > 0 {
		dst.Rules = src.Rules
	}
	if src.DeterministicFirst != nil {
		dst.DeterministicFirst = src.DeterministicFirst
	}
	if len(src.AllowedAssociations) > 0 {
		dst.AllowedAssociations = src.AllowedAssociations
	}
	if src.CommitMessage != "" {
		dst.CommitMessage = src.CommitMessage
	}
}

func validTool(t migrate.Tool) bool {
	for _, x := range migrate.Tools {
		if x == t {
			return true
		}
	}
	return false
}

// sqlcFile is the subset of sqlc.yaml (v1 and v2) that safe_sql reads. It is
// decoded leniently: unknown keys are ignored.
type sqlcFile struct {
	Version  string        `yaml:"version" json:"version"`
	SQL      []sqlcPackage `yaml:"sql" json:"sql"`
	Packages []sqlcPackage `yaml:"packages" json:"packages"`
}

type sqlcPackage struct {
	Name     string `yaml:"name" json:"name"`
	Engine   string `yaml:"engine" json:"engine"`
	Schema   Paths  `yaml:"schema" json:"schema"`
	Queries  Paths  `yaml:"queries" json:"queries"`
	Database *struct {
		URI string `yaml:"uri" json:"uri"`
	} `yaml:"database" json:"database"`
}

func loadSqlc(path string) ([]Package, []string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var sf sqlcFile
	if strings.HasSuffix(path, ".json") {
		err = json.Unmarshal(b, &sf)
	} else {
		err = yaml.Unmarshal(b, &sf)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	dir := filepath.Dir(path)
	entries := sf.SQL
	if len(entries) == 0 {
		entries = sf.Packages
	}
	var pkgs []Package
	var skipped []string
	for i, e := range entries {
		name := e.Name
		if name == "" {
			name = fmt.Sprintf("sql[%d]", i)
		}
		eng := sqlparse.Engine(e.Engine)
		if eng != sqlparse.EnginePostgres && eng != sqlparse.EngineSQLite {
			skipped = append(skipped, fmt.Sprintf("%s (engine %q)", name, e.Engine))
			continue
		}
		pkg := Package{Name: name, Engine: eng, Schema: resolveAll(dir, e.Schema), Queries: resolveAll(dir, e.Queries)}
		if e.Database != nil {
			pkg.DatabaseURL = os.ExpandEnv(e.Database.URI)
		}
		pkgs = append(pkgs, pkg)
	}
	return pkgs, skipped, nil
}

// resolve joins p to dir (when relative) and then, when the result lies
// under the working directory, returns it relative to the working directory
// so reported paths are short and stable.
func resolve(dir, p string) string {
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(dir, p)
	}
	abs = filepath.Clean(abs)
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, abs); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return abs
}

func resolveAll(dir string, ps []string) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = resolve(dir, p)
	}
	return out
}

// findUp searches dir and its parents for the first existing file among names.
func findUp(dir string, names []string) string {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		for _, n := range names {
			p := filepath.Join(dir, n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// Starter returns a commented safe_sql.yaml for `safe_sql init`.
func Starter(p *Project) string {
	var b strings.Builder
	b.WriteString("# safe_sql configuration. Every key is optional.\n")
	b.WriteString("# Docs: https://github.com/trishtzy/safe_sql\n")
	b.WriteString("version: \"1\"\n\n")
	if p.SqlcPath != "" {
		rel, err := filepath.Rel(filepath.Dir(p.ConfigPathOrCwd()), p.SqlcPath)
		if err != nil {
			rel = p.SqlcPath
		}
		fmt.Fprintf(&b, "# Engine, schema and queries paths are read from this sqlc config.\nsqlc: %s\n\n", rel)
		b.WriteString("# Uncomment to override what sqlc.yaml says:\n# engine: postgresql\n# schema: [db/migrations]\n# queries: [db/queries]\n\n")
	} else {
		eng, schema := "postgresql", "[db/migrations]"
		if len(p.Packages) > 0 {
			eng = string(p.Packages[0].Engine)
			if len(p.Packages[0].Schema) > 0 {
				schema = "[" + strings.Join(p.Packages[0].Schema, ", ") + "]"
			}
		}
		fmt.Fprintf(&b, "# No sqlc.yaml found; configure paths here.\nengine: %s        # postgresql | sqlite\nschema: %s\n# queries: [db/queries]   # used by `safe_sql verify`\n\n", eng, schema)
	}
	b.WriteString(`# Migration tool convention: auto | plain | goose | golang-migrate | dbmate | tern | sql-migrate | atlas
migration_tool: auto

# Production database version, so version-gated rules give the right advice.
# Postgres major ("16") or SQLite version ("3.45"). Unset assumes the latest.
# target_version: "16"

# Migrations at or before this version prefix are trusted (they still feed the
# schema model). Set to your latest deployed migration when adopting safe_sql.
# start_after: "20260101000000"

# Files whose migration tool cannot be detected are assumed to run in a
# transaction. Set to false for plain schema dumps.
plain_in_transaction: true

rules:
  disable: []
  # Opt-in rules: require-concurrent-index-drop, require-lock-timeout
  enable: []
  # severity: { ban-dml-in-migration: error }

verify:
  # database_url: ${SAFE_SQL_DATABASE_URL}   # else a throwaway container is started
  base_ref: main

ai:
  enabled: false
  trigger: "@safe_sql_ai"
  mode: commit            # commit | suggest | comment
  model: claude-opus-5     # ANTHROPIC_API_KEY (and optional ANTHROPIC_BASE_URL) come from the environment
  max_iterations: 3
`)
	return b.String()
}

// ConfigPathOrCwd returns the loaded config path or the working directory.
func (p *Project) ConfigPathOrCwd() string {
	if p.ConfigPath != "" {
		return p.ConfigPath
	}
	wd, _ := os.Getwd()
	return filepath.Join(wd, FileNames[0])
}
