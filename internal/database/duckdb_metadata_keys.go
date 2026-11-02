package database

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/sqls-server/sqls/catalog"
)

type duckDBConstraintColumn struct {
	schema, table, kind, name, column string
	position                          int64
}

func duckDBConstraintColumnsQuery(where string) string {
	return `
SELECT c.schema_name, c.table_name, c.constraint_type, c.constraint_name,
       u.column_name, u.position
FROM duckdb_constraints() AS c,
     duckdb_tables() AS t,
     UNNEST(c.constraint_column_names) WITH ORDINALITY AS u(column_name, position)
WHERE c.database_name = ?
  AND t.database_name = c.database_name
  AND t.schema_name = c.schema_name
  AND t.table_name = c.table_name
  AND NOT t.internal AND NOT t.temporary
  AND c.schema_name NOT IN ('information_schema', 'pg_catalog')
  AND ` + where + `
ORDER BY c.schema_name, c.table_name, c.constraint_name, u.position`
}

func (db *DuckDBMetadataRepository) populateDuckDBColumnKeys(ctx context.Context, conn *sql.Conn, columns []duckDBMetadataColumn) error {
	const filter = "c.constraint_type IN ('PRIMARY KEY', 'UNIQUE')"
	query := duckDBConstraintColumnsQuery(filter)
	byColumn := make(map[string]*catalog.ColumnDesc, len(columns))
	for _, column := range columns {
		desc := column.desc
		byColumn[duckDBColumnIdentity(desc.Schema, desc.Table, desc.Name)] = desc
	}
	return duckDBMetadataRows(ctx, conn, query, []any{db.Catalog}, func(rows *sql.Rows) error {
		var schema, table, kind, constraint, name string
		var position int64
		if err := rows.Scan(&schema, &table, &kind, &constraint, &name, &position); err != nil {
			return err
		}
		if position < 1 {
			return fmt.Errorf("constraint %q has invalid column position %d", constraint, position)
		}
		column := byColumn[duckDBColumnIdentity(schema, table, name)]
		if column == nil {
			return fmt.Errorf("constraint %q references unknown column %s.%s.%s", constraint, schema, table, name)
		}
		if kind == "PRIMARY KEY" {
			column.Key = "PRI"
		} else if column.Key == "" {
			column.Key = "UNI"
		}
		return nil
	})
}

func duckDBColumnIdentity(schema, table, column string) string {
	return schema + "\x00" + table + "\x00" + column
}

func (db *DuckDBMetadataRepository) readMetadataPrimaryKeys(ctx context.Context, conn *sql.Conn, _ *catalog.Cache) (catalog.MetadataPatch, error) {
	query := duckDBConstraintColumnsQuery("c.constraint_type = 'PRIMARY KEY'")
	rows := make([]duckDBConstraintColumn, 0)
	err := duckDBMetadataRows(ctx, conn, query, []any{db.Catalog}, func(result *sql.Rows) error {
		var row duckDBConstraintColumn
		if err := result.Scan(&row.schema, &row.table, &row.kind, &row.name, &row.column, &row.position); err != nil {
			return err
		}
		if row.position < 1 {
			return fmt.Errorf("primary key %q has invalid column position %d", row.name, row.position)
		}
		rows = append(rows, row)
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].schema != rows[j].schema {
			return rows[i].schema < rows[j].schema
		}
		if rows[i].table != rows[j].table {
			return rows[i].table < rows[j].table
		}
		return rows[i].position < rows[j].position
	})
	primaryColumns := make(map[string]map[string]struct{})
	primaryKeysByID := make(map[string][]string)
	positions := make(map[string]map[int64]struct{})
	for _, row := range rows {
		relationKey := catalog.ColumnDatabaseKey(row.schema, row.table)
		if primaryColumns[relationKey] == nil {
			primaryColumns[relationKey] = make(map[string]struct{})
			positions[relationKey] = make(map[int64]struct{})
		}
		if _, duplicate := positions[relationKey][row.position]; duplicate {
			return catalog.MetadataPatch{}, fmt.Errorf("primary key on %s.%s has duplicate position %d", row.schema, row.table, row.position)
		}
		positions[relationKey][row.position] = struct{}{}
		primaryColumns[relationKey][row.column] = struct{}{}
		id := catalog.ObjectID{Schema: row.schema, Kind: catalog.ObjectKindTable, Name: row.table}.Key()
		primaryKeysByID[id] = append(primaryKeysByID[id], row.column)
	}
	return catalog.MetadataPatch{Cache: &catalog.Cache{PrimaryKeyColumns: primaryColumns, PrimaryKeysByID: primaryKeysByID}, Count: len(primaryKeysByID)}, nil
}

type duckDBForeignKeyRow struct {
	schema, table, name, referencedTable string
	sourceColumn, targetColumn           string
	position                             int64
}

