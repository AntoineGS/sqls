package sqlsymbol

import (
	"fmt"
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
	Text        string
	Variant     dialect.DriverVariant
	Symbols     []*Symbol
	procedures  []procedure
	lexemes     []lexeme
	resolutions []indexedResolution
	prefixes    []indexedResolution
	contexts    []tokenContext
	procedureAt []int
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

// Analyze discovers InterBase procedure declarations and binds source
// occurrences to those declarations.
func Analyze(text string, dv dialect.DriverVariant) (*Analysis, error) {
	analysis := &Analysis{Text: text, Variant: dv}
	if dv.Driver != dialect.DatabaseDriverInterBase {
		return analysis, nil
	}

	items, err := lex(text, dv)
	if err != nil {
		return nil, err
	}
	analysis.lexemes = items
	items = significantLexemes(items)
	current := -1
	var frames []bodyFrame
	bodyStarted := false
	for i := 0; i < len(items); i++ {
		if header, ok, err := procedureHeaderAt(text, items, i); err != nil {
			return nil, err
		} else if ok {
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
				name, ok := nameFromLexeme(text, items[i+2])
				if !ok {
					return nil, fmt.Errorf("local variable declaration has an invalid name")
				}
				if i+3 >= len(items) || !declarationTypeStart(items[i+3]) {
					return nil, fmt.Errorf("local variable %s is missing a type", name.Key())
				}
				declarationEnd := i + 3
				for declarationEnd < len(items) && items[declarationEnd].Token.Kind != token.Semicolon {
					if isWord(items[declarationEnd], "BEGIN") {
						return nil, fmt.Errorf("local variable %s declaration is missing a terminator", name.Key())
					}
					declarationEnd++
				}
				if declarationEnd == len(items) {
					return nil, fmt.Errorf("local variable %s declaration is incomplete", name.Key())
				}
				addSymbol(analysis, current, &Symbol{
					Name:        name,
					Kind:        Variable,
					Declaration: items[i+2].Span,
				})
				i += 2
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
	bindOccurrences(analysis, items)
	return analysis, nil
}

func declarationTypeStart(item lexeme) bool {
	if item.Token == nil || item.Token.Kind != token.SQLKeyword {
		return false
	}
	return !isWord(item, "BEGIN") && !isWord(item, "END") &&
		!isWord(item, "AS") && !isWord(item, "DECLARE") &&
		!isWord(item, "VARIABLE") && !isWord(item, "CREATE") &&
		!isWord(item, "ALTER")
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

func procedureHeaderAt(text string, items []lexeme, start int) (procedureHeader, bool, error) {
	if !isWord(items[start], "CREATE") && !isWord(items[start], "ALTER") {
		return procedureHeader{}, false, nil
	}
	i := start + 1
	if isWord(items[start], "CREATE") && i+1 < len(items) && isWord(items[i], "OR") && isWord(items[i+1], "ALTER") {
		i += 2
	}
	if i >= len(items) || !isWord(items[i], "PROCEDURE") || i+1 >= len(items) {
		return procedureHeader{}, false, nil
	}
	if _, ok := nameFromLexeme(text, items[i+1]); !ok {
		return procedureHeader{}, false, nil
	}
	i += 2
	var symbols []*Symbol
	if i < len(items) && items[i].Token.Kind == token.LParen {
		end, names, err := parseDeclarationList(text, items, i, InputParameter)
		if err != nil {
			return procedureHeader{}, false, err
		}
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
			end, names, err := parseDeclarationList(text, items, i, OutputParameter)
			if err != nil {
				return procedureHeader{}, false, err
			}
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
			return procedureHeader{next: i + 1, symbols: symbols}, true, nil
		}
		if i != start && (isWord(items[i], "CREATE") || isWord(items[i], "ALTER")) {
			break
		}
	}
	return procedureHeader{next: i, symbols: symbols}, true, nil
}

type declaration struct {
	symbol *Symbol
}

func parseDeclarationList(text string, items []lexeme, open int, kind SymbolKind) (int, []declaration, error) {
	depth := 0
	expectName := true
	sawName := false
	sawType := false
	var declarations []declaration
	for i := open; i < len(items); i++ {
		item := items[i]
		switch item.Token.Kind {
		case token.LParen:
			if depth == 1 && !expectName {
				// A type such as NUMERIC(15,2) is structurally complete;
				// the nested list belongs to that type.
				if !sawType {
					return open, nil, fmt.Errorf("procedure parameter is missing a type")
				}
			}
			depth++
		case token.RParen:
			if depth == 1 {
				if expectName && sawName {
					return open, nil, fmt.Errorf("procedure parameter is missing a name")
				}
				if !expectName && !sawType {
					return open, nil, fmt.Errorf("procedure parameter is missing a type")
				}
			}
			depth--
			if depth == 0 {
				return i + 1, declarations, nil
			}
		case token.Comma:
			if depth == 1 {
				if expectName {
					return open, nil, fmt.Errorf("procedure parameter is missing a name")
				}
				if !sawType {
					return open, nil, fmt.Errorf("procedure parameter is missing a type")
				}
				expectName = true
				sawType = false
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
					sawName = true
				} else {
					return open, nil, fmt.Errorf("procedure parameter has an invalid name")
				}
			} else if depth == 1 && !sawType && declarationTypeStart(item) {
				sawType = true
			}
		}
	}
	return open, nil, fmt.Errorf("unterminated procedure parameter list")
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
