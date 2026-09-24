package handler

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

type readinessPlanRepository struct {
	database.DBRepository
	plan database.MetadataPlan
}

func (r readinessPlanRepository) MetadataPlan() database.MetadataPlan { return r.plan }

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
	s.stateMu.Lock()
	s.connGeneration = 41
	s.connectionState = connectionReady
	s.stateMu.Unlock()
	s.metadata.Reset(41)
	plan := database.MetadataPlan{Parallelism: 3, Jobs: []database.MetadataJob{
		{Kind: database.MetadataViews, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{}, errors.New("views unavailable")
		}},
		{Kind: database.MetadataPrimaryKeys, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{}, errors.New("primary key catalog unavailable")
		}},
		{Kind: database.MetadataProcedures, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{Cache: &database.DBCache{Catalog: &database.CatalogCache{Procedures: map[string]*database.ProcedureDesc{"READYPROC": {Name: "READYPROC"}}}}}, nil
		}},
	}}
	load, err := s.metadata.Start(context.Background(), 41, readinessPlanRepository{DBRepository: &database.MockDBRepository{}, plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	clientSide, serverSide := net.Pipe()
	client := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(clientSide, jsonrpc2.VSCodeObjectCodec{}), jsonrpc2.HandlerWithError(func(context.Context, *jsonrpc2.Conn, *jsonrpc2.Request) (interface{}, error) { return nil, nil }))
	server := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(serverSide, jsonrpc2.VSCodeObjectCodec{}), NewDispatcher(jsonrpc2.HandlerWithError(s.Handle)))
	t.Cleanup(func() { _ = client.Close(); _ = server.Close(); _ = s.Stop() })
	if err := client.Call(context.Background(), "initialize", lsp.InitializeParams{}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-load.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("metadata plan did not settle")
	}
	if _, ok := s.metadata.Cache().Procedure("READYPROC"); !ok {
		t.Fatal("successful procedure metadata was not published")
	}
	var status lsp.MetadataStatusResult
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
}
