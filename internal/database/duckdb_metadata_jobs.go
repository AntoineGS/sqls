package database

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/sqls-server/sqls/catalog"
)

// MetadataPlan provides the DuckDB metadata categories supported by the
// shared catalog. Omitted categories are reported as unsupported by the loader.
func (db *DuckDBMetadataRepository) MetadataPlan() catalog.MetadataPlan {
	dependsOnSchemas := []catalog.MetadataKind{catalog.MetadataSchemas}
	job := func(kind catalog.MetadataKind, dependencies []catalog.MetadataKind, read func(context.Context, *sql.Conn, *catalog.Cache) (catalog.MetadataPatch, error)) catalog.MetadataJob {
		depends := append(append([]catalog.MetadataKind(nil), dependsOnSchemas...), dependencies...)
		return catalog.MetadataJob{Kind: kind, DependsOn: depends, Run: func(ctx context.Context, cache *catalog.Cache) (catalog.MetadataPatch, error) {
			return db.withMetadataRead(ctx, func(ctx context.Context, conn *sql.Conn) (catalog.MetadataPatch, error) {
				return read(ctx, conn, cache)
			})
		}}
	}
	return catalog.MetadataPlan{Parallelism: 3, Jobs: []catalog.MetadataJob{
		{Kind: catalog.MetadataSchemas, Run: func(ctx context.Context, _ *catalog.Cache) (catalog.MetadataPatch, error) {
			return db.withMetadataRead(ctx, db.readMetadataSchemas)
		}},
		job(catalog.MetadataRelations, nil, db.readMetadataRelations),
		job(catalog.MetadataColumnsCurrent, nil, db.readMetadataColumns),
		job(catalog.MetadataViews, []catalog.MetadataKind{catalog.MetadataColumnsCurrent}, db.readMetadataViews),
		job(catalog.MetadataIndexes, nil, db.readMetadataIndexes),
		job(catalog.MetadataPrimaryKeys, nil, db.readMetadataPrimaryKeys),
		job(catalog.MetadataForeignKeys, nil, db.readMetadataForeignKeys),
		job(catalog.MetadataGenerators, nil, db.readMetadataGenerators),
		job(catalog.MetadataFunctions, nil, db.readMetadataFunctions),
	}}
}

