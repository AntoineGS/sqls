package sqlsymbol

import (
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func TestStringTypeWidth(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		want int
		ok   bool
	}{
		{name: "varchar", typ: "VARCHAR(40)", want: 40, ok: true},
		{name: "char with character set", typ: "CHAR(20) CHARACTER SET UTF8", want: 20, ok: true},
		{name: "blob is unbounded", typ: "BLOB", ok: false},
		{name: "malformed width", typ: "VARCHAR(nope)", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := stringTypeWidth(tt.typ)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("stringTypeWidth(%q) = %d, %v; want %d, %v", tt.typ, got, ok, tt.want, tt.ok)
			}
		})
	}
}

type widthTestCatalog map[string][]ColumnType

func (c widthTestCatalog) Columns(table Name) ([]ColumnType, bool) {
	columns, ok := c[table.Key()]
	return columns, ok
}

func TestExpressionWidth(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		expr      string
		catalog   Catalog
		wantWidth int
		wantOK    bool
	}{
		{
			name: "literal counts decoded unicode characters",
			text: `CREATE PROCEDURE P AS BEGIN RESULT = 'a''😀'; END`,
			expr: `'a''😀'`, wantWidth: 3, wantOK: true,
		},
		{
			name: "concatenation adds known variable widths",
			text: `CREATE PROCEDURE P (X VARCHAR(3), Y VARCHAR(4)) AS BEGIN RESULT = X || Y; END`,
			expr: `X || Y`, wantWidth: 7, wantOK: true,
		},
		{
			name: "cast uses its declared result bound",
			text: `CREATE PROCEDURE P (X VARCHAR(40)) AS BEGIN RESULT = CAST(X AS VARCHAR(7)); END`,
			expr: `CAST(X AS VARCHAR(7))`, wantWidth: 7, wantOK: true,
		},
		{
			name: "substring is bounded by literal length",
			text: `CREATE PROCEDURE P (X VARCHAR(40)) AS BEGIN RESULT = SUBSTRING(X FROM 1 FOR 5); END`,
			expr: `SUBSTRING(X FROM 1 FOR 5)`, wantWidth: 5, wantOK: true,
		},
		{
			name: "trim propagates input bound",
			text: `CREATE PROCEDURE P (X VARCHAR(12)) AS BEGIN RESULT = TRIM(X); END`,
			expr: `TRIM(X)`, wantWidth: 12, wantOK: true,
		},
		{
			name: "unknown function stays unknown",
			text: `CREATE PROCEDURE P (X VARCHAR(12)) AS BEGIN RESULT = UNKNOWN_FN(X); END`,
			expr: `UNKNOWN_FN(X)`, wantOK: false,
		},
		{
			name:      "select source alias and quoted identifiers resolve",
			text:      `CREATE PROCEDURE P AS BEGIN SELECT T."Mixed" FROM "Src" AS T; END`,
			expr:      `T."Mixed"`,
			catalog:   widthTestCatalog{"Src": {{Name: "Mixed", Type: "VARCHAR(9)"}}},
			wantWidth: 9, wantOK: true,
		},
		{
			name: "ambiguous unqualified SQL column stays unknown",
			text: `CREATE PROCEDURE P AS BEGIN SELECT VALUE FROM SRC A JOIN OTHER B ON A.ID = B.ID; END`,
			expr: `VALUE`,
			catalog: widthTestCatalog{
				"SRC":   {{Name: "VALUE", Type: "VARCHAR(9)"}, {Name: "ID", Type: "INTEGER"}},
				"OTHER": {{Name: "VALUE", Type: "VARCHAR(11)"}, {Name: "ID", Type: "INTEGER"}},
			},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := Analyze(tt.text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
			if err != nil {
				t.Fatal(err)
			}
			start := strings.Index(tt.text, tt.expr)
			if start < 0 {
				t.Fatalf("expression %q not found", tt.expr)
			}
			items := significantLexemes(a.lexemes)
			exprItems := make([]lexeme, 0)
			for _, item := range items {
				if item.Span.Start >= start && item.Span.End <= start+len(tt.expr) {
					exprItems = append(exprItems, item)
				}
			}
			gotWidth, gotOK := a.expressionWidth(exprItems, tt.catalog)
			if gotWidth != tt.wantWidth || gotOK != tt.wantOK {
				t.Fatalf("expressionWidth(%q) = %d, %v; want %d, %v", tt.expr, gotWidth, gotOK, tt.wantWidth, tt.wantOK)
			}
		})
	}
}
