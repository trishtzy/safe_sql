// Package sqlparse defines an engine-neutral model of the SQL statements that
// matter for migration safety, plus the Parser interface each engine implements.
//
// Rules never see raw parser ASTs; they only see Statement values. Anything a
// rule cannot express with this model is a gap in the model, not something to
// work around in a rule.
package sqlparse

import "strings"

// Engine identifies a database dialect. Values match sqlc's `engine:` names.
type Engine string

const (
	EnginePostgres Engine = "postgresql"
	EngineSQLite   Engine = "sqlite"
)

// Kind classifies a Statement. Exactly one payload field on Statement is set
// for each Kind (none for KindOther).
type Kind int

const (
	KindOther Kind = iota
	KindCreateTable
	KindAlterTable
	KindCreateIndex
	KindDropIndex
	KindDropTable
	KindRenameTable
	KindRenameColumn
	KindAlterEnum
	KindRenameSchema
	KindDML
	KindSet
	KindPragma
	KindCreateView
	KindDropView
	KindCreateTrigger
	KindDropTrigger
	KindTransaction
	// KindInvalid is a statement the engine's parser rejected. Raw holds the
	// text and Error the parser's message. Engines that can recover from a
	// bad statement and keep parsing (SQLite) produce these; Postgres fails
	// the whole file instead.
	KindInvalid
)

var kindNames = map[Kind]string{
	KindOther: "other", KindCreateTable: "create_table", KindAlterTable: "alter_table",
	KindCreateIndex: "create_index", KindDropIndex: "drop_index", KindDropTable: "drop_table",
	KindRenameTable: "rename_table", KindRenameColumn: "rename_column", KindAlterEnum: "alter_enum",
	KindRenameSchema: "rename_schema", KindDML: "dml", KindSet: "set", KindPragma: "pragma",
	KindCreateView: "create_view", KindDropView: "drop_view", KindCreateTrigger: "create_trigger",
	KindDropTrigger: "drop_trigger", KindTransaction: "transaction", KindInvalid: "invalid",
}

func (k Kind) String() string { return kindNames[k] }

// IsDDL reports whether the statement changes schema (as opposed to data,
// session settings, or transaction control).
func (k Kind) IsDDL() bool {
	switch k {
	case KindCreateTable, KindAlterTable, KindCreateIndex, KindDropIndex, KindDropTable,
		KindRenameTable, KindRenameColumn, KindAlterEnum, KindRenameSchema,
		KindCreateView, KindDropView, KindCreateTrigger, KindDropTrigger:
		return true
	}
	return false
}

// Statement is one parsed SQL statement with its source position.
type Statement struct {
	Kind Kind
	// Raw is the statement text without the trailing semicolon.
	Raw string
	// Line is the 1-based line of the statement's first token within the
	// source passed to Parse.
	Line int
	// Offset and End are byte offsets of Raw within the source.
	Offset, End int
	// LeadingComments are the comment lines (marker stripped, trimmed) that
	// appear between the previous statement and this one. Disable annotations
	// are read from here.
	LeadingComments []string
	// Error is set for KindInvalid.
	Error string

	CreateTable  *CreateTable
	AlterTable   *AlterTable
	CreateIndex  *CreateIndex
	DropIndex    *DropIndex
	DropTable    *DropTable
	RenameTable  *RenameTable
	RenameColumn *RenameColumn
	AlterEnum    *AlterEnum
	RenameSchema *RenameSchema
	DML          *DML
	Set          *Set
	Pragma       *Pragma
	View         *ObjectRef // KindCreateView / KindDropView
	Trigger      *ObjectRef // KindCreateTrigger / KindDropTrigger
	Transaction  *Transaction
}

