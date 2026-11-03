package database

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/sqls-server/sqls/catalog"
)

func (db *DuckDBMetadataRepository) readMetadataDomains(ctx context.Context, conn *sql.Conn, _ *catalog.Cache) (catalog.MetadataPatch, error) {
	const query = `
SELECT schema_name, type_name, logical_type
FROM duckdb_types()
WHERE database_name = ? AND NOT internal
  AND schema_name NOT IN ('information_schema', 'pg_catalog')
ORDER BY schema_name, type_name`
	byName := make(map[string]*catalog.DomainDesc)
	byID := make(map[string]*catalog.DomainDesc)
	err := duckDBMetadataRows(ctx, conn, query, []any{db.Catalog}, func(rows *sql.Rows) error {
		var schema, name, logicalType string
		if err := rows.Scan(&schema, &name, &logicalType); err != nil {
			return err
		}
		id := catalog.ObjectID{Schema: schema, Kind: catalog.ObjectKindDomain, Name: name}.Key()
		if _, exists := byID[id]; exists {
			return fmt.Errorf("duplicate DuckDB type %s.%s", schema, name)
		}
		desc := &catalog.DomainDesc{
			Schema: schema,
			Name:   name,
			Type:   logicalType,
			Facts:  &catalog.ColumnTypeFacts{UserDomain: true},
		}
		byName[catalog.CatalogCacheKey(name)] = desc
		byID[id] = desc
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Domains: byName, DomainsByID: byID}}, Count: len(byID)}, nil
}

func (db *DuckDBMetadataRepository) readMetadataGenerators(ctx context.Context, conn *sql.Conn, _ *catalog.Cache) (catalog.MetadataPatch, error) {
	const query = `
SELECT schema_name, sequence_name
FROM duckdb_sequences()
WHERE database_name = ? AND NOT temporary
  AND schema_name NOT IN ('information_schema', 'pg_catalog')
ORDER BY schema_name, sequence_name`
	byName := make(map[string]*catalog.GeneratorDesc)
	byID := make(map[string]*catalog.GeneratorDesc)
	err := duckDBMetadataRows(ctx, conn, query, []any{db.Catalog}, func(rows *sql.Rows) error {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			return err
		}
		id := catalog.ObjectID{Schema: schema, Kind: catalog.ObjectKindGenerator, Name: name}.Key()
		if _, exists := byID[id]; exists {
			return fmt.Errorf("duplicate DuckDB sequence %s.%s", schema, name)
		}
		desc := &catalog.GeneratorDesc{Schema: schema, Name: name}
		byName[catalog.CatalogCacheKey(name)] = desc
		byID[id] = desc
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Generators: byName, GeneratorsByID: byID}}, Count: len(byID)}, nil
}

type duckDBMacroArgument struct {
	name     sql.NullString
	position sql.NullInt64
}

func (db *DuckDBMetadataRepository) readMetadataFunctions(ctx context.Context, conn *sql.Conn, _ *catalog.Cache) (catalog.MetadataPatch, error) {
	const query = `
SELECT f.schema_name, f.function_name, f.macro_definition,
       COALESCE(CAST(f.parameters AS VARCHAR), '[]'),
       p.parameter_name, p.position
FROM duckdb_functions() AS f
LEFT JOIN UNNEST(f.parameters) WITH ORDINALITY AS p(parameter_name, position) ON TRUE
WHERE f.database_name = ?
  AND f.function_type IN ('macro', 'table_macro')
  AND NOT f.internal
  AND f.schema_name NOT IN ('information_schema', 'pg_catalog')
ORDER BY f.schema_name, f.function_name, f.macro_definition,
         COALESCE(CAST(f.parameters AS VARCHAR), '[]'), p.position`
	type macro struct {
		desc       *catalog.FunctionDesc
		definition string
		signature  string
		arguments  []duckDBMacroArgument
	}
	macros := make(map[string]*macro)
	err := duckDBMetadataRows(ctx, conn, query, []any{db.Catalog}, func(rows *sql.Rows) error {
		var schema, name, definition, signature string
		var parameter sql.NullString
		var position sql.NullInt64
		if err := rows.Scan(&schema, &name, &definition, &signature, &parameter, &position); err != nil {
			return err
		}
		id := catalog.ObjectID{Schema: schema, Kind: catalog.ObjectKindFunction, Name: name}.Key()
		entry := macros[id]
		if entry == nil {
			entry = &macro{
				desc:       &catalog.FunctionDesc{Schema: schema, Name: name, Arguments: make([]*catalog.FunctionArgumentDesc, 0)},
				definition: definition,
				signature:  signature,
			}
			macros[id] = entry
		} else if entry.definition != definition || entry.signature != signature {
			// The shared catalog models one function descriptor per name. Keep a
			// deterministic representative overload instead of failing this whole
			// metadata category when DuckDB exposes several definitions.
			return nil
		}
		if parameter.Valid || position.Valid {
			if !parameter.Valid || !position.Valid || parameter.String == "" || position.Int64 < 1 {
				return fmt.Errorf("macro %s.%s has invalid argument metadata", schema, name)
			}
			entry.arguments = append(entry.arguments, duckDBMacroArgument{name: parameter, position: position})
		}
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	byName := make(map[string]*catalog.FunctionDesc)
	byID := make(map[string]*catalog.FunctionDesc, len(macros))
	for id, entry := range macros {
		sort.SliceStable(entry.arguments, func(i, j int) bool { return entry.arguments[i].position.Int64 < entry.arguments[j].position.Int64 })
		for _, argument := range entry.arguments {
			entry.desc.Arguments = append(entry.desc.Arguments, &catalog.FunctionArgumentDesc{
				Name: argument.name.String, FunctionName: entry.desc.Name,
				Position: argument.position,
			})
		}
		byID[id] = entry.desc
		byName[catalog.CatalogCacheKey(entry.desc.Name)] = entry.desc
	}
	return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Functions: byName, FunctionsByID: byID}}, Count: len(byID)}, nil
}
