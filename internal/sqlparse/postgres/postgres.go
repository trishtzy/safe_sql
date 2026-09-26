// Package postgres adapts the oliphant (pg_query-compatible) parser to the
// engine-neutral sqlparse model.
package postgres

import (
	"errors"
	"strings"

	pg "github.com/sqlc-dev/oliphant"
	"github.com/sqlc-dev/oliphant/ast"
	"github.com/sqlc-dev/oliphant/parser"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trishtzy/safe_sql/internal/sqlparse"
)

func init() { sqlparse.Register(sqlparse.EnginePostgres, Parser{}) }

// Parser implements sqlparse.Parser for PostgreSQL.
type Parser struct{}

// Parse implements sqlparse.Parser.
func (Parser) Parse(src string) ([]sqlparse.Statement, error) {
	res, err := pg.Parse(src)
	if err != nil {
		var pe *parser.Error
		if errors.As(err, &pe) {
			off := pe.Cursorpos - 1
			if off < 0 {
				off = 0
			}
			return nil, &sqlparse.ParseError{Message: pe.Message, Offset: off, Line: sqlparse.LineAt(src, off)}
		}
		return nil, &sqlparse.ParseError{Message: err.Error(), Offset: -1}
	}
	out := make([]sqlparse.Statement, 0, len(res.Stmts))
	prevEnd := 0
	for i, raw := range res.Stmts {
		start := int(raw.StmtLocation)
		end := start + int(raw.StmtLen)
		if raw.StmtLen == 0 {
			end = len(src)
		}
		text, lead := sqlparse.TrimStatement(src[start:end])
		start += lead
		end = start + len(text)

		// Comments on the same line as the previous statement belong to it,
		// not to this one.
		cstart := prevEnd
		if i > 0 {
			if nl := strings.IndexByte(src[prevEnd:start], '\n'); nl >= 0 {
				cstart = prevEnd + nl + 1
			} else {
				cstart = start
			}
		}
		st := sqlparse.Statement{
			Raw:             text,
			Line:            sqlparse.LineAt(src, start),
			Offset:          start,
			End:             end,
			LeadingComments: sqlparse.CommentsBetween(src, cstart, start),
		}
		convert(&st, raw.Stmt)
		out = append(out, st)
		// Advance past the statement's semicolon if present.
		prevEnd = end
		if prevEnd < len(src) && src[prevEnd] == ';' {
			prevEnd++
		}
	}
	return out, nil
}

