package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

var definitionTestCases = []struct {
	name  string
	input string
	pos   lsp.Position
	want  lsp.Definition
}{
	{
		name:  "subquery",
		input: "SELECT it.ID, it.Name FROM (SELECT ci.ID, ci.Name, ci.CountryCode, ci.District, ci.Population FROM city AS ci) as it",
		pos: lsp.Position{
			Line:      0,
			Character: 8,
		},
		want: []lsp.Location{
			{
				URI: testFileURI,
				Range: lsp.Range{
					Start: lsp.Position{
						Line:      0,
						Character: 114,
					},
					End: lsp.Position{
						Line:      0,
						Character: 116,
					},
				},
			},
		},
	},
	{
		name:  "inner subquery",
		input: "SELECT it.ID, it.Name FROM (SELECT ci.ID, ci.Name, ci.CountryCode, ci.District, ci.Population FROM city AS ci) as it",
		pos: lsp.Position{
			Line:      0,
			Character: 36,
		},
		want: []lsp.Location{
			{
				URI: testFileURI,
				Range: lsp.Range{
					Start: lsp.Position{
						Line:      0,
						Character: 107,
					},
					End: lsp.Position{
						Line:      0,
						Character: 109,
					},
				},
			},
		},
	},
	{
		name:  "alias",
		input: "SELECT ci.ID, ci.Name FROM city as ci",
		pos: lsp.Position{
			Line:      0,
			Character: 8,
		},
		want: []lsp.Location{
			{
				URI: testFileURI,
				Range: lsp.Range{
					Start: lsp.Position{
						Line:      0,
						Character: 35,
					},
					End: lsp.Position{
						Line:      0,
						Character: 37,
					},
				},
			},
		},
	},
}

func TestDefinition(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()

	cfg := &config.Config{
		Connections: []*database.DBConfig{
			{Driver: "mock"},
		},
	}
	tx.addWorkspaceConfig(t, cfg)

	for _, tt := range definitionTestCases {
		t.Run(tt.name, func(t *testing.T) {
			tx.textDocumentDidOpen(t, testFileURI, tt.input)

			params := lsp.DefinitionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{
						URI: testFileURI,
					},
					Position: tt.pos,
				},
			}
			var got lsp.Definition
			err := tx.conn.Call(tx.ctx, "textDocument/definition", params, &got)
			if err != nil {
				t.Errorf("conn.Call textDocument/definition: %+v", err)
				return
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("unmatch hover contents (- want, + got):\n%s", diff)
			}
		})
	}
}

func TestTypeDefinition(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()

	cfg := &config.Config{
		Connections: []*database.DBConfig{
			{Driver: "mock"},
		},
	}
	tx.addWorkspaceConfig(t, cfg)

	for _, tt := range definitionTestCases {
		t.Run(tt.name, func(t *testing.T) {
			tx.textDocumentDidOpen(t, testFileURI, tt.input)

			params := lsp.DefinitionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{
						URI: testFileURI,
					},
					Position: tt.pos,
				},
			}
			var got lsp.Definition
			err := tx.conn.Call(tx.ctx, "textDocument/typeDefinition", params, &got)
			if err != nil {
				t.Errorf("conn.Call textDocument/definition: %+v", err)
				return
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("unmatch hover contents (- want, + got):\n%s", diff)
			}
		})
	}
}

func TestDefinitionProcedureLocalDoesNotNeedRepository(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	tx.server.stateMu.Lock()
	tx.server.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	tx.server.stateMu.Unlock()
	text := "ALTER PROCEDURE p AS\nDECLARE VARIABLE value INTEGER;\nBEGIN\nvalue = :value;\nEND"
	tx.textDocumentDidOpen(t, testFileURI, text)
	var got lsp.Definition
	for _, character := range []int{0, 9} { // bare assignment and prefixed read
		params := lsp.DefinitionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 3, Character: character},
		}}
		if err := tx.conn.Call(tx.ctx, "textDocument/definition", params, &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].URI != testFileURI {
			t.Fatalf("character %d: got local definition %#v, want one location in the open document", character, got)
		}
	}
}

