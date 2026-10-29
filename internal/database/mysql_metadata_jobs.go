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
	return catalog.MetadataPlan{Parallelism: 3, Jobs: []catalog.MetadataJob{
		{Kind: catalog.MetadataSchemas, Run: func(ctx context.Context, _ *catalog.Cache) (catalog.MetadataPatch, error) {
			return db.withMetadataRead(ctx, db.readMetadataSchemas)
		}},
		{Kind: catalog.MetadataRelations, Run: func(ctx context.Context, _ *catalog.Cache) (catalog.MetadataPatch, error) {
			return db.withMetadataRead(ctx, db.readMetadataRelations)
		}},
		{Kind: catalog.MetadataColumnsCurrent, Run: func(ctx context.Context, _ *catalog.Cache) (catalog.MetadataPatch, error) {
			return db.withMetadataRead(ctx, db.readMetadataColumns)
		}},
		{Kind: catalog.MetadataViews, DependsOn: []catalog.MetadataKind{catalog.MetadataColumnsCurrent}, Run: func(ctx context.Context, cache *catalog.Cache) (catalog.MetadataPatch, error) {
			return db.withMetadataRead(ctx, func(ctx context.Context, tx *sql.Tx) (catalog.MetadataPatch, error) {
				return db.readMetadataViews(ctx, tx, cache)
			})
		}},
		{Kind: catalog.MetadataIndexes, Run: func(ctx context.Context, _ *catalog.Cache) (catalog.MetadataPatch, error) {
			return db.withMetadataRead(ctx, db.readMetadataIndexes)
		}},
		{Kind: catalog.MetadataPrimaryKeys, Run: func(ctx context.Context, _ *catalog.Cache) (catalog.MetadataPatch, error) {
			return db.withMetadataRead(ctx, db.readMetadataPrimaryKeys)
		}},
		{Kind: catalog.MetadataForeignKeys, Run: func(ctx context.Context, _ *catalog.Cache) (catalog.MetadataPatch, error) {
			return db.withMetadataRead(ctx, db.readMetadataForeignKeys)
		}},
		{Kind: catalog.MetadataProcedures, Run: func(ctx context.Context, _ *catalog.Cache) (catalog.MetadataPatch, error) {
			return db.withMetadataRead(ctx, db.readMetadataProcedures)
		}},
		{Kind: catalog.MetadataFunctions, Run: func(ctx context.Context, _ *catalog.Cache) (catalog.MetadataPatch, error) {
			return db.withMetadataRead(ctx, db.readMetadataFunctions)
		}},
		{Kind: catalog.MetadataTriggers, Run: func(ctx context.Context, _ *catalog.Cache) (catalog.MetadataPatch, error) {
			return db.withMetadataRead(ctx, db.readMetadataTriggers)
		}},
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
		if candidate == db.Schema {
			schemaName = candidate
		}
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
