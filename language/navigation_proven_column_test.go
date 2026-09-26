package language_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/metadata"
)

const ambiguousLocalColumnSQL = "ALTER PROCEDURE P AS\r\nDECLARE VARIABLE DBID INTEGER;\r\nBEGIN\r\nSELECT /*😀*/ DBID FROM DATABASEID INTO :DBID;\r\nEND"

func provenColumnRequest(text string, details language.DetailReader) language.Request {
	cache := readyNavigationCache()
	addNavigationTable(cache, "DATABASEID", "DBID")
	return language.Request{
		Document: language.Document{URI: "file:///proven.sql", Text: text, Version: 1, Binding: 1},
		Context: language.Context{
			Snapshot: &catalog.MetadataSnapshot{Generation: 1, Revision: 1, Cache: cache},
			Variant:  dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			Details:  details,
		},
	}
}

func provenColumnPosition(t *testing.T, text string) language.Position {
	t.Helper()
	pos, err := language.PositionAt(text, strings.Index(text, "DBID FROM"))
	if err != nil {
		t.Fatal(err)
	}
	return pos
}

func TestProvenDefinitionNavigatesAmbiguousLocalToColumnDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		results map[string]detailResult
		body    string
	}{
		{
			name: "DDL",
			body: "CREATE TABLE DATABASEID (DBID INTEGER)",
			results: map[string]detailResult{
				"ddl": {detail: metadata.Detail{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "DATABASEID"}, Text: "CREATE TABLE DATABASEID (DBID INTEGER)", Provenance: "generated-ddl"}},
			},
		},
		{
			name: "informational description fallback",
			body: "CREATE TABLE DATABASEID (DBID INTEGER)",
			results: map[string]detailResult{
				"ddl":         {err: structuredUnsupportedDDL{}},
				"description": {detail: metadata.Detail{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "DATABASEID"}, Description: catalog.TableDescription{Table: spanOf("CREATE TABLE DATABASEID (DBID INTEGER)", "DATABASEID"), Body: "CREATE TABLE DATABASEID (DBID INTEGER)", Columns: []catalog.DescriptionColumn{{Name: "DBID", Span: spanOf("CREATE TABLE DATABASEID (DBID INTEGER)", "DBID")}}}}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			details := &facetDetails{results: tc.results}
			req := provenColumnRequest(ambiguousLocalColumnSQL, details)
			result, err := language.NewService().Definition(context.Background(), req, provenColumnPosition(t, ambiguousLocalColumnSQL))
			if err != nil {
				t.Fatalf("Definition: %v", err)
			}
			if len(result.Value) != 1 || result.Value[0].Source == nil || result.Value[0].Source.Text != tc.body {
				t.Fatalf("Definition = %#v, want one source location", result.Value)
			}
			wantStart := strings.LastIndex(tc.body, "DBID")
			wantRange, err := language.PositionAt(tc.body, wantStart)
			if err != nil {
				t.Fatal(err)
			}
			wantEnd, err := language.PositionAt(tc.body, wantStart+len("DBID"))
			if err != nil {
				t.Fatal(err)
			}
			if result.Value[0].Range != (language.Range{Start: wantRange, End: wantEnd}) {
				t.Fatalf("column range = %#v, want exact DBID declaration span", result.Value[0].Range)
			}
			var wantFacets []string
			if tc.name == "DDL" {
				wantFacets = []string{"ddl"}
			} else {
				wantFacets = []string{"ddl", "description"}
			}
			if len(details.calls) != len(wantFacets) {
				t.Fatalf("detail calls = %#v, want facets %v", details.calls, wantFacets)
			}
			for i, facet := range wantFacets {
				if details.calls[i].Facet != facet || details.calls[i].Object != (catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "DATABASEID"}) {
					t.Fatalf("detail call %d = %#v, want %s for exact base table", i, details.calls[i], facet)
				}
			}
			colon := strings.Index(ambiguousLocalColumnSQL, ":DBID")
			for _, offset := range []int{colon, colon + 1} {
				into, err := language.PositionAt(ambiguousLocalColumnSQL, offset)
				if err != nil {
					t.Fatal(err)
				}
				local, err := language.NewService().Definition(context.Background(), req, into)
				if err != nil {
					t.Fatalf("INTO local Definition at byte %d: %v", offset, err)
				}
				if len(local.Value) != 1 || local.Value[0].URI != req.Document.URI || local.Value[0].Range.Start.Line != 1 {
					t.Fatalf("INTO local Definition at byte %d = %#v, want declaration of local DBID", offset, local.Value)
				}
				if len(details.calls) != len(wantFacets) {
					t.Fatalf("INTO local cursor loaded source details: %#v", details.calls)
				}
			}
		})
	}
}

