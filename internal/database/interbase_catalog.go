package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"interbase-go/schema"
)

// interBaseTypeName renders the column/parameter type for sqlDialect, in the
// full form that includes any CHARACTER SET and COLLATE suffix.
//
// The rules, in order (spec §4.3):
//
//  1. A nil domain renders nothing: the catalog row resolved to no RDB$FIELDS
//     entry, so there is no type to report.
//  2. Under SQL Dialect 1, field type 35 is DATE. Dialect 1 has no separate
//     TIMESTAMP, and this is the only genuinely dialect-dependent rule.
//  3. Otherwise delegate to Domain.SQLType(), which is Dialect 3 correct and
//     renders charset, collation and validated precision/scale.
//  4. On error, fall back to the retained interBaseColumnType switch rather
//     than to TYPE(n). SQLType() returns ErrUnsupportedDDL on a dozen paths the
//     switch renders correctly today, so the fallback is what makes the
//     migration behavior preserving: SQLType() can upgrade a rendering and the
//     switch guarantees none regresses.
func interBaseTypeName(domain *schema.Domain, sqlDialect int) string {
	if domain == nil {
		return ""
	}
	if sqlDialect == 1 && domain.FieldType.Valid && domain.FieldType.Int64 == interBaseFieldTypeTimestamp {
		return "DATE"
	}
	if rendered, err := domain.SQLType(); err == nil {
		return rendered
	}
	return interBaseColumnType(domain)
}

// interBaseColumnTypeName renders the one-line form used for ColumnDesc.Type.
// SQLType appends CHARACTER SET and COLLATE clauses and the completion detail
// line must stay short, so the text is cut at the first of them. The cut is
// safe: no base type name contains either keyword.
func interBaseColumnTypeName(domain *schema.Domain, sqlDialect int) string {
	rendered := interBaseTypeName(domain, sqlDialect)
	if index := strings.Index(rendered, " CHARACTER SET "); index >= 0 {
		rendered = rendered[:index]
	}
	if index := strings.Index(rendered, " COLLATE "); index >= 0 {
		rendered = rendered[:index]
	}
	return rendered
}

// interBaseFieldTypeTimestamp is RDB$FIELD_TYPE 35, which Dialect 3 calls
// TIMESTAMP and Dialect 1 calls DATE.
const interBaseFieldTypeTimestamp = 35

// interBaseColumnType is the fixed-dialect type switch sqls has always used,
// re-sourced to read schema.Domain instead of a hand-scanned catalog row. Its
// logic is unchanged. It is retained as the exhaustive fallback for
// interBaseTypeName; TYPE(n) is reached exactly where it is reached today.
func interBaseColumnType(domain *schema.Domain) string {
	if domain == nil || !domain.FieldType.Valid {
		return ""
	}
	fieldType := domain.FieldType.Int64
	switch fieldType {
	case 7:
		return interBaseNumericType("SMALLINT", 4, domain)
	case 8:
		return interBaseNumericType("INTEGER", 9, domain)
	case 9:
		return "QUAD"
	case 10:
		return "FLOAT"
	case 12:
		return "DATE"
	case 13:
		return "TIME"
	case 14:
		return fmt.Sprintf("CHAR(%d)", interBaseCharacterLength(domain))
	case 16:
		return interBaseNumericType("BIGINT", 18, domain)
	case 17:
		return "BOOLEAN"
	case 27:
		// In Dialect 1, scaled NUMERIC/DECIMAL values can use DOUBLE
		// PRECISION as their underlying field type. A subtype without a
		// negative scale does not carry a fixed-point declaration, so keep
		// the underlying DOUBLE PRECISION rather than inventing (15, 0).
		if domain.FieldScale.Valid && domain.FieldScale.Int64 < 0 {
			return interBaseNumericType("DOUBLE PRECISION", 15, domain)
		}
		return "DOUBLE PRECISION"
	case interBaseFieldTypeTimestamp:
		// Dialect 1 uses field type 35 for DATE. The dialect 3 distinction is
		// made by interBaseTypeName, which reaches this switch only as a
		// fallback.
		return "DATE"
	case 37:
		return fmt.Sprintf("VARCHAR(%d)", interBaseCharacterLength(domain))
	case 40:
		return fmt.Sprintf("CSTRING(%d)", interBaseCharacterLength(domain))
	case 45:
		return "BLOB_ID"
	case 261:
		return "BLOB"
	default:
		return fmt.Sprintf("TYPE(%d)", fieldType)
	}
}

