// Package sqlsymbol contains the source-level building blocks used by SQL
// symbol navigation.
package sqlsymbol

import (
	"errors"
	"io"
	"strings"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/token"
)

// Span is a half-open byte range in the original SQL source.
type Span struct {
	Start int
	End   int
}

// Name is an SQL identifier after decoding its source spelling. Quoted names
// retain their case-sensitive identity; unquoted names use SQL's folded form.
type Name struct {
	Text   string
	Quoted bool
}

// Key returns the catalog and symbol identity key for the name.
func (n Name) Key() string {
	if n.Quoted {
		return n.Text
	}
	return strings.ToUpper(n.Text)
}

// MatchesCatalogName reports whether actual is the catalog spelling for n.
func (n Name) MatchesCatalogName(actual string) bool {
	return n.Key() == actual
}

// Edit replaces Span with NewText in the original source.
type Edit struct {
	Span    Span
	NewText string
}

type lexeme struct {
	Token *token.Token
	Span  Span
}

// lex tokenizes text while retaining exact byte spans into the original text.
// In particular, scanner offsets are used instead of token values because the
// tokenizer intentionally normalizes some newlines and escaped strings.
func lex(text string, dv dialect.DriverVariant) ([]lexeme, error) {
	tokenizer := token.NewTokenizer(strings.NewReader(text), dialect.DialectForDriverVariant(dv))
	result := make([]lexeme, 0)
	for {
		start := tokenizer.Scanner.Pos().Offset
		tok, err := tokenizer.NextToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		end := tokenizer.Scanner.Pos().Offset
		result = append(result, lexeme{Token: tok, Span: Span{Start: start, End: end}})
	}
	return result, nil
}

// nameFromLexeme converts an identifier token to its source-level identity.
// It is deliberately based on the original span: SQLWord.Value has differed
// between dialects for doubled quote escapes over the tokenizer's history.
func nameFromLexeme(text string, item lexeme) (Name, bool) {
	if item.Token == nil || item.Token.Kind != token.SQLKeyword {
		return Name{}, false
	}
	word, ok := item.Token.Value.(*token.SQLWord)
	if !ok {
		return Name{}, false
	}
	if word.QuoteStyle == 0 {
		return Name{Text: word.Value}, true
	}

	raw := text[item.Span.Start:item.Span.End]
	runes := []rune(raw)
	end := matchingEndQuote(word.QuoteStyle)
	if len(runes) >= 2 && runes[0] == word.QuoteStyle && runes[len(runes)-1] == end {
		body := string(runes[1 : len(runes)-1])
		body = strings.ReplaceAll(body, string(end)+string(end), string(end))
		return Name{Text: body, Quoted: true}, true
	}

	// A malformed delimited identifier is returned by the tokenizer as an
	// ordinary SQL word. Keep this fallback defensive for callers that inspect
	// a token stream produced from incomplete editor input.
	return Name{Text: word.Value, Quoted: true}, true
}

func matchingEndQuote(quote rune) rune {
	switch quote {
	case '"', '`':
		return quote
	case '[':
		return ']'
	default:
		return quote
	}
}
