package database

import (
	"context"
	"database/sql"
	"fmt"

	"interbase-go/schema"
)

func (db *InterBaseDBRepository) readMetadataProcedures(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
	procedures := make([]schema.Procedure, 0)
	byName := make(map[string]int)
	var err error
	procedureName := interBaseMetadataIdentifier("p.RDB$PROCEDURE_NAME", width)
	ownerName := interBaseMetadataIdentifier("p.RDB$OWNER_NAME", width)
	headerQuery := fmt.Sprintf(`
SELECT %s, %s, p.RDB$PROCEDURE_SOURCE, p.RDB$DESCRIPTION
FROM RDB$PROCEDURES p
WHERE COALESCE(p.RDB$SYSTEM_FLAG, 0) = 0
ORDER BY p.RDB$PROCEDURE_NAME`, procedureName, ownerName)
	err = interBaseBulkQuery(ctx, q, "procedure headers", headerQuery, func(rows *sql.Rows) error {
		var rawName, owner sql.NullString
		var procedure schema.Procedure
		if err := rows.Scan(&rawName, &owner, &procedure.Source, &procedure.Description); err != nil {
			return err
		}
		name, err := interBaseRequiredName(rawName, "procedure name")
		if err != nil {
			return err
		}
		procedure.Name = name
		procedure.OwnerName = trimInterBaseCatalogName(owner)
		if _, exists := byName[name]; exists {
			return fmt.Errorf("duplicate procedure header %q", name)
		}
		byName[name] = len(procedures)
		procedures = append(procedures, procedure)
		return nil
	})
	if err != nil {
		return MetadataPatch{}, err
	}

	parameterName := interBaseMetadataIdentifier("pp.RDB$PARAMETER_NAME", width)
	parameterProcedureName := interBaseMetadataIdentifier("pp.RDB$PROCEDURE_NAME", width)
	fieldSource := interBaseMetadataIdentifier("pp.RDB$FIELD_SOURCE", width)
	domainName := interBaseMetadataIdentifier("f.RDB$FIELD_NAME", width)
	characterSetName := interBaseMetadataIdentifier("cs.RDB$CHARACTER_SET_NAME", width)
	collationName := interBaseMetadataIdentifier("co.RDB$COLLATION_NAME", width)
	parametersQuery := fmt.Sprintf(`
SELECT %s, %s, pp.RDB$PARAMETER_NUMBER, pp.RDB$PARAMETER_TYPE,
       %s, pp.RDB$DESCRIPTION, pp.RDB$SYSTEM_FLAG,
       f.RDB$VALIDATION_SOURCE, f.RDB$COMPUTED_SOURCE, f.RDB$DEFAULT_SOURCE,
       f.RDB$FIELD_LENGTH, f.RDB$FIELD_SCALE, f.RDB$FIELD_TYPE, f.RDB$FIELD_SUB_TYPE,
       f.RDB$DESCRIPTION, f.RDB$SYSTEM_FLAG, f.RDB$SEGMENT_LENGTH,
       f.RDB$EXTERNAL_LENGTH, f.RDB$EXTERNAL_SCALE, f.RDB$EXTERNAL_TYPE,
       f.RDB$DIMENSIONS, f.RDB$NULL_FLAG, f.RDB$CHARACTER_LENGTH,
       f.RDB$COLLATION_ID, f.RDB$CHARACTER_SET_ID, f.RDB$FIELD_PRECISION,
       %s, %s, %s
FROM RDB$PROCEDURE_PARAMETERS pp
JOIN RDB$PROCEDURES p ON p.RDB$PROCEDURE_NAME = pp.RDB$PROCEDURE_NAME
LEFT JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = pp.RDB$FIELD_SOURCE
LEFT JOIN RDB$CHARACTER_SETS cs ON cs.RDB$CHARACTER_SET_ID = f.RDB$CHARACTER_SET_ID
LEFT JOIN RDB$COLLATIONS co ON co.RDB$CHARACTER_SET_ID = f.RDB$CHARACTER_SET_ID
                         AND co.RDB$COLLATION_ID = f.RDB$COLLATION_ID
WHERE COALESCE(p.RDB$SYSTEM_FLAG, 0) = 0
ORDER BY pp.RDB$PROCEDURE_NAME, pp.RDB$PARAMETER_TYPE, pp.RDB$PARAMETER_NUMBER`,
		parameterName, parameterProcedureName, fieldSource, domainName, characterSetName, collationName)
	positions := make(map[string]map[schema.ParameterDirection]map[int64]struct{})
	err = interBaseBulkQuery(ctx, q, "procedure parameters", parametersQuery, func(rows *sql.Rows) error {
		parameter, domain, err := scanInterBaseProcedureParameter(rows)
		if err != nil {
			return err
		}
		parent, ok := byName[parameter.ProcedureName]
		if !ok {
			return fmt.Errorf("procedure parameter %q has no matching procedure header: %q", parameter.Name, parameter.ProcedureName)
		}
		if !parameter.Number.Valid || parameter.Number.Int64 < 0 || parameter.Number.Int64 > int64(maxInt()) {
			return fmt.Errorf("procedure %q parameter %q has invalid position %v", parameter.ProcedureName, parameter.Name, parameter.Number)
		}
		seen := positions[parameter.ProcedureName]
		if seen == nil {
			seen = make(map[schema.ParameterDirection]map[int64]struct{})
			positions[parameter.ProcedureName] = seen
		}
		directionPositions := seen[parameter.Direction]
		if directionPositions == nil {
			directionPositions = make(map[int64]struct{})
			seen[parameter.Direction] = directionPositions
		}
		if _, duplicate := directionPositions[parameter.Number.Int64]; duplicate {
			return fmt.Errorf("procedure %q has duplicate %s parameter position %d", parameter.ProcedureName, parameter.Direction, parameter.Number.Int64)
		}
		directionPositions[parameter.Number.Int64] = struct{}{}
		parameter.Domain = domain
		if domain != nil && domain.Nullable.Valid && !domain.Nullable.Bool {
			parameter.Nullable = sql.NullBool{Bool: false, Valid: true}
		}
		if parameter.Direction == schema.ParameterInput {
			procedures[parent].InputParameters = append(procedures[parent].InputParameters, parameter)
		} else {
			procedures[parent].OutputParameters = append(procedures[parent].OutputParameters, parameter)
		}
		return nil
	})
	if err != nil {
		return MetadataPatch{}, err
	}
	descriptions := db.procedureDescriptions(procedures)
	byDescription := make(map[string]*ProcedureDesc, len(descriptions))
	for _, description := range descriptions {
		byDescription[description.Name] = description
	}
	return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Procedures: byDescription}}, Count: len(descriptions)}, nil
}

