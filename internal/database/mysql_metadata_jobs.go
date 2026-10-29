package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/sqls-server/sqls/catalog"
)

// MetadataPlan provides the schema-scoped MySQL categories supported by the
// shared catalog. Unsupported categories are omitted for the loader to mark.
func (db *MySQLMetadataRepository) MetadataPlan() catalog.MetadataPlan {
	schemaJob := func(kind catalog.MetadataKind, dependencies []catalog.MetadataKind, read mysqlMetadataSchemaRead) catalog.MetadataJob {
		dependsOn := append([]catalog.MetadataKind{catalog.MetadataSchemas}, dependencies...)
		return catalog.MetadataJob{Kind: kind, DependsOn: dependsOn, Run: func(ctx context.Context, cache *catalog.Cache) (catalog.MetadataPatch, error) {
			return db.withSchemaMetadataRead(ctx, cache, read)
		}}
	}
	return catalog.MetadataPlan{Parallelism: 3, Jobs: []catalog.MetadataJob{
		{Kind: catalog.MetadataSchemas, Run: func(ctx context.Context, _ *catalog.Cache) (catalog.MetadataPatch, error) {
			return db.withMetadataRead(ctx, db.readMetadataSchemas)
		}},
		schemaJob(catalog.MetadataRelations, nil, db.readMetadataRelations),
		schemaJob(catalog.MetadataColumnsCurrent, nil, db.readMetadataColumns),
		{Kind: catalog.MetadataViews, DependsOn: []catalog.MetadataKind{catalog.MetadataSchemas, catalog.MetadataColumnsCurrent}, Run: func(ctx context.Context, cache *catalog.Cache) (catalog.MetadataPatch, error) {
			return db.withSchemaMetadataRead(ctx, cache, func(ctx context.Context, tx *sql.Tx, schema string) (catalog.MetadataPatch, error) {
				return db.readMetadataViews(ctx, tx, schema, cache)
			})
		}},
		schemaJob(catalog.MetadataIndexes, nil, db.readMetadataIndexes),
		schemaJob(catalog.MetadataPrimaryKeys, nil, db.readMetadataPrimaryKeys),
		schemaJob(catalog.MetadataForeignKeys, nil, db.readMetadataForeignKeys),
		schemaJob(catalog.MetadataProcedures, nil, db.readMetadataProcedures),
		schemaJob(catalog.MetadataFunctions, nil, db.readMetadataFunctions),
		schemaJob(catalog.MetadataTriggers, nil, db.readMetadataTriggers),
	}}
}

func (db *MySQLMetadataRepository) readMetadataSchemas(ctx context.Context, tx *sql.Tx) (catalog.MetadataPatch, error) {
	const query = `
SELECT SCHEMA_NAME
FROM information_schema.SCHEMATA
WHERE SCHEMA_NAME = ?`
	var schemaName string
	err := mysqlMetadataRows(ctx, tx, query, db.Schema, func(rows *sql.Rows) error {
		var candidate string
		if err := rows.Scan(&candidate); err != nil {
			return err
		}
		if schemaName != "" {
			return fmt.Errorf("mysql: schema lookup for %q returned more than one row", db.Schema)
		}
		schemaName = candidate
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	if schemaName == "" {
		return catalog.MetadataPatch{}, fmt.Errorf("mysql: connected schema %q was not found in information_schema.SCHEMATA", db.Schema)
	}
	cache := &catalog.Cache{
		DefaultSchema: schemaName,
		Schemas:       map[string]string{strings.ToUpper(schemaName): schemaName},
	}
	return catalog.MetadataPatch{Cache: cache, Count: 1}, nil
}
