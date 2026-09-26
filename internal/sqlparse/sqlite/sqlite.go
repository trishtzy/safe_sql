// Package sqlite adapts the meyer SQLite parser to the sqlparse model.
//
// meyer fails the whole input on a syntax error, so this adapter recovers:
// it parses the text before the failing statement, emits a KindInvalid
// statement for the failing one, and continues after it. That is how
// unsupported ALTER forms (ALTER COLUMN ... TYPE) become findings instead of
// aborting the lint.
package sqlite

import (
	"errors"
	"regexp"
	"strings"

	"github.com/sqlc-dev/meyer/ast"
	"github.com/sqlc-dev/meyer/parser"

	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

func init() { sqlparse.Register(sqlparse.EngineSQLite, Parser{}) }

// Parser implements sqlparse.Parser for SQLite.
type Parser struct{}

// stmtStartRe finds the start of a statement after a semicolon: the keyword
// that begins it. Semicolons inside trigger bodies are followed by keywords
// too, but the recovery only uses this to bracket a failing statement.
var stmtStartRe = regexp.MustCompile(`(?is)^\s*(?:--[^\n]*\n\s*|/\*.*?\*/\s*)*(ALTER|CREATE|DROP|SELECT|INSERT|UPDATE|DELETE|PRAGMA|BEGIN|COMMIT|END|ROLLBACK|WITH|REPLACE|VACUUM|ANALYZE|REINDEX|ATTACH|DETACH|SAVEPOINT|RELEASE|EXPLAIN)\b`)

// Parse implements sqlparse.Parser.
func (Parser) Parse(src string) ([]sqlparse.Statement, error) {
	var out []sqlparse.Statement
	base := 0
	prevEnd := 0
	rest := src
	for strings.TrimSpace(rest) != "" {
		stmts, err := parser.ParseString(rest)
		if err == nil {
			for _, s := range stmts {
				st := convertStmt(src, base, s, &prevEnd, len(out) > 0)
				out = append(out, st)
			}
			break
		}
		var pe *parser.Error
		if !errors.As(err, &pe) {
			return nil, &sqlparse.ParseError{Message: err.Error(), Offset: -1}
		}
		off := pe.Offset
		if off < 0 || off > len(rest) {
			off = 0
		}
		// Bracket the failing statement: from the last statement boundary
		// before the error to the next boundary after it.
		start := lastBoundary(rest, off)
		end := nextBoundary(rest, off)
		if start > 0 {
			prefix, perr := parser.ParseString(rest[:start])
			if perr != nil {
				// The prefix itself does not parse: give up with a position.
				abs := base + start
				return nil, &sqlparse.ParseError{Message: pe.Message, Offset: base + off, Line: sqlparse.LineAt(src, abs)}
			}
			for _, s := range prefix {
				out = append(out, convertStmt(src, base, s, &prevEnd, len(out) > 0))
			}
		}
		text, lead := sqlparse.TrimStatement(rest[start:end])
		absStart := base + start + lead
		if text == "" {
			return nil, &sqlparse.ParseError{Message: pe.Message, Offset: base + off, Line: sqlparse.LineAt(src, base+off)}
		}
		out = append(out, sqlparse.Statement{
			Kind: sqlparse.KindInvalid, Raw: text, Error: pe.Message,
			Line: sqlparse.LineAt(src, absStart), Offset: absStart, End: absStart + len(text),
			LeadingComments: leading(src, prevEnd, absStart, len(out) > 0),
		})
		prevEnd = absStart + len(text)
		if prevEnd < len(src) && src[prevEnd] == ';' {
			prevEnd++
		}
		base += end
		rest = src[base:]
	}
	return out, nil
}

// lastBoundary returns the offset in s of the statement containing off:
// the position after the last ';' before off that is followed by a
// statement keyword (0 if none).
func lastBoundary(s string, off int) int {
	i := off
	for i > 0 {
		j := strings.LastIndexByte(s[:i], ';')
		if j < 0 {
			return 0
		}
		if stmtStartRe.MatchString(s[j+1:]) {
			return j + 1
		}
		i = j
	}
	return 0
}

// nextBoundary returns the offset in s just past the ';' that ends the
// statement containing off (len(s) if none).
func nextBoundary(s string, off int) int {
	i := off
	for i < len(s) {
		j := strings.IndexByte(s[i:], ';')
		if j < 0 {
			return len(s)
		}
		k := i + j + 1
		if strings.TrimSpace(s[k:]) == "" || stmtStartRe.MatchString(s[k:]) {
			return k
		}
		i = k
	}
	return len(s)
}

func leading(src string, prevEnd, start int, hasPrev bool) []string {
	cstart := prevEnd
	if hasPrev {
		if nl := strings.IndexByte(src[prevEnd:start], '\n'); nl >= 0 {
			cstart = prevEnd + nl + 1
		} else {
			cstart = start
		}
	}
	return sqlparse.CommentsBetween(src, cstart, start)
}

func convertStmt(src string, base int, s ast.Stmt, prevEnd *int, hasPrev bool) sqlparse.Statement {
	start, stop := base+s.Pos(), base+s.End()
	if stop > len(src) {
		stop = len(src)
	}
	text, lead := sqlparse.TrimStatement(src[start:stop])
	start += lead
	st := sqlparse.Statement{
		Raw: text, Line: sqlparse.LineAt(src, start), Offset: start, End: start + len(text),
		LeadingComments: leading(src, *prevEnd, start, hasPrev),
	}
	convert(&st, s)
	*prevEnd = start + len(text)
	if *prevEnd < len(src) && src[*prevEnd] == ';' {
		*prevEnd++
	}
	return st
}

func convert(st *sqlparse.Statement, s ast.Stmt) {
	switch x := s.(type) {
	case *ast.CreateTableStmt:
		ct := &sqlparse.CreateTable{Table: qname(x.Name), IfNotExists: x.IfNotExists, Temporary: x.Temp}
		for _, c := range x.Columns {
			ct.Columns = append(ct.Columns, columnDef(c))
		}
		for _, c := range x.Constraints {
			ct.Constraints = append(ct.Constraints, tableConstraint(c))
		}
		st.Kind, st.CreateTable = sqlparse.KindCreateTable, ct

	case *ast.AlterTableStmt:
		table := qname(x.Table)
		switch x.Action {
		case ast.AlterRenameTable:
			st.Kind, st.RenameTable = sqlparse.KindRenameTable, &sqlparse.RenameTable{Table: table, NewName: ident(x.NewName)}
		case ast.AlterRenameColumn:
			st.Kind, st.RenameColumn = sqlparse.KindRenameColumn, &sqlparse.RenameColumn{Table: table, Old: ident(x.Column), New: ident(x.NewName)}
		default:
			at := &sqlparse.AlterTable{Table: table}
			a := sqlparse.AlterAction{Column: ident(x.Column)}
			switch x.Action {
			case ast.AlterAddColumn:
				a.Kind = sqlparse.ActionAddColumn
				if x.ColumnDef != nil {
					cd := columnDef(x.ColumnDef)
					a.ColumnDef, a.Column = &cd, cd.Name
				}
				a.Raw = "ADD COLUMN " + a.Column
			case ast.AlterDropColumn:
				a.Kind, a.Raw = sqlparse.ActionDropColumn, "DROP COLUMN "+a.Column
			default:
				// sqlc's schema-only extensions: SQLite itself cannot run these.
				a.Kind, a.Raw = sqlparse.ActionUnsupported, x.Action.String()
			}
			at.Actions = append(at.Actions, a)
			st.Kind, st.AlterTable = sqlparse.KindAlterTable, at
		}

	case *ast.CreateIndexStmt:
		ci := &sqlparse.CreateIndex{Name: qname(x.Name).Name, Table: sqlparse.TableName{Name: ident(x.Table)}, Unique: x.Unique, IfNotExists: x.IfNotExists, Partial: x.Where != nil}
		for _, c := range x.Columns {
			ci.Columns = append(ci.Columns, exprText(c.Expr))
		}
		st.Kind, st.CreateIndex = sqlparse.KindCreateIndex, ci
	case *ast.DropIndexStmt:
		st.Kind, st.DropIndex = sqlparse.KindDropIndex, &sqlparse.DropIndex{Names: []string{qname(x.Name).Name}, IfExists: x.IfExists}
	case *ast.DropTableStmt:
		st.Kind, st.DropTable = sqlparse.KindDropTable, &sqlparse.DropTable{Tables: []sqlparse.TableName{qname(x.Name)}, IfExists: x.IfExists}
	case *ast.CreateViewStmt:
		def := ""
		if x.Select != nil {
			def = ast.String(x.Select)
		}
		st.Kind, st.View = sqlparse.KindCreateView, &sqlparse.ObjectRef{Name: qname(x.Name).Name, Definition: def}
	case *ast.DropViewStmt:
		st.Kind, st.View = sqlparse.KindDropView, &sqlparse.ObjectRef{Name: qname(x.Name).Name}
	case *ast.CreateTriggerStmt:
		st.Kind, st.Trigger = sqlparse.KindCreateTrigger, &sqlparse.ObjectRef{Name: qname(x.Name).Name, Table: qname(x.Table), Definition: st.Raw}
	case *ast.DropTriggerStmt:
		st.Kind, st.Trigger = sqlparse.KindDropTrigger, &sqlparse.ObjectRef{Name: qname(x.Name).Name}
	case *ast.PragmaStmt:
		st.Kind, st.Pragma = sqlparse.KindPragma, &sqlparse.Pragma{Name: strings.ToLower(qname(x.Name).Name), Value: x.Value}
	case *ast.BeginStmt:
		st.Kind, st.Transaction = sqlparse.KindTransaction, &sqlparse.Transaction{Begin: true}
	case *ast.CommitStmt:
		st.Kind, st.Transaction = sqlparse.KindTransaction, &sqlparse.Transaction{Commit: true}
	case *ast.InsertStmt:
		d := &sqlparse.DML{Op: "insert", Table: qname(x.Table)}
		if x.Select != nil && !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(ast.String(x.Select))), "VALUES") {
			d.FromSelect = true
		}
		st.Kind, st.DML = sqlparse.KindDML, d
	case *ast.UpdateStmt:
		st.Kind, st.DML = sqlparse.KindDML, &sqlparse.DML{Op: "update", Table: qname(x.Table)}
	case *ast.DeleteStmt:
		st.Kind, st.DML = sqlparse.KindDML, &sqlparse.DML{Op: "delete", Table: qname(x.Table)}
	case *ast.SelectStmt:
		st.Kind, st.DML = sqlparse.KindDML, &sqlparse.DML{Op: "select"}
	}
}

