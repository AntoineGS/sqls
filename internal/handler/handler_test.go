package handler

import (
	"context"
	"errors"
	"log"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

const testFileURI = "file:///Users/octref/Code/css-test/test.sql"

type TestContext struct {
	h          jsonrpc2.Handler
	conn       *jsonrpc2.Conn
	connServer *jsonrpc2.Conn
	server     *Server
	ctx        context.Context
}

// loadMetadataForTest installs a loader generation just like an attachment
// transition, then waits for deterministic settlement rather than sleeping.
func loadMetadataForTest(t *testing.T, s *Server, repo database.DBRepository) {
	t.Helper()
	s.diagnosticsPublishMu.Lock()
	s.stateMu.Lock()
	s.connGeneration++
	generation := s.connGeneration
	s.metadata.Reset(uint64(generation))
	s.stateMu.Unlock()
	s.diagnosticsPublishMu.Unlock()
	load, err := s.metadata.Start(context.Background(), uint64(generation), repo)
	if err != nil {
		t.Fatalf("start metadata: %v", err)
	}
	select {
	case <-load.Done:
	case <-time.After(10 * time.Second):
		t.Fatal("metadata loader watchdog expired")
	}
	s.republishOpenDiagnostics(context.Background())
}

func newTestContext() *TestContext {
	server := NewServer()
	handler := NewDispatcher(jsonrpc2.HandlerWithError(server.Handle))
	ctx := context.Background()
	return &TestContext{
		h:      handler,
		ctx:    ctx,
		server: server,
	}
}

func (tx *TestContext) setup(t *testing.T) {
	t.Helper()
	tx.initServer(t)
}

func (tx *TestContext) tearDown() {
	if tx.conn != nil {
		if err := tx.conn.Close(); err != nil {
			log.Fatal("conn.Close:", err)
		}
	}

	if tx.connServer != nil {
		if err := tx.connServer.Close(); err != nil {
			if !errors.Is(err, jsonrpc2.ErrClosed) {
				log.Fatal("connServer.Close:", err)
			}
		}
	}
}

func (tx *TestContext) initServer(t *testing.T) {
	t.Helper()

	// Prepare the server and client connection.
	client, server := net.Pipe()
	tx.connServer = jsonrpc2.NewConn(tx.ctx, jsonrpc2.NewBufferedStream(server, jsonrpc2.VSCodeObjectCodec{}), tx.h)
	tx.conn = jsonrpc2.NewConn(tx.ctx, jsonrpc2.NewBufferedStream(client, jsonrpc2.VSCodeObjectCodec{}), tx.h)

	// Initialize Language Server
	params := lsp.InitializeParams{
		InitializationOptions: lsp.InitializeOptions{},
	}
	if err := tx.conn.Call(tx.ctx, "initialize", params, nil); err != nil {
		t.Fatal("conn.Call initialize:", err)
	}
	if err := tx.conn.Notify(tx.ctx, "initialized", struct{}{}); err != nil {
		t.Fatal("conn.Notify initialized:", err)
	}
}

func (tx *TestContext) addWorkspaceConfig(t *testing.T, cfg *config.Config) {
	t.Helper()
	tx.changeWorkspaceConfig(t, cfg)
	// The LSP notification itself must return without waiting for attachment.
	// Tests which subsequently exercise DB-backed behavior explicitly select
	// and await a ready attachment here rather than racing the coordinator.
	connection, index, dbName := tx.server.desiredConnection()
	if connection == nil {
		return
	}
	select {
	case err := <-tx.server.coordinator.RequestExplicit(tx.ctx, connection, index, dbName):
		if err != nil {
			t.Fatalf("attach workspace test config: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for workspace test attachment")
	}
	ctx, cancel := context.WithTimeout(tx.ctx, 5*time.Second)
	defer cancel()
	if err := tx.server.metadata.Wait(ctx); err != nil {
		t.Fatalf("wait for metadata test readiness: %v", err)
	}
}

func (tx *TestContext) changeWorkspaceConfig(t *testing.T, cfg *config.Config) {
	t.Helper()
	didChangeConfigurationParams := lsp.DidChangeConfigurationParams{
		Settings: struct {
			SQLS *config.Config "json:\"sqls\""
		}{
			SQLS: cfg,
		},
	}
	if err := tx.conn.Call(tx.ctx, "workspace/didChangeConfiguration", didChangeConfigurationParams, nil); err != nil {
		t.Fatal("conn.Call workspace/didChangeConfiguration:", err)
	}
}

func (tx *TestContext) textDocumentDidOpen(t *testing.T, uri, input string) {
	didOpenParams := lsp.DidOpenTextDocumentParams{
		TextDocument: lsp.TextDocumentItem{
			URI:        testFileURI,
			LanguageID: "sql",
			Version:    0,
			Text:       input,
		},
	}
	if err := tx.conn.Call(tx.ctx, "textDocument/didOpen", didOpenParams, nil); err != nil {
		t.Fatal("conn.Call textDocument/didOpen:", err)
	}
	tx.testFile(t, didOpenParams.TextDocument.URI, didOpenParams.TextDocument.Text)
}

func TestInitialized(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()

	want := lsp.InitializeResult{
		Capabilities: lsp.ServerCapabilities{
			TextDocumentSync: lsp.TDSKFull,
			HoverProvider:    true,
			CompletionProvider: &lsp.CompletionOptions{
				TriggerCharacters: []string{"(", "."},
			},
			SignatureHelpProvider: &lsp.SignatureHelpOptions{
				TriggerCharacters:   []string{"(", ","},
				RetriggerCharacters: []string{"(", ","},
				WorkDoneProgressOptions: lsp.WorkDoneProgressOptions{
					WorkDoneProgress: false,
				},
			},
			CodeActionProvider:              true,
			DefinitionProvider:              true,
			ReferencesProvider:              true,
			DocumentFormattingProvider:      true,
			DocumentRangeFormattingProvider: true,
			RenameProvider:                  true,
			ExecuteCommandProvider: &lsp.ExecuteCommandOptions{Commands: []string{
				CommandExecuteQuery, CommandExplainQuery, CommandGetQueryParameters,
				CommandShowDatabases, CommandShowSchemas, CommandShowConnections,
				CommandSwitchDatabase, CommandSwitchConnection, CommandShowTables, CommandShowMetadataStatus,
			}},
		},
	}
	var got lsp.InitializeResult
	params := lsp.InitializeParams{
		InitializationOptions: lsp.InitializeOptions{},
	}
	if err := tx.conn.Call(tx.ctx, "initialize", params, &got); err != nil {
		t.Fatal("conn.Call initialize:", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("not match \n%+v\n%+v", want, got)
	}
}

func TestFileWatch(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()

	uri := "file:///Users/octref/Code/css-test/test.sql"
	openText := "SELECT * FROM todo ORDER BY id ASC"
	changeText := "SELECT * FROM todo ORDER BY name ASC"

	didOpenParams := lsp.DidOpenTextDocumentParams{
		TextDocument: lsp.TextDocumentItem{
			URI:        uri,
			LanguageID: "sql",
			Version:    0,
			Text:       openText,
		},
	}
	if err := tx.conn.Call(tx.ctx, "textDocument/didOpen", didOpenParams, nil); err != nil {
		t.Fatal("conn.Call textDocument/didOpen:", err)
	}
	tx.testFile(t, didOpenParams.TextDocument.URI, didOpenParams.TextDocument.Text)

	didChangeParams := lsp.DidChangeTextDocumentParams{
		TextDocument: lsp.VersionedTextDocumentIdentifier{
			URI:     uri,
			Version: 1,
		},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{
			lsp.TextDocumentContentChangeEvent{
				Range: lsp.Range{
					Start: lsp.Position{
						Line:      1,
						Character: 1,
					},
					End: lsp.Position{
						Line:      1,
						Character: 1,
					},
				},
				RangeLength: 1,
				Text:        changeText,
			},
		},
	}
	if err := tx.conn.Call(tx.ctx, "textDocument/didChange", didChangeParams, nil); err != nil {
		t.Fatal("conn.Call textDocument/didChange:", err)
	}
	tx.testFile(t, didChangeParams.TextDocument.URI, didChangeParams.ContentChanges[0].Text)

	didSaveParams := lsp.DidSaveTextDocumentParams{
		Text:         openText,
		TextDocument: lsp.TextDocumentIdentifier{URI: uri},
	}
	if err := tx.conn.Call(tx.ctx, "textDocument/didSave", didSaveParams, nil); err != nil {
		t.Fatal("conn.Call textDocument/didSave:", err)
	}
	tx.testFile(t, didSaveParams.TextDocument.URI, didSaveParams.Text)

	didCloseParams := lsp.DidCloseTextDocumentParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: uri},
	}
	if err := tx.conn.Call(tx.ctx, "textDocument/didClose", didCloseParams, nil); err != nil {
		t.Fatal("conn.Call textDocument/didClose:", err)
	}
	_, ok := tx.server.fileText(didCloseParams.TextDocument.URI)
	if ok {
		t.Errorf("found opened file. URI:%s", didCloseParams.TextDocument.URI)
	}
}

func (tx *TestContext) testFile(t *testing.T, uri, text string) {
	got, ok := tx.server.fileText(uri)
	if !ok {
		t.Errorf("not found opened file. URI:%s", uri)
	}
	if got != text {
		t.Errorf("not match %s. got: %s", text, got)
	}
}
