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

func TestHoverColumnReferenceReturnsOnlyItsColumnDoc(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		field     string
		fieldPart string
	}{
		{name: "bare SELECT", text: "SELECT ID FROM CITY", field: "ID", fieldPart: "ID"},
		{name: "bare WHERE", text: "SELECT ID FROM CITY WHERE NAME = 'x'", field: "NAME", fieldPart: "NAME"},
		{name: "alias SELECT", text: "SELECT C.ID FROM CITY C", field: "ID", fieldPart: "C.ID"},
		{name: "alias WHERE", text: "SELECT ID FROM CITY C WHERE C.NAME = 'x'", field: "NAME", fieldPart: "C.NAME"},
		{name: "table SELECT", text: "SELECT CITY.ID FROM CITY", field: "ID", fieldPart: "CITY.ID"},
		{name: "table WHERE", text: "SELECT ID FROM CITY WHERE CITY.NAME = 'x'", field: "NAME", fieldPart: "CITY.NAME"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cache := hoverCache()
			text := test.text
			position := strings.LastIndex(text, test.fieldPart)
			if strings.Contains(test.fieldPart, ".") {
				position += strings.LastIndex(test.fieldPart, ".") + 1
			} else if test.name == "bare SELECT" {
				position = strings.Index(text, test.fieldPart)
			}
			req := hoverRequest(cache, text)
			result, err := language.NewService().Hover(context.Background(), req, language.Position{Line: 0, UTF16Column: position + 1})
			if err != nil {
				t.Fatal(err)
			}
			column, ok := cache.Column("CITY", test.field)
			if !ok {
				t.Fatalf("test column %q missing from fixture", test.field)
			}
			want := catalog.ColumnHoverDoc("CITY", column)
			if result.Value.Markdown != want {
				t.Fatalf("hover markdown = %q, want only the column doc %q", result.Value.Markdown, want)
			}
		})
	}
}

func TestHoverViewColumnReferenceReturnsOnlyItsColumnDoc(t *testing.T) {
	cache := hoverCache()
	viewColumns := []*catalog.ColumnDesc{
		{ColumnBase: catalog.ColumnBase{Table: "CITY_VIEW", Name: "ID"}, Type: "INTEGER", Key: "YES"},
		{ColumnBase: catalog.ColumnBase{Table: "CITY_VIEW", Name: "NAME"}, Type: "VARCHAR(35)"},
	}
	cache.ColumnsWithParent[catalog.ColumnDatabaseKey("", "CITY_VIEW")] = viewColumns
	cache.Metadata[catalog.MetadataRelations] = catalog.MetadataReady
	cache.Metadata[catalog.MetadataViews] = catalog.MetadataReady
	cache.Metadata[catalog.MetadataColumnsCurrent] = catalog.MetadataReady
	cache.ColumnsByID = map[string][]*catalog.ColumnDesc{
		(catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "CITY"}).Key(): cache.ColumnsWithParent[catalog.ColumnDatabaseKey("", "CITY")],
	}
	view := &catalog.ViewDesc{Name: "CITY_VIEW", Columns: viewColumns}
	cache.Catalog = &catalog.CatalogCache{
		Views:     map[string]*catalog.ViewDesc{"CITY_VIEW": view},
		ViewsByID: map[string]*catalog.ViewDesc{(catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "CITY_VIEW"}).Key(): view},
	}
	cache.Catalog.RebuildObjects(strings.ToUpper)

	for _, test := range []struct {
		name      string
		text      string
		fieldPart string
		field     string
	}{
		{name: "bare SELECT", text: "SELECT ID FROM CITY_VIEW", fieldPart: "ID", field: "ID"},
		{name: "bare WHERE", text: "SELECT ID FROM CITY_VIEW WHERE NAME = 'x'", fieldPart: "NAME", field: "NAME"},
		{name: "alias SELECT", text: "SELECT V.ID FROM CITY_VIEW V", fieldPart: "V.ID", field: "ID"},
		{name: "alias WHERE", text: "SELECT ID FROM CITY_VIEW V WHERE V.NAME = 'x'", fieldPart: "V.NAME", field: "NAME"},
		{name: "table SELECT", text: "SELECT CITY_VIEW.ID FROM CITY_VIEW", fieldPart: "CITY_VIEW.ID", field: "ID"},
		{name: "table WHERE", text: "SELECT ID FROM CITY_VIEW WHERE CITY_VIEW.NAME = 'x'", fieldPart: "CITY_VIEW.NAME", field: "NAME"},
	} {
		t.Run(test.name, func(t *testing.T) {
			position := strings.LastIndex(test.text, test.fieldPart)
			if strings.Contains(test.fieldPart, ".") {
				position += strings.LastIndex(test.fieldPart, ".") + 1
			}
			req := hoverRequest(cache, test.text)
			details := &recordingDetails{}
			req.Context.Details = details
			result, err := language.NewService().Hover(context.Background(), req, language.Position{Line: 0, UTF16Column: position + 1})
			if err != nil {
				t.Fatal(err)
			}
			var column *catalog.ColumnDesc
			for _, candidate := range viewColumns {
				if candidate.Name == test.field {
					column = candidate
					break
				}
			}
			want := catalog.ColumnHoverDoc("CITY_VIEW", column)
			if result.Value.Markdown != want {
				t.Fatalf("hover markdown = %q, want only the column doc %q", result.Value.Markdown, want)
			}
			if len(details.requests) != 0 {
				t.Fatalf("column hover made relation detail requests: %#v", details.requests)
			}
		})
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
