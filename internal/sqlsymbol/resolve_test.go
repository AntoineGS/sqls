package sqlsymbol

import (
	"reflect"
	"sort"
	"strconv"
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

func TestResolveUpdatePredicateIsNotSetTarget(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE x INTEGER;
BEGIN
  UPDATE t SET c = (SELECT y FROM u WHERE x = 1) WHERE x = 2;
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	x := a.procedures[0].Symbols["X"][0]
	for _, marker := range []string{"WHERE x = 1", "WHERE x = 2"} {
		got := a.Resolve(strings.Index(text, marker) + len("WHERE "))
		if got.Role != Ambiguous || got.Symbol != nil {
			t.Fatalf("predicate %q = %+v, want ambiguous SQL value", marker, got)
		}
	}
	if x.RenameBlocked == "" {
		t.Fatal("predicate ambiguity did not block rename")
	}
	if got := a.Resolve(strings.Index(text, "SET c") + len("SET ")); got.Role != Column {
		t.Fatalf("SET target = %+v, want column", got)
	}
}

func TestResolveSQLCaseAndNestedSelectKeepSQLContext(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE x INTEGER;
BEGIN
  SELECT CASE WHEN 1 = 1 THEN x ELSE 0 END FROM t;
  SELECT y FROM (SELECT x FROM u) q WHERE q.y = x;
  FOR SELECT x FROM t INTO :x DO x = x + 1;
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{
		strings.Index(text, "THEN x") + len("THEN "),
		strings.Index(text, "SELECT x FROM u") + len("SELECT "),
		strings.Index(text, "= x;") + 2,
		strings.Index(text, "FOR SELECT x") + len("FOR SELECT "),
	} {
		if got := a.Resolve(offset); got.Role != Ambiguous {
			t.Fatalf("SQL x at %d = %+v, want ambiguous", offset, got)
		}
	}
}

func TestResolveAllIntoAndReturningValuesTargets(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE x INTEGER;
DECLARE VARIABLE y INTEGER;
BEGIN
  SELECT c1, c2 FROM t INTO x, y;
  EXECUTE PROCEDURE f() RETURNING_VALUES x, :y;
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"x", "y"} {
		symbol := a.procedures[0].Symbols[strings.ToUpper(name)][0]
		var want []Span
		if name == "x" {
			into := strings.Index(text, "INTO x") + len("INTO ")
			returned := strings.Index(text, "RETURNING_VALUES x") + len("RETURNING_VALUES ")
			want = []Span{{Start: into, End: into + 1}, {Start: returned, End: returned + 1}}
		} else {
			into := strings.Index(text, "x, y") + len("x, ")
			returned := strings.Index(text, ":y") + 1
			want = []Span{{Start: into, End: into + 1}, {Start: returned, End: returned + 1}}
		}
		if got := a.References(symbol, false); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s uses = %v, want %v", name, got, want)
		}
	}
}

func TestResolveUnsupportedSQLAndExecuteBlockRenameSafety(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE x INTEGER;
DECLARE VARIABLE unrelated INTEGER;
BEGIN
  MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN x = 1;
  WITH q AS (SELECT 1) SELECT x FROM q;
  EXECUTE BLOCK AS BEGIN x = 2; END;
  unrelated = 3;
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	x := a.procedures[0].Symbols["X"][0]
	for _, marker := range []string{"THEN x", "SELECT x FROM q", "BEGIN x"} {
		if got := a.Resolve(strings.Index(text, marker) + strings.Index(marker, "x")); got.Role != Ambiguous {
			t.Fatalf("unsupported %q = %+v, want ambiguous", marker, got)
		}
	}
	if x.RenameBlocked == "" {
		t.Fatal("unsupported syntax did not block x")
	}
	if got := a.Resolve(strings.Index(text, "unrelated = 3")); got.Role != Local {
		t.Fatalf("unrelated local = %+v, want local", got)
	}
}