func convert(st *sqlparse.Statement, n *ast.Node) {
	if n == nil {
		return
	}
	switch x := n.Node.(type) {
	case *ast.Node_CreateStmt:
		s := x.CreateStmt
		ct := &sqlparse.CreateTable{
			Table:       rangeVar(s.Relation),
			IfNotExists: s.IfNotExists,
			Temporary:   s.Relation != nil && s.Relation.Relpersistence == "t",
			Partitioned: s.Partspec != nil,
		}
		for _, elt := range s.TableElts {
			if cd := elt.GetColumnDef(); cd != nil {
				ct.Columns = append(ct.Columns, columnDef(cd))
			} else if c := elt.GetConstraint(); c != nil {
				ct.Constraints = append(ct.Constraints, constraint(c))
			}
		}
		st.Kind, st.CreateTable = sqlparse.KindCreateTable, ct

	case *ast.Node_AlterTableStmt:
		s := x.AlterTableStmt
		if s.Objtype != ast.ObjectType_OBJECT_TABLE {
			return
		}
		at := &sqlparse.AlterTable{Table: rangeVar(s.Relation), IfExists: s.MissingOk}
		for _, c := range s.Cmds {
			if cmd := c.GetAlterTableCmd(); cmd != nil {
				at.Actions = append(at.Actions, alterCmd(cmd))
			}
		}
		st.Kind, st.AlterTable = sqlparse.KindAlterTable, at

	case *ast.Node_IndexStmt:
		s := x.IndexStmt
		ci := &sqlparse.CreateIndex{
			Name: s.Idxname, Table: rangeVar(s.Relation), Unique: s.Unique,
			Concurrent: s.Concurrent, IfNotExists: s.IfNotExists, Method: s.AccessMethod,
			Partial: s.WhereClause != nil,
		}
		for _, p := range s.IndexParams {
			if ie := p.GetIndexElem(); ie != nil {
				if ie.Name != "" {
					ci.Columns = append(ci.Columns, ie.Name)
				} else {
					ci.Columns = append(ci.Columns, exprText(ie.Expr))
				}
			}
		}
		st.Kind, st.CreateIndex = sqlparse.KindCreateIndex, ci

	case *ast.Node_DropStmt:
		s := x.DropStmt
		switch s.RemoveType {
		case ast.ObjectType_OBJECT_TABLE:
			dt := &sqlparse.DropTable{IfExists: s.MissingOk}
			for _, o := range s.Objects {
				dt.Tables = append(dt.Tables, qualifiedName(o))
			}
			st.Kind, st.DropTable = sqlparse.KindDropTable, dt
		case ast.ObjectType_OBJECT_INDEX:
			di := &sqlparse.DropIndex{IfExists: s.MissingOk, Concurrent: s.Concurrent}
			for _, o := range s.Objects {
				di.Names = append(di.Names, qualifiedName(o).String())
			}
			st.Kind, st.DropIndex = sqlparse.KindDropIndex, di
		case ast.ObjectType_OBJECT_VIEW, ast.ObjectType_OBJECT_MATVIEW:
			if len(s.Objects) > 0 {
				st.Kind = sqlparse.KindDropView
				st.View = &sqlparse.ObjectRef{Name: qualifiedName(s.Objects[0]).String()}
			}
		case ast.ObjectType_OBJECT_TRIGGER:
			if len(s.Objects) > 0 {
				parts := stringList(s.Objects[0])
				ref := &sqlparse.ObjectRef{}
				if len(parts) >= 2 {
					ref.Name = parts[len(parts)-1]
					ref.Table = sqlparse.TableName{Name: parts[len(parts)-2]}
					if len(parts) >= 3 {
						ref.Table.Schema = parts[len(parts)-3]
					}
				}
				st.Kind, st.Trigger = sqlparse.KindDropTrigger, ref
			}
		}

	case *ast.Node_RenameStmt:
		s := x.RenameStmt
		switch s.RenameType {
		case ast.ObjectType_OBJECT_TABLE:
			st.Kind = sqlparse.KindRenameTable
			st.RenameTable = &sqlparse.RenameTable{Table: rangeVar(s.Relation), NewName: s.Newname}
		case ast.ObjectType_OBJECT_COLUMN:
			if s.RelationType == ast.ObjectType_OBJECT_TABLE {
				st.Kind = sqlparse.KindRenameColumn
				st.RenameColumn = &sqlparse.RenameColumn{Table: rangeVar(s.Relation), Old: s.Subname, New: s.Newname}
			}
		case ast.ObjectType_OBJECT_SCHEMA:
			st.Kind = sqlparse.KindRenameSchema
			st.RenameSchema = &sqlparse.RenameSchema{Old: s.Subname, New: s.Newname}
		}

	case *ast.Node_AlterEnumStmt:
		s := x.AlterEnumStmt
		ae := &sqlparse.AlterEnum{Type: strings.Join(stringNodes(s.TypeName), ".")}
		if s.OldVal != "" {
			ae.RenameOld, ae.RenameNew = s.OldVal, s.NewVal
		} else {
			ae.AddValue = s.NewVal
		}
		st.Kind, st.AlterEnum = sqlparse.KindAlterEnum, ae

	case *ast.Node_VariableSetStmt:
		s := x.VariableSetStmt
		set := &sqlparse.Set{Name: strings.ToLower(s.Name), Local: s.IsLocal}
		if len(s.Args) > 0 {
			set.Value = exprText(s.Args[0])
		}
		st.Kind, st.Set = sqlparse.KindSet, set

	case *ast.Node_TransactionStmt:
		s := x.TransactionStmt
		switch s.Kind {
		case ast.TransactionStmtKind_TRANS_STMT_BEGIN, ast.TransactionStmtKind_TRANS_STMT_START:
			st.Kind, st.Transaction = sqlparse.KindTransaction, &sqlparse.Transaction{Begin: true}
		case ast.TransactionStmtKind_TRANS_STMT_COMMIT:
			st.Kind, st.Transaction = sqlparse.KindTransaction, &sqlparse.Transaction{Commit: true}
		}

	case *ast.Node_InsertStmt:
		s := x.InsertStmt
		d := &sqlparse.DML{Op: "insert", Table: rangeVar(s.Relation)}
		if sel := s.SelectStmt.GetSelectStmt(); sel != nil && len(sel.ValuesLists) == 0 {
			d.FromSelect = true
		}
		st.Kind, st.DML = sqlparse.KindDML, d
	case *ast.Node_UpdateStmt:
		st.Kind, st.DML = sqlparse.KindDML, &sqlparse.DML{Op: "update", Table: rangeVar(x.UpdateStmt.Relation)}
	case *ast.Node_DeleteStmt:
		st.Kind, st.DML = sqlparse.KindDML, &sqlparse.DML{Op: "delete", Table: rangeVar(x.DeleteStmt.Relation)}
	case *ast.Node_SelectStmt:
		st.Kind, st.DML = sqlparse.KindDML, &sqlparse.DML{Op: "select"}

	case *ast.Node_ViewStmt:
		st.Kind = sqlparse.KindCreateView
		st.View = &sqlparse.ObjectRef{Name: rangeVar(x.ViewStmt.View).String(), Definition: exprText(x.ViewStmt.Query)}
	case *ast.Node_CreateTrigStmt:
		st.Kind = sqlparse.KindCreateTrigger
		st.Trigger = &sqlparse.ObjectRef{Name: x.CreateTrigStmt.Trigname, Table: rangeVar(x.CreateTrigStmt.Relation)}
	}
}

