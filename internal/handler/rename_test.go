package handler

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

var renameTestCases = []struct {
	name    string
	input   string
	newName string
	output  lsp.WorkspaceEdit
	pos     lsp.Position
}{
	{
		name:    "subquery",
		input:   "SELECT it.ID, it.Name FROM (SELECT ci.ID, ci.Name, ci.CountryCode, ci.District, ci.Population FROM city AS ci) as it",
		newName: "ct",
		output: lsp.WorkspaceEdit{
			DocumentChanges: []lsp.TextDocumentEdit{
				{
					TextDocument: lsp.OptionalVersionedTextDocumentIdentifier{
						Version: 0,
						TextDocumentIdentifier: lsp.TextDocumentIdentifier{
							URI: "file:///Users/octref/Code/css-test/test.sql",
						},
					},
					Edits: []lsp.TextEdit{
						{
							Range: lsp.Range{
								Start: lsp.Position{
									Line:      0,
									Character: 7,
								},
								End: lsp.Position{
									Line:      0,
									Character: 9,
								},
							},
							NewText: "ct",
						},
						{
							Range: lsp.Range{
								Start: lsp.Position{
									Line:      0,
									Character: 14,
								},
								End: lsp.Position{
									Line:      0,
									Character: 16,
								},
							},
							NewText: "ct",
						},
						{
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
							NewText: "ct",
						},
					},
				},
			},
		},
		pos: lsp.Position{
			Line:      0,
			Character: 8,
		},
	},
	{
		name:    "ok",
		input:   "SELECT ci.ID, ci.Name FROM city as ci",
		newName: "ct",
		output: lsp.WorkspaceEdit{
			DocumentChanges: []lsp.TextDocumentEdit{
				{
					TextDocument: lsp.OptionalVersionedTextDocumentIdentifier{
						Version: 0,
						TextDocumentIdentifier: lsp.TextDocumentIdentifier{
							URI: "file:///Users/octref/Code/css-test/test.sql",
						},
					},
					Edits: []lsp.TextEdit{
						{
							Range: lsp.Range{
								Start: lsp.Position{
									Line:      0,
									Character: 7,
								},
								End: lsp.Position{
									Line:      0,
									Character: 9,
								},
							},
							NewText: "ct",
						},
						{
							Range: lsp.Range{
								Start: lsp.Position{
									Line:      0,
									Character: 14,
								},
								End: lsp.Position{
									Line:      0,
									Character: 16,
								},
							},
							NewText: "ct",
						},
						{
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
							NewText: "ct",
						},
					},
				},
			},
		},
		pos: lsp.Position{
			Line:      0,
			Character: 8,
		},
	},
}

func TestRenameMain(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()

	cfg := &config.Config{
		Connections: []*database.DBConfig{
			{Driver: "mock"},
		},
	}
	tx.addWorkspaceConfig(t, cfg)

	for _, tt := range renameTestCases {
		t.Run(tt.name, func(t *testing.T) {
			tx.textDocumentDidOpen(t, testFileURI, tt.input)

			params := lsp.RenameParams{
				TextDocument: lsp.TextDocumentIdentifier{
					URI: testFileURI,
				},
				Position: tt.pos,
				NewName:  tt.newName,
			}
			var got lsp.WorkspaceEdit
			err := tx.conn.Call(tx.ctx, "textDocument/rename", params, &got)
			if err != nil {
				t.Errorf("conn.Call textDocument/rename: %+v", err)
				return
			}

			if diff := cmp.Diff(tt.output, got); diff != "" {
				t.Errorf("unmatch rename edits (- want, + got):\n%s", diff)
			}
		})
	}
}