func scanInterBaseProcedureParameter(rows *sql.Rows) (schema.ProcedureParameter, *schema.Domain, error) {
	var parameter schema.ProcedureParameter
	var rawName, rawProcedureName, rawSource sql.NullString
	var rawDirection sql.NullInt64
	var domainName sql.NullString
	var domain schema.Domain
	if err := rows.Scan(&rawName, &rawProcedureName, &parameter.Number, &rawDirection, &rawSource,
		&parameter.Description, &parameter.SystemFlag,
		&domain.ValidationSource, &domain.ComputedSource, &domain.DefaultSource,
		&domain.FieldLength, &domain.FieldScale, &domain.FieldType, &domain.FieldSubType,
		&domain.Description, &domain.SystemFlag, &domain.SegmentLength,
		&domain.ExternalLength, &domain.ExternalScale, &domain.ExternalType,
		&domain.Dimensions, &domain.NullFlag, &domain.CharacterLength,
		&domain.CollationID, &domain.CharacterSetID, &domain.FieldPrecision,
		&domainName, &domain.CharacterSetName, &domain.CollationName); err != nil {
		return schema.ProcedureParameter{}, nil, err
	}
	name, err := interBaseRequiredName(rawName, "parameter name")
	if err != nil {
		return schema.ProcedureParameter{}, nil, err
	}
	procedureName, err := interBaseRequiredName(rawProcedureName, "parameter procedure name")
	if err != nil {
		return schema.ProcedureParameter{}, nil, err
	}
	var direction schema.ParameterDirection
	switch {
	case rawDirection.Valid && rawDirection.Int64 == 0:
		direction = schema.ParameterInput
	case rawDirection.Valid && rawDirection.Int64 == 1:
		direction = schema.ParameterOutput
	default:
		return schema.ProcedureParameter{}, nil, fmt.Errorf("parameter %q has invalid parameter type %v", name, rawDirection)
	}
	parameter.Name, parameter.ProcedureName = name, procedureName
	parameter.Direction, parameter.ParameterType = direction, rawDirection
	parameter.FieldSource = trimInterBaseCatalogName(rawSource)
	if !domainName.Valid {
		return parameter, nil, nil
	}
	domain.Name, err = interBaseRequiredName(domainName, "domain name")
	if err != nil {
		return schema.ProcedureParameter{}, nil, err
	}
	domain.CharacterSetName = trimInterBaseCatalogName(domain.CharacterSetName)
	domain.CollationName = trimInterBaseCatalogName(domain.CollationName)
	if domain.NullFlag.Valid {
		domain.Nullable = sql.NullBool{Bool: domain.NullFlag.Int64 == 0, Valid: true}
	}
	return parameter, &domain, nil
}