func TestDefinitionInvalidInterBasePositionsDoNotUseLegacyFallback(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	tx.server.stateMu.Lock()
	tx.server.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	tx.server.stateMu.Unlock()
	text := "😀ci.ID FROM city AS ci"
	tx.textDocumentDidOpen(t, testFileURI, text)

	valid := lsp.DefinitionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
		Position:     lsp.Position{Line: 0, Character: 2},
	}}
	var got lsp.Definition
	if err := tx.conn.Call(tx.ctx, "textDocument/definition", valid, &got); err != nil {
		t.Fatal("valid alias request:", err)
	}
	if len(got) != 1 {
		t.Fatalf("valid alias request got %d locations, want one candidate", len(got))
	}

	for _, position := range []lsp.Position{
		{Line: 0, Character: 1},   // middle of the emoji surrogate pair
		{Line: 0, Character: 100}, // past the line
		{Line: 1, Character: 0},   // missing line
		{Line: -1, Character: 0},  // invalid line
	} {
		params := valid
		params.Position = position
		got = nil
		if err := tx.conn.Call(tx.ctx, "textDocument/definition", params, &got); err != nil {
			t.Fatalf("invalid position %+v: %v", position, err)
		}
		if len(got) != 0 {
			t.Errorf("invalid position %+v got legacy candidate %#v, want empty", position, got)
		}
	}
}

func TestDefinitionProtocolValidation(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	for _, method := range []string{"textDocument/definition", "textDocument/references"} {
		_, err := tx.server.handle(context.Background(), nil, &jsonrpc2.Request{Method: method})
		if err == nil {
			t.Errorf("%s without params returned nil error", method)
		} else if rpcErr, ok := err.(*jsonrpc2.Error); !ok || rpcErr.Code != jsonrpc2.CodeInvalidParams {
			t.Errorf("%s without params error = %T %v, want invalid params", method, err, err)
		}
	}

	raw := json.RawMessage(`{"textDocument":{"uri":"file:///missing.sql"}}`)
	_, err := tx.server.handle(context.Background(), nil, &jsonrpc2.Request{Method: "textDocument/definition", Params: &raw})
	if err == nil || !strings.Contains(err.Error(), "document not found") {
		t.Fatalf("missing definition document error = %v, want document not found", err)
	}
	raw = json.RawMessage(`{"textDocument":{"uri":"file:///missing.sql"}}`)
	_, err = tx.server.handle(context.Background(), nil, &jsonrpc2.Request{Method: "textDocument/references", Params: &raw})
	if err == nil || !strings.Contains(err.Error(), "document not found") {
		t.Fatalf("missing references document error = %v, want document not found", err)
	}
}

func TestAmbiguousLocalBeatsSameSpelledCatalogCandidate(t *testing.T) {
	text := "ALTER PROCEDURE p AS\nDECLARE VARIABLE MYPROC INTEGER;\nBEGIN\nSELECT MYPROC FROM t;\nEND"
	params := lsp.DefinitionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
		Position:     lsp.Position{Line: 3, Character: 7},
	}}
	if _, ok := resolveSnapshotTarget(text, params, definitionCatalog(), dialect.DatabaseDriverInterBase); !ok {
		t.Fatal("fixture did not provide the same-spelled catalog candidate")
	}
	got, handled, err := localDefinition(testFileURI, text, params.Position, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	if !handled || len(got) != 0 {
		t.Fatalf("ambiguous local definition = (%v, %v), want handled empty result", got, handled)
	}
}

func TestDefinitionDispatcherRoutesSQLRolesBySemantics(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	tx.server.stateMu.Lock()
	tx.server.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	tx.server.stateMu.Unlock()

	// Both the column and relation spellings have an unrelated alias with the
	// same name. Contextual SQL routing must prevent that alias from winning.
	text := "SELECT customer FROM other AS customer;\nSELECT id FROM customer;"
	tx.textDocumentDidOpen(t, testFileURI, text)
	for _, position := range []lsp.Position{
		{Line: 0, Character: len("SELECT ")},
		{Line: 1, Character: len("SELECT id FROM ")},
	} {
		params := lsp.DefinitionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     position,
		}}
		var got lsp.Definition
		if err := tx.conn.Call(tx.ctx, "textDocument/definition", params, &got); err != nil {
			t.Fatalf("SQL role at %+v: %v", position, err)
		}
		if len(got) != 0 {
			t.Fatalf("SQL role at %+v got unrelated alias %#v", position, got)
		}
	}

	for _, tc := range []struct {
		name string
		text string
		pos  lsp.Position
	}{
		{name: "alias", text: "SELECT c.id FROM city AS c", pos: lsp.Position{Line: 0, Character: 7}},
		{name: "callable", text: "SELECT f() FROM t AS f", pos: lsp.Position{Line: 0, Character: 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx.textDocumentDidOpen(t, testFileURI, tc.text)
			params := lsp.DefinitionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
				TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
				Position:     tc.pos,
			}}
			var got lsp.Definition
			if err := tx.conn.Call(tx.ctx, "textDocument/definition", params, &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].URI != testFileURI {
				t.Fatalf("got %#v, want in-document alias definition", got)
			}
		})
	}
}