// Tables returns every table the statement targets (for catalog updates and
// "which migration touched this table" heuristics).
func (s *Statement) Tables() []TableName {
	switch s.Kind {
	case KindCreateTable:
		return []TableName{s.CreateTable.Table}
	case KindAlterTable:
		return []TableName{s.AlterTable.Table}
	case KindCreateIndex:
		return []TableName{s.CreateIndex.Table}
	case KindDropTable:
		return s.DropTable.Tables
	case KindRenameTable:
		return []TableName{s.RenameTable.Table}
	case KindRenameColumn:
		return []TableName{s.RenameColumn.Table}
	case KindDML:
		if s.DML.Table.Name != "" {
			return []TableName{s.DML.Table}
		}
	case KindCreateTrigger, KindDropTrigger:
		if s.Trigger != nil && s.Trigger.Table.Name != "" {
			return []TableName{s.Trigger.Table}
		}
	}
	return nil
}

// TableName is a possibly schema-qualified relation name.
type TableName struct {
	Schema string
	Name   string
}

func (t TableName) String() string {
	if t.Schema != "" {
		return t.Schema + "." + t.Name
	}
	return t.Name
}

// Key returns a case-folded identity for catalog lookups. An unqualified name
// matches the same unqualified name; callers that need schema resolution
// compare Schema separately.
func (t TableName) Key() string { return strings.ToLower(t.Name) }

// ObjectRef names a view or trigger and, for triggers, the table it is on.
type ObjectRef struct {
	Name  string
	Table TableName
	// Definition is the view's SELECT or the trigger body, when available.
	// SQLite rules search it for column references.
	Definition string
}

// Expr is an opaque expression with the properties rules care about.
type Expr struct {
	Raw string
	// IsConstant is true for literals, NULL, and constant casts of literals.
	IsConstant bool
	// Funcs lists the lower-cased names of every function called, in order.
	Funcs []string
}

// ColumnDef describes a column in CREATE TABLE or ADD COLUMN.
type ColumnDef struct {
	Name string
	// Type is the lower-cased base type name with Postgres internal aliases
	// resolved to their SQL spelling: "int4"->"integer", "int8"->"bigint",
	// "bpchar"->"char", "varchar", "text", "json", "jsonb", "timestamp",
	// "timestamptz", "numeric", "serial", "bigserial", ...
	Type string
	// TypeMods are the numeric modifiers: varchar(255) -> [255], numeric(10,2) -> [10,2].
	TypeMods []int
	IsArray  bool
	NotNull  bool
	Default  *Expr
	// Identity is set for GENERATED {ALWAYS|BY DEFAULT} AS IDENTITY.
	Identity bool
	// Serial is set for serial/smallserial/bigserial pseudo-types.
	Serial bool
	// Generated is "stored" or "virtual" for GENERATED ALWAYS AS (expr), else "".
	Generated string
	// GeneratedExpr is the generation expression, when the parser exposes it.
	GeneratedExpr *Expr
	PrimaryKey    bool
	Unique        bool
	// References is the inline REFERENCES target, if any.
	References *TableName
	// Constraints are inline CHECK/FK/etc. constraints attached to the column.
	Constraints []Constraint
}

// ConstraintKind classifies table and column constraints.
type ConstraintKind int

const (
	ConstraintOther ConstraintKind = iota
	ConstraintCheck
	ConstraintPrimaryKey
	ConstraintUnique
	ConstraintForeignKey
	ConstraintExclusion
	ConstraintNotNull
)

var constraintNames = map[ConstraintKind]string{
	ConstraintOther: "other", ConstraintCheck: "check", ConstraintPrimaryKey: "primary_key",
	ConstraintUnique: "unique", ConstraintForeignKey: "foreign_key",
	ConstraintExclusion: "exclusion", ConstraintNotNull: "not_null",
}

func (c ConstraintKind) String() string { return constraintNames[c] }

// Constraint is a table-level or column-level constraint.
type Constraint struct {
	Kind    ConstraintKind
	Name    string
	Columns []string
	// NotValid is Postgres' NOT VALID (skip validation of existing rows).
	NotValid bool
	// Expr is the CHECK expression.
	Expr *Expr
	// References / RefColumns describe a foreign key target.
	References *TableName
	RefColumns []string
	// UsingIndex is set for UNIQUE/PRIMARY KEY ... USING INDEX name.
	UsingIndex string
}

