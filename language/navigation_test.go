package language_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

type detailResult struct {
	detail metadata.Detail
	err    error
}

type facetDetails struct {
	results map[string]detailResult
	calls   []metadata.Request
}

type structuredUnsupportedDDL struct{}

func (structuredUnsupportedDDL) Error() string { return "unsupported" }
func (structuredUnsupportedDDL) Unwrap() error { return database.ErrUnsupportedDDL }
func (structuredUnsupportedDDL) UnsupportedDDLDetail() (string, string, string) {
	return "procedure", "DUP", "parameter nullability"
}

type cancelingDetails struct{ cancel context.CancelFunc }

func (d cancelingDetails) Detail(context.Context, metadata.Request) (metadata.Detail, error) {
	d.cancel()
	return metadata.Detail{}, database.ErrUnsupportedDDL
}

func (d *facetDetails) Detail(_ context.Context, req metadata.Request) (metadata.Detail, error) {
	d.calls = append(d.calls, req)
	result, ok := d.results[req.Facet]
	if !ok {
		return metadata.Detail{}, metadata.ErrUnsupported
	}
	return result.detail, result.err
}

func facetNavigationRequest(text string, details *facetDetails) language.Request {
	req := navigationRequest(text, readyNavigationCache())
	req.Context.Details = details
	return req
}

func detailLocation(t *testing.T, req language.Request, marker string) language.Location {
	t.Helper()
	offset := strings.LastIndex(req.Document.Text, marker)
	if offset < 0 {
		t.Fatalf("marker %q absent from %q", marker, req.Document.Text)
	}
	position, err := language.PositionAt(req.Document.Text, offset)
	if err != nil {
		t.Fatal(err)
	}
	result, err := language.NewService().Definition(context.Background(), req, position)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(result.Value) == 0 {
		return language.Location{}
	}
	if len(result.Value) != 1 {
		t.Fatalf("Definition locations = %#v, want one", result.Value)
	}
	return result.Value[0]
}

func spanOf(body, name string) catalog.DescriptionSpan {
	start := strings.LastIndex(body, name)
	return catalog.DescriptionSpan{Start: start, End: start + len(name)}
}

func TestDefinitionUsesDDLAndBodyRelativeUTF16Range(t *testing.T) {
	body := "-- 😀\r\nCREATE TABLE T (ID INTEGER)"
	details := &facetDetails{results: map[string]detailResult{"ddl": {detail: metadata.Detail{
		Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Text: body, Provenance: "generated-ddl",
	}}}}
	req := facetNavigationRequest("SELECT * FROM T", details)
	location := detailLocation(t, req, "T")
	if location.Source == nil || location.Source.Text != body || location.Source.Provenance != "generated-ddl" {
		t.Fatalf("source = %#v, want generated DDL body", location.Source)
	}
	position, err := language.PositionAt(body, strings.Index(body, "T ("))
	if err != nil {
		t.Fatal(err)
	}
	end, err := language.PositionAt(body, strings.Index(body, "T (")+1)
	if err != nil {
		t.Fatal(err)
	}
	if location.Range.Start != position || location.Range.End != end {
		t.Fatalf("DDL body range = %#v, want table declaration span %#v..%#v", location.Range, position, end)
	}
	if len(details.calls) != 1 || details.calls[0].Facet != "ddl" {
		t.Fatalf("detail calls = %#v, want only DDL", details.calls)
	}
}

