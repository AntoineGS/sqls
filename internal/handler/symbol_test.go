package handler

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

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
