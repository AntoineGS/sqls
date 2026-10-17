package formatter

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/token"
)

// ErrUnsafeFormat reports that the formatter failed or produced text that is
// not equivalent to its input. Callers leave the document unchanged.
var ErrUnsafeFormat = errors.New("formatting would change the SQL")

// recoverFormat turns a formatter panic into ErrUnsafeFormat.
func recoverFormat(format func() ([]lsp.TextEdit, error)) (edits []lsp.TextEdit, err error) {
	defer func() {
		if r := recover(); r != nil {
			edits, err = nil, fmt.Errorf("%w: formatter failed: %v", ErrUnsafeFormat, r)
		}
	}()
	return format()
}

type significantToken struct {
	tok *token.Token
	// spaced reports whitespace between this token and the previous one.
	spaced bool
}

// verifyFormatted accepts formatted only if it re-tokenizes to the same tokens
// as original, allowing changes to whitespace and to keyword case only. Where
// original separated two word-like tokens with whitespace, formatted must too,
// so that "2 desc" never becomes "2desc"; adjacent operator symbols and
// unrecognized characters stay adjacent, so that "||" never becomes "| |".
func verifyFormatted(original, formatted string, d dialect.Dialect) error {
	want, err := significantTokens(original, d)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsafeFormat, err)
	}
	got, err := significantTokens(formatted, d)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnsafeFormat, err)
	}
	if len(got) != len(want) {
		return fmt.Errorf("%w: %d tokens became %d", ErrUnsafeFormat, len(want), len(got))
	}
	for i := range want {
		if !sameToken(want[i].tok, got[i].tok) {
			return fmt.Errorf("%w: token %d changed", ErrUnsafeFormat, i)
		}
		if i > 0 && want[i].spaced && !got[i].spaced && wordLike(want[i-1].tok.Kind) && wordLike(want[i].tok.Kind) {
			return fmt.Errorf("%w: tokens %d and %d were joined", ErrUnsafeFormat, i-1, i)
		}
		if i > 0 && !want[i].spaced && got[i].spaced && (operatorLike(want[i-1].tok) && operatorLike(want[i].tok) ||
			opaqueChar(want[i-1].tok) || opaqueChar(want[i].tok)) {
			return fmt.Errorf("%w: tokens %d and %d were split", ErrUnsafeFormat, i-1, i)
		}
	}
	return nil
}

func significantTokens(text string, d dialect.Dialect) ([]significantToken, error) {
	toks, err := token.NewTokenizer(strings.NewReader(text), d).Tokenize()
	if err != nil {
		return nil, err
	}
	result := make([]significantToken, 0, len(toks))
	spaced := false
	for _, tok := range toks {
		if tok.Kind == token.Whitespace {
			spaced = true
			continue
		}
		result = append(result, significantToken{tok: tok, spaced: spaced})
		spaced = false
	}
	return result, nil
}

func sameToken(want, got *token.Token) bool {
	if want.Kind != got.Kind {
		return false
	}
	switch w := want.Value.(type) {
	case *token.SQLWord:
		g, ok := got.Value.(*token.SQLWord)
		if !ok || w.QuoteStyle != g.QuoteStyle {
			return false
		}
		if w.QuoteStyle == 0 && (w.Kind != dialect.Unmatched || slices.Contains(identifierKeywords, strings.ToUpper(w.Value))) {
			return strings.EqualFold(w.Value, g.Value)
		}
		return w.Value == g.Value
	case string:
		g, ok := got.Value.(string)
		return ok && w == g
	default:
		return fmt.Sprint(want.Value) == fmt.Sprint(got.Value)
	}
}

func wordLike(kind token.Kind) bool {
	switch kind {
	case token.SQLKeyword, token.Number, token.Char, token.SingleQuotedString,
		token.NationalStringLiteral, token.Comment, token.MultilineComment:
		return true
	}
	return false
}

// operatorLike reports symbols that may form one operator with an adjacent
// symbol, as the two characters of "||".
func operatorLike(tok *token.Token) bool {
	switch tok.Kind {
	case token.Char:
		// "?" is a parameter placeholder, an operand
		return tok.Value != "?"
	case token.ILLEGAL, token.Eq, token.Neq, token.Lt, token.Gt, token.LtEq, token.GtEq,
		token.Plus, token.Minus, token.Mult, token.Div, token.Caret, token.Mod,
		token.Ampersand, token.Backslash:
		return true
	}
	return false
}

// opaqueChar reports a character the lexer does not recognize, such as the
// "_" of the charset introducer in "_utf8'abc'". Its meaning may depend on
// what it touches, so it is never separated from an adjacent token.
func opaqueChar(tok *token.Token) bool {
	if tok.Kind != token.Char {
		return false
	}
	value, _ := tok.Value.(string)
	return value != "|" && value != "?"
}
