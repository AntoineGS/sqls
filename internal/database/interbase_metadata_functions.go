package database

import (
	"context"
	"database/sql"
	"fmt"

	"interbase-go/schema"
)

// readMetadataFunctions reads UDF headers and arguments in two set-based
// statements. The returned catalog map is complete even when no UDF exists.
func (db *InterBaseDBRepository) readMetadataFunctions(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
	functions := make([]schema.Function, 0)
	byName := make(map[string]int)
	headerQuery := fmt.Sprintf(`
SELECT %s, f.RDB$FUNCTION_TYPE, f.RDB$DESCRIPTION, f.RDB$MODULE_NAME,
       f.RDB$ENTRYPOINT, f.RDB$RETURN_ARGUMENT, f.RDB$SYSTEM_FLAG
FROM RDB$FUNCTIONS f
WHERE COALESCE(f.RDB$SYSTEM_FLAG, 0) = 0
ORDER BY f.RDB$FUNCTION_NAME`, interBaseMetadataIdentifier("f.RDB$FUNCTION_NAME", width))
	err := interBaseBulkQuery(ctx, q, "function headers", headerQuery, func(rows *sql.Rows) error {
		var name sql.NullString
		var function schema.Function
		if err := rows.Scan(&name, &function.FunctionType, &function.Description, &function.ModuleName,
			&function.EntryPoint, &function.ReturnArgument, &function.SystemFlag); err != nil {
			return err
		}
		functionName, err := interBaseRequiredName(name, "function name")
		if err != nil {
			return err
		}
		function.Name = functionName
		function.ModuleName = trimInterBaseCatalogName(function.ModuleName)
		function.EntryPoint = trimInterBaseCatalogName(function.EntryPoint)
		if _, exists := byName[function.Name]; exists {
			return fmt.Errorf("duplicate function header %q", function.Name)
		}
		byName[function.Name] = len(functions)
		functions = append(functions, function)
		return nil
	})
	if err != nil {
		return MetadataPatch{}, err
	}

	functionName := interBaseMetadataIdentifier("a.RDB$FUNCTION_NAME", width)
	argumentQuery := fmt.Sprintf(`
SELECT %s, a.RDB$ARGUMENT_POSITION, a.RDB$MECHANISM, a.RDB$FIELD_LENGTH,
       a.RDB$FIELD_SCALE, a.RDB$FIELD_TYPE, a.RDB$FIELD_SUB_TYPE,
       a.RDB$CHARACTER_SET_ID, a.RDB$FIELD_PRECISION, a.RDB$CHARACTER_LENGTH
FROM RDB$FUNCTION_ARGUMENTS a
JOIN RDB$FUNCTIONS f ON f.RDB$FUNCTION_NAME = a.RDB$FUNCTION_NAME
WHERE COALESCE(f.RDB$SYSTEM_FLAG, 0) = 0
ORDER BY a.RDB$FUNCTION_NAME, a.RDB$ARGUMENT_POSITION`, functionName)
	err = interBaseBulkQuery(ctx, q, "function arguments", argumentQuery, func(rows *sql.Rows) error {
		var rawName sql.NullString
		var argument schema.FunctionArgument
		if err := rows.Scan(&rawName, &argument.Position, &argument.Mechanism,
			&argument.FieldLength, &argument.FieldScale, &argument.FieldType,
			&argument.FieldSubType, &argument.CharacterSetID, &argument.FieldPrecision,
			&argument.CharacterLength); err != nil {
			return err
		}
		name, err := interBaseRequiredName(rawName, "function argument function name")
		if err != nil {
			return err
		}
		parent, ok := byName[name]
		if !ok {
			return fmt.Errorf("function argument has no matching function header: %q", name)
		}
		argument.FunctionName = name
		argument.Name = name
		if argument.Position.Valid {
			argument.Name = fmt.Sprintf("%s_%d", name, argument.Position.Int64)
		}
		functions[parent].Arguments = append(functions[parent].Arguments, argument)
		return nil
	})
	if err != nil {
		return MetadataPatch{}, err
	}
	descriptions, err := db.functionDescriptions(functions)
	if err != nil {
		return MetadataPatch{}, err
	}
	byDescription := make(map[string]*FunctionDesc, len(descriptions))
	for _, description := range descriptions {
		byDescription[catalogCacheKey(description.Name)] = description
	}
	return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Functions: byDescription}}, Count: len(descriptions)}, nil
}
