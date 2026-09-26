// Package catalog is a lightweight schema model built by replaying migration
// statements in order. Rules use it to answer questions like "was this table
// created in the current file?" or "is there a validated NOT NULL check on
// this column?".
package catalog

import (
	"strings"

	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

// Catalog holds tables, views and triggers keyed by lower-cased name.
type Catalog struct {
	tables   map[string]*Table
	views    map[string]*View
	triggers map[string]*Trigger
}

// Table is one table's known state.
type Table struct {
	Name sqlparse.TableName
	// CreatedIn is the file that created the table ("" when unknown).
	CreatedIn   string
	Columns     []Column
	Constraints []Constraint
	Indexes     []Index
}

// Column is a table column.
type Column struct {
	Name    string
	Def     sqlparse.ColumnDef
	AddedIn string
}

// Constraint is a table constraint with its validation state.
type Constraint struct {
	sqlparse.Constraint
	// Validated is false while a NOT VALID constraint awaits VALIDATE CONSTRAINT.
	Validated bool
	AddedIn   string
}

// Index is an index on a table.
type Index struct {
	Name    string
	Columns []string
	Unique  bool
	AddedIn string
}

// View is a view and the statement that defined it.
type View struct {
	Name       string
	Definition string
	CreatedIn  string
}

// Trigger is a trigger on a table.
type Trigger struct {
	Name       string
	Table      sqlparse.TableName
	Definition string
	CreatedIn  string
}

// New returns an empty catalog.
func New() *Catalog {
	return &Catalog{tables: map[string]*Table{}, views: map[string]*View{}, triggers: map[string]*Trigger{}}
}

// Table looks up a table; nil when unknown.
func (c *Catalog) Table(t sqlparse.TableName) *Table {
	return c.tables[t.Key()]
}

// Tables returns every known table.
func (c *Catalog) Tables() []*Table {
	out := make([]*Table, 0, len(c.tables))
	for _, t := range c.tables {
		out = append(out, t)
	}
	return out
}

// Views returns every known view.
func (c *Catalog) Views() []*View {
	out := make([]*View, 0, len(c.views))
	for _, v := range c.views {
		out = append(out, v)
	}
	return out
}

// TriggersOn returns triggers defined on t.
func (c *Catalog) TriggersOn(t sqlparse.TableName) []*Trigger {
	var out []*Trigger
	for _, tr := range c.triggers {
		if tr.Table.Key() == t.Key() {
			out = append(out, tr)
		}
	}
	return out
}

// Column looks up a column on a table; nil when unknown.
func (t *Table) Column(name string) *Column {
	for i := range t.Columns {
		if strings.EqualFold(t.Columns[i].Name, name) {
			return &t.Columns[i]
		}
	}
	return nil
}

// Constraint looks up a named constraint; nil when unknown.
func (t *Table) Constraint(name string) *Constraint {
	for i := range t.Constraints {
		if strings.EqualFold(t.Constraints[i].Name, name) {
			return &t.Constraints[i]
		}
	}
	return nil
}

// HasValidatedNotNullCheck reports whether a validated CHECK (col IS NOT NULL)
// constraint exists on the table, which makes SET NOT NULL skip its scan on
// Postgres 12+.
func (t *Table) HasValidatedNotNullCheck(col string) bool {
	want := normalizeExpr(col + " is not null")
	for _, c := range t.Constraints {
		if c.Kind == sqlparse.ConstraintCheck && c.Validated && c.Expr != nil && normalizeExpr(c.Expr.Raw) == want {
			return true
		}
	}
	return false
}

// IndexedColumns reports whether col participates in any index or unique/PK constraint.
func (t *Table) IndexedColumns(col string) bool {
	for _, ix := range t.Indexes {
		for _, c := range ix.Columns {
			if strings.EqualFold(c, col) {
				return true
			}
		}
	}
	for _, c := range t.Constraints {
		if c.Kind == sqlparse.ConstraintPrimaryKey || c.Kind == sqlparse.ConstraintUnique {
			for _, k := range c.Columns {
				if strings.EqualFold(k, col) {
					return true
				}
			}
		}
	}
	if c := t.Column(col); c != nil && (c.Def.PrimaryKey || c.Def.Unique) {
		return true
	}
	return false
}

// ReferencedBy lists tables whose foreign keys point at column col of t.
func (c *Catalog) ReferencedBy(t sqlparse.TableName, col string) []string {
	var out []string
	for _, other := range c.tables {
		for _, cn := range other.Constraints {
			if cn.Kind != sqlparse.ConstraintForeignKey || cn.References == nil || cn.References.Key() != t.Key() {
				continue
			}
			for _, rc := range cn.RefColumns {
				if strings.EqualFold(rc, col) {
					out = append(out, other.Name.String())
				}
			}
		}
	}
	return out
}

// ViewsMentioning lists views whose definition contains col as a word.
func (c *Catalog) ViewsMentioning(col string) []string {
	var out []string
	for _, v := range c.views {
		if mentionsWord(v.Definition, col) {
			out = append(out, v.Name)
		}
	}
	return out
}

// TriggersMentioning lists triggers on t whose body mentions col as a word.
func (c *Catalog) TriggersMentioning(t sqlparse.TableName, col string) []string {
	var out []string
	for _, tr := range c.triggers {
		if tr.Table.Key() == t.Key() && mentionsWord(tr.Definition, col) {
			out = append(out, tr.Name)
		}
	}
	return out
}

// GeneratedColumnsUsing lists generated columns of t whose expression
// mentions col.
func (t *Table) GeneratedColumnsUsing(col string) []string {
	var out []string
	for _, c := range t.Columns {
		if c.Def.GeneratedExpr != nil && mentionsWord(c.Def.GeneratedExpr.Raw, col) {
			out = append(out, c.Name)
		}
	}
	return out
}

func mentionsWord(text, word string) bool {
	if word == "" {
		return false
	}
	lower, w := strings.ToLower(text), strings.ToLower(word)
	for i := 0; i+len(w) <= len(lower); i++ {
		if lower[i:i+len(w)] != w {
			continue
		}
		before := i == 0 || !isIdent(lower[i-1])
		after := i+len(w) == len(lower) || !isIdent(lower[i+len(w)])
		if before && after {
			return true
		}
	}
	return false
}

func isIdent(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func normalizeExpr(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("(", "", ")", "", " ", "", "\t", "", "\n", "", "\"", "").Replace(s)
	return s
}

// Apply updates the catalog with one statement from file.
func (c *Catalog) Apply(st *sqlparse.Statement, file string) {
	switch st.Kind {
	case sqlparse.KindCreateTable:
		ct := st.CreateTable
		t := &Table{Name: ct.Table, CreatedIn: file}
		for _, col := range ct.Columns {
			t.Columns = append(t.Columns, Column{Name: col.Name, Def: col, AddedIn: file})
			for _, cn := range col.Constraints {
				t.Constraints = append(t.Constraints, Constraint{Constraint: cn, Validated: true, AddedIn: file})
			}
		}
		for _, cn := range ct.Constraints {
			t.Constraints = append(t.Constraints, Constraint{Constraint: cn, Validated: !cn.NotValid, AddedIn: file})
		}
		c.tables[ct.Table.Key()] = t

	case sqlparse.KindAlterTable:
		t := c.ensure(st.AlterTable.Table)
		for _, a := range st.AlterTable.Actions {
			switch a.Kind {
			case sqlparse.ActionAddColumn:
				if a.ColumnDef != nil {
					t.Columns = append(t.Columns, Column{Name: a.ColumnDef.Name, Def: *a.ColumnDef, AddedIn: file})
					for _, cn := range a.ColumnDef.Constraints {
						t.Constraints = append(t.Constraints, Constraint{Constraint: cn, Validated: true, AddedIn: file})
					}
				}
			case sqlparse.ActionDropColumn:
				for i := range t.Columns {
					if strings.EqualFold(t.Columns[i].Name, a.Column) {
						t.Columns = append(t.Columns[:i], t.Columns[i+1:]...)
						break
					}
				}
			case sqlparse.ActionAlterColumnType:
				if col := t.Column(a.Column); col != nil && a.NewType != nil {
					col.Def.Type, col.Def.TypeMods, col.Def.IsArray = a.NewType.Type, a.NewType.TypeMods, a.NewType.IsArray
				}
			case sqlparse.ActionSetNotNull:
				if col := t.Column(a.Column); col != nil {
					col.Def.NotNull = true
				}
			case sqlparse.ActionDropNotNull:
				if col := t.Column(a.Column); col != nil {
					col.Def.NotNull = false
				}
			case sqlparse.ActionSetDefault:
				if col := t.Column(a.Column); col != nil {
					col.Def.Default = a.Default
				}
			case sqlparse.ActionDropDefault:
				if col := t.Column(a.Column); col != nil {
					col.Def.Default = nil
				}
			case sqlparse.ActionAddConstraint:
				if a.Constraint != nil {
					t.Constraints = append(t.Constraints, Constraint{Constraint: *a.Constraint, Validated: !a.Constraint.NotValid, AddedIn: file})
				}
			case sqlparse.ActionDropConstraint:
				for i := range t.Constraints {
					if strings.EqualFold(t.Constraints[i].Name, a.ConstraintName) {
						t.Constraints = append(t.Constraints[:i], t.Constraints[i+1:]...)
						break
					}
				}
			case sqlparse.ActionValidateConstraint:
				if cn := t.Constraint(a.ConstraintName); cn != nil {
					cn.Validated = true
				}
			}
		}

	case sqlparse.KindCreateIndex:
		ci := st.CreateIndex
		t := c.ensure(ci.Table)
		t.Indexes = append(t.Indexes, Index{Name: ci.Name, Columns: ci.Columns, Unique: ci.Unique, AddedIn: file})

	case sqlparse.KindDropIndex:
		for _, name := range st.DropIndex.Names {
			for _, t := range c.tables {
				for i := range t.Indexes {
					if strings.EqualFold(t.Indexes[i].Name, name) {
						t.Indexes = append(t.Indexes[:i], t.Indexes[i+1:]...)
						break
					}
				}
			}
		}

	case sqlparse.KindDropTable:
		for _, tn := range st.DropTable.Tables {
			delete(c.tables, tn.Key())
		}

	case sqlparse.KindRenameTable:
		rt := st.RenameTable
		if t, ok := c.tables[rt.Table.Key()]; ok {
			delete(c.tables, rt.Table.Key())
			t.Name = sqlparse.TableName{Schema: rt.Table.Schema, Name: rt.NewName}
			c.tables[t.Name.Key()] = t
		}

	case sqlparse.KindRenameColumn:
		if t := c.Table(st.RenameColumn.Table); t != nil {
			if col := t.Column(st.RenameColumn.Old); col != nil {
				col.Name, col.Def.Name = st.RenameColumn.New, st.RenameColumn.New
			}
		}

	case sqlparse.KindCreateView:
		c.views[strings.ToLower(st.View.Name)] = &View{Name: st.View.Name, Definition: st.View.Definition, CreatedIn: file}
	case sqlparse.KindDropView:
		delete(c.views, strings.ToLower(st.View.Name))
	case sqlparse.KindCreateTrigger:
		c.triggers[strings.ToLower(st.Trigger.Name)] = &Trigger{Name: st.Trigger.Name, Table: st.Trigger.Table, Definition: st.Trigger.Definition, CreatedIn: file}
	case sqlparse.KindDropTrigger:
		delete(c.triggers, strings.ToLower(st.Trigger.Name))
	}
}

// ensure returns the table, creating an unknown-origin placeholder when the
// migrations we saw never created it (e.g. start of history not included).
func (c *Catalog) ensure(t sqlparse.TableName) *Table {
	if tbl := c.tables[t.Key()]; tbl != nil {
		return tbl
	}
	tbl := &Table{Name: t}
	c.tables[t.Key()] = tbl
	return tbl
}
