package token

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/k0kubun/pp"

	"github.com/sqls-server/sqls/dialect"
)

func TestTokenizer_Tokenize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		out  []*Token
	}{
		{
			name: "whitespace",
			in:   " ",
			out: []*Token{
				{
					Kind:  Whitespace,
					Value: " ",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 1},
				},
			},
		},
		{
			name: "linebreak",
			in:   "\n",
			out: []*Token{
				{
					Kind:  Whitespace,
					Value: "\n",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 1, Col: 0},
				},
			},
		},
		{
			name: "tab",
			in:   "\t",
			out: []*Token{
				{
					Kind:  Whitespace,
					Value: "\t",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 1},
				},
			},
		},
		{
			name: "whitespace and new line",
			in: `
 `,
			out: []*Token{
				{
					Kind:  Whitespace,
					Value: "\n",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 1, Col: 0},
				},
				{
					Kind:  Whitespace,
					Value: " ",
					From:  Pos{Line: 1, Col: 0},
					To:    Pos{Line: 1, Col: 1},
				},
			},
		},
		{
			name: "whitespace and tab",
			in:   "\r\n	",
			out: []*Token{
				{
					Kind:  Whitespace,
					Value: "\n",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 1, Col: 0},
				},
				{
					Kind:  Whitespace,
					Value: "\t",
					From:  Pos{Line: 1, Col: 0},
					To:    Pos{Line: 1, Col: 1},
				},
			},
		},
		{
			name: "N string",
			in:   "N'string'",
			out: []*Token{
				{
					Kind:  NationalStringLiteral,
					Value: "'string'",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 9},
				},
			},
		},
		{
			name: "N string with keyword",
			in:   "N'string' NOT",
			out: []*Token{
				{
					Kind:  NationalStringLiteral,
					Value: "'string'",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 9},
				},
				{
					Kind:  Whitespace,
					Value: " ",
					From:  Pos{Line: 0, Col: 9},
					To:    Pos{Line: 0, Col: 10},
				},
				{
					Kind: SQLKeyword,
					Value: &SQLWord{
						Value:   "NOT",
						Keyword: "NOT",
						Kind:    dialect.Matched,
					},
					From: Pos{Line: 0, Col: 10},
					To:   Pos{Line: 0, Col: 13},
				},
			},
		},
		{
			name: "Ident",
			in:   "select",
			out: []*Token{
				{
					Kind: SQLKeyword,
					Value: &SQLWord{
						Value:   "select",
						Keyword: "SELECT",
						Kind:    dialect.DML,
					},
					From: Pos{Line: 0, Col: 0},
					To:   Pos{Line: 0, Col: 6},
				},
			},
		},
		{
			name: "Multi line ident",
			in: `select
select
select`,
			out: []*Token{
				{
					Kind: SQLKeyword,
					Value: &SQLWord{
						Value:   "select",
						Keyword: "SELECT",
						Kind:    dialect.DML,
					},
					From: Pos{Line: 0, Col: 0},
					To:   Pos{Line: 0, Col: 6},
				},
				{
					Kind:  Whitespace,
					Value: "\n",
					From:  Pos{Line: 0, Col: 6},
					To:    Pos{Line: 1, Col: 0},
				},
				{
					Kind: SQLKeyword,
					Value: &SQLWord{
						Value:   "select",
						Keyword: "SELECT",
						Kind:    dialect.DML,
					},
					From: Pos{Line: 1, Col: 0},
					To:   Pos{Line: 1, Col: 6},
				},
				{
					Kind:  Whitespace,
					Value: "\n",
					From:  Pos{Line: 1, Col: 6},
					To:    Pos{Line: 2, Col: 0},
				},
				{
					Kind: SQLKeyword,
					Value: &SQLWord{
						Value:   "select",
						Keyword: "SELECT",
						Kind:    dialect.DML,
					},
					From: Pos{Line: 2, Col: 0},
					To:   Pos{Line: 2, Col: 6},
				},
			},
		},
		{
			name: "single quote string",
			in:   "'test'",
			out: []*Token{
				{
					Kind:  SingleQuotedString,
					Value: "'test'",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 6},
				},
			},
		},
		{
			name: "single quoted string with escaped quote",
			in:   "'a''b',",
			out: []*Token{
				{
					Kind:  SingleQuotedString,
					Value: "'a'b'",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 6},
				},
				{
					Kind:  Comma,
					Value: ",",
					From:  Pos{Line: 0, Col: 6},
					To:    Pos{Line: 0, Col: 7},
				},
			},
		},
		{
			name: "quoted string",
			in:   `"SELECT"`,
			out: []*Token{
				{
					Kind: SQLKeyword,
					Value: &SQLWord{
						Value:      "SELECT",
						Keyword:    "SELECT",
						QuoteStyle: '"',
						Kind:       dialect.Unmatched,
					},
					From: Pos{Line: 0, Col: 0},
					To:   Pos{Line: 0, Col: 8},
				},
			},
		},
		{
			name: "parent identifier",
			in:   "foo.bar",
			out: []*Token{
				{
					Kind: SQLKeyword,
					Value: &SQLWord{
						Value:   "foo",
						Keyword: "FOO",
						Kind:    dialect.Unmatched,
					},
					From: Pos{Line: 0, Col: 0},
					To:   Pos{Line: 0, Col: 3},
				},
				{
					Kind:  Period,
					Value: ".",
					From:  Pos{Line: 0, Col: 3},
					To:    Pos{Line: 0, Col: 4},
				},
				{
					Kind: SQLKeyword,
					Value: &SQLWord{
						Value:   "bar",
						Keyword: "BAR",
						Kind:    dialect.Unmatched,
					},
					From: Pos{Line: 0, Col: 4},
					To:   Pos{Line: 0, Col: 7},
				},
			},
		},
		{
			name: "parent identifier with double quote",
			in:   `"foo"."bar"`,
			out: []*Token{
				{
					Kind: SQLKeyword,
					Value: &SQLWord{
						Value:      "foo",
						Keyword:    "FOO",
						QuoteStyle: '"',
						Kind:       dialect.Unmatched,
					},
					From: Pos{Line: 0, Col: 0},
					To:   Pos{Line: 0, Col: 5},
				},
				{
					Kind:  Period,
					Value: ".",
					From:  Pos{Line: 0, Col: 5},
					To:    Pos{Line: 0, Col: 6},
				},
				{
					Kind: SQLKeyword,
					Value: &SQLWord{
						Value:      "bar",
						Keyword:    "BAR",
						QuoteStyle: '"',
						Kind:       dialect.Unmatched,
					},
					From: Pos{Line: 0, Col: 6},
					To:   Pos{Line: 0, Col: 11},
				},
			},
		},
		{
			name: "parent identifier with back quote",
			in:   "`foo`.`bar`",
			out: []*Token{
				{
					Kind: SQLKeyword,
					Value: &SQLWord{
						Value:      "foo",
						Keyword:    "FOO",
						QuoteStyle: '`',
						Kind:       dialect.Unmatched,
					},
					From: Pos{Line: 0, Col: 0},
					To:   Pos{Line: 0, Col: 5},
				},
				{
					Kind:  Period,
					Value: ".",
					From:  Pos{Line: 0, Col: 5},
					To:    Pos{Line: 0, Col: 6},
				},
				{
					Kind: SQLKeyword,
					Value: &SQLWord{
						Value:      "bar",
						Keyword:    "BAR",
						QuoteStyle: '`',
						Kind:       dialect.Unmatched,
					},
					From: Pos{Line: 0, Col: 6},
					To:   Pos{Line: 0, Col: 11},
				},
			},
		},
		{
			name: "non closed single quote identifier",
			in:   "'foo",
			out: []*Token{
				{
					Kind:  SingleQuotedString,
					Value: "'foo",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 4},
				},
			},
		},
		{
			name: "non closed double quote identifier",
			in:   `"foo`,
			out: []*Token{
				{
					Kind: SQLKeyword,
					Value: &SQLWord{
						Value:      `"foo`,
						Keyword:    `"FOO`,
						QuoteStyle: 0,
						Kind:       dialect.Unmatched,
					},
					From: Pos{Line: 0, Col: 0},
					To:   Pos{Line: 0, Col: 4},
				},
			},
		},
		{
			name: "non closed back quote identifier",
			in:   "`foo bar",
			out: []*Token{
				{
					Kind: SQLKeyword,
					Value: &SQLWord{
						Value:      "`foo",
						Keyword:    "`FOO",
						QuoteStyle: 0,
						Kind:       dialect.Unmatched,
					},
					From: Pos{Line: 0, Col: 0},
					To:   Pos{Line: 0, Col: 4},
				},
				{
					Kind:  Whitespace,
					Value: " ",
					From:  Pos{Line: 0, Col: 4},
					To:    Pos{Line: 0, Col: 5},
				},
				{
					Kind: SQLKeyword,
					Value: &SQLWord{
						Value:      "bar",
						Keyword:    "BAR",
						QuoteStyle: 0,
						Kind:       dialect.Unmatched,
					},
					From: Pos{Line: 0, Col: 5},
					To:   Pos{Line: 0, Col: 8},
				},
			},
		},
		{
			name: "parents with number",
			in:   "(123),",
			out: []*Token{
				{
					Kind:  LParen,
					Value: "(",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 1},
				},
				{
					Kind:  Number,
					Value: "123",
					From:  Pos{Line: 0, Col: 1},
					To:    Pos{Line: 0, Col: 4},
				},
				{
					Kind:  RParen,
					Value: ")",
					From:  Pos{Line: 0, Col: 4},
					To:    Pos{Line: 0, Col: 5},
				},
				{
					Kind:  Comma,
					Value: ",",
					From:  Pos{Line: 0, Col: 5},
					To:    Pos{Line: 0, Col: 6},
				},
			},
		},
		{
			name: "minus comment",
			in:   "-- test",
			out: []*Token{
				{
					Kind:  Comment,
					Value: " test",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 7},
				},
			},
		},
		{
			name: "minus operator",
			in:   "1-3",
			out: []*Token{
				{
					Kind:  Number,
					Value: "1",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 1},
				},
				{
					Kind:  Minus,
					Value: "-",
					From:  Pos{Line: 0, Col: 1},
					To:    Pos{Line: 0, Col: 2},
				},
				{
					Kind:  Number,
					Value: "3",
					From:  Pos{Line: 0, Col: 2},
					To:    Pos{Line: 0, Col: 3},
				},
			},
		},
		{
			name: "/* comment",
			in: `/* test
multiline
comment */`,
			out: []*Token{
				{
					Kind:  MultilineComment,
					Value: " test\nmultiline\ncomment ",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 2, Col: 10},
				},
			},
		},
		{
			name: "/* comment with asterisk",
			in:   `/* 1*2 */`,
			out: []*Token{
				{
					Kind:  MultilineComment,
					Value: " 1*2 ",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 9},
				},
			},
		},
		{
			name: "operators",
			in:   "1/1*1+1%1=1.1-.",
			out: []*Token{
				{
					Kind:  Number,
					Value: "1",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 1},
				},
				{
					Kind:  Div,
					Value: "/",
					From:  Pos{Line: 0, Col: 1},
					To:    Pos{Line: 0, Col: 2},
				},
				{
					Kind:  Number,
					Value: "1",
					From:  Pos{Line: 0, Col: 2},
					To:    Pos{Line: 0, Col: 3},
				},
				{
					Kind:  Mult,
					Value: "*",
					From:  Pos{Line: 0, Col: 3},
					To:    Pos{Line: 0, Col: 4},
				},
				{
					Kind:  Number,
					Value: "1",
					From:  Pos{Line: 0, Col: 4},
					To:    Pos{Line: 0, Col: 5},
				},
				{
					Kind:  Plus,
					Value: "+",
					From:  Pos{Line: 0, Col: 5},
					To:    Pos{Line: 0, Col: 6},
				},
				{
					Kind:  Number,
					Value: "1",
					From:  Pos{Line: 0, Col: 6},
					To:    Pos{Line: 0, Col: 7},
				},
				{
					Kind:  Mod,
					Value: "%",
					From:  Pos{Line: 0, Col: 7},
					To:    Pos{Line: 0, Col: 8},
				},
				{
					Kind:  Number,
					Value: "1",
					From:  Pos{Line: 0, Col: 8},
					To:    Pos{Line: 0, Col: 9},
				},
				{
					Kind:  Eq,
					Value: "=",
					From:  Pos{Line: 0, Col: 9},
					To:    Pos{Line: 0, Col: 10},
				},
				{
					Kind:  Number,
					Value: "1.1",
					From:  Pos{Line: 0, Col: 10},
					To:    Pos{Line: 0, Col: 13},
				},
				{
					Kind:  Minus,
					Value: "-",
					From:  Pos{Line: 0, Col: 13},
					To:    Pos{Line: 0, Col: 14},
				},
				{
					Kind:  Period,
					Value: ".",
					From:  Pos{Line: 0, Col: 14},
					To:    Pos{Line: 0, Col: 15},
				},
			},
		},
		{
			name: "Neq",
			in:   "1!=2",
			out: []*Token{
				{
					Kind:  Number,
					Value: "1",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 1},
				},
				{
					Kind:  Neq,
					Value: "!=",
					From:  Pos{Line: 0, Col: 1},
					To:    Pos{Line: 0, Col: 3},
				},
				{
					Kind:  Number,
					Value: "2",
					From:  Pos{Line: 0, Col: 3},
					To:    Pos{Line: 0, Col: 4},
				},
			},
		},
		{
			name: "Lts",
			in:   "<<=<>",
			out: []*Token{
				{
					Kind:  Lt,
					Value: "<",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 1},
				},
				{
					Kind:  LtEq,
					Value: "<=",
					From:  Pos{Line: 0, Col: 1},
					To:    Pos{Line: 0, Col: 3},
				},
				{
					Kind:  Neq,
					Value: "<>",
					From:  Pos{Line: 0, Col: 3},
					To:    Pos{Line: 0, Col: 5},
				},
			},
		},
		{
			name: "Gts",
			in:   ">>=",
			out: []*Token{
				{
					Kind:  Gt,
					Value: ">",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 1},
				},
				{
					Kind:  GtEq,
					Value: ">=",
					From:  Pos{Line: 0, Col: 1},
					To:    Pos{Line: 0, Col: 3},
				},
			},
		},
		{
			name: "colons",
			in:   ":1::1;",
			out: []*Token{
				{
					Kind:  Colon,
					Value: ":",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 1},
				},
				{
					Kind:  Number,
					Value: "1",
					From:  Pos{Line: 0, Col: 1},
					To:    Pos{Line: 0, Col: 2},
				},
				{
					Kind:  DoubleColon,
					Value: "::",
					From:  Pos{Line: 0, Col: 2},
					To:    Pos{Line: 0, Col: 4},
				},
				{
					Kind:  Number,
					Value: "1",
					From:  Pos{Line: 0, Col: 4},
					To:    Pos{Line: 0, Col: 5},
				},
				{
					Kind:  Semicolon,
					Value: ";",
					From:  Pos{Line: 0, Col: 5},
					To:    Pos{Line: 0, Col: 6},
				},
			},
		},
		{
			name: "others",
			in:   "\\[{&}]",
			out: []*Token{
				{
					Kind:  Backslash,
					Value: "\\",
					From:  Pos{Line: 0, Col: 0},
					To:    Pos{Line: 0, Col: 1},
				},
				{
					Kind:  LBracket,
					Value: "[",
					From:  Pos{Line: 0, Col: 1},
					To:    Pos{Line: 0, Col: 2},
				},
				{
					Kind:  LBrace,
					Value: "{",
					From:  Pos{Line: 0, Col: 2},
					To:    Pos{Line: 0, Col: 3},
				},
				{
					Kind:  Ampersand,
					Value: "&",
					From:  Pos{Line: 0, Col: 3},
					To:    Pos{Line: 0, Col: 4},
				},
				{
					Kind:  RBrace,
					Value: "}",
					From:  Pos{Line: 0, Col: 4},
					To:    Pos{Line: 0, Col: 5},
				},
				{
					Kind:  RBracket,
					Value: "]",
					From:  Pos{Line: 0, Col: 5},
					To:    Pos{Line: 0, Col: 6},
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := strings.NewReader(c.in)
			tokenizer := NewTokenizer(src, &dialect.GenericSQLDialect{})

			tok, err := tokenizer.Tokenize()
			if err != nil {
				t.Errorf("should be no error %v", err)
			}

			if len(tok) != len(c.out) {
				t.Fatalf("should be same length but want %d, got %d, want%s,got%s", len(tok), len(c.out), pp.Sprint(tok), pp.Sprint(c.out))
			}

			for i := 0; i < len(tok); i++ {
				if tok[i].Kind != c.out[i].Kind {
					t.Errorf("%d, expected sqltoken: %d, but got %d", i, c.out[i].Kind, tok[i].Kind)
				}
				if d := cmp.Diff(tok[i], c.out[i]); d != "" {
					t.Errorf("%d, unmatched Value(+want -got): %s", i, d)
				}
			}
		})
	}
}

