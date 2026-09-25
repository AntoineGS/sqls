package handler

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
)

func TestSymbolNavigationAcceptance(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	configureInterBaseTestServer(t, tx, dialect.SQLVariantInterBase1)

	uri := testFileURI
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE HEADEREMPLYID_TEMP INTEGER;
DECLARE VARIABLE ORDERTOTAL NUMERIC(9,2);
BEGIN
  HEADEREMPLYID_TEMP = 0;
  UPDATE CUSTOMERINVOICE SET AMOUNTPAID = :ORDERTOTAL
  WHERE HEADEREMPLYID = :HEADEREMPLYID_TEMP;
END`
	tx.textDocumentDidOpen(t, uri, text)

	callPosition := func(needle string, occurrence int) lsp.Position {
		t.Helper()
		start := -1
		for i := 0; i <= occurrence; i++ {
			next := strings.Index(text[start+1:], needle)
			if next < 0 {
				t.Fatalf("occurrence %d of %q not found", occurrence, needle)
			}
			start += next + 1
		}
		line := strings.Count(text[:start], "\n")
		lineStart := strings.LastIndex(text[:start], "\n") + 1
		return lsp.Position{Line: line, Character: start - lineStart}
	}
	locationAt := func(needle string, occurrence int) lsp.Location {
		start := callPosition(needle, occurrence)
		return lsp.Location{
			URI: uri,
			Range: lsp.Range{
				Start: start,
				End:   lsp.Position{Line: start.Line, Character: start.Character + len(needle)},
			},
		}
	}

	var definition lsp.Definition
	if err := tx.conn.Call(tx.ctx, "textDocument/definition", lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: uri},
		Position:     callPosition("HEADEREMPLYID_TEMP", 1),
	}, &definition); err != nil {
		t.Fatal("definition for local parameter:", err)
	}
	wantDefinition := lsp.Definition{locationAt("HEADEREMPLYID_TEMP", 0)}
	if diff := cmp.Diff(wantDefinition, definition); diff != "" {
		t.Fatalf("local definition mismatch (-want +got):\n%s", diff)
	}

	for _, includeDeclaration := range []bool{false, true} {
		var references []lsp.Location
		params := lsp.ReferenceParams{
			TextDocumentPositionParams: lsp.TextDocumentPositionParams{
				TextDocument: lsp.TextDocumentIdentifier{URI: uri},
				Position:     callPosition("HEADEREMPLYID_TEMP", 2),
			},
			Context: lsp.ReferenceContext{IncludeDeclaration: includeDeclaration},
		}
		if err := tx.conn.Call(tx.ctx, "textDocument/references", params, &references); err != nil {
			t.Fatal("references:", err)
		}
		wantReferences := []lsp.Location{
			locationAt("HEADEREMPLYID_TEMP", 1),
			locationAt("HEADEREMPLYID_TEMP", 2),
		}
		if includeDeclaration {
			wantReferences = append([]lsp.Location{locationAt("HEADEREMPLYID_TEMP", 0)}, wantReferences...)
		}
		if diff := cmp.Diff(wantReferences, references); diff != "" {
			t.Fatalf("references includeDeclaration=%v mismatch (-want +got):\n%s", includeDeclaration, diff)
		}
	}

	var orderDefinition lsp.Definition
	if err := tx.conn.Call(tx.ctx, "textDocument/definition", lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: uri},
		Position:     callPosition("ORDERTOTAL", 1),
	}, &orderDefinition); err != nil {
		t.Fatal("definition for ORDERTOTAL:", err)
	}
	wantOrderDefinition := lsp.Definition{locationAt("ORDERTOTAL", 0)}
	if diff := cmp.Diff(wantOrderDefinition, orderDefinition); diff != "" {
		t.Fatalf("ORDERTOTAL definition mismatch (-want +got):\n%s", diff)
	}

	var rename lsp.WorkspaceEdit
	if err := tx.conn.Call(tx.ctx, "textDocument/rename", lsp.RenameParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: uri},
		Position:     callPosition("HEADEREMPLYID_TEMP", 2),
		NewName:      "HEADER_EMPLOYEE_TEMP",
	}, &rename); err != nil {
		t.Fatal("rename:", err)
	}
	edits := rename.Changes[uri]
	if len(edits) != 3 {
		t.Fatalf("rename returned %d edits, want declaration and two uses: %#v", len(edits), edits)
	}
	// The edit array is ordered by source position; apply from right to left so
	// earlier byte offsets stay valid as replacement lengths change.
	updated := []byte(text)
	for i := len(edits) - 1; i >= 0; i-- {
		edit := edits[i]
		start, _ := language.ByteOffset(text, language.Position{Line: edit.Range.Start.Line, UTF16Column: edit.Range.Start.Character})
		end, _ := language.ByteOffset(text, language.Position{Line: edit.Range.End.Line, UTF16Column: edit.Range.End.Character})
		updated = append(updated[:start], append([]byte(edit.NewText), updated[end:]...)...)
	}
	newText := string(updated)
	if strings.Contains(newText, "HEADEREMPLYID_TEMP") || strings.Count(newText, "HEADER_EMPLOYEE_TEMP") != 3 {
		t.Fatalf("rename was not applied to all local occurrences:\n%s", newText)
	}
	if !strings.Contains(newText, "AMOUNTPAID = :ORDERTOTAL") {
		t.Fatalf("rename changed the UPDATE column or ORDERTOTAL local:\n%s", newText)
	}
	if err := tx.conn.Call(tx.ctx, "textDocument/didChange", lsp.DidChangeTextDocumentParams{
		TextDocument:   lsp.VersionedTextDocumentIdentifier{URI: uri, Version: 2},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: newText}},
	}, nil); err != nil {
		t.Fatal("didChange after applying rename:", err)
	}
	newPos := lsp.Position{Line: 4, Character: strings.Index(strings.Split(newText, "\n")[4], "HEADER_EMPLOYEE_TEMP")}
	var changedDefinition lsp.Definition
	if err := tx.conn.Call(tx.ctx, "textDocument/definition", lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: uri}, Position: newPos,
	}, &changedDefinition); err != nil {
		t.Fatal("definition after didChange:", err)
	}
	wantChangedDefinition := lsp.Definition{{
		URI: uri,
		Range: lsp.Range{
			Start: lsp.Position{Line: 1, Character: 17},
			End:   lsp.Position{Line: 1, Character: 17 + len("HEADER_EMPLOYEE_TEMP")},
		},
	}}
	if diff := cmp.Diff(wantChangedDefinition, changedDefinition); diff != "" {
		t.Fatalf("definition after didChange mismatch (-want +got):\n%s", diff)
	}
}
