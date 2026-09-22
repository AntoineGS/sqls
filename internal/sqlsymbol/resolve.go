package sqlsymbol

import (
	"sort"

	"github.com/sqls-server/sqls/token"
)

// Role describes the syntactic role of the source token under a cursor.
type Role uint8

const (
	Outside Role = iota
	Local
	Relation
	Column
	Alias
	Callable
	Other
	Ambiguous
)

// RelationRef is a relation visible to a SQL expression. Candidate scopes are
// populated by the SQL ownership pass; the binding pass keeps this small value
// available on every SQL reference.
type RelationRef struct {
	Name  Name
	Alias *Name
}

// SQLReference identifies an identifier occurring in a SQL statement.
type SQLReference struct {
	Name      Name
	Qualifier *Name
	Scopes    [][]RelationRef
}

// Resolution is the result of resolving a byte offset in the source.
type Resolution struct {
	Role        Role
	Span        Span
	InProcedure bool
	Symbol      *Symbol
	SQL         *SQLReference
}

type indexedResolution struct {
	Resolution
	prefix Span
}

func (a *Analysis) addResolution(r Resolution, prefix Span) {
	if r.Span.End <= r.Span.Start {
		return
	}
	a.resolutions = append(a.resolutions, indexedResolution{Resolution: r, prefix: prefix})
	if prefix.End > prefix.Start {
		a.prefixes = append(a.prefixes, indexedResolution{Resolution: r, prefix: prefix})
	}
}

// Resolve uses half-open source spans. A cursor on a variable prefix colon is
// accepted as a convenience, but the returned editable span is the identifier
// alone.
func (a *Analysis) Resolve(offset int) Resolution {
	if offset < 0 || offset >= len(a.Text) {
		return Resolution{}
	}
	if len(a.prefixes) > 0 {
		if i := sort.Search(len(a.prefixes), func(i int) bool {
			return a.prefixes[i].prefix.Start > offset
		}) - 1; i >= 0 && offset < a.prefixes[i].prefix.End {
			return a.prefixes[i].Resolution
		}
	}
	if len(a.resolutions) == 0 {
		return Resolution{}
	}
	i := sort.Search(len(a.resolutions), func(i int) bool {
		return a.resolutions[i].Span.Start > offset
	}) - 1
	if i >= 0 && offset < a.resolutions[i].Span.End {
		return a.resolutions[i].Resolution
	}
	return Resolution{}
}

// References returns declaration and use spans for symbol. The result is
// sorted by source position and contains no duplicate spans.
func (a *Analysis) References(symbol *Symbol, includeDeclaration bool) []Span {
	if symbol == nil {
		return nil
	}
	seen := make(map[Span]struct{}, len(symbol.Uses)+1)
	result := make([]Span, 0, len(symbol.Uses)+1)
	add := func(span Span) {
		if _, ok := seen[span]; ok {
			return
		}
		seen[span] = struct{}{}
		result = append(result, span)
	}
	if includeDeclaration {
		add(symbol.Declaration)
	}
	for _, span := range symbol.Uses {
		add(span)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Start == result[j].Start {
			return result[i].End < result[j].End
		}
		return result[i].Start < result[j].Start
	})
	return result
}

