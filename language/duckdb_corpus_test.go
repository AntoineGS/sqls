package language_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/parser"
)

var duckdbCorpus = []string{
	`SELECT "odd""name", o.id FROM main.orders AS o WHERE o.id > 1 ORDER BY 1 LIMIT 10 OFFSET 5`,
	`SELECT * FROM sales."MixedCase"`,
	`WITH r AS (SELECT id, row_number() OVER (PARTITION BY cid ORDER BY id) AS rn FROM orders) SELECT id FROM r WHERE rn = 1`,
	`SELECT id, row_number() OVER (ORDER BY id) AS rn FROM orders QUALIFY rn = 1`,
	`FROM orders`,
	`SELECT * EXCLUDE (secret) FROM customers`,
	`SELECT 'it''s', E'a\nb', $$dollar$$ AS d`,
	`INSERT INTO seq_t (v) VALUES ('x') RETURNING id`,
	`UPDATE orders SET total = total + 1 WHERE id = 1`,
	`DELETE FROM orders WHERE id = 2`,
	`CREATE TABLE probe (id INTEGER PRIMARY KEY, tags VARCHAR[], s STRUCT(a INTEGER))`,
	`SELECT [1, 2, 3] AS l, {'a': 1} AS s, 1::HUGEINT AS h`,
	`DESCRIBE orders`,
	`SUMMARIZE orders`,
	`PIVOT orders ON status USING sum(total)`,
}

func TestDuckDBCorpusParsesAndDiagnosticsHaveNoErrors(t *testing.T) {
	variant := dialect.DriverVariant{Driver: dialect.DatabaseDriverDuckDB}
	service := language.NewService()
	for i, statement := range duckdbCorpus {
		t.Run(fmt.Sprintf("statement-%d", i+1), func(t *testing.T) {
			parsed, err := parser.ParseWithDriverVariant(statement, variant)
			if err != nil {
				t.Fatalf("ParseWithDriverVariant() error = %v", err)
			}
			if !hasStatementTree(parsed) {
				t.Fatalf("statement produced no statement tree: %#v", parsed)
			}
			result, err := service.Diagnostics(context.Background(), language.Request{
				Document: language.Document{URI: fmt.Sprintf("memory:duckdb/%d", i), Text: statement, Version: 1},
				Context:  language.Context{Variant: variant, DialectResolved: true},
			})
			if err != nil {
				t.Fatalf("Diagnostics() error = %v", err)
			}
			for _, diagnostic := range result.Value {
				if diagnostic.Severity == 1 {
					t.Errorf("error-severity diagnostic for %q: %+v", statement, diagnostic)
				}
			}
		})
	}
}

func TestDuckDBCompletionUsesVariant(t *testing.T) {
	variant := dialect.DriverVariant{Driver: dialect.DatabaseDriverDuckDB}
	service := language.NewService()
	for _, tc := range []struct {
		name     string
		text     string
		position int
		want     string
	}{
		{name: "keyword", text: "SELECT 1 FROM t ", position: len("SELECT 1 FROM t "), want: "QUALIFY"},
		{name: "function", text: "SELECT ", position: len("SELECT "), want: "read_parquet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			completion, err := service.Complete(context.Background(), language.Request{
				Document: language.Document{URI: "memory:duckdb/completion/" + tc.name, Text: tc.text, Version: 1},
				Context:  language.Context{Variant: variant, DialectResolved: true},
			}, language.Position{UTF16Column: tc.position})
			if err != nil {
				t.Fatalf("Complete() error = %v", err)
			}
			for _, item := range completion.Value.Items {
				if strings.EqualFold(item.Label, tc.want) {
					return
				}
			}
			t.Fatalf("completion did not include %q", tc.want)
		})
	}
}

func TestDuckDBVocabularyIncludesRequiredKeywordsAndFunctions(t *testing.T) {
	for _, keyword := range []string{
		"QUALIFY", "PIVOT", "UNPIVOT", "ASOF", "POSITIONAL", "EXCLUDE", "REPLACE",
		"COLUMNS", "SUMMARIZE", "DESCRIBE", "MACRO", "SEQUENCE", "ANTI", "SEMI",
		"STRUCT", "MAP", "UNION",
	} {
		if !containsFold(dialect.DataBaseKeywords(dialect.DatabaseDriverDuckDB), keyword) {
			t.Errorf("DuckDB keyword list is missing %q", keyword)
		}
	}
	for _, function := range []string{
		"read_parquet", "read_csv", "read_json", "list_value", "struct_pack", "strftime",
		"date_trunc", "regexp_matches", "string_agg", "unnest", "range", "generate_series",
	} {
		if !containsFold(dialect.DataBaseFunctions(dialect.DatabaseDriverDuckDB), function) {
			t.Errorf("DuckDB function list is missing %q", function)
		}
	}
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}
