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
