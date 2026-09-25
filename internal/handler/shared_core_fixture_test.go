package handler

import (
	"context"
	"database/sql"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

func TestNeovimFixtureSnapshotIsDeterministic(t *testing.T) {
	registry := metadata.NewRegistry(context.Background(), metadata.Options{})
	t.Cleanup(func() { _ = registry.Close(context.Background()) })
	provider := &neovimFixtureProvider{variant: "old"}
	lease, err := registry.Acquire(context.Background(), metadata.Key{Engine: "fixture", URL: "fixture://old"}, metadata.Scope{Principal: "fixture"}, provider.factory)
	if err != nil {
		t.Fatal("acquire fixture metadata lease:", err)
	}
	t.Cleanup(lease.Release)
	if err := lease.Refresh(context.Background()); err != nil {
		t.Fatal("start fixture metadata lease:", err)
	}
	if err := metadatatest.WaitReady(context.Background(), lease); err != nil {
		t.Fatalf("fixture metadata readiness: %v; snapshot=%+v", err, lease.Snapshot())
	}
	cache := lease.Snapshot().Cache
	if cache == nil || !cache.HasCatalog() {
		t.Fatal("fixture cache must include a catalog")
	}
	columns, ok := cache.ColumnDescs("CUSTOMERS")
	if !ok || len(columns) != 3 || columns[0].Name != "CUSTOMER_ID" || columns[0].Key != "YES" {
		t.Fatalf("CUSTOMERS columns = %#v, want ordered fixture columns and primary key", columns)
	}
	procedure, ok := cache.Procedure("FIND_CUSTOMER")
	if !ok || len(procedure.InputParameters) != 1 || procedure.InputParameters[0].Name != "P_CUSTOMER_ID" {
		t.Fatalf("FIND_CUSTOMER signature = %#v, want one P_CUSTOMER_ID input", procedure)
	}
	if index, ok := cache.Catalog.Indexes["PK_CUSTOMERS"]; !ok || index.RelationName != "CUSTOMERS" || len(index.Columns) != 1 || index.Columns[0] != "CUSTOMER_ID" {
		t.Fatalf("PK_CUSTOMERS = %#v, want one CUSTOMER_ID segment on CUSTOMERS", index)
	}
	if !strings.Contains(neovimFixtureTableDDL, "CREATE TABLE CUSTOMERS") || !strings.Contains(neovimFixtureProcedureSource, "SUSPEND") {
		t.Fatal("fixture DDL/source contract is incomplete")
	}
	for _, kind := range []catalog.MetadataKind{database.MetadataRelations, database.MetadataColumnsCurrent, database.MetadataPrimaryKeys, database.MetadataProcedures} {
		if !cache.MetadataReady(kind) {
			t.Errorf("fixture category %q is not ready", kind)
		}
	}
	if provider.calls.Load() != 5 {
		t.Fatalf("fixture provider job calls = %d, want 5", provider.calls.Load())
	}

	newProvider := &neovimFixtureProvider{variant: "new"}
	newLease, err := registry.Acquire(context.Background(), metadata.Key{Engine: "fixture", URL: "fixture://new"}, metadata.Scope{Principal: "fixture"}, newProvider.factory)
	if err != nil {
		t.Fatal("acquire new-target fixture lease:", err)
	}
	t.Cleanup(newLease.Release)
	if err := newLease.Refresh(context.Background()); err != nil {
		t.Fatal("start new-target fixture metadata lease:", err)
	}
	if err := metadatatest.WaitReady(context.Background(), newLease); err != nil {
		t.Fatal("new-target metadata readiness:", err)
	}
	newName, _ := newLease.Snapshot().Cache.ColumnDescs("CUSTOMERS")
	if len(newName) != 3 || newName[1].Type != "VARCHAR(100)" || columns[1].Type != "VARCHAR(80)" {
		t.Fatalf("old/new fixture target values = %v / %v, want VARCHAR(80) / VARCHAR(100)", columns, newName)
	}
}

// TestNeovimFixtureServer is a test-binary stdio endpoint for the external
// headless Neovim acceptance client. Do not run it through `go test` with the
// fixture enabled; build the package test binary and invoke this test directly.
func TestNeovimFixtureServer(t *testing.T) {
	if os.Getenv("SQLS_NEOVIM_FIXTURE") != "1" {
		t.Skip("set SQLS_NEOVIM_FIXTURE=1 to run the stdio LSP fixture")
	}
	serveNeovimFixture(t)
}

func neovimFixtureCache(variant string) *catalog.Cache {
	nameType := "VARCHAR(80)"
	if variant == "new" {
		nameType = "VARCHAR(100)"
	}
	columns := []*catalog.ColumnDesc{
		{ColumnBase: catalog.ColumnBase{Table: "CUSTOMERS", Name: "CUSTOMER_ID"}, Type: "INTEGER", Null: "NO", Key: "YES"},
		{ColumnBase: catalog.ColumnBase{Table: "CUSTOMERS", Name: "NAME"}, Type: nameType, Null: "YES", Key: "NO"},
		{ColumnBase: catalog.ColumnBase{Table: "CUSTOMERS", Name: "EMAIL"}, Type: "VARCHAR(120)", Null: "YES", Key: "NO"},
	}
	proc := &catalog.ProcedureDesc{Name: "FIND_CUSTOMER", InputParameters: []*catalog.ProcedureParameterDesc{{Name: "P_CUSTOMER_ID", Position: 0, Direction: catalog.ParameterInput, Type: "INTEGER"}}, OutputParameters: []*catalog.ProcedureParameterDesc{{Name: "O_NAME", Position: 0, Direction: catalog.ParameterOutput, Type: "VARCHAR(80)"}}}
	cache := &catalog.Cache{
		DefaultSchema: "", SchemaTables: map[string][]string{"": {"CUSTOMERS"}},
		ColumnsWithParent: catalog.GenColumnMap(columns), ColumnsByID: map[string][]*catalog.ColumnDesc{(catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "CUSTOMERS"}).Key(): columns},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{}, PrimaryKeyColumns: map[string]map[string]struct{}{"\tCUSTOMERS": {"CUSTOMER_ID": {}}},
		Catalog: &catalog.CatalogCache{
			Procedures:     map[string]*catalog.ProcedureDesc{"FIND_CUSTOMER": proc},
			ProceduresByID: map[string]*catalog.ProcedureDesc{(catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "FIND_CUSTOMER"}).Key(): proc},
			Views:          map[string]*catalog.ViewDesc{}, ViewsByID: map[string]*catalog.ViewDesc{},
			Indexes:     map[string]*catalog.IndexDesc{"PK_CUSTOMERS": {Name: "PK_CUSTOMERS", RelationName: "CUSTOMERS", Columns: []string{"CUSTOMER_ID"}, Unique: sql.NullBool{Bool: true, Valid: true}, Active: sql.NullBool{Bool: true, Valid: true}}},
			IndexesByID: map[string]*catalog.IndexDesc{},
		},
	}
	cache.Catalog.RebuildObjects(strings.ToUpper)
	return cache
}

