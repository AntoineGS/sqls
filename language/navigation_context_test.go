package language_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/metadata"
)

type recordingDetails struct {
	requests []metadata.Request
}

func (r *recordingDetails) Detail(_ context.Context, req metadata.Request) (metadata.Detail, error) {
	r.requests = append(r.requests, req)
	text := "source"
	if req.Object.Kind == catalog.ObjectKindTable {
		text = "CREATE TABLE " + req.Object.Name + " (ID INTEGER)"
	}
	return metadata.Detail{Object: req.Object, Text: text, Provenance: "generated-ddl"}, nil
}

func navigationRequest(text string, cache *catalog.Cache) language.Request {
	return language.Request{
		Document: language.Document{URI: "file:///query.sql", Text: text, Version: 1, Binding: 1},
		Context: language.Context{
			Snapshot: &catalog.MetadataSnapshot{Generation: 1, Revision: 1, Cache: cache},
			Variant:  dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			Details:  &recordingDetails{},
		},
	}
}

func readyNavigationCache() *catalog.Cache {
	cache := &catalog.Cache{
		DefaultSchema: "A",
		Schemas:       map[string]string{"A": "A", "B": "B"},
		SchemaTables:  map[string][]string{"A": {"T", "Foo", "FOO", "OUTER_T", "INNER_T"}, "B": {"OTHER"}},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{
			catalog.MetadataRelations:      catalog.MetadataReady,
			catalog.MetadataViews:          catalog.MetadataReady,
			catalog.MetadataProcedures:     catalog.MetadataReady,
			catalog.MetadataTriggers:       catalog.MetadataReady,
			catalog.MetadataColumnsCurrent: catalog.MetadataReady,
		},
		ColumnsByID: map[string][]*catalog.ColumnDesc{
			(catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}).Key():       {{ColumnBase: catalog.ColumnBase{Schema: "A", Table: "T", Name: "ID"}}},
			(catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "OUTER_T"}).Key(): {{ColumnBase: catalog.ColumnBase{Schema: "A", Table: "OUTER_T", Name: "ID"}}},
			(catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "INNER_T"}).Key(): {{ColumnBase: catalog.ColumnBase{Schema: "A", Table: "INNER_T", Name: "ID"}}},
		},
		Catalog: &catalog.CatalogCache{
			Procedures:     map[string]*catalog.ProcedureDesc{"DUP": {Name: "DUP"}},
			Views:          map[string]*catalog.ViewDesc{"DUP": {Name: "DUP"}},
			Triggers:       map[string]*catalog.TriggerDesc{"DUP": {Name: "DUP"}},
			ProceduresByID: map[string]*catalog.ProcedureDesc{(catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "DUP"}).Key(): {Name: "DUP"}},
			ViewsByID:      map[string]*catalog.ViewDesc{(catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "DUP"}).Key(): {Name: "DUP"}},
			TriggersByID:   map[string]*catalog.TriggerDesc{(catalog.ObjectID{Kind: catalog.ObjectKindTrigger, Name: "DUP"}).Key(): {Name: "DUP"}},
		},
	}
	cache.Catalog.RebuildObjects(strings.ToUpper)
	return cache
}

func addNavigationTable(cache *catalog.Cache, name string, columns ...string) {
	if cache.ColumnsByID == nil {
		cache.ColumnsByID = make(map[string][]*catalog.ColumnDesc)
	}
	if cache.ColumnsWithParent == nil {
		cache.ColumnsWithParent = make(map[string][]*catalog.ColumnDesc)
	}
	cache.SchemaTables["A"] = append(cache.SchemaTables["A"], name)
	descs := make([]*catalog.ColumnDesc, 0, len(columns))
	for _, column := range columns {
		descs = append(descs, &catalog.ColumnDesc{ColumnBase: catalog.ColumnBase{Schema: "A", Table: name, Name: column}, Type: "INTEGER"})
	}
	cache.ColumnsByID[(catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: name}).Key()] = descs
	cache.ColumnsWithParent[catalog.ColumnDatabaseKey("A", name)] = descs
	cache.Catalog.RebuildObjects(strings.ToUpper)
}

