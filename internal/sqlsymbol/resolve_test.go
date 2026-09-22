package sqlsymbol

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func TestResolveOptionalVariablePrefixes(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE amountpaid INTEGER;
BEGIN
  amountpaid = 0;
  amountpaid = amountpaid + 1;
  amountpaid = :amountpaid + 1;
  UPDATE customerinvoice SET amountpaid = :amountpaid;
  /* amountpaid */
END`
	a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatal(err)
	}
	got := a.Resolve(strings.Index(text, "amountpaid = 0"))
	if got.Role != Local || got.Symbol == nil {
		t.Fatalf("resolution: %+v", got)
	}
	if n := len(a.References(got.Symbol, true)); n != 7 {
		t.Fatalf("got %d occurrences, want 7", n)
	}
	column := strings.Index(text, "SET amountpaid") + len("SET ")
	if r := a.Resolve(column); r.Role != Column {
		t.Fatalf("SET target: %+v", r)
	}

	for _, offset := range []int{
		strings.Index(text, ":amountpaid"),
		strings.Index(text, ":amountpaid") + 1,
	} {
		if r := a.Resolve(offset); r.Role != Local || r.Span.Start != strings.Index(text, ":amountpaid")+1 {
			t.Fatalf("prefix resolution at %d: %+v", offset, r)
		}
	}
}

func TestResolveProceduralExpressionsAndSQLBoundaries(t *testing.T) {
	text := `ALTER PROCEDURE p (id INTEGER) RETURNS (ordertotal INTEGER) AS
DECLARE VARIABLE payDate DATE;
DECLARE VARIABLE amount INTEGER;
BEGIN
  payDate = F_StripTime(payDate);
  ordertotal = amount + id;
  SELECT amount + id INTO :ordertotal FROM customerinvoice;
  EXECUTE PROCEDURE otherproc(amount) RETURNING_VALUES :ordertotal;
  UPDATE customerinvoice SET amount = amount + 1 WHERE id = id;
  -- payDate amount id ordertotal
  SELECT 'payDate amount' FROM customerinvoice;
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	payDate := a.procedures[0].Symbols["PAYDATE"][0]
	if got := a.References(payDate, true); len(got) != 3 {
		t.Fatalf("payDate references = %d, want declaration and two uses: %v", len(got), got)
	}
	for _, name := range []string{"F_StripTime", "otherproc"} {
		offset := strings.Index(text, name)
		if got := a.Resolve(offset); got.Role != Callable {
			t.Fatalf("%s resolution = %+v, want callable", name, got)
		}
	}
	setAmount := strings.Index(text, "SET amount") + len("SET ")
	if got := a.Resolve(setAmount); got.Role != Column || got.SQL == nil {
		t.Fatalf("UPDATE SET resolution = %+v, want column", got)
	}
	if got := a.Resolve(strings.Index(text, "SELECT 'payDate") + len("SELECT '")).Role; got != Other {
		t.Fatalf("string resolution = %v, want Other", got)
	}
	if got := a.Resolve(strings.Index(text, "-- payDate") + 3).Role; got != Other {
		t.Fatalf("comment resolution = %v, want Other", got)
	}
}

func TestResolveProcedureScopesAndDuplicateDeclarations(t *testing.T) {
	text := `CREATE PROCEDURE first AS
DECLARE VARIABLE local INTEGER;
BEGIN local = 1; END;
CREATE PROCEDURE second AS
DECLARE VARIABLE local INTEGER;
DECLARE VARIABLE local INTEGER;
BEGIN local = 2; END;`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Index(text, "local = 1")
	second := strings.Index(text, "local = 2")
	if got := a.Resolve(first); got.Role != Local || got.Symbol != a.procedures[0].Symbols["LOCAL"][0] {
		t.Fatalf("first local = %+v", got)
	}
	if got := a.Resolve(second); got.Role != Ambiguous || got.Symbol != nil {
		t.Fatalf("duplicate local = %+v", got)
	}
	if got := a.References(a.procedures[0].Symbols["LOCAL"][0], true); len(got) != 2 {
		t.Fatalf("first references = %v, want declaration and use", got)
	}
	if got := a.References(a.procedures[1].Symbols["LOCAL"][0], true); len(got) != 1 {
		t.Fatalf("ambiguous references = %v, want declaration only", got)
	}
}

func TestReferencesSortedAndDeduplicated(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE x INTEGER;
BEGIN
  x = :x;
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	symbol := a.procedures[0].Symbols["X"][0]
	got := a.References(symbol, false)
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i].Start < got[j].Start }) {
		t.Fatalf("references are not sorted: %v", got)
	}
	if len(got) != 2 || got[0].Start == got[1].Start {
		t.Fatalf("references = %v, want two distinct uses", got)
	}
}

func TestResolveIdentifierEndDoesNotSelectPrecedingToken(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE x INTEGER;
BEGIN
  x = 1;
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(text, "x = 1")
	if got := a.Resolve(start + 1); got.Role != Outside {
		t.Fatalf("identifier end resolution = %+v, want outside", got)
	}
}

func TestResolveHeaderEmployeeFixtureUsesExactSpans(t *testing.T) {
	text := `ALTER PROCEDURE one AS
DECLARE VARIABLE HEADEREMPLYID_TEMP INTEGER;
DECLARE VARIABLE payDate DATE;
DECLARE VARIABLE amount INTEGER;
DECLARE VARIABLE ordertotal INTEGER;
BEGIN
  SELECT :HEADEREMPLYID_TEMP FROM employee;
  SELECT :HEADEREMPLYID_TEMP FROM employee;
  payDate = F_StripTime(payDate);
  ordertotal = amount + amount;
  obj.HEADEREMPLYID_TEMP = 'HEADEREMPLYID_TEMP';
  -- HEADEREMPLYID_TEMP
END;
ALTER PROCEDURE two AS
DECLARE VARIABLE HEADEREMPLYID_TEMP INTEGER;
BEGIN
  HEADEREMPLYID_TEMP = 2;
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	first := a.procedures[0].Symbols["HEADEREMPLYID_TEMP"][0]
	want := []Span{
		first.Declaration,
		{Start: strings.Index(text, ":HEADEREMPLYID_TEMP") + 1, End: strings.Index(text, ":HEADEREMPLYID_TEMP") + len(":HEADEREMPLYID_TEMP")},
	}
	secondPrefix := strings.LastIndex(text, ":HEADEREMPLYID_TEMP")
	want = append(want, Span{Start: secondPrefix + 1, End: secondPrefix + len(":HEADEREMPLYID_TEMP")})
	if got := a.References(first, true); !reflect.DeepEqual(got, want) {
		t.Fatalf("header occurrences = %v, want %v", got, want)
	}
	if got := a.Resolve(strings.Index(text, "F_StripTime")); got.Role != Callable {
		t.Fatalf("function = %+v, want callable", got)
	}
	if got := a.Resolve(strings.Index(text, "obj.HEADEREMPLYID_TEMP") + len("obj.")); got.Role != Column {
		t.Fatalf("member field = %+v, want column", got)
	}
	if got := a.Resolve(strings.Index(text, "'HEADEREMPLYID_TEMP'") + 1); got.Role != Other {
		t.Fatalf("string = %+v, want other", got)
	}
	if got := a.Resolve(strings.Index(text, "-- HEADEREMPLYID_TEMP") + 3); got.Role != Other {
		t.Fatalf("comment = %+v, want other", got)
	}
	if got := a.Resolve(strings.Index(text, "HEADEREMPLYID_TEMP = 2")); got.Symbol == first {
		t.Fatal("same-named symbol from another procedure leaked into first procedure")
	}
}