func BenchmarkAnalyzeResolutionScaling(b *testing.B) {
	for _, count := range []int{100, 1000, 5000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			var source strings.Builder
			source.WriteString("ALTER PROCEDURE p AS\nDECLARE VARIABLE x INTEGER;\nBEGIN\n")
			for i := 0; i < count; i++ {
				source.WriteString("x = x + 1;\n")
			}
			source.WriteString("END")
			text := source.String()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := Analyze(text, interBaseVariant()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestResolveParameterOnlyProcedureBody(t *testing.T) {
	text := `ALTER PROCEDURE p(x INTEGER) AS
BEGIN
  x = x + 1;
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	symbol := a.procedures[0].Symbols["X"][0]
	if got := a.Resolve(strings.Index(text, "x = x")); got.Role != Local || got.Symbol != symbol {
		t.Fatalf("parameter assignment = %+v, want local parameter", got)
	}
	if got := a.References(symbol, true); len(got) != 3 {
		t.Fatalf("parameter references = %v, want declaration and two uses", got)
	}
	if symbol.RenameBlocked != "" {
		t.Fatalf("parameter unexpectedly blocked: %q", symbol.RenameBlocked)
	}
}

func TestResolveUnsupportedExecuteBlockKeepsCaseFrames(t *testing.T) {
	text := `ALTER PROCEDURE p AS
DECLARE VARIABLE x INTEGER;
BEGIN
  EXECUTE BLOCK AS BEGIN
    SELECT CASE WHEN 1 = 1 THEN 0 ELSE 0 END FROM t;
    x = 2;
  END;
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	x := a.procedures[0].Symbols["X"][0]
	got := a.Resolve(strings.Index(text, "x = 2"))
	if got.Role != Ambiguous || got.Symbol != nil {
		t.Fatalf("execute block local = %+v, want ambiguous", got)
	}
	if x.RenameBlocked == "" {
		t.Fatal("nested CASE caused execute-block protection to end early")
	}
}

func BenchmarkAnalyzeMalformedInsertScaling(b *testing.B) {
	for _, count := range []int{100, 1000, 5000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			text := "ALTER PROCEDURE p AS BEGIN " + strings.Repeat("INSERT ", count) + " END"
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := Analyze(text, interBaseVariant()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestResolveInsertValuesAreNotColumnList(t *testing.T) {
	text := `ALTER PROCEDURE p(x INTEGER) AS
BEGIN
  INSERT INTO t VALUES (x);
  INSERT INTO t (x) VALUES (x);
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	valuesX := strings.Index(text, "VALUES (x)") + len("VALUES (")
	if got := a.Resolve(valuesX); got.Role != Ambiguous || got.Symbol != nil {
		t.Fatalf("VALUES variable = %+v, want ambiguous SQL value", got)
	}
	columnX := strings.Index(text, "INSERT INTO t (x)") + len("INSERT INTO t (")
	if got := a.Resolve(columnX); got.Role != Column {
		t.Fatalf("explicit column = %+v, want column", got)
	}
	if symbol := a.procedures[0].Symbols["X"][0]; symbol.RenameBlocked == "" {
		t.Fatal("VALUES variable did not block parameter rename")
	}
}

func TestResolveExecuteBlockFramesEndAtBlockAndProcedure(t *testing.T) {
	text := `ALTER PROCEDURE first AS
DECLARE VARIABLE x INTEGER;
DECLARE VARIABLE y INTEGER;
BEGIN
  EXECUTE BLOCK AS BEGIN
    SELECT CASE WHEN 1 = 1 THEN 0 ELSE 0 END FROM t;
    x = 1;
  END;
  y = 2;
END;
ALTER PROCEDURE second AS
DECLARE VARIABLE y INTEGER;
BEGIN
  y = 3;
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	x := a.procedures[0].Symbols["X"][0]
	if got := a.Resolve(strings.Index(text, "x = 1")); got.Role != Ambiguous || got.Symbol != nil {
		t.Fatalf("execute block x = %+v, want ambiguous", got)
	}
	if x.RenameBlocked == "" {
		t.Fatal("execute block x was not rename-blocked")
	}
	firstY := a.procedures[0].Symbols["Y"][0]
	if got := a.Resolve(strings.Index(text, "y = 2")); got.Role != Local || got.Symbol != firstY {
		t.Fatalf("post-block y = %+v, want first-procedure local", got)
	}
	if firstY.RenameBlocked != "" {
		t.Fatalf("post-block y unexpectedly blocked: %q", firstY.RenameBlocked)
	}
	secondY := a.procedures[1].Symbols["Y"][0]
	if got := a.Resolve(strings.LastIndex(text, "y = 3")); got.Role != Local || got.Symbol != secondY {
		t.Fatalf("second-procedure y = %+v, want separate local", got)
	}
	if secondY.RenameBlocked != "" {
		t.Fatalf("second-procedure y unexpectedly blocked: %q", secondY.RenameBlocked)
	}
}