func TestTokenizer_Pos(t *testing.T) {
	t.Run("operators", func(t *testing.T) {
		cases := []struct {
			operator string
			add      int
		}{
			{
				operator: "+",
			},
			{
				operator: "-",
			},
			{
				operator: "%",
			},
			{
				operator: "*",
			},
			{
				operator: "/",
			},
			{
				operator: ">",
			},
			{
				operator: "=",
			},
			{
				operator: "<",
			},
			{
				operator: "<=",
				add:      1,
			},
			{
				operator: "<>",
				add:      1,
			},
			{
				operator: ">=",
				add:      1,
			},
		}

		for _, c := range cases {
			t.Run(c.operator, func(t *testing.T) {
				src := fmt.Sprintf("1 %s 1", c.operator)
				tokenizer := NewTokenizer(bytes.NewBufferString(src), &dialect.GenericSQLDialect{})

				if _, err := tokenizer.Tokenize(); err != nil {
					t.Fatal(err)
				}

				if d := cmp.Diff(tokenizer.Pos(), Pos{Line: 0, Col: 5 + c.add}); d != "" {
					t.Errorf("must be same but diff: %s", d)
				}
			})
		}
	})
	t.Run("other expressions", func(t *testing.T) {
		cases := []struct {
			name   string
			src    string
			expect Pos
		}{
			{
				name: "multiline",
				src: `1+1
asdf`,
				expect: Pos{Line: 1, Col: 4},
			},
			{
				name:   "single line comment",
				src:    `-- comments`,
				expect: Pos{Line: 0, Col: 11},
			},
			{
				name:   "statements",
				src:    `select count(id) from account`,
				expect: Pos{Line: 0, Col: 29},
			},
			{
				name: "multiline statements",
				src: `select count(id)
from account 
where name like '%test%'`,
				expect: Pos{Line: 2, Col: 24},
			},
			{
				name: "multiline comment",
				src: `/*
test comment
test comment
*/`,
				expect: Pos{Line: 3, Col: 2},
			},
			{
				name:   "single line comment",
				src:    "/* asdf */",
				expect: Pos{Line: 0, Col: 10},
			},
			{
				name:   "comment inside sql",
				src:    "select * from /* test table */ test_table where id != 123",
				expect: Pos{Line: 0, Col: 57},
			},
		}

		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				tokenizer := NewTokenizer(bytes.NewBufferString(c.src), &dialect.GenericSQLDialect{})

				if _, err := tokenizer.Tokenize(); err != nil {
					t.Fatal(err)
				}

				if d := cmp.Diff(tokenizer.Pos(), c.expect); d != "" {
					t.Errorf("must be same but diff: %s", d)
				}
			})
		}
	})

	t.Run("illegal cases", func(t *testing.T) {
		cases := []struct {
			name string
			src  string
		}{
			{
				name: "unclosed multiline comment",
				src: `
/* test
test
`,
			},
		}

		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				tokenizer := NewTokenizer(bytes.NewBufferString(c.src), &dialect.GenericSQLDialect{})

				_, err := tokenizer.Tokenize()
				if err == nil {
					t.Errorf("must be error but blank")
				}
				t.Logf("%+v", err)

			})
		}
	})
}

