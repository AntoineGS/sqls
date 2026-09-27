package sqlsymbol

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func TestLocalInterBaseExample(t *testing.T) {
	path := os.Getenv("SQLS_SYMBOL_EXAMPLE")
	if path == "" {
		t.Skip("SQLS_SYMBOL_EXAMPLE not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	a, err := Analyze(text, dialect.DriverVariant{
		Driver:  dialect.DatabaseDriverInterBase,
		Variant: dialect.SQLVariantInterBase1,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(text, "\n")
	for _, target := range []struct {
		line int
		name string
	}{
		{line: 265, name: "import_order_line_items"},
		{line: 266, name: "import_order_payment"},
	} {
		if target.line > len(lines) {
			t.Fatalf("example has %d lines; want line %d", len(lines), target.line)
		}
		line := lines[target.line-1]
		column := strings.Index(line, target.name)
		if column < 0 {
			t.Fatalf("expected %s on example line %d", target.name, target.line)
		}
		lineOffset := column
		for index := 0; index < target.line-1; index++ {
			lineOffset += len(lines[index]) + 1
		}
		resolution := a.Resolve(lineOffset)
		if resolution.Role != Relation || resolution.SQL == nil || !resolution.SQL.Name.MatchesCatalogName(strings.ToUpper(target.name)) {
			t.Fatalf("line %d relation resolution = %+v, want exact table %s", target.line, resolution, strings.ToUpper(target.name))
		}
	}
	needle := "IMPORTEXTERNALORDER_EMPLYID(:HEADEREMPLYID_TEMP"
	call := strings.Index(text, needle)
	if call < 0 {
		t.Fatal("expected example call not found")
	}
	offset := call + len("IMPORTEXTERNALORDER_EMPLYID(:")
	r := a.Resolve(offset)
	if r.Role != Local || r.Symbol == nil {
		t.Fatalf("resolution: %+v", r)
	}
	if n := len(a.References(r.Symbol, true)); n != 3 {
		t.Fatalf("HEADEREMPLYID_TEMP has %d occurrences, want 3", n)
	}
	if _, err := a.Rename(r.Symbol, "HEADER_EMPLOYEE_TEMP"); err != nil {
		t.Fatal(err)
	}

	t.Run("CUSTOMERINVOICE update references", func(t *testing.T) {
		upper := strings.ToUpper(text)
		update := strings.Index(upper, "UPDATE CUSTOMERINVOICE")
		if update < 0 {
			t.Skip("CUSTOMERINVOICE UPDATE is not present in this example")
		}
		orderTotal := strings.Index(upper[update:], "ORDERTOTAL")
		if orderTotal < 0 {
			t.Fatal("expected ORDERTOTAL in UPDATE not found")
		}
		order := a.Resolve(update + orderTotal)
		if order.Role != Local || order.Symbol == nil || order.Symbol.Name.Key() != "ORDERTOTAL" {
			t.Fatalf("ORDERTOTAL resolution: %+v", order)
		}
		declaration := order.Symbol.Declaration
		if declaration.Start < 0 || declaration.End > len(text) || declaration.Start >= declaration.End {
			t.Fatalf("ORDERTOTAL declaration has invalid span: %+v", declaration)
		}
		declarationText := text[declaration.Start:declaration.End]
		declarationLine := strings.Count(text[:declaration.Start], "\n") + 1
		if declarationText != "ORDERTOTAL" || declarationLine != 55 {
			t.Fatalf("ORDERTOTAL resolved from UPDATE to declaration %q on line %d (span %+v), want exact ORDERTOTAL token on line 55", declarationText, declarationLine, declaration)
		}

		amountPaid := strings.Index(upper[update:], "AMOUNTPAID")
		if amountPaid < 0 {
			t.Fatal("expected AMOUNTPAID target not found")
		}
		column := a.Resolve(update + amountPaid)
		if column.Role != Column || column.SQL == nil {
			t.Fatalf("AMOUNTPAID resolution: %+v, want column with SQL candidates", column)
		}
		owned := false
		for _, scope := range column.SQL.Scopes {
			for _, relation := range scope {
				if relation.Name.Key() == "CUSTOMERINVOICE" {
					owned = true
				}
			}
		}
		if !owned {
			t.Fatalf("AMOUNTPAID candidates do not include CUSTOMERINVOICE: %+v", column.SQL.Scopes)
		}
	})
}

func TestDiagnosticExpansionRecoveryAndCommentLiteralNegatives(t *testing.T) {
	variant := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}
	commentLiteral := "SELECT 'MISSING = NULL' AS NOTE FROM T; -- MISSING = NULL\nSELECT 1 FROM T;"
	commentAnalysis, err := Analyze(commentLiteral, variant)
	if err != nil {
		t.Fatal(err)
	}
	if findings := commentAnalysis.Diagnostics(nil); len(findings) != 0 {
		t.Fatalf("comment/literal-only names and NULL comparisons produced findings: %+v", findings)
	}
	for _, text := range []string{
		"SELECT MISSING FROM T; SELECT FROM; SELECT 1 + 2 FROM T;",
		commentLiteral,
		"/* SELECT MISSING FROM T WHERE A = NULL; */ SELECT 1 FROM T;",
		"CREATE PROCEDURE P AS BEGIN IF (1 + (2 * 3) > 0) THEN V = 1; END",
	} {
		a, err := Analyze(text, variant)
		if err != nil {
			t.Fatalf("Analyze(%q): %v", text, err)
		}
		got := a.Diagnostics(nil)
		again := a.Diagnostics(nil)
		if !reflect.DeepEqual(got, again) {
			t.Fatalf("diagnostics are not deterministic for %q: first=%+v second=%+v", text, got, again)
		}
		for _, finding := range got {
			if finding.Span.Start < 0 || finding.Span.Start >= finding.Span.End || finding.Span.End > len(text) {
				t.Fatalf("finding %s has out-of-source span %+v for %q", finding.Code, finding.Span, text)
			}
		}
		if strings.Contains(text, "MISSING") {
			for _, finding := range got {
				if strings.Contains(text[finding.Span.Start:finding.Span.End], "MISSING") {
					t.Fatalf("finding %s points into a comment/literal identifier in %q", finding.Code, text)
				}
			}
		}
	}
}

func TestDiagnosticExpansionBudgetWithholdsPartialRegionAndKeepsCompleteNeighbor(t *testing.T) {
	var source strings.Builder
	source.WriteString("SELECT ID FROM T WHERE V = NULL;\n")
	source.WriteString("CREATE PROCEDURE OVER_BUDGET RETURNS (O INTEGER) AS DECLARE VARIABLE V INTEGER; BEGIN O = V;")
	for i := 0; i < 6000; i++ {
		source.WriteString(" V = 1;")
	}
	source.WriteString(" SUSPEND; END")
	text := source.String()
	a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
	if err != nil {
		t.Fatalf("Analyze adversarial bounded fixture: %v", err)
	}
	options := DiagnosticOptions{Rules: map[string]string{
		codeReadBeforeAssignment: "warning",
		codeOutputNotAssigned:    "warning",
		codeDeadStore:            "warning",
		codeUnreachable:          "warning",
		codeNullableAssignment:   "warning",
		codeNullableNotIn:        "warning",
		codeOuterJoinFilter:      "warning",
	}}
	findings := a.DiagnosticsWithOptions(nil, options)
	if len(findings) != 1 || findings[0].Code != codeNullComparison {
		t.Fatalf("findings after an over-budget procedure = %+v, want only the independently complete neighboring null-comparison", findings)
	}
	if got, want := text[findings[0].Span.Start:findings[0].Span.End], "V = NULL"; got != want {
		t.Fatalf("neighbor finding span text = %q, want %q", got, want)
	}
}

func FuzzDiagnosticExpansionRecovery(f *testing.F) {
	for _, seed := range []string{
		"SELECT A FROM T; SELECT FROM; SELECT B FROM U;",
		"SELECT (1 + 2) * 3 FROM T WHERE ID = NULL;",
		"WITH A AS (SELECT 1 AS X), B AS (SELECT X FROM A) SELECT X FROM B;",
		"SELECT 'MISSING = NULL' FROM T; -- MISSING = NULL\nSELECT 1 FROM T;",
		"CREATE PROCEDURE P AS BEGIN V = CASE WHEN 1 = 1 THEN 2 ELSE 3 END; END",
		"SELECT (((((((1)))))));",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 4096 {
			t.Skip()
		}
		variant := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}
		a, err := Analyze(text, variant)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		first := a.Diagnostics(nil)
		second := a.Diagnostics(nil)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("nondeterministic findings: first=%+v second=%+v", first, second)
		}
		for _, finding := range first {
			if finding.Span.Start < 0 || finding.Span.Start >= finding.Span.End || finding.Span.End > len(text) {
				t.Fatalf("finding %s span %+v outside source length %d", finding.Code, finding.Span, len(text))
			}
			for _, ignored := range []string{"'MISSING = NULL'", "-- MISSING = NULL"} {
				if start := strings.Index(text, ignored); start >= 0 {
					end := start + len(ignored)
					if finding.Span.Start < end && start < finding.Span.End {
						t.Fatalf("finding %s span %+v overlaps comment/literal seed %q", finding.Code, finding.Span, ignored)
					}
				}
			}
		}
	})
}
