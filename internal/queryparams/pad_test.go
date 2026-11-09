package queryparams

import (
	"strings"
	"testing"
)

func TestPadNamedMarkers(t *testing.T) {
	tests := []struct {
		name      string
		statement string
		want      string
		wantOK    bool
	}{
		{
			name:      "pads marker without shifting bytes",
			statement: "SELECT * FROM T WHERE A = :ORDERID",
			want:      "SELECT * FROM T WHERE A = ?       ",
			wantOK:    true,
		},
		{
			name:      "ignores quoted comments casts and assignment",
			statement: "SELECT ':X', \" :X \", A FROM T -- :X\n WHERE B = :REAL /* :X */ AND C::INT = 1 AND D := 2",
			want:      "SELECT ':X', \" :X \", A FROM T -- :X\n WHERE B = ?     /* :X */ AND C::INT = 1 AND D := 2",
			wantOK:    true,
		},
		{
			name:      "doubled quotes hide markers",
			statement: `SELECT 'it'':HIDDEN', "col"":HIDDEN", :VISIBLE FROM T`,
			want:      `SELECT 'it'':HIDDEN', "col"":HIDDEN", ?        FROM T`,
			wantOK:    true,
		},
		{
			name:      "supports with-prefixed select after comments",
			statement: "  /* note */ WITH q AS (SELECT :N FROM T) SELECT * FROM q",
			want:      "  /* note */ WITH q AS (SELECT ?  FROM T) SELECT * FROM q",
			wantOK:    true,
		},
		{
			name:      "unsupported procedure is unchanged",
			statement: "CREATE PROCEDURE P AS BEGIN X = :A; END",
			want:      "CREATE PROCEDURE P AS BEGIN X = :A; END",
		},
		{
			name:      "no marker is unchanged",
			statement: "SELECT A FROM T",
			want:      "SELECT A FROM T",
		},
		{
			name:      "unterminated single quote is unchanged",
			statement: "SELECT ':A FROM T",
			want:      "SELECT ':A FROM T",
		},
		{
			name:      "unterminated double quote is unchanged",
			statement: `SELECT ":A FROM T`,
			want:      `SELECT ":A FROM T`,
		},
		{
			name:      "unterminated block comment is unchanged",
			statement: "SELECT :A FROM T /* :B",
			want:      "SELECT :A FROM T /* :B",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := PadNamedMarkers(tt.statement)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("PadNamedMarkers() = (%q, %t), want (%q, %t)", got, ok, tt.want, tt.wantOK)
			}
			if len(got) != len(tt.statement) {
				t.Errorf("padded length = %d, want original byte length %d", len(got), len(tt.statement))
			}
		})
	}
}

func TestPadNamedMarkersSupportsStatementStarts(t *testing.T) {
	statements := []string{
		"SELECT :A FROM T",
		"INSERT INTO T VALUES (:A)",
		"UPDATE T SET A = :A",
		"DELETE FROM T WHERE A = :A",
		"WITH q AS (SELECT :A FROM T) SELECT * FROM q",
		"EXECUTE PROCEDURE P(:A)",
	}
	for _, statement := range statements {
		t.Run(statement, func(t *testing.T) {
			got, ok := PadNamedMarkers(statement)
			if !ok || !strings.Contains(got, "?") {
				t.Fatalf("PadNamedMarkers() = (%q, %t), want a padded supported statement", got, ok)
			}
			if len(got) != len(statement) {
				t.Fatalf("padded length = %d, want %d", len(got), len(statement))
			}
		})
	}
}

func TestPadNamedMarkersLeavesColonsWithoutNamesUntouched(t *testing.T) {
	statement := "SELECT x::integer, y := 1, :9 FROM T"
	got, ok := PadNamedMarkers(statement)
	if ok || got != statement {
		t.Fatalf("PadNamedMarkers() = (%q, %t), want unchanged text and false", got, ok)
	}
}
