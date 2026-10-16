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

func TestHoverTableFallbackWaitsForViewsAndStillShowsKnownView(t *testing.T) {
	const text = "SELECT * FROM V"
	position := language.Position{Line: 0, UTF16Column: len(text) - 1}
	cache := &catalog.Cache{
		DefaultSchema: "A",
		SchemaTables:  map[string][]string{"A": {"V"}},
		ColumnsByID: map[string][]*catalog.ColumnDesc{
			(catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "V"}).Key(): {{ColumnBase: catalog.ColumnBase{Schema: "A", Table: "V", Name: "ID"}, Type: "INTEGER"}},
		},
		ColumnsWithParent: map[string][]*catalog.ColumnDesc{
			catalog.ColumnDatabaseKey("A", "V"): {{ColumnBase: catalog.ColumnBase{Schema: "A", Table: "V", Name: "ID"}, Type: "INTEGER"}},
		},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{
			catalog.MetadataRelations:      catalog.MetadataReady,
			catalog.MetadataViews:          catalog.MetadataLoading,
			catalog.MetadataColumnsCurrent: catalog.MetadataReady,
		},
		Catalog: &catalog.CatalogCache{Views: map[string]*catalog.ViewDesc{"V": {Name: "V", ViewSource: sql.NullString{String: "SELECT ID FROM T", Valid: true}}}},
	}
	cache.Catalog.ViewsByID = map[string]*catalog.ViewDesc{(catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "V"}).Key(): cache.Catalog.Views["V"]}
	cache.Catalog.RebuildObjects(strings.ToUpper)

	knownViewReq := hoverRequest(cache, text)
	knownViewReq.Context.Details = &recordingDetails{}
	got, err := language.NewService().Hover(context.Background(), knownViewReq, position)
	if err != nil {
		t.Fatalf("known view while views load: %v", err)
	}
	wantView := catalog.ViewDoc(cache.Catalog.Views["V"])
	if got.Value.Markdown != wantView {
		t.Fatalf("known view markdown = %q, want exact cached view summary %q", got.Value.Markdown, wantView)
	}
	if got.Value.Range == nil || *got.Value.Range != (language.Range{Start: position, End: language.Position{Line: 0, UTF16Column: len(text)}}) {
		t.Fatalf("known view range = %#v, want V token range", got.Value.Range)
	}
	if calls := knownViewReq.Context.Details.(*recordingDetails).requests; len(calls) != 0 {
		t.Fatalf("known view while views load made detail requests: %#v", calls)
	}

	delete(cache.Catalog.Views, "V")
	delete(cache.Catalog.ViewsByID, (catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "V"}).Key())
	cache.Catalog.RebuildObjects(strings.ToUpper)
	unknownTableReq := hoverRequest(cache, text)
	unknownTableReq.Context.Details = &recordingDetails{}
	if got, err := language.NewService().Hover(context.Background(), unknownTableReq, position); err != language.ErrNoHover {
		t.Fatalf("unknown view/table fallback while views load = (%#v, %v), want no hover", got.Value, err)
	}
	if calls := unknownTableReq.Context.Details.(*recordingDetails).requests; len(calls) != 0 {
		t.Fatalf("unclassified relation while views load made detail requests: %#v", calls)
	}

	cache.Metadata[catalog.MetadataViews] = catalog.MetadataReady
	readyTableReq := hoverRequest(cache, text)
	readyTableReq.Context.Details = &recordingDetails{}
	got, err = language.NewService().Hover(context.Background(), readyTableReq, position)
	if err != nil {
		t.Fatalf("table fallback after views are ready: %v", err)
	}
	wantTable := strings.TrimRight(catalog.TableDoc("V", cache.ColumnsWithParent[catalog.ColumnDatabaseKey("A", "V")]), "\n") + "\n\n```sql\nCREATE TABLE V (ID INTEGER)\n```"
	if got.Value.Markdown != wantTable {
		t.Fatalf("ready table markdown = %q, want exact cached summary plus detail %q", got.Value.Markdown, wantTable)
	}
	if got.Value.Range == nil || *got.Value.Range != (language.Range{Start: position, End: language.Position{Line: 0, UTF16Column: len(text)}}) {
		t.Fatalf("ready table range = %#v, want V token range", got.Value.Range)
	}
	if calls := readyTableReq.Context.Details.(*recordingDetails).requests; len(calls) != 1 || calls[0].Object != (catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "V"}) {
		t.Fatalf("ready table detail requests = %#v, want one table V detail", calls)
	}
}

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

func TestHoverUsesNewCatalogSnapshotForSameDocumentIdentity(t *testing.T) {
	const text = "SELECT ID FROM CITY"
	position := language.Position{Line: 0, UTF16Column: 8}
	service := language.NewService()

	firstRequest := hoverRequest(hoverCache(), text)
	first, err := service.Hover(context.Background(), firstRequest, position)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Value.Markdown, "INTEGER") {
		t.Fatalf("first hover = %q, want INTEGER from first catalog snapshot", first.Value.Markdown)
	}

	updatedCache := hoverCache()
	key := catalog.ColumnDatabaseKey("", "CITY")
	updatedCache.ColumnsWithParent[key][0].Type = "BIGINT"
	secondRequest := hoverRequest(updatedCache, text)
	if firstRequest.Document != secondRequest.Document {
		t.Fatal("requests must have identical document identity")
	}
	second, err := service.Hover(context.Background(), secondRequest, position)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Value.Markdown, "BIGINT") || strings.Contains(second.Value.Markdown, "INTEGER") {
		t.Fatalf("second hover = %q, want BIGINT from the new snapshot", second.Value.Markdown)
	}
}
