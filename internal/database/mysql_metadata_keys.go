package database

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/sqls-server/sqls/catalog"
)

type mysqlMetadataIndex struct {
	desc      *catalog.IndexDesc
	positions map[int64]string
}

func (db *MySQLMetadataRepository) readMetadataIndexes(ctx context.Context, tx *sql.Tx) (catalog.MetadataPatch, error) {
	const query = `
SELECT TABLE_SCHEMA, TABLE_NAME, INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX, COLUMN_NAME
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = ?
ORDER BY TABLE_SCHEMA, TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`
	indexesByIdentity := make(map[string]*mysqlMetadataIndex)
	err := mysqlMetadataRows(ctx, tx, query, db.Schema, func(rows *sql.Rows) error {
		var schemaName, tableName, name string
		var nonUnique, position int64
		var column sql.NullString
		if err := rows.Scan(&schemaName, &tableName, &name, &nonUnique, &position, &column); err != nil {
			return err
		}
		if schemaName != db.Schema {
			return nil
		}
		if position < 1 || (nonUnique != 0 && nonUnique != 1) {
			return fmt.Errorf("index %q has invalid segment metadata: position %d, non-unique %d", name, position, nonUnique)
		}
		identity := tableName + "\x00" + name
		index := indexesByIdentity[identity]
		unique := sql.NullBool{Bool: nonUnique == 0, Valid: true}
		if index == nil {
			index = &mysqlMetadataIndex{
				desc: &catalog.IndexDesc{
					Schema: schemaName, Name: name, RelationName: tableName,
					Unique: unique, Active: sql.NullBool{Bool: true, Valid: true},
					Columns: make([]string, 0),
				},
				positions: make(map[int64]string),
			}
			indexesByIdentity[identity] = index
		} else if index.desc.Unique != unique {
			return fmt.Errorf("index %q has inconsistent uniqueness across segments", name)
		}
		if _, exists := index.positions[position]; exists {
			return fmt.Errorf("index %q has duplicate segment position %d", name, position)
		}
		if column.Valid {
			index.positions[position] = column.String
		} else {
			index.positions[position] = ""
		}
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	indexes := make([]*catalog.IndexDesc, 0, len(indexesByIdentity))
	for _, indexed := range indexesByIdentity {
		positions := make([]int64, 0, len(indexed.positions))
		for position := range indexed.positions {
			positions = append(positions, position)
		}
		sort.Slice(positions, func(i, j int) bool { return positions[i] < positions[j] })
		for _, position := range positions {
			column := indexed.positions[position]
			if column != "" {
				indexed.desc.Columns = append(indexed.desc.Columns, column)
			}
		}
		indexes = append(indexes, indexed.desc)
	}
	sort.Slice(indexes, func(i, j int) bool {
		if indexes[i].RelationName != indexes[j].RelationName {
			return indexes[i].RelationName < indexes[j].RelationName
		}
		return indexes[i].Name < indexes[j].Name
	})
	byName := make(map[string]*catalog.IndexDesc, len(indexes))
	byID := make(map[string]*catalog.IndexDesc, len(indexes))
	byTable := make(map[string][]*catalog.IndexDesc)
	for _, index := range indexes {
		byName[catalog.CatalogCacheKey(index.Name)] = index
		byID[catalog.ObjectID{Kind: catalog.ObjectKindIndex, Name: index.Name}.Key()] = index
		byTable[catalog.CatalogCacheKey(index.RelationName)] = append(byTable[catalog.CatalogCacheKey(index.RelationName)], index)
	}
	fragment := &catalog.CatalogCache{Indexes: byName, IndexesByTable: byTable, IndexesByID: byID}
	return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: fragment}, Count: len(indexes)}, nil
}

type mysqlMetadataPrimaryKeyRow struct {
	schema, table, column string
	position              int64
}

