package queryparams

import (
	"reflect"
	"strings"
	"testing"
)

func TestCompileRepeatedNamesAndIgnoredColons(t *testing.T) {
	source := "-- :ignored\nSELECT :EMPLYID, ':literal', :emplyid FROM T;"
	batch, err := Compile(source, 3)
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []Parameter{{Name: "EMPLYID", Key: "EMPLYID"}}
	if !reflect.DeepEqual(batch.Parameters, wantNames) {
		t.Fatalf("parameters = %#v", batch.Parameters)
	}
	wantSQL := "-- :ignored\nSELECT ?, ':literal', ? FROM T"
	if batch.Statements[0].SQL != wantSQL {
		t.Fatal(batch.Statements[0].SQL)
	}
	if !reflect.DeepEqual(batch.Statements[0].Keys, []string{"EMPLYID", "EMPLYID"}) {
		t.Fatal(batch.Statements[0].Keys)
	}
	if len(batch.Statements) != 1 {
		t.Fatalf("statements = %#v", batch.Statements)
	}
}

// Both InterBase SQL dialects treat a doubled quote as an escape that keeps
// the region open; the scanner does not need dialect-specific quote handling
// because dialect 1's double-quoted strings and dialect 3's delimited
// identifiers both preserve the doubled-quote spelling (see
// dialect.InterBaseDialect.PreservesQuotedStringEscapes and
// ScansWholeDelimitedIdentifier). Run the same fixtures against both dialect
// numbers to demonstrate that equivalence.
func TestCompileDoubledQuotesBothDialects(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		wantSQL  string
		wantKeys []string
	}{
		{
			name:     "doubled single quote",
			source:   "SELECT 'it''s here', :NAME FROM T",
			wantSQL:  "SELECT 'it''s here', ? FROM T",
			wantKeys: []string{"NAME"},
		},
		{
			name:     "doubled double quote",
			source:   `SELECT "col""name", :NAME FROM T`,
			wantSQL:  `SELECT "col""name", ? FROM T`,
			wantKeys: []string{"NAME"},
		},
	}
	for _, tt := range tests {
		for _, dialect := range []int{1, 3} {
			t.Run(tt.name, func(t *testing.T) {
				batch, err := Compile(tt.source, dialect)
				if err != nil {
					t.Fatalf("dialect %d: %v", dialect, err)
				}
				if len(batch.Statements) != 1 {
					t.Fatalf("dialect %d: statements = %#v", dialect, batch.Statements)
				}
				if got := batch.Statements[0].SQL; got != tt.wantSQL {
					t.Fatalf("dialect %d: SQL = %q, want %q", dialect, got, tt.wantSQL)
				}
				if !reflect.DeepEqual(batch.Statements[0].Keys, tt.wantKeys) {
					t.Fatalf("dialect %d: Keys = %#v, want %#v", dialect, batch.Statements[0].Keys, tt.wantKeys)
				}
			})
		}
	}
}

func TestCompileCommentsIgnoreColons(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		wantSQL string
	}{
		{
			name:    "line comment before statement",
			source:  "-- :ignored comment\nSELECT :NAME FROM T",
			wantSQL: "-- :ignored comment\nSELECT ? FROM T",
		},
		{
			name:    "block comment before statement",
			source:  "/* :ignored :also */\nSELECT :NAME FROM T",
			wantSQL: "/* :ignored :also */\nSELECT ? FROM T",
		},
		{
			name:    "block comment spanning lines",
			source:  "SELECT /* line1\nline2 :ignored */ :NAME FROM T",
			wantSQL: "SELECT /* line1\nline2 :ignored */ ? FROM T",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			batch, err := Compile(tt.source, 3)
			if err != nil {
				t.Fatal(err)
			}
			if len(batch.Statements) != 1 {
				t.Fatalf("statements = %#v", batch.Statements)
			}
			if got := batch.Statements[0].SQL; got != tt.wantSQL {
				t.Fatalf("SQL = %q, want %q", got, tt.wantSQL)
			}
			if !reflect.DeepEqual(batch.Statements[0].Keys, []string{"NAME"}) {
				t.Fatalf("Keys = %#v", batch.Statements[0].Keys)
			}
		})
	}
}

func TestCompileSemicolonsInsideLiteralsAndComments(t *testing.T) {
	source := "SELECT ';', :NAME FROM T -- trailing ; comment\n; SELECT :NAME2 FROM U"
	batch, err := Compile(source, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Statements) != 2 {
		t.Fatalf("statements = %#v", batch.Statements)
	}
	wantFirst := "SELECT ';', ? FROM T -- trailing ; comment"
	if got := batch.Statements[0].SQL; got != wantFirst {
		t.Fatalf("first SQL = %q, want %q", got, wantFirst)
	}
	wantSecond := "SELECT ? FROM U"
	if got := batch.Statements[1].SQL; got != wantSecond {
		t.Fatalf("second SQL = %q, want %q", got, wantSecond)
	}
}

