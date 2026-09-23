package sqlsymbol

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestDiagnosticUnused(t *testing.T) {
	tests := []struct {
		name         string
		text         string
		declarations []string
		wantFindings []Finding
	}{
		{
			name:         "unread input parameter",
			text:         "CREATE PROCEDURE P (IN_NAME VARCHAR(40)) AS BEGIN END",
			declarations: []string{"IN_NAME"},
			wantFindings: []Finding{{Code: "interbase-unused", Message: "Unused declaration", Severity: 4}},
		},
		{
			name:         "assigned-only local",
			text:         "CREATE PROCEDURE P AS DECLARE VARIABLE OUT_NAME VARCHAR(20); BEGIN OUT_NAME = 'x'; END",
			declarations: []string{"OUT_NAME"},
			wantFindings: []Finding{{Code: "interbase-unused", Message: "Unused declaration", Severity: 4}},
		},
		{
			name:         "read local",
			text:         "CREATE PROCEDURE P AS DECLARE VARIABLE SOURCE_NAME VARCHAR(20); BEGIN IF (SOURCE_NAME = 'x') THEN SOURCE_NAME = 'y'; END",
			declarations: []string{"SOURCE_NAME"},
		},
		{
			name:         "output parameter",
			text:         "CREATE PROCEDURE P RETURNS (OUT_NAME VARCHAR(20)) AS BEGIN END",
			declarations: []string{"OUT_NAME"},
		},
		{
			name:         "ambiguous same-name declarations",
			text:         "CREATE PROCEDURE P AS DECLARE VARIABLE DUP VARCHAR(20); DECLARE VARIABLE DUP VARCHAR(30); BEGIN END",
			declarations: []string{"DUP", "DUP"},
		},
		{
			name:         "SQL occurrence is not proven local",
			text:         "CREATE PROCEDURE P AS DECLARE VARIABLE VALUE_NAME VARCHAR(20); BEGIN SELECT VALUE_NAME FROM T; END",
			declarations: []string{"VALUE_NAME"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := Analyze(tt.text, interBaseVariant())
			if err != nil {
				t.Fatal(err)
			}

			want := make([]Finding, len(tt.wantFindings))
			copy(want, tt.wantFindings)
			for i := range want {
				start := strings.Index(tt.text, tt.declarations[i])
				if start < 0 {
					t.Fatalf("declaration %q not found", tt.declarations[i])
				}
				want[i].Span = Span{Start: start, End: start + len(tt.declarations[i])}
			}
			if got := a.Diagnostics(nil); cmp.Diff(want, got) != "" {
				t.Fatalf("Diagnostics() mismatch (-want +got):\n%s", cmp.Diff(want, got))
			}
		})
	}
}

func TestDiagnosticReadsAndWritesPreserveReferences(t *testing.T) {
	text := `CREATE PROCEDURE P (INPUT_NAME VARCHAR(40)) RETURNS (OUTPUT_NAME VARCHAR(40)) AS
DECLARE VARIABLE LOCAL_NAME VARCHAR(40);
BEGIN
  LOCAL_NAME = INPUT_NAME;
  SELECT :LOCAL_NAME INTO :OUTPUT_NAME FROM T;
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}

	byName := make(map[string]*Symbol, len(a.Symbols))
	for _, symbol := range a.Symbols {
		byName[symbol.Name.Key()] = symbol
	}
	span := func(marker string, occurrence int) Span {
		start, from := -1, 0
		for i := 0; i <= occurrence; i++ {
			relative := strings.Index(text[from:], marker)
			if relative < 0 {
				break
			}
			start = from + relative
			from = start + len(marker)
		}
		if start < 0 {
			t.Fatalf("%q occurrence %d not found", marker, occurrence)
		}
		return Span{Start: start, End: start + len(marker)}
	}

	for _, tc := range []struct {
		name   string
		reads  []Span
		writes []Span
		uses   int
	}{
		{name: "INPUT_NAME", reads: []Span{span("INPUT_NAME", 1)}, uses: 1},
		{name: "LOCAL_NAME", reads: []Span{span("LOCAL_NAME", 2)}, writes: []Span{span("LOCAL_NAME", 1)}, uses: 2},
		{name: "OUTPUT_NAME", writes: []Span{span("OUTPUT_NAME", 1)}, uses: 1},
	} {
		symbol := byName[tc.name]
		if symbol == nil {
			t.Fatalf("missing symbol %s", tc.name)
		}
		if diff := cmp.Diff(tc.reads, symbol.Reads); diff != "" {
			t.Errorf("%s reads (-want +got):\n%s", tc.name, diff)
		}
		if diff := cmp.Diff(tc.writes, symbol.Writes); diff != "" {
			t.Errorf("%s writes (-want +got):\n%s", tc.name, diff)
		}
		if got := len(symbol.Uses); got != tc.uses {
			t.Errorf("%s Uses count = %d, want %d", tc.name, got, tc.uses)
		}
		if got := len(a.References(symbol, true)); got != tc.uses+1 {
			t.Errorf("%s reference count = %d, want %d", tc.name, got, tc.uses+1)
		}
	}
}

func TestDiagnosticSuppressesUnsupportedPossibleRead(t *testing.T) {
	text := `ALTER PROCEDURE P AS
DECLARE VARIABLE MAYBE_READ VARCHAR(20);
BEGIN
  MERGE INTO T USING S ON T.ID = S.ID WHEN MATCHED THEN MAYBE_READ = 'x';
END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Diagnostics(nil); len(got) != 0 {
		t.Fatalf("Diagnostics() = %+v, want no speculative unused finding", got)
	}
}

func TestAnalyzeRetainsDeclarationTypes(t *testing.T) {
	text := `CREATE PROCEDURE P (AMOUNT NUMERIC(15, 2), LABEL VARCHAR(40) CHARACTER SET UTF8)
RETURNS (RESULT DECIMAL(12,3)) AS
DECLARE VARIABLE LOCAL_AMOUNT NUMERIC(8, 2);
BEGIN END`
	a, err := Analyze(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"NUMERIC(15, 2)", "VARCHAR(40) CHARACTER SET UTF8", "DECIMAL(12,3)", "NUMERIC(8, 2)"}
	got := make([]string, 0, len(a.Symbols))
	for _, symbol := range a.Symbols {
		got = append(got, symbol.Type)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("declaration types (-want +got):\n%s", diff)
	}
}