func (db *MySQLMetadataRepository) readMetadataPrimaryKeys(ctx context.Context, tx *sql.Tx) (catalog.MetadataPatch, error) {
	const query = `
SELECT kcu.TABLE_SCHEMA, kcu.TABLE_NAME, kcu.COLUMN_NAME, kcu.ORDINAL_POSITION
FROM information_schema.TABLE_CONSTRAINTS AS tc
JOIN information_schema.KEY_COLUMN_USAGE AS kcu
  ON kcu.CONSTRAINT_SCHEMA = tc.CONSTRAINT_SCHEMA
 AND kcu.TABLE_SCHEMA = tc.TABLE_SCHEMA
 AND kcu.TABLE_NAME = tc.TABLE_NAME
 AND kcu.CONSTRAINT_NAME = tc.CONSTRAINT_NAME
WHERE tc.TABLE_SCHEMA = ?
  AND tc.CONSTRAINT_TYPE = 'PRIMARY KEY'
ORDER BY kcu.TABLE_SCHEMA, kcu.TABLE_NAME, kcu.ORDINAL_POSITION`
	primaryRows := make([]mysqlMetadataPrimaryKeyRow, 0)
	err := mysqlMetadataRows(ctx, tx, query, db.Schema, func(rows *sql.Rows) error {
		var key mysqlMetadataPrimaryKeyRow
		if err := rows.Scan(&key.schema, &key.table, &key.column, &key.position); err != nil {
			return err
		}
		if key.schema != db.Schema {
			return nil
		}
		if key.position < 1 {
			return fmt.Errorf("primary key on %s has invalid segment position %d", key.table, key.position)
		}
		primaryRows = append(primaryRows, key)
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	sort.SliceStable(primaryRows, func(i, j int) bool {
		left, right := primaryRows[i], primaryRows[j]
		if left.table != right.table {
			return left.table < right.table
		}
		return left.position < right.position
	})
	primaryKeys := make(map[string]map[string]struct{})
	primaryKeysByID := make(map[string][]string)
	seen := make(map[string]map[int64]struct{})
	for _, row := range primaryRows {
		relationKey := catalog.ColumnDatabaseKey(row.schema, row.table)
		if primaryKeys[relationKey] == nil {
			primaryKeys[relationKey] = make(map[string]struct{})
		}
		if seen[relationKey] == nil {
			seen[relationKey] = make(map[int64]struct{})
		}
		if _, duplicate := seen[relationKey][row.position]; duplicate {
			return catalog.MetadataPatch{}, fmt.Errorf("primary key on %s has duplicate segment position %d", row.table, row.position)
		}
		seen[relationKey][row.position] = struct{}{}
		primaryKeys[relationKey][row.column] = struct{}{}
		id := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: row.table}.Key()
		primaryKeysByID[id] = append(primaryKeysByID[id], row.column)
	}
	return catalog.MetadataPatch{Cache: &catalog.Cache{PrimaryKeyColumns: primaryKeys, PrimaryKeysByID: primaryKeysByID}, Count: len(primaryKeysByID)}, nil
}

type mysqlMetadataForeignKeyRow struct {
	sourceSchema, sourceTable, sourceColumn string
	targetSchema, targetTable, targetColumn string
	name                                    string
	position                                int64
}

type mysqlMetadataForeignKeyIdentity struct {
	schema, table, name string
}

func (db *MySQLMetadataRepository) readMetadataForeignKeys(ctx context.Context, tx *sql.Tx) (catalog.MetadataPatch, error) {
	const query = `
SELECT rc.CONSTRAINT_SCHEMA, kcu.TABLE_SCHEMA, kcu.TABLE_NAME, kcu.COLUMN_NAME,
       kcu.REFERENCED_TABLE_SCHEMA, kcu.REFERENCED_TABLE_NAME,
       kcu.REFERENCED_COLUMN_NAME, rc.CONSTRAINT_NAME, kcu.ORDINAL_POSITION
FROM information_schema.REFERENTIAL_CONSTRAINTS AS rc
JOIN information_schema.KEY_COLUMN_USAGE AS kcu
  ON kcu.CONSTRAINT_SCHEMA = rc.CONSTRAINT_SCHEMA
 AND kcu.TABLE_SCHEMA = rc.CONSTRAINT_SCHEMA
 AND kcu.TABLE_NAME = rc.TABLE_NAME
 AND kcu.CONSTRAINT_NAME = rc.CONSTRAINT_NAME
WHERE rc.CONSTRAINT_SCHEMA = ?
  AND kcu.REFERENCED_TABLE_NAME IS NOT NULL
ORDER BY kcu.TABLE_SCHEMA, rc.CONSTRAINT_NAME, kcu.ORDINAL_POSITION`
	fkRows := make([]mysqlMetadataForeignKeyRow, 0)
	err := mysqlMetadataRows(ctx, tx, query, db.Schema, func(rows *sql.Rows) error {
		var constraintSchema string
		var key mysqlMetadataForeignKeyRow
		if err := rows.Scan(&constraintSchema, &key.sourceSchema, &key.sourceTable, &key.sourceColumn,
			&key.targetSchema, &key.targetTable, &key.targetColumn, &key.name, &key.position); err != nil {
			return err
		}
		if constraintSchema != db.Schema || key.sourceSchema != db.Schema {
			return nil
		}
		if key.targetSchema == "" || key.targetTable == "" || key.targetColumn == "" || key.position < 1 {
			return fmt.Errorf("foreign key %q has incomplete endpoint or position data", key.name)
		}
		fkRows = append(fkRows, key)
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	sort.SliceStable(fkRows, func(i, j int) bool {
		left, right := fkRows[i], fkRows[j]
		if left.sourceTable != right.sourceTable {
			return left.sourceTable < right.sourceTable
		}
		if left.name != right.name {
			return left.name < right.name
		}
		return left.position < right.position
	})
	foreignKeyParts := make(map[mysqlMetadataForeignKeyIdentity][]mysqlMetadataForeignKeyRow)
	for _, row := range fkRows {
		identity := mysqlMetadataForeignKeyIdentity{schema: row.sourceSchema, table: row.sourceTable, name: row.name}
		parts := foreignKeyParts[identity]
		if len(parts) > 0 && parts[len(parts)-1].position == row.position {
			return catalog.MetadataPatch{}, fmt.Errorf("foreign key %q has duplicate segment position %d", row.name, row.position)
		}
		foreignKeyParts[identity] = append(parts, row)
	}
	grouped := make(map[string]map[string][]*catalog.ForeignKey)
	for identity, parts := range foreignKeyParts {
		key := new(catalog.ForeignKey)
		for _, part := range parts {
			source := &catalog.ColumnBase{Schema: part.sourceSchema, Table: part.sourceTable, Name: part.sourceColumn}
			target := &catalog.ColumnBase{Schema: part.targetSchema, Table: part.targetTable, Name: part.targetColumn}
			*key = append(*key, [2]*catalog.ColumnBase{source, target})
		}
		if err := catalog.ValidateForeignKey(key); err != nil {
			return catalog.MetadataPatch{}, fmt.Errorf("foreign key %q: %w", identity.name, err)
		}
		sourceKey := catalog.ColumnDatabaseKey(parts[0].sourceSchema, parts[0].sourceTable)
		targetKey := catalog.ColumnDatabaseKey(parts[0].targetSchema, parts[0].targetTable)
		if grouped[sourceKey] == nil {
			grouped[sourceKey] = make(map[string][]*catalog.ForeignKey)
		}
		grouped[sourceKey][targetKey] = append(grouped[sourceKey][targetKey], key)
		if sourceKey != targetKey {
			if grouped[targetKey] == nil {
				grouped[targetKey] = make(map[string][]*catalog.ForeignKey)
			}
			grouped[targetKey][sourceKey] = append(grouped[targetKey][sourceKey], key)
		}
	}
	return catalog.MetadataPatch{Cache: &catalog.Cache{ForeignKeys: grouped}, Count: len(foreignKeyParts)}, nil
}
