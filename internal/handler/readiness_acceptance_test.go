package handler

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

const (
	readinessOldDriver   dialect.DatabaseDriver = "readiness-acceptance-old"
	readinessNewDriver   dialect.DatabaseDriver = "readiness-acceptance-new"
	readinessQueryDriver dialect.DatabaseDriver = "readiness-acceptance-query"
)

var readinessRepositories sync.Map

func init() {
	for _, driver := range []dialect.DatabaseDriver{readinessOldDriver, readinessNewDriver, readinessQueryDriver} {
		driver := driver
		database.RegisterFactory(driver, func(*sql.DB) database.DBRepository {
			repo, _ := readinessRepositories.Load(driver)
			if repo == nil {
				return nil
			}
			return repo.(database.DBRepository)
		})
	}
}

type readinessPlanRepository struct {
	database.DBRepository
	plan database.MetadataPlan
}

func (r readinessPlanRepository) MetadataPlan() database.MetadataPlan { return r.plan }

func readinessRPC(t *testing.T, s *Server) *jsonrpc2.Conn {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	client := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(clientSide, jsonrpc2.VSCodeObjectCodec{}), jsonrpc2.HandlerWithError(func(context.Context, *jsonrpc2.Conn, *jsonrpc2.Request) (interface{}, error) { return nil, nil }))
	server := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(serverSide, jsonrpc2.VSCodeObjectCodec{}), NewDispatcher(jsonrpc2.HandlerWithError(s.Handle)))
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	if err := client.Call(context.Background(), "initialize", lsp.InitializeParams{}, nil); err != nil {
		t.Fatal(err)
	}
	return client
}

func readinessSeed(t testing.TB) *database.DBCache {
	t.Helper()
	cache, err := database.NewDBCacheUpdater(database.NewMockDBRepository(nil)).GenerateDBCachePrimary(context.Background())
	if err != nil {
		t.Fatalf("generate readiness fixture: %v", err)
	}
	return cache
}

func readinessSeedPlan(seed *database.DBCache, columnsEntered chan<- struct{}, columnsGate <-chan struct{}) database.MetadataPlan {
	return database.MetadataPlan{Parallelism: 3, Jobs: []database.MetadataJob{
		{Kind: database.MetadataSchemas, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{Cache: seed}, nil
		}},
		{Kind: database.MetadataRelations, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{Cache: seed}, nil
		}},
		{Kind: database.MetadataColumnsCurrent, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			close(columnsEntered)
			<-columnsGate
			return database.MetadataPatch{Cache: seed}, nil
		}},
	}}
}

// TestReadinessAcceptanceInitializeAndFormatWhileAttachBlocked exercises the
// actual JSON-RPC dispatch path. The two-second select is only a deadlock
// watchdog; the opener gate proves formatting returns before attach completes.
func TestReadinessAcceptanceInitializeAndFormatWhileAttachBlocked(t *testing.T) {
	s := NewServer()
	attachEntered, releaseOpener := make(chan struct{}), make(chan struct{})
	s.openConnection = func(context.Context, *database.DBConfig) (*database.DBConnection, error) {
		close(attachEntered)
		<-releaseOpener
		return nil, context.Canceled
	}
	clientSide, serverSide := net.Pipe()
	client := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(clientSide, jsonrpc2.VSCodeObjectCodec{}), jsonrpc2.HandlerWithError(func(context.Context, *jsonrpc2.Conn, *jsonrpc2.Request) (interface{}, error) { return nil, nil }))
	server := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(serverSide, jsonrpc2.VSCodeObjectCodec{}), NewDispatcher(jsonrpc2.HandlerWithError(s.Handle)))
	t.Cleanup(func() {
		close(releaseOpener)
		_ = client.Close()
		_ = server.Close()
		_ = s.Stop()
	})

	cfg := &database.DBConfig{Driver: dialect.DatabaseDriverSQLite3, DataSourceName: ":memory:"}
	initDone := make(chan error, 1)
	go func() {
		initDone <- client.Call(context.Background(), "initialize", lsp.InitializeParams{InitializationOptions: lsp.InitializeOptions{ConnectionConfig: cfg}}, nil)
	}()
	select {
	case err := <-initDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("initialize blocked behind attachment")
	}
	if err := client.Notify(context.Background(), "initialized", struct{}{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-attachEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("attachment did not enter opener")
	}
	if err := client.Notify(context.Background(), "textDocument/didOpen", lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{URI: "file:///readiness.sql", LanguageID: "sql", Text: "select 1", Version: 1}}); err != nil {
		t.Fatal(err)
	}
	formatReturned := make(chan error, 1)
	go func() {
		formatReturned <- client.Call(context.Background(), "textDocument/formatting", lsp.DocumentFormattingParams{TextDocument: lsp.TextDocumentIdentifier{URI: "file:///readiness.sql"}}, nil)
	}()
	select {
	case err := <-formatReturned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("formatting blocked behind attach")
	}
	select {
	case <-releaseOpener:
		t.Fatal("test accidentally released its blocking condition")
	default:
	}
}

