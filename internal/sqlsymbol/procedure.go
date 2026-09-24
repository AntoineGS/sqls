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

// Symbol is a declaration found in a procedure. Uses retain every bound
// occurrence for navigation; Reads and Writes separately describe its value
// usage for diagnostics.
type Symbol struct {
	Name          Name
	Kind          SymbolKind
	Declaration   Span
	Type          string
	TypeSpan      Span
	Scope         Span
	Uses          []Span
	Reads         []Span
	Writes        []Span
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
	sqlScopes   [][][]RelationRef

	// malformedDeclarations records the byte-span recoverLocalDeclaration
	// skipped over for a malformed DECLARE VARIABLE, so diagnostics-only
	// rules (e.g. unknown-variable) can suppress findings inside a region
	// whose declared-name shape could not be parsed, consistent with
	// Unsupported-style suppression elsewhere.
	malformedDeclarations []Span

	// malformedDeclarationNames records, per procedure index, the Key() of
	// any name a malformed DECLARE VARIABLE attempted (but failed) to
	// declare -- e.g. a missing type or missing terminator. Diagnostics-only
	// rules must not flag a later use of that same name as unknown: the
	// declaration's failure is the reportable problem, not the name's
	// later "unresolved" appearance, which would otherwise double-report
	// the same root cause under a confusing code.
	malformedDeclarationNames map[int]map[string]bool
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
	return analyze(text, dv, false)
}

// AnalyzeDiagnostics uses the same binder as navigation but tolerates a
// malformed procedure declaration when a later safe procedure boundary lets
// independent findings remain useful. Navigation continues to use Analyze's
// strict parse-error behavior.
func AnalyzeDiagnostics(text string, dv dialect.DriverVariant) (*Analysis, error) {
	return analyze(text, dv, true)
}