func definitionDetails(t *testing.T, req language.Request, pos language.Position) []metadata.Request {
	t.Helper()
	details := req.Context.Details.(*recordingDetails)
	_, err := language.NewService().Definition(context.Background(), req, pos)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	return details.requests
}

func indexAt(t *testing.T, text, marker string) int {
	t.Helper()
	index := strings.LastIndex(text, marker)
	if index < 0 {
		t.Fatalf("marker %q not found in %q", marker, text)
	}
	return index
}

func TestNavigationDefinitionUsesExactQuotedCatalogIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, text, target string
	}{
		{name: "quoted mixed case", text: `SELECT * FROM "Foo"`, target: "Foo"},
		{name: "unquoted uppercase lookalike", text: `SELECT * FROM FOO`, target: "FOO"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := navigationRequest(tc.text, readyNavigationCache())
			pos := language.Position{Line: 0, UTF16Column: len(tc.text) - 2}
			calls := definitionDetails(t, req, pos)
			if len(calls) != 1 || calls[0].Object != (catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: tc.target}) {
				t.Fatalf("detail requests = %#v, want exact table %q", calls, tc.target)
			}
		})
	}
}

func TestNavigationDefinitionRefusesAmbiguousUnqualifiedRelation(t *testing.T) {
	cache := readyNavigationCache()
	cache.SchemaTables["B"] = []string{"T"}
	req := navigationRequest("SELECT * FROM T", cache)
	calls := definitionDetails(t, req, language.Position{Line: 0, UTF16Column: 14})
	if len(calls) != 0 {
		t.Fatalf("ambiguous relation made detail requests: %#v", calls)
	}
}

func TestNavigationDefinitionRefusesPartialRelationMetadata(t *testing.T) {
	cache := readyNavigationCache()
	cache.Metadata[catalog.MetadataViews] = catalog.MetadataLoading
	req := navigationRequest("SELECT * FROM T", cache)
	calls := definitionDetails(t, req, language.Position{Line: 0, UTF16Column: 14})
	if len(calls) != 0 {
		t.Fatalf("partial relation metadata made detail requests: %#v", calls)
	}
}

func TestNavigationDefinitionRejectsUnprovenColumnOwnersWithoutDetailReads(t *testing.T) {
	cases := []struct {
		name, text, marker string
		prepare            func(*catalog.Cache)
	}{
		{
			name:   "unknown column",
			text:   "SELECT ci.MISSING FROM CUSTOMERINVOICE ci",
			marker: "MISSING",
			prepare: func(cache *catalog.Cache) {
				addNavigationTable(cache, "CUSTOMERINVOICE", "BALANCE")
			},
		},
		{
			name:   "column ambiguous between two tables",
			text:   "SELECT BALANCE FROM CUSTOMERINVOICE ci JOIN OTHERINVOICE oi ON 1=1",
			marker: "BALANCE",
			prepare: func(cache *catalog.Cache) {
				addNavigationTable(cache, "CUSTOMERINVOICE", "BALANCE")
				addNavigationTable(cache, "OTHERINVOICE", "BALANCE")
			},
		},
		{
			name:   "unknown inner derived binding shadows known outer",
			text:   "SELECT c.BALANCE FROM CUSTOMERINVOICE c WHERE EXISTS (SELECT 1 FROM (SELECT 1) c WHERE c.BALANCE = 1)",
			marker: "BALANCE",
			prepare: func(cache *catalog.Cache) {
				addNavigationTable(cache, "CUSTOMERINVOICE", "BALANCE")
			},
		},
		{
			name:   "duplicate same-scope qualifier",
			text:   "SELECT c.BALANCE FROM CUSTOMERINVOICE c JOIN OTHERINVOICE c ON 1=1",
			marker: "BALANCE",
			prepare: func(cache *catalog.Cache) {
				addNavigationTable(cache, "CUSTOMERINVOICE", "BALANCE")
				addNavigationTable(cache, "OTHERINVOICE", "BALANCE")
			},
		},
		{
			name:   "quoted mixed-case column mismatch",
			text:   `SELECT ci."camelcol" FROM "MixedCase" ci`,
			marker: `"camelcol"`,
			prepare: func(cache *catalog.Cache) {
				addNavigationTable(cache, "MixedCase", "CamelCol")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := readyNavigationCache()
			tc.prepare(cache)
			req := navigationRequest(tc.text, cache)
			offset := strings.LastIndex(tc.text, tc.marker)
			pos, err := language.PositionAt(tc.text, offset+1)
			if err != nil {
				t.Fatal(err)
			}
			result, err := language.NewService().Definition(context.Background(), req, pos)
			if err != nil {
				t.Fatalf("Definition: %v", err)
			}
			if len(result.Value) != 0 {
				t.Fatalf("Definition = %#v, want no source location", result.Value)
			}
			if calls := req.Context.Details.(*recordingDetails).requests; len(calls) != 0 {
				t.Fatalf("unproven owner made detail requests: %#v", calls)
			}
		})
	}
}