func interBaseCharacterLength(domain *schema.Domain) int64 {
	if domain == nil {
		return 0
	}
	if domain.CharacterLength.Valid {
		return domain.CharacterLength.Int64
	}
	if domain.FieldLength.Valid {
		return domain.FieldLength.Int64
	}
	return 0
}

func interBaseNumericType(base string, naturalPrecision int64, domain *schema.Domain) string {
	subtype := int64(0)
	if domain.FieldSubType.Valid {
		subtype = domain.FieldSubType.Int64
	}
	scale := int64(0)
	if domain.FieldScale.Valid {
		scale = domain.FieldScale.Int64
	}
	if subtype == 0 && scale >= 0 {
		return base
	}

	precision := naturalPrecision
	if domain.FieldPrecision.Valid && domain.FieldPrecision.Int64 > 0 {
		precision = domain.FieldPrecision.Int64
	}
	numericName := "NUMERIC"
	if subtype == 2 {
		numericName = "DECIMAL"
	}
	if scale < 0 {
		scale = -scale
	}
	return fmt.Sprintf("%s(%d, %d)", numericName, precision, scale)
}

// interBaseCatalogSnapshot holds one consistent catalog read. A repository
// carrying one serves every cache-build read from it instead of issuing fresh
// queries; see CatalogSnapshot.
type interBaseCatalogSnapshot struct {
	catalog     *schema.Catalog
	relations   []schema.Relation
	constraints []schema.Constraint
}

// catalogReader returns the catalog to read through: the snapshot's
// transaction-bound one when this repository is a snapshot, a fresh one over
// the pooled *sql.DB otherwise.
func (db *InterBaseDBRepository) catalogReader() (*schema.Catalog, error) {
	if db == nil {
		return nil, errors.New("interbase: database connection is nil")
	}
	if db.snapshot != nil {
		return db.snapshot.catalog, nil
	}
	if db.Conn == nil {
		return nil, errors.New("interbase: database connection is nil")
	}
	return schema.New(db.Conn), nil
}

// relations returns every user table and view with its ordered columns.
// Relations issues one column query per relation, so a snapshot reads it once
// per cache build rather than once per repository method.
func (db *InterBaseDBRepository) relations(ctx context.Context) ([]schema.Relation, error) {
	if db != nil && db.snapshot != nil {
		return db.snapshot.relations, nil
	}
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	return catalog.Relations(ctx, "")
}

// constraints returns every user-relation constraint with its resolved
// columns. Constraints loads the enforcing and referenced index per
// constraint, so the same snapshot rule applies.
func (db *InterBaseDBRepository) constraints(ctx context.Context) ([]schema.Constraint, error) {
	if db != nil && db.snapshot != nil {
		return db.snapshot.constraints, nil
	}
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	return catalog.Constraints(ctx, "")
}

func (db *InterBaseDBRepository) SchemaTables(ctx context.Context) (map[string][]string, error) {
	relations, err := db.relations(ctx)
	if err != nil {
		return nil, err
	}
	// Tables and views together, exactly as interBaseRelationsQuery returned
	// them: the extended view cache is additive metadata, not a replacement.
	names := make([]string, 0, len(relations))
	for _, relation := range relations {
		names = append(names, relation.Name)
	}
	return map[string][]string{"": names}, nil
}

func (db *InterBaseDBRepository) DescribeDatabaseTable(ctx context.Context) ([]*ColumnDesc, error) {
	return db.describeColumns(ctx)
}

func (db *InterBaseDBRepository) DescribeDatabaseTableBySchema(ctx context.Context, _ string) ([]*ColumnDesc, error) {
	return db.describeColumns(ctx)
}

func (db *InterBaseDBRepository) describeColumns(ctx context.Context) ([]*ColumnDesc, error) {
	relations, err := db.relations(ctx)
	if err != nil {
		return nil, err
	}
	constraints, err := db.constraints(ctx)
	if err != nil {
		return nil, err
	}
	primaryKeys := interBasePrimaryKeyColumns(constraints)

	result := make([]*ColumnDesc, 0)
	for _, relation := range relations {
		for _, column := range relation.Columns {
			result = append(result, db.columnDescription(relation.Name, column, primaryKeys))
		}
	}
	return result, nil
}

