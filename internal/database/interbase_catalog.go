package database

import (
	"fmt"
	"strings"

	"interbase-go/schema"
)

// interBaseTypeName renders the column/parameter type for sqlDialect, in the
// full form that includes any CHARACTER SET and COLLATE suffix.
//
// The rules, in order (spec §4.3):
//
//  1. A nil domain renders nothing: the catalog row resolved to no RDB$FIELDS
//     entry, so there is no type to report.
//  2. Under SQL Dialect 1, field type 35 is DATE. Dialect 1 has no separate
//     TIMESTAMP, and this is the only genuinely dialect-dependent rule.
//  3. Otherwise delegate to Domain.SQLType(), which is Dialect 3 correct and
//     renders charset, collation and validated precision/scale.
//  4. On error, fall back to the retained interBaseColumnType switch rather
//     than to TYPE(n). SQLType() returns ErrUnsupportedDDL on a dozen paths the
//     switch renders correctly today, so the fallback is what makes the
//     migration behavior preserving: SQLType() can upgrade a rendering and the
//     switch guarantees none regresses.
func interBaseTypeName(domain *schema.Domain, sqlDialect int) string {
	if domain == nil {
		return ""
	}
	if sqlDialect == 1 && domain.FieldType.Valid && domain.FieldType.Int64 == interBaseFieldTypeTimestamp {
		return "DATE"
	}
	if rendered, err := domain.SQLType(); err == nil {
		return rendered
	}
	return interBaseColumnType(domain)
}

// interBaseColumnTypeName renders the one-line form used for ColumnDesc.Type.
// SQLType appends CHARACTER SET and COLLATE clauses and the completion detail
// line must stay short, so the text is cut at the first of them. The cut is
// safe: no base type name contains either keyword.
func interBaseColumnTypeName(domain *schema.Domain, sqlDialect int) string {
	rendered := interBaseTypeName(domain, sqlDialect)
	if index := strings.Index(rendered, " CHARACTER SET "); index >= 0 {
		rendered = rendered[:index]
	}
	if index := strings.Index(rendered, " COLLATE "); index >= 0 {
		rendered = rendered[:index]
	}
	return rendered
}

// interBaseFieldTypeTimestamp is RDB$FIELD_TYPE 35, which Dialect 3 calls
// TIMESTAMP and Dialect 1 calls DATE.
const interBaseFieldTypeTimestamp = 35

// interBaseColumnType is the fixed-dialect type switch sqls has always used,
// re-sourced to read schema.Domain instead of a hand-scanned catalog row. Its
// logic is unchanged. It is retained as the exhaustive fallback for
// interBaseTypeName; TYPE(n) is reached exactly where it is reached today.
func interBaseColumnType(domain *schema.Domain) string {
	if domain == nil || !domain.FieldType.Valid {
		return ""
	}
	fieldType := domain.FieldType.Int64
	switch fieldType {
	case 7:
		return interBaseNumericType("SMALLINT", 4, domain)
	case 8:
		return interBaseNumericType("INTEGER", 9, domain)
	case 9:
		return "QUAD"
	case 10:
		return "FLOAT"
	case 12:
		return "DATE"
	case 13:
		return "TIME"
	case 14:
		return fmt.Sprintf("CHAR(%d)", interBaseCharacterLength(domain))
	case 16:
		return interBaseNumericType("BIGINT", 18, domain)
	case 17:
		return "BOOLEAN"
	case 27:
		// In Dialect 1, scaled NUMERIC/DECIMAL values can use DOUBLE
		// PRECISION as their underlying field type. A subtype without a
		// negative scale does not carry a fixed-point declaration, so keep
		// the underlying DOUBLE PRECISION rather than inventing (15, 0).
		if domain.FieldScale.Valid && domain.FieldScale.Int64 < 0 {
			return interBaseNumericType("DOUBLE PRECISION", 15, domain)
		}
		return "DOUBLE PRECISION"
	case interBaseFieldTypeTimestamp:
		// Dialect 1 uses field type 35 for DATE. The dialect 3 distinction is
		// made by interBaseTypeName, which reaches this switch only as a
		// fallback.
		return "DATE"
	case 37:
		return fmt.Sprintf("VARCHAR(%d)", interBaseCharacterLength(domain))
	case 40:
		return fmt.Sprintf("CSTRING(%d)", interBaseCharacterLength(domain))
	case 45:
		return "BLOB_ID"
	case 261:
		return "BLOB"
	default:
		return fmt.Sprintf("TYPE(%d)", fieldType)
	}
}

func interBaseCharacterLength(domain *schema.Domain) int64 {
	if domain == nil {
		return 0
	}
	if domain.CharacterLength.Valid {
		return domain.CharacterLength.Int64
	}
	if domain.FieldLength.Valid {
		return domain.FieldLength.Int64
	}
	return 0
}

func interBaseNumericType(base string, naturalPrecision int64, domain *schema.Domain) string {
	subtype := int64(0)
	if domain.FieldSubType.Valid {
		subtype = domain.FieldSubType.Int64
	}
	scale := int64(0)
	if domain.FieldScale.Valid {
		scale = domain.FieldScale.Int64
	}
	if subtype == 0 && scale >= 0 {
		return base
	}

	precision := naturalPrecision
	if domain.FieldPrecision.Valid && domain.FieldPrecision.Int64 > 0 {
		precision = domain.FieldPrecision.Int64
	}
	numericName := "NUMERIC"
	if subtype == 2 {
		numericName = "DECIMAL"
	}
	if scale < 0 {
		scale = -scale
	}
	return fmt.Sprintf("%s(%d, %d)", numericName, precision, scale)
}