func TestProvenTableColumnRejectsViewOrMixedDDLDeclarations(t *testing.T) {
	for _, body := range []struct {
		name string
		text string
	}{
		{name: "view declaration only", text: "CREATE VIEW DATABASEID (DBID) AS SELECT 1 FROM RDB$DATABASE"},
		{name: "table header and view column are different declarations", text: "CREATE TABLE DATABASEID (OTHER INTEGER); CREATE VIEW DATABASEID (DBID) AS SELECT 1 FROM RDB$DATABASE"},
	} {
		t.Run(body.name, func(t *testing.T) {
			id := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "DATABASEID"}
			details := &facetDetails{results: map[string]detailResult{"ddl": {detail: metadata.Detail{Object: id, Text: body.text, Provenance: "generated-ddl"}}}}
			req := provenColumnRequest(ambiguousLocalColumnSQL, details)
			result, err := language.NewService().Definition(context.Background(), req, provenColumnPosition(t, ambiguousLocalColumnSQL))
			if err != nil {
				t.Fatalf("Definition: %v", err)
			}
			if len(result.Value) != 0 {
				t.Fatalf("mismatched table DDL returned location %#v, want no range", result.Value)
			}
			if len(details.calls) != 1 || details.calls[0].Facet != "ddl" {
				t.Fatalf("detail calls = %#v, want only one DDL read", details.calls)
			}
		})
	}
}