const (
	neovimFixtureTableDDL        = "CREATE TABLE CUSTOMERS (CUSTOMER_ID INTEGER NOT NULL PRIMARY KEY, NAME VARCHAR(80), EMAIL VARCHAR(120));"
	neovimFixtureProcedureSource = "BEGIN\n  SELECT NAME FROM CUSTOMERS WHERE CUSTOMER_ID = :P_CUSTOMER_ID INTO :O_NAME;\n  SUSPEND;\nEND"
)

type neovimFixtureProvider struct {
	variant string
	calls   atomic.Int64
}

func (p *neovimFixtureProvider) Plan() catalog.MetadataPlan {
	return catalog.MetadataPlan{Parallelism: 3, Jobs: []catalog.MetadataJob{
		{Kind: database.MetadataRelations, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			p.calls.Add(1)
			return catalog.MetadataPatch{Cache: &catalog.Cache{SchemaTables: map[string][]string{"": {"CUSTOMERS"}}}, Count: 1}, nil
		}},
		{Kind: database.MetadataColumnsCurrent, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			p.calls.Add(1)
			cache := neovimFixtureCache(p.variant)
			return catalog.MetadataPatch{Cache: &catalog.Cache{ColumnsWithParent: cache.ColumnsWithParent, ColumnsByID: cache.ColumnsByID}, Count: 3}, nil
		}},
		{Kind: database.MetadataPrimaryKeys, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			p.calls.Add(1)
			keys := map[string]map[string]struct{}{"\tCUSTOMERS": {"CUSTOMER_ID": {}}}
			return catalog.MetadataPatch{Cache: &catalog.Cache{PrimaryKeyColumns: keys}, Count: 1}, nil
		}},
		{Kind: database.MetadataProcedures, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			p.calls.Add(1)
			cache := neovimFixtureCache(p.variant)
			return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Procedures: cache.Catalog.Procedures, ProceduresByID: cache.Catalog.ProceduresByID}}, Count: 1}, nil
		}},
		{Kind: database.MetadataIndexes, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			p.calls.Add(1)
			cache := neovimFixtureCache(p.variant)
			return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Indexes: cache.Catalog.Indexes, IndexesByID: cache.Catalog.IndexesByID}}, Count: 1}, nil
		}},
	}}
}
func (p *neovimFixtureProvider) DetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	text := neovimFixtureTableDDL
	if p.variant == "new" {
		text = strings.ReplaceAll(text, "VARCHAR(80)", "VARCHAR(100)")
	}
	if req.Object.Kind == catalog.ObjectKindProcedure {
		text = neovimFixtureProcedureSource
	}
	return metadata.DetailPlan{Requires: []catalog.MetadataKind{database.MetadataRelations}, Coverage: req.Object.Key() + "\x00fixture-source", Load: func(context.Context, *catalog.Cache) (metadata.Detail, error) {
		return metadata.Detail{Object: req.Object, Text: text, Provenance: "fixture"}, nil
	}}, nil
}
func (*neovimFixtureProvider) Close() error { return nil }