func columnDef(c *ast.ColumnDef) sqlparse.ColumnDef {
	col := sqlparse.ColumnDef{Name: c.Name.Name}
	if c.Type != nil {
		col.Type = strings.ToLower(strings.Join(strings.Fields(c.Type.Name), " "))
		for _, a := range c.Type.Args {
			var n int
			if _, err := parseInt(a, &n); err == nil {
				col.TypeMods = append(col.TypeMods, n)
			}
		}
	}
	for _, cn := range c.Constraints {
		switch cn.Kind {
		case ast.ColumnNotNull:
			col.NotNull = true
		case ast.ColumnPrimaryKey:
			col.PrimaryKey = true
			if cn.AutoIncrement {
				col.Serial = true
			}
		case ast.ColumnUnique:
			col.Unique = true
		case ast.ColumnDefault:
			col.Default = expr(cn.Expr)
		case ast.ColumnCheck:
			col.Constraints = append(col.Constraints, sqlparse.Constraint{Kind: sqlparse.ConstraintCheck, Name: ident(cn.Name), Expr: expr(cn.Expr)})
		case ast.ColumnReferences:
			if cn.References != nil {
				t := sqlparse.TableName{Name: ident(cn.References.Table)}
				col.References = &t
				col.Constraints = append(col.Constraints, sqlparse.Constraint{Kind: sqlparse.ConstraintForeignKey, Name: ident(cn.Name), Columns: []string{col.Name}, References: &t, RefColumns: indexedNames(cn.References.Columns)})
			}
		case ast.ColumnGenerated:
			col.Generated = "virtual"
			if cn.GeneratedKind != nil && strings.EqualFold(cn.GeneratedKind.Name, "STORED") {
				col.Generated = "stored"
			}
			col.GeneratedExpr = expr(cn.Expr)
		}
	}
	return col
}