func bindOccurrences(a *Analysis, items []lexeme) {
	for _, item := range a.lexemes {
		switch item.Token.Kind {
		case token.Comment, token.MultilineComment, token.SingleQuotedString, token.NationalStringLiteral:
			a.addResolution(Resolution{Role: Other, Span: item.Span}, Span{})
		}
	}

	declarations := make(map[Span]*Symbol, len(a.Symbols))
	for _, symbol := range a.Symbols {
		declarations[symbol.Declaration] = symbol
		a.addResolution(Resolution{
			Role:        declarationRole(symbol),
			Span:        symbol.Declaration,
			InProcedure: true,
			Symbol:      declarationResolutionSymbol(symbol),
		}, Span{})
	}

	relations, aliases, insertColumns, updateTargets := sqlPositions(items)
	for i, item := range items {
		name, ok := nameFromLexeme(a.Text, item)
		if !ok || !isNameToken(item) {
			continue
		}
		if _, ok := declarations[item.Span]; ok {
			continue
		}
		role, symbol, sqlRef, prefix, blocked := classifyName(a, items, i, name, relations, aliases, insertColumns, updateTargets)
		if role == Outside {
			continue
		}
		if blocked != "" && symbol != nil && symbol.RenameBlocked == "" {
			symbol.RenameBlocked = blocked
		}
		resolution := Resolution{
			Role:        role,
			Span:        item.Span,
			InProcedure: procedureForOffset(a, item.Span.Start) >= 0,
			Symbol:      symbol,
			SQL:         sqlRef,
		}
		a.addResolution(resolution, prefix)
		if role == Local && symbol != nil {
			symbol.Uses = append(symbol.Uses, item.Span)
		}
	}
	sort.SliceStable(a.resolutions, func(i, j int) bool {
		return a.resolutions[i].Span.Start < a.resolutions[j].Span.Start
	})
	sort.SliceStable(a.prefixes, func(i, j int) bool {
		return a.prefixes[i].prefix.Start < a.prefixes[j].prefix.Start
	})
	for _, symbol := range a.Symbols {
		sort.Slice(symbol.Uses, func(i, j int) bool { return symbol.Uses[i].Start < symbol.Uses[j].Start })
	}
}

func declarationRole(symbol *Symbol) Role {
	if symbol.RenameBlocked == "ambiguous declaration" {
		return Ambiguous
	}
	return Local
}

func declarationResolutionSymbol(symbol *Symbol) *Symbol {
	if symbol.RenameBlocked == "ambiguous declaration" {
		return nil
	}
	return symbol
}

func procedureForOffset(a *Analysis, offset int) int {
	for i := range a.procedures {
		p := &a.procedures[i]
		if p.Span.Start <= offset && offset < p.Span.End {
			return i
		}
	}
	return -1
}

func symbolFor(a *Analysis, offset int, name Name) (*Symbol, bool) {
	index := procedureForOffset(a, offset)
	if index < 0 {
		return nil, false
	}
	symbols := a.procedures[index].Symbols[name.Key()]
	if len(symbols) != 1 {
		return nil, len(symbols) > 1
	}
	return symbols[0], false
}

func sqlPositions(items []lexeme) (map[int]bool, map[int]bool, map[int]bool, map[int]bool) {
	relations := make(map[int]bool)
	aliases := make(map[int]bool)
	insertColumns := make(map[int]bool)
	updateTargets := make(map[int]bool)
	for i := range items {
		if !isNameToken(items[i]) {
			continue
		}
		if i > 0 && (isWord(items[i-1], "FROM") || isWord(items[i-1], "JOIN") ||
			isWord(items[i-1], "UPDATE")) {
			relations[i] = true
		}
		if i > 1 && isWord(items[i-1], "INTO") && isWord(items[i-2], "INSERT") {
			relations[i] = true
		}
		if i > 1 && isWord(items[i-1], "FROM") && isWord(items[i-2], "DELETE") {
			relations[i] = true
		}
		if i > 0 && isWord(items[i-1], "AS") && isLikelySQLStatement(items, i) {
			aliases[i] = true
		}
		if i > 0 && items[i-1].Token.Kind == token.Period {
			if i > 1 {
				aliases[i-2] = true
			}
		}
	}
	for relation := range relations {
		end := relation + 1
		if end < len(items) && isWord(items[end], "AS") {
			end++
		}
		if end < len(items) && isNameToken(items[end]) && !isSQLClause(items[end]) {
			aliases[end] = true
		}
	}
	for i := range items {
		if !isWord(items[i], "INSERT") {
			continue
		}
		into := i + 1
		for into < len(items) && !isWord(items[into], "INTO") && items[into].Token.Kind != token.Semicolon {
			into++
		}
		if into+2 >= len(items) || !isNameToken(items[into+1]) {
			continue
		}
		open := into + 2
		if items[open].Token.Kind != token.LParen {
			continue
		}
		depth := 1
		for j := open + 1; j < len(items) && depth > 0; j++ {
			switch items[j].Token.Kind {
			case token.LParen:
				depth++
			case token.RParen:
				depth--
			case token.Comma:
				// The names in this list are all explicit target columns.
			default:
				if depth == 1 && isNameToken(items[j]) {
					insertColumns[j] = true
				}
			}
		}
	}
	update, set := false, false
	for i, item := range items {
		if statementBoundary(item) {
			update, set = false, false
		}
		if isWord(item, "UPDATE") {
			update = true
		}
		if update && isWord(item, "SET") {
			set = true
		}
		if update && set && isNameToken(item) && i+1 < len(items) && items[i+1].Token.Kind == token.Eq {
			updateTargets[i] = true
		}
	}
	return relations, aliases, insertColumns, updateTargets
}