func (p *neovimFixtureProvider) factory(context.Context) (metadata.Provider, error) {
	return p, nil
}

func serveNeovimFixture(t *testing.T) {
	// testing emits its final PASS line to os.Stdout. Keep the original file
	// handle solely for JSON-RPC and redirect the test harness/log output.
	protocolOut := os.Stdout
	os.Stdout = os.Stderr
	log.SetOutput(os.Stderr)
	backend := installStubBackend(t)
	_ = backend
	registerNeovimFixtureDriver()
	server := NewServer()
	server.openConnection = database.OpenContext
	server.stateMu.Lock()
	server.metadataFactoryOverride = func(cfg *database.DBConfig) (metadata.Key, metadata.Scope, metadata.Factory, bool) {
		variant := "old"
		if strings.HasSuffix(cfg.DataSourceName, "/new") {
			variant = "new"
		}
		provider := &neovimFixtureProvider{variant: variant}
		return metadata.Key{Engine: "interbase", URL: cfg.DataSourceName}, metadata.Scope{Principal: "fixture"}, provider.factory, true
	}
	server.stateMu.Unlock()
	handler := NewDispatcher(jsonrpc2.HandlerWithError(server.Handle))
	stream := jsonrpc2.NewBufferedStream(fixtureStdio{Reader: os.Stdin, Writer: protocolOut}, jsonrpc2.VSCodeObjectCodec{})
	conn := jsonrpc2.NewConn(context.Background(), stream, handler, jsonrpc2.SetLogger(log.New(os.Stderr, "sqls fixture: ", 0)))
	<-conn.DisconnectNotify()
	_ = server.Stop()
	<-server.cleanupDone
}

const neovimFixtureDriver dialect.DatabaseDriver = "sqls-neovim-fixture"

var fixtureDriverOnce sync.Once

func registerNeovimFixtureDriver() {
	fixtureDriverOnce.Do(func() {
		database.RegisterOpen(neovimFixtureDriver, func(*database.DBConfig) (*database.DBConnection, error) {
			db, err := sql.Open("sqls-handler-stub", "")
			if err != nil {
				return nil, err
			}
			return &database.DBConnection{Conn: db, Driver: dialect.DatabaseDriverInterBase}, nil
		})
		database.RegisterFactory(neovimFixtureDriver, func(db *sql.DB) database.DBRepository {
			backend := activeStubBackend()
			if backend == nil {
				backend = &stubBackend{gates: map[string]*stubGate{}}
			}
			return &neovimFixtureRepository{stubRepository: &stubRepository{MockDBRepository: database.NewMockDBRepository(db).(*database.MockDBRepository), backend: backend, db: db}}
		})
	})
}

type fixtureStdio struct {
	io.Reader
	io.Writer
}

func (fixtureStdio) Close() error { return nil }

type neovimFixtureRepository struct{ *stubRepository }

func (r *neovimFixtureRepository) Query(ctx context.Context, query string) (*sql.Rows, error) {
	if strings.Contains(query, "SQLS_FIXTURE_WAIT") {
		r.backend.recordQuery(r.db, query)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.stubRepository.Query(ctx, query)
}

func (*neovimFixtureRepository) ExplainPlan(context.Context, string) (string, error) {
	return "PLAN (CUSTOMERS NATURAL)", nil
}
