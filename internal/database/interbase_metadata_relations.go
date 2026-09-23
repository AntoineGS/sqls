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
	query := interBaseBulkColumnsQueryForWidth(width)
	query = strings.Replace(query, "WHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0", "WHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0 AND r.RDB$VIEW_BLR IS NOT NULL", 1)
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
	for _, description := range descriptions {
		byDescription[description.Name] = description
	}
	return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Views: byDescription}}, Count: len(descriptions)}, nil
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
	return MetadataPatch{Cache: &DBCache{ColumnsWithParent: genColumnMap(columns)}, Count: len(columns)}, nil
}

func (db *InterBaseDBRepository) readMetadataPrimaryKeys(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
	primaryKeys := make(map[string]map[string]struct{})
	err := interBaseBulkScanPrimaryKeyFields(ctx, q, width, func(relation, column string) error {
		relationKey := columnDatabaseKey("", relation)
		if primaryKeys[relationKey] == nil {
			primaryKeys[relationKey] = make(map[string]struct{})
		}
		primaryKeys[relationKey][column] = struct{}{}
		return nil
	})
	if err != nil {
		return MetadataPatch{}, err
	}
	return MetadataPatch{Cache: &DBCache{PrimaryKeyColumns: primaryKeys}, Count: len(primaryKeys)}, nil
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
