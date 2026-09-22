package handler

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
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
		start, _ := symbolOffset(text, edit.Range.Start)
		end, _ := symbolOffset(text, edit.Range.End)
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

func TestSymbolOffsetUsesUTF16AndRejectsInvalidPositions(t *testing.T) {
	text := "😀  amount\r\nnext"
	nameStart := strings.Index(text, "amount")

	tests := []struct {
		name string
		pos  lsp.Position
		want int
		ok   bool
	}{
		{name: "before supplementary rune", pos: lsp.Position{Line: 0, Character: 0}, want: 0, ok: true},
		{name: "name after emoji", pos: lsp.Position{Line: 0, Character: 4}, want: nameStart, ok: true},
		{name: "next line after CRLF", pos: lsp.Position{Line: 1, Character: 0}, want: strings.Index(text, "next"), ok: true},
		{name: "surrogate midpoint", pos: lsp.Position{Line: 0, Character: 1}, ok: false},
		{name: "past line", pos: lsp.Position{Line: 0, Character: 11}, ok: false},
		{name: "missing line", pos: lsp.Position{Line: 2, Character: 0}, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := symbolOffset(text, tt.pos)
			if ok != tt.ok || (ok && got != tt.want) {
				t.Fatalf("symbolOffset(%+v) = (%d, %v), want (%d, %v)", tt.pos, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestSymbolRangeUsesOriginalUTF8Text(t *testing.T) {
	text := "😀 amount\r\namount"
	span := sqlsymbol.Span{Start: strings.Index(text, "amount"), End: strings.Index(text, "amount") + len("amount")}
	got, ok := symbolRange(text, span)
	if !ok {
		t.Fatal("symbolRange rejected a valid span")
	}
	want := lsp.Range{
		Start: lsp.Position{Line: 0, Character: 3},
		End:   lsp.Position{Line: 0, Character: 9},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("range mismatch (-want +got):\n%s", diff)
	}
	if _, ok := symbolRange(text, sqlsymbol.Span{Start: 1, End: 2}); ok {
		t.Fatal("symbolRange accepted a span inside an encoded rune")
	}
}

func TestSymbolOffsetAndRangeAgreeOnStandaloneCRLines(t *testing.T) {
	text := "first\rsecond"
	pos := lsp.Position{Line: 1, Character: 2}
	offset, ok := symbolOffset(text, pos)
	wantOffset := strings.Index(text, "second") + 2
	if !ok || offset != wantOffset {
		t.Fatalf("symbolOffset(%+v) = (%d, %v), want (%d, true)", pos, offset, ok, wantOffset)
	}
	rangeValue, ok := symbolRange(text, sqlsymbol.Span{Start: wantOffset, End: wantOffset + 1})
	if !ok || rangeValue.Start != pos || rangeValue.End != (lsp.Position{Line: 1, Character: 3}) {
		t.Fatalf("symbolRange = (%+v, %v), want line 1 characters 2..3", rangeValue, ok)
	}
}

func TestLocalDefinitionRoutesProceduralTargets(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE amount INTEGER;
BEGIN
amount = :amount;
END`
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}
	pos := lsp.Position{Line: 3, Character: 0}
	got, handled, err := localDefinition("file:///test.sql", text, pos, dv)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || len(got) != 1 {
		t.Fatalf("localDefinition = (%v, %v), want one handled location", got, handled)
	}
	want := lsp.Location{URI: "file:///test.sql", Range: lsp.Range{
		Start: lsp.Position{Line: 1, Character: 17},
		End:   lsp.Position{Line: 1, Character: 23},
	}}
	if diff := cmp.Diff([]lsp.Location{want}, got); diff != "" {
		t.Fatalf("definition mismatch (-want +got):\n%s", diff)
	}

	// SQL ambiguity is handled locally and must not be handed to a spelling-
	// based catalog or alias fallback.
	ambiguous := "ALTER PROCEDURE p AS\nDECLARE VARIABLE amount INTEGER;\nBEGIN\nSELECT amount FROM unrelated;\nEND"
	got, handled, err = localDefinition("file:///test.sql", ambiguous, lsp.Position{Line: 3, Character: 7}, dv)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || len(got) != 0 {
		t.Fatalf("ambiguous localDefinition = (%v, %v), want handled empty result", got, handled)
	}
}

func TestLocalDefinitionLeavesSQLRolesForCatalogRouting(t *testing.T) {
	text := "SELECT c.id FROM customer AS c"
	got, handled, err := localDefinition("file:///test.sql", text, lsp.Position{Line: 0, Character: 9}, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	if handled || got != nil {
		t.Fatalf("SQL localDefinition = (%v, %v), want unhandled", got, handled)
	}
}

func TestLocalDefinitionAcceptsUTF16PositionAfterEmoji(t *testing.T) {
	text := "ALTER PROCEDURE p AS\nDECLARE VARIABLE value INTEGER;\nBEGIN\n😀 value = 1;\nEND"
	got, handled, err := localDefinition("file:///test.sql", text, lsp.Position{Line: 3, Character: 3}, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	if !handled || len(got) != 1 {
		t.Fatalf("localDefinition after emoji = (%v, %v), want one handled location", got, handled)
	}
}