func TestTokenizer_InterBaseDialect1(t *testing.T) {
	src := `"a""b" 'c''d' RDB$DATABASE ?`
	tokenizer := NewTokenizer(bytes.NewBufferString(src), &dialect.InterBaseDialect{SQLDialect: 1})

	tokens, err := tokenizer.Tokenize()
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 7 {
		t.Fatalf("got %d tokens, want 7: %s", len(tokens), pp.Sprint(tokens))
	}

	if got, want := tokens[0].Kind, SingleQuotedString; got != want {
		t.Fatalf("double-quoted Dialect 1 string kind = %v, want %v", got, want)
	}
	if got, want := tokens[0].Value, `"a""b"`; got != want {
		t.Fatalf("double-quoted Dialect 1 string value = %q, want %q", got, want)
	}
	if got, want := tokens[0].To, (Pos{Line: 0, Col: 6}); got != want {
		t.Fatalf("double-quoted Dialect 1 string end = %v, want %v", got, want)
	}

	if got, want := tokens[2].Kind, SingleQuotedString; got != want {
		t.Fatalf("single-quoted Dialect 1 string kind = %v, want %v", got, want)
	}
	if got, want := tokens[2].Value, `'c''d'`; got != want {
		t.Fatalf("single-quoted Dialect 1 string value = %q, want %q", got, want)
	}

	word, ok := tokens[4].Value.(*SQLWord)
	if !ok {
		t.Fatalf("RDB$DATABASE token value = %T, want *SQLWord", tokens[4].Value)
	}
	if got, want := word.Value, "RDB$DATABASE"; got != want {
		t.Errorf("RDB$DATABASE value = %q, want %q", got, want)
	}
	if got, want := word.Keyword, "RDB$DATABASE"; got != want {
		t.Errorf("RDB$DATABASE keyword = %q, want %q", got, want)
	}
	if got, want := word.Kind, dialect.KeywordKind(dialect.Unmatched); got != want {
		t.Errorf("RDB$DATABASE keyword kind = %v, want %v", got, want)
	}

	if got, want := tokens[6].Kind, Char; got != want {
		t.Errorf("positional placeholder kind = %v, want %v", got, want)
	}
	if got, want := tokens[6].Value, "?"; got != want {
		t.Errorf("positional placeholder value = %q, want %q", got, want)
	}
}