func (db *DuckDBMetadataRepository) readMetadataForeignKeys(ctx context.Context, conn *sql.Conn, cache *catalog.Cache) (catalog.MetadataPatch, error) {
	const query = `
SELECT c.schema_name, c.table_name, c.constraint_name, c.referenced_table,
       source.column_name, source.position, target.column_name
FROM duckdb_constraints() AS c,
     duckdb_tables() AS t,
     UNNEST(c.constraint_column_names) WITH ORDINALITY AS source(column_name, position),
     UNNEST(c.referenced_column_names) WITH ORDINALITY AS target(column_name, position)
WHERE c.database_name = ?
  AND t.database_name = c.database_name
  AND t.schema_name = c.schema_name
  AND t.table_name = c.table_name
  AND NOT t.internal AND NOT t.temporary
  AND c.constraint_type = 'FOREIGN KEY'
  AND c.schema_name NOT IN ('information_schema', 'pg_catalog')
  AND source.position = target.position
ORDER BY c.schema_name, c.table_name, c.constraint_name, source.position`
	fkRows := make([]duckDBForeignKeyRow, 0)
	err := duckDBMetadataRows(ctx, conn, query, []any{db.Catalog}, func(rows *sql.Rows) error {
		var row duckDBForeignKeyRow
		if err := rows.Scan(&row.schema, &row.table, &row.name, &row.referencedTable,
			&row.sourceColumn, &row.position, &row.targetColumn); err != nil {
			return err
		}
		if row.name == "" || row.referencedTable == "" || row.position < 1 {
			return fmt.Errorf("foreign key has incomplete metadata: %+v", row)
		}
		fkRows = append(fkRows, row)
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	groupedRows := make(map[string][]duckDBForeignKeyRow)
	for _, row := range fkRows {
		identity := row.schema + "\x00" + row.table + "\x00" + row.name
		parts := groupedRows[identity]
		if len(parts) > 0 && parts[len(parts)-1].position == row.position {
			return catalog.MetadataPatch{}, fmt.Errorf("foreign key %q has duplicate segment position %d", row.name, row.position)
		}
		groupedRows[identity] = append(parts, row)
	}
	identities := make([]string, 0, len(groupedRows))
	for identity := range groupedRows {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	grouped := make(map[string]map[string][]*catalog.ForeignKey)
	for _, identity := range identities {
		parts := groupedRows[identity]
		schema := parts[0].schema
		targetSchema, targetTable := duckDBReferencedTable(schema, parts[0].referencedTable, cache)
		key := new(catalog.ForeignKey)
		for _, part := range parts {
			if part.referencedTable != parts[0].referencedTable {
				return catalog.MetadataPatch{}, fmt.Errorf("foreign key %q has inconsistent referenced tables", part.name)
			}
			source := &catalog.ColumnBase{Schema: schema, Table: part.table, Name: part.sourceColumn}
			target := &catalog.ColumnBase{Schema: targetSchema, Table: targetTable, Name: part.targetColumn}
			*key = append(*key, [2]*catalog.ColumnBase{source, target})
		}
		if err := catalog.ValidateForeignKey(key); err != nil {
			return catalog.MetadataPatch{}, fmt.Errorf("foreign key %q: %w", parts[0].name, err)
		}
		sourceKey := catalog.ColumnDatabaseKey(schema, parts[0].table)
		targetKey := catalog.ColumnDatabaseKey(targetSchema, targetTable)
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
	return catalog.MetadataPatch{Cache: &catalog.Cache{ForeignKeys: grouped}, Count: len(groupedRows)}, nil
}

func duckDBReferencedTable(sourceSchema, referencedTable string, cache *catalog.Cache) (string, string) {
	if table, ok := duckDBRelationInSchema(cache, sourceSchema, duckDBUnquoteIdentifier(referencedTable)); ok {
		return sourceSchema, table
	}
	for index := strings.IndexByte(referencedTable, '.'); index >= 0; {
		schemaName := duckDBUnquoteIdentifier(referencedTable[:index])
		tableName := duckDBUnquoteIdentifier(referencedTable[index+1:])
		if schema, ok := duckDBSchemaName(cache, schemaName); ok {
			if table, ok := duckDBRelationInSchema(cache, schema, tableName); ok {
				return schema, table
			}
		}
		next := strings.IndexByte(referencedTable[index+1:], '.')
		if next < 0 {
			break
		}
		index += next + 1
	}
	return sourceSchema, duckDBUnquoteIdentifier(referencedTable)
}

func duckDBSchemaName(cache *catalog.Cache, requested string) (string, bool) {
	if cache == nil {
		return "", false
	}
	if schema, ok := cache.Schemas[strings.ToUpper(requested)]; ok {
		return schema, true
	}
	return "", false
}

func duckDBRelationInSchema(cache *catalog.Cache, schema, requested string) (string, bool) {
	if cache == nil {
		return "", false
	}
	for _, relation := range cache.SchemaTables[strings.ToUpper(schema)] {
		if relation == requested || strings.EqualFold(relation, requested) {
			return relation, true
		}
	}
	return "", false
}

func duckDBUnquoteIdentifier(identifier string) string {
	if len(identifier) >= 2 && identifier[0] == '"' && identifier[len(identifier)-1] == '"' {
		return strings.ReplaceAll(identifier[1:len(identifier)-1], `""`, `"`)
	}
	return identifier
}

type duckDBMetadataIndex struct {
	desc       *catalog.IndexDesc
	positions  map[int64]string
	allColumns bool
}

func (db *DuckDBMetadataRepository) readMetadataIndexes(ctx context.Context, conn *sql.Conn, _ *catalog.Cache) (catalog.MetadataPatch, error) {
	const query = `
SELECT i.schema_name, i.table_name, i.index_name, i.is_unique,
       e.expression, e.position
FROM duckdb_indexes() AS i,
     duckdb_tables() AS t,
     UNNEST(CAST(i.expressions AS VARCHAR[])) WITH ORDINALITY AS e(expression, position)
WHERE i.database_name = ?
  AND t.database_name = i.database_name
  AND t.schema_name = i.schema_name
  AND t.table_name = i.table_name
  AND NOT t.internal AND NOT t.temporary
  AND i.schema_name NOT IN ('information_schema', 'pg_catalog')
ORDER BY i.schema_name, i.table_name, i.index_name, e.position`
	indexesByIdentity := make(map[string]*duckDBMetadataIndex)
	err := duckDBMetadataRows(ctx, conn, query, []any{db.Catalog}, func(rows *sql.Rows) error {
		var schema, table, name, expression string
		var unique bool
		var position int64
		if err := rows.Scan(&schema, &table, &name, &unique, &expression, &position); err != nil {
			return err
		}
		if position < 1 {
			return fmt.Errorf("index %q has invalid expression position %d", name, position)
		}
		identity := schema + "\x00" + table + "\x00" + name
		index := indexesByIdentity[identity]
		if index == nil {
			index = &duckDBMetadataIndex{desc: &catalog.IndexDesc{
				Schema: schema, Name: name, RelationName: table,
				Unique:  sql.NullBool{Bool: unique, Valid: true},
				Active:  sql.NullBool{Bool: true, Valid: true},
				Columns: make([]string, 0),
			}, positions: make(map[int64]string), allColumns: true}
			indexesByIdentity[identity] = index
		} else if index.desc.Unique.Bool != unique {
			return fmt.Errorf("index %q has inconsistent uniqueness across expressions", name)
		}
		if _, exists := index.positions[position]; exists {
			return fmt.Errorf("index %q has duplicate expression position %d", name, position)
		}
		index.positions[position] = expression
		if _, ok := duckDBBareColumnExpression(expression); !ok {
			index.allColumns = false
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
		expressions := make([]string, 0, len(positions))
		for _, position := range positions {
			expression := indexed.positions[position]
			expressions = append(expressions, expression)
			if indexed.allColumns {
				column, _ := duckDBBareColumnExpression(expression)
				indexed.desc.Columns = append(indexed.desc.Columns, column)
			}
		}
		if !indexed.allColumns {
			indexed.desc.Columns = make([]string, 0)
			indexed.desc.Expression = sql.NullString{String: strings.Join(expressions, ", "), Valid: true}
		}
		indexes = append(indexes, indexed.desc)
	}
	sort.Slice(indexes, func(i, j int) bool {
		if indexes[i].Schema != indexes[j].Schema {
			return indexes[i].Schema < indexes[j].Schema
		}
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
		byID[catalog.IndexObjectID(index.Schema, index.RelationName, index.Name).Key()] = index
		byTable[catalog.CatalogCacheKey(index.RelationName)] = append(byTable[catalog.CatalogCacheKey(index.RelationName)], index)
	}
	return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Indexes: byName, IndexesByTable: byTable, IndexesByID: byID}}, Count: len(indexes)}, nil
}

func duckDBBareColumnExpression(expression string) (string, bool) {
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return "", false
	}
	if expression[0] == '"' {
		var column strings.Builder
		for i := 1; i < len(expression); i++ {
			if expression[i] != '"' {
				column.WriteByte(expression[i])
				continue
			}
			if i+1 < len(expression) && expression[i+1] == '"' {
				column.WriteByte('"')
				i++
				continue
			}
			if strings.TrimSpace(expression[i+1:]) == "" && column.Len() > 0 {
				return column.String(), true
			}
			return "", false
		}
		return "", false
	}
	for i, r := range expression {
		if i == 0 {
			if !unicode.IsLetter(r) && r != '_' {
				return "", false
			}
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '$' {
			return "", false
		}
	}
	return expression, true
}