func classifyName(a *Analysis, items []lexeme, i int, name Name, relations, aliases, insertColumns, updateTargets map[int]bool) (Role, *Symbol, *SQLReference, Span, string) {
	item := items[i]
	procIndex := procedureForOffset(a, item.Span.Start)
	symbol, duplicate := symbolFor(a, item.Span.Start, name)
	prev := i - 1
	next := i + 1
	if prev >= 0 && items[prev].Token.Kind == token.Colon {
		if duplicate {
			return Ambiguous, nil, nil, items[prev].Span, ""
		}
		if symbol != nil {
			return Local, symbol, nil, items[prev].Span, ""
		}
	}

	if relations[i] {
		return Relation, nil, &SQLReference{Name: name}, Span{}, ""
	}
	if aliases[i] {
		return Alias, nil, &SQLReference{Name: name}, Span{}, ""
	}
	if prev >= 0 && items[prev].Token.Kind == token.Period {
		qualifier := nameBeforePeriod(a.Text, items, prev)
		return Column, nil, &SQLReference{Name: name, Qualifier: qualifier}, Span{}, ""
	}
	if next < len(items) && items[next].Token.Kind == token.Period {
		return Alias, nil, &SQLReference{Name: name}, Span{}, ""
	}
	if insertColumns[i] || updateTargets[i] {
		return Column, nil, &SQLReference{Name: name}, Span{}, ""
	}
	if isCallable(items, i) || isProcedureCallName(items, i) {
		return Callable, nil, &SQLReference{Name: name}, Span{}, ""
	}

	kind := statementKind(items, i)
	if kind == statementExecute {
		if isReturningTarget(items, i) && symbol != nil && !duplicate {
			return Local, symbol, nil, colonPrefix(items, i), ""
		}
		if symbol != nil && !duplicate {
			return Local, symbol, nil, Span{}, ""
		}
		if duplicate {
			return Ambiguous, nil, nil, Span{}, ""
		}
		return Column, nil, &SQLReference{Name: name}, Span{}, ""
	}
	if kind == statementSQL {
		if isReturningTarget(items, i) && symbol != nil && !duplicate {
			return Local, symbol, nil, colonPrefix(items, i), ""
		}
		if symbol != nil {
			symbol.RenameBlocked = firstReason(symbol.RenameBlocked, "ambiguous SQL value expression")
			return Ambiguous, nil, &SQLReference{Name: name}, Span{}, ""
		}
		if duplicate {
			return Ambiguous, nil, &SQLReference{Name: name}, Span{}, ""
		}
		return Column, nil, &SQLReference{Name: name}, Span{}, ""
	}
	if procIndex >= 0 {
		if duplicate {
			return Ambiguous, nil, nil, Span{}, ""
		}
		if symbol != nil {
			if unsupportedProcedureSegment(items, i) {
				return Local, symbol, nil, Span{}, "unsupported syntax may contain a local occurrence"
			}
			return Local, symbol, nil, Span{}, ""
		}
	}
	return Other, nil, nil, Span{}, ""
}

const (
	statementProc = iota
	statementSQL
	statementExecute
)

func statementKind(items []lexeme, index int) int {
	start := index
	for start > 0 && !statementBoundary(items[start-1]) {
		start--
	}
	if start >= len(items) {
		return statementProc
	}
	if isWord(items[start], "EXECUTE") {
		return statementExecute
	}
	if isWord(items[start], "SELECT") || isWord(items[start], "UPDATE") ||
		isWord(items[start], "INSERT") || isWord(items[start], "DELETE") {
		return statementSQL
	}
	if isWord(items[start], "FOR") {
		for j := start + 1; j < index && !statementBoundary(items[j]); j++ {
			if isWord(items[j], "SELECT") {
				return statementSQL
			}
		}
	}
	for j := start; j < index; j++ {
		if isWord(items[j], "SELECT") {
			return statementSQL
		}
	}
	return statementProc
}

