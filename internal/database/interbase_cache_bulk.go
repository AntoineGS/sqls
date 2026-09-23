package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"interbase-go/schema"
)

// Bulk catalog loading for the completion cache.
//
// Measured on 2026-09-22 against NRF01 (1,565 relations, 7,291 constraints)
// through this driver and one read-only transaction, the driver's
// general-purpose catalog APIs cost 98.076 s over 10,295 statements for a
// single cache build: schema.Relations issues one column query per relation,
// and schema.Constraints resolves each constraint's enforcing index, its
// referenced index and both sets of index segments one statement at a time.
// Evidence and the captured SQL: ~/.local/share/sqls/catalog-loading/.
//
// The completion cache needs a far narrower answer than those APIs return:
// relation names, one ColumnDesc per column, which columns are primary-key
// fields, and foreign-key field pairs. That is four SELECTs whose count does
// not depend on how many relations or constraints the database holds. The
// primary-key and foreign-key statements follow the bulk loaders MDExplorer
// has used in production (Common/MetadataProvider.InterBase.pas LoadAllPKs
// line 642, LoadAllFKs line 733), which took 65 ms and 105 ms against the same
// database and transaction.
//
// This replaces nothing else: the standalone repository path and every
// extended catalog accessor still read through the driver's schema package.
// Only a CatalogSnapshot serves its cache build from here.
//
// Every identifier column below (relation, field, domain, constraint,
// character-set and collation names) is wrapped in
// CAST(... AS VARCHAR(interBaseBulkIdentifierCastWidth)). This is a targeted
// bypass of a confirmed native driver defect, not a driver fix: the native
// SQL_TEXT decoder caps a CHAR/UNICODE_FSS column's returned length at
// declaredByteWidth/3 regardless of the value's real length, silently
// truncating any catalog identifier past ~22 characters and, in the worst
// case, merging two distinct constraint names that share a 22-byte prefix.
// See /tmp/opencode/sqls-bulk/native-diagnosis.md for the SQLDA evidence and
// /tmp/opencode/sqls-bulk/driver-followup.md for why deleting the driver's
// cap outright is rejected (it would break the CHAR-padding guarantee
// interbase-go/tests/native_values_test.c:204-243 already pins). The
// driver's SQL_VARYING path takes the wire-provided length prefix as
// authoritative with no charset-width recomputation, so casting to VARCHAR
// here reaches that already-correct path instead. interBaseBulkIdentifierCastWidth
// (67) is the live-confirmed byte width of NRF01's relation-name domain
// (RDB$FIELDS: RDB$FIELD_LENGTH=67, RDB$CHARACTER_SET_ID=3/UNICODE_FSS).
// Comparing the complete cache to a full-name reference also verified this
// projection on NRF01 and centrale. A different InterBase catalog with
// identifiers wider than 67 bytes needs a catalog-derived cast width before
// using this loader, or the snapshot can fail on an over-length identifier.
// This does not fix the driver: the standalone repository and every extended catalog
// accessor still read CHAR identifiers through the driver's unpatched
// SQL_TEXT decode and remain subject to the same truncation.
const interBaseBulkIdentifierCastWidth = 67

func interBaseBulkRelationsQueryForWidth(width int) string {
	return fmt.Sprintf(`
SELECT %s
FROM RDB$RELATIONS r
WHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0
ORDER BY r.RDB$RELATION_NAME`, interBaseMetadataIdentifier("r.RDB$RELATION_NAME", width))
}

var interBaseBulkRelationsQuery = interBaseBulkRelationsQueryForWidth(interBaseBulkIdentifierCastWidth)

