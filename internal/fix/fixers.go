package fix

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/trishtzy/safe_sql/internal/lint"
	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

var (
	createIndexRe   = regexp.MustCompile(`(?i)^\s*CREATE\s+(UNIQUE\s+)?INDEX\b`)
	dropIndexRe     = regexp.MustCompile(`(?i)^\s*DROP\s+INDEX\b`)
	addConstraintRe = regexp.MustCompile(`(?i)\bADD\s+(CONSTRAINT\s+("[^"]+"|\S+)\s+)?(FOREIGN\s+KEY|CHECK)\b`)
	jsonTypeRe      = regexp.MustCompile(`(?i)(\w+|"[^"]+")(\s+)json\b`)
)

func init() {
	Register("require-concurrent-index-creation", fixConcurrently(createIndexRe, "CREATE %sINDEX CONCURRENTLY", func(f lint.Finding) string {
		return "DROP INDEX CONCURRENTLY IF EXISTS " + f.Statement.CreateIndex.Name
	}))
	Register("require-concurrent-index-drop", fixConcurrently(dropIndexRe, "DROP INDEX CONCURRENTLY", nil))
	Register("add-foreign-key-not-valid", fixNotValid("fkey"))
	Register("add-check-constraint-not-valid", fixNotValid("check"))
	Register("add-unique-constraint-via-index", fixUniqueViaIndex)
	Register("prefer-jsonb", fixJSONB)
}

// fixConcurrently adds CONCURRENTLY to a CREATE/DROP INDEX. When the file
// runs in a transaction, it either adds the tool's no-transaction directive
// (sole statement) or moves the statement into a new no-transaction file.
func fixConcurrently(re *regexp.Regexp, format string, downFor func(lint.Finding) string) Fixer {
	return func(c *Context, f lint.Finding) (Change, bool) {
		raw := f.Statement.Raw
		m := re.FindStringSubmatchIndex(raw)
		if m == nil {
			return Change{}, false
		}
		unique := ""
		if len(m) > 3 && m[2] >= 0 {
			unique = "UNIQUE "
		}
		head := fmt.Sprintf(format, unique)
		if !strings.Contains(format, "%s") {
			head = format
		}
		fixed := head + raw[m[1]:]
		ch := Change{Finding: f}
		start := c.StmtStart(f)
		if !c.InTransaction {
			ch.Edits = []Edit{{Path: f.File, Start: start, End: start + len(raw), Replacement: fixed}}
			return ch, true
		}
		if c.Statements == 1 {
			if e, ok := NoTxDirectiveEdit(c.File); ok {
				ch.Edits = []Edit{e, {Path: f.File, Start: start, End: start + len(raw), Replacement: fixed}}
				ch.Note = "added the no-transaction directive to the file"
				return ch, true
			}
		}
		var down []string
		if downFor != nil {
			down = []string{downFor(f)}
		}
		slug := "concurrently"
		if f.Statement.Kind == sqlparse.KindCreateIndex {
			slug = "create_" + f.Statement.CreateIndex.Name + "_concurrently"
		}
		files, ok := NewMigration(c.Versions, slug, []string{fixed}, down, true)
		if !ok {
			return Change{}, false
		}
		ch.Edits = []Edit{{Path: f.File, Start: start, End: c.StmtEnd(f), Replacement: ""}}
		ch.NewFiles = files
		ch.Note = "moved the statement to " + files[0].Path + ", which runs outside a transaction"
		return ch, true
	}
}