func TestCompileNonASCIITextBeforePlaceholder(t *testing.T) {
	source := "SELECT 'café', :PRICE FROM T"
	batch, err := Compile(source, 3)
	if err != nil {
		t.Fatal(err)
	}
	wantSQL := "SELECT 'café', ? FROM T"
	if got := batch.Statements[0].SQL; got != wantSQL {
		t.Fatalf("SQL = %q, want %q", got, wantSQL)
	}
	if !reflect.DeepEqual(batch.Statements[0].Keys, []string{"PRICE"}) {
		t.Fatalf("Keys = %#v", batch.Statements[0].Keys)
	}
}

func TestCompileDollarInNames(t *testing.T) {
	source := "SELECT :NAME$1 FROM T"
	batch, err := Compile(source, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []Parameter{{Name: "NAME$1", Key: "NAME$1"}}
	if !reflect.DeepEqual(batch.Parameters, want) {
		t.Fatalf("parameters = %#v", batch.Parameters)
	}
	wantSQL := "SELECT ? FROM T"
	if got := batch.Statements[0].SQL; got != wantSQL {
		t.Fatalf("SQL = %q, want %q", got, wantSQL)
	}
}

func TestCompileIgnoredColonSequences(t *testing.T) {
	source := "SELECT x::integer, y := 1, :9, :NAME FROM T"
	batch, err := Compile(source, 3)
	if err != nil {
		t.Fatal(err)
	}
	wantSQL := "SELECT x::integer, y := 1, :9, ? FROM T"
	if got := batch.Statements[0].SQL; got != wantSQL {
		t.Fatalf("SQL = %q, want %q", got, wantSQL)
	}
	if !reflect.DeepEqual(batch.Statements[0].Keys, []string{"NAME"}) {
		t.Fatalf("Keys = %#v", batch.Statements[0].Keys)
	}
	if !reflect.DeepEqual(batch.Parameters, []Parameter{{Name: "NAME", Key: "NAME"}}) {
		t.Fatalf("parameters = %#v", batch.Parameters)
	}
}

func TestCompileRepeatedNamesAcrossStatements(t *testing.T) {
	source := "SELECT :NAME FROM T; SELECT :name FROM U"
	batch, err := Compile(source, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []Parameter{{Name: "NAME", Key: "NAME"}}
	if !reflect.DeepEqual(batch.Parameters, want) {
		t.Fatalf("parameters = %#v", batch.Parameters)
	}
	if len(batch.Statements) != 2 {
		t.Fatalf("statements = %#v", batch.Statements)
	}
	if !reflect.DeepEqual(batch.Statements[0].Keys, []string{"NAME"}) {
		t.Fatalf("first keys = %#v", batch.Statements[0].Keys)
	}
	if !reflect.DeepEqual(batch.Statements[1].Keys, []string{"NAME"}) {
		t.Fatalf("second keys = %#v", batch.Statements[1].Keys)
	}
}

func TestCompileUnterminatedQuoteAndComment(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{name: "unterminated single quote", source: "SELECT 'unterminated FROM T"},
		{name: "unterminated double quote", source: `SELECT "unterminated FROM T`},
		{name: "unterminated block comment", source: "SELECT 1 /* unterminated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Compile(tt.source, 3); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

func TestCompileMixedNamedAndPositionalMarkers(t *testing.T) {
	if _, err := Compile("SELECT :x, ?", 3); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestCompileBarePositionalMarker(t *testing.T) {
	if _, err := Compile("SELECT ?", 3); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestCompileQuestionMarkInsideCommentOrLiteralDoesNotCount(t *testing.T) {
	tests := []string{
		"SELECT '?', :NAME FROM T",
		"SELECT :NAME FROM T -- what about ?\n",
		"SELECT /* ? */ :NAME FROM T",
	}
	for _, source := range tests {
		t.Run(source, func(t *testing.T) {
			if _, err := Compile(source, 3); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCompileCommentOnlyInput(t *testing.T) {
	tests := []string{
		"-- just a comment\n",
		"   ",
		"/* only a comment */",
		"-- one\n/* two */\n  ",
	}
	for _, source := range tests {
		t.Run(source, func(t *testing.T) {
			batch, err := Compile(source, 3)
			if err != nil {
				t.Fatal(err)
			}
			if len(batch.Statements) != 0 {
				t.Fatalf("statements = %#v", batch.Statements)
			}
			if len(batch.Parameters) != 0 {
				t.Fatalf("parameters = %#v", batch.Parameters)
			}
		})
	}
}

func TestCompileRejectsPSQLNamedMarkers(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{
			name: "create procedure",
			source: "CREATE PROCEDURE P RETURNS (OUT_VAL INTEGER) AS\n" +
				"BEGIN\n" +
				"  OUT_VAL = :local;\n" +
				"END",
		},
		{
			name: "execute block",
			source: "EXECUTE BLOCK AS\n" +
				"DECLARE VARIABLE local INTEGER = :local;\n" +
				"BEGIN\n" +
				"END",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Compile(tt.source, 3); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

// TestCompileRejectsPSQLBatchWhenMarkerIsInALaterFragment guards against a
// PSQL body being split by its internal semicolons into a marker-free
// CREATE PROCEDURE/BEGIN header fragment and a separate fragment that starts
// with a supported keyword (SELECT) and happens to carry the marker. The
// whole batch must still be rejected, because the marker is a PSQL
// local-variable reference inside a CREATE PROCEDURE body, not a real user
// parameter — the header fragment alone never reveals that.
func TestCompileRejectsPSQLBatchWhenMarkerIsInALaterFragment(t *testing.T) {
	source := "CREATE PROCEDURE P AS BEGIN X = 1; SELECT :x INTO :y FROM T; END"
	if _, err := Compile(source, 3); err == nil {
		t.Fatal("want error, got nil")
	}
}

// TestCompileNoMarkerDDLPassesThroughUnchanged pins the other side of the
// same rule: a batch with the identical PSQL/DDL shape but zero named
// markers anywhere must not be validated at all and must pass through as
// ordinary SQL, unchanged and unsplit-by-rejection.
func TestCompileNoMarkerDDLPassesThroughUnchanged(t *testing.T) {
	source := "CREATE PROCEDURE P AS BEGIN X = 1; SELECT Y INTO Z FROM T; END"
	batch, err := Compile(source, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Parameters) != 0 {
		t.Fatalf("parameters = %#v", batch.Parameters)
	}
	var gotSQL []string
	for _, s := range batch.Statements {
		if len(s.Keys) != 0 {
			t.Fatalf("statement %q has keys %#v, want none", s.SQL, s.Keys)
		}
		gotSQL = append(gotSQL, s.SQL)
	}
	want := []string{
		"CREATE PROCEDURE P AS BEGIN X = 1",
		"SELECT Y INTO Z FROM T",
		"END",
	}
	if !reflect.DeepEqual(gotSQL, want) {
		t.Fatalf("statement SQL = %#v, want %#v", gotSQL, want)
	}
}

func TestCompileSupportedStatementForms(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{name: "select", source: "SELECT :NAME FROM T"},
		{name: "insert", source: "INSERT INTO T (A) VALUES (:NAME)"},
		{name: "update", source: "UPDATE T SET A = :NAME WHERE ID = 1"},
		{name: "delete", source: "DELETE FROM T WHERE A = :NAME"},
		{name: "execute procedure", source: "EXECUTE PROCEDURE P(:NAME)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			batch, err := Compile(tt.source, 3)
			if err != nil {
				t.Fatal(err)
			}
			if len(batch.Statements) != 1 || len(batch.Statements[0].Keys) != 1 {
				t.Fatalf("statements = %#v", batch.Statements)
			}
		})
	}
}

func TestCompileWithPrefixedSelectReusesParameterFromCTE(t *testing.T) {
	source := "WITH recent AS (SELECT id FROM orders WHERE created = :FROMDATE) " +
		"SELECT * FROM recent WHERE id = :FROMDATE"
	batch, err := Compile(source, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []Parameter{{Name: "FROMDATE", Key: "FROMDATE"}}
	if !reflect.DeepEqual(batch.Parameters, want) {
		t.Fatalf("parameters = %#v", batch.Parameters)
	}
	if len(batch.Statements) != 1 {
		t.Fatalf("statements = %#v", batch.Statements)
	}
	if !reflect.DeepEqual(batch.Statements[0].Keys, []string{"FROMDATE", "FROMDATE"}) {
		t.Fatalf("keys = %#v", batch.Statements[0].Keys)
	}
	wantArgCount := strings.Count(batch.Statements[0].SQL, "?")
	if wantArgCount != 2 {
		t.Fatalf("positional argument count = %d, want 2", wantArgCount)
	}
}

func TestTrimLeadingTrivia(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "whitespace only", in: "  \t\n  ", want: ""},
		{name: "line comment only", in: "-- comment", want: ""},
		{name: "line comment only with newline", in: "-- comment\n", want: ""},
		{name: "block comment only", in: "/* comment */", want: ""},
		{name: "whitespace and multiple comments", in: "  -- one\n  /* two */  \n", want: ""},
		{name: "real sql after trivia", in: "  -- note\nSELECT 1", want: "SELECT 1"},
		{name: "incomplete block comment kept", in: "/* not closed\nSELECT 1", want: "/* not closed\nSELECT 1"},
		{name: "no trivia", in: "SELECT 1 -- trailing", want: "SELECT 1 -- trailing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TrimLeadingTrivia(tt.in); got != tt.want {
				t.Fatalf("TrimLeadingTrivia(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