func alterCmd(c *ast.AlterTableCmd) sqlparse.AlterAction {
	a := sqlparse.AlterAction{Column: c.Name}
	switch c.Subtype {
	case ast.AlterTableType_AT_AddColumn:
		a.Kind = sqlparse.ActionAddColumn
		if cd := c.Def.GetColumnDef(); cd != nil {
			col := columnDef(cd)
			a.ColumnDef = &col
			a.Column = col.Name
		}
		a.Raw = "ADD COLUMN " + a.Column
	case ast.AlterTableType_AT_DropColumn:
		a.Kind, a.Raw = sqlparse.ActionDropColumn, "DROP COLUMN "+c.Name
	case ast.AlterTableType_AT_AlterColumnType:
		a.Kind, a.Raw = sqlparse.ActionAlterColumnType, "ALTER COLUMN "+c.Name+" TYPE"
		if cd := c.Def.GetColumnDef(); cd != nil {
			nt := columnDef(cd)
			a.NewType = &nt
			if cd.RawDefault != nil {
				a.Using = expr(cd.RawDefault)
			}
		}
	case ast.AlterTableType_AT_SetNotNull:
		a.Kind, a.Raw = sqlparse.ActionSetNotNull, "ALTER COLUMN "+c.Name+" SET NOT NULL"
	case ast.AlterTableType_AT_DropNotNull:
		a.Kind, a.Raw = sqlparse.ActionDropNotNull, "ALTER COLUMN "+c.Name+" DROP NOT NULL"
	case ast.AlterTableType_AT_ColumnDefault:
		if c.Def == nil {
			a.Kind, a.Raw = sqlparse.ActionDropDefault, "ALTER COLUMN "+c.Name+" DROP DEFAULT"
		} else {
			a.Kind, a.Raw = sqlparse.ActionSetDefault, "ALTER COLUMN "+c.Name+" SET DEFAULT"
			a.Default = expr(c.Def)
		}
	case ast.AlterTableType_AT_AddConstraint:
		a.Kind = sqlparse.ActionAddConstraint
		if cn := c.Def.GetConstraint(); cn != nil {
			con := constraint(cn)
			a.Constraint = &con
			a.ConstraintName = con.Name
			a.Raw = "ADD CONSTRAINT " + con.Kind.String()
		}
	case ast.AlterTableType_AT_DropConstraint:
		a.Kind, a.ConstraintName, a.Raw = sqlparse.ActionDropConstraint, c.Name, "DROP CONSTRAINT "+c.Name
	case ast.AlterTableType_AT_ValidateConstraint:
		a.Kind, a.ConstraintName, a.Raw = sqlparse.ActionValidateConstraint, c.Name, "VALIDATE CONSTRAINT "+c.Name
	case ast.AlterTableType_AT_AddIdentity:
		a.Kind, a.Raw = sqlparse.ActionAddIdentity, "ALTER COLUMN "+c.Name+" ADD GENERATED AS IDENTITY"
	default:
		a.Kind, a.Raw = sqlparse.ActionOther, strings.TrimPrefix(c.Subtype.String(), "AT_")
	}
	return a
}

