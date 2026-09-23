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
	if fieldType == 27 && domain.FieldScale.Valid && domain.FieldScale.Int64 < 0 {
		if label, normalized := domain.CatalogTypeLabel(); normalized {
			return label
		}
	}
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
//
// bulk holds the cache-visible metadata, read in a fixed number of statements
// by interbase_cache_bulk.go. catalog stays bound to the same transaction so
// the extended catalog accessors — views, procedures, indexes and the rest —
// read the same instant through the driver's own APIs.
type interBaseCatalogSnapshot struct {
	catalog *schema.Catalog
	bulk    *interBaseBulkCatalog
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

// bulkCatalog returns the snapshot's cache-visible catalog, or nil on a
// repository that is not bound to a snapshot. It is the one test every
// cache-build read makes before falling back to the driver's catalog APIs.
func (db *InterBaseDBRepository) bulkCatalog() *interBaseBulkCatalog {
	if db == nil || db.snapshot == nil {
		return nil
	}
	return db.snapshot.bulk
}

// relations returns every user table and view with its ordered columns. It
// issues one column query per relation, which is why a cache build reads
// through a snapshot's bulk catalog instead; this is the standalone
// repository's path.
func (db *InterBaseDBRepository) relations(ctx context.Context) ([]schema.Relation, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	return catalog.Relations(ctx, "")
}

// constraints returns every user-relation constraint with its resolved
// columns. It loads the enforcing and referenced index per constraint, so the
// same rule applies: a cache build never reaches it.
func (db *InterBaseDBRepository) constraints(ctx context.Context) ([]schema.Constraint, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	return catalog.Constraints(ctx, "")
}

func (db *InterBaseDBRepository) SchemaTables(ctx context.Context) (map[string][]string, error) {
	// Tables and views together, exactly as interBaseRelationsQuery returned
	// them: the extended view cache is additive metadata, not a replacement.
	if bulk := db.bulkCatalog(); bulk != nil {
		return map[string][]string{"": bulk.relationNames()}, nil
	}

	relations, err := db.relations(ctx)
	if err != nil {
		return nil, err
	}
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
	if bulk := db.bulkCatalog(); bulk != nil {
		result := make([]*ColumnDesc, 0, bulk.columnCount())
		for _, relation := range bulk.relations {
			for _, column := range relation.columns {
				// A fresh descriptor per call: the snapshot's own data is
				// never handed to a cache that may outlive or mutate it.
				result = append(result, db.columnDescription(relation.name, column, bulk.primaryKeys))
			}
		}
		return result, nil
	}

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

// interBaseFlagIsSet renders an InterBase flag as "the flag is set", leaving
// an unset flag unknown rather than false.
func interBaseFlagIsSet(flag sql.NullInt64) sql.NullBool {
	if !flag.Valid {
		return sql.NullBool{}
	}
	return sql.NullBool{Bool: flag.Int64 != 0, Valid: true}
}

// interBaseFlagIsClear renders an InterBase flag as "the flag is clear". The
// catalog stores INACTIVE flags, and the descriptors carry Active.
func interBaseFlagIsClear(flag sql.NullInt64) sql.NullBool {
	if !flag.Valid {
		return sql.NullBool{}
	}
	return sql.NullBool{Bool: flag.Int64 == 0, Valid: true}
}

func (db *InterBaseDBRepository) DescribeViews(ctx context.Context) ([]*ViewDesc, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	views, err := catalog.Views(ctx, "")
	if err != nil {
		return nil, err
	}

	return db.viewDescriptions(views), nil
}

func (db *InterBaseDBRepository) viewDescriptions(views []schema.Relation) []*ViewDesc {
	noPrimaryKeys := map[string]struct{}{}
	result := make([]*ViewDesc, 0, len(views))
	for _, view := range views {
		columns := make([]*ColumnDesc, 0, len(view.Columns))
		for _, column := range view.Columns {
			columns = append(columns, db.columnDescription(view.Name, column, noPrimaryKeys))
		}
		result = append(result, &ViewDesc{Schema: "", Name: view.Name, OwnerName: view.OwnerName, ViewSource: view.ViewSource, Description: view.Description, Columns: columns})
	}
	return result
}

func (db *InterBaseDBRepository) DescribeGenerators(ctx context.Context) ([]*GeneratorDesc, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	generators, err := catalog.Generators(ctx, "")
	if err != nil {
		return nil, err
	}
	result := make([]*GeneratorDesc, 0, len(generators))
	for _, generator := range generators {
		result = append(result, &GeneratorDesc{Schema: "", Name: generator.Name, ID: generator.ID})
	}
	return result, nil
}

func (db *InterBaseDBRepository) DescribeDomains(ctx context.Context) ([]*DomainDesc, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	domains, err := catalog.Domains(ctx, "")
	if err != nil {
		return nil, err
	}
	result := make([]*DomainDesc, 0, len(domains))
	for index := range domains {
		domain := &domains[index]
		result = append(result, &DomainDesc{
			Schema: "",
			Name:   domain.Name,
			// The full rendering: a domain declaration is where the charset
			// and collation belong, and DDL reproduces it verbatim.
			Type:             interBaseTypeName(domain, db.SQLDialect),
			Nullable:         domain.Nullable,
			DefaultSource:    domain.DefaultSource,
			ValidationSource: domain.ValidationSource,
			CharacterSetName: domain.CharacterSetName,
			CollationName:    domain.CollationName,
			Description:      domain.Description,
		})
	}
	return result, nil
}

func (db *InterBaseDBRepository) DescribeIndexes(ctx context.Context) ([]*IndexDesc, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	indexes, err := catalog.Indexes(ctx, "")
	if err != nil {
		return nil, err
	}
	return db.indexDescriptions(indexes), nil
}

func (db *InterBaseDBRepository) indexDescriptions(indexes []schema.Index) []*IndexDesc {
	result := make([]*IndexDesc, 0, len(indexes))
	for _, index := range indexes {
		columns := make([]string, 0, len(index.Segments))
		for _, segment := range index.Segments {
			columns = append(columns, segment.FieldName)
		}
		result = append(result, &IndexDesc{
			Schema:         "",
			Name:           index.Name,
			RelationName:   index.RelationName,
			Columns:        columns,
			Expression:     index.Expression,
			Unique:         interBaseFlagIsSet(index.UniqueFlag),
			Active:         interBaseFlagIsClear(index.Inactive),
			ConstraintName: index.ConstraintName,
			Description:    index.Description,
		})
	}
	return result
}

var _ CatalogRepository = (*InterBaseDBRepository)(nil)

func (db *InterBaseDBRepository) DescribeProcedures(ctx context.Context) ([]*ProcedureDesc, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	procedures, err := catalog.Procedures(ctx, "")
	if err != nil {
		return nil, err
	}
	result := make([]*ProcedureDesc, 0, len(procedures))
	for _, procedure := range procedures {
		result = append(result, &ProcedureDesc{
			Schema:           "",
			Name:             procedure.Name,
			OwnerName:        procedure.OwnerName,
			Source:           procedure.Source,
			Description:      procedure.Description,
			InputParameters:  db.parameterDescriptions(procedure.InputParameters, ParameterInput),
			OutputParameters: db.parameterDescriptions(procedure.OutputParameters, ParameterOutput),
		})
	}
	return result, nil
}

// parameterDescriptions maps one direction's parameters, already ordered by
// RDB$PARAMETER_NUMBER by the catalog query. Type keeps the full rendering:
// a parameter is a declaration, and when Domain is empty the inline type is
// exactly what the user wrote, charset included.
func (db *InterBaseDBRepository) parameterDescriptions(parameters []schema.ProcedureParameter, direction ParameterDirection) []*ProcedureParameterDesc {
	result := make([]*ProcedureParameterDesc, 0, len(parameters))
	for index := range parameters {
		parameter := &parameters[index]
		position := index
		if parameter.Number.Valid {
			position = int(parameter.Number.Int64)
		}
		result = append(result, &ProcedureParameterDesc{
			Name:        parameter.Name,
			Position:    position,
			Direction:   direction,
			Type:        interBaseTypeName(parameter.Domain, db.SQLDialect),
			Domain:      interBaseUserDomainName(parameter.FieldSource, parameter.Domain),
			Nullable:    parameter.Nullable,
			Description: parameter.Description,
		})
	}
	return result
}

// interBaseUserDomainName implements the user-versus-system domain test from
// §4.4. A parameter declared with an inline type gets a system-generated RDB$
// domain that must not be shown as a domain reference, so the name is reported
// only when both conditions hold: it is non-empty and does not begin with
// "RDB$" (case insensitively, after right-trimming catalog padding), and the
// domain's SystemFlag is NULL or 0.
//
// The driver's own predicate, userDomainReference (schema/ddl.go), is
// unexported and the companion driver spec declined to export it, so this is a
// deliberate second copy of a two-condition rule rather than drift. If the
// driver ever exports it, delete this and call the exported form.
func interBaseUserDomainName(fieldSource sql.NullString, domain *schema.Domain) string {
	if !fieldSource.Valid {
		return ""
	}
	name := strings.TrimRight(fieldSource.String, " ")
	if name == "" {
		return ""
	}
	if len(name) >= len("RDB$") && strings.EqualFold(name[:len("RDB$")], "RDB$") {
		return ""
	}
	if domain != nil && domain.SystemFlag.Valid && domain.SystemFlag.Int64 != 0 {
		return ""
	}
	return name
}

// interBaseOptionalRendering applies the degradation rule the catalog
// accessors need. ErrUnsupportedDDL from Trigger.Event,
// FunctionArgument.SQLType and Function.ReturnType is a normal, expected
// result rather than a failure — RDB$CHARACTER_LENGTH is never populated for
// function arguments, so every CHAR and VARCHAR argument reaches it during
// ordinary operation. It maps to "" for that one field and the catalog build
// carries on: it never aborts, never propagates to
// DescribeFunctions/DescribeTriggers, and never drops the surrounding
// descriptor. Any other error is a real catalog fault and is returned.
func interBaseOptionalRendering(rendered string, err error) (string, error) {
	if err == nil {
		return rendered, nil
	}
	if errors.Is(err, schema.ErrUnsupportedDDL) {
		return "", nil
	}
	return "", err
}

func (db *InterBaseDBRepository) DescribeTriggers(ctx context.Context) ([]*TriggerDesc, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	triggers, err := catalog.Triggers(ctx, "")
	if err != nil {
		return nil, err
	}
	result := make([]*TriggerDesc, 0, len(triggers))
	for _, trigger := range triggers {
		event, err := interBaseOptionalRendering(trigger.Event())
		if err != nil {
			return nil, err
		}
		result = append(result, &TriggerDesc{
			Schema:       "",
			Name:         trigger.Name,
			RelationName: trigger.RelationName,
			// Taken verbatim: a multi-event trigger yields one joined string
			// such as "BEFORE INSERT OR UPDATE". sqls does not split it.
			Event:       event,
			Sequence:    trigger.Sequence,
			Active:      interBaseFlagIsClear(trigger.Inactive),
			Source:      trigger.Source,
			Description: trigger.Description,
		})
	}
	return result, nil
}

func (db *InterBaseDBRepository) DescribeFunctions(ctx context.Context) ([]*FunctionDesc, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	functions, err := catalog.Functions(ctx, "")
	if err != nil {
		return nil, err
	}
	result := make([]*FunctionDesc, 0, len(functions))
	for _, function := range functions {
		arguments := make([]*FunctionArgumentDesc, 0, len(function.Arguments))
		for _, argument := range function.Arguments {
			argumentType, err := interBaseOptionalRendering(argument.SQLType())
			if err != nil {
				return nil, err
			}
			arguments = append(arguments, &FunctionArgumentDesc{
				Name:     argument.Name,
				Position: argument.Position,
				Type:     argumentType,
			})
		}
		// RDB$RETURN_ARGUMENT is an argument position, not an index into
		// Arguments; the driver resolves it, so sqls never indexes the slice
		// with it. When the position names an input argument, that argument
		// legitimately appears both here and in Arguments.
		returnType, err := interBaseOptionalRendering(function.ReturnType())
		if err != nil {
			return nil, err
		}
		result = append(result, &FunctionDesc{
			Schema:         "",
			Name:           function.Name,
			ReturnType:     returnType,
			ReturnPosition: function.ReturnArgument,
			Arguments:      arguments,
			ModuleName:     function.ModuleName,
			EntryPoint:     function.EntryPoint,
			Description:    function.Description,
		})
	}
	return result, nil
}

var _ CatalogSnapshotRepository = (*InterBaseDBRepository)(nil)

// CatalogSnapshot returns a read-only repository bound to one transaction that
// has already read every relation name, column, primary-key field and
// foreign-key field mapping the cache shows. A whole cache build then costs
// the four bulk statements of interbase_cache_bulk.go, whatever the schema's
// size, instead of a query per relation and several per constraint.
//
// The returned repository is new. The receiver is never mutated, because
// ReCache runs on a handler goroutine while the worker's secondary pass runs
// on its own; a shared mutable snapshot field would race.
func (db *InterBaseDBRepository) CatalogSnapshot(ctx context.Context) (DBRepository, func() error, error) {
	if db == nil || db.Conn == nil {
		return nil, nil, errors.New("interbase: database connection is nil")
	}
	if db.snapshot != nil {
		// Already bound; nesting would open a second transaction for nothing.
		return db, func() error { return nil }, nil
	}

	tx, err := db.Conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	catalog := schema.New(tx)
	// Either the whole catalog or none of it: a failed read rolls the
	// transaction back and returns, so no partially loaded snapshot is ever
	// served.
	bulk, err := loadInterBaseBulkCatalog(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return nil, nil, err
	}

	bound := &InterBaseDBRepository{
		Conn:             db.Conn,
		SQLDialect:       db.SQLDialect,
		SourceSQLDialect: db.SourceSQLDialect,
		DatabaseName:     db.DatabaseName,
		snapshot: &interBaseCatalogSnapshot{
			catalog: catalog,
			bulk:    bulk,
		},
	}
	// The snapshot is read-only, so rolling back is the whole of closing it.
	return bound, tx.Rollback, nil
}

// interBaseForeignKeyMapping is one foreign key's field pairing: the two
// column lists are the same length and are paired by position, which is the
// index segment position in both indexes. It is the single shape both the
// snapshot's bulk read and the direct constraint read render from, so the two
// cannot drift apart.
type interBaseForeignKeyMapping struct {
	relationName           string
	referencedRelationName string
	columns                []string
	referencedColumns      []string
}

// interBaseForeignKeys renders field mappings onto the shared descriptor.
// A mapping whose enforcing or referenced fields could not be resolved is
// dropped whole, rather than returning half of a foreign key.
func interBaseForeignKeys(mappings []interBaseForeignKeyMapping) []*ForeignKey {
	foreignKeys := make([]*ForeignKey, 0, len(mappings))
	for _, mapping := range mappings {
		if len(mapping.columns) == 0 || len(mapping.columns) != len(mapping.referencedColumns) {
			continue
		}
		foreignKey := new(ForeignKey)
		for i, column := range mapping.columns {
			left := &ColumnBase{Schema: "", Table: mapping.relationName, Name: column}
			right := &ColumnBase{Schema: "", Table: mapping.referencedRelationName, Name: mapping.referencedColumns[i]}
			*foreignKey = append(*foreignKey, [2]*ColumnBase{left, right})
		}
		foreignKeys = append(foreignKeys, foreignKey)
	}
	return foreignKeys
}

func (db *InterBaseDBRepository) DescribeForeignKeysBySchema(ctx context.Context, _ string) ([]*ForeignKey, error) {
	if bulk := db.bulkCatalog(); bulk != nil {
		return interBaseForeignKeys(bulk.foreignKeys), nil
	}

	constraints, err := db.constraints(ctx)
	if err != nil {
		return nil, err
	}

	mappings := make([]interBaseForeignKeyMapping, 0)
	for _, constraint := range constraints {
		if !strings.EqualFold(strings.TrimSpace(constraint.ConstraintType), string(schema.ConstraintForeignKey)) {
			continue
		}
		mappings = append(mappings, interBaseForeignKeyMapping{
			relationName:           constraint.RelationName,
			referencedRelationName: constraint.ReferencedRelationName,
			columns:                constraint.Columns,
			referencedColumns:      constraint.ReferencedColumns,
		})
	}
	return interBaseForeignKeys(mappings), nil
}
