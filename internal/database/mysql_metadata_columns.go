package database

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/sqls-server/sqls/catalog"
)

type mysqlMetadataColumn struct {
	descriptor *catalog.ColumnDesc
	ordinal    int64
}

func (db *MySQLMetadataRepository) readMetadataColumns(ctx context.Context, tx *sql.Tx, schema string) (catalog.MetadataPatch, error) {
	const query = `
SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE,
       COLUMN_KEY, COLUMN_DEFAULT, EXTRA, ORDINAL_POSITION
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = ?
ORDER BY TABLE_SCHEMA, TABLE_NAME, ORDINAL_POSITION`
	columns := make([]mysqlMetadataColumn, 0)
	err := mysqlMetadataRows(ctx, tx, query, schema, func(rows *sql.Rows) error {
		var schemaName, tableName, name, columnType, nullable, key, extra string
		var defaultValue sql.NullString
		var ordinal int64
		if err := rows.Scan(&schemaName, &tableName, &name, &columnType, &nullable, &key, &defaultValue, &extra, &ordinal); err != nil {
			return err
		}
		if schemaName != schema {
			return nil
		}
		if ordinal < 1 {
			return fmt.Errorf("column %s.%s has invalid ordinal position %d", tableName, name, ordinal)
		}
		columns = append(columns, mysqlMetadataColumn{
			descriptor: &catalog.ColumnDesc{
				ColumnBase: catalog.ColumnBase{Schema: schemaName, Table: tableName, Name: name},
				Type:       columnType,
				Null:       nullable,
				Key:        key,
				Default:    defaultValue,
				Extra:      extra,
			},
			ordinal: ordinal,
		})
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	sort.SliceStable(columns, func(i, j int) bool {
		left, right := columns[i].descriptor, columns[j].descriptor
		if left.Table != right.Table {
			return left.Table < right.Table
		}
		return columns[i].ordinal < columns[j].ordinal
	})
	withParent := make(map[string][]*catalog.ColumnDesc)
	byID := make(map[string][]*catalog.ColumnDesc)
	for _, column := range columns {
		descriptor := column.descriptor
		withParent[catalog.ColumnDatabaseKey(descriptor.Schema, descriptor.Table)] = append(
			withParent[catalog.ColumnDatabaseKey(descriptor.Schema, descriptor.Table)], descriptor,
		)
		id := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: descriptor.Table}.Key()
		byID[id] = append(byID[id], descriptor)
	}
	cache := &catalog.Cache{ColumnsWithParent: withParent, ColumnsByID: byID}
	return catalog.MetadataPatch{Cache: cache, Count: len(columns)}, nil
}