func TestTokenizer_InterBaseDialect3(t *testing.T) {
	src := `"a""b" 'c''d' RDB$DATABASE ?`
	tokenizer := NewTokenizer(bytes.NewBufferString(src), &dialect.InterBaseDialect{SQLDialect: 3})

	tokens, err := tokenizer.Tokenize()
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 7 {
		t.Fatalf("got %d tokens, want 7: %s", len(tokens), pp.Sprint(tokens))
	}

	word, ok := tokens[0].Value.(*SQLWord)
	if !ok {
		t.Fatalf("double-quoted Dialect 3 token value = %T, want *SQLWord", tokens[0].Value)
	}
	if got, want := tokens[0].Kind, SQLKeyword; got != want {
		t.Errorf("double-quoted Dialect 3 token kind = %v, want %v", got, want)
	}
	if got, want := word.Value, `a""b`; got != want {
		t.Errorf("delimited identifier value = %q, want %q", got, want)
	}
	if got, want := word.QuoteStyle, '"'; got != want {
		t.Errorf("delimited identifier quote style = %q, want %q", got, want)
	}
	if got, want := word.String(), `"a""b"`; got != want {
		t.Errorf("delimited identifier reprint = %q, want %q", got, want)
	}
	if got, want := tokens[0].To, (Pos{Line: 0, Col: 6}); got != want {
		t.Errorf("delimited identifier end = %v, want %v", got, want)
	}

	// The escape-preservation guard: a single-quoted string must survive
	// unchanged under Dialect 3 even though '"' now delimits identifiers.
	if got, want := tokens[2].Kind, SingleQuotedString; got != want {
		t.Errorf("single-quoted Dialect 3 string kind = %v, want %v", got, want)
	}
	if got, want := tokens[2].Value, `'c''d'`; got != want {
		t.Errorf("single-quoted Dialect 3 string value = %q, want %q", got, want)
	}

	catalog, ok := tokens[4].Value.(*SQLWord)
	if !ok {
		t.Fatalf("RDB$DATABASE token value = %T, want *SQLWord", tokens[4].Value)
	}
	if got, want := catalog.Value, "RDB$DATABASE"; got != want {
		t.Errorf("RDB$DATABASE value = %q, want %q", got, want)
	}
	if got, want := tokens[6].Value, "?"; got != want {
		t.Errorf("positional placeholder value = %q, want %q", got, want)
	}
}