func TestLocalRenameUsesProcedureSymbols(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE amountpaid INTEGER;
BEGIN
  amountpaid = 0;
  UPDATE customerinvoice SET amountpaid = :amountpaid;
END`
	params := lsp.RenameParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
		Position:     lsp.Position{Line: 3, Character: 3},
		NewName:      "paid",
	}
	got, handled, err := localRename(text, params, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	if !handled || got == nil {
		t.Fatalf("localRename = (%v, %v), want handled workspace edit", got, handled)
	}
	edits := got.Changes[testFileURI]
	if len(edits) != 3 {
		t.Fatalf("got %d edits, want declaration and two uses: %#v", len(edits), edits)
	}
	for _, edit := range edits {
		if edit.NewText != "paid" {
			t.Errorf("edit NewText = %q, want paid", edit.NewText)
		}
	}
	if edits[2].Range.Start.Character != 43 {
		t.Fatalf("colon-prefixed edit started at UTF-16 character %d, want identifier start", edits[2].Range.Start.Character)
	}
}

func TestLocalRenameRejectsSQLTargetAndFallsBackOutsideProcedure(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE amountpaid INTEGER;
BEGIN
  UPDATE customerinvoice SET amountpaid = :amountpaid;
END`
	column := lsp.RenameParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
		Position:     lsp.Position{Line: 3, Character: 35},
		NewName:      "paid",
	}
	if _, handled, err := localRename(text, column, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}); !handled || err == nil {
		t.Fatalf("SQL target localRename = handled %v, err %v; want handled error", handled, err)
	}

	outside := lsp.RenameParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
		Position:     lsp.Position{Line: 0, Character: 7},
		NewName:      "q",
	}
	if _, handled, err := localRename("SELECT value FROM table_name", outside, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}); err != nil || handled {
		t.Fatalf("outside localRename = handled %v, err %v; want fallback", handled, err)
	}
	if !strings.Contains(text, "amountpaid") {
		t.Fatal("test source unexpectedly changed")
	}
}

func TestLocalRenameAmbiguousTargetIsHandledError(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE duplicate INTEGER;
DECLARE VARIABLE duplicate INTEGER;
BEGIN
  duplicate = 1;
END`
	params := lsp.RenameParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
		Position:     lsp.Position{Line: 4, Character: 2},
		NewName:      "renamed",
	}
	got, handled, err := localRename(text, params, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if !handled || err == nil {
		t.Fatalf("localRename = (%#v, %v, %v), want handled ambiguity error", got, handled, err)
	}
	if got != nil {
		t.Fatalf("ambiguous local returned edits: %#v", got)
	}
}

func TestRenameJSONRPCUsesCurrentDocumentWithoutVersionZero(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	tx.server.stateMu.Lock()
	tx.server.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	tx.server.stateMu.Unlock()

	text := "ALTER PROCEDURE p AS\nDECLARE VARIABLE old_name INTEGER;\nBEGIN\n  old_name = 1;\nEND"
	tx.textDocumentDidOpen(t, testFileURI, text)
	changed := "ALTER PROCEDURE p AS\nDECLARE VARIABLE old_name INTEGER;\nBEGIN\n  old_name = old_name + 1;\nEND"
	if err := tx.conn.Call(tx.ctx, "textDocument/didChange", lsp.DidChangeTextDocumentParams{
		TextDocument:   lsp.VersionedTextDocumentIdentifier{URI: testFileURI, Version: 7},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: changed}},
	}, nil); err != nil {
		t.Fatal("didChange:", err)
	}

	var got lsp.WorkspaceEdit
	if err := tx.conn.Call(tx.ctx, "textDocument/rename", lsp.RenameParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
		Position:     lsp.Position{Line: 3, Character: 2},
		NewName:      "new_name",
	}, &got); err != nil {
		t.Fatal("rename:", err)
	}
	if len(got.Changes[testFileURI]) != 3 {
		t.Fatalf("got %d current-document edits, want 3", len(got.Changes[testFileURI]))
	}
	if len(got.DocumentChanges) != 0 {
		t.Fatalf("local rename unexpectedly returned versioned document changes: %#v", got.DocumentChanges)
	}
}