// interBaseBulkColumnsQuery is the driver's per-relation column projection
// restricted to the fields ColumnDesc renders, and widened to every user
// relation at once. The join onto RDB$RELATIONS applies the same system-flag
// filter as the relation list, so a system relation's columns never appear.
//
// The driver also reads the column's own collation (its rco join); ColumnDesc
// renders the domain's type alone, so that join is left out rather than paid
// for on every column of every relation.
func interBaseBulkColumnsQueryForWidth(width int) string {
	return fmt.Sprintf(`
SELECT %s, %s, %s,
       rf.RDB$NULL_FLAG, rf.RDB$DEFAULT_SOURCE,
       %s, f.RDB$COMPUTED_SOURCE, f.RDB$DEFAULT_SOURCE,
       f.RDB$FIELD_LENGTH, f.RDB$FIELD_SCALE, f.RDB$FIELD_TYPE,
       f.RDB$FIELD_SUB_TYPE, f.RDB$SEGMENT_LENGTH, f.RDB$DIMENSIONS,
       f.RDB$NULL_FLAG, f.RDB$CHARACTER_LENGTH, f.RDB$COLLATION_ID,
       f.RDB$CHARACTER_SET_ID, f.RDB$FIELD_PRECISION,
       %s, %s
FROM RDB$RELATION_FIELDS rf
JOIN RDB$RELATIONS r ON r.RDB$RELATION_NAME = rf.RDB$RELATION_NAME
LEFT JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE
LEFT JOIN RDB$CHARACTER_SETS cs ON cs.RDB$CHARACTER_SET_ID = f.RDB$CHARACTER_SET_ID
LEFT JOIN RDB$COLLATIONS co ON co.RDB$CHARACTER_SET_ID = f.RDB$CHARACTER_SET_ID
                           AND co.RDB$COLLATION_ID = f.RDB$COLLATION_ID
WHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0
ORDER BY rf.RDB$RELATION_NAME, rf.RDB$FIELD_POSITION`,
		interBaseMetadataIdentifier("rf.RDB$RELATION_NAME", width),
		interBaseMetadataIdentifier("rf.RDB$FIELD_NAME", width),
		interBaseMetadataIdentifier("rf.RDB$FIELD_SOURCE", width),
		interBaseMetadataIdentifier("f.RDB$FIELD_NAME", width),
		interBaseMetadataIdentifier("cs.RDB$CHARACTER_SET_NAME", width),
		interBaseMetadataIdentifier("co.RDB$COLLATION_NAME", width))
}

var interBaseBulkColumnsQuery = interBaseBulkColumnsQueryForWidth(interBaseBulkIdentifierCastWidth)

// interBaseBulkPrimaryKeyFieldsQuery returns one row per primary-key field.
// The key flag is a membership test, so the fields need no ordering; the
// enforcing index supplies them exactly as the driver's per-constraint index
// lookup did.
//
// The join onto RDB$INDICES pi filters the enforcing index's own
// RDB$SYSTEM_FLAG, matching schema.Catalog.Index() (interbase-go's
// catalog_extended.go), which the bulk read previously did not check
// (review.md residual concern 1). Unreachable through normal DDL — InterBase
// itself never flags a user relation's own index system — but cheap to close
// and closes the one documented divergence from the loader it replaces.
func interBaseBulkPrimaryKeyFieldsQueryForWidth(width int) string {
	return fmt.Sprintf(`
SELECT %s, %s
FROM RDB$RELATION_CONSTRAINTS pk
JOIN RDB$RELATIONS r ON r.RDB$RELATION_NAME = pk.RDB$RELATION_NAME
JOIN RDB$INDICES pi ON pi.RDB$INDEX_NAME = pk.RDB$INDEX_NAME
JOIN RDB$INDEX_SEGMENTS s ON s.RDB$INDEX_NAME = pk.RDB$INDEX_NAME
WHERE pk.RDB$CONSTRAINT_TYPE = 'PRIMARY KEY'
  AND COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0
  AND COALESCE(pi.RDB$SYSTEM_FLAG, 0) = 0`,
		interBaseMetadataIdentifier("pk.RDB$RELATION_NAME", width),
		interBaseMetadataIdentifier("s.RDB$FIELD_NAME", width))
}

var interBaseBulkPrimaryKeyFieldsQuery = interBaseBulkPrimaryKeyFieldsQueryForWidth(interBaseBulkIdentifierCastWidth)

