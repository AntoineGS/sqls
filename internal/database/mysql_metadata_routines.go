package database

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/sqls-server/sqls/catalog"
)

type mysqlMetadataRoutine struct {
	schema, name string
	source       sql.NullString
	definer      sql.NullString
	comment      sql.NullString
}

type mysqlMetadataParameter struct {
	schema, routine, name, mode, dataType string
	position                              int64
}

func (db *MySQLMetadataRepository) readMetadataProcedures(ctx context.Context, tx *sql.Tx, schema string) (catalog.MetadataPatch, error) {
	const routinesQuery = `
SELECT ROUTINE_SCHEMA, ROUTINE_NAME, ROUTINE_TYPE, ROUTINE_DEFINITION, DEFINER, ROUTINE_COMMENT
FROM information_schema.ROUTINES
WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'PROCEDURE'
ORDER BY ROUTINE_SCHEMA, ROUTINE_NAME`
	routines := make(map[string]*catalog.ProcedureDesc)
	err := mysqlMetadataRows(ctx, tx, routinesQuery, schema, func(rows *sql.Rows) error {
		var routine mysqlMetadataRoutine
		var routineType string
		if err := rows.Scan(&routine.schema, &routine.name, &routineType, &routine.source, &routine.definer, &routine.comment); err != nil {
			return err
		}
		if routine.schema != schema || routineType != "PROCEDURE" {
			return nil
		}
		if _, exists := routines[routine.name]; exists {
			return fmt.Errorf("duplicate procedure %q", routine.name)
		}
		routines[routine.name] = &catalog.ProcedureDesc{
			Schema: routine.schema, Name: routine.name, OwnerName: routine.definer,
			Source: routine.source, Description: routine.comment,
			InputParameters:  make([]*catalog.ProcedureParameterDesc, 0),
			OutputParameters: make([]*catalog.ProcedureParameterDesc, 0),
		}
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	const parametersQuery = `
SELECT p.SPECIFIC_SCHEMA, p.SPECIFIC_NAME, p.PARAMETER_NAME, p.PARAMETER_MODE,
       p.DTD_IDENTIFIER, p.ORDINAL_POSITION
FROM information_schema.PARAMETERS AS p
JOIN information_schema.ROUTINES AS r
  ON r.ROUTINE_SCHEMA = p.SPECIFIC_SCHEMA
 AND r.ROUTINE_NAME = p.SPECIFIC_NAME
 AND r.SPECIFIC_NAME = p.SPECIFIC_NAME
WHERE p.SPECIFIC_SCHEMA = ? AND r.ROUTINE_TYPE = 'PROCEDURE'
ORDER BY p.SPECIFIC_SCHEMA, p.SPECIFIC_NAME, p.ORDINAL_POSITION`
	err = mysqlMetadataRows(ctx, tx, parametersQuery, schema, func(rows *sql.Rows) error {
		var parameter mysqlMetadataParameter
		var parameterName, mode sql.NullString
		if err := rows.Scan(&parameter.schema, &parameter.routine, &parameterName, &mode, &parameter.dataType, &parameter.position); err != nil {
			return err
		}
		if parameter.schema != schema {
			return nil
		}
		procedure := routines[parameter.routine]
		if procedure == nil {
			return fmt.Errorf("procedure parameter has no matching procedure %q", parameter.routine)
		}
		if !parameterName.Valid || parameterName.String == "" || !mode.Valid || parameter.position < 1 || parameter.position > int64(maxInt()) {
			return fmt.Errorf("procedure %q has invalid parameter metadata at position %d", parameter.routine, parameter.position)
		}
		parameter.name, parameter.mode = parameterName.String, strings.ToUpper(mode.String)
		descriptor := &catalog.ProcedureParameterDesc{
			Name: parameter.name, Position: int(parameter.position), Type: parameter.dataType,
		}
		switch parameter.mode {
		case "IN":
			descriptor.Direction = catalog.ParameterInput
			procedure.InputParameters = append(procedure.InputParameters, descriptor)
		case "OUT":
			descriptor.Direction = catalog.ParameterOutput
			procedure.OutputParameters = append(procedure.OutputParameters, descriptor)
		case "INOUT":
			input := *descriptor
			input.Direction = catalog.ParameterInput
			output := *descriptor
			output.Direction = catalog.ParameterOutput
			procedure.InputParameters = append(procedure.InputParameters, &input)
			procedure.OutputParameters = append(procedure.OutputParameters, &output)
		default:
			return fmt.Errorf("procedure %q parameter %q has unsupported mode %q", parameter.routine, parameter.name, parameter.mode)
		}
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	byName := make(map[string]*catalog.ProcedureDesc, len(routines))
	byID := make(map[string]*catalog.ProcedureDesc, len(routines))
	for _, routine := range routines {
		sort.SliceStable(routine.InputParameters, func(i, j int) bool { return routine.InputParameters[i].Position < routine.InputParameters[j].Position })
		sort.SliceStable(routine.OutputParameters, func(i, j int) bool {
			return routine.OutputParameters[i].Position < routine.OutputParameters[j].Position
		})
		byName[catalog.CatalogCacheKey(routine.Name)] = routine
		byID[catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: routine.Name}.Key()] = routine
	}
	return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Procedures: byName, ProceduresByID: byID}}, Count: len(routines)}, nil
}

