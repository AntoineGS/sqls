package sqlsymbol

import (
	"strings"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/token"
)

// SymbolKind identifies the declaration form of a procedural symbol.
type SymbolKind uint8

const (
	Variable SymbolKind = iota
	InputParameter
	OutputParameter
)

// Symbol is a declaration found in a procedure. Uses are intentionally left
// empty here; occurrence binding is added by a later analysis increment.
type Symbol struct {
	Name          Name
	Kind          SymbolKind
	Declaration   Span
	Scope         Span
	Uses          []Span
	RenameBlocked string
}

type procedure struct {
	Span    Span
	Symbols map[string][]*Symbol
}

// Analysis is the request-local symbol index for one source document.
type Analysis struct {
	Text       string
	Variant    dialect.DriverVariant
	Symbols    []*Symbol
	procedures []procedure
}

type procedureHeader struct {
	next    int
	symbols []*Symbol
}

type bodyFrame uint8

const (
	beginFrame bodyFrame = iota
	caseFrame
)

// Analyze discovers InterBase procedure declarations and their source scopes.
// It does not bind references in the procedure body.
func Analyze(text string, dv dialect.DriverVariant) (*Analysis, error) {
	analysis := &Analysis{Text: text, Variant: dv}
	if dv.Driver != dialect.DatabaseDriverInterBase {
		return analysis, nil
	}

	items, err := lex(text, dv)
	if err != nil {
		return nil, err
	}
	items = significantLexemes(items)
	current := -1
	var frames []bodyFrame
	bodyStarted := false
	for i := 0; i < len(items); i++ {
		if header, ok := procedureHeaderAt(text, items, i); ok {
			if current >= 0 {
				closeProcedure(analysis, current, items[i].Span.Start)
			}
			current = appendProcedure(analysis, header, text, items[i].Span.Start)
			frames = nil
			bodyStarted = false
			i = header.next - 1
			continue
		}
		if current < 0 {
			continue
		}

		item := items[i]
		if !bodyStarted {
			if isWord(item, "DECLARE") && i+2 < len(items) && isWord(items[i+1], "VARIABLE") {
				if name, ok := nameFromLexeme(text, items[i+2]); ok {
					addSymbol(analysis, current, &Symbol{
						Name:        name,
						Kind:        Variable,
						Declaration: items[i+2].Span,
					})
					i += 2
				}
				continue
			}
			if isWord(item, "BEGIN") {
				bodyStarted = true
				frames = append(frames, beginFrame)
			}
			continue
		}

		switch {
		case isWord(item, "BEGIN"):
			frames = append(frames, beginFrame)
		case isWord(item, "CASE"):
			frames = append(frames, caseFrame)
		case isWord(item, "END") && len(frames) > 0:
			frames = frames[:len(frames)-1]
			if len(frames) == 0 {
				closeProcedure(analysis, current, item.Span.End)
				current = -1
				bodyStarted = false
			}
		}
	}
	if current >= 0 {
		closeProcedure(analysis, current, len(text))
	}
	return analysis, nil
}

func significantLexemes(items []lexeme) []lexeme {
	result := make([]lexeme, 0, len(items))
	for _, item := range items {
		switch item.Token.Kind {
		case token.Whitespace, token.Comment, token.MultilineComment:
			continue
		default:
			result = append(result, item)
		}
	}
	return result
}

func procedureHeaderAt(text string, items []lexeme, start int) (procedureHeader, bool) {
	if !isWord(items[start], "CREATE") && !isWord(items[start], "ALTER") {
		return procedureHeader{}, false
	}
	i := start + 1
	if isWord(items[start], "CREATE") && i+1 < len(items) && isWord(items[i], "OR") && isWord(items[i+1], "ALTER") {
		i += 2
	}
	if i >= len(items) || !isWord(items[i], "PROCEDURE") || i+1 >= len(items) {
		return procedureHeader{}, false
	}
	if _, ok := nameFromLexeme(text, items[i+1]); !ok {
		return procedureHeader{}, false
	}
	i += 2
	var symbols []*Symbol
	if i < len(items) && items[i].Token.Kind == token.LParen {
		var end int
		var names []declaration
		end, names = parseDeclarationList(text, items, i, InputParameter)
		for _, item := range names {
			symbols = append(symbols, item.symbol)
		}
		if end > i {
			i = end
		}
	}
	if i < len(items) && isWord(items[i], "RETURNS") {
		i++
		if i < len(items) && items[i].Token.Kind == token.LParen {
			end, names := parseDeclarationList(text, items, i, OutputParameter)
			for _, item := range names {
				symbols = append(symbols, item.symbol)
			}
			if end > i {
				i = end
			}
		}
	}
	// Header types can contain arbitrary nested parentheses. The first
	// unquoted AS after the parameter lists starts the declaration section.
	for ; i < len(items); i++ {
		if isWord(items[i], "AS") {
			return procedureHeader{next: i + 1, symbols: symbols}, true
		}
		if i != start && (isWord(items[i], "CREATE") || isWord(items[i], "ALTER")) {
			break
		}
	}
	return procedureHeader{next: i, symbols: symbols}, true
}

type declaration struct {
	symbol *Symbol
}

func parseDeclarationList(text string, items []lexeme, open int, kind SymbolKind) (int, []declaration) {
	depth := 0
	expectName := true
	var declarations []declaration
	for i := open; i < len(items); i++ {
		item := items[i]
		switch item.Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
			if depth == 0 {
				return i + 1, declarations
			}
		case token.Comma:
			if depth == 1 {
				expectName = true
			}
		default:
			if depth == 1 && expectName {
				if name, ok := nameFromLexeme(text, item); ok {
					declarations = append(declarations, declaration{symbol: &Symbol{
						Name:        name,
						Kind:        kind,
						Declaration: item.Span,
					}})
					expectName = false
				}
			}
		}
	}
	return open, declarations
}

func isWord(item lexeme, expected string) bool {
	if item.Token == nil || item.Token.Kind != token.SQLKeyword {
		return false
	}
	word, ok := item.Token.Value.(*token.SQLWord)
	return ok && word.QuoteStyle == 0 && strings.EqualFold(word.Keyword, expected)
}

func appendProcedure(analysis *Analysis, header procedureHeader, text string, start int) int {
	p := procedure{Span: Span{Start: start, End: len(text)}, Symbols: make(map[string][]*Symbol)}
	analysis.procedures = append(analysis.procedures, p)
	index := len(analysis.procedures) - 1
	for _, symbol := range header.symbols {
		addSymbol(analysis, index, symbol)
	}
	return index
}

func addSymbol(analysis *Analysis, procedureIndex int, symbol *Symbol) {
	p := &analysis.procedures[procedureIndex]
	key := symbol.Name.Key()
	if existing := p.Symbols[key]; len(existing) > 0 {
		symbol.RenameBlocked = "ambiguous declaration"
		for _, other := range existing {
			other.RenameBlocked = "ambiguous declaration"
		}
	}
	p.Symbols[key] = append(p.Symbols[key], symbol)
	analysis.Symbols = append(analysis.Symbols, symbol)
}

func closeProcedure(analysis *Analysis, index, end int) {
	p := &analysis.procedures[index]
	if end < p.Span.Start {
		end = p.Span.Start
	}
	p.Span.End = end
	for _, symbols := range p.Symbols {
		for _, symbol := range symbols {
			symbol.Scope = p.Span
		}
	}
}
