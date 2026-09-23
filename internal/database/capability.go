package database

import (
	"context"
	"database/sql"
	"errors"
)

// CatalogRepository is implemented by repositories that can enumerate catalog
// objects beyond tables and columns. All methods return objects for the whole
// attachment; sqls drivers without a schema namespace use an empty Schema.
type CatalogRepository interface {
	DescribeViews(ctx context.Context) ([]*ViewDesc, error)
	DescribeProcedures(ctx context.Context) ([]*ProcedureDesc, error)
	DescribeGenerators(ctx context.Context) ([]*GeneratorDesc, error)
	DescribeTriggers(ctx context.Context) ([]*TriggerDesc, error)
	DescribeDomains(ctx context.Context) ([]*DomainDesc, error)
	DescribeIndexes(ctx context.Context) ([]*IndexDesc, error)
	DescribeFunctions(ctx context.Context) ([]*FunctionDesc, error)
}

// DDLRepository is implemented by repositories that can reproduce an object's
// definition. Callers must handle ErrObjectNotFound and ErrUnsupportedDDL.
type DDLRepository interface {
	ObjectDDL(ctx context.Context, kind ObjectKind, name string) (string, error)
}

// TableDescriptionRepository is an optional capability for repositories that
// can return a non-executable, catalog-derived description of an exact table.
type TableDescriptionRepository interface {
	TableDescription(ctx context.Context, name string) (TableDescription, error)
}

// ExplainRepository is implemented by repositories that can return a server
// query plan without executing the statement's result set.
type ExplainRepository interface {
	ExplainPlan(ctx context.Context, query string) (string, error)
}

// CatalogSnapshotRepository is implemented by repositories that can serve a
// whole cache build from one consistent catalog read. The returned repository
// is read-only and valid until close is called; the source repository is
// unaffected and remains usable concurrently.
type CatalogSnapshotRepository interface {
	CatalogSnapshot(ctx context.Context) (repo DBRepository, close func() error, err error)
}

// TableDescription is a SQL-shaped informational reconstruction of recorded
// table metadata. It is not guaranteed executable SQL. Its spans are byte
// offsets into Body and Columns remain in catalog order.
type TableDescription struct {
	Body    string
	Table   DescriptionSpan
	Columns []DescriptionColumn
}

// DescriptionSpan identifies a byte range in a TableDescription body.
type DescriptionSpan struct {
	Start int
	End   int
}

// DescriptionColumn identifies a table column and its byte range in a
// TableDescription body.
type DescriptionColumn struct {
	Name string
	Span DescriptionSpan
}

var (
	// ErrObjectNotFound reports that the named catalog object does not exist.
	ErrObjectNotFound = errors.New("database: catalog object not found")
	// ErrUnsupportedDDL reports that the catalog cannot reproduce the object's
	// definition faithfully. Use UnsupportedDDLDetail for the structured reason.
	ErrUnsupportedDDL = errors.New("database: DDL is unavailable for this object")
)

// unsupportedDDLDetailer is implemented by driver-specific errors that carry a
// structured reason. It keeps this file free of any driver import.
type unsupportedDDLDetailer interface {
	UnsupportedDDLDetail() (object, name, feature string)
}

// UnsupportedDDLDetail reports the structured reason behind an
// ErrUnsupportedDDL error: the object kind, the object name, and the metadata
// facet that could not be rendered. ok is false when err carries no detail.
func UnsupportedDDLDetail(err error) (object, name, feature string, ok bool) {
	var detailer unsupportedDDLDetailer
	if !errors.As(err, &detailer) {
		return "", "", "", false
	}
	object, name, feature = detailer.UnsupportedDDLDetail()
	return object, name, feature, true
}

// ObjectKind identifies a catalog object kind for capability lookups.
type ObjectKind string

const (
	ObjectKindTable     ObjectKind = "table"
	ObjectKindView      ObjectKind = "view"
	ObjectKindProcedure ObjectKind = "procedure"
	ObjectKindTrigger   ObjectKind = "trigger"
	ObjectKindDomain    ObjectKind = "domain"
	ObjectKindIndex     ObjectKind = "index"
	ObjectKindGenerator ObjectKind = "generator"
	ObjectKindFunction  ObjectKind = "function"
)

// ViewDesc describes a view, its source text, and its resolved columns.
type ViewDesc struct {
	Schema      string // "" for InterBase
	Name        string
	OwnerName   sql.NullString
	ViewSource  sql.NullString
	Description sql.NullString
	Columns     []*ColumnDesc // ordered; same rendering as table columns
}

// GeneratorDesc describes a generator/sequence. InterBase's own DDL is
// CREATE GENERATOR, and the catalog carries nothing but identity.
type GeneratorDesc struct {
	Schema string
	Name   string
	ID     sql.NullInt64
}

// ProcedureDesc describes a stored procedure and its ordered parameters.
type ProcedureDesc struct {
	Schema           string
	Name             string
	OwnerName        sql.NullString
	Source           sql.NullString // PSQL body verbatim
	Description      sql.NullString
	InputParameters  []*ProcedureParameterDesc // ordered by Position
	OutputParameters []*ProcedureParameterDesc // ordered by Position
}

// ParameterDirection identifies a parameter's direction. The values match
// schema.ParameterInput and schema.ParameterOutput.
type ParameterDirection string

const (
	ParameterInput  ParameterDirection = "input"
	ParameterOutput ParameterDirection = "output"
)

// ProcedureParameterDesc describes one procedure parameter.
type ProcedureParameterDesc struct {
	Name        string
	Position    int
	Direction   ParameterDirection
	Type        string       // rendered for the resolved dialect; "" when unrenderable
	Domain      string       // user domain name; "" for an inline type
	Nullable    sql.NullBool // invalid when the catalog cannot determine it
	Description sql.NullString
}

// TriggerDesc describes a DML or database trigger.
type TriggerDesc struct {
	Schema       string
	Name         string
	RelationName sql.NullString // invalid for a database-level trigger
	Event        string         // e.g. "BEFORE INSERT"; "" when undecodable
	Sequence     sql.NullInt64
	Active       sql.NullBool
	Source       sql.NullString
	Description  sql.NullString
}

// DomainDesc describes a user domain.
type DomainDesc struct {
	Schema           string
	Name             string
	Type             string // full rendering, including CHARACTER SET/COLLATE
	Nullable         sql.NullBool
	DefaultSource    sql.NullString
	ValidationSource sql.NullString // CHECK text, verbatim
	CharacterSetName sql.NullString
	CollationName    sql.NullString
	Description      sql.NullString
}

// IndexDesc describes an index and its ordered segments.
type IndexDesc struct {
	Schema         string
	Name           string
	RelationName   string
	Columns        []string       // ordered segment names; empty for an expression index
	Expression     sql.NullString // invalid for a segment index
	Unique         sql.NullBool
	Active         sql.NullBool
	ConstraintName sql.NullString // owning constraint; invalid when standalone
	Description    sql.NullString
}

// FunctionDesc describes an external function (UDF) declaration. sqls never
// invokes a UDF; this is declaration metadata only.
type FunctionDesc struct {
	Schema         string
	Name           string
	ReturnType     string // rendered; "" when the catalog cannot render it
	ReturnPosition sql.NullInt64
	Arguments      []*FunctionArgumentDesc // ordered by Position
	ModuleName     sql.NullString
	EntryPoint     sql.NullString
	Description    sql.NullString
}

// FunctionArgumentDesc describes one external function argument.
type FunctionArgumentDesc struct {
	Name     string
	Position sql.NullInt64
	Type     string // rendered; "" when the catalog cannot render it
}
