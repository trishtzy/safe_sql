// Package rules defines the Rule type, the registry, and every built-in rule.
//
// Each rule lives in its own file next to a test with "bad" and "good" cases.
// Rules only see the sqlparse model and the Context; they never parse SQL.
package rules

import (
	"sort"

	"github.com/trishtzy/safe_sql/internal/catalog"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

// Severity of a finding.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Rule is one safety check.
type Rule struct {
	// ID is the kebab-case name used in config and disable annotations.
	ID string
	// Engines the rule applies to.
	Engines []sqlparse.Engine
	// Severity is the default severity; config may override it.
	Severity Severity
	// Summary is a one-line description for `safe_sql rules`.
	Summary string
	// Guidance explains why the operation is dangerous and shows the safe
	// pattern. Shown in full in the human report.
	Guidance string
	// OptIn rules are disabled unless enabled in config.
	OptIn bool
	// Check inspects one statement and returns findings.
	Check func(ctx *Context, st *sqlparse.Statement) []Finding
}

// AppliesTo reports whether the rule is registered for the engine.
func (r *Rule) AppliesTo(e sqlparse.Engine) bool {
	for _, x := range r.Engines {
		if x == e {
			return true
		}
	}
	return false
}

// Finding is one problem found by a rule in one statement.
type Finding struct {
	RuleID   string
	Severity Severity
	// Message is the specific one-line explanation for this statement.
	Message string
	// Guidance is the rule's guidance, possibly specialised for this statement.
	Guidance string
	// Line is relative to the source the statement was parsed from; the lint
	// runner rebases it to the file.
	Line      int
	Statement *sqlparse.Statement
}

// Context is everything a rule may consult beyond the statement itself.
type Context struct {
	Engine        sqlparse.Engine
	TargetVersion Version
	// InTransaction is true when the migration tool wraps this file in a
	// transaction.
	InTransaction bool
	Tool          string
	Filename      string
	// Catalog reflects every statement before the current one, across all
	// earlier files and this file.
	Catalog *catalog.Catalog
	// Statements are all statements of the current section, Index the
	// position of the one being checked. Rules that need file-wide context
	// (DML next to DDL, DROP then CREATE) read these.
	Statements []sqlparse.Statement
	Index      int
}

// TableCreatedHere reports whether t was created earlier in the same file,
// meaning no running application can depend on it yet and locks on it
// block nobody.
func (c *Context) TableCreatedHere(t sqlparse.TableName) bool {
	if c.Catalog == nil {
		return false
	}
	tbl := c.Catalog.Table(t)
	return tbl != nil && tbl.CreatedIn == c.Filename
}

// HasDDL reports whether any statement in the section is DDL.
func (c *Context) HasDDL() bool {
	for i := range c.Statements {
		if c.Statements[i].Kind.IsDDL() {
			return true
		}
	}
	return false
}

func (c *Context) finding(r *Rule, st *sqlparse.Statement, msg string) Finding {
	return Finding{RuleID: r.ID, Severity: r.Severity, Message: msg, Guidance: r.Guidance, Line: st.Line, Statement: st}
}

var registry = map[string]*Rule{}

// Register adds a rule. It panics on duplicate IDs.
func Register(r *Rule) {
	if _, dup := registry[r.ID]; dup {
		panic("duplicate rule " + r.ID)
	}
	registry[r.ID] = r
}

// Get returns a rule by ID.
func Get(id string) (*Rule, bool) {
	r, ok := registry[id]
	return r, ok
}

// All returns every rule sorted by ID.
func All() []*Rule {
	out := make([]*Rule, 0, len(registry))
	for _, r := range registry {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ForEngine returns every rule registered for e, sorted by ID.
func ForEngine(e sqlparse.Engine) []*Rule {
	var out []*Rule
	for _, r := range All() {
		if r.AppliesTo(e) {
			out = append(out, r)
		}
	}
	return out
}

var (
	pgOnly = []sqlparse.Engine{sqlparse.EnginePostgres}
	common = []sqlparse.Engine{sqlparse.EnginePostgres, sqlparse.EngineSQLite}
)
