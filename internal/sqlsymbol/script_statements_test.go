package sqlsymbol

import (
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func TestScriptStatements(t *testing.T) {
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}
	tests := []struct {
		name string
		text string
		want []struct {
			text    string
			routine bool
		}
	}{
		{
			name: "ordinary semicolons",
			text: "SELECT 1 FROM A; SELECT 2 FROM B;",
			want: []struct {
				text    string
				routine bool
			}{{"SELECT 1 FROM A", false}, {"SELECT 2 FROM B", false}},
		},
		{
			name: "set term procedure",
			text: "SET TERM ^ ;\nCREATE PROCEDURE P AS BEGIN SELECT 1 FROM A INTO :X; SUSPEND; END^\nSET TERM ; ^\nSELECT 3 FROM C;",
			want: []struct {
				text    string
				routine bool
			}{{"CREATE PROCEDURE P AS BEGIN SELECT 1 FROM A INTO :X; SUSPEND; END", true}, {"SELECT 3 FROM C", false}},
		},
		{
			name: "routine body without set term",
			text: "CREATE PROCEDURE P (A INTEGER) RETURNS (X INTEGER) AS DECLARE VARIABLE V INTEGER; BEGIN IF (A = 1) THEN BEGIN X = 1; END ELSE X = CASE A WHEN 2 THEN 2 END; SUSPEND; END; SELECT 1 FROM D;",
			want: []struct {
				text    string
				routine bool
			}{{"CREATE PROCEDURE P (A INTEGER) RETURNS (X INTEGER) AS DECLARE VARIABLE V INTEGER; BEGIN IF (A = 1) THEN BEGIN X = 1; END ELSE X = CASE A WHEN 2 THEN 2 END; SUSPEND; END", true}, {"SELECT 1 FROM D", false}},
		},
		{
			name: "alter trigger without body does not swallow",
			text: "ALTER TRIGGER T INACTIVE; SELECT 1 FROM E;",
			want: []struct {
				text    string
				routine bool
			}{{"ALTER TRIGGER T INACTIVE", false}, {"SELECT 1 FROM E", false}},
		},
		{
			name: "trigger body",
			text: "CREATE TRIGGER T FOR X ACTIVE BEFORE INSERT AS BEGIN NEW.A = 1; END; DELETE FROM Y;",
			want: []struct {
				text    string
				routine bool
			}{{"CREATE TRIGGER T FOR X ACTIVE BEFORE INSERT AS BEGIN NEW.A = 1; END", true}, {"DELETE FROM Y", false}},
		},
		{
			name: "isql commands are omitted",
			text: "SET SQL DIALECT 3; CONNECT 'x'; COMMIT; SET GENERATOR G TO 1; SHOW TABLES;",
			want: []struct {
				text    string
				routine bool
			}{{"SET GENERATOR G TO 1", false}},
		},
		{
			name: "comments and empty statement",
			text: "/* only comment */ ; -- x\n",
		},
		{
			name: "quoted semicolon and missing final terminator",
			text: "SELECT 'a;b' FROM A /* ; */; SELECT 2 FROM B",
			want: []struct {
				text    string
				routine bool
			}{{"SELECT 'a;b' FROM A", false}, {"SELECT 2 FROM B", false}},
		},
		{
			name: "byte spans after non-ascii source",
			text: "-- café\nSELECT 1; SELECT 2;",
			want: []struct {
				text    string
				routine bool
			}{{"SELECT 1", false}, {"SELECT 2", false}},
		},
		{
			name: "all routine start forms",
			text: "CREATE OR ALTER PROCEDURE P AS BEGIN X = 1; END; ALTER PROCEDURE Q AS BEGIN X = 1; END; RECREATE TRIGGER R FOR T ACTIVE BEFORE INSERT AS BEGIN X = 1; END;",
			want: []struct {
				text    string
				routine bool
			}{
				{"CREATE OR ALTER PROCEDURE P AS BEGIN X = 1; END", true},
				{"ALTER PROCEDURE Q AS BEGIN X = 1; END", true},
				{"RECREATE TRIGGER R FOR T ACTIVE BEFORE INSERT AS BEGIN X = 1; END", true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ScriptStatements(tt.text, dv)
			if err != nil {
				t.Fatalf("ScriptStatements() error = %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ScriptStatements() returned %d statements, want %d: %#v", len(got), len(tt.want), got)
			}
			for i, want := range tt.want {
				statement := got[i]
				if statement.Span.Start < 0 || statement.Span.End > len(tt.text) || statement.Span.Start > statement.Span.End {
					t.Fatalf("statement %d has invalid span %#v", i, statement.Span)
				}
				if text := tt.text[statement.Span.Start:statement.Span.End]; text != want.text {
					t.Errorf("statement %d text = %q, want %q", i, text, want.text)
				}
				if statement.Routine != want.routine {
					t.Errorf("statement %d Routine = %t, want %t", i, statement.Routine, want.routine)
				}
			}
		})
	}
}

func TestScriptStatementsRejectsUnterminatedStringWithoutPartialResults(t *testing.T) {
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}
	got, err := ScriptStatements("SELECT 'unterminated FROM A", dv)
	if err == nil {
		t.Fatal("ScriptStatements() error = nil, want an unterminated-string error")
	}
	if got != nil {
		t.Fatalf("ScriptStatements() statements = %#v, want nil on error", got)
	}
}

func TestScriptStatementsRejectsUnterminatedBlockComment(t *testing.T) {
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}
	got, err := ScriptStatements("SELECT 1 /* unterminated", dv)
	if err == nil {
		t.Fatal("ScriptStatements() error = nil, want an unterminated-comment error")
	}
	if got != nil {
		t.Fatalf("ScriptStatements() statements = %#v, want nil on error", got)
	}
}

func TestScriptStatementsExposeIdentifierWords(t *testing.T) {
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}
	text := `SELECT "Mixed" FROM tbl`
	statements, err := ScriptStatements(text, dv)
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 1 {
		t.Fatalf("statements = %#v, want one statement", statements)
	}
	var mixed, table bool
	for _, tok := range statements[0].Tokens {
		if tok.Word == "Mixed" && tok.Quoted {
			mixed = true
		}
		if tok.Word == "TBL" && !tok.Quoted {
			table = true
		}
	}
	if !mixed || !table {
		t.Fatalf("identifier tokens = %#v, want quoted Mixed and unquoted TBL", statements[0].Tokens)
	}
}
