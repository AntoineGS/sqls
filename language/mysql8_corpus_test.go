package language_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/parser"
)

var mysql8Corpus = []string{
	"SELECT t.`select`, t.`odd``name` FROM `order` AS t LIMIT 10 OFFSET 5",
	"WITH ranked AS (SELECT id, ROW_NUMBER() OVER (PARTITION BY group_id ORDER BY id) AS rn FROM `events`) SELECT id FROM ranked WHERE rn = 1",
	"INSERT INTO `events` (`id`, `payload`) VALUES (1, JSON_OBJECT('a', 1)) ON DUPLICATE KEY UPDATE `payload` = VALUES(`payload`)",
	"REPLACE INTO `events` (`id`, `payload`) VALUES (2, '{}')",
	"SELECT `payload`->'$.a', `payload`->>'$.a' FROM `events`",
	"SHOW TABLES",
	"DESCRIBE `events`",
	"CREATE TABLE `mysql8_engine_probe` (`id` BIGINT UNSIGNED NOT NULL PRIMARY KEY) ENGINE=InnoDB",
}

func TestMySQL8CorpusParsesAndDiagnosticsHaveNoErrors(t *testing.T) {
	variant := dialect.DriverVariant{Driver: dialect.DatabaseDriverMySQL8}
	service := language.NewService()
	for i, statement := range mysql8Corpus {
		t.Run(fmt.Sprintf("statement-%d", i+1), func(t *testing.T) {
			parsed, err := parser.ParseWithDriverVariant(statement, variant)
			if err != nil {
				t.Fatalf("ParseWithDriverVariant() error = %v", err)
			}
			if !hasStatementTree(parsed) {
				t.Fatalf("statement produced no statement tree: %#v", parsed)
			}
			result, err := service.Diagnostics(context.Background(), language.Request{
				Document: language.Document{URI: fmt.Sprintf("memory:mysql8/%d", i), Text: statement, Version: 1},
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

func TestMySQL8CompletionUsesVariant(t *testing.T) {
	variant := dialect.DriverVariant{Driver: dialect.DatabaseDriverMySQL8}
	service := language.NewService()
	completion, err := service.Complete(context.Background(), language.Request{
		Document: language.Document{URI: "memory:mysql8/completion", Text: "SELECT ", Version: 1},
		Context:  language.Context{Variant: variant, DialectResolved: true},
	}, language.Position{UTF16Column: 7})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	found := false
	for _, item := range completion.Value.Items {
		found = found || item.Label == "JSON_EXTRACT"
	}
	if !found {
		t.Fatal("MySQL 8 completion did not include JSON_EXTRACT")
	}
}