// typeAliases maps Postgres internal type names to their SQL spellings.
var typeAliases = map[string]string{
	"int2": "smallint", "int4": "integer", "int8": "bigint",
	"float4": "real", "float8": "double precision", "bool": "boolean",
	"bpchar": "char", "timestamptz": "timestamptz", "timetz": "timetz",
}

func columnDef(cd *ast.ColumnDef) sqlparse.ColumnDef {
	col := sqlparse.ColumnDef{Name: cd.Colname}
	if tn := cd.TypeName; tn != nil {
		names := stringNodes(tn.Names)
		if len(names) > 0 {
			t := strings.ToLower(names[len(names)-1])
			if alias, ok := typeAliases[t]; ok {
				t = alias
			}
			col.Type = t
		}
		for _, m := range tn.Typmods {
			if ac := m.GetAConst(); ac != nil {
				if iv := ac.GetIval(); iv != nil {
					col.TypeMods = append(col.TypeMods, int(iv.Ival))
				}
			}
		}
		col.IsArray = len(tn.ArrayBounds) > 0
		switch col.Type {
		case "serial", "bigserial", "smallserial", "serial2", "serial4", "serial8":
			col.Serial = true
		}
	}
	for _, cn := range cd.Constraints {
		c := cn.GetConstraint()
		if c == nil {
			continue
		}
		switch c.Contype {
		case ast.ConstrType_CONSTR_NOTNULL:
			col.NotNull = true
		case ast.ConstrType_CONSTR_DEFAULT:
			col.Default = expr(c.RawExpr)
		case ast.ConstrType_CONSTR_PRIMARY:
			col.PrimaryKey, col.NotNull = true, true
		case ast.ConstrType_CONSTR_UNIQUE:
			col.Unique = true
		case ast.ConstrType_CONSTR_IDENTITY:
			col.Identity = true
		case ast.ConstrType_CONSTR_GENERATED:
			if c.GeneratedKind == "v" {
				col.Generated = "virtual"
			} else {
				col.Generated = "stored"
			}
		case ast.ConstrType_CONSTR_FOREIGN:
			t := rangeVar(c.Pktable)
			col.References = &t
			col.Constraints = append(col.Constraints, constraint(c))
		case ast.ConstrType_CONSTR_CHECK:
			col.Constraints = append(col.Constraints, constraint(c))
		}
	}
	return col
}

func constraint(c *ast.Constraint) sqlparse.Constraint {
	out := sqlparse.Constraint{Name: c.Conname, NotValid: c.SkipValidation, UsingIndex: c.Indexname}
	switch c.Contype {
	case ast.ConstrType_CONSTR_CHECK:
		out.Kind, out.Expr = sqlparse.ConstraintCheck, expr(c.RawExpr)
	case ast.ConstrType_CONSTR_PRIMARY:
		out.Kind, out.Columns = sqlparse.ConstraintPrimaryKey, stringNodes(c.Keys)
	case ast.ConstrType_CONSTR_UNIQUE:
		out.Kind, out.Columns = sqlparse.ConstraintUnique, stringNodes(c.Keys)
	case ast.ConstrType_CONSTR_FOREIGN:
		out.Kind, out.Columns, out.RefColumns = sqlparse.ConstraintForeignKey, stringNodes(c.FkAttrs), stringNodes(c.PkAttrs)
		t := rangeVar(c.Pktable)
		out.References = &t
	case ast.ConstrType_CONSTR_EXCLUSION:
		out.Kind = sqlparse.ConstraintExclusion
	case ast.ConstrType_CONSTR_NOTNULL:
		out.Kind, out.Columns = sqlparse.ConstraintNotNull, stringNodes(c.Keys)
	default:
		out.Kind = sqlparse.ConstraintOther
	}
	return out
}

