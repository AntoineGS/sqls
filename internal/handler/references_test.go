package handler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

func TestLocalReferencesAcrossStatements(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE v INTEGER;
BEGIN
v = 0;
v = :v + 1;
-- v
SELECT 'v' FROM t;
END`
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}
	position := lsp.ReferenceParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: "file:///test.sql"},
		Position:     lsp.Position{Line: 3, Character: 0},
	}}
	got, err := localReferences(position.TextDocument.URI, text, position, dv)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d references, want declaration excluded plus two uses: %v", len(got), got)
	}
	want := []lsp.Location{
		{URI: position.TextDocument.URI, Range: lsp.Range{Start: lsp.Position{Line: 3, Character: 0}, End: lsp.Position{Line: 3, Character: 1}}},
		{URI: position.TextDocument.URI, Range: lsp.Range{Start: lsp.Position{Line: 4, Character: 0}, End: lsp.Position{Line: 4, Character: 1}}},
		{URI: position.TextDocument.URI, Range: lsp.Range{Start: lsp.Position{Line: 4, Character: 5}, End: lsp.Position{Line: 4, Character: 6}}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("references mismatch (-want +got):\n%s", diff)
	}

	position.Context.IncludeDeclaration = true
	got, err = localReferences(position.TextDocument.URI, text, position, dv)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d references with declaration, want 4", len(got))
	}
}

func TestReferencesUseConfiguredEditorSnapshotBeforeAttachment(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	s.WSCfg = &config.Config{Connections: []*database.DBConfig{{Driver: dialect.DatabaseDriverInterBase, Dialect: 1}}}
	text := "ALTER PROCEDURE p AS\nDECLARE VARIABLE v INTEGER;\nBEGIN\nv=0;\nv=:v+1;\nEND"
	if err := s.openFileAtVersion("file:///configured.sql", "sql", text, 1); err != nil {
		t.Fatal(err)
	}
	params := lsp.ReferenceParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: "file:///configured.sql"},
		Position:     lsp.Position{Line: 4, Character: 3},
	}}
	data, _ := json.Marshal(params)
	result, err := s.handleReferences(context.Background(), nil, &jsonrpc2.Request{Params: (*json.RawMessage)(&data)})
	if err != nil {
		t.Fatal(err)
	}
	locations, ok := result.([]lsp.Location)
	if !ok || len(locations) != 3 {
		t.Fatalf("configured InterBase references = %#v, want three local references", result)
	}
}

func TestLocalReferencesUnsupportedDriverAndAmbiguousTargetAreEmpty(t *testing.T) {
	params := lsp.ReferenceParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: "file:///test.sql"},
		Position:     lsp.Position{Line: 0, Character: 7},
	}}
	got, err := localReferences(params.TextDocument.URI, "SELECT value FROM t", params, dialect.DriverVariant{Driver: dialect.DatabaseDriverPostgreSQL})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("unsupported-driver references = %#v, want non-nil empty", got)
	}

	text := "ALTER PROCEDURE p AS\nDECLARE VARIABLE value INTEGER;\nBEGIN\nSELECT value FROM t;\nEND"
	params.Position = lsp.Position{Line: 3, Character: 7}
	got, err = localReferences(params.TextDocument.URI, text, params, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("ambiguous references = %#v, want non-nil empty", got)
	}
}

func TestReferencesProcedureAcrossStatements(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	tx.server.stateMu.Lock()
	tx.server.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	tx.server.stateMu.Unlock()
	text := "ALTER PROCEDURE p AS\nDECLARE VARIABLE v INTEGER;\nBEGIN\nv=0;\nv=:v+1;\nEND"
	tx.textDocumentDidOpen(t, testFileURI, text)
	for _, include := range []bool{false, true} {
		params := lsp.ReferenceParams{
			TextDocumentPositionParams: lsp.TextDocumentPositionParams{
				TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
				Position:     lsp.Position{Line: 4, Character: 3},
			},
			Context: lsp.ReferenceContext{IncludeDeclaration: include},
		}
		var got []lsp.Location
		if err := tx.conn.Call(tx.ctx, "textDocument/references", params, &got); err != nil {
			t.Fatal(err)
		}
		want := 3
		if include {
			want++
		}
		if len(got) != want {
			t.Fatalf("includeDeclaration=%v: got %d references, want %d", include, len(got), want)
		}
	}
}

func TestReferencesDispatcherReturnsEmptyForUnsupportedAndCommentTargets(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	text := "SELECT value FROM table_name"
	tx.textDocumentDidOpen(t, testFileURI, text)
	tx.server.stateMu.Lock()
	tx.server.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverPostgreSQL}
	tx.server.stateMu.Unlock()
	params := lsp.ReferenceParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
		Position:     lsp.Position{Line: 0, Character: 7},
	}}
	var got []lsp.Location
	if err := tx.conn.Call(tx.ctx, "textDocument/references", params, &got); err != nil {
		t.Fatal("unsupported driver references:", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("unsupported driver references = %#v, want empty result", got)
	}

	tx.server.stateMu.Lock()
	tx.server.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	tx.server.stateMu.Unlock()
	text = "ALTER PROCEDURE p AS\nDECLARE VARIABLE value INTEGER;\nBEGIN\n-- value\nEND"
	tx.textDocumentDidOpen(t, testFileURI, text)
	params.Position = lsp.Position{Line: 3, Character: 3}
	got = nil
	if err := tx.conn.Call(tx.ctx, "textDocument/references", params, &got); err != nil {
		t.Fatal("comment references:", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("comment references = %#v, want empty result", got)
	}
}