// TestReadinessAcceptanceIndependentCatalogJobsRemainVisibleAfterFailures
// drives the real command over JSON-RPC while independent extended metadata
// succeeds despite failures in views and primary-key metadata.
func TestReadinessAcceptanceIndependentCatalogJobsRemainVisibleAfterFailures(t *testing.T) {
	s := NewServer()
	client := readinessRPC(t, s)
	t.Cleanup(func() { _ = s.Stop() })
	primaryEntered, failRelease := make(chan struct{}), make(chan struct{})
	var failOnce sync.Once
	releaseFailure := func() { failOnce.Do(func() { close(failRelease) }) }
	t.Cleanup(releaseFailure)
	plan := database.MetadataPlan{Parallelism: 3, Jobs: []database.MetadataJob{
		{Kind: database.MetadataViews, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			panic("views panic")
		}},
		{Kind: database.MetadataPrimaryKeys, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			close(primaryEntered)
			<-failRelease
			return database.MetadataPatch{}, errors.New("primary key catalog unavailable")
		}},
		{Kind: database.MetadataProcedures, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{Cache: &database.DBCache{Catalog: &database.CatalogCache{Procedures: map[string]*database.ProcedureDesc{"READYPROC": {Name: "READYPROC"}}}}}, nil
		}},
	}}
	readinessRepositories.Store(readinessOldDriver, readinessPlanRepository{DBRepository: &database.MockDBRepository{}, plan: plan})
	readinessRepositories.Store(readinessNewDriver, readinessPlanRepository{DBRepository: &database.MockDBRepository{}, plan: database.MetadataPlan{Parallelism: 1, Jobs: []database.MetadataJob{
		{Kind: database.MetadataViews, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{Cache: &database.DBCache{Catalog: &database.CatalogCache{Views: map[string]*database.ViewDesc{}}}}, nil
		}},
		{Kind: database.MetadataPrimaryKeys, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{Cache: &database.DBCache{PrimaryKeyColumns: map[string]map[string]struct{}{}}}, nil
		}},
		{Kind: database.MetadataProcedures, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{Cache: &database.DBCache{Catalog: &database.CatalogCache{Procedures: map[string]*database.ProcedureDesc{"RESETPROC": {Name: "RESETPROC"}}}}}, nil
		}},
	}}})
	t.Cleanup(func() {
		readinessRepositories.Delete(readinessOldDriver)
		readinessRepositories.Delete(readinessNewDriver)
	})
	s.WSCfg = &config.Config{Connections: []*database.DBConfig{{Driver: readinessOldDriver}, {Driver: readinessNewDriver}}}
	s.openConnection = func(_ context.Context, cfg *database.DBConfig) (*database.DBConnection, error) {
		return &database.DBConnection{Driver: cfg.Driver}, nil
	}
	switchCall := func(index string) error {
		return client.Call(context.Background(), "workspace/executeCommand", lsp.ExecuteCommandParams{Command: CommandSwitchConnection, Arguments: []interface{}{index}}, nil)
	}
	if err := switchCall("1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-primaryEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("primary metadata did not remain gated in loading state")
	}
	var status lsp.MetadataStatusResult
	if err := client.Call(context.Background(), "workspace/executeCommand", lsp.ExecuteCommandParams{Command: CommandShowMetadataStatus}, &status); err != nil {
		t.Fatal(err)
	}
	if status.Settled {
		t.Fatalf("gated metadata unexpectedly settled: %+v", status)
	}
	loadingPrimary := false
	for _, category := range status.Categories {
		if category.Kind == string(database.MetadataPrimaryKeys) && category.State == string(database.MetadataLoading) {
			loadingPrimary = true
		}
	}
	if !loadingPrimary {
		t.Fatalf("status did not expose loading primary metadata: %+v", status.Categories)
	}
	releaseFailure()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := s.metadata.Wait(ctx); err != nil {
		cancel()
		t.Fatalf("initial degraded metadata did not settle: %v", err)
	}
	cancel()
	if _, ok := s.metadata.Cache().Procedure("READYPROC"); !ok {
		t.Fatal("successful procedure metadata was not published")
	}
	if err := client.Call(context.Background(), "workspace/executeCommand", lsp.ExecuteCommandParams{Command: CommandShowMetadataStatus}, &status); err != nil {
		t.Fatal(err)
	}
	states := make(map[string]string, len(status.Categories))
	for _, category := range status.Categories {
		states[category.Kind] = category.State
	}
	if states[string(database.MetadataViews)] != string(database.MetadataFailed) || states[string(database.MetadataPrimaryKeys)] != string(database.MetadataFailed) || states[string(database.MetadataProcedures)] != string(database.MetadataReady) {
		t.Fatalf("partial metadata status = %+v", status.Categories)
	}
	if !status.Settled || !status.Degraded {
		t.Fatalf("failed load status = %+v", status)
	}
	if err := switchCall("2"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	if err := s.metadata.Wait(ctx); err != nil {
		cancel()
		t.Fatalf("reset metadata did not settle: %v", err)
	}
	cancel()
	if err := client.Call(context.Background(), "workspace/executeCommand", lsp.ExecuteCommandParams{Command: CommandShowMetadataStatus}, &status); err != nil {
		t.Fatal(err)
	}
	if !status.Settled || status.Degraded || status.Generation != uint64(s.connGeneration) {
		t.Fatalf("status after successful reset = %+v", status)
	}
	if _, ok := s.metadata.Cache().Procedure("RESETPROC"); !ok {
		t.Fatal("successful reset procedure was not published")
	}
}

