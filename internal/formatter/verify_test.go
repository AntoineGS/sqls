package formatter

import (
	"errors"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/lsp"
)

func TestVerifyFormatted(t *testing.T) {
	d := dialect.DialectForDriverVariant(dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3})
	tests := []struct {
		name, original, formatted string
		ok                        bool
	}{
		{"whitespace and keyword case", "select a,b from t where x=1", "SELECT\n  a,\n  b\nFROM\n  t\nWHERE\n  x = 1", true},
		{"punctuation may lose spaces", "insert into t (a) values (1)", "INSERT INTO t(a) VALUES(1)", true},
		{"dropped comma", "values (1, 'x', current_date)", "VALUES(1, 'x' CURRENT_DATE)", false},
		{"joined number and keyword", "order by 2 desc", "ORDER BY 2DESC", false},
		{"joined comment and word", "select /* c */ x from t", "SELECT /* c */x FROM t", false},
		{"identifier case", "select Name from t", "SELECT NAME FROM t", false},
		{"identifier the formatter renders as keyword", "drop table IF exists t", "DROP TABLE if EXISTS t", true},
		{"table named term", "select * from TERM", "select * from term", false},
		{"delimited identifier case", `select "Name" from t`, `SELECT "NAME" FROM t`, false},
		{"string literal", "select 'a b' from t", "SELECT 'a  b' FROM t", false},
		{"comment text", "select 1 -- Note\nfrom t", "SELECT 1 -- note\nFROM t", false},
		{"line comment swallows code", "select 1 -- c\nfrom t", "SELECT 1 -- c FROM t", false},
		{"split operator", "select a <> b from t", "SELECT a < > b FROM t", false},
		{"split concatenation", "select a||b from t", "SELECT a | | b FROM t", false},
		{"split charset introducer", "select _utf8'abc' from t", "SELECT _ utf8'abc' FROM t", false},
		{"spaced placeholder", "select a from t where a=?", "SELECT a FROM t WHERE a = ?", true},
		{"spaced operators", "select a||b, -1 from t where x>=1", "SELECT a || b, - 1 FROM t WHERE x >= 1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyFormatted(tt.original, tt.formatted, d)
			if tt.ok && err != nil {
				t.Fatalf("verifyFormatted() = %v, want nil", err)
			}
			if !tt.ok && !errors.Is(err, ErrUnsafeFormat) {
				t.Fatalf("verifyFormatted() = %v, want ErrUnsafeFormat", err)
			}
		})
	}
}

func TestRecoverFormatPanic(t *testing.T) {
	edits, err := recoverFormat(func() ([]lsp.TextEdit, error) { panic("boom") })
	if edits != nil || !errors.Is(err, ErrUnsafeFormat) {
		t.Fatalf("recoverFormat() = %v, %v; want nil, ErrUnsafeFormat", edits, err)
	}
}
