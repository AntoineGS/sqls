// Package catalog holds the neutral, engine-independent descriptor models,
// exact object identities and immutable-by-contract cache shared by SQLs and
// its embedding hosts.
package catalog

import (
	"database/sql"
	"fmt"
	"strings"
)

type ColumnBase struct {
	Schema string
	Table  string
	Name   string
}

type ColumnDesc struct {
	ColumnBase
	Type    string
	Null    string
	Key     string
	Default sql.NullString
	Extra   string
	// Facts retains the native scalar facts behind Type so a caller can
	// re-render it for a different SQL dialect without a second catalog
	// read. Nil when the source engine has no dialect-sensitive rendering
	// (or has not been updated to populate it).
	Facts *ColumnTypeFacts
}

// ColumnTypeFacts is the raw, engine-native projection a dialect-aware
// renderer needs to reproduce Type (or a domain/parameter's type) for a
// specific SQL dialect without re-querying the database. Its fields follow
// InterBase's RDB$FIELDS projection; other engines leave it nil. Display
// labels are derived from these facts, never the reverse: a rendered string
// is never parsed back into one of these fields.
type ColumnTypeFacts struct {
	FieldType        sql.NullInt64
	FieldSubType     sql.NullInt64
	FieldScale       sql.NullInt64
	FieldPrecision   sql.NullInt64
	FieldLength      sql.NullInt64
	CharacterLength  sql.NullInt64
	CharacterSetID   sql.NullInt64
	CharacterSetName sql.NullString
	CollationID      sql.NullInt64
	CollationName    sql.NullString
	Dimensions       sql.NullInt64
	// DomainName is the resolved field-source/domain identifier, valid
	// whenever the column or parameter resolved to an RDB$FIELDS row.
	DomainName sql.NullString
	// UserDomain reports whether Type refers to a shared, user-declared
	// domain (rendered as a bare domain-name reference) rather than an
	// inline type declaration.
	UserDomain bool
	// SystemDomain reports whether the resolved domain is system-generated
	// (RDB$FIELDS.RDB$SYSTEM_FLAG <> 0), which excludes it from DDL that
	// regenerates user domains.
	SystemDomain bool
	// ComputedSource, ValidationSource and DomainDefault are the domain's
	// own COMPUTED BY, CHECK and DEFAULT sources, verbatim.
	ComputedSource   sql.NullString
	ValidationSource sql.NullString
	DomainDefault    sql.NullString
	// ColumnCollationID/ColumnCollationName are the column's own collation
	// override, distinct from the domain's collation.
	ColumnCollationID   sql.NullInt64
	ColumnCollationName sql.NullString
}

func (cd *ColumnDesc) OnelineDesc() string {
	items := []string{}
	if cd.Type != "" {
		items = append(items, "`"+cd.Type+"`")
	}
	if cd.Key == "YES" {
		items = append(items, "PRIMARY KEY")
	} else if cd.Key != "" && cd.Key != "NO" {
		items = append(items, cd.Key)
	}
	if cd.Extra != "" {
		items = append(items, cd.Extra)
	}
	return strings.Join(items, " ")
}

type ForeignKey [][2]*ColumnBase

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
	ObjectKindException ObjectKind = "exception"
)

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

// SystemRelationDesc describes one system relation. It is stored separately
// from user SchemaTables and carries classification explicitly rather than
// inferring it from the relation name.
type SystemRelationDesc struct {
	Schema    string
	Name      string
	ID        sql.NullInt64
	OwnerName sql.NullString
	IsSystem  bool
}

// ExceptionDesc describes one InterBase exception. Message keeps SQL NULL
// distinct from a valid empty string for generated-detail consumers.
type ExceptionDesc struct {
	Schema  string
	Name    string
	Number  sql.NullInt64
	Message sql.NullString
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
	// Facts retains the native facts behind Type; see ColumnDesc.Facts.
	Facts *ColumnTypeFacts
	// Firebird's procedure-parameter catalog can retain TYPE OF declarations.
	// InterBase leaves these optional fields empty.
	FirebirdParameterMechanism sql.NullInt64
	FirebirdTypeOfFieldName    sql.NullString
	FirebirdTypeOfRelationName sql.NullString
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
	// TypeCode is the raw RDB$TRIGGER_TYPE code Event was decoded from. DDL
	// regeneration needs the code itself, not the decoded label: multi-event
	// triggers are valid catalog metadata but invalid CREATE TRIGGER DDL,
	// and only the raw code lets a renderer detect that case.
	TypeCode sql.NullInt64
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
	// Facts retains the native facts behind Type; see ColumnDesc.Facts.
	Facts *ColumnTypeFacts
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
	// Direction is the raw RDB$INDEX_TYPE code (0 ascending, 1 descending).
	// DDL regeneration needs the code, not just Unique/Active.
	Direction sql.NullInt64
}

// FunctionDesc describes an external function (UDF) declaration.
type FunctionDesc struct {
	Schema         string
	Name           string
	FunctionType   sql.NullInt64
	SystemFlag     sql.NullInt64
	ReturnType     string // rendered; "" when the catalog cannot render it
	ReturnPosition sql.NullInt64
	Arguments      []*FunctionArgumentDesc // ordered by Position
	ModuleName     sql.NullString
	EntryPoint     sql.NullString
	Description    sql.NullString
}

