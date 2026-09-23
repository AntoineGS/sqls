package database

import (
	"context"
	"database/sql"

	"interbase-go/schema"
)

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
	// interBaseBulkForeignKeys historically uses the fixed-width compatibility
	// query. Temporarily route the shared grouping scan through a width-aware
	// queryer; this avoids maintaining a second grouping implementation.
	return interBaseBulkForeignKeysWithQuery(ctx, q, interBaseBulkForeignKeyFieldsQueryForWidth(width))
}
