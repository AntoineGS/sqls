package handler

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

type provenNavigationProvider struct {
	loads atomic.Int64
	gate  *provenDetailGate
}

func (p *provenNavigationProvider) Plan() catalog.MetadataPlan {
	return catalog.MetadataPlan{Parallelism: 3, Jobs: []catalog.MetadataJob{
		{Kind: database.MetadataRelations, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{Cache: &catalog.Cache{DefaultSchema: "", SchemaTables: map[string][]string{"": {"DATABASEID"}}}}, nil
		}},
		{Kind: database.MetadataColumnsCurrent, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			columns := []*catalog.ColumnDesc{{ColumnBase: catalog.ColumnBase{Table: "DATABASEID", Name: "DBID"}, Type: "INTEGER"}}
			return catalog.MetadataPatch{Cache: &catalog.Cache{ColumnsByID: map[string][]*catalog.ColumnDesc{(catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "DATABASEID"}).Key(): columns}, ColumnsWithParent: catalog.GenColumnMap(columns)}}, nil
		}},
		{Kind: database.MetadataViews, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Views: map[string]*catalog.ViewDesc{}, ViewsByID: map[string]*catalog.ViewDesc{}}}}, nil
		}},
	}}
}

func (p *provenNavigationProvider) DetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	if req.Object != (catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "DATABASEID"}) || req.Facet != "ddl" {
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
	return metadata.DetailPlan{Requires: []catalog.MetadataKind{database.MetadataRelations}, Coverage: req.Object.Key() + "\x00ddl",
		Load: func(context.Context, *catalog.Cache) (metadata.Detail, error) {
			p.loads.Add(1)
			if p.gate != nil {
				p.gate.entered <- struct{}{}
				<-p.gate.release
			}
			return metadata.Detail{Object: req.Object, Text: "CREATE TABLE DATABASEID (DBID INTEGER)", Provenance: "generated-ddl"}, nil
		}}, nil
}
func (*provenNavigationProvider) Close() error { return nil }

type provenDetailGate struct{ entered, release chan struct{} }

func readyProvenNavigationLease(t *testing.T, s *Server, key string, provider *provenNavigationProvider) *metadata.Lease {
	t.Helper()
	lease, err := s.registry.Acquire(context.Background(), metadata.Key{Engine: "test", URL: key}, metadata.Scope{Principal: "test"}, func(context.Context) (metadata.Provider, error) { return provider, nil })
	if err != nil {
		t.Fatal("Acquire:", err)
	}
	t.Cleanup(lease.Release)
	if err := metadatatest.WaitReady(context.Background(), lease); err != nil {
		t.Fatal("WaitReady:", err)
	}
	return lease
}

func TestDefinitionProvenColumnUsesLeaseDetailAndPublishesColumnRange(t *testing.T) {
	s := newDefinitionServer(t)
	provider := &provenNavigationProvider{}
	lease := readyProvenNavigationLease(t, s, "proven-column", provider)
	const text = "ALTER PROCEDURE P AS\nDECLARE VARIABLE DBID INTEGER;\nBEGIN\nSELECT DBID FROM DATABASEID INTO :DBID;\nEND"
	s.files[testFileURI] = &File{Text: text, Version: 1, Revision: 1}
	s.connGeneration, s.metaBoundGeneration, s.metaLease = 1, 1, lease
	s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}
	character := strings.Index(strings.Split(text, "\n")[3], "DBID")
	for attempt := 0; attempt < 2; attempt++ {
		params, _ := json.Marshal(lsp.DefinitionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI}, Position: lsp.Position{Line: 3, Character: character},
		}})
		raw := json.RawMessage(params)
		value, err := s.handleDefinition(context.Background(), nil, &jsonrpc2.Request{Params: &raw})
		if err != nil {
			t.Fatalf("handleDefinition: %v", err)
		}
		locations, ok := value.([]lsp.Location)
		if !ok || len(locations) != 1 || locations[0].URI == testFileURI {
			t.Fatalf("definition locations = %#v, want one published source location (detail loads=%d snapshot=%+v)", value, provider.loads.Load(), lease.Snapshot().Status)
		}
		path := snapshotPathFromURI(t, locations[0].URI)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("read source snapshot:", err)
		}
		lines := strings.Split(string(content), "\n")
		line := locations[0].Range.Start.Line
		if line != locations[0].Range.End.Line || line < 0 || line >= len(lines) || locations[0].Range.End.Character > len(lines[line]) || lines[line][locations[0].Range.Start.Character:locations[0].Range.End.Character] != "DBID" {
			t.Fatalf("published range=%+v selects wrong source text", locations[0].Range)
		}
	}
	if got := provider.loads.Load(); got != 1 {
		t.Fatalf("lease detail loads=%d, want one shared load across equivalent requests", got)
	}
}

func TestDefinitionProvenColumnCancellationWhileDetailBlockedPublishesNothing(t *testing.T) {
	s := newDefinitionServer(t)
	gate := provenDetailGate{entered: make(chan struct{}, 1), release: make(chan struct{})}
	provider := &provenNavigationProvider{gate: &gate}
	lease := readyProvenNavigationLease(t, s, "proven-column-cancel", provider)
	const text = "ALTER PROCEDURE P AS\nDECLARE VARIABLE DBID INTEGER;\nBEGIN\nSELECT DBID FROM DATABASEID INTO :DBID;\nEND"
	s.files[testFileURI] = &File{Text: text, Version: 1, Revision: 1}
	s.connGeneration, s.metaBoundGeneration, s.metaLease = 1, 1, lease
	s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}
	line := strings.Split(text, "\n")[3]
	params, _ := json.Marshal(lsp.DefinitionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI}, Position: lsp.Position{Line: 3, Character: strings.Index(line, "DBID")},
	}})
	raw := json.RawMessage(params)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	answer := make(chan struct {
		value interface{}
		err   error
	}, 1)
	go func() {
		value, err := s.handleDefinition(ctx, nil, &jsonrpc2.Request{Params: &raw})
		answer <- struct {
			value interface{}
			err   error
		}{value, err}
	}()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("proven definition did not reach blocked Detail load")
	}
	cancel()
	close(gate.release)
	got := <-answer
	if got.err == nil || got.value != nil {
		t.Fatalf("canceled definition = (%#v, %v), want no published result", got.value, got.err)
	}
	if !snapshotRootIsEmpty(t, s.snapshots) {
		t.Fatal("canceled proven-column source was published")
	}
}