func TestDDLViewColumnNavigationRemainsSupported(t *testing.T) {
	cache := readyNavigationCache()
	viewColumn := &catalog.ColumnDesc{ColumnBase: catalog.ColumnBase{Table: "DUP", Name: "ID"}, Type: "INTEGER"}
	cache.Catalog.Views["DUP"].Columns = []*catalog.ColumnDesc{viewColumn}
	cache.Catalog.ViewsByID[(catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "DUP"}).Key()].Columns = []*catalog.ColumnDesc{viewColumn}
	cache.Catalog.RebuildObjects(strings.ToUpper)
	body := "CREATE VIEW DUP (ID) AS SELECT 1 FROM RDB$DATABASE"
	details := &facetDetails{results: map[string]detailResult{"ddl": {detail: metadata.Detail{
		Object: catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "DUP"}, Text: body, Provenance: "interbase:view-ddl",
	}}}}
	req := navigationRequest("SELECT ID FROM DUP", cache)
	req.Context.Details = details
	result, err := language.NewService().Definition(context.Background(), req, testPosition(req.Document.Text, strings.Index(req.Document.Text, "ID")))
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(result.Value) != 1 || result.Value[0].Source == nil || result.Value[0].Source.Object.Kind != catalog.ObjectKindView {
		t.Fatalf("view-column Definition = %#v, want one view source location", result.Value)
	}
	start := strings.Index(body, "ID")
	wantStart, err := language.PositionAt(body, start)
	if err != nil {
		t.Fatal(err)
	}
	wantEnd, err := language.PositionAt(body, start+len("ID"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Value[0].Range != (language.Range{Start: wantStart, End: wantEnd}) {
		t.Fatalf("view column range = %#v, want exact ID declaration", result.Value[0].Range)
	}
}

func TestProvenDefinitionDoesNotNavigateUnprovenLocalColumn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		text    string
		prepare func(*catalog.Cache)
		stale   bool
	}{
		{name: "matching table name but different column", text: ambiguousLocalColumnSQL, prepare: func(cache *catalog.Cache) { addNavigationTable(cache, "DATABASEID", "OTHER") }},
		{name: "incomplete exact columns", text: ambiguousLocalColumnSQL, prepare: func(cache *catalog.Cache) { cache.Metadata[catalog.MetadataColumnsCurrent] = catalog.MetadataLoading }},
		{name: "derived source", text: "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; BEGIN SELECT DBID FROM (SELECT 1 AS DBID) X; END"},
		{name: "CTE source", text: "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; BEGIN WITH DATABASEID AS (SELECT 1 AS DBID) SELECT DBID FROM DATABASEID; END"},
		{name: "ambiguous source", text: "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; BEGIN SELECT DBID FROM DATABASEID JOIN OTHER ON 1=1; END", prepare: func(cache *catalog.Cache) { addNavigationTable(cache, "OTHER", "DBID") }},
		{name: "correlated source", text: "ALTER PROCEDURE P AS DECLARE VARIABLE DBID INTEGER; BEGIN SELECT DBID FROM DATABASEID WHERE EXISTS (SELECT 1 FROM OTHER WHERE OTHER.DBID = DATABASEID.DBID); END", prepare: func(cache *catalog.Cache) { addNavigationTable(cache, "OTHER", "DBID") }},
		{name: "view is not a base table", text: ambiguousLocalColumnSQL, prepare: func(cache *catalog.Cache) {
			viewColumn := &catalog.ColumnDesc{ColumnBase: catalog.ColumnBase{Table: "DATABASEID", Name: "DBID"}, Type: "INTEGER"}
			cache.Catalog.Views["DATABASEID"] = &catalog.ViewDesc{Name: "DATABASEID", Columns: []*catalog.ColumnDesc{viewColumn}}
			cache.Catalog.ViewsByID[(catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "DATABASEID"}).Key()] = cache.Catalog.Views["DATABASEID"]
			cache.Catalog.RebuildObjects(strings.ToUpper)
		}},
		{name: "stale snapshot", text: ambiguousLocalColumnSQL, stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			details := &facetDetails{results: map[string]detailResult{"ddl": {detail: metadata.Detail{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "DATABASEID"}, Text: "CREATE TABLE DATABASEID (DBID INTEGER)"}}}}
			req := provenColumnRequest(tc.text, details)
			if tc.prepare != nil {
				tc.prepare(req.Context.Snapshot.Cache)
			}
			req.Context.Snapshot.Stale = tc.stale
			result, err := language.NewService().Definition(context.Background(), req, testPosition(tc.text, strings.Index(tc.text, "DBID FROM")))
			if err != nil {
				t.Fatalf("Definition: %v", err)
			}
			if len(result.Value) != 0 {
				t.Fatalf("unproven Definition = %#v, want no location", result.Value)
			}
			if len(details.calls) != 0 {
				t.Fatalf("unproven column made detail requests: %#v", details.calls)
			}
		})
	}
}

type blockingProvenDetails struct {
	entered chan struct{}
	release chan struct{}
}

func (d blockingProvenDetails) Detail(ctx context.Context, req metadata.Request) (metadata.Detail, error) {
	if req.Facet == "ddl" {
		close(d.entered)
		select {
		case <-d.release:
		case <-ctx.Done():
			return metadata.Detail{}, ctx.Err()
		}
	}
	return metadata.Detail{Object: req.Object, Text: "CREATE TABLE DATABASEID (DBID INTEGER)"}, nil
}

func TestProvenDefinitionCancellationDoesNotPublishDetail(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	details := blockingProvenDetails{entered: make(chan struct{}), release: make(chan struct{})}
	req := provenColumnRequest(ambiguousLocalColumnSQL, details)
	result := make(chan error, 1)
	go func() {
		_, err := language.NewService().Definition(ctx, req, provenColumnPosition(t, ambiguousLocalColumnSQL))
		result <- err
	}()
	select {
	case <-details.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("proven definition did not request DDL detail")
	}
	cancel()
	err := <-result
	if !errors.Is(err, language.ErrCanceled) {
		t.Fatalf("Definition error = %v, want ErrCanceled", err)
	}
}