// interBaseBulkForeignKeyFieldsQuery returns one row per foreign-key field,
// paired with the referenced field at the same index segment position. That
// pairing is MDExplorer's; the referenced index is reached the way the driver
// reaches it, through RDB$REF_CONSTRAINTS and the referenced constraint, so
// the referenced relation name is the one the replaced code path reported.
//
// The referenced segment join is a LEFT JOIN so that an enforcing field with
// no counterpart is visible to the loader, which then drops the whole
// constraint rather than reporting half a foreign key.
//
// The joins onto RDB$INDICES fi and pi filter both the enforcing and the
// referenced index's own RDB$SYSTEM_FLAG, matching schema.Catalog.Index()
// (review.md residual concern 1). pi is an inner JOIN, not LEFT: rc always
// names a real unique/primary-key constraint with a real index, so a missing
// pi row means a corrupt catalog, the same "drop the whole key" stance the
// query already takes for a missing enforcing index.
func interBaseBulkForeignKeyFieldsQueryForWidth(width int) string {
	return fmt.Sprintf(`
SELECT %s, %s, %s,
       %s, %s
FROM RDB$RELATION_CONSTRAINTS fk
JOIN RDB$RELATIONS r ON r.RDB$RELATION_NAME = fk.RDB$RELATION_NAME
JOIN RDB$INDICES fi ON fi.RDB$INDEX_NAME = fk.RDB$INDEX_NAME
JOIN RDB$REF_CONSTRAINTS rc ON rc.RDB$CONSTRAINT_NAME = fk.RDB$CONSTRAINT_NAME
JOIN RDB$RELATION_CONSTRAINTS pk ON pk.RDB$CONSTRAINT_NAME = rc.RDB$CONST_NAME_UQ
JOIN RDB$INDICES pi ON pi.RDB$INDEX_NAME = pk.RDB$INDEX_NAME
JOIN RDB$INDEX_SEGMENTS fs ON fs.RDB$INDEX_NAME = fk.RDB$INDEX_NAME
LEFT JOIN RDB$INDEX_SEGMENTS ps ON ps.RDB$INDEX_NAME = pk.RDB$INDEX_NAME
                               AND ps.RDB$FIELD_POSITION = fs.RDB$FIELD_POSITION
WHERE fk.RDB$CONSTRAINT_TYPE = 'FOREIGN KEY'
  AND COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0
  AND COALESCE(fi.RDB$SYSTEM_FLAG, 0) = 0
  AND COALESCE(pi.RDB$SYSTEM_FLAG, 0) = 0
ORDER BY fk.RDB$CONSTRAINT_NAME, fs.RDB$FIELD_POSITION`,
		interBaseMetadataIdentifier("fk.RDB$CONSTRAINT_NAME", width),
		interBaseMetadataIdentifier("fk.RDB$RELATION_NAME", width),
		interBaseMetadataIdentifier("fs.RDB$FIELD_NAME", width),
		interBaseMetadataIdentifier("pk.RDB$RELATION_NAME", width),
		interBaseMetadataIdentifier("ps.RDB$FIELD_NAME", width))
}

var interBaseBulkForeignKeyFieldsQuery = interBaseBulkForeignKeyFieldsQueryForWidth(interBaseBulkIdentifierCastWidth)

// interBaseBulkRelation is one relation with its ordered columns, in the shape
// columnDescription already consumes. A relation with no columns keeps its
// entry: it still completes in FROM position.
type interBaseBulkRelation struct {
	name    string
	columns []schema.Column
}

// interBaseBulkCatalog is everything a cache build reads, loaded by
// loadInterBaseBulkCatalog in a fixed number of statements. Its fields are
// never handed out directly; the repository renders a fresh descriptor per
// call so a caller cannot mutate one build's answer into the next one's.
type interBaseBulkCatalog struct {
	relations []interBaseBulkRelation
	// primaryKeys is keyed by interBaseRelationColumnKey, matching the map
	// interBasePrimaryKeyColumns builds from schema.Constraint.
	primaryKeys map[string]struct{}
	foreignKeys []interBaseForeignKeyMapping
}

// relationNames returns every user table and view in catalog order. The slice
// is freshly allocated because DBCache sorts the names it is given in place
// (DBCache.SortedTablesByDBName).
func (c *interBaseBulkCatalog) relationNames() []string {
	names := make([]string, 0, len(c.relations))
	for _, relation := range c.relations {
		names = append(names, relation.name)
	}
	return names
}

// columnCount is the number of descriptors a full column read produces, so
// that read allocates once.
func (c *interBaseBulkCatalog) columnCount() int {
	total := 0
	for _, relation := range c.relations {
		total += len(relation.columns)
	}
	return total
}

