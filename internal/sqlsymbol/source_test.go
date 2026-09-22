package sqlsymbol

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/token"
)

func TestLexPreservesOriginalSpans(t *testing.T) {
	text := "/* 😀 */\r\namountpaid = :amountpaid;"
	dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase,
		Variant: dialect.SQLVariantInterBase1}
	tokens, err := lex(text, dv)
	if err != nil {
		t.Fatal(err)
	}
	var got []Span
	for _, tok := range tokens {
		if text[tok.Span.Start:tok.Span.End] == "amountpaid" {
			got = append(got, tok.Span)
		}
	}
	first, last := strings.Index(text, "amountpaid"), strings.LastIndex(text, "amountpaid")
	want := []Span{{first, first + 10}, {last, last + 10}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatal(diff)
	}
}

func TestNameIdentity(t *testing.T) {
	if (Name{Text: "foo"}).Key() != (Name{Text: "FOO", Quoted: true}).Key() {
		t.Fatal("unquoted foo must match quoted uppercase FOO")
	}
	if (Name{Text: "foo"}).Key() == (Name{Text: "foo", Quoted: true}).Key() {
		t.Fatal("quoted lowercase foo is distinct")
	}
}

func TestLexDialectSensitiveTokensAndOriginalSpelling(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		dv         dialect.DriverVariant
		kind       token.Kind
		wantSource string
		wantName   Name
		wantNameOK bool
	}{
		{
			name:       "doubled identifier quote",
			text:       `"a""b"`,
			dv:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			kind:       token.SQLKeyword,
			wantSource: `"a""b"`,
			wantName:   Name{Text: `a"b`, Quoted: true},
			wantNameOK: true,
		},
		{
			name:       "single quoted escape",
			text:       `'a''b'`,
			dv:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			kind:       token.SingleQuotedString,
			wantSource: `'a''b'`,
		},
		{
			name:       "dialect 1 double quoted string",
			text:       `"a""b"`,
			dv:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase1},
			kind:       token.SingleQuotedString,
			wantSource: `"a""b"`,
		},
		{
			name:       "tab",
			text:       "\tamount",
			dv:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			kind:       token.Whitespace,
			wantSource: "\t",
		},
		{
			name:       "multiline string",
			text:       "'first\nsecond'",
			dv:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			kind:       token.SingleQuotedString,
			wantSource: "'first\nsecond'",
		},
		{
			name:       "comment",
			text:       "/* amount */",
			dv:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			kind:       token.MultilineComment,
			wantSource: "/* amount */",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := lex(tt.text, tt.dv)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 {
				t.Fatal("lex returned no tokens")
			}
			var found bool
			for _, item := range got {
				if item.Token.Kind != tt.kind {
					continue
				}
				found = true
				if source := tt.text[item.Span.Start:item.Span.End]; source != tt.wantSource {
					t.Fatalf("source span = %q, want %q", source, tt.wantSource)
				}
				name, ok := nameFromLexeme(tt.text, item)
				if ok != tt.wantNameOK || (ok && name != tt.wantName) {
					t.Fatalf("name = %#v, %t; want %#v, %t", name, ok, tt.wantName, tt.wantNameOK)
				}
				break
			}
			if !found {
				t.Fatalf("lex did not produce token kind %s", tt.kind)
			}
		})
	}
}