func TestReadinessAcceptanceCompletionsWhileColumnsAreGated(t *testing.T) {
	s := NewServer()
	client := readinessRPC(t, s)
	t.Cleanup(func() { _ = s.Stop() })
	s.stateMu.Lock()
	s.connGeneration, s.connectionState = 52, connectionReady
	s.stateMu.Unlock()
	s.metadata.Reset(52)
	entered, release := make(chan struct{}), make(chan struct{})
	load, err := s.metadata.Start(context.Background(), 52, readinessPlanRepository{DBRepository: &database.MockDBRepository{}, plan: readinessSeedPlan(readinessSeed(t), entered, release)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(release); <-load.Done }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("columns job did not enter gate")
	}

	queries := []struct {
		text string
		want string
	}{{"sel", "SELECT"}, {"select * from ", "city"}}
	for _, query := range queries {
		uri := "file:///readiness-" + query.want + ".sql"
		if err := client.Notify(context.Background(), "textDocument/didOpen", lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{URI: uri, LanguageID: "sql", Text: query.text, Version: 1}}); err != nil {
			t.Fatal(err)
		}
		params := lsp.CompletionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{TextDocument: lsp.TextDocumentIdentifier{URI: uri}, Position: lsp.Position{Line: 0, Character: len(query.text)}}}
		var result lsp.CompletionList
		if err := client.Call(context.Background(), "textDocument/completion", params, &result); err != nil {
			t.Fatal(err)
		}
		if !result.IsIncomplete {
			t.Fatalf("completion for %q should be incomplete during column load", query.text)
		}
		found := false
		for _, item := range result.Items {
			if item.Label == query.want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("completion for %q omitted %q: %#v", query.text, query.want, result.Items)
		}
	}
	select {
	case <-release:
		t.Fatal("columns gate released before liveness assertions finished")
	default:
	}
}