func rangeVar(r *ast.RangeVar) sqlparse.TableName {
	if r == nil {
		return sqlparse.TableName{}
	}
	return sqlparse.TableName{Schema: r.Schemaname, Name: r.Relname}
}

// qualifiedName reads a List of String nodes (schema, name) or a single String.
func qualifiedName(n *ast.Node) sqlparse.TableName {
	parts := stringList(n)
	switch len(parts) {
	case 0:
		return sqlparse.TableName{}
	case 1:
		return sqlparse.TableName{Name: parts[0]}
	default:
		return sqlparse.TableName{Schema: parts[len(parts)-2], Name: parts[len(parts)-1]}
	}
}

func stringList(n *ast.Node) []string {
	if l := n.GetList(); l != nil {
		return stringNodes(l.Items)
	}
	if s := n.GetString_(); s != nil {
		return []string{s.Sval}
	}
	return nil
}

func stringNodes(nodes []*ast.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if s := n.GetString_(); s != nil {
			out = append(out, s.Sval)
		}
	}
	return out
}

// expr summarises an expression node.
func expr(n *ast.Node) *sqlparse.Expr {
	if n == nil {
		return nil
	}
	e := &sqlparse.Expr{Raw: exprText(n), IsConstant: isConstant(n)}
	walk(n, func(m protoreflect.Message) {
		switch v := m.Interface().(type) {
		case *ast.FuncCall:
			names := stringNodes(v.Funcname)
			if len(names) > 0 {
				e.Funcs = append(e.Funcs, strings.ToLower(names[len(names)-1]))
			}
		case *ast.SQLValueFunction:
			// CURRENT_TIMESTAMP, CURRENT_DATE, ... map to their function names.
			op := strings.ToLower(strings.TrimPrefix(v.Op.String(), "SVFOP_"))
			e.Funcs = append(e.Funcs, op)
		}
	})
	return e
}

func isConstant(n *ast.Node) bool {
	switch x := n.Node.(type) {
	case *ast.Node_AConst:
		return true
	case *ast.Node_TypeCast:
		return x.TypeCast.Arg != nil && isConstant(x.TypeCast.Arg)
	}
	return false
}

// exprText deparses an expression by wrapping it in SELECT.
func exprText(n *ast.Node) string {
	if n == nil {
		return ""
	}
	if sel := n.GetSelectStmt(); sel != nil {
		s, err := pg.Deparse(&ast.ParseResult{Stmts: []*ast.RawStmt{{Stmt: n}}})
		if err == nil {
			return s
		}
		return ""
	}
	sel := &ast.Node{Node: &ast.Node_SelectStmt{SelectStmt: &ast.SelectStmt{
		TargetList:  []*ast.Node{pg.MakeResTargetNodeWithVal(n, -1)},
		Op:          ast.SetOperation_SETOP_NONE,
		LimitOption: ast.LimitOption_LIMIT_OPTION_DEFAULT,
	}}}
	s, err := pg.Deparse(&ast.ParseResult{Stmts: []*ast.RawStmt{{Stmt: sel}}})
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(s, "SELECT ")
}

// walk visits every message reachable from n, depth-first.
func walk(n proto.Message, fn func(protoreflect.Message)) {
	var visit func(m protoreflect.Message)
	visit = func(m protoreflect.Message) {
		fn(m)
		m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			switch {
			case fd.IsList() && fd.Kind() == protoreflect.MessageKind:
				l := v.List()
				for i := 0; i < l.Len(); i++ {
					visit(l.Get(i).Message())
				}
			case fd.Kind() == protoreflect.MessageKind && !fd.IsMap():
				visit(v.Message())
			}
			return true
		})
	}
	visit(n.ProtoReflect())
}
