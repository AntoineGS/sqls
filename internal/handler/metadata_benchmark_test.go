package handler

import (
	"context"
	"database/sql"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

func benchmarkMetadataCatalog() *database.DBCache {
	cache := &database.DBCache{
		Schemas:           map[string]string{"PUBLIC": "PUBLIC"},
		SchemaTables:      map[string][]string{"PUBLIC": make([]string, 256)},
		ColumnsWithParent: make(map[string][]*database.ColumnDesc, 256),
		Metadata:          make(map[database.MetadataKind]database.MetadataState),
		Catalog: &database.CatalogCache{
			Views: make(map[string]*database.ViewDesc), Procedures: make(map[string]*database.ProcedureDesc),
			Generators: make(map[string]*database.GeneratorDesc), Domains: make(map[string]*database.DomainDesc),
			Functions: make(map[string]*database.FunctionDesc), Indexes: make(map[string]*database.IndexDesc),
			IndexesByTable: make(map[string][]*database.IndexDesc), Triggers: make(map[string]*database.TriggerDesc),
			TriggersByTable: make(map[string][]*database.TriggerDesc),
		},
	}
	kinds := []database.MetadataKind{database.MetadataSchemas, database.MetadataRelations, database.MetadataColumnsCurrent, database.MetadataColumnsAll, database.MetadataPrimaryKeys, database.MetadataForeignKeys, database.MetadataViews, database.MetadataProcedures, database.MetadataGenerators, database.MetadataDomains, database.MetadataFunctions, database.MetadataIndexes, database.MetadataTriggers}
	for _, kind := range kinds {
		cache.Metadata[kind] = database.MetadataReady
	}
	for i := range cache.SchemaTables["PUBLIC"] {
		table := "TABLE_" + strconv.Itoa(i)
		cache.SchemaTables["PUBLIC"][i] = table
		cols := make([]*database.ColumnDesc, 12)
		for j := range cols {
			cols[j] = &database.ColumnDesc{ColumnBase: database.ColumnBase{Schema: "PUBLIC", Table: table, Name: "COLUMN_" + strconv.Itoa(j)}, Type: "VARCHAR(128)", Null: "YES"}
		}
		cache.ColumnsWithParent["PUBLIC\t"+table] = cols
		index := &database.IndexDesc{Schema: "PUBLIC", Name: "IDX_" + table, RelationName: table, Columns: []string{"COLUMN_0"}, Unique: sql.NullBool{Bool: false, Valid: true}, Active: sql.NullBool{Bool: true, Valid: true}}
		cache.Catalog.Indexes[index.Name] = index
		cache.Catalog.IndexesByTable[table] = []*database.IndexDesc{index}
	}
	return cache
}

func BenchmarkMetadataDiagnosticCatalogReuse(b *testing.B) {
	s := NewServer()
	defer s.Stop()
	cache := benchmarkMetadataCatalog()
	b.Run("same-cache", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if got := s.diagnosticCatalogFor(cache); got == nil {
				b.Fatal("missing derived catalog")
			}
		}
	})
	b.Run("new-cache-conversion", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			newCache := *cache // maps are immutable and shared; the snapshot pointer is new
			if got := s.diagnosticCatalogFor(&newCache); got == nil {
				b.Fatal("missing derived catalog")
			}
		}
	})
}

type benchmarkPlanRepository struct {
	database.DBRepository
	plan database.MetadataPlan
}

func (r benchmarkPlanRepository) MetadataPlan() database.MetadataPlan { return r.plan }

func BenchmarkCompletionDuringMetadataLoad(b *testing.B) {
	s := NewServer()
	gate := make(chan struct{})
	entered := make(chan struct{})
	s.metadata.Reset(1)
	load, err := s.metadata.Start(context.Background(), 1, benchmarkPlanRepository{DBRepository: &database.MockDBRepository{}, plan: database.MetadataPlan{Parallelism: 1, Jobs: []database.MetadataJob{{Kind: database.MetadataDomains, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
		close(entered)
		<-gate
		return database.MetadataPatch{}, nil
	}}}}})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { close(gate); <-load.Done; _ = s.Stop() }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		b.Fatal("metadata job did not enter gate")
	}

	clientSide, serverSide := net.Pipe()
	client := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(clientSide, jsonrpc2.VSCodeObjectCodec{}), jsonrpc2.HandlerWithError(func(context.Context, *jsonrpc2.Conn, *jsonrpc2.Request) (interface{}, error) { return nil, nil }))
	server := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(serverSide, jsonrpc2.VSCodeObjectCodec{}), NewDispatcher(jsonrpc2.HandlerWithError(s.Handle)))
	defer client.Close()
	defer server.Close()
	if err := client.Call(context.Background(), "initialize", lsp.InitializeParams{}, nil); err != nil {
		b.Fatal(err)
	}
	uri := "file:///benchmark.sql"
	if err := client.Notify(context.Background(), "textDocument/didOpen", lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{URI: uri, LanguageID: "sql", Text: "select ", Version: 1}}); err != nil {
		b.Fatal(err)
	}
	params := lsp.CompletionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{TextDocument: lsp.TextDocumentIdentifier{URI: uri}, Position: lsp.Position{Line: 0, Character: 7}}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var result lsp.CompletionList
		if err := client.Call(context.Background(), "textDocument/completion", params, &result); err != nil {
			b.Fatal(err)
		}
	}
}
