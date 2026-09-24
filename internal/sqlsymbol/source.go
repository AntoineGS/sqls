// Package sqlsymbol contains the source-level building blocks used by SQL
// symbol navigation.
package sqlsymbol

import (
	"errors"
	"fmt"
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
	// InterBase's SET TERM directive changes the delimiter used by a script.
	// Split at that delimiter before invoking the SQL tokenizer: a delimiter
	// such as !! is not a legal standalone SQL token, while it is perfectly
	// valid in an InterBase script.
	active := ";"
	result := make([]lexeme, 0)
	for offset := 0; offset < len(text); {
		end, delimiter, err := nextDelimiter(text, offset, active)
		if err != nil {
			return nil, err
		}
		segment := text[offset:end]
		next, isDirective := setTermDelimiter(segment)
		if isDirective {
			active = next
		} else {
			items, err := lexRaw(segment, dv)
			if err != nil {
				return nil, err
			}
			for _, item := range items {
				item.Span.Start += offset
				item.Span.End += offset
				result = append(result, item)
			}
		}
		if delimiter == "" {
			break
		}
		// Keep ordinary semicolons visible to existing source consumers. Custom
		// script terminators are deliberately intercepted instead of being sent
		// to the SQL lexer.
		if delimiter == ";" && !isDirective {
			result = append(result, lexeme{
				Token: &token.Token{Kind: token.Semicolon, Value: delimiter},
				Span:  Span{Start: end, End: end + len(delimiter)},
			})
		}
		offset = end + len(delimiter)
	}
	return result, nil
}

// scriptDelimiterOffsets returns, in document order, the byte offset where
// each SET TERM custom (non-";") statement delimiter begins. lex() consumes
// a custom delimiter without producing any lexeme for it at all (only
// ordinary ";" boundaries are represented as Semicolon lexemes), so
// diagnostic statement-boundary detection cannot see those boundaries
// without this separate, lightweight mirror of lex()'s own segment
// splitting. It changes nothing about what lex() returns to its other
// callers (navigation, rename).
func scriptDelimiterOffsets(text string) []int {
	active := ";"
	var offsets []int
	for offset := 0; offset < len(text); {
		end, delimiter, err := nextDelimiter(text, offset, active)
		if err != nil {
			return offsets
		}
		segment := text[offset:end]
		next, isDirective := setTermDelimiter(segment)
		if isDirective {
			active = next
		} else if delimiter != "" && delimiter != ";" {
			offsets = append(offsets, end)
		}
		if delimiter == "" {
			break
		}
		offset = end + len(delimiter)
	}
	return offsets
}

func lexRaw(text string, dv dialect.DriverVariant) ([]lexeme, error) {
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

// nextDelimiter returns the next active script delimiter outside SQL strings
// and comments. It also validates those protected regions so an incomplete
// literal or comment cannot produce a misleading partial symbol index.
func nextDelimiter(text string, offset int, delimiter string) (int, string, error) {
	for i := offset; i < len(text); i++ {
		switch text[i] {
		case '\'', '"':
			quote := text[i]
			i++
			closed := false
			for i < len(text) {
				if text[i] != quote {
					i++
					continue
				}
				if i+1 < len(text) && text[i+1] == quote {
					i += 2
					continue
				}
				i++
				closed = true
				break
			}
			if !closed {
				return 0, "", fmt.Errorf("unclosed quoted literal")
			}
			i--
		case '-':
			if i+1 < len(text) && text[i+1] == '-' {
				i += 2
				for i < len(text) && text[i] != '\n' {
					i++
				}
				i--
			}
		case '/':
			if i+1 < len(text) && text[i+1] == '*' {
				i += 2
				closed := false
				for i+1 < len(text) {
					if text[i] == '*' && text[i+1] == '/' {
						i += 2
						closed = true
						break
					}
					i++
				}
				if !closed {
					return 0, "", fmt.Errorf("unclosed multiline comment")
				}
				i--
			}
		default:
			if delimiter != "" && strings.HasPrefix(text[i:], delimiter) {
				return i, delimiter, nil
			}
		}
	}
	return len(text), "", nil
}

func setTermDelimiter(segment string) (string, bool) {
	fields := strings.Fields(maskSQLProtected(segment))
	if len(fields) != 3 || !strings.EqualFold(fields[0], "SET") || !strings.EqualFold(fields[1], "TERM") {
		return "", false
	}
	return fields[2], true
}

// maskSQLProtected leaves ordinary source bytes intact while hiding strings
// and comments from the small SET TERM recognizer.
func maskSQLProtected(text string) string {
	masked := []byte(text)
	for i := 0; i < len(masked); i++ {
		switch masked[i] {
		case '\'', '"':
			quote := masked[i]
			masked[i] = ' '
			for i+1 < len(masked) {
				i++
				ch := masked[i]
				masked[i] = ' '
				if ch == quote {
					if i+1 < len(masked) && masked[i+1] == quote {
						i++
						masked[i] = ' '
						continue
					}
					break
				}
			}
		case '-':
			if i+1 < len(masked) && masked[i+1] == '-' {
				masked[i], masked[i+1] = ' ', ' '
				i += 2
				for i < len(masked) && masked[i] != '\n' {
					masked[i] = ' '
					i++
				}
				i--
			}
		case '/':
			if i+1 < len(masked) && masked[i+1] == '*' {
				masked[i], masked[i+1] = ' ', ' '
				i += 2
				for i+1 < len(masked) {
					if masked[i] == '*' && masked[i+1] == '/' {
						masked[i], masked[i+1] = ' ', ' '
						i += 2
						break
					}
					masked[i] = ' '
					i++
				}
			}
		}
	}
	return string(masked)
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