func TestDefinitionUsesNameRangeForExecutableViewDDL(t *testing.T) {
	body := "CREATE VIEW DUP AS SELECT 1 FROM RDB$DATABASE"
	details := &facetDetails{results: map[string]detailResult{"ddl": {detail: metadata.Detail{
		Object: catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "DUP"}, Text: body, Provenance: "interbase:view-ddl",
	}}}}
	req := facetNavigationRequest("SELECT * FROM DUP", details)
	location := detailLocation(t, req, "DUP")
	if location.Source == nil || location.Source.Provenance != "interbase:view-ddl" {
		t.Fatalf("view DDL source = %#v, want executable view DDL", location.Source)
	}
	want, err := language.PositionAt(body, strings.Index(body, "DUP"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Range.Start != want || location.Range.End != want {
		t.Fatalf("view DDL range = %#v, want name point %#v", location.Range, want)
	}
}

func TestDefinitionUsesNativeSourceOnlyAfterUnsupportedDDL(t *testing.T) {
	text := "EXECUTE PROCEDURE DUP"
	body := "CREATE PROCEDURE DUP AS BEGIN END"
	details := &facetDetails{results: map[string]detailResult{
		"ddl":    {err: structuredUnsupportedDDL{}},
		"source": {detail: metadata.Detail{Object: catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "DUP"}, Text: body, Provenance: "interbase:procedure-source"}},
	}}
	req := facetNavigationRequest(text, details)
	location := detailLocation(t, req, "DUP")
	if location.Source == nil || location.Source.Provenance != "interbase:procedure-source" || location.Source.Text != body {
		t.Fatalf("source = %#v, want native procedure source", location.Source)
	}
	if !strings.Contains(location.Source.Note, `procedure "DUP": parameter nullability`) {
		t.Fatalf("native source explanation = %q, want structured DDL refusal", location.Source.Note)
	}
	want, err := language.PositionAt(body, strings.Index(body, "DUP"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Range.Start != want || location.Range.End != want {
		t.Fatalf("native source range = %#v, want name point %#v", location.Range, want)
	}
	if len(details.calls) != 2 || details.calls[0].Facet != "ddl" || details.calls[1].Facet != "source" {
		t.Fatalf("facet sequence = %#v, want DDL then native source", details.calls)
	}
}

func TestDefinitionNativeViewColumnUsesOnlyDeclaredColumnSpan(t *testing.T) {
	for _, tc := range []struct {
		name       string
		source     string
		wantSource bool
	}{
		{name: "explicit declaration", source: "-- 😀\r\nCREATE VIEW DUP (ID) AS SELECT 1 FROM CITY", wantSource: true},
		{name: "source body without declaration", source: "-- 😀\r\nSELECT DUP FROM CITY", wantSource: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := readyNavigationCache()
			viewColumn := &catalog.ColumnDesc{ColumnBase: catalog.ColumnBase{Schema: "A", Table: "DUP", Name: "ID"}}
			cache.Catalog.Views["DUP"].Columns = []*catalog.ColumnDesc{viewColumn}
			cache.Catalog.ViewsByID[(catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "DUP"}).Key()].Columns = []*catalog.ColumnDesc{viewColumn}
			details := &facetDetails{results: map[string]detailResult{
				"ddl":    {err: database.ErrUnsupportedDDL},
				"source": {detail: metadata.Detail{Object: catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "DUP"}, Text: tc.source, Provenance: "interbase:view-source"}},
			}}
			req := facetNavigationRequest("SELECT ID FROM DUP", details)
			req.Context.Snapshot.Cache = cache
			location := detailLocation(t, req, "ID")
			if !tc.wantSource {
				if location.Source != nil {
					t.Fatalf("undeclared view column got a fabricated source range: %#v", location)
				}
				return
			}
			if location.Source == nil || location.Source.Text != tc.source {
				t.Fatalf("view source = %#v, want declared native source", location.Source)
			}
			start := strings.Index(tc.source, "ID")
			wantStart, err := language.PositionAt(tc.source, start)
			if err != nil {
				t.Fatal(err)
			}
			wantEnd, err := language.PositionAt(tc.source, start+len("ID"))
			if err != nil {
				t.Fatal(err)
			}
			if location.Range != (language.Range{Start: wantStart, End: wantEnd}) {
				t.Fatalf("native view-column range=%#v, want declaration span %#v..%#v", location.Range, wantStart, wantEnd)
			}
		})
	}
}