// fixNotValid appends NOT VALID to a single-action ADD CONSTRAINT and
// creates a follow-up migration that validates it.
func fixNotValid(kind string) Fixer {
	return func(c *Context, f lint.Finding) (Change, bool) {
		st := f.Statement
		if st.Kind != sqlparse.KindAlterTable || len(st.AlterTable.Actions) != 1 {
			return Change{}, false
		}
		a := st.AlterTable.Actions[0]
		if a.Kind != sqlparse.ActionAddConstraint || a.Constraint == nil {
			return Change{}, false // inline REFERENCES on ADD COLUMN: needs restructuring
		}
		m := addConstraintRe.FindStringSubmatchIndex(st.Raw)
		if m == nil {
			return Change{}, false
		}
		raw := st.Raw
		name := a.Constraint.Name
		if name == "" {
			cols := strings.Join(a.Constraint.Columns, "_")
			if cols == "" {
				cols = kind
			}
			name = fmt.Sprintf("%s_%s_%s", st.AlterTable.Table.Name, cols, kind)
			raw = raw[:m[0]] + "ADD CONSTRAINT " + name + " " + raw[m[6]:]
		}
		fixed := strings.TrimRight(raw, " \n\t") + " NOT VALID"
		start := c.StmtStart(f)
		ch := Change{Finding: f, Edits: []Edit{{Path: f.File, Start: start, End: start + len(st.Raw), Replacement: fixed}}}
		validate := fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", st.AlterTable.Table, name)
		if files, ok := NewMigration(c.Versions, "validate_"+name, []string{validate}, []string{"SELECT 1 -- nothing to undo"}, false); ok {
			ch.NewFiles = files
			ch.Note = "constraint validation moved to " + files[0].Path
		} else {
			ch.Note = "run in a later migration: " + validate + ";"
		}
		return ch, true
	}
}

// fixUniqueViaIndex turns ADD CONSTRAINT ... UNIQUE (cols) into a
// concurrently built unique index plus ADD CONSTRAINT ... USING INDEX.
func fixUniqueViaIndex(c *Context, f lint.Finding) (Change, bool) {
	st := f.Statement
	if st.Kind != sqlparse.KindAlterTable || len(st.AlterTable.Actions) != 1 {
		return Change{}, false
	}
	a := st.AlterTable.Actions[0]
	if a.Kind != sqlparse.ActionAddConstraint || a.Constraint == nil || a.Constraint.Kind != sqlparse.ConstraintUnique || len(a.Constraint.Columns) == 0 {
		return Change{}, false
	}
	table := st.AlterTable.Table
	name := a.Constraint.Name
	if name == "" {
		name = fmt.Sprintf("%s_%s_key", table.Name, strings.Join(a.Constraint.Columns, "_"))
	}
	index := fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY %s ON %s (%s)", name, table, strings.Join(a.Constraint.Columns, ", "))
	attach := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s UNIQUE USING INDEX %s", table, name, name)
	start := c.StmtStart(f)
	ch := Change{Finding: f}
	if !c.InTransaction {
		ch.Edits = []Edit{{Path: f.File, Start: start, End: start + len(st.Raw), Replacement: index + ";\n" + attach}}
		return ch, true
	}
	idx, ok := NewMigration(c.Versions, "create_"+name+"_concurrently", []string{index}, []string{"DROP INDEX CONCURRENTLY IF EXISTS " + name}, true)
	if !ok {
		return Change{}, false
	}
	att, _ := NewMigration(c.Versions, "attach_"+name, []string{attach}, []string{fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", table, name)}, false)
	ch.Edits = []Edit{{Path: f.File, Start: start, End: c.StmtEnd(f), Replacement: ""}}
	ch.NewFiles = append(idx, att...)
	ch.Note = "split into " + idx[0].Path + " (index, outside a transaction) and " + att[0].Path + " (constraint)"
	return ch, true
}

// fixJSONB changes json column types to jsonb.
func fixJSONB(c *Context, f lint.Finding) (Change, bool) {
	raw := f.Statement.Raw
	fixed := jsonTypeRe.ReplaceAllString(raw, "${1}${2}jsonb")
	if fixed == raw {
		return Change{}, false
	}
	start := c.StmtStart(f)
	return Change{Finding: f, Edits: []Edit{{Path: f.File, Start: start, End: start + len(raw), Replacement: fixed}}}, true
}