func tableConstraint(c *ast.TableConstraint) sqlparse.Constraint {
	out := sqlparse.Constraint{Name: ident(c.Name)}
	switch c.Kind {
	case ast.TablePrimaryKey:
		out.Kind = sqlparse.ConstraintPrimaryKey
	case ast.TableUnique:
		out.Kind = sqlparse.ConstraintUnique
	case ast.TableCheck:
		out.Kind, out.Expr = sqlparse.ConstraintCheck, expr(c.Expr)
	case ast.TableForeignKey:
		out.Kind = sqlparse.ConstraintForeignKey
		out.Columns = indexedNames(c.FKColumns)
		if c.References != nil {
			t := sqlparse.TableName{Name: ident(c.References.Table)}
			out.References = &t
			out.RefColumns = indexedNames(c.References.Columns)
		}
	default:
		out.Kind = sqlparse.ConstraintOther
	}
	for _, t := range c.Columns {
		out.Columns = append(out.Columns, exprText(t.Expr))
	}
	return out
}

func indexedNames(xs []*ast.IndexedColumn) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, ident(x.Name))
	}
	return out
}

func qname(q *ast.QualifiedName) sqlparse.TableName {
	if q == nil {
		return sqlparse.TableName{}
	}
	return sqlparse.TableName{Schema: ident(q.Schema), Name: ident(q.Name)}
}