func TestDefinitionRequiresDDLDeclarationNotCommentOrLiteralName(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "comment and literal precede declaration", body: "-- DUP\r\nSELECT 'DUP';\r\nCREATE PROCEDURE DUP AS BEGIN END"},
		{name: "quoted declaration follows comment", body: "-- DUP\r\nCREATE PROCEDURE \"DUP\" AS BEGIN END"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			details := &facetDetails{results: map[string]detailResult{"ddl": {detail: metadata.Detail{
				Object: catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "DUP"}, Text: tc.body, Provenance: "interbase:procedure-ddl",
			}}}}
			req := facetNavigationRequest("EXECUTE PROCEDURE DUP", details)
			location := detailLocation(t, req, "DUP")
			if location.Source == nil {
				t.Fatalf("valid declaration was not navigable")
			}
			declarationName := strings.LastIndex(tc.body, "DUP")
			if tc.body[declarationName-1] == '"' {
				declarationName--
			}
			want, err := language.PositionAt(tc.body, declarationName)
			if err != nil {
				t.Fatal(err)
			}
			if location.Range.Start != want || location.Range.End != want {
				t.Fatalf("range=%#v, want declaration point %#v in %q", location.Range, want, tc.body)
			}
		})
	}
}

func TestDefinitionRejectsNameWithoutDDLDeclaration(t *testing.T) {
	body := "-- DUP\r\nSELECT 'DUP'"
	details := &facetDetails{results: map[string]detailResult{"ddl": {detail: metadata.Detail{
		Object: catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "DUP"}, Text: body, Provenance: "interbase:procedure-ddl",
	}}}}
	req := facetNavigationRequest("EXECUTE PROCEDURE DUP", details)
	if location := detailLocation(t, req, "DUP"); location.Source != nil {
		t.Fatalf("comment/literal identifier published as declaration: %#v", location)
	}
}

func TestDefinitionTableUnsupportedUsesValidatedDescriptionSpan(t *testing.T) {
	body := "CREATE TABLE T (\r\n  C INTEGER\r\n)"
	colSpan := spanOf(body, "C")
	tableStart := strings.Index(body, "T (")
	description := catalog.TableDescription{Body: body, Table: catalog.DescriptionSpan{Start: tableStart, End: tableStart + 1}, Columns: []catalog.DescriptionColumn{{Name: "C", Span: colSpan}}}
	details := &facetDetails{results: map[string]detailResult{
		"ddl":         {err: database.ErrUnsupportedDDL},
		"description": {detail: metadata.Detail{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Description: description}},
	}}
	cache := readyNavigationCache()
	cache.ColumnsByID[(catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}).Key()] = append(cache.ColumnsByID[(catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}).Key()], &catalog.ColumnDesc{ColumnBase: catalog.ColumnBase{Schema: "A", Table: "T", Name: "C"}})
	req := facetNavigationRequest("SELECT C FROM T", details)
	req.Context.Snapshot.Cache = cache
	location := detailLocation(t, req, "C")
	if location.Source == nil || location.Source.Text != body || location.Source.Provenance != "catalog-description" {
		t.Fatalf("source = %#v, calls=%#v, want informational table description", location.Source, details.calls)
	}
	wantStart, err := language.PositionAt(body, colSpan.Start)
	if err != nil {
		t.Fatal(err)
	}
	wantEnd, err := language.PositionAt(body, colSpan.End)
	if err != nil {
		t.Fatal(err)
	}
	if location.Range != (language.Range{Start: wantStart, End: wantEnd}) {
		t.Fatalf("description column range = %#v, want %#v", location.Range, language.Range{Start: wantStart, End: wantEnd})
	}
	if len(details.calls) != 2 || details.calls[1].Facet != "description" {
		t.Fatalf("facet sequence = %#v, want DDL then description", details.calls)
	}
}

