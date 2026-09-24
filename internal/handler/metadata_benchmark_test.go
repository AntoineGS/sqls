package handler

import (
	"context"
	"database/sql"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

func TestBenchmarkMetadataCatalogUsesGeneratedDefaultSchema(t *testing.T) {
	cache := benchmarkMetadataCatalog(t)
	assertBenchmarkMetadataCatalog(t, cache)
}

func assertBenchmarkMetadataCatalog(tb testing.TB, cache *database.DBCache) {
	tb.Helper()
	if len(cache.SortedTables()) == 0 {
		tb.Fatal("benchmark cache has no tables in its default schema")
	}
	if !cache.MetadataReady(database.MetadataSchemas, database.MetadataRelations, database.MetadataColumnsCurrent, database.MetadataIndexes) {
		tb.Fatalf("benchmark cache readiness is incomplete: %+v", cache.Metadata)
	}
	columns, ok := cache.ColumnDescs(cache.SortedTables()[0])
	if !ok || len(columns) == 0 {
		tb.Fatal("benchmark cache has no representative columns")
	}
	if len(cache.Catalog.Indexes) == 0 || len(cache.PrimaryKeyColumns) == 0 {
		tb.Fatal("benchmark cache has no representative indexes/keys")
	}
	for _, table := range cache.SchemaTables["WORLD"] {
		if len(cache.Catalog.IndexesByTable[table]) == 0 {
			tb.Fatalf("table %q has no grouped index", table)
		}
	}
}

func benchmarkMetadataCatalog(tb testing.TB) *database.DBCache {
	tb.Helper()
	cache, err := database.NewDBCacheUpdater(database.NewMockDBRepository(nil)).GenerateDBCachePrimary(context.Background())
	if err != nil {
		tb.Fatalf("generate seeded metadata cache: %v", err)
	}
	cache.Catalog = &database.CatalogCache{
		Views: make(map[string]*database.ViewDesc), Procedures: make(map[string]*database.ProcedureDesc),
		Generators: make(map[string]*database.GeneratorDesc), Domains: make(map[string]*database.DomainDesc),
		Functions: make(map[string]*database.FunctionDesc), Indexes: make(map[string]*database.IndexDesc),
		IndexesByTable: make(map[string][]*database.IndexDesc), Triggers: make(map[string]*database.TriggerDesc),
		TriggersByTable: make(map[string][]*database.TriggerDesc),
	}
	schemaName, ok := cache.Database("world")
	if !ok {
		tb.Fatal("generated fixture did not provide its current schema")
	}
	schema := strings.ToUpper(schemaName)
	cache.SchemaTables[schema] = make([]string, 256)
	cache.ColumnsWithParent = make(map[string][]*database.ColumnDesc, 256)
	cache.PrimaryKeyColumns = make(map[string]map[string]struct{}, 256)
	kinds := []database.MetadataKind{database.MetadataSchemas, database.MetadataRelations, database.MetadataColumnsCurrent, database.MetadataColumnsAll, database.MetadataPrimaryKeys, database.MetadataForeignKeys, database.MetadataViews, database.MetadataProcedures, database.MetadataGenerators, database.MetadataDomains, database.MetadataFunctions, database.MetadataIndexes, database.MetadataTriggers}
	for _, kind := range kinds {
		cache.Metadata[kind] = database.MetadataReady
	}
	for i := range cache.SchemaTables[schema] {
		table := "TABLE_" + strconv.Itoa(i)
		cache.SchemaTables[schema][i] = table
		cols := make([]*database.ColumnDesc, 12)
		for j := range cols {
			cols[j] = &database.ColumnDesc{ColumnBase: database.ColumnBase{Schema: schema, Table: table, Name: "COLUMN_" + strconv.Itoa(j)}, Type: "VARCHAR(128)", Null: "YES"}
		}
		cache.ColumnsWithParent[schema+"\t"+table] = cols
		cache.PrimaryKeyColumns[schema+"\t"+table] = map[string]struct{}{"COLUMN_0": {}}
		index := &database.IndexDesc{Schema: schema, Name: "IDX_" + table, RelationName: table, Columns: []string{"COLUMN_0"}, Unique: sql.NullBool{Bool: false, Valid: true}, Active: sql.NullBool{Bool: true, Valid: true}}
		cache.Catalog.Indexes[index.Name] = index
		cache.Catalog.IndexesByTable[table] = []*database.IndexDesc{index}
	}
	return cache
}

func BenchmarkMetadataDiagnosticCatalogReuse(b *testing.B) {
	s := NewServer()
	defer s.Stop()
	cache := benchmarkMetadataCatalog(b)
	assertBenchmarkMetadataCatalog(b, cache)
	b.Run("same-cache", func(b *testing.B) {
		b.StopTimer()
		if got := s.diagnosticCatalogFor(cache); got == nil {
			b.Fatal("prewarming derived catalog returned nil")
		}
		b.ReportAllocs()
		b.ResetTimer()
		b.StartTimer()
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
	s.stateMu.Lock()
	s.connGeneration = 1
	s.connectionState = connectionReady
	s.stateMu.Unlock()
	s.metadata.Reset(1)
	seed, err := database.NewDBCacheUpdater(database.NewMockDBRepository(nil)).GenerateDBCachePrimary(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	plan := database.MetadataPlan{Parallelism: 3, Jobs: []database.MetadataJob{
		{Kind: database.MetadataSchemas, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{Cache: seed}, nil
		}},
		{Kind: database.MetadataRelations, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{Cache: seed}, nil
		}},
		{Kind: database.MetadataColumnsCurrent, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			close(entered)
			<-gate
			return database.MetadataPatch{Cache: seed}, nil
		}},
	}}
	load, err := s.metadata.Start(context.Background(), 1, benchmarkPlanRepository{DBRepository: &database.MockDBRepository{}, plan: plan})
	if err != nil {
		b.Fatal(err)
	}
	var client, server *jsonrpc2.Conn
	defer func() {
		close(gate)
		<-load.Done
		_ = s.Stop()
		<-s.cleanupDone
		<-s.diagnosticsDone
		if client != nil {
			_ = client.Close()
		}
		if server != nil {
			_ = server.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		b.Fatal("metadata job did not enter gate")
	}

	clientSide, serverSide := net.Pipe()
	client = jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(clientSide, jsonrpc2.VSCodeObjectCodec{}), jsonrpc2.HandlerWithError(func(context.Context, *jsonrpc2.Conn, *jsonrpc2.Request) (interface{}, error) { return nil, nil }))
	server = jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(serverSide, jsonrpc2.VSCodeObjectCodec{}), NewDispatcher(jsonrpc2.HandlerWithError(s.Handle)))
	if err := client.Call(context.Background(), "initialize", lsp.InitializeParams{}, nil); err != nil {
		b.Fatal(err)
	}
	uri := "file:///benchmark.sql"
	if err := client.Notify(context.Background(), "textDocument/didOpen", lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{URI: uri, LanguageID: "sql", Text: "select * from ", Version: 1}}); err != nil {
		b.Fatal(err)
	}
	params := lsp.CompletionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{TextDocument: lsp.TextDocumentIdentifier{URI: uri}, Position: lsp.Position{Line: 0, Character: 14}}}
	check := func(result lsp.CompletionList) {
		b.Helper()
		if !result.IsIncomplete {
			b.Fatal("loading completion was not marked incomplete")
		}
		for _, item := range result.Items {
			if item.Label == "city" {
				return
			}
		}
		b.Fatalf("expected useful table completion while columns load, got %#v", result.Items)
	}
	var initial lsp.CompletionList
	if err := client.Call(context.Background(), "textDocument/completion", params, &initial); err != nil {
		b.Fatal(err)
	}
	check(initial)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var result lsp.CompletionList
		if err := client.Call(context.Background(), "textDocument/completion", params, &result); err != nil {
			b.Fatal(err)
		}
		check(result)
	}
}
