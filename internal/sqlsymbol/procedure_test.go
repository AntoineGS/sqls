package sqlsymbol

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/sqls-server/sqls/dialect"
)

func interBaseVariant() dialect.DriverVariant {
	return dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}
}

func symbolKeys(symbols []*Symbol) []string {
	keys := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		keys = append(keys, symbol.Name.Key())
	}
	return keys
}

func TestAnalyzeProcedureDeclarations(t *testing.T) {
	text := `ALTER PROCEDURE p (id INTEGER)
RETURNS (result NUMERIC(15,2)) AS
DECLARE VARIABLE amountpaid NUMERIC(15,2);
BEGIN
  amountpaid = 0;
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"ID", "RESULT", "AMOUNTPAID"}, symbolKeys(a.Symbols)); diff != "" {
		t.Fatal(diff)
	}
	for _, symbol := range a.Symbols {
		if symbol.Scope.Start != 0 || symbol.Scope.End != len(text) {
			t.Fatalf("wrong scope: %+v", symbol.Scope)
		}
	}
}

func TestAnalyzeProcedureDeclarationsKeepProceduresSeparate(t *testing.T) {
	text := `CREATE PROCEDURE first (id INTEGER) RETURNS (result INTEGER) AS
DECLARE VARIABLE local INTEGER;
BEGIN
END;
CREATE PROCEDURE second (id INTEGER) RETURNS (result INTEGER) AS
DECLARE VARIABLE local INTEGER;
BEGIN
END;`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(a.procedures), 2; got != want {
		t.Fatalf("procedure count = %d, want %d", got, want)
	}
	if got, want := symbolKeys(a.Symbols), []string{"ID", "RESULT", "LOCAL", "ID", "RESULT", "LOCAL"}; cmp.Diff(want, got) != "" {
		t.Fatalf("symbols = %#v, want %#v", got, want)
	}
	if a.procedures[0].Span.End >= a.procedures[1].Span.Start {
		t.Fatalf("overlapping procedure scopes: %+v and %+v", a.procedures[0].Span, a.procedures[1].Span)
	}
	if a.procedures[0].Symbols["LOCAL"][0] == a.procedures[1].Symbols["LOCAL"][0] {
		t.Fatal("equal local names in separate procedures share identity")
	}
}

func TestAnalyzeProcedureNestedBlocksAndParameterTypeCommas(t *testing.T) {
	text := `CREATE PROCEDURE p (id INTEGER, amount NUMERIC(15,2))
RETURNS (result NUMERIC(15,2)) AS
DECLARE VARIABLE local NUMERIC(15,2);
BEGIN
  CASE WHEN id > 0 THEN
    BEGIN
      local = amount;
    END
  END
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"ID", "AMOUNT", "RESULT", "LOCAL"}, symbolKeys(a.Symbols)); diff != "" {
		t.Fatal(diff)
	}
	if got, want := a.procedures[0].Span.End, len(text); got != want {
		t.Fatalf("procedure end = %d, want %d", got, want)
	}
}

func TestAnalyzeProcedureQuotedAndDuplicateDeclarations(t *testing.T) {
	text := `CREATE PROCEDURE p ("MixedName" INTEGER) RETURNS (result INTEGER) AS
DECLARE VARIABLE local INTEGER;
DECLARE VARIABLE local VARCHAR(10);
BEGIN
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := symbolKeys(a.Symbols), []string{"MixedName", "RESULT", "LOCAL", "LOCAL"}; cmp.Diff(want, got) != "" {
		t.Fatalf("symbols = %#v, want %#v", got, want)
	}
	for _, symbol := range a.procedures[0].Symbols["LOCAL"] {
		if symbol.RenameBlocked == "" {
			t.Fatalf("duplicate declaration is not blocked: %+v", symbol)
		}
	}
	if a.procedures[0].Symbols["MixedName"][0].Name.Quoted != true {
		t.Fatal("quoted parameter lost its quoted identity")
	}
}

func TestAnalyzeProcedureRecoversIncompleteBodyAtNextHeader(t *testing.T) {
	text := `ALTER PROCEDURE first (oldid INTEGER) AS
DECLARE VARIABLE oldlocal INTEGER;
BEGIN
  oldlocal = oldid;
ALTER PROCEDURE second (newid INTEGER) AS
DECLARE VARIABLE newlocal INTEGER;
BEGIN
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(a.procedures), 2; got != want {
		t.Fatalf("procedure count = %d, want %d", got, want)
	}
	if got, want := a.procedures[0].Span.End, a.procedures[1].Span.Start; got != want {
		t.Fatalf("incomplete scope end = %d, next start = %d", got, want)
	}
	if got, want := a.procedures[1].Span.End, len(text); got != want {
		t.Fatalf("last incomplete scope end = %d, want %d", got, want)
	}
}

func TestAnalyzeProcedureInterBaseTerminators(t *testing.T) {
	tests := []struct {
		name string
		text string
		key  string
	}{
		{
			name: "caret",
			text: `SET TERM ^ ;
CREATE PROCEDURE p (id INTEGER) RETURNS (result INTEGER) AS
BEGIN
  result = id;
END^`,
			key: "ID",
		},
		{
			name: "double exclamation",
			text: `SET TERM !! ;
CREATE PROCEDURE p (id INTEGER) RETURNS (result INTEGER) AS
BEGIN
  result = '!!';
END!!`,
			key: "ID",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := Analyze(tt.text, interBaseVariant())
			if err != nil {
				t.Fatal(err)
			}
			if got, want := symbolKeys(a.Symbols), []string{tt.key, "RESULT"}; cmp.Diff(want, got) != "" {
				t.Fatalf("symbols = %#v, want %#v", got, want)
			}
		})
	}
}