// FunctionArgumentDesc describes one external function argument.
type FunctionArgumentDesc struct {
	Name            string
	FunctionName    string
	Position        sql.NullInt64
	Mechanism       sql.NullInt64
	FieldLength     sql.NullInt64
	FieldScale      sql.NullInt64
	FieldType       sql.NullInt64
	FieldSubType    sql.NullInt64
	CharacterSetID  sql.NullInt64
	FieldPrecision  sql.NullInt64
	CharacterLength sql.NullInt64
	Type            string // rendered; "" when the catalog cannot render it
}

// CatalogCache holds extended catalog objects. A nil *CatalogCache means the
// active repository does not implement CatalogRepository. The Views,
// Procedures, Generators, Domains, Functions, Indexes and Triggers maps are
// keyed by the upper-cased object name: two exact-identity objects of the
// same kind that fold to the same upper-cased name (for example a
// quoted-mixed-case name alongside its all-upper-case counterpart) silently
// collapse to one entry in these legacy maps. Existing consumers rely on
// that shape and are unaffected by the fields below.
//
// The *ByID maps and Objects preserve every object's exact identity
// alongside those legacy maps, keyed by catalog.ObjectID.Key() instead of an
// upper-cased name, so a caller that already has (or resolves) an exact
// ObjectID never risks that collision. Objects additionally exposes
// dialect-aware unquoted lookup across all kinds, reporting an ambiguous
// unquoted name explicitly rather than picking one object arbitrarily. A nil
// Objects/*ByID map (an old caller's CatalogCache literal, or a category
// that has not published yet) means no exact-identity data is available yet,
// not that no objects exist.
type CatalogCache struct {
	Views           map[string]*ViewDesc
	Procedures      map[string]*ProcedureDesc
	Generators      map[string]*GeneratorDesc
	Domains         map[string]*DomainDesc
	Functions       map[string]*FunctionDesc
	Indexes         map[string]*IndexDesc
	IndexesByTable  map[string][]*IndexDesc
	Triggers        map[string]*TriggerDesc
	TriggersByTable map[string][]*TriggerDesc

	Objects        *ObjectIndex
	ViewsByID      map[string]*ViewDesc
	ProceduresByID map[string]*ProcedureDesc
	GeneratorsByID map[string]*GeneratorDesc
	DomainsByID    map[string]*DomainDesc
	FunctionsByID  map[string]*FunctionDesc
	// IndexesByID uses ObjectID.Key; schema-bearing index IDs include
	// Parent=RelationName so duplicate names on different tables remain exact.
	IndexesByID  map[string]*IndexDesc
	TriggersByID map[string]*TriggerDesc
	// SystemRelationsByID and ExceptionsByID are exact-only category stores.
	// System relations use ObjectKindTable identities but never enter the user
	// relation maps or completion candidates.
	SystemRelationsByID map[string]*SystemRelationDesc
	ExceptionsByID      map[string]*ExceptionDesc
}

// ColumnDatabaseKey normalises a schema/table pair for column cache lookup.
func ColumnDatabaseKey(dbName, tableName string) string {
	return strings.ToUpper(dbName) + "\t" + strings.ToUpper(tableName)
}

// CatalogCacheKey normalises an object name for cache lookup. InterBase
// stores catalog names upper-cased while users type them lower-cased, so an
// exact-match accessor would silently miss for every lower-case identifier.
func CatalogCacheKey(name string) string {
	return strings.ToUpper(strings.TrimSpace(name))
}

// GenColumnMap groups column descriptors by their schema/table key.
func GenColumnMap(columnDescs []*ColumnDesc) map[string][]*ColumnDesc {
	columnMap := map[string][]*ColumnDesc{}
	for _, desc := range columnDescs {
		key := ColumnDatabaseKey(desc.Schema, desc.Table)
		columnMap[key] = append(columnMap[key], desc)
	}
	return columnMap
}

// ValidateForeignKey reports whether key has a non-empty table pair with both
// endpoints populated for every segment pair.
func ValidateForeignKey(key *ForeignKey) error {
	if key == nil || len(*key) == 0 {
		return fmt.Errorf("missing table pair")
	}
	for pairIndex, pair := range *key {
		if len(pair) != 2 || pair[0] == nil || pair[1] == nil {
			return fmt.Errorf("pair %d is missing a table endpoint", pairIndex)
		}
	}
	return nil
}

// GroupForeignKeys indexes every foreign key under both endpoint tables.
func GroupForeignKeys(keys []*ForeignKey) (map[string]map[string][]*ForeignKey, error) {
	grouped := make(map[string]map[string][]*ForeignKey)
	for i, key := range keys {
		if err := ValidateForeignKey(key); err != nil {
			return nil, fmt.Errorf("malformed foreign key at index %d: %w", i, err)
		}
		pair := (*key)[0]
		left, right := pair[0].Table, pair[1].Table
		if grouped[left] == nil {
			grouped[left] = make(map[string][]*ForeignKey)
		}
		grouped[left][right] = append(grouped[left][right], key)
		if grouped[right] == nil {
			grouped[right] = make(map[string][]*ForeignKey)
		}
		grouped[right][left] = append(grouped[right][left], key)
	}
	return grouped, nil
}
