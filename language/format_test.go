package language_test

import (
	"context"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
)

func TestFormatProducesOneEdit(t *testing.T) {
	req := language.Request{
		Document: language.Document{URI: "memory:one", Text: "select * from t", Version: 1},
		Context:  language.Context{Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}},
	}
	result, err := language.NewService().Format(context.Background(), req, language.FormatOptions{TabSize: 4, InsertSpaces: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Value) != 1 {
		t.Fatalf("edits = %d, want 1", len(result.Value))
	}
}

func TestFormatUppercasesKeywordsOnly(t *testing.T) {
	req := language.Request{
		Document: language.Document{URI: "memory:case", Text: "select Name from t where a.active = :id", Version: 1},
		Context:  language.Context{Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}},
	}
	result, err := language.NewService().Format(context.Background(), req, language.FormatOptions{TabSize: 2, InsertSpaces: true, UppercaseKeywords: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := "SELECT\n  Name\nFROM\n  t\nWHERE\n  a.active = :id"; len(result.Value) != 1 || result.Value[0].Text != want {
		t.Fatalf("edits = %#v, want one edit with %q", result.Value, want)
	}
}

// The formatter invents a closing parenthesis here; Format must refuse
// rather than return text that differs from the document.
func TestFormatRefusesUnsafeOutput(t *testing.T) {
	req := language.Request{
		Document: language.Document{URI: "memory:unsafe", Text: "select (a from t", Version: 1},
		Context:  language.Context{Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}},
	}
	result, err := language.NewService().Format(context.Background(), req, language.FormatOptions{TabSize: 2, InsertSpaces: true})
	if err == nil || len(result.Value) != 0 {
		t.Fatalf("Format() = %#v, %v; want no edits and an error", result.Value, err)
	}
}
