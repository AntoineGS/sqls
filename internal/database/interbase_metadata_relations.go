package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"interbase-go/schema"
)

func (db *InterBaseDBRepository) readMetadataViews(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
	views := make([]schema.Relation, 0)
	byName := make(map[string]int)
	headerQuery := fmt.Sprintf(`
SELECT %s, %s, r.RDB$VIEW_SOURCE, r.RDB$DESCRIPTION
FROM RDB$RELATIONS r
WHERE r.RDB$VIEW_BLR IS NOT NULL
  AND COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0
ORDER BY r.RDB$RELATION_NAME`, interBaseMetadataIdentifier("r.RDB$RELATION_NAME", width), interBaseMetadataIdentifier("r.RDB$OWNER_NAME", width))
	err := interBaseBulkQuery(ctx, q, "view headers", headerQuery, func(rows *sql.Rows) error {
		var rawName, owner sql.NullString
		var view schema.Relation
		if err := rows.Scan(&rawName, &owner, &view.ViewSource, &view.Description); err != nil {
			return err
		}
		name, err := interBaseRequiredName(rawName, "view name")
		if err != nil {
			return err
		}
		view.Name, view.Kind = name, schema.RelationView
		view.OwnerName = trimInterBaseCatalogName(owner)
		if _, exists := byName[name]; exists {
			return fmt.Errorf("duplicate view header %q", name)
		}
		byName[name] = len(views)
		views = append(views, view)
		return nil
	})
	if err != nil {
		return MetadataPatch{}, err
	}
	query := interBaseBulkViewColumnsQueryForWidth(width)
	err = interBaseBulkScanColumnsWithQuery(ctx, q, query, func(relation string, column schema.Column) error {
		index, ok := byName[relation]
		if !ok {
			return fmt.Errorf("view column has no matching view header: %q", relation)
		}
		views[index].Columns = append(views[index].Columns, column)
		return nil
	})
	if err != nil {
		return MetadataPatch{}, err
	}
	descriptions := db.viewDescriptions(views)
	byDescription := make(map[string]*ViewDesc, len(descriptions))
	byID := make(map[string]*ViewDesc, len(descriptions))
	for _, description := range descriptions {
		byDescription[catalogCacheKey(description.Name)] = description
		byID[interBaseByIDKey(ObjectKindView, description.Name)] = description
	}
	return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Views: byDescription, ViewsByID: byID}}, Count: len(descriptions)}, nil
}

func trimInterBaseCatalogName(value sql.NullString) sql.NullString {
	if value.Valid {
		value.String = strings.TrimRight(value.String, " ")
	}
	return value
}

func (db *InterBaseDBRepository) readMetadataRelations(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
	query := interBaseBulkRelationsQueryForWidth(width)
	relations, err := interBaseBulkRelationsForQuery(ctx, q, query)
	if err != nil {
		return MetadataPatch{}, err
	}
	names := make([]string, 0, len(relations))
	for _, relation := range relations {
		names = append(names, relation.name)
	}
	return MetadataPatch{Cache: &DBCache{SchemaTables: map[string][]string{"": names}}, Count: len(names)}, nil
}

func (db *InterBaseDBRepository) readMetadataColumns(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
	columns := make([]*ColumnDesc, 0)
	err := interBaseBulkScanColumns(ctx, q, width, func(relation string, column schema.Column) error {
		desc := db.columnDescription(relation, column, nil)
		// PK metadata is a separate independently published category. An
		// absent PK result must not be represented as a definitive NO.
		desc.Key = ""
		columns = append(columns, desc)
		return nil
	})
	if err != nil {
		return MetadataPatch{}, err
	}
	// byID retains every relation's columns under its exact catalog identity
	// (see catalog.Cache.ColumnsByID) alongside genColumnMap's shared
	// upper-cased grouping: two exact-identity relations that fold to the
	// same case (a quoted "Foo" alongside FOO) must not merge into one
	// column list for a caller that has already resolved an exact identity.
	byID := make(map[string][]*ColumnDesc)
	for _, desc := range columns {
		key := interBaseByIDKey(ObjectKindTable, desc.Table)
		byID[key] = append(byID[key], desc)
	}
	return MetadataPatch{Cache: &DBCache{ColumnsWithParent: genColumnMap(columns), ColumnsByID: byID}, Count: len(columns)}, nil
}