func TestNavigationDefinitionResolvesRelationColumnAndNearestAlias(t *testing.T) {
	cases := []struct {
		name, text, marker, target string
	}{
		{name: "relation token", text: "SELECT * FROM T", marker: "T", target: "T"},
		{name: "unqualified column", text: "SELECT ID FROM T", marker: "ID", target: "T"},
		{name: "nearest alias binding", text: "SELECT (SELECT x.ID FROM INNER_T x) FROM OUTER_T x", marker: "ID", target: "INNER_T"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := navigationRequest(tc.text, readyNavigationCache())
			offset := indexAt(t, tc.text, tc.marker)
			pos, err := language.PositionAt(tc.text, offset)
			if err != nil {
				t.Fatal(err)
			}
			calls := definitionDetails(t, req, pos)
			if len(calls) != 1 || calls[0].Object != (catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: tc.target}) {
				t.Fatalf("detail requests = %#v, want owning table %q", calls, tc.target)
			}
		})
	}
}

func TestNavigationDefinitionKeepsProcedureViewTriggerPrecedence(t *testing.T) {
	req := navigationRequest("EXECUTE PROCEDURE DUP", readyNavigationCache())
	calls := definitionDetails(t, req, language.Position{Line: 0, UTF16Column: 19})
	if len(calls) != 1 || calls[0].Object != (catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "DUP"}) {
		t.Fatalf("precedence detail requests = %#v, want procedure DUP", calls)
	}

	cache := readyNavigationCache()
	delete(cache.Catalog.Procedures, "DUP")
	delete(cache.Catalog.ProceduresByID, (catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "DUP"}).Key())
	cache.Catalog.RebuildObjects(strings.ToUpper)
	req = navigationRequest("EXECUTE PROCEDURE DUP", cache)
	calls = definitionDetails(t, req, language.Position{Line: 0, UTF16Column: 19})
	if len(calls) != 1 || calls[0].Object != (catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "DUP"}) {
		t.Fatalf("view precedence detail requests = %#v, want view DUP", calls)
	}
}

func TestNavigationQuotedCatalogNamesRequireExactIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind catalog.ObjectKind
		text string
	}{
		{name: "view relation", kind: catalog.ObjectKindView, text: `SELECT * FROM "foo"`},
		{name: "procedure identifier", kind: catalog.ObjectKindProcedure, text: `EXECUTE PROCEDURE "foo"`},
		{name: "trigger identifier", kind: catalog.ObjectKindTrigger, text: `"foo"`},
	} {
		t.Run(tc.name+" folded miss", func(t *testing.T) {
			cache := readyNavigationCacheWithFoldNames(tc.kind, "FOO")
			req := navigationRequest(tc.text, cache)
			pos := testPosition(tc.text, strings.Index(tc.text, `"foo"`)+2)
			if calls := definitionDetails(t, req, pos); len(calls) != 0 {
				t.Fatalf("quoted miss requested folded catalog target: %#v", calls)
			}
		})
		t.Run(tc.name+" exact among fold collision", func(t *testing.T) {
			cache := readyNavigationCacheWithFoldNames(tc.kind, "FOO", "foo")
			req := navigationRequest(tc.text, cache)
			pos := testPosition(tc.text, strings.Index(tc.text, `"foo"`)+2)
			calls := definitionDetails(t, req, pos)
			want := catalog.ObjectID{Kind: tc.kind, Name: "foo"}
			if len(calls) != 1 || calls[0].Object != want {
				t.Fatalf("quoted exact identity requests = %#v, want %#v", calls, want)
			}
		})
	}
}