// CreateTable payload.
type CreateTable struct {
	Table       TableName
	Columns     []ColumnDef
	Constraints []Constraint
	IfNotExists bool
	Temporary   bool
	// Partitioned is true for PARTITION BY tables (Postgres).
	Partitioned bool
}

// AlterActionKind classifies each action inside ALTER TABLE.
type AlterActionKind int

const (
	ActionOther AlterActionKind = iota
	ActionAddColumn
	ActionDropColumn
	ActionAlterColumnType
	ActionSetNotNull
	ActionDropNotNull
	ActionSetDefault
	ActionDropDefault
	ActionAddConstraint
	ActionDropConstraint
	ActionValidateConstraint
	ActionAddIdentity
	// ActionUnsupported is an ALTER form the engine cannot execute at all
	// (SQLite: ALTER COLUMN, ADD CONSTRAINT, ...). Raw describes it.
	ActionUnsupported
)

var actionNames = map[AlterActionKind]string{
	ActionOther: "other", ActionAddColumn: "add_column", ActionDropColumn: "drop_column",
	ActionAlterColumnType: "alter_column_type", ActionSetNotNull: "set_not_null",
	ActionDropNotNull: "drop_not_null", ActionSetDefault: "set_default", ActionDropDefault: "drop_default",
	ActionAddConstraint: "add_constraint", ActionDropConstraint: "drop_constraint",
	ActionValidateConstraint: "validate_constraint", ActionAddIdentity: "add_identity",
	ActionUnsupported: "unsupported",
}

func (a AlterActionKind) String() string { return actionNames[a] }

// AlterAction is one clause of an ALTER TABLE statement.
type AlterAction struct {
	Kind AlterActionKind
	// Column is the target column for column-level actions.
	Column string
	// ColumnDef is set for ActionAddColumn.
	ColumnDef *ColumnDef
	// NewType is set for ActionAlterColumnType (only Type/TypeMods/IsArray are meaningful).
	NewType *ColumnDef
	// Using is the USING expression of ALTER COLUMN TYPE, if any.
	Using *Expr
	// Default is set for ActionSetDefault.
	Default *Expr
	// Constraint is set for ActionAddConstraint.
	Constraint *Constraint
	// ConstraintName is set for ActionDropConstraint / ActionValidateConstraint.
	ConstraintName string
	// Raw is a short description of the action for messages.
	Raw string
}

// AlterTable payload.
type AlterTable struct {
	Table    TableName
	Actions  []AlterAction
	IfExists bool
}

// CreateIndex payload.
type CreateIndex struct {
	Name        string
	Table       TableName
	Columns     []string // column names; expression columns appear as their raw text
	Unique      bool
	Concurrent  bool
	IfNotExists bool
	Method      string
	Partial     bool // has WHERE
}

// DropIndex payload.
type DropIndex struct {
	Names      []string
	Concurrent bool
	IfExists   bool
}

// DropTable payload.
type DropTable struct {
	Tables   []TableName
	IfExists bool
}

// RenameTable payload (ALTER TABLE x RENAME TO y).
type RenameTable struct {
	Table   TableName
	NewName string
}

// RenameColumn payload (ALTER TABLE x RENAME [COLUMN] a TO b).
type RenameColumn struct {
	Table TableName
	Old   string
	New   string
}

// AlterEnum payload (ALTER TYPE ... RENAME VALUE / ADD VALUE).
type AlterEnum struct {
	Type string
	// RenameOld/RenameNew are set for RENAME VALUE.
	RenameOld, RenameNew string
	// AddValue is set for ADD VALUE.
	AddValue string
}

// RenameSchema payload.
type RenameSchema struct {
	Old, New string
}

// DML payload for INSERT/UPDATE/DELETE (and bare SELECT, Op "select").
type DML struct {
	Op    string // insert | update | delete | select
	Table TableName
	// FromSelect is true for INSERT ... SELECT.
	FromSelect bool
}

// Set payload for SET [LOCAL] name = value.
type Set struct {
	Name  string
	Value string
	Local bool
}

// Pragma payload (SQLite).
type Pragma struct {
	Name  string
	Value string
}

// Transaction payload.
type Transaction struct {
	Begin  bool
	Commit bool // COMMIT or END
}