// readMetadataViewsFromColumns reads only view headers via SQL; each view's
// columns come from cache — the already-loaded MetadataColumnsCurrent
// category, which reads every relation's columns, tables and views alike
// (see interBaseBulkColumnsQueryForWidth's lack of a view filter) — rather
// than issuing interBaseBulkViewColumnsQueryForWidth's second, redundant
// query for exactly the same data. This is the MetadataViews job's actual
// Run function; readMetadataViews above remains available standalone (used
// directly by existing tests and any caller without a coordinated Columns
// category to depend on) and still issues its own view-columns query.
//
// Column lookup goes through the exact-identity ColumnDescsByID, not the
// shared upper-cased ColumnDescs/ColumnDatabase: two exact-identity
// relations that fold to the same case must not merge into one view's
// column list, the same requirement a table detail read has.
//
// Key is always forced to "NO" (see interBaseViewColumnForDescription),
// matching the old standalone viewDescriptions path exactly: InterBase
// views are never primary-key-bearing in this cache's model, and
// ColumnsByID's cached descriptors carry whatever Key state the columns
// and primary-keys bulk categories happen to have merged by the moment
// this job was admitted -- "" before MetadataPrimaryKeys merges, "NO"
// after. That is a race between two independently scheduled bulk jobs,
// not real per-view data, so it must never leak into a published view
// column's Key.
func (db *InterBaseDBRepository) readMetadataViewsFromColumns(ctx context.Context, q schema.Queryer, width int, cache *DBCache) (MetadataPatch, error) {
	names := make([]string, 0)
	byName := make(map[string]*ViewDesc)
	headerQuery := fmt.Sprintf(`
SELECT %s, %s, r.RDB$VIEW_SOURCE, r.RDB$DESCRIPTION
FROM RDB$RELATIONS r
WHERE r.RDB$VIEW_BLR IS NOT NULL
  AND COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0
ORDER BY r.RDB$RELATION_NAME`, interBaseMetadataIdentifier("r.RDB$RELATION_NAME", width), interBaseMetadataIdentifier("r.RDB$OWNER_NAME", width))
	err := interBaseBulkQuery(ctx, q, "view headers", headerQuery, func(rows *sql.Rows) error {
		var rawName, owner, viewSource, description sql.NullString
		if err := rows.Scan(&rawName, &owner, &viewSource, &description); err != nil {
			return err
		}
		name, err := interBaseRequiredName(rawName, "view name")
		if err != nil {
			return err
		}
		if _, exists := byName[name]; exists {
			return fmt.Errorf("duplicate view header %q", name)
		}
		columns, _ := cache.ColumnDescsByID(name)
		byName[name] = &ViewDesc{Schema: "", Name: name, OwnerName: trimInterBaseCatalogName(owner), ViewSource: viewSource, Description: description, Columns: interBaseViewColumnsWithoutKey(columns)}
		names = append(names, name)
		return nil
	})
	if err != nil {
		return MetadataPatch{}, err
	}
	byDescription := make(map[string]*ViewDesc, len(names))
	byID := make(map[string]*ViewDesc, len(names))
	for _, name := range names {
		view := byName[name]
		byDescription[catalogCacheKey(view.Name)] = view
		byID[interBaseByIDKey(ObjectKindView, view.Name)] = view
	}
	return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Views: byDescription, ViewsByID: byID}}, Count: len(names)}, nil
}

// interBaseViewColumnsWithoutKey returns copies of columns with Key forced
// to "NO", matching the old standalone viewDescriptions path's constant
// (built with an always-empty primary-key set). columns comes from the
// shared ColumnsByID cache, whose descriptors' Key field reflects whichever
// bulk categories (columns, primary keys) had merged by the moment the
// views job was admitted -- a race outcome, not real per-view data -- so
// the cached value must never be forwarded as-is.
func interBaseViewColumnsWithoutKey(columns []*ColumnDesc) []*ColumnDesc {
	// A non-nil empty slice, matching the old standalone viewDescriptions
	// path (make([]*ColumnDesc, 0, len(view.Columns))): a view with zero
	// columns must compare equal to that legacy shape, not diverge to nil.
	result := make([]*ColumnDesc, 0, len(columns))
	for _, column := range columns {
		copy := *column
		copy.Key = "NO"
		result = append(result, &copy)
	}
	return result
}