func TestNavigationUnquotedCatalogNamesRejectFoldCollisions(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind catalog.ObjectKind
		text string
	}{
		{name: "view relation", kind: catalog.ObjectKindView, text: "SELECT * FROM FOO"},
		{name: "procedure identifier", kind: catalog.ObjectKindProcedure, text: "EXECUTE PROCEDURE FOO"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := readyNavigationCacheWithFoldNames(tc.kind, "FOO", "foo")
			req := navigationRequest(tc.text, cache)
			pos := testPosition(tc.text, strings.LastIndex(tc.text, "FOO"))
			if calls := definitionDetails(t, req, pos); len(calls) != 0 {
				t.Fatalf("ambiguous unquoted lookup requested detail: %#v", calls)
			}
		})
	}
}

func readyNavigationCacheWithFoldNames(kind catalog.ObjectKind, names ...string) *catalog.Cache {
	cache := readyNavigationCache()
	for _, name := range names {
		cache.Catalog.Objects.Insert(catalog.ObjectID{Kind: kind, Name: name})
	}
	return cache
}

func TestNavigationColumnDefinitionDoesNotBecomeRelationReferencesOrRename(t *testing.T) {
	text := "SELECT ID FROM T"
	req := navigationRequest(text, readyNavigationCache())
	pos := testPosition(text, strings.Index(text, "ID"))
	service := language.NewService()

	definition, err := service.Definition(context.Background(), req, pos)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if details := req.Context.Details.(*recordingDetails).requests; len(details) != 1 || details[0].Object != (catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}) {
		t.Fatalf("column Definition details = %#v, want proven owner T", details)
	}
	if len(definition.Value) != 1 || definition.Value[0].Source == nil {
		t.Fatalf("column Definition = %#v, want owner source", definition.Value)
	}

	references, err := service.References(context.Background(), req, pos, true)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(references.Value) != 0 {
		t.Fatalf("column References became relation edits: %#v", references.Value)
	}
	rename, err := service.Rename(context.Background(), req, pos, "RENAMED")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if len(rename.Value) != 1 || rename.Value[0].Shape != language.RenameDocumentChanges || len(rename.Value[0].Edits) != 1 {
		t.Fatalf("column Rename = %#v, want the legacy spelling-based identifier edit", rename.Value)
	}
}

func TestNavigationDefinitionDoesNotRequestForCommentsLiteralsOrProcedureLocals(t *testing.T) {
	cases := []struct {
		name, text, marker string
	}{
		{name: "comment", text: "-- DUP\nSELECT 1", marker: "DUP"},
		{name: "literal", text: "SELECT 'DUP'", marker: "DUP"},
		{name: "procedure local shadows catalog", text: "CREATE PROCEDURE P AS DECLARE VARIABLE DUP INTEGER; BEGIN DUP = 1; END", marker: "DUP ="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := navigationRequest(tc.text, readyNavigationCache())
			offset := indexAt(t, tc.text, tc.marker)
			pos, err := language.PositionAt(tc.text, offset)
			if err != nil {
				t.Fatal(err)
			}
			calls := definitionDetails(t, req, pos)
			if len(calls) != 0 {
				t.Fatalf("unsafe/non-target token requested detail: %#v", calls)
			}
		})
	}
}

func TestNavigationDefinitionCursorAfterEmojiAndCRLFUsesUTF16(t *testing.T) {
	text := "-- 😀\r\nSELECT * FROM \"Foo\""
	req := navigationRequest(text, readyNavigationCache())
	pos := language.Position{Line: 1, UTF16Column: 16}
	calls := definitionDetails(t, req, pos)
	if len(calls) != 1 || calls[0].Object != (catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "Foo"}) {
		t.Fatalf("UTF-16 cursor detail requests = %#v, want exact Foo", calls)
	}
	references, err := language.NewService().References(context.Background(), req, pos, false)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if references.Value == nil || len(references.Value) != 0 {
		t.Fatalf("relation References = %#v, want non-nil empty under standalone parity", references.Value)
	}
}
