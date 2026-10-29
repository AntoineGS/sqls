package database

import (
	"context"
	"database/sql"
	"strings"

	"github.com/sqls-server/sqls/catalog"
)

func (db *MySQLMetadataRepository) readMetadataRelations(ctx context.Context, tx *sql.Tx, schema string) (catalog.MetadataPatch, error) {
	const query = `
SELECT TABLE_SCHEMA, TABLE_NAME, TABLE_TYPE
FROM information_schema.TABLES
WHERE TABLE_SCHEMA = ?
  AND TABLE_TYPE IN ('BASE TABLE', 'VIEW')
ORDER BY TABLE_SCHEMA, TABLE_NAME`
	tables := make([]string, 0)
	err := mysqlMetadataRows(ctx, tx, query, schema, func(rows *sql.Rows) error {
		var schemaName, tableName, tableType string
		if err := rows.Scan(&schemaName, &tableName, &tableType); err != nil {
			return err
		}
		if schemaName != schema || (tableType != "BASE TABLE" && tableType != "VIEW") {
			return nil
		}
		tables = append(tables, tableName)
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	cache := &catalog.Cache{SchemaTables: map[string][]string{strings.ToUpper(schema): tables}}
	return catalog.MetadataPatch{Cache: cache, Count: len(tables)}, nil
}

func (db *MySQLMetadataRepository) readMetadataViews(ctx context.Context, tx *sql.Tx, schema string, cache *catalog.Cache) (catalog.MetadataPatch, error) {
	const query = `
SELECT TABLE_SCHEMA, TABLE_NAME, VIEW_DEFINITION, DEFINER
FROM information_schema.VIEWS
WHERE TABLE_SCHEMA = ?
ORDER BY TABLE_SCHEMA, TABLE_NAME`
	views := make(map[string]*catalog.ViewDesc)
	viewsByID := make(map[string]*catalog.ViewDesc)
	err := mysqlMetadataRows(ctx, tx, query, schema, func(rows *sql.Rows) error {
		var schemaName, name string
		var source, definer sql.NullString
		if err := rows.Scan(&schemaName, &name, &source, &definer); err != nil {
			return err
		}
		if schemaName != schema {
			return nil
		}
		columns := make([]*catalog.ColumnDesc, 0)
		if cache != nil {
			for _, column := range cache.ColumnsByID[catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: name}.Key()] {
				if column == nil || column.Schema != schemaName || column.Table != name {
					continue
				}
				copy := *column
				columns = append(columns, &copy)
			}
		}
		view := &catalog.ViewDesc{Schema: schemaName, Name: name, OwnerName: definer, ViewSource: source, Columns: columns}
		views[catalog.CatalogCacheKey(name)] = view
		viewsByID[catalog.ObjectID{Kind: catalog.ObjectKindView, Name: name}.Key()] = view
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	fragment := &catalog.CatalogCache{Views: views, ViewsByID: viewsByID}
	return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: fragment}, Count: len(views)}, nil
}