func (db *DuckDBMetadataRepository) readMetadataSchemas(ctx context.Context, conn *sql.Conn) (catalog.MetadataPatch, error) {
	const query = `
SELECT schema_name
FROM duckdb_schemas()
WHERE database_name = ?
  AND (schema_name = 'main' OR NOT internal)
  AND schema_name NOT IN ('information_schema', 'pg_catalog')
ORDER BY schema_name`
	schemas := make(map[string]string)
	err := duckDBMetadataRows(ctx, conn, query, []any{db.Catalog}, func(rows *sql.Rows) error {
		var schema string
		if err := rows.Scan(&schema); err != nil {
			return err
		}
		key := strings.ToUpper(schema)
		if _, exists := schemas[key]; exists {
			return fmt.Errorf("duplicate DuckDB schema %q", schema)
		}
		schemas[key] = schema
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	if _, ok := schemas["MAIN"]; !ok {
		return catalog.MetadataPatch{}, fmt.Errorf("duckdb: current catalog %q has no main schema", db.Catalog)
	}
	return catalog.MetadataPatch{Cache: &catalog.Cache{DefaultSchema: "main", Schemas: schemas}, Count: len(schemas)}, nil
}

func (db *DuckDBMetadataRepository) readMetadataRelations(ctx context.Context, conn *sql.Conn, cache *catalog.Cache) (catalog.MetadataPatch, error) {
	const query = `
SELECT schema_name, relation_name
FROM (
    SELECT schema_name, table_name AS relation_name
    FROM duckdb_tables()
    WHERE database_name = ? AND NOT internal AND NOT temporary
      AND schema_name NOT IN ('information_schema', 'pg_catalog')
    UNION ALL
    SELECT schema_name, view_name AS relation_name
    FROM duckdb_views()
    WHERE database_name = ? AND NOT internal AND NOT temporary
      AND schema_name NOT IN ('information_schema', 'pg_catalog')
) AS relations
ORDER BY schema_name, relation_name`
	relations := make(map[string][]string)
	if cache != nil {
		for _, schema := range cache.Schemas {
			relations[strings.ToUpper(schema)] = make([]string, 0)
		}
	}
	err := duckDBMetadataRows(ctx, conn, query, []any{db.Catalog, db.Catalog}, func(rows *sql.Rows) error {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			return err
		}
		if relations[strings.ToUpper(schema)] == nil {
			return fmt.Errorf("relation %s.%s belongs to an unlisted schema", schema, name)
		}
		relations[strings.ToUpper(schema)] = append(relations[strings.ToUpper(schema)], name)
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	count := 0
	for _, names := range relations {
		count += len(names)
	}
	return catalog.MetadataPatch{Cache: &catalog.Cache{SchemaTables: relations}, Count: count}, nil
}

type duckDBMetadataColumn struct {
	desc  *catalog.ColumnDesc
	index int64
}

func (db *DuckDBMetadataRepository) readMetadataColumns(ctx context.Context, conn *sql.Conn, _ *catalog.Cache) (catalog.MetadataPatch, error) {
	const query = `
WITH visible_relations AS (
    SELECT database_name, schema_name, table_name AS relation_name
    FROM duckdb_tables()
    WHERE database_name = ? AND NOT internal AND NOT temporary
      AND schema_name NOT IN ('information_schema', 'pg_catalog')
    UNION ALL
    SELECT database_name, schema_name, view_name AS relation_name
    FROM duckdb_views()
    WHERE database_name = ? AND NOT internal AND NOT temporary
      AND schema_name NOT IN ('information_schema', 'pg_catalog')
)
SELECT c.schema_name, c.table_name, c.column_name, c.column_index, c.data_type,
       CASE WHEN c.is_nullable THEN 'YES' ELSE 'NO' END, c.column_default
FROM duckdb_columns() AS c
JOIN visible_relations AS r
  ON r.database_name = c.database_name
 AND r.schema_name = c.schema_name
 AND r.relation_name = c.table_name
WHERE c.database_name = ? AND NOT c.internal
  AND c.schema_name NOT IN ('information_schema', 'pg_catalog')
ORDER BY c.schema_name, c.table_name, c.column_index`
	columns := make([]duckDBMetadataColumn, 0)
	err := duckDBMetadataRows(ctx, conn, query, []any{db.Catalog, db.Catalog, db.Catalog}, func(rows *sql.Rows) error {
		var schema, table, name, dataType, nullable string
		var index int64
		var defaultValue sql.NullString
		if err := rows.Scan(&schema, &table, &name, &index, &dataType, &nullable, &defaultValue); err != nil {
			return err
		}
		if index < 1 {
			return fmt.Errorf("column %s.%s has invalid DuckDB index %d", table, name, index)
		}
		columns = append(columns, duckDBMetadataColumn{desc: &catalog.ColumnDesc{
			ColumnBase: catalog.ColumnBase{Schema: schema, Table: table, Name: name},
			Type:       dataType,
			Null:       nullable,
			Default:    defaultValue,
		}, index: index})
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	if err := db.populateDuckDBColumnKeys(ctx, conn, columns); err != nil {
		return catalog.MetadataPatch{}, err
	}
	sort.SliceStable(columns, func(i, j int) bool {
		left, right := columns[i], columns[j]
		if left.desc.Schema != right.desc.Schema {
			return left.desc.Schema < right.desc.Schema
		}
		if left.desc.Table != right.desc.Table {
			return left.desc.Table < right.desc.Table
		}
		return left.index < right.index
	})
	withParent := make(map[string][]*catalog.ColumnDesc)
	byID := make(map[string][]*catalog.ColumnDesc)
	for _, column := range columns {
		desc := column.desc
		parent := catalog.ColumnDatabaseKey(desc.Schema, desc.Table)
		withParent[parent] = append(withParent[parent], desc)
		id := catalog.ObjectID{Schema: desc.Schema, Kind: catalog.ObjectKindTable, Name: desc.Table}.Key()
		byID[id] = append(byID[id], desc)
	}
	return catalog.MetadataPatch{Cache: &catalog.Cache{ColumnsWithParent: withParent, ColumnsByID: byID}, Count: len(columns)}, nil
}

func (db *DuckDBMetadataRepository) readMetadataViews(ctx context.Context, conn *sql.Conn, cache *catalog.Cache) (catalog.MetadataPatch, error) {
	const query = `
SELECT schema_name, view_name, sql
FROM duckdb_views()
WHERE database_name = ? AND NOT internal AND NOT temporary
  AND schema_name NOT IN ('information_schema', 'pg_catalog')
ORDER BY schema_name, view_name`
	views := make(map[string]*catalog.ViewDesc)
	viewsByID := make(map[string]*catalog.ViewDesc)
	err := duckDBMetadataRows(ctx, conn, query, []any{db.Catalog}, func(rows *sql.Rows) error {
		var schema, name string
		var source sql.NullString
		if err := rows.Scan(&schema, &name, &source); err != nil {
			return err
		}
		columns := make([]*catalog.ColumnDesc, 0)
		if cache != nil {
			id := catalog.ObjectID{Schema: schema, Kind: catalog.ObjectKindTable, Name: name}.Key()
			for _, column := range cache.ColumnsByID[id] {
				if column == nil || column.Schema != schema || column.Table != name {
					continue
				}
				copy := *column
				columns = append(columns, &copy)
			}
		}
		view := &catalog.ViewDesc{Schema: schema, Name: name, ViewSource: source, Columns: columns}
		views[catalog.CatalogCacheKey(name)] = view
		viewsByID[catalog.ObjectID{Schema: schema, Kind: catalog.ObjectKindView, Name: name}.Key()] = view
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	fragment := &catalog.CatalogCache{Views: views, ViewsByID: viewsByID}
	return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: fragment}, Count: len(viewsByID)}, nil
}