// readMetadataPrimaryKeys publishes both the legacy shared upper-cased
// PrimaryKeyColumns set and the exact-identity, segment-ordered
// PrimaryKeysByID projection (see catalog.Cache.PrimaryKeysByID) from the
// same interBaseBulkScanPrimaryKeyFields read: interBaseBulkPrimaryKeyFieldsQueryForWidth
// orders its rows by relation and then by segment position, so appending in
// scan order reproduces the composite key's exact column order.
func (db *InterBaseDBRepository) readMetadataPrimaryKeys(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
	primaryKeys := make(map[string]map[string]struct{})
	primaryKeysByID := make(map[string][]string)
	err := interBaseBulkScanPrimaryKeyFields(ctx, q, width, func(relation, column string) error {
		relationKey := columnDatabaseKey("", relation)
		if primaryKeys[relationKey] == nil {
			primaryKeys[relationKey] = make(map[string]struct{})
		}
		primaryKeys[relationKey][column] = struct{}{}
		idKey := interBaseByIDKey(ObjectKindTable, relation)
		primaryKeysByID[idKey] = append(primaryKeysByID[idKey], column)
		return nil
	})
	if err != nil {
		return MetadataPatch{}, err
	}
	return MetadataPatch{Cache: &DBCache{PrimaryKeyColumns: primaryKeys, PrimaryKeysByID: primaryKeysByID}, Count: len(primaryKeys)}, nil
}

func (db *InterBaseDBRepository) readMetadataForeignKeys(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
	mappings, err := interBaseBulkForeignKeysForWidth(ctx, q, width)
	if err != nil {
		return MetadataPatch{}, err
	}
	foreignKeys := make(map[string]map[string][]*ForeignKey)
	for _, mapping := range mappings {
		for _, foreignKey := range interBaseForeignKeys([]interBaseForeignKeyMapping{mapping}) {
			first := (*foreignKey)[0]
			leftKey := columnDatabaseKey("", first[0].Table)
			rightKey := columnDatabaseKey("", first[1].Table)
			if foreignKeys[leftKey] == nil {
				foreignKeys[leftKey] = make(map[string][]*ForeignKey)
			}
			foreignKeys[leftKey][rightKey] = append(foreignKeys[leftKey][rightKey], foreignKey)
			if foreignKeys[rightKey] == nil {
				foreignKeys[rightKey] = make(map[string][]*ForeignKey)
			}
			foreignKeys[rightKey][leftKey] = append(foreignKeys[rightKey][leftKey], foreignKey)
		}
	}
	return MetadataPatch{Cache: &DBCache{ForeignKeys: foreignKeys}, Count: len(mappings)}, nil
}

func interBaseBulkRelationsForQuery(ctx context.Context, q schema.Queryer, query string) ([]interBaseBulkRelation, error) {
	// Keep the existing relation validation/scan while allowing the metadata
	// job to use the width discovered in its own snapshot transaction.
	relations := make([]interBaseBulkRelation, 0)
	err := interBaseBulkQuery(ctx, q, "relations", query, func(rows *sql.Rows) error {
		var name sql.NullString
		if err := rows.Scan(&name); err != nil {
			return err
		}
		relation, err := interBaseRequiredName(name, "relation name")
		if err != nil {
			return err
		}
		relations = append(relations, interBaseBulkRelation{name: relation})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return relations, nil
}

func interBaseBulkForeignKeysForWidth(ctx context.Context, q schema.Queryer, width int) ([]interBaseForeignKeyMapping, error) {
	// Pass a width-aware SQL string into the shared grouping scan so the job
	// uses its snapshot-derived identifier capacity without duplicating logic.
	return interBaseBulkForeignKeysWithQuery(ctx, q, interBaseBulkForeignKeyFieldsQueryForWidth(width))
}