func TestReadinessAcceptanceRPCSwitchDrainsSlowOldMetadata(t *testing.T) {
	s := NewServer()
	client := readinessRPC(t, s)
	t.Cleanup(func() { _ = s.Stop() })
	oldGate, oldEntered := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseOld := func() { releaseOnce.Do(func() { close(oldGate) }) }
	t.Cleanup(releaseOld)
	var active, peak atomic.Int32
	job := func(name string, entered chan struct{}, gate <-chan struct{}) database.MetadataJob {
		return database.MetadataJob{Kind: database.MetadataProcedures, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			n := active.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			defer active.Add(-1)
			if entered != nil {
				close(entered)
			}
			if gate != nil {
				<-gate
			}
			return database.MetadataPatch{Cache: &database.DBCache{Catalog: &database.CatalogCache{Procedures: map[string]*database.ProcedureDesc{name: {Name: name}}}}}, nil
		}}
	}
	readinessRepositories.Store(readinessOldDriver, readinessPlanRepository{DBRepository: &database.MockDBRepository{}, plan: database.MetadataPlan{Parallelism: 1, Jobs: []database.MetadataJob{job("OLDPROC", oldEntered, oldGate)}}})
	readinessRepositories.Store(readinessNewDriver, readinessPlanRepository{DBRepository: &database.MockDBRepository{}, plan: database.MetadataPlan{Parallelism: 1, Jobs: []database.MetadataJob{job("NEWPROC", nil, nil)}}})
	t.Cleanup(func() {
		readinessRepositories.Delete(readinessOldDriver)
		readinessRepositories.Delete(readinessNewDriver)
	})
	s.WSCfg = &config.Config{Connections: []*database.DBConfig{{Driver: readinessOldDriver}, {Driver: readinessNewDriver}}}
	s.openConnection = func(_ context.Context, cfg *database.DBConfig) (*database.DBConnection, error) {
		return &database.DBConnection{Driver: cfg.Driver}, nil
	}
	switchCall := func(index string) error {
		return client.Call(context.Background(), "workspace/executeCommand", lsp.ExecuteCommandParams{Command: CommandSwitchConnection, Arguments: []interface{}{index}}, nil)
	}
	if err := switchCall("1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-oldEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("old generation did not enter gated procedure query")
	}
	oldCache := s.metadata.Cache()
	s.diagnosticCatalogFor(oldCache)
	if err := switchCall("2"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for s.metadata.Snapshot().Status[database.MetadataProcedures].State != database.MetadataReady {
		select {
		case <-deadline:
			t.Fatal("replacement metadata did not publish while old call remained gated")
		default:
		}
		runtime.Gosched()
	}
	if active.Load() != 1 {
		t.Fatalf("active queries = %d; old native query should remain held until gate release", active.Load())
	}
	var status lsp.MetadataStatusResult
	if err := client.Call(context.Background(), "workspace/executeCommand", lsp.ExecuteCommandParams{Command: CommandShowMetadataStatus}, &status); err != nil {
		t.Fatal(err)
	}
	procedureReady := false
	for _, category := range status.Categories {
		if category.Kind == string(database.MetadataProcedures) && category.State == string(database.MetadataReady) {
			procedureReady = true
		}
	}
	if !procedureReady || status.Generation != uint64(s.connGeneration) {
		t.Fatalf("status after RPC switch = %+v; generation=%d", status, s.connGeneration)
	}
	if _, ok := s.metadata.Cache().Procedure("NEWPROC"); !ok {
		t.Fatal("replacement procedure output missing")
	}
	if _, ok := s.metadata.Cache().Procedure("OLDPROC"); ok {
		t.Fatal("stale old-generation procedure leaked into replacement cache")
	}
	currentCache := s.metadata.Cache()
	s.diagnosticCatalogFor(currentCache)
	s.diagnosticCatalogMu.Lock()
	if s.diagnosticCache != currentCache || s.diagnosticCache == oldCache || s.derivedCatalog == nil {
		s.diagnosticCatalogMu.Unlock()
		t.Fatal("diagnostic memo retained a retired-generation cache")
	}
	s.diagnosticCatalogMu.Unlock()
	releaseOld()
	if err := s.metadata.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got > 3 {
		t.Fatalf("peak active jobs = %d, want <=3", got)
	}
}

