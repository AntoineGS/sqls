package handler

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

func TestConnectionLifecycleRequestCopiesConfig(t *testing.T) {
	cfg := &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Params: map[string]string{"x": "before"}, InterBase: &database.InterBaseConfig{TLS: &database.InterBaseTLSConfig{Enabled: true}}}
	s := NewServer()
	s.openConnection = func(context.Context, *database.DBConfig) (*database.DBConnection, error) {
		return nil, context.Canceled
	}
	done := s.coordinator.Request(s.lifecycleCtx, cfg, 0, "")
	cfg.Params["x"] = "after"
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("coordinator did not complete request")
	}
	s.Stop()
}

func TestInitializeDoesNotWaitForConnectionOpen(t *testing.T) {
	s := NewServer()
	entered, release := make(chan struct{}), make(chan struct{})
	s.openConnection = func(ctx context.Context, cfg *database.DBConfig) (*database.DBConnection, error) {
		close(entered)
		select {
		case <-release:
			return nil, context.Canceled
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	clientPipe, serverPipe := net.Pipe()
	serverConn := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(serverPipe, jsonrpc2.VSCodeObjectCodec{}), jsonrpc2.HandlerWithError(s.Handle))
	clientConn := jsonrpc2.NewConn(context.Background(), jsonrpc2.NewBufferedStream(clientPipe, jsonrpc2.VSCodeObjectCodec{}), jsonrpc2.HandlerWithError(func(context.Context, *jsonrpc2.Conn, *jsonrpc2.Request) (interface{}, error) { return nil, nil }))
	defer clientConn.Close()
	defer serverConn.Close()
	defer close(release)
	defer s.Stop()
	cfg := &database.DBConfig{Driver: dialect.DatabaseDriverSQLite3, DataSourceName: ":memory:"}
	init := lsp.InitializeParams{InitializationOptions: lsp.InitializeOptions{ConnectionConfig: cfg}}
	callDone := make(chan error, 1)
	go func() { callDone <- clientConn.Call(context.Background(), "initialize", init, nil) }()
	select {
	case err := <-callDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("initialize waited for connection work")
	}
	select {
	case <-entered:
		t.Fatal("connection started before initialized")
	default:
	}
	if err := clientConn.Notify(context.Background(), "initialized", struct{}{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("bootstrap did not start")
	}
	if err := clientConn.Notify(context.Background(), "textDocument/didOpen", lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{URI: "file:///test.sql", LanguageID: "sql", Text: "select 1", Version: 1}}); err != nil {
		t.Fatal(err)
	}
	formatDone := make(chan error, 1)
	go func() {
		formatDone <- clientConn.Call(context.Background(), "textDocument/formatting", lsp.DocumentFormattingParams{TextDocument: lsp.TextDocumentIdentifier{URI: "file:///test.sql"}}, nil)
	}()
	select {
	case err := <-formatDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("formatting blocked behind connection open")
	}
}

func TestCloneConnectionConfigDeepCopiesNestedSettings(t *testing.T) {
	cfg := &database.DBConfig{Params: map[string]string{"a": "b"}, SSHCfg: &database.SSHConfig{Host: "host"}, InterBase: &database.InterBaseConfig{TLS: &database.InterBaseTLSConfig{Enabled: true}}}
	clone := cloneConnectionConfig(cfg)
	clone.Params["a"] = "c"
	clone.SSHCfg.Host = "other"
	clone.InterBase.TLS.Enabled = false
	if cfg.Params["a"] != "b" || cfg.SSHCfg.Host != "host" || !cfg.InterBase.TLS.Enabled {
		t.Fatal("clone shares mutable config state")
	}
}