func TestDefinitionRefusesMalformedDescriptionSpan(t *testing.T) {
	body := "CREATE TABLE T (C INTEGER)"
	details := &facetDetails{results: map[string]detailResult{
		"ddl":         {err: database.ErrUnsupportedDDL},
		"description": {detail: metadata.Detail{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Description: catalog.TableDescription{Body: body, Table: catalog.DescriptionSpan{Start: strings.Index(body, "T ("), End: strings.Index(body, "T (") + 1}, Columns: []catalog.DescriptionColumn{{Name: "C", Span: catalog.DescriptionSpan{Start: -1, End: 1}}}}}},
	}}
	cache := readyNavigationCache()
	cache.ColumnsByID[(catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}).Key()] = append(cache.ColumnsByID[(catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}).Key()], &catalog.ColumnDesc{ColumnBase: catalog.ColumnBase{Schema: "A", Table: "T", Name: "C"}})
	req := facetNavigationRequest("SELECT C FROM T", details)
	req.Context.Snapshot.Cache = cache
	location := detailLocation(t, req, "C")
	if location.Source != nil {
		t.Fatalf("malformed description published as source: %#v", location)
	}
}

func TestDefinitionDoesNotFallbackAfterDetailWaitIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := facetNavigationRequest("EXECUTE PROCEDURE DUP", nil)
	req.Context.Details = cancelingDetails{cancel: cancel}
	position := language.Position{Line: 0, UTF16Column: strings.LastIndex(req.Document.Text, "DUP")}
	_, err := language.NewService().Definition(ctx, req, position)
	if !errors.Is(err, language.ErrCanceled) {
		t.Fatalf("Definition err=%v, want canceled after DDL wait", err)
	}
}

func TestDefinitionRejectsMismatchedDetailIdentity(t *testing.T) {
	details := &facetDetails{results: map[string]detailResult{
		"ddl":    {detail: metadata.Detail{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "t"}, Text: "CREATE TABLE t (ID INTEGER)", Provenance: "generated-ddl"}},
		"source": {detail: metadata.Detail{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Text: "SHOULD NOT BE USED"}},
	}}
	req := facetNavigationRequest("SELECT * FROM T", details)
	position := language.Position{Line: 0, UTF16Column: strings.LastIndex(req.Document.Text, "T")}
	result, err := language.NewService().Definition(context.Background(), req, position)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(result.Value) != 0 || len(details.calls) != 1 {
		t.Fatalf("mismatched detail identity published or fell back: result=%#v calls=%#v", result, details.calls)
	}
}

func TestHoverKeepsTableSummaryWithoutExecutableDDLClaim(t *testing.T) {
	body := "CREATE TABLE T (ID INTEGER)"
	details := &facetDetails{results: map[string]detailResult{
		"ddl":         {err: database.ErrUnsupportedDDL},
		"description": {detail: metadata.Detail{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Description: catalog.TableDescription{Body: body, Table: catalog.DescriptionSpan{Start: strings.Index(body, "T ("), End: strings.Index(body, "T (") + 1}}}},
	}}
	cache := readyNavigationCache()
	req := facetNavigationRequest("SELECT * FROM T", details)
	req.Context.Snapshot.Cache = cache
	result, err := language.NewService().Hover(context.Background(), req, language.Position{Line: 0, UTF16Column: 14})
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if !strings.Contains(result.Value.Markdown, "ID") || !strings.Contains(result.Value.Markdown, "Informational catalog description") || strings.Contains(result.Value.Markdown, "DDL unavailable") {
		t.Fatalf("hover markdown lost table summary or mislabeled fallback: %q", result.Value.Markdown)
	}
	if len(details.calls) != 2 || details.calls[0].Facet != "ddl" || details.calls[1].Facet != "description" {
		t.Fatalf("hover detail requests = %#v, want DDL then description", details.calls)
	}
}

