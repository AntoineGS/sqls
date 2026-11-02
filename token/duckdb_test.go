package token

import (
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func TestDuckDBUsesPostgreSQLQuotedSyntax(t *testing.T) {
	d := dialect.DialectForDriverVariant(dialect.DriverVariant{Driver: dialect.DatabaseDriverDuckDB})
	for _, tc := range []struct {
		name       string
		sql        string
		want       string
		identifier bool
	}{
		{name: "doubled identifier quote and spaces", sql: `"odd""name with spaces"`, want: `"odd""name with spaces"`, identifier: true},
		{name: "doubled string quote", sql: `'it''s'`, want: `'it''s'`},
		{name: "escape string prefix", sql: `E'a\nb'`, want: `E'a\nb'`},
		{name: "dollar quoted string", sql: `$$dollar$$`, want: `$$dollar$$`},
		{name: "tagged dollar quoted string", sql: `$body$a'b$body$`, want: `$body$a'b$body$`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokens, err := NewTokenizer(strings.NewReader(tc.sql), d).Tokenize()
			if err != nil {
				t.Fatalf("Tokenize() error = %v", err)
			}
			if len(tokens) != 1 {
				t.Fatalf("Tokenize() produced %d tokens, want 1: %#v", len(tokens), tokens)
			}
			if tc.identifier {
				word, ok := tokens[0].Value.(*SQLWord)
				if !ok || word.QuoteStyle != '"' || word.String() != tc.want {
					t.Fatalf("identifier token = %#v, want a round-trippable quoted SQLWord %q", tokens[0].Value, tc.want)
				}
				return
			}
			if tokens[0].Kind != SingleQuotedString || tokens[0].Value != tc.want {
				t.Fatalf("string token = %#v, want SingleQuotedString %q", tokens[0], tc.want)
			}
		})
	}
}