// TestTokenizeQuotedStringEscapePreservation is the spec's named regression
// guard: only the generic dialect decodes a doubled quote. Both InterBase
// dialects keep the source spelling, because the formatter reprints tokens.
func TestTokenizeQuotedStringEscapePreservation(t *testing.T) {
	tests := []struct {
		name    string
		dialect dialect.Dialect
		want    string
	}{
		{name: "generic decodes", dialect: &dialect.GenericSQLDialect{}, want: `'c'd'`},
		{name: "interbase dialect 1 preserves", dialect: &dialect.InterBaseDialect{SQLDialect: 1}, want: `'c''d'`},
		{name: "interbase dialect 3 preserves", dialect: &dialect.InterBaseDialect{SQLDialect: 3}, want: `'c''d'`},
		{name: "interbase zero value preserves", dialect: &dialect.InterBaseDialect{}, want: `'c''d'`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens, err := NewTokenizer(bytes.NewBufferString(`'c''d'`), tt.dialect).Tokenize()
			if err != nil {
				t.Fatal(err)
			}
			if len(tokens) != 1 {
				t.Fatalf("got %d tokens, want 1: %s", len(tokens), pp.Sprint(tokens))
			}
			if got := tokens[0].Value; got != tt.want {
				t.Fatalf("token value = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTokenizer_GenericDoubleQuotesRemainDelimitedIdentifiers(t *testing.T) {
	tokenizer := NewTokenizer(bytes.NewBufferString(`"name"`), &dialect.GenericSQLDialect{})

	tokens, err := tokenizer.Tokenize()
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 {
		t.Fatalf("got %d tokens, want 1", len(tokens))
	}
	word, ok := tokens[0].Value.(*SQLWord)
	if !ok || word.QuoteStyle != '"' {
		t.Fatalf("generic double-quoted token = %#v, want a delimited SQLWord", tokens[0].Value)
	}
}

// optionalHookDialect is a generic dialect that opts into the optional lexer
// interfaces, so the hooks can be tested without depending on any real dialect.
type optionalHookDialect struct {
	dialect.GenericSQLDialect
	preserve  bool
	scanWhole bool
}

func (d *optionalHookDialect) PreservesQuotedStringEscapes() bool  { return d.preserve }
func (d *optionalHookDialect) ScansWholeDelimitedIdentifier() bool { return d.scanWhole }

func TestTokenizerOptionalQuotedStringEscapePreserver(t *testing.T) {
	tests := []struct {
		name    string
		dialect dialect.Dialect
		src     string
		want    string
	}{
		{
			name:    "generic dialect decodes the doubled quote",
			dialect: &dialect.GenericSQLDialect{},
			src:     `'c''d'`,
			want:    `'c'd'`,
		},
		{
			name:    "opting out matches the generic dialect",
			dialect: &optionalHookDialect{preserve: false},
			src:     `'c''d'`,
			want:    `'c'd'`,
		},
		{
			name:    "opting in preserves the source spelling",
			dialect: &optionalHookDialect{preserve: true},
			src:     `'c''d'`,
			want:    `'c''d'`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens, err := NewTokenizer(bytes.NewBufferString(tt.src), tt.dialect).Tokenize()
			if err != nil {
				t.Fatal(err)
			}
			if len(tokens) != 1 {
				t.Fatalf("got %d tokens, want 1: %s", len(tokens), pp.Sprint(tokens))
			}
			if got, want := tokens[0].Kind, SingleQuotedString; got != want {
				t.Fatalf("token kind = %v, want %v", got, want)
			}
			if got := tokens[0].Value; got != tt.want {
				t.Fatalf("token value = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTokenizerOptionalDelimitedIdentifierScanner(t *testing.T) {
	tests := []struct {
		name       string
		scanWhole  bool
		src        string
		wantValues []string
		wantQuote  rune
	}{
		{
			name:       "opting out stops at a space, as today",
			scanWhole:  false,
			src:        `"My Column"`,
			wantValues: []string{`"My`, "Column", `"`},
			wantQuote:  0,
		},
		{
			name:       "opting in scans the whole identifier",
			scanWhole:  true,
			src:        `"My Column"`,
			wantValues: []string{"My Column"},
			wantQuote:  '"',
		},
		{
			name:       "opting in keeps a doubled quote in the identifier",
			scanWhole:  true,
			src:        `"a""b"`,
			wantValues: []string{`a""b`},
			wantQuote:  '"',
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &optionalHookDialect{scanWhole: tt.scanWhole}
			tokens, err := NewTokenizer(bytes.NewBufferString(tt.src), d).Tokenize()
			if err != nil {
				t.Fatal(err)
			}

			var got []string
			for _, tok := range tokens {
				if tok.Kind == Whitespace {
					continue
				}
				word, ok := tok.Value.(*SQLWord)
				if !ok {
					t.Fatalf("token value = %T, want *SQLWord: %s", tok.Value, pp.Sprint(tokens))
				}
				got = append(got, word.Value)
			}
			if !cmp.Equal(got, tt.wantValues) {
				t.Fatalf("identifier values = %#v, want %#v", got, tt.wantValues)
			}

			first, ok := tokens[0].Value.(*SQLWord)
			if !ok {
				t.Fatalf("first token value = %T, want *SQLWord", tokens[0].Value)
			}
			if got, want := first.QuoteStyle, tt.wantQuote; got != want {
				t.Fatalf("first token quote style = %q, want %q", got, want)
			}
		})
	}
}

func TestTokenizerUnclosedDelimitedIdentifierStopsAtNewline(t *testing.T) {
	d := &optionalHookDialect{scanWhole: true}
	tokens, err := NewTokenizer(bytes.NewBufferString("\"abc\nfrom"), d).Tokenize()
	if err != nil {
		t.Fatal(err)
	}
	word, ok := tokens[0].Value.(*SQLWord)
	if !ok {
		t.Fatalf("token value = %T, want *SQLWord", tokens[0].Value)
	}
	if got, want := word.Value, `"abc`; got != want {
		t.Fatalf("unclosed identifier value = %q, want %q", got, want)
	}
	if got, want := word.QuoteStyle, rune(0); got != want {
		t.Fatalf("unclosed identifier quote style = %q, want %q", got, want)
	}
	if got, want := tokens[1].Value, "\n"; got != want {
		t.Fatalf("token after unclosed identifier = %#v, want %q", tokens[1].Value, want)
	}
}