// loadInterBaseBulkCatalog reads the whole cache-visible catalog through
// queryer, which is the snapshot's read-only transaction. Every statement must
// succeed: a caller gets the complete catalog or an error, never a partial one.
func loadInterBaseBulkCatalog(ctx context.Context, queryer schema.Queryer) (*interBaseBulkCatalog, error) {
	if queryer == nil {
		return nil, errors.New("interbase: catalog query source is nil")
	}

	relations, err := interBaseBulkRelations(ctx, queryer)
	if err != nil {
		return nil, err
	}
	if err := interBaseBulkLoadColumns(ctx, queryer, relations); err != nil {
		return nil, err
	}
	primaryKeys, err := interBaseBulkPrimaryKeys(ctx, queryer)
	if err != nil {
		return nil, err
	}
	foreignKeys, err := interBaseBulkForeignKeys(ctx, queryer)
	if err != nil {
		return nil, err
	}

	return &interBaseBulkCatalog{
		relations:   relations,
		primaryKeys: primaryKeys,
		foreignKeys: foreignKeys,
	}, nil
}

// interBaseBulkQuery runs one bulk statement and hands every row to scan. It
// owns the close and iteration error handling, so a read that fails part way
// returns an error instead of the rows it managed to scan.
func interBaseBulkQuery(ctx context.Context, queryer schema.Queryer, what, query string, scan func(*sql.Rows) error) error {
	rows, err := queryer.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("interbase: query %s: %w", what, err)
	}
	if rows == nil {
		return fmt.Errorf("interbase: query %s returned no rows handle", what)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		if err := scan(rows); err != nil {
			return fmt.Errorf("interbase: scan %s: %w", what, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("interbase: read %s: %w", what, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("interbase: close %s: %w", what, err)
	}
	return nil
}

func interBaseBulkRelations(ctx context.Context, queryer schema.Queryer) ([]interBaseBulkRelation, error) {
	return interBaseBulkRelationsForQuery(ctx, queryer, interBaseBulkRelationsQuery)
}

// interBaseBulkLoadColumns fills in each relation's columns from one read of
// every user relation's fields. Rows arrive grouped by relation and ordered by
// RDB$FIELD_POSITION, and are appended to the relation they name, so column
// order is the catalog's rather than the scan's.
func interBaseBulkLoadColumns(ctx context.Context, queryer schema.Queryer, relations []interBaseBulkRelation) error {
	positions := make(map[string]int, len(relations))
	for index, relation := range relations {
		positions[relation.name] = index
	}
	return interBaseBulkScanColumns(ctx, queryer, interBaseBulkIdentifierCastWidth, func(relationName string, column schema.Column) error {
		index, ok := positions[relationName]
		if !ok {
			// The relation list was read first. A column naming a relation it
			// does not contain means the relation disappeared between the two
			// statements; reporting the column would invent a table.
			return nil
		}
		relations[index].columns = append(relations[index].columns, column)
		return nil
	})
}

// interBaseBulkScanColumns reads the common column projection and delegates
// each fully validated row to consume. Independent metadata jobs and the
// legacy whole-catalog snapshot share this scan so descriptor inputs cannot
// drift.
func interBaseBulkScanColumns(ctx context.Context, queryer schema.Queryer, width int, consume func(string, schema.Column) error) error {
	return interBaseBulkScanColumnsWithQuery(ctx, queryer, interBaseBulkColumnsQueryForWidth(width), consume)
}

func interBaseBulkScanColumnsWithQuery(ctx context.Context, queryer schema.Queryer, query string, consume func(string, schema.Column) error) error {
	return interBaseBulkQuery(ctx, queryer, "columns", query, func(rows *sql.Rows) error {
		var raw interBaseBulkColumnRow
		if err := rows.Scan(raw.destinations()...); err != nil {
			return err
		}
		relationName, column, err := raw.column()
		if err != nil {
			return err
		}
		return consume(relationName, column)
	})
}

// interBaseBulkPrimaryKeys indexes every primary-key field of every user
// relation. The map is the one columnDescription consults for the key flag.
func interBaseBulkPrimaryKeys(ctx context.Context, queryer schema.Queryer) (map[string]struct{}, error) {
	primaryKeys := make(map[string]struct{})
	err := interBaseBulkScanPrimaryKeyFields(ctx, queryer, interBaseBulkIdentifierCastWidth, func(relation, field string) error {
		primaryKeys[interBaseRelationColumnKey(relation, field)] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return primaryKeys, nil
}

func interBaseBulkScanPrimaryKeyFields(ctx context.Context, queryer schema.Queryer, width int, consume func(string, string) error) error {
	return interBaseBulkQuery(ctx, queryer, "primary key fields", interBaseBulkPrimaryKeyFieldsQueryForWidth(width), func(rows *sql.Rows) error {
		var relationName, fieldName sql.NullString
		if err := rows.Scan(&relationName, &fieldName); err != nil {
			return err
		}
		relation, err := interBaseRequiredName(relationName, "primary key relation name")
		if err != nil {
			return err
		}
		field, err := interBaseRequiredName(fieldName, "primary key field name")
		if err != nil {
			return err
		}
		return consume(relation, field)
	})
}

// interBaseBulkForeignKeys groups foreign-key field rows into one mapping per
// constraint, in constraint-name order, preserving segment order within each.
//
// Grouping is by the constraint's full identity — its relation and its name —
// rather than by a concatenation that two differently split names could both
// produce. A constraint whose referenced index has no field at some position
// is dropped whole: half a foreign key would complete a join with a column
// that does not participate in it.
func interBaseBulkForeignKeys(ctx context.Context, queryer schema.Queryer) ([]interBaseForeignKeyMapping, error) {
	return interBaseBulkForeignKeysWithQuery(ctx, queryer, interBaseBulkForeignKeyFieldsQuery)
}

func interBaseBulkForeignKeysWithQuery(ctx context.Context, queryer schema.Queryer, query string) ([]interBaseForeignKeyMapping, error) {
	type constraintIdentity struct {
		relationName   string
		constraintName string
	}

	order := make([]constraintIdentity, 0)
	mappings := make(map[constraintIdentity]*interBaseForeignKeyMapping)
	incomplete := make(map[constraintIdentity]struct{})

	err := interBaseBulkQuery(ctx, queryer, "foreign key fields", query, func(rows *sql.Rows) error {
		var constraintName, relationName, fieldName, referencedRelation, referencedField sql.NullString
		if err := rows.Scan(&constraintName, &relationName, &fieldName, &referencedRelation, &referencedField); err != nil {
			return err
		}
		constraint, err := interBaseRequiredName(constraintName, "foreign key constraint name")
		if err != nil {
			return err
		}
		relation, err := interBaseRequiredName(relationName, "foreign key relation name")
		if err != nil {
			return err
		}
		field, err := interBaseRequiredName(fieldName, "foreign key field name")
		if err != nil {
			return err
		}

		identity := constraintIdentity{relationName: relation, constraintName: constraint}
		mapping, ok := mappings[identity]
		if !ok {
			mapping = &interBaseForeignKeyMapping{relationName: relation}
			mappings[identity] = mapping
			order = append(order, identity)
		}

		// An unmatched referenced segment is the catalog saying this pairing
		// cannot be resolved, not a NULL field name.
		if !referencedField.Valid || !referencedRelation.Valid {
			incomplete[identity] = struct{}{}
			return nil
		}
		referenced, err := interBaseRequiredName(referencedRelation, "referenced relation name")
		if err != nil {
			return err
		}
		referencedName, err := interBaseRequiredName(referencedField, "referenced field name")
		if err != nil {
			return err
		}
		if mapping.referencedRelationName == "" {
			mapping.referencedRelationName = referenced
		}
		mapping.columns = append(mapping.columns, field)
		mapping.referencedColumns = append(mapping.referencedColumns, referencedName)
		return nil
	})
	if err != nil {
		return nil, err
	}

	foreignKeys := make([]interBaseForeignKeyMapping, 0, len(order))
	for _, identity := range order {
		if _, partial := incomplete[identity]; partial {
			continue
		}
		mapping := mappings[identity]
		if len(mapping.columns) == 0 || len(mapping.columns) != len(mapping.referencedColumns) {
			continue
		}
		foreignKeys = append(foreignKeys, *mapping)
	}
	return foreignKeys, nil
}

// interBaseBulkColumnRow is one row of interBaseBulkColumnsQuery. Its fields
// are exactly what ColumnDesc rendering reads: the type through
// interBaseColumnTypeName, nullability through interBaseNullability, the
// default through interBaseEffectiveDefault and the COMPUTED marker through
// interBaseIsComputed.
type interBaseBulkColumnRow struct {
	relationName  sql.NullString
	fieldName     sql.NullString
	fieldSource   sql.NullString
	nullFlag      sql.NullInt64
	defaultSource sql.NullString

	domainName            sql.NullString
	domainComputedSource  sql.NullString
	domainDefaultSource   sql.NullString
	domainFieldLength     sql.NullInt64
	domainFieldScale      sql.NullInt64
	domainFieldType       sql.NullInt64
	domainFieldSubType    sql.NullInt64
	domainSegmentLength   sql.NullInt64
	domainDimensions      sql.NullInt64
	domainNullFlag        sql.NullInt64
	domainCharacterLength sql.NullInt64
	domainCollationID     sql.NullInt64
	domainCharacterSetID  sql.NullInt64
	domainFieldPrecision  sql.NullInt64
	characterSetName      sql.NullString
	collationName         sql.NullString
}

func (r *interBaseBulkColumnRow) destinations() []any {
	return []any{
		&r.relationName, &r.fieldName, &r.fieldSource, &r.nullFlag, &r.defaultSource,
		&r.domainName, &r.domainComputedSource, &r.domainDefaultSource,
		&r.domainFieldLength, &r.domainFieldScale, &r.domainFieldType,
		&r.domainFieldSubType, &r.domainSegmentLength, &r.domainDimensions,
		&r.domainNullFlag, &r.domainCharacterLength, &r.domainCollationID,
		&r.domainCharacterSetID, &r.domainFieldPrecision,
		&r.characterSetName, &r.collationName,
	}
}

// column maps the row onto the driver's own column type, following the
// driver's scanColumn: a domain is attached only when the field source names
// one and that domain row exists, and the column's computed source is the
// domain's, because InterBase stores COMPUTED BY on RDB$FIELDS.
func (r *interBaseBulkColumnRow) column() (string, schema.Column, error) {
	relationName, err := interBaseRequiredName(r.relationName, "column relation name")
	if err != nil {
		return "", schema.Column{}, err
	}
	name, err := interBaseRequiredName(r.fieldName, "column name")
	if err != nil {
		return "", schema.Column{}, err
	}

	column := schema.Column{
		Name:          name,
		RelationName:  relationName,
		FieldSource:   interBaseTrimmedName(r.fieldSource),
		NullFlag:      r.nullFlag,
		DefaultSource: r.defaultSource,
	}
	if !r.fieldSource.Valid || strings.TrimRight(r.fieldSource.String, " ") == "" || !r.domainName.Valid {
		return relationName, column, nil
	}

	domainName, err := interBaseRequiredName(r.domainName, "domain name")
	if err != nil {
		return "", schema.Column{}, err
	}
	column.Domain = &schema.Domain{
		Name:             domainName,
		ComputedSource:   r.domainComputedSource,
		DefaultSource:    r.domainDefaultSource,
		FieldLength:      r.domainFieldLength,
		FieldScale:       r.domainFieldScale,
		FieldType:        r.domainFieldType,
		FieldSubType:     r.domainFieldSubType,
		SegmentLength:    r.domainSegmentLength,
		Dimensions:       r.domainDimensions,
		NullFlag:         r.domainNullFlag,
		CharacterLength:  r.domainCharacterLength,
		CollationID:      r.domainCollationID,
		CharacterSetID:   r.domainCharacterSetID,
		FieldPrecision:   r.domainFieldPrecision,
		CharacterSetName: interBaseTrimmedName(r.characterSetName),
		CollationName:    interBaseTrimmedName(r.collationName),
	}
	column.ComputedSource = column.Domain.ComputedSource
	return relationName, column, nil
}

// interBaseRequiredName applies the driver's identifier rule: catalog padding
// is trailing blanks, and a name that is NULL or blank is a catalog fault
// rather than an object without a name.
func interBaseRequiredName(value sql.NullString, label string) (string, error) {
	if !value.Valid {
		return "", fmt.Errorf("interbase: %s is NULL", label)
	}
	name := strings.TrimRight(value.String, " ")
	if name == "" {
		return "", fmt.Errorf("interbase: %s is empty", label)
	}
	return name, nil
}

func interBaseTrimmedName(value sql.NullString) sql.NullString {
	if value.Valid {
		value.String = strings.TrimRight(value.String, " ")
	}
	return value
}