func ident(i *ast.Ident) string {
	if i == nil {
		return ""
	}
	return i.Name
}

func exprText(e ast.Expr) string {
	if e == nil {
		return ""
	}
	return ast.String(e)
}

// expr summarises an expression. CURRENT_TIMESTAMP and friends are
// literals in SQLite's grammar but behave like functions for our purposes.
func expr(e ast.Expr) *sqlparse.Expr {
	if e == nil {
		return nil
	}
	out := &sqlparse.Expr{Raw: exprText(e), IsConstant: isConstant(e)}
	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		if n == nil {
			return
		}
		switch v := n.(type) {
		case *ast.FuncCall:
			out.Funcs = append(out.Funcs, strings.ToLower(ident(v.Name)))
		case *ast.Literal:
			switch v.Kind {
			case ast.LitCurrentDate:
				out.Funcs = append(out.Funcs, "current_date")
			case ast.LitCurrentTime:
				out.Funcs = append(out.Funcs, "current_time")
			case ast.LitCurrentTimestamp:
				out.Funcs = append(out.Funcs, "current_timestamp")
			}
		}
		for _, c := range n.Children() {
			walk(c)
		}
	}
	walk(e)
	return out
}

func isConstant(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Literal:
		return v.Kind != ast.LitCurrentDate && v.Kind != ast.LitCurrentTime && v.Kind != ast.LitCurrentTimestamp
	case *ast.ParenExpr:
		return isConstant(v.X)
	case *ast.UnaryExpr:
		return isConstant(v.X)
	case *ast.CastExpr:
		return isConstant(v.X)
	}
	return false
}

func parseInt(s string, n *int) (int, error) {
	var v int
	neg := false
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	if s == "" {
		return 0, errors.New("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number")
		}
		v = v*10 + int(r-'0')
	}
	if neg {
		v = -v
	}
	*n = v
	return v, nil
}
