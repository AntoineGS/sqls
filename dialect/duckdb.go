package dialect

var duckdbKeywords = []string{
	"ANTI",
	"ASOF",
	"COLUMNS",
	"DESCRIBE",
	"EXCLUDE",
	"MACRO",
	"MAP",
	"PIVOT",
	"POSITIONAL",
	"QUALIFY",
	"REPLACE",
	"SEMI",
	"SEQUENCE",
	"STRUCT",
	"SUMMARIZE",
	"UNION",
	"UNPIVOT",
}

var duckdbFunctions = []string{
	"date_trunc",
	"generate_series",
	"list_value",
	"range",
	"read_csv",
	"read_json",
	"read_parquet",
	"regexp_matches",
	"string_agg",
	"strftime",
	"struct_pack",
	"unnest",
}