func (db *MySQLMetadataRepository) readMetadataFunctions(ctx context.Context, tx *sql.Tx, schema string) (catalog.MetadataPatch, error) {
	const routinesQuery = `
SELECT ROUTINE_SCHEMA, ROUTINE_NAME, ROUTINE_TYPE, ROUTINE_DEFINITION, DEFINER, ROUTINE_COMMENT
FROM information_schema.ROUTINES
WHERE ROUTINE_SCHEMA = ? AND ROUTINE_TYPE = 'FUNCTION'
ORDER BY ROUTINE_SCHEMA, ROUTINE_NAME`
	routines := make(map[string]*catalog.FunctionDesc)
	err := mysqlMetadataRows(ctx, tx, routinesQuery, schema, func(rows *sql.Rows) error {
		var routine mysqlMetadataRoutine
		var routineType string
		if err := rows.Scan(&routine.schema, &routine.name, &routineType, &routine.source, &routine.definer, &routine.comment); err != nil {
			return err
		}
		if routine.schema != schema || routineType != "FUNCTION" {
			return nil
		}
		if _, exists := routines[routine.name]; exists {
			return fmt.Errorf("duplicate function %q", routine.name)
		}
		routines[routine.name] = &catalog.FunctionDesc{
			Schema: routine.schema, Name: routine.name,
			Arguments: make([]*catalog.FunctionArgumentDesc, 0),
		}
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	const parametersQuery = `
SELECT p.SPECIFIC_SCHEMA, p.SPECIFIC_NAME, p.PARAMETER_NAME, p.PARAMETER_MODE,
       p.DTD_IDENTIFIER, p.ORDINAL_POSITION
FROM information_schema.PARAMETERS AS p
JOIN information_schema.ROUTINES AS r
  ON r.ROUTINE_SCHEMA = p.SPECIFIC_SCHEMA
 AND r.ROUTINE_NAME = p.SPECIFIC_NAME
 AND r.SPECIFIC_NAME = p.SPECIFIC_NAME
WHERE p.SPECIFIC_SCHEMA = ? AND r.ROUTINE_TYPE = 'FUNCTION'
ORDER BY p.SPECIFIC_SCHEMA, p.SPECIFIC_NAME, p.ORDINAL_POSITION`
	err = mysqlMetadataRows(ctx, tx, parametersQuery, schema, func(rows *sql.Rows) error {
		var parameter mysqlMetadataParameter
		var parameterName, mode sql.NullString
		if err := rows.Scan(&parameter.schema, &parameter.routine, &parameterName, &mode, &parameter.dataType, &parameter.position); err != nil {
			return err
		}
		if parameter.schema != schema {
			return nil
		}
		function := routines[parameter.routine]
		if function == nil {
			return fmt.Errorf("function parameter has no matching function %q", parameter.routine)
		}
		if parameter.position < 0 || parameter.position > int64(maxInt()) {
			return fmt.Errorf("function %q has invalid parameter position %d", parameter.routine, parameter.position)
		}
		if parameter.position == 0 {
			function.ReturnType = parameter.dataType
			function.ReturnPosition = sql.NullInt64{Int64: parameter.position, Valid: true}
			return nil
		}
		if !parameterName.Valid || parameterName.String == "" {
			return fmt.Errorf("function %q parameter at position %d has no name", parameter.routine, parameter.position)
		}
		function.Arguments = append(function.Arguments, &catalog.FunctionArgumentDesc{
			Name: parameterName.String, FunctionName: parameter.routine,
			Position: sql.NullInt64{Int64: parameter.position, Valid: true},
			Type:     parameter.dataType,
		})
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	byName := make(map[string]*catalog.FunctionDesc, len(routines))
	byID := make(map[string]*catalog.FunctionDesc, len(routines))
	for _, routine := range routines {
		sort.SliceStable(routine.Arguments, func(i, j int) bool {
			return routine.Arguments[i].Position.Int64 < routine.Arguments[j].Position.Int64
		})
		byName[catalog.CatalogCacheKey(routine.Name)] = routine
		byID[catalog.ObjectID{Kind: catalog.ObjectKindFunction, Name: routine.Name}.Key()] = routine
	}
	return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Functions: byName, FunctionsByID: byID}}, Count: len(routines)}, nil
}

func (db *MySQLMetadataRepository) readMetadataTriggers(ctx context.Context, tx *sql.Tx, schema string) (catalog.MetadataPatch, error) {
	const query = `
SELECT TRIGGER_SCHEMA, TRIGGER_NAME, EVENT_OBJECT_SCHEMA, EVENT_OBJECT_TABLE,
       ACTION_TIMING, EVENT_MANIPULATION, ACTION_STATEMENT
FROM information_schema.TRIGGERS
WHERE TRIGGER_SCHEMA = ?
ORDER BY EVENT_OBJECT_SCHEMA, EVENT_OBJECT_TABLE, ACTION_ORDER, TRIGGER_NAME`
	triggers := make(map[string]*catalog.TriggerDesc)
	byID := make(map[string]*catalog.TriggerDesc)
	byTable := make(map[string][]*catalog.TriggerDesc)
	err := mysqlMetadataRows(ctx, tx, query, schema, func(rows *sql.Rows) error {
		var schemaName, name, objectSchema, table, timing, event string
		var source sql.NullString
		if err := rows.Scan(&schemaName, &name, &objectSchema, &table, &timing, &event, &source); err != nil {
			return err
		}
		if schemaName != schema || objectSchema != schema {
			return nil
		}
		if name == "" || table == "" {
			return fmt.Errorf("trigger in schema %q has an empty name or relation", schemaName)
		}
		desc := &catalog.TriggerDesc{
			Schema: schemaName, Name: name,
			RelationName: sql.NullString{String: table, Valid: true},
			Event:        strings.ToUpper(strings.TrimSpace(timing)) + " " + strings.ToUpper(strings.TrimSpace(event)),
			Active:       sql.NullBool{Bool: true, Valid: true}, Source: source,
		}
		triggers[catalog.CatalogCacheKey(name)] = desc
		byID[catalog.ObjectID{Kind: catalog.ObjectKindTrigger, Name: name}.Key()] = desc
		byTable[catalog.CatalogCacheKey(table)] = append(byTable[catalog.CatalogCacheKey(table)], desc)
		return nil
	})
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	for _, group := range byTable {
		sort.SliceStable(group, func(i, j int) bool { return group[i].Name < group[j].Name })
	}
	fragment := &catalog.CatalogCache{Triggers: triggers, TriggersByID: byID, TriggersByTable: byTable}
	return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: fragment}, Count: len(triggers)}, nil
}
