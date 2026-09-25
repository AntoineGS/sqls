package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
)

type documentSymbolResult struct {
	Name           string    `json:"name"`
	Kind           int       `json:"kind"`
	Range          lsp.Range `json:"range"`
	SelectionRange lsp.Range `json:"selectionRange"`
}

func TestDocumentSymbolJSONRPCUsesSharedProcedureSymbols(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	configureInterBaseTestServer(t, tx, dialect.SQLVariantInterBase1)

	uri := "file:///symbols.sql"
	text := "CREATE PROCEDURE P (\r\n  A INTEGER\r\n) RETURNS (R INTEGER) AS\r\nDECLARE VARIABLE /*😀*/ X INTEGER;\r\nBEGIN\r\n  X = A;\r\nEND"
	openDocument(t, tx, uri, text)

	var got []documentSymbolResult
	if err := tx.conn.Call(tx.ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]string{"uri": uri},
	}, &got); err != nil {
		t.Fatal("documentSymbol:", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d symbols, want input/output parameters and local: %#v", len(got), got)
	}
	for i, want := range []struct {
		name string
		line int
	}{
		{name: "A", line: 1},
		{name: "R", line: 2},
		{name: "X", line: 3},
	} {
		if got[i].Name != want.name || got[i].Kind != 13 {
			t.Errorf("symbol[%d] = (%q, kind %d), want (%q, kind 13)", i, got[i].Name, got[i].Kind, want.name)
		}
		if got[i].SelectionRange.Start.Line != want.line {
			t.Errorf("symbol[%d] selection starts on line %d, want %d", i, got[i].SelectionRange.Start.Line, want.line)
		}
	}
	// UTF-16 columns count the supplementary character as two units; CRLF
	// contributes one line break.
	if got[2].SelectionRange.Start.Character != 24 {
		t.Errorf("local selection column = %d, want 24 UTF-16 units", got[2].SelectionRange.Start.Character)
	}
	wantStarts := []lsp.Position{{Line: 1, Character: 2}, {Line: 2, Character: 11}, {Line: 3, Character: 24}}
	for i, want := range wantStarts {
		if got[i].SelectionRange.Start != want || got[i].SelectionRange.End != (lsp.Position{Line: want.Line, Character: want.Character + 1}) {
			t.Errorf("symbol[%d] selection = %+v, want [%+v,%+v]", i, got[i].SelectionRange, want, lsp.Position{Line: want.Line, Character: want.Character + 1})
		}
		if got[i].Range != (lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 6, Character: 3}}) {
			t.Errorf("symbol[%d] scope = %+v, want full procedure range", i, got[i].Range)
		}
	}
}

func TestDocumentSymbolJSONRPCPlainSelectReturnsEmptyArray(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	tx.server.stateMu.Lock()
	tx.server.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverPostgreSQL}
	tx.server.curDBCfg = &database.DBConfig{Driver: dialect.DatabaseDriverPostgreSQL}
	tx.server.connectionState = connectionReady
	tx.server.stateMu.Unlock()
	uri := "file:///select.sql"
	openDocument(t, tx, uri, "SELECT 1")
	var got []documentSymbolResult
	if err := tx.conn.Call(tx.ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]string{"uri": uri},
	}, &got); err != nil {
		t.Fatal("documentSymbol:", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("plain SELECT symbols = %#v, want non-nil empty array", got)
	}
}

func TestDocumentSymbolMalformedParamsAreRejected(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	bad := json.RawMessage(`{"textDocument":`)
	err := tx.conn.Call(tx.ctx, "textDocument/documentSymbol", &bad, nil)
	if err == nil {
		t.Fatal("malformed documentSymbol params unexpectedly succeeded")
	}
}

func TestDocumentSymbolInitializeCapabilityIsNarrow(t *testing.T) {
	tx := newTestContext()
	tx.initServer(t)
	defer tx.tearDown()
	var got map[string]any
	if err := tx.conn.Call(tx.ctx, "initialize", lsp.InitializeParams{}, &got); err != nil {
		t.Fatal("initialize:", err)
	}
	capabilities := got["capabilities"].(map[string]any)
	if capabilities["documentSymbolProvider"] != true {
		t.Errorf("documentSymbolProvider = %v, want true", capabilities["documentSymbolProvider"])
	}
	for _, key := range []string{"workspaceSymbolProvider", "foldingRangeProvider"} {
		if value, ok := capabilities[key]; ok && value == true {
			t.Errorf("unexpected broad outline capability %s=true", key)
		}
	}
}

func TestDocumentSymbolCancellationAndStaleFencing(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	uri := "file:///stale-symbols.sql"
	text := "CREATE PROCEDURE P AS BEGIN END"
	openDocument(t, tx, uri, text)

	ctx, cancel := context.WithCancel(tx.ctx)
	cancel()
	request := json.RawMessage(`{"textDocument":{"uri":"file:///stale-symbols.sql"}}`)
	_, err := tx.server.handleTextDocumentDocumentSymbol(ctx, nil, &jsonrpc2.Request{Params: (*json.RawMessage)(&request)})
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Errorf("canceled documentSymbol error = %v, want cancellation", err)
	}

	captured, err := tx.server.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal("capture snapshot:", err)
	}
	requestSnapshot := languageRequest(captured)
	symbolResult, err := language.NewService().Symbols(tx.ctx, requestSnapshot)
	if err != nil {
		t.Fatal("compute symbols:", err)
	}
	// A save may change the binding while preserving both URI and LSP version.
	if _, err := tx.server.updateFileAtRevision(uri, text+"\n-- saved", captured.Revision); err != nil {
		t.Fatal("update saved text:", err)
	}
	live, err := tx.server.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal("capture saved snapshot:", err)
	}
	if live.Version != captured.Version || live.Revision == captured.Revision {
		t.Fatalf("save changed version/revision to %d/%d, captured %d/%d", live.Version, live.Revision, captured.Version, captured.Revision)
	}
	if err := tx.server.navigationResultCurrent(tx.ctx, captured, symbolResult.Stamp); err == nil {
		t.Fatal("stale symbol snapshot passed navigation fence")
	}
}

func openDocument(t *testing.T, tx *TestContext, uri, text string) {
	t.Helper()
	if err := tx.conn.Call(tx.ctx, "textDocument/didOpen", lsp.DidOpenTextDocumentParams{
		TextDocument: lsp.TextDocumentItem{URI: uri, LanguageID: "sql", Version: 1, Text: text},
	}, nil); err != nil {
		t.Fatal("didOpen:", err)
	}
}