func TestHoverReturnsCachedProcedureSummaryWithoutDetails(t *testing.T) {
	cache := readyNavigationCache()
	desc := &catalog.ProcedureDesc{Name: "DUP", Description: sql.NullString{String: "procedure documentation", Valid: true}}
	cache.Catalog.Procedures["DUP"] = desc
	cache.Catalog.ProceduresByID[(catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "DUP"}).Key()] = desc
	req := facetNavigationRequest("EXECUTE PROCEDURE DUP", nil)
	req.Context.Snapshot.Cache = cache
	req.Context.Details = nil
	result, err := language.NewService().Hover(context.Background(), req, language.Position{Line: 0, UTF16Column: strings.LastIndex(req.Document.Text, "DUP")})
	if err != nil {
		t.Fatalf("Hover without details: %v", err)
	}
	want := catalog.ProcedureDoc(desc)
	if result.Value.Markdown != want {
		t.Fatalf("cached procedure hover = %q, want %q", result.Value.Markdown, want)
	}
}

func TestHoverReturnsCachedTriggerSummaryWithoutDetails(t *testing.T) {
	cache := readyNavigationCache()
	delete(cache.Catalog.Procedures, "DUP")
	delete(cache.Catalog.ProceduresByID, (catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "DUP"}).Key())
	delete(cache.Catalog.Views, "DUP")
	delete(cache.Catalog.ViewsByID, (catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "DUP"}).Key())
	trigger := &catalog.TriggerDesc{Name: "DUP", Description: sql.NullString{String: "trigger documentation", Valid: true}}
	cache.Catalog.Triggers["DUP"] = trigger
	cache.Catalog.TriggersByID[(catalog.ObjectID{Kind: catalog.ObjectKindTrigger, Name: "DUP"}).Key()] = trigger
	cache.Catalog.RebuildObjects(strings.ToUpper)
	req := facetNavigationRequest("DUP", nil)
	req.Context.Snapshot.Cache = cache
	req.Context.Details = nil
	result, err := language.NewService().Hover(context.Background(), req, language.Position{Line: 0, UTF16Column: 1})
	if err != nil {
		t.Fatalf("Hover without details: %v", err)
	}
	if result.Value.Markdown != catalog.TriggerDoc(trigger) {
		t.Fatalf("cached trigger hover = %q, want %q", result.Value.Markdown, catalog.TriggerDoc(trigger))
	}
}

func TestHoverCombinesCachedProcedureSummaryAndDDLAppendix(t *testing.T) {
	cache := readyNavigationCache()
	desc := &catalog.ProcedureDesc{Name: "DUP", Description: sql.NullString{String: "procedure documentation", Valid: true}}
	cache.Catalog.Procedures["DUP"] = desc
	cache.Catalog.ProceduresByID[(catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "DUP"}).Key()] = desc
	body := "CREATE PROCEDURE DUP AS BEGIN END"
	details := &facetDetails{results: map[string]detailResult{"ddl": {detail: metadata.Detail{
		Object: catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "DUP"}, Text: body, Provenance: "interbase:procedure-ddl",
	}}}}
	req := facetNavigationRequest("EXECUTE PROCEDURE DUP", details)
	req.Context.Snapshot.Cache = cache
	result, err := language.NewService().Hover(context.Background(), req, language.Position{Line: 0, UTF16Column: strings.LastIndex(req.Document.Text, "DUP")})
	if err != nil {
		t.Fatalf("Hover with DDL: %v", err)
	}
	want := strings.TrimRight(catalog.ProcedureDoc(desc), "\n") + "\n\n```sql\n" + body + "\n```"
	if result.Value.Markdown != want {
		t.Fatalf("hover summary+DDL = %q, want %q", result.Value.Markdown, want)
	}
}

