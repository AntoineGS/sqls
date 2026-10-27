package dialect

import "sort"

var firebird25Keywords = []string{
	"AUTONOMOUS",
	"BLOCK",
	"BOTH",
	"COMMON",
	"CONNECT",
	"CROSS",
	"DISCONNECT",
	"GLOBAL",
	"IIF",
	"INSENSITIVE",
	"LEADING",
	"LIST",
	"MAPPING",
	"MATCHED",
	"MATCHING",
	"MERGE",
	"NEXT",
	"PRESERVE",
	"RECREATE",
	"RECURSIVE",
	"RELEASE",
	"RESTART",
	"RETURNING",
	"ROWS",
	"SAVEPOINT",
	"SEQUENCE",
	"SIMILAR",
	"TEMPORARY",
	"TRAILING",
	"TRANSACTION",
}

var firebird25Functions = []string{
	"ABS",
	"ACOS",
	"ASCII_CHAR",
	"ASCII_VAL",
	"ASIN",
	"ATAN",
	"ATAN2",
	"BIN_AND",
	"BIN_NOT",
	"BIN_OR",
	"BIN_SHL",
	"BIN_SHR",
	"BIN_XOR",
	"BIT_LENGTH",
	"CEIL",
	"CEILING",
	"CHARACTER_LENGTH",
	"CHAR_LENGTH",
	"CHAR_TO_UUID",
	"COALESCE",
	"COS",
	"COSH",
	"COT",
	"DATEADD",
	"DATEDIFF",
	"DECODE",
	"EXP",
	"FLOOR",
	"GEN_ID",
	"GEN_UUID",
	"HASH",
	"IIF",
	"LEFT",
	"LIST",
	"LN",
	"LOG",
	"LOG10",
	"LOWER",
	"LPAD",
	"MAXVALUE",
	"MINVALUE",
	"MOD",
	"NULLIF",
	"OCTET_LENGTH",
	"OVERLAY",
	"PI",
	"POSITION",
	"POWER",
	"RAND",
	"RDB$GET_CONTEXT",
	"RDB$SET_CONTEXT",
	"REPLACE",
	"REVERSE",
	"RIGHT",
	"ROUND",
	"RPAD",
	"SIGN",
	"SIN",
	"SINH",
	"SQRT",
	"SUBSTRING",
	"TAN",
	"TANH",
	"TRIM",
	"TRUNC",
	"UPPER",
	"UUID_TO_CHAR",
}

func mergeSortedUnique(left, right []string) []string {
	merged := make([]string, 0, len(left)+len(right))
	merged = append(merged, left...)
	merged = append(merged, right...)
	sort.Strings(merged)

	unique := merged[:0]
	for _, value := range merged {
		if len(unique) == 0 || unique[len(unique)-1] != value {
			unique = append(unique, value)
		}
	}
	return unique
}
