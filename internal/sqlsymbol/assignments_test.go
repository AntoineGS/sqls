package sqlsymbol

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

type testCatalog struct {
	columns map[string][]ColumnType
}

func (c testCatalog) Columns(table Name) ([]ColumnType, bool) {
	columns, ok := c.columns[table.Key()]
	return columns, ok
}

func TestWidthAssignments(t *testing.T) {
	tests := []struct {
		name        string
		text        string
		catalog     Catalog
		markers     []string
		occurrences []int
		wantText    []string
		widths      []string
	}{
		{
			name: "local assignment",
			text: `CREATE PROCEDURE P (LARGE VARCHAR(40)) RETURNS (SMALL VARCHAR(20)) AS
BEGIN
  SMALL = LARGE;
END`,
			markers:     []string{"SMALL"},
			occurrences: []int{1},
			wantText:    []string{"LARGE", "SMALL", "40", "20"},
		},
		{
			name: "quoted character set assignment",
			text: `CREATE PROCEDURE P (LARGE CHAR(40) CHARACTER SET "UTF8") RETURNS (SMALL CHAR(20)) AS BEGIN SMALL = LARGE; END`,
			markers:     []string{"SMALL"},
			occurrences: []int{1},
			wantText:    []string{"LARGE", "SMALL", "40", "20"},
		},
		{
			name:     "update target resolves only from updated relation",
			text:     `CREATE PROCEDURE P (LARGE VARCHAR(40)) AS BEGIN UPDATE DST SET VALUE = :LARGE; END`,
			catalog:  testCatalog{columns: map[string][]ColumnType{"DST": {{Name: "VALUE", Type: "VARCHAR(20)"}}}},
			markers:  []string{"VALUE"},
			wantText: []string{"LARGE", "VALUE", "40", "20"},
		},
		{
			name: "insert values matches columns across rows",
			text: `CREATE PROCEDURE P AS BEGIN
  INSERT INTO DST (VALUE, LABEL)
  VALUES ('abcdefghijklmnopqrstu', 'ok'), ('fine', 'abcdef');
END`,
			catalog: testCatalog{columns: map[string][]ColumnType{
				"DST": {{Name: "VALUE", Type: "VARCHAR(20)"}, {Name: "LABEL", Type: "VARCHAR(4)"}},
			}},
			markers:  []string{"VALUE", "LABEL"},
			wantText: []string{"abcdefghijklmnopqrstu", "VALUE", "21", "20", "abcdef", "LABEL", "6", "4"},
		},
		{
			name: "select into local output",
			text: `CREATE PROCEDURE P AS
DECLARE VARIABLE SMALL VARCHAR(20);
BEGIN
  SELECT SRC.VALUE INTO :SMALL FROM SRC;
  SMALL = SMALL;
END`,
			catalog:     testCatalog{columns: map[string][]ColumnType{"SRC": {{Name: "VALUE", Type: "VARCHAR(40)"}}}},
			markers:     []string{"SMALL"},
			occurrences: []int{1},
			wantText:    []string{"SRC.VALUE", "SMALL", "40", "20"},
		},
		{
			name: "select into skips unresolved target independently",
			text: `CREATE PROCEDURE P AS
DECLARE VARIABLE SMALL VARCHAR(20);
BEGIN
  SELECT SRC.VALUE, SRC.VALUE INTO :SMALL, :UNKNOWN_TARGET FROM SRC;
  SMALL = SMALL;
END`,
			catalog:     testCatalog{columns: map[string][]ColumnType{"SRC": {{Name: "VALUE", Type: "VARCHAR(40)"}}}},
			markers:     []string{"SMALL"},
			occurrences: []int{1},
			wantText:    []string{"SRC.VALUE", "SMALL", "40", "20"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := Analyze(tt.text, interBaseVariant())
			if err != nil {
				t.Fatal(err)
			}
			findings := widthFindings(a.Diagnostics(tt.catalog))
			if len(findings) != len(tt.markers) {
				t.Fatalf("width diagnostics = %+v, want %d findings", findings, len(tt.markers))
			}
			want := make([]Finding, len(tt.markers))
			for i, marker := range tt.markers {
				occurrence := 0
				if len(tt.occurrences) > i {
					occurrence = tt.occurrences[i]
				}
				want[i] = Finding{Span: markerSpan(t, tt.text, marker, occurrence), Code: "interbase-string-truncation", Severity: 2}
			}
			messages := make([]string, 0, len(findings))
			for i := range findings {
				want[i].Message = findings[i].Message
				if findings[i].Message == "" {
					t.Fatalf("finding %d has an empty message", i)
				}
				messages = append(messages, findings[i].Message)
			}
			allMessages := strings.Join(messages, "\n")
			for _, text := range tt.wantText {
				if !strings.Contains(allMessages, text) {
					t.Errorf("finding messages %q do not identify %q", allMessages, text)
				}
			}
			if diff := cmp.Diff(want, findings); diff != "" {
				t.Fatalf("width diagnostics (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWidthAssignmentsStaySilentWhenUnproven(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		catalog Catalog
	}{
		{
			name: "fitting literal",
			text: `CREATE PROCEDURE P AS BEGIN INSERT INTO DST (VALUE) VALUES ('short'); END`,
			catalog: testCatalog{columns: map[string][]ColumnType{
				"DST": {{Name: "VALUE", Type: "VARCHAR(20)"}},
			}},
		},
		{
			name: "implicit insert target order",
			text: `CREATE PROCEDURE P AS BEGIN INSERT INTO DST VALUES ('abcdefghijklmnopqrstu'); END`,
			catalog: testCatalog{columns: map[string][]ColumnType{
				"DST": {{Name: "VALUE", Type: "VARCHAR(20)"}},
			}},
		},
		{
			name: "incomplete values row",
			text: `CREATE PROCEDURE P AS BEGIN INSERT INTO DST (VALUE) VALUES ('abcdefghijklmnopqrstu'; END`,
			catalog: testCatalog{columns: map[string][]ColumnType{
				"DST": {{Name: "VALUE", Type: "VARCHAR(20)"}},
			}},
		},
		{
			name: "incomplete trailing update assignment",
			text: `CREATE PROCEDURE P (LARGE VARCHAR(40)) AS BEGIN UPDATE DST SET VALUE = :LARGE, OTHER =; END`,
			catalog: testCatalog{columns: map[string][]ColumnType{
				"DST": {{Name: "VALUE", Type: "VARCHAR(20)"}, {Name: "OTHER", Type: "VARCHAR(20)"}},
			}},
		},
		{
			name: "incomplete insert select predicate",
			text: `CREATE PROCEDURE P AS BEGIN INSERT INTO DST (VALUE) SELECT SRC.VALUE FROM SRC WHERE; END`,
			catalog: testCatalog{columns: map[string][]ColumnType{
				"SRC": {{Name: "VALUE", Type: "VARCHAR(40)"}},
				"DST": {{Name: "VALUE", Type: "VARCHAR(20)"}},
			}},
		},
		{
			name: "incomplete insert select relation list",
			text: `CREATE PROCEDURE P AS BEGIN INSERT INTO DST (VALUE) SELECT SRC.VALUE FROM SRC,; END`,
			catalog: testCatalog{columns: map[string][]ColumnType{
				"SRC": {{Name: "VALUE", Type: "VARCHAR(40)"}},
				"DST": {{Name: "VALUE", Type: "VARCHAR(20)"}},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := Analyze(tt.text, interBaseVariant())
			if err != nil {
				t.Fatal(err)
			}
			if got := widthFindings(a.Diagnostics(tt.catalog)); len(got) != 0 {
				t.Fatalf("width diagnostics = %+v, want none", got)
			}
		})
	}
}

func TestInsertSelectWidths(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		catalog Catalog
		markers []string
		widths  []string
	}{
		{
			name: "qualified source column",
			text: `CREATE PROCEDURE P AS BEGIN INSERT INTO DST (VALUE) SELECT SRC.VALUE FROM SRC; END`,
			catalog: testCatalog{columns: map[string][]ColumnType{
				"SRC": {{Name: "VALUE", Type: "VARCHAR(40)"}},
				"DST": {{Name: "VALUE", Type: "VARCHAR(20)"}},
			}},
			markers: []string{"SRC.VALUE"},
		},
		{
			name: "known expression projections",
			text: `CREATE PROCEDURE P AS BEGIN
  INSERT INTO DST (A, B, C, D)
  SELECT SRC.VALUE || 'x', CAST(SRC.VALUE AS VARCHAR(30)), TRIM(SRC.VALUE), SUBSTRING(SRC.VALUE FROM 1 FOR 25)
  FROM SRC;
END`,
			catalog: testCatalog{columns: map[string][]ColumnType{
				"SRC": {{Name: "VALUE", Type: "VARCHAR(40)"}},
				"DST": {
					{Name: "A", Type: "VARCHAR(20)"},
					{Name: "B", Type: "VARCHAR(20)"},
					{Name: "C", Type: "VARCHAR(20)"},
					{Name: "D", Type: "VARCHAR(20)"},
				},
			}},
			markers: []string{"SRC.VALUE || 'x'", "CAST(SRC.VALUE AS VARCHAR(30))", "TRIM(SRC.VALUE)", "SUBSTRING(SRC.VALUE FROM 1 FOR 25)"},
			widths:  []string{"41", "30", "40", "25"},
		},
		{
			name: "quoted source and destination names",
			text: `CREATE PROCEDURE P AS BEGIN INSERT INTO "Dst" ("Value") SELECT S."Mixed" FROM "Src" S; END`,
			catalog: testCatalog{columns: map[string][]ColumnType{
				"Src": {{Name: "Mixed", Type: "VARCHAR(40)"}},
				"Dst": {{Name: "Value", Type: "VARCHAR(20)"}},
			}},
			markers: []string{`S."Mixed"`},
		},
		{
			name: "ambiguous joined source column",
			text: `CREATE PROCEDURE P AS BEGIN INSERT INTO DST (VALUE) SELECT VALUE FROM SRC S JOIN OTHER O ON S.ID = O.ID; END`,
			catalog: testCatalog{columns: map[string][]ColumnType{
				"SRC":   {{Name: "VALUE", Type: "VARCHAR(40)"}, {Name: "ID", Type: "INTEGER"}},
				"OTHER": {{Name: "VALUE", Type: "VARCHAR(50)"}, {Name: "ID", Type: "INTEGER"}},
				"DST":   {{Name: "VALUE", Type: "VARCHAR(20)"}},
			}},
		},
		{
			name: "unsupported projection",
			text: `CREATE PROCEDURE P AS BEGIN INSERT INTO DST (VALUE) SELECT UNKNOWN_FN(SRC.VALUE) FROM SRC; END`,
			catalog: testCatalog{columns: map[string][]ColumnType{
				"SRC": {{Name: "VALUE", Type: "VARCHAR(40)"}},
				"DST": {{Name: "VALUE", Type: "VARCHAR(20)"}},
			}},
		},
		{
			name: "incomplete select statement",
			text: `CREATE PROCEDURE P AS BEGIN INSERT INTO DST (VALUE) SELECT SRC.VALUE FROM SRC`,
			catalog: testCatalog{columns: map[string][]ColumnType{
				"SRC": {{Name: "VALUE", Type: "VARCHAR(40)"}},
				"DST": {{Name: "VALUE", Type: "VARCHAR(20)"}},
			}},
		},
		{
			name: "complete statement after malformed region",
			text: `CREATE PROCEDURE P AS BEGIN
  INSERT INTO DST (VALUE) VALUES ('incomplete';
  INSERT INTO DST (VALUE) SELECT SRC.VALUE FROM SRC;
END`,
			catalog: testCatalog{columns: map[string][]ColumnType{
				"SRC": {{Name: "VALUE", Type: "VARCHAR(40)"}},
				"DST": {{Name: "VALUE", Type: "VARCHAR(20)"}},
			}},
			markers: []string{"SRC.VALUE"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := Analyze(tt.text, interBaseVariant())
			if err != nil {
				t.Fatal(err)
			}
			findings := widthFindings(a.Diagnostics(tt.catalog))
			if len(findings) != len(tt.markers) {
				t.Fatalf("width diagnostics = %+v, want %d findings", findings, len(tt.markers))
			}
			want := make([]Finding, len(tt.markers))
			for i, marker := range tt.markers {
				want[i] = Finding{Span: markerSpan(t, tt.text, marker, 0), Code: "interbase-string-truncation", Severity: 2, Message: findings[i].Message}
			}
			for i, finding := range findings {
				wantWidth := "40"
				if len(tt.widths) > i {
					wantWidth = tt.widths[i]
				}
				if !strings.Contains(finding.Message, wantWidth) || !strings.Contains(finding.Message, "20") {
					t.Errorf("finding message %q does not identify source width %s and destination width 20", finding.Message, wantWidth)
				}
				if finding.Span != want[i].Span {
					t.Errorf("finding span = %+v, want projection span %+v", finding.Span, want[i].Span)
				}
			}
		})
	}
}

func widthFindings(findings []Finding) []Finding {
	result := make([]Finding, 0)
	for _, finding := range findings {
		if finding.Code == "interbase-string-truncation" {
			result = append(result, finding)
		}
	}
	return result
}

func markerSpan(t *testing.T, text, marker string, occurrence int) Span {
	t.Helper()
	from := 0
	for i := 0; i <= occurrence; i++ {
		relative := strings.Index(text[from:], marker)
		if relative < 0 {
			t.Fatalf("marker %q occurrence %d not found", marker, occurrence)
		}
		from += relative
		if i != occurrence {
			from += len(marker)
		}
	}
	return Span{Start: from, End: from + len(marker)}
}