// columnDescription maps one catalog column onto the shared descriptor. A
// column whose field source resolved to no RDB$FIELDS row keeps its place with
// an empty type rather than disappearing.
func (db *InterBaseDBRepository) columnDescription(relationName string, column schema.Column, primaryKeys map[string]struct{}) *ColumnDesc {
	key := "NO"
	if _, ok := primaryKeys[interBaseRelationColumnKey(relationName, column.Name)]; ok {
		key = "YES"
	}

	var domainNullFlag sql.NullInt64
	var domainDefault sql.NullString
	if column.Domain != nil {
		domainNullFlag = column.Domain.NullFlag
		domainDefault = column.Domain.DefaultSource
	}

	extra := ""
	if interBaseIsComputed(column) {
		extra = "COMPUTED"
	}

	return &ColumnDesc{
		ColumnBase: ColumnBase{
			Schema: "",
			Table:  relationName,
			Name:   column.Name,
		},
		Type:    interBaseColumnTypeName(column.Domain, db.SQLDialect),
		Null:    interBaseNullability(column.NullFlag, domainNullFlag),
		Key:     key,
		Default: interBaseEffectiveDefault(column.DefaultSource, domainDefault),
		Extra:   extra,
	}
}

// interBaseIsComputed reports whether a column carries a COMPUTED BY source.
// InterBase stores it on the column's RDB$FIELDS row, which schema surfaces on
// both the column and its domain depending on the projection, so both are
// consulted.
func interBaseIsComputed(column schema.Column) bool {
	if column.ComputedSource.Valid && strings.TrimSpace(column.ComputedSource.String) != "" {
		return true
	}
	if column.Domain != nil && column.Domain.ComputedSource.Valid &&
		strings.TrimSpace(column.Domain.ComputedSource.String) != "" {
		return true
	}
	return false
}

// interBasePrimaryKeyColumns indexes every primary-key column segment so a
// column's key flag is one map lookup rather than a per-column subquery.
func interBasePrimaryKeyColumns(constraints []schema.Constraint) map[string]struct{} {
	primaryKeys := make(map[string]struct{})
	for _, constraint := range constraints {
		if !strings.EqualFold(strings.TrimSpace(constraint.ConstraintType), string(schema.ConstraintPrimaryKey)) {
			continue
		}
		for _, column := range constraint.Columns {
			primaryKeys[interBaseRelationColumnKey(constraint.RelationName, column)] = struct{}{}
		}
	}
	return primaryKeys
}

func interBaseRelationColumnKey(relationName, columnName string) string {
	return strings.ToUpper(strings.TrimSpace(relationName)) + "\t" + strings.ToUpper(strings.TrimSpace(columnName))
}

func (db *InterBaseDBRepository) DescribeForeignKeysBySchema(ctx context.Context, _ string) ([]*ForeignKey, error) {
	constraints, err := db.constraints(ctx)
	if err != nil {
		return nil, err
	}

	foreignKeys := make([]*ForeignKey, 0)
	for _, constraint := range constraints {
		if !strings.EqualFold(strings.TrimSpace(constraint.ConstraintType), string(schema.ConstraintForeignKey)) {
			continue
		}
		// A constraint whose enforcing or referenced index could not be
		// resolved carries no column pairing; skipping it drops one foreign
		// key rather than returning half of one.
		if len(constraint.Columns) == 0 || len(constraint.Columns) != len(constraint.ReferencedColumns) {
			continue
		}
		foreignKey := new(ForeignKey)
		for i, column := range constraint.Columns {
			left := &ColumnBase{Schema: "", Table: constraint.RelationName, Name: column}
			right := &ColumnBase{Schema: "", Table: constraint.ReferencedRelationName, Name: constraint.ReferencedColumns[i]}
			*foreignKey = append(*foreignKey, [2]*ColumnBase{left, right})
		}
		foreignKeys = append(foreignKeys, foreignKey)
	}
	return foreignKeys, nil
}