func statementBoundary(item lexeme) bool {
	return item.Token.Kind == token.Semicolon || isWord(item, "BEGIN") ||
		isWord(item, "END") || isWord(item, "THEN") || isWord(item, "ELSE") ||
		isWord(item, "DO")
}

func unsupportedProcedureSegment(items []lexeme, index int) bool {
	start := index
	for start > 0 && !statementBoundary(items[start-1]) {
		start--
	}
	if start >= index || !isNameToken(items[start]) {
		return false
	}
	for _, word := range []string{"IF", "WHILE", "FOR", "CASE", "SUSPEND", "EXIT", "LEAVE", "BREAK", "CONTINUE", "EXCEPTION"} {
		if isWord(items[start], word) {
			return false
		}
	}
	if start+1 < len(items) && items[start+1].Token.Kind == token.Eq {
		return false
	}
	return true
}

func isReturningTarget(items []lexeme, i int) bool {
	return i > 0 && (isWord(items[i-1], "INTO") || isWord(items[i-1], "RETURNING_VALUES"))
}

func colonPrefix(items []lexeme, i int) Span {
	if i > 0 && items[i-1].Token.Kind == token.Colon {
		return items[i-1].Span
	}
	return Span{}
}

func isCallable(items []lexeme, i int) bool {
	return i+1 < len(items) && items[i+1].Token.Kind == token.LParen
}

func isProcedureCallName(items []lexeme, i int) bool {
	return i > 0 && isWord(items[i-1], "PROCEDURE")
}

func isNameToken(item lexeme) bool {
	if item.Token == nil || item.Token.Kind != token.SQLKeyword {
		return false
	}
	word, ok := item.Token.Value.(*token.SQLWord)
	return ok && (word.QuoteStyle != 0 || !syntaxWords[word.Keyword])
}

// Some InterBase keywords are also legal declaration names (LOCAL is a common
// example). The lexer intentionally does not decide whether a word is an
// identifier, so this small syntactic set is used only while walking a source
// context. Quoted words always remain identifiers.
var syntaxWords = map[string]bool{
	"ADD": true, "ALTER": true, "AND": true, "AS": true, "ASC": true,
	"BEGIN": true, "BY": true, "CASE": true, "CAST": true, "CHECK": true,
	"CREATE": true, "DECLARE": true, "DELETE": true, "DO": true,
	"ELSE": true, "END": true, "EXECUTE": true, "FOR": true, "FROM": true,
	"FULL": true, "GROUP": true, "HAVING": true, "IF": true, "IN": true,
	"INNER": true, "INSERT": true, "INTO": true, "IS": true, "JOIN": true,
	"LEFT": true, "LIKE": true, "LIMIT": true, "MERGE": true, "NOT": true,
	"NULL": true, "ON": true, "OR": true, "ORDER": true, "OUTER": true,
	"PROCEDURE": true, "RETURNING_VALUES": true, "RETURNS": true,
	"RIGHT": true, "SELECT": true, "SET": true, "THEN": true, "UNION": true,
	"UPDATE": true, "VALUES": true, "VARIABLE": true, "WHEN": true,
	"WHERE": true, "WHILE": true, "WITH": true,
	"BREAK": true, "CONTINUE": true, "EXIT": true, "EXCEPTION": true,
	"LEAVE": true, "SUSPEND": true,
}

func isLikelySQLStatement(items []lexeme, i int) bool {
	for j := i - 1; j >= 0 && !statementBoundary(items[j]); j-- {
		if isWord(items[j], "SELECT") || isWord(items[j], "UPDATE") || isWord(items[j], "INSERT") || isWord(items[j], "DELETE") {
			return true
		}
	}
	return false
}

func isSQLClause(item lexeme) bool {
	for _, word := range []string{"WHERE", "GROUP", "ORDER", "HAVING", "JOIN", "ON", "SET", "VALUES", "RETURNING"} {
		if isWord(item, word) {
			return true
		}
	}
	return false
}

func nameBeforePeriod(text string, items []lexeme, period int) *Name {
	if period <= 0 {
		return nil
	}
	name, ok := nameFromLexeme(text, items[period-1])
	if !ok {
		return nil
	}
	return &name
}

func firstReason(current, reason string) string {
	if current != "" {
		return current
	}
	return reason
}