func TestHoverPreservesCachedProcedureSummaryWhenDetailIsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "unsupported facet", err: metadata.ErrUnsupported},
		{name: "object not found", err: database.ErrObjectNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := readyNavigationCache()
			desc := &catalog.ProcedureDesc{Name: "DUP", Description: sql.NullString{String: "procedure documentation", Valid: true}}
			cache.Catalog.Procedures["DUP"] = desc
			cache.Catalog.ProceduresByID[(catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "DUP"}).Key()] = desc
			details := &facetDetails{results: map[string]detailResult{"ddl": {err: tc.err}}}
			req := facetNavigationRequest("EXECUTE PROCEDURE DUP", details)
			req.Context.Snapshot.Cache = cache
			result, err := language.NewService().Hover(context.Background(), req, language.Position{Line: 0, UTF16Column: strings.LastIndex(req.Document.Text, "DUP")})
			if err != nil {
				t.Fatalf("Hover: %v", err)
			}
			if result.Value.Markdown != catalog.ProcedureDoc(desc) {
				t.Fatalf("hover summary = %q, want %q", result.Value.Markdown, catalog.ProcedureDoc(desc))
			}
		})
	}
}

func TestHoverUsesViewSummaryBeforeSameNamedTable(t *testing.T) {
	cache := readyNavigationCache()
	cache.SchemaTables["A"] = append(cache.SchemaTables["A"], "DUP")
	view := &catalog.ViewDesc{Name: "DUP", Description: sql.NullString{String: "view documentation", Valid: true}}
	cache.Catalog.Views["DUP"] = view
	cache.Catalog.ViewsByID[(catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "DUP"}).Key()] = view
	body := "CREATE VIEW DUP AS SELECT 1 FROM RDB$DATABASE"
	details := &facetDetails{results: map[string]detailResult{"ddl": {detail: metadata.Detail{
		Object: catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "DUP"}, Text: body, Provenance: "interbase:view-ddl",
	}}}}
	req := facetNavigationRequest("SELECT * FROM DUP", details)
	req.Context.Snapshot.Cache = cache
	result, err := language.NewService().Hover(context.Background(), req, language.Position{Line: 0, UTF16Column: strings.LastIndex(req.Document.Text, "DUP")})
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	want := strings.TrimRight(catalog.ViewDoc(view), "\n") + "\n\n```sql\n" + body + "\n```"
	if result.Value.Markdown != want {
		t.Fatalf("view hover = %q, want view summary before DDL: %q", result.Value.Markdown, want)
	}
	if len(details.calls) != 1 || details.calls[0].Object.Kind != catalog.ObjectKindView {
		t.Fatalf("hover detail requests = %#v, want view DDL before same-name table", details.calls)
	}
}

func TestDefinitionDoesNotFallbackOnNotFoundTransportOrCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "not found", err: database.ErrObjectNotFound},
		{name: "transport", err: errors.New("network failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			details := &facetDetails{results: map[string]detailResult{"ddl": {err: tc.err}, "source": {detail: metadata.Detail{Text: "SHOULD NOT BE USED"}}}}
			req := facetNavigationRequest("EXECUTE PROCEDURE DUP", details)
			position := language.Position{Line: 0, UTF16Column: strings.LastIndex(req.Document.Text, "DUP")}
			result, err := language.NewService().Definition(context.Background(), req, position)
			if tc.name == "transport" {
				if err == nil || len(details.calls) != 1 {
					t.Fatalf("Definition err=%v calls=%#v, want transport error without fallback", err, details.calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("Definition: %v", err)
			}
			if len(result.Value) != 0 || len(details.calls) != 1 {
				t.Fatalf("unexpected fallback: result=%#v calls=%#v", result, details.calls)
			}
		})
	}
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		details := &facetDetails{results: map[string]detailResult{"ddl": {err: database.ErrUnsupportedDDL}, "source": {detail: metadata.Detail{Text: "SHOULD NOT BE USED"}}}}
		req := facetNavigationRequest("EXECUTE PROCEDURE DUP", details)
		position := language.Position{Line: 0, UTF16Column: strings.Index(req.Document.Text, "DUP")}
		_, err := language.NewService().Definition(ctx, req, position)
		if !errors.Is(err, language.ErrCanceled) || len(details.calls) != 0 {
			t.Fatalf("Definition err=%v calls=%#v, want canceled before detail", err, details.calls)
		}
	})
}

