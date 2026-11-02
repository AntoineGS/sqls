package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
)

// MetadataDetailPlan adapts supported DuckDB DDL requests to the native SQL
// text retained by DuckDB's catalog table functions.
func (db *DuckDBMetadataRepository) MetadataDetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	if db == nil || db.Conn == nil || db.Catalog == "" || req.Facet != "ddl" {
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
	if req.Consistency != "" && req.Consistency != metadata.Committed {
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
	var required catalog.MetadataKind
	switch req.Object.Kind {
	case catalog.ObjectKindTable:
		required = catalog.MetadataRelations
	case catalog.ObjectKindView:
		required = catalog.MetadataViews
	case catalog.ObjectKindFunction:
		required = catalog.MetadataFunctions
	case catalog.ObjectKindIndex:
		required = catalog.MetadataIndexes
	case catalog.ObjectKindGenerator:
		required = catalog.MetadataGenerators
	default:
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
	requires := []catalog.MetadataKind{required}
	if req.Object.Kind == catalog.ObjectKindTable {
		requires = append(requires, catalog.MetadataViews)
	}
	return metadata.DetailPlan{
		Requires: requires,
		Coverage: req.Object.Key() + "\x00ddl",
		Load: func(ctx context.Context, cache *catalog.Cache) (metadata.Detail, error) {
			schema, err := duckDBDetailSchema(cache, req.Object.Schema)
			if err != nil {
				return metadata.Detail{}, err
			}
			name, err := duckDBResolveDetailName(cache, schema, req.Object.Kind, req.Object.Name)
			if err != nil {
				return metadata.Detail{}, err
			}
			var statement string
			switch req.Object.Kind {
			case catalog.ObjectKindTable:
				statement = `SELECT sql FROM duckdb_tables() WHERE database_name = ? AND schema_name = ? AND table_name = ? AND NOT internal AND NOT temporary`
			case catalog.ObjectKindView:
				statement = `SELECT sql FROM duckdb_views() WHERE database_name = ? AND schema_name = ? AND view_name = ? AND NOT internal AND NOT temporary`
			case catalog.ObjectKindFunction:
				statement = `SELECT macro_definition FROM duckdb_functions() WHERE database_name = ? AND schema_name = ? AND function_name = ? AND function_type IN ('macro', 'table_macro') AND NOT internal ORDER BY macro_definition, CAST(parameters AS VARCHAR) LIMIT 1`
			case catalog.ObjectKindIndex:
				statement = `SELECT i.sql FROM duckdb_indexes() AS i JOIN duckdb_tables() AS t ON t.database_name = i.database_name AND t.schema_name = i.schema_name AND t.table_name = i.table_name WHERE i.database_name = ? AND i.schema_name = ? AND i.index_name = ? AND NOT t.internal AND NOT t.temporary`
			case catalog.ObjectKindGenerator:
				statement = `SELECT sql FROM duckdb_sequences() WHERE database_name = ? AND schema_name = ? AND sequence_name = ? AND NOT temporary`
			}
			var text string
			_, err = db.withMetadataRead(ctx, func(ctx context.Context, conn *sql.Conn) (catalog.MetadataPatch, error) {
				readErr := conn.QueryRowContext(ctx, statement, db.Catalog, schema, name).Scan(&text)
				if errors.Is(readErr, sql.ErrNoRows) {
					return catalog.MetadataPatch{}, ErrObjectNotFound
				}
				if readErr != nil {
					return catalog.MetadataPatch{}, fmt.Errorf("duckdb: read DDL for %s.%s: %w", schema, name, readErr)
				}
				return catalog.MetadataPatch{}, nil
			})
			if err != nil {
				return metadata.Detail{}, err
			}
			return metadata.Detail{
				Object: catalog.ObjectID{Schema: schema, Kind: req.Object.Kind, Name: name},
				Text:   text, Provenance: "duckdb:" + string(req.Object.Kind) + "-ddl",
			}, nil
		},
	}, nil
}

func duckDBDetailSchema(cache *catalog.Cache, requested string) (string, error) {
	if cache == nil || cache.DefaultSchema == "" || cache.Schemas == nil {
		return "", errors.New("duckdb: canonical detail schema is unavailable")
	}
	if requested == "" {
		return cache.DefaultSchema, nil
	}
	schema, ok := cache.Schemas[strings.ToUpper(requested)]
	if !ok {
		return "", metadata.ErrUnsupported
	}
	return schema, nil
}

func duckDBResolveDetailName(cache *catalog.Cache, schema string, kind catalog.ObjectKind, requested string) (string, error) {
	if cache == nil {
		return "", ErrObjectNotFound
	}
	var candidates []string
	switch kind {
	case catalog.ObjectKindTable:
		for _, name := range cache.SchemaTables[strings.ToUpper(schema)] {
			isView := false
			if cache.Catalog != nil {
				_, isView = cache.Catalog.ViewsByID[catalog.ObjectID{Schema: schema, Kind: catalog.ObjectKindView, Name: name}.Key()]
			}
			if !isView {
				candidates = append(candidates, name)
			}
		}
	case catalog.ObjectKindView:
		if cache.Catalog != nil {
			for _, view := range cache.Catalog.ViewsByID {
				if view != nil && view.Schema == schema {
					candidates = append(candidates, view.Name)
				}
			}
		}
	case catalog.ObjectKindFunction:
		if cache.Catalog != nil {
			for _, function := range cache.Catalog.FunctionsByID {
				if function != nil && function.Schema == schema {
					candidates = append(candidates, function.Name)
				}
			}
		}
	case catalog.ObjectKindIndex:
		if cache.Catalog != nil {
			for _, index := range cache.Catalog.IndexesByID {
				if index != nil && index.Schema == schema {
					candidates = append(candidates, index.Name)
				}
			}
		}
	case catalog.ObjectKindGenerator:
		if cache.Catalog != nil {
			for _, generator := range cache.Catalog.GeneratorsByID {
				if generator != nil && generator.Schema == schema {
					candidates = append(candidates, generator.Name)
				}
			}
		}
	default:
		return "", metadata.ErrUnsupported
	}
	var folded string
	matches := 0
	for _, candidate := range candidates {
		if candidate == requested {
			return candidate, nil
		}
		if strings.EqualFold(candidate, requested) {
			folded = candidate
			matches++
		}
	}
	switch matches {
	case 0:
		return "", ErrObjectNotFound
	case 1:
		return folded, nil
	default:
		return "", fmt.Errorf("duckdb: %s name %q is ambiguous: %w", kind, requested, catalog.ErrAmbiguous)
	}
}
