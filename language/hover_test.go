package language_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
)

func hoverCache() *catalog.Cache {
	return &catalog.Cache{
		DefaultSchema: "",
		SchemaTables:  map[string][]string{"": {"CITY"}},
		ColumnsWithParent: map[string][]*catalog.ColumnDesc{
			catalog.ColumnDatabaseKey("", "CITY"): {
				{ColumnBase: catalog.ColumnBase{Table: "CITY", Name: "ID"}, Type: "INTEGER", Key: "YES"},
				{ColumnBase: catalog.ColumnBase{Table: "CITY", Name: "NAME"}, Type: "VARCHAR(35)"},
			},
		},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{
			catalog.MetadataColumnsCurrent: catalog.MetadataReady,
			catalog.MetadataViews:          catalog.MetadataReady,
		},
	}
}

func hoverRequest(cache *catalog.Cache, text string) language.Request {
	return language.Request{
		Document: language.Document{URI: "memory:hover", Text: text, Version: 1, Binding: 1},
		Context: language.Context{
			Snapshot:        &catalog.MetadataSnapshot{Cache: cache},
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			DialectResolved: true,
		},
	}
}

func TestHoverColumnIdentifierReturnsColumnDoc(t *testing.T) {
	text := "SELECT ID FROM CITY"
	req := hoverRequest(hoverCache(), text)
	// Column "ID" spans [7,9); place the cursor inside it.
	result, err := language.NewService().Hover(context.Background(), req, language.Position{Line: 0, UTF16Column: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Value.Markdown, "ID") || !strings.Contains(result.Value.Markdown, "INTEGER") {
		t.Fatalf("hover markdown = %q, want column doc mentioning ID and INTEGER", result.Value.Markdown)
	}
	if result.Value.Range == nil {
		t.Fatal("expected a non-nil range for a resolved identifier")
	}
}

func TestHoverTableIdentifierReturnsTableDoc(t *testing.T) {
	text := "SELECT ID FROM CITY"
	req := hoverRequest(hoverCache(), text)
	// "CITY" starts at column 16.
	result, err := language.NewService().Hover(context.Background(), req, language.Position{Line: 0, UTF16Column: 17})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Value.Markdown, "CITY") {
		t.Fatalf("hover markdown = %q, want table doc mentioning CITY", result.Value.Markdown)
	}
}

func TestHoverNoIdentifierUnderCursorReturnsErrNoHover(t *testing.T) {
	text := "SELECT ID FROM CITY"
	req := hoverRequest(hoverCache(), text)
	// Whitespace between SELECT and ID.
	_, err := language.NewService().Hover(context.Background(), req, language.Position{Line: 0, UTF16Column: 6})
	if err != language.ErrNoHover {
		t.Fatalf("err = %v, want ErrNoHover", err)
	}
}

func TestHoverNilSnapshotReturnsErrNoHover(t *testing.T) {
	req := language.Request{
		Document: language.Document{URI: "memory:hover", Text: "SELECT ID FROM CITY", Version: 1, Binding: 1},
		Context: language.Context{
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			DialectResolved: true,
		},
	}
	_, err := language.NewService().Hover(context.Background(), req, language.Position{Line: 0, UTF16Column: 8})
	if err != language.ErrNoHover {
		t.Fatalf("err = %v, want ErrNoHover for a nil-catalog snapshot", err)
	}
}

func TestHoverReadsSharedSnapshotOnDemandNotACopy(t *testing.T) {
	cache := hoverCache()
	snap := &catalog.MetadataSnapshot{Cache: cache}
	req1 := language.Request{
		Document: language.Document{URI: "memory:one", Text: "SELECT ID FROM CITY", Version: 1, Binding: 1},
		Context:  language.Context{Snapshot: snap, Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}, DialectResolved: true},
	}
	svc := language.NewService()
	if _, err := svc.Hover(context.Background(), req1, language.Position{Line: 0, UTF16Column: 8}); err != nil {
		t.Fatal(err)
	}
	// Mutate the shared cache in place (as a live metadata refresh would)
	// and confirm a second Hover call against the same snapshot observes
	// the change: proof Hover reads through the pointer on demand rather
	// than retaining a copy taken at snapshot-construction time.
	cache.ColumnsWithParent[catalog.ColumnDatabaseKey("", "CITY")] = append(
		cache.ColumnsWithParent[catalog.ColumnDatabaseKey("", "CITY")],
		&catalog.ColumnDesc{ColumnBase: catalog.ColumnBase{Table: "CITY", Name: "POPULATION"}, Type: "INTEGER", Default: sql.NullString{}},
	)
	req2 := language.Request{
		Document: language.Document{URI: "memory:one", Text: "SELECT POPULATION FROM CITY", Version: 2, Binding: 1},
		Context:  language.Context{Snapshot: snap, Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}, DialectResolved: true},
	}
	result, err := svc.Hover(context.Background(), req2, language.Position{Line: 0, UTF16Column: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Value.Markdown, "POPULATION") {
		t.Fatalf("hover markdown = %q, want it to reflect the mutated shared cache", result.Value.Markdown)
	}
}
