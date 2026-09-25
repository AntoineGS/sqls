package language_test

import (
	"context"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
)

func TestSymbolsReportsProcedureLocals(t *testing.T) {
	text := "CREATE PROCEDURE P AS DECLARE VARIABLE X INTEGER; BEGIN X = 1; END"
	req := language.Request{
		Document: language.Document{URI: "memory:one", Text: text, Version: 1},
		Context:  language.Context{Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}},
	}
	result, err := language.NewService().Symbols(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, symbol := range result.Value {
		if symbol.Name == "X" && symbol.Kind == language.SymbolKindVariable {
			found = true
		}
	}
	if !found {
		t.Fatalf("did not report local X: %+v", result.Value)
	}
}

func TestSymbolsEmptyForNonInterBase(t *testing.T) {
	req := language.Request{
		Document: language.Document{URI: "memory:one", Text: "SELECT 1", Version: 1},
		Context:  language.Context{Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverMySQL}},
	}
	result, err := language.NewService().Symbols(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Value) != 0 {
		t.Fatalf("symbols = %+v, want empty", result.Value)
	}
}
