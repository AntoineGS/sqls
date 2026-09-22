package sqlsymbol

import (
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func TestRenamePreservesVariablePrefixes(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE amountpaid INTEGER;
BEGIN
  amountpaid = 0;
  amountpaid = amountpaid + :amountpaid;
  UPDATE customerinvoice SET amountpaid = :amountpaid;
END`
	want := `ALTER PROCEDURE p AS
DECLARE VARIABLE paid INTEGER;
BEGIN
  paid = 0;
  paid = paid + :paid;
  UPDATE customerinvoice SET amountpaid = :paid;
END`
	a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	r := a.Resolve(strings.Index(text, "amountpaid = 0"))
	edits, err := a.Rename(r.Symbol, "paid")
	if err != nil {
		t.Fatal(err)
	}
	got := text
	for i := len(edits) - 1; i >= 0; i-- {
		edit := edits[i]
		got = got[:edit.Span.Start] + edit.NewText + got[edit.Span.End:]
	}
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenameRejectsInvalidNamesAndCollisions(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE amount INTEGER;
DECLARE VARIABLE other INTEGER;
BEGIN
  amount = other;
END`
	a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	symbol := a.Resolve(strings.Index(text, "amount =")).Symbol
	if symbol == nil {
		t.Fatal("amount was not resolved to a symbol")
	}
	for _, newName := range []string{"", "new name", "new\nname", "1new", "SELECT", "INTEGER"} {
		if _, err := a.Rename(symbol, newName); err == nil {
			t.Errorf("Rename(%q) succeeded, want validation error", newName)
		}
	}
	if _, err := a.Rename(symbol, "other"); err == nil {
		t.Fatal("rename to another declaration succeeded, want collision error")
	}
	if _, err := a.Rename(symbol, "AMOUNT"); err != nil {
		t.Fatalf("case-only unquoted rename failed: %v", err)
	}
	if _, err := a.Rename(symbol, "ABS"); err != nil {
		t.Fatalf("generic completion function was treated as reserved: %v", err)
	}
}

func TestRenameRejectsNilAndForeignSymbols(t *testing.T) {
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}
	a, err := Analyze("ALTER PROCEDURE p AS DECLARE VARIABLE x INTEGER; BEGIN x = 1; END", dv)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Analyze("ALTER PROCEDURE p AS DECLARE VARIABLE x INTEGER; BEGIN x = 1; END", dv)
	if err != nil {
		t.Fatal(err)
	}
	foreign := b.Symbols[0]
	for _, symbol := range []*Symbol{nil, foreign} {
		if _, err := a.Rename(symbol, "y"); err == nil {
			t.Errorf("Rename(%v) succeeded, want ownership error", symbol)
		}
	}
}

func TestRenameRejectsBlockedSymbol(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE amount INTEGER;
BEGIN
  UPDATE customerinvoice SET amount = amount;
END`
	a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	symbol := a.Symbols[0]
	if symbol.RenameBlocked == "" {
		t.Fatal("expected SQL value expression to block rename")
	}
	if _, err := a.Rename(symbol, "newamount"); err == nil {
		t.Fatal("blocked rename succeeded")
	}
}

func TestRenameQuotedNameKeepsRequestedSpelling(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE "MixedName" INTEGER;
BEGIN
  "MixedName" = :"MixedName";
END`
	a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3})
	if err != nil {
		t.Fatal(err)
	}
	symbol := a.Symbols[0]
	edits, err := a.Rename(symbol, `"New Name"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 3 {
		t.Fatalf("got %d edits, want declaration and two uses", len(edits))
	}
	for _, edit := range edits {
		if edit.NewText != `"New Name"` {
			t.Errorf("NewText = %q, want quoted request spelling", edit.NewText)
		}
	}
}