func analyze(text string, dv dialect.DriverVariant, tolerant bool) (*Analysis, error) {
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
			if !tolerant {
				return nil, err
			}
			if current >= 0 {
				suppressProcedureDiagnostics(analysis, current)
				closeProcedure(analysis, current, items[i].Span.Start)
				current = -1
			}
			frames = nil
			bodyStarted = false
			continue
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
					if !tolerant {
						return nil, fmt.Errorf("local variable declaration has an invalid name")
					}
					i = recoverLocalDeclaration(analysis, &current, &frames, &bodyStarted, items, i, "")
					continue
				}
				if i+3 >= len(items) || !declarationTypeStart(items[i+3]) {
					if !tolerant {
						return nil, fmt.Errorf("local variable %s is missing a type", name.Key())
					}
					i = recoverLocalDeclaration(analysis, &current, &frames, &bodyStarted, items, i, name.Key())
					continue
				}
				declarationEnd := i + 3
				for declarationEnd < len(items) && items[declarationEnd].Token.Kind != token.Semicolon {
					if isWord(items[declarationEnd], "BEGIN") {
						if !tolerant {
							return nil, fmt.Errorf("local variable %s declaration is missing a terminator", name.Key())
						}
						declarationEnd = len(items)
						break
					}
					declarationEnd++
				}
				if declarationEnd == len(items) || isWord(items[declarationEnd], "BEGIN") {
					if !tolerant {
						if declarationEnd == len(items) {
							return nil, fmt.Errorf("local variable %s declaration is incomplete", name.Key())
						}
						return nil, fmt.Errorf("local variable %s declaration is missing a terminator", name.Key())
					}
					i = recoverLocalDeclaration(analysis, &current, &frames, &bodyStarted, items, i, name.Key())
					continue
				}
				addSymbol(analysis, current, &Symbol{
					Name:        name,
					Kind:        Variable,
					Declaration: items[i+2].Span,
					Type:        text[items[i+3].Span.Start:items[declarationEnd-1].Span.End],
					TypeSpan:    Span{Start: items[i+3].Span.Start, End: items[declarationEnd-1].Span.End},
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

// recoverLocalDeclaration skips a malformed declaration to the next safe
// declaration terminator or procedure body. If only another procedure or EOF
// remains, the current procedure's diagnostics are suppressed because its
// reads cannot be bound reliably. attemptedName is the Key() of the name the
// failed declaration was trying to declare, or "" when even the name itself
// could not be parsed; when non-empty, it is recorded so unknown-variable
// detection excludes every later use of that same name in this procedure,
// not only positions inside the malformed declaration's own span.
func recoverLocalDeclaration(analysis *Analysis, current *int, frames *[]bodyFrame, bodyStarted *bool, items []lexeme, start int, attemptedName string) int {
	procIndex := *current
	if procIndex >= 0 && attemptedName != "" {
		if analysis.malformedDeclarationNames == nil {
			analysis.malformedDeclarationNames = make(map[int]map[string]bool)
		}
		if analysis.malformedDeclarationNames[procIndex] == nil {
			analysis.malformedDeclarationNames[procIndex] = make(map[string]bool)
		}
		analysis.malformedDeclarationNames[procIndex][attemptedName] = true
	}
	recordMalformed := func(end int) {
		analysis.malformedDeclarations = append(analysis.malformedDeclarations, Span{Start: items[start].Span.Start, End: end})
	}
	for i := start + 1; i < len(items); i++ {
		switch {
		case items[i].Token.Kind == token.Semicolon:
			recordMalformed(items[i].Span.End)
			return i
		case isWord(items[i], "BEGIN"):
			recordMalformed(items[i].Span.Start)
			return i - 1
		case isProcedureHeaderStart(items, i):
			recordMalformed(items[i].Span.Start)
			suppressProcedureDiagnostics(analysis, *current)
			closeProcedure(analysis, *current, items[i].Span.Start)
			*current = -1
			*frames = nil
			*bodyStarted = false
			return i - 1
		}
	}
	recordMalformed(len(analysis.Text))
	suppressProcedureDiagnostics(analysis, *current)
	closeProcedure(analysis, *current, len(analysis.Text))
	*current = -1
	*frames = nil
	*bodyStarted = false
	return len(items) - 1
}

func isProcedureHeaderStart(items []lexeme, start int) bool {
	if start < 0 || start >= len(items) || (!isWord(items[start], "CREATE") && !isWord(items[start], "ALTER")) {
		return false
	}
	next := start + 1
	if isWord(items[start], "CREATE") && next+1 < len(items) && isWord(items[next], "OR") && isWord(items[next+1], "ALTER") {
		next += 2
	}
	return next < len(items) && isWord(items[next], "PROCEDURE")
}

func suppressProcedureDiagnostics(analysis *Analysis, index int) {
	if analysis == nil || index < 0 || index >= len(analysis.procedures) {
		return
	}
	for _, symbols := range analysis.procedures[index].Symbols {
		for _, symbol := range symbols {
			symbol.RenameBlocked = firstReason(symbol.RenameBlocked, "malformed procedure declaration")
		}
	}
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
	var current *Symbol
	for i := open; i < len(items); i++ {
		item := items[i]
		if current != nil && sawType && !(item.Token.Kind == token.Comma && depth == 1) && !(item.Token.Kind == token.RParen && depth == 1) {
			current.TypeSpan.End = item.Span.End
		}
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
				if current != nil {
					current.Type = text[current.TypeSpan.Start:current.TypeSpan.End]
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
				if current != nil {
					current.Type = text[current.TypeSpan.Start:current.TypeSpan.End]
				}
				expectName = true
				sawType = false
				current = nil
			}
		default:
			if depth == 1 && expectName {
				if name, ok := nameFromLexeme(text, item); ok {
					current = &Symbol{
						Name:        name,
						Kind:        kind,
						Declaration: item.Span,
					}
					declarations = append(declarations, declaration{symbol: current})
					expectName = false
					sawName = true
				} else {
					return open, nil, fmt.Errorf("procedure parameter has an invalid name")
				}
			} else if !expectName && !sawType && declarationTypeStart(item) {
				sawType = true
				current.TypeSpan = Span{Start: item.Span.Start, End: item.Span.End}
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