func TestReadinessAcceptanceStopWithPendingRPCQueryAndNotification(t *testing.T) {
	s := NewServer()
	client := readinessRPC(t, s)
	t.Cleanup(func() { _ = s.Stop() })
	queryEntered, releaseQuery := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	deferRelease := func() { releaseOnce.Do(func() { close(releaseQuery) }) }
	t.Cleanup(deferRelease)
	repo := database.NewMockDBRepository(nil).(*database.MockDBRepository)
	repo.MockQuery = func(context.Context, string) (*sql.Rows, error) {
		close(queryEntered)
		<-releaseQuery
		return &sql.Rows{}, nil
	}
	readinessRepositories.Store(readinessQueryDriver, repo)
	t.Cleanup(func() { readinessRepositories.Delete(readinessQueryDriver) })
	s.WSCfg = &config.Config{Connections: []*database.DBConfig{{Driver: readinessQueryDriver}}}
	s.openConnection = func(_ context.Context, cfg *database.DBConfig) (*database.DBConnection, error) {
		return &database.DBConnection{Driver: cfg.Driver}, nil
	}
	if err := client.Call(context.Background(), "workspace/executeCommand", lsp.ExecuteCommandParams{Command: CommandSwitchConnection, Arguments: []interface{}{"1"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.metadata.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	uri := "file:///pending-query.sql"
	if err := client.Notify(context.Background(), "textDocument/didOpen", lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{URI: uri, LanguageID: "sql", Text: "select 1", Version: 1}}); err != nil {
		t.Fatal(err)
	}
	queryDone := make(chan error, 1)
	go func() {
		queryDone <- client.Call(context.Background(), "workspace/executeCommand", lsp.ExecuteCommandParams{Command: CommandExecuteQuery, Arguments: []interface{}{uri}}, nil)
	}()
	select {
	case <-queryEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("RPC query did not enter blocking repository call")
	}
	if err := client.Notify(context.Background(), "textDocument/didChange", lsp.DidChangeTextDocumentParams{TextDocument: lsp.VersionedTextDocumentIdentifier{URI: uri, Version: 2}, ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: "select 2"}}}); err != nil {
		t.Fatalf("didChange notification while query blocked: %v", err)
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- s.Stop() }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked behind pending RPC query")
	}
	var status lsp.MetadataStatusResult
	if err := client.Call(context.Background(), "workspace/executeCommand", lsp.ExecuteCommandParams{Command: CommandShowMetadataStatus}, &status); err != nil {
		t.Fatal(err)
	}
	if status.ConnectionState != string(connectionStopped) {
		t.Fatalf("status after Stop = %+v", status)
	}
	deferRelease()
	select {
	case <-queryDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pending RPC query did not finish after driver release")
	}
}
