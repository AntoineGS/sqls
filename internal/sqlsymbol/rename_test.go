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
	for _, newName := range []string{"", "new name", "new\nname", "new--comment", "new /* comment */", " new", "new ", "1new", "SELECT", "INTEGER"} {
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

func TestRenameRejectsInterBaseReservedWordsCaseInsensitively(t *testing.T) {
	text := "ALTER PROCEDURE p AS DECLARE VARIABLE old_name INTEGER; BEGIN old_name = 1; END"
	a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	symbol := a.Symbols[0]
	for _, newName := range []string{"AS", "as", "BEGIN", "begin", "CHAR", "char"} {
		if _, err := a.Rename(symbol, newName); err == nil {
			t.Errorf("Rename(%q) succeeded, want reserved-word validation error", newName)
		}
	}
}

func TestRenameRejectsEmptyDecodedQuotedName(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE old_name INTEGER;
BEGIN old_name = 1; END`
	a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Rename(a.Symbols[0], `""`); err == nil {
		t.Fatal(`Rename("") succeeded, want empty decoded-name validation error`)
	}
}

func TestRenameRejectsDialect1DelimitedReplacement(t *testing.T) {
	text := "ALTER PROCEDURE p AS DECLARE VARIABLE old_name INTEGER; BEGIN old_name = 1; END"
	a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Rename(a.Symbols[0], `"new_name"`); err == nil {
		t.Fatal(`Dialect 1 accepted a double-quoted string as an identifier`)
	}
}

func TestRenameDecodedQuotedAndUnquotedCollision(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE "FOO" INTEGER;
DECLARE VARIABLE bar INTEGER;
BEGIN bar = 1; END`
	a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3})
	if err != nil {
		t.Fatal(err)
	}
	bar := a.Resolve(strings.Index(text, "bar = 1")).Symbol
	if bar == nil {
		t.Fatal("bar was not resolved to a symbol")
	}
	for _, newName := range []string{"foo", `"FOO"`} {
		if _, err := a.Rename(bar, newName); err == nil {
			t.Errorf("Rename(%q) succeeded, want decoded-name collision", newName)
		}
	}
}

func TestRenameAppliedHeaderFixtureIsScopeSensitive(t *testing.T) {
	text := `ALTER PROCEDURE one AS
DECLARE VARIABLE HEADEREMPLYID_TEMP INTEGER;
DECLARE VARIABLE MixedCase INTEGER;
BEGIN
  SELECT :HEADEREMPLYID_TEMP FROM employee;
  SELECT :headeremplYid_temp FROM employee;
  IF (1 = 1) THEN
  BEGIN
    SELECT :HEADEREMPLYID_TEMP FROM employee;
  END
  MixedCase = mixedcase + :MixedCase;
  obj.HEADEREMPLYID_TEMP = 'HEADEREMPLYID_TEMP';
  -- HEADEREMPLYID_TEMP
END;
ALTER PROCEDURE two AS
DECLARE VARIABLE HEADEREMPLYID_TEMP INTEGER;
BEGIN
  HEADEREMPLYID_TEMP = 2;
END`
	want := `ALTER PROCEDURE one AS
DECLARE VARIABLE paid INTEGER;
DECLARE VARIABLE MixedCase INTEGER;
BEGIN
  SELECT :paid FROM employee;
  SELECT :paid FROM employee;
  IF (1 = 1) THEN
  BEGIN
    SELECT :paid FROM employee;
  END
  MixedCase = mixedcase + :MixedCase;
  obj.HEADEREMPLYID_TEMP = 'HEADEREMPLYID_TEMP';
  -- HEADEREMPLYID_TEMP
END;
ALTER PROCEDURE two AS
DECLARE VARIABLE HEADEREMPLYID_TEMP INTEGER;
BEGIN
  HEADEREMPLYID_TEMP = 2;
END`
	a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	resolution := a.Resolve(strings.Index(text, ":HEADEREMPLYID_TEMP") + 1)
	edits, err := a.Rename(resolution.Symbol, "paid")
	if err != nil {
		t.Fatal(err)
	}
	got := applyRenameEdits(text, edits)
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenameAppliedInputAndOutputParameters(t *testing.T) {
	text := `ALTER PROCEDURE p (input_value INTEGER) RETURNS (output_value INTEGER) AS
BEGIN
  output_value = input_value;
  SUSPEND;
END`
	want := `ALTER PROCEDURE p (source_value INTEGER) RETURNS (result_value INTEGER) AS
BEGIN
  result_value = source_value;
  SUSPEND;
END`
	a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	input := a.Resolve(strings.Index(text, "input_value;")).Symbol
	inputEdits, err := a.Rename(input, "source_value")
	if err != nil {
		t.Fatal(err)
	}
	text = applyRenameEdits(text, inputEdits)
	a, err = Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	output := a.Resolve(strings.Index(text, "output_value =")).Symbol
	outputEdits, err := a.Rename(output, "result_value")
	if err != nil {
		t.Fatal(err)
	}
	if got := applyRenameEdits(text, outputEdits); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func applyRenameEdits(text string, edits []Edit) string {
	for i := len(edits) - 1; i >= 0; i-- {
		edit := edits[i]
		text = text[:edit.Span.Start] + edit.NewText + text[edit.Span.End:]
	}
	return text
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
