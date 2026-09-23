package handler

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/queryparams"
)

const parameterDescriptionTimeout = 3 * time.Second

type parameterTypeCandidate struct {
	wireType     string
	databaseType string
	hasLabel     bool
}

// inferParameterTypes matches each compiled input occurrence to the driver
// descriptor at the same position. Any unsupported, failed, or conflicting
// occurrence leaves that unique parameter for the client's type picker.
func inferParameterTypes(
	ctx context.Context,
	batch queryparams.Batch,
	describe func(context.Context, string) ([]database.InputDescriptor, error),
	sqlDialect int,
) ([]queryparams.Parameter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parameters := append([]queryparams.Parameter(nil), batch.Parameters...)
	if len(parameters) == 0 || describe == nil {
		return parameters, nil
	}

	candidates := make(map[string]parameterTypeCandidate, len(parameters))
	uncertain := make(map[string]bool, len(parameters))
	for _, statement := range batch.Statements {
		if len(statement.Keys) == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		statementCtx, cancel := context.WithTimeout(ctx, parameterDescriptionTimeout)
		descriptors, err := describe(statementCtx, statement.SQL)
		cancel()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err != nil || len(descriptors) != len(statement.Keys) {
			for _, key := range statement.Keys {
				uncertain[key] = true
			}
			continue
		}

		for i, key := range statement.Keys {
			wireType, databaseType, ok := inferredInputType(descriptors[i], sqlDialect)
			if !ok {
				uncertain[key] = true
				continue
			}
			candidate, exists := candidates[key]
			if !exists {
				candidates[key] = parameterTypeCandidate{
					wireType:     wireType,
					databaseType: databaseType,
					hasLabel:     databaseType != "",
				}
				continue
			}
			if candidate.wireType != wireType {
				uncertain[key] = true
				continue
			}
			if candidate.hasLabel && candidate.databaseType != databaseType {
				candidate.databaseType = ""
				candidate.hasLabel = false
				candidates[key] = candidate
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for i := range parameters {
		key := parameters[i].Key
		candidate, ok := candidates[key]
		if !ok || uncertain[key] {
			continue
		}
		parameters[i].InferredType = candidate.wireType
		if candidate.hasLabel {
			parameters[i].DatabaseType = candidate.databaseType
		}
	}
	return parameters, nil
}

func inferredInputType(descriptor database.InputDescriptor, sqlDialect int) (wireType, databaseType string, ok bool) {
	kind := strings.ToUpper(strings.TrimSpace(descriptor.Kind))
	if kind == "" || strings.Contains(kind, "OCTETS") || strings.Contains(kind, "BINARY") {
		return "", "", false
	}

	switch kind {
	case "CHAR", "CHARACTER", "VARCHAR", "CHARACTER VARYING", "NCHAR", "NVARCHAR":
		return "text", kind, true
	case "SMALLINT", "INTEGER", "BIGINT", "SHORT", "LONG", "INT64":
		if descriptor.Scale == 0 {
			if descriptor.Subtype == 1 || descriptor.Subtype == 2 {
				if sqlDialect != 3 || descriptor.Precision > 18 {
					return "", "", false
				}
				return "integer", exactNumericLabel(descriptor), true
			}
			if descriptor.Subtype != 0 {
				return "", "", false
			}
			return "integer", kind, true
		}
		if descriptor.Scale > 0 || (descriptor.Subtype != 1 && descriptor.Subtype != 2) || sqlDialect != 3 {
			return "", "", false
		}
		return "text", exactNumericLabel(descriptor), true
	case "BLOB":
		if descriptor.Subtype == 1 {
			return "text", "BLOB SUB_TYPE TEXT", true
		}
		return "", "", false
	case "NUMERIC", "DECIMAL":
		if sqlDialect != 3 {
			return "", "", false
		}
		subtype := descriptor.Subtype
		if subtype == 0 {
			if kind == "DECIMAL" {
				subtype = 2
			} else {
				subtype = 1
			}
		}
		if subtype != 1 && subtype != 2 || descriptor.Scale > 0 {
			return "", "", false
		}
		if descriptor.Scale < 0 {
			if sqlDialect != 3 {
				return "", "", false
			}
			return "text", exactNumericLabelWithSubtype(descriptor, subtype), true
		}
		if descriptor.Precision <= 0 || descriptor.Precision > 18 {
			return "", "", false
		}
		return "integer", exactNumericLabelWithSubtype(descriptor, subtype), true
	case "FLOAT", "REAL", "DOUBLE", "DOUBLE PRECISION", "D_FLOAT":
		return "number", kind, true
	case "DATE":
		return "date", kind, true
	case "TIMESTAMP":
		if sqlDialect == 1 {
			return "", "", false
		}
		return "timestamp", kind, true
	case "BOOLEAN":
		return "boolean", kind, true
	default:
		// TIME, BLOB, ARRAY, and unknown descriptor kinds have no safe
		// existing conversion in queryparams.Bind.
		return "", "", false
	}
}

func exactNumericLabel(descriptor database.InputDescriptor) string {
	subtype := descriptor.Subtype
	if subtype != 1 && subtype != 2 {
		return ""
	}
	return exactNumericLabelWithSubtype(descriptor, subtype)
}

func exactNumericLabelWithSubtype(descriptor database.InputDescriptor, subtype int) string {
	name := "NUMERIC"
	if subtype == 2 {
		name = "DECIMAL"
	}
	precision := descriptor.Precision
	if precision <= 0 {
		switch strings.ToUpper(strings.TrimSpace(descriptor.Kind)) {
		case "SMALLINT", "SHORT":
			precision = 4
		case "INTEGER", "LONG":
			precision = 9
		case "BIGINT", "INT64":
			precision = 18
		}
	}
	if precision <= 0 {
		return name
	}
	scale := descriptor.Scale
	if scale < 0 {
		scale = -scale
	}
	return name + "(" + strconv.Itoa(precision) + "," + strconv.Itoa(scale) + ")"
}