func TestSourceSharingBetweenHoverAndDefinition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	registry := metadata.NewRegistry(ctx, metadata.Options{})
	defer registry.Close(ctx)
	provider := metadatatest.New()
	provider.DetailValue = metadata.Detail{
		Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"},
		Text:   "CREATE TABLE T (ID INTEGER)", Provenance: "generated-ddl",
	}
	key := metadata.Key{Engine: "interbase", URL: "ib://host/db"}
	lease, err := registry.Acquire(ctx, key, metadata.Scope{Principal: "USER"}, provider.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := metadatatest.WaitReady(ctx, lease); err != nil {
		t.Fatal(err)
	}
	req := language.Request{
		Document: language.Document{URI: "file:///query.sql", Text: "SELECT * FROM T", Version: 1, Binding: 1},
		Context: language.Context{Key: key, Incarnation: lease.Incarnation(), Snapshot: navigationSnapshotWithViewsReady(lease.Snapshot()),
			Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}, DialectResolved: true, Details: lease},
	}
	service := language.NewService()
	position := language.Position{Line: 0, UTF16Column: 14}
	if _, err := service.Hover(ctx, req, position); err != nil {
		t.Fatalf("Hover: %v", err)
	}
	definition, err := service.Definition(ctx, req, position)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(definition.Value) == 0 || definition.Value[0].Source == nil {
		t.Fatalf("Definition did not return source: %#v", definition.Value)
	}
	if got := provider.DetailCalls.Load(); got != 1 {
		t.Fatalf("Hover+Definition loaded detail %d times, want one", got)
	}
}

func TestOldBindingDefinitionResultRejected(t *testing.T) {
	ctx := context.Background()
	gate := make(chan struct{})
	provider := metadatatest.New()
	provider.DetailGate = gate
	provider.DetailValue = metadata.Detail{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Text: "CREATE TABLE T (ID INTEGER)", Provenance: "generated-ddl"}
	registry := metadata.NewRegistry(ctx, metadata.Options{})
	defer registry.Close(ctx)
	key := metadata.Key{Engine: "interbase", URL: "ib://host/db"}
	lease, err := registry.Acquire(ctx, key, metadata.Scope{Principal: "USER"}, provider.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := metadatatest.WaitReady(ctx, lease); err != nil {
		t.Fatal(err)
	}
	req := language.Request{Document: language.Document{URI: "file:///q.sql", Text: "SELECT * FROM T", Version: 4, Binding: 8},
		Context: language.Context{Key: key, Incarnation: lease.Incarnation(), Snapshot: navigationSnapshotWithViewsReady(lease.Snapshot()),
			Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}, Details: lease}}
	done := make(chan struct{})
	var result language.Result[[]language.Location]
	var callErr error
	go func() {
		result, callErr = language.NewService().Definition(ctx, req, language.Position{Line: 0, UTF16Column: 14})
		close(done)
	}()
	select {
	case <-provider.DetailEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("definition did not request detail")
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("definition did not finish")
	}
	if callErr != nil {
		t.Fatalf("Definition: %v", callErr)
	}
	completedDefinition := result
	currentDocument := req.Document
	currentDocument.Binding++
	if language.Current(completedDefinition.Stamp, currentDocument, req.Context) {
		t.Fatal("definition from old database binding accepted")
	}
}

func navigationSnapshotWithViewsReady(snapshot *catalog.MetadataSnapshot) *catalog.MetadataSnapshot {
	copySnapshot := *snapshot
	copyCache := *snapshot.Cache
	copyCache.Metadata = make(map[catalog.MetadataKind]catalog.MetadataState, len(snapshot.Cache.Metadata)+1)
	for kind, state := range snapshot.Cache.Metadata {
		copyCache.Metadata[kind] = state
	}
	copyCache.Metadata[catalog.MetadataViews] = catalog.MetadataReady
	copySnapshot.Cache = &copyCache
	return &copySnapshot
}
