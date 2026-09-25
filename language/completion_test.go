package language_test

import (
	"context"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
)

func TestLanguageUsesSuppliedSnapshot(t *testing.T) {
	cache := &catalog.Cache{SchemaTables: map[string][]string{"": {"CUSTOMERS"}}}
	snap := &catalog.MetadataSnapshot{Generation: 1, Revision: 1, Cache: cache}
	req := language.Request{
		Document: language.Document{URI: "memory:one", Text: "SELECT * FROM ", Version: 1, Binding: 1},
		Context:  language.Context{Snapshot: snap, Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}, DialectResolved: true},
	}
	result, err := language.NewService().Complete(context.Background(), req, language.Position{UTF16Column: 14})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.Value.Items {
		found = found || item.Label == "CUSTOMERS"
	}
	if !found {
		t.Fatal("did not consume supplied catalog")
	}
	if req.Context.Snapshot.Cache != cache {
		t.Fatal("snapshot replaced")
	}
}
