package sqlsymbol

import (
	"sort"
	"strings"

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

type contextKind uint8

const (
	contextProcedure contextKind = iota
	contextSQL
	contextExecute
	contextUnsupported
	contextExecutePending
)

type tokenContext struct {
	kind         contextKind
	relation     bool
	alias        bool
	insertColumn bool
	updateTarget bool
	outputTarget bool
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

	a.contexts, a.procedureAt = buildContexts(a, items)
	a.sqlScopes = buildSQLOwnership(a.Text, items, a.contexts)
	for i, item := range items {
		name, ok := nameFromLexeme(a.Text, item)
		if !ok || !isNameToken(item) {
			continue
		}
		if _, ok := declarations[item.Span]; ok {
			continue
		}
		procIndex := a.procedureAt[i]
		role, symbol, sqlRef, prefix, blocked := classifyName(a, items, i, name, a.contexts[i], procIndex)
		if role == Outside {
			continue
		}
		if blocked != "" && symbol != nil && symbol.RenameBlocked == "" {
			symbol.RenameBlocked = blocked
		}
		resolution := Resolution{
			Role:        role,
			Span:        item.Span,
			InProcedure: procIndex >= 0,
			Symbol:      symbol,
			SQL:         sqlRef,
		}
		a.addResolution(resolution, prefix)
		if role == Local && symbol != nil {
			symbol.Uses = append(symbol.Uses, item.Span)
			if isWriteOccurrence(items, i, a.contexts[i]) {
				symbol.Writes = append(symbol.Writes, item.Span)
			} else {
				symbol.Reads = append(symbol.Reads, item.Span)
			}
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

func isWriteOccurrence(items []lexeme, index int, context tokenContext) bool {
	if context.outputTarget {
		return true
	}
	if context.kind != contextProcedure || index+1 >= len(items) || items[index+1].Token.Kind != token.Eq || index == 0 {
		return false
	}
	previous := items[index-1]
	if previous.Token.Kind == token.Semicolon {
		return true
	}
	return isWord(previous, "BEGIN") || isWord(previous, "THEN") || isWord(previous, "ELSE") || isWord(previous, "DO")
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

func symbolFor(a *Analysis, procedureIndex int, name Name) (*Symbol, bool) {
	if procedureIndex < 0 || procedureIndex >= len(a.procedures) {
		return nil, false
	}
	symbols := a.procedures[procedureIndex].Symbols[name.Key()]
	if len(symbols) != 1 {
		return nil, len(symbols) > 1
	}
	return symbols[0], false
}

func buildContexts(a *Analysis, items []lexeme) ([]tokenContext, []int) {
	contexts := make([]tokenContext, len(items))
	procedureAt := make([]int, len(items))
	procedure := 0
	for i, item := range items {
		for procedure < len(a.procedures) && a.procedures[procedure].Span.End <= item.Span.Start {
			procedure++
		}
		if procedure < len(a.procedures) && a.procedures[procedure].Span.Start <= item.Span.Start {
			procedureAt[i] = procedure
		} else {
			procedureAt[i] = -1
		}
	}

	kind := contextProcedure
	active := false
	depth := 0
	restoreProcedureDepth := -1
	updateSetDepth := -1
	updateExpectTarget := false
	outputDepth := -1
	outputActive := false
	outputExpect := false
	unsupportedFrames := make([]bodyFrame, 0, 2)
	bodyStarted := make([]bool, len(a.procedures))
	head := ""
	for i, item := range items {
		procedureIndex := procedureAt[i]
		if item.Token.Kind == token.Semicolon && len(unsupportedFrames) == 0 {
			kind, active, depth, restoreProcedureDepth = contextProcedure, false, 0, -1
			updateSetDepth, updateExpectTarget, outputDepth, outputActive, outputExpect, head = -1, false, -1, false, false, ""
		}
		if len(unsupportedFrames) > 0 {
			contexts[i].kind = contextUnsupported
			if isWord(item, "CASE") {
				unsupportedFrames = append(unsupportedFrames, caseFrame)
			}
			if isWord(item, "BEGIN") {
				unsupportedFrames = append(unsupportedFrames, beginFrame)
			}
			if isWord(item, "END") && len(unsupportedFrames) > 0 {
				last := len(unsupportedFrames) - 1
				// CASE and BEGIN are typed frames: END closes only the
				// innermost matching construct, never the outer block by
				// accident.
				switch unsupportedFrames[last] {
				case beginFrame, caseFrame:
					unsupportedFrames = unsupportedFrames[:last]
				}
				if len(unsupportedFrames) == 0 {
					kind, active = contextUnsupported, true
				}
			}
			continue
		}
		if item.Token.Kind == token.LParen {
			depth++
		}
		if isWord(item, "BEGIN") && procedureIndex >= 0 && !bodyStarted[procedureIndex] && head != "EXECUTE_BLOCK" {
			bodyStarted[procedureIndex] = true
			kind, active, head = contextProcedure, false, ""
		}

		if kind == contextExecutePending {
			if isWord(item, "PROCEDURE") {
				kind, active = contextExecute, true
			} else if isWord(item, "BLOCK") {
				kind, active = contextUnsupported, true
				head = "EXECUTE_BLOCK"
			} else if active {
				kind = contextUnsupported
			}
		}
		if !active {
			switch {
			case isWord(item, "BEGIN"):
				kind, active = contextProcedure, false
			case isWord(item, "SELECT"), isWord(item, "UPDATE"), isWord(item, "INSERT"), isWord(item, "DELETE"):
				kind, active = contextSQL, true
				head = strings.ToUpper(item.Token.Value.(*token.SQLWord).Keyword)
			case isWord(item, "EXECUTE"):
				kind, active = contextExecutePending, true
				head = "EXECUTE"
			case isWord(item, "MERGE"), isWord(item, "WITH"):
				kind, active = contextUnsupported, true
				head = strings.ToUpper(item.Token.Value.(*token.SQLWord).Keyword)
			case isWord(item, "IF"), isWord(item, "WHILE"), isWord(item, "FOR"), isWord(item, "CASE"):
				kind, active = contextProcedure, true
				head = strings.ToUpper(item.Token.Value.(*token.SQLWord).Keyword)
			case isNameToken(item):
				active = true
				if i+1 >= len(items) || items[i+1].Token.Kind != token.Eq {
					kind = contextUnsupported
				}
			}
		}
		if kind == contextProcedure && isWord(item, "SELECT") && depth > 0 {
			kind, restoreProcedureDepth = contextSQL, depth
		}
		if kind == contextProcedure && head == "FOR" && isWord(item, "SELECT") {
			kind = contextSQL
		}
		if kind == contextSQL && head == "FOR" && isWord(item, "DO") {
			kind, active = contextProcedure, false
			updateSetDepth, updateExpectTarget, outputDepth, outputActive, outputExpect = -1, false, -1, false, false
		}
		if kind == contextProcedure && (isWord(item, "THEN") || isWord(item, "ELSE") || isWord(item, "DO")) {
			active = false
		}
		if kind == contextUnsupported && head == "EXECUTE_BLOCK" && isWord(item, "BEGIN") && i > 0 && isWord(items[i-1], "AS") {
			unsupportedFrames = append(unsupportedFrames, beginFrame)
		}
		contexts[i].kind = kind

		if kind == contextSQL || kind == contextExecute {
			if isWord(item, "SET") && kind == contextSQL && head == "UPDATE" {
				updateSetDepth = depth
				updateExpectTarget = true
			}
			if kind == contextSQL && isSQLClause(item) && depth <= updateSetDepth && !isWord(item, "SET") {
				updateSetDepth = -1
			}
			if kind == contextSQL && isWord(item, "INTO") && head != "INSERT" {
				outputDepth, outputActive, outputExpect = depth, true, true
			}
			if kind == contextExecute && isWord(item, "RETURNING_VALUES") {
				outputDepth, outputActive, outputExpect = depth, true, true
			}
			if outputActive && depth <= outputDepth && (isWord(item, "FROM") || isWord(item, "WHERE") || isWord(item, "RETURNING")) {
				outputActive, outputExpect = false, false
			}
			if outputActive && item.Token.Kind == token.Comma && depth == outputDepth {
				outputExpect = true
			}
			if outputActive && isNameToken(item) && depth == outputDepth && outputExpect {
				contexts[i].outputTarget = true
				outputExpect = false
			}
			if updateSetDepth >= 0 && kind == contextSQL && depth == updateSetDepth {
				if item.Token.Kind == token.Comma {
					updateExpectTarget = true
				}
				if updateExpectTarget && isNameToken(item) && i+1 < len(items) && items[i+1].Token.Kind == token.Eq {
					contexts[i].updateTarget = true
					updateExpectTarget = false
				}
			}
		}
		if item.Token.Kind == token.RParen {
			depth--
			if restoreProcedureDepth >= 0 && depth < restoreProcedureDepth {
				kind, active, restoreProcedureDepth = contextProcedure, true, -1
			}
		}
	}

	markSQLPositions(items, contexts)
	return contexts, procedureAt
}

func markSQLPositions(items []lexeme, contexts []tokenContext) {
	for i, item := range items {
		if !isNameToken(item) {
			continue
		}
		if i > 0 && (isWord(items[i-1], "FROM") || isWord(items[i-1], "JOIN") || isWord(items[i-1], "UPDATE")) {
			contexts[i].relation = true
		}
		if i > 1 && isWord(items[i-1], "INTO") && isWord(items[i-2], "INSERT") {
			contexts[i].relation = true
		}
		if i > 1 && isWord(items[i-1], "FROM") && isWord(items[i-2], "DELETE") {
			contexts[i].relation = true
		}
	}
	for relation := range contexts {
		if !contexts[relation].relation {
			continue
		}
		alias := relation + 1
		if alias < len(items) && isWord(items[alias], "AS") {
			alias++
		}
		if alias < len(items) && isNameToken(items[alias]) && !isSQLClause(items[alias]) {
			contexts[alias].alias = true
		}
	}
	insertState := 0 // 1: INSERT, 2: INTO, 3: relation, 4: column list
	insertDepth := 0
	for i, item := range items {
		if item.Token.Kind == token.Semicolon {
			insertState, insertDepth = 0, 0
		}
		switch {
		case isWord(item, "INSERT"):
			insertState, insertDepth = 1, 0
		case insertState == 1 && isWord(item, "INTO"):
			insertState = 2
		case insertState == 2 && isNameToken(item):
			contexts[i].relation = true
			insertState = 3
		case insertState == 3 && item.Token.Kind == token.LParen:
			insertState, insertDepth = 4, 1
		case insertState == 3:
			// Only an immediately following parenthesis starts an
			// explicit INSERT column list. VALUES and aliases end the
			// discovery state.
			insertState = 0
		case insertState == 4:
			switch item.Token.Kind {
			case token.LParen:
				insertDepth++
			case token.RParen:
				insertDepth--
				if insertDepth == 0 {
					insertState = 0
				}
			default:
				if insertDepth == 1 && isNameToken(item) {
					contexts[i].insertColumn = true
				}
			}
		}
	}
}

func classifyName(a *Analysis, items []lexeme, i int, name Name, context tokenContext, procIndex int) (Role, *Symbol, *SQLReference, Span, string) {
	symbol, duplicate := symbolFor(a, procIndex, name)
	prev := i - 1
	next := i + 1
	if prev >= 0 && items[prev].Token.Kind == token.Colon && (context.kind == contextUnsupported || context.kind == contextExecutePending) {
		if symbol != nil && !duplicate {
			symbol.RenameBlocked = firstReason(symbol.RenameBlocked, "unsupported syntax may contain a local occurrence")
			return Ambiguous, nil, a.sqlReference(i, name, nil), colonPrefix(items, i), ""
		}
		if duplicate {
			return Ambiguous, nil, nil, Span{}, ""
		}
		return Other, nil, nil, Span{}, ""
	}
	if prev >= 0 && items[prev].Token.Kind == token.Colon {
		if duplicate {
			return Ambiguous, nil, nil, items[prev].Span, ""
		}
		if symbol != nil {
			return Local, symbol, nil, items[prev].Span, ""
		}
	}

	if context.relation {
		return Relation, nil, a.sqlReference(i, name, nil), Span{}, ""
	}
	if context.alias {
		return Alias, nil, a.sqlReference(i, name, nil), Span{}, ""
	}
	if prev >= 0 && items[prev].Token.Kind == token.Period {
		qualifier := nameBeforePeriod(a.Text, items, prev)
		return Column, nil, a.sqlReference(i, name, qualifier), Span{}, ""
	}
	if next < len(items) && items[next].Token.Kind == token.Period {
		return Alias, nil, a.sqlReference(i, name, nil), Span{}, ""
	}
	if (context.kind == contextUnsupported || context.kind == contextExecutePending) && symbol != nil {
		if !duplicate {
			symbol.RenameBlocked = firstReason(symbol.RenameBlocked, "unsupported syntax may contain a local occurrence")
			return Ambiguous, nil, a.sqlReference(i, name, nil), Span{}, ""
		}
		return Ambiguous, nil, nil, Span{}, ""
	}
	if context.insertColumn || context.updateTarget {
		return Column, nil, a.sqlReference(i, name, nil), Span{}, ""
	}
	if isCallable(items, i) || isProcedureCallName(items, i) {
		return Callable, nil, a.sqlReference(i, name, nil), Span{}, ""
	}
	if context.kind == contextUnsupported || context.kind == contextExecutePending {
		if symbol != nil && !duplicate {
			symbol.RenameBlocked = firstReason(symbol.RenameBlocked, "unsupported syntax may contain a local occurrence")
			return Ambiguous, nil, a.sqlReference(i, name, nil), Span{}, ""
		}
		if duplicate {
			return Ambiguous, nil, nil, Span{}, ""
		}
		return Other, nil, nil, Span{}, ""
	}

	if context.kind == contextExecute {
		if context.outputTarget && symbol != nil && !duplicate {
			return Local, symbol, nil, colonPrefix(items, i), ""
		}
		if symbol != nil && !duplicate {
			return Local, symbol, nil, Span{}, ""
		}
		if duplicate {
			return Ambiguous, nil, nil, Span{}, ""
		}
		return Column, nil, a.sqlReference(i, name, nil), Span{}, ""
	}
	if context.kind == contextSQL {
		if context.outputTarget && symbol != nil && !duplicate {
			return Local, symbol, nil, colonPrefix(items, i), ""
		}
		if symbol != nil {
			symbol.RenameBlocked = firstReason(symbol.RenameBlocked, "ambiguous SQL value expression")
			return Ambiguous, nil, a.sqlReference(i, name, nil), Span{}, ""
		}
		if duplicate {
			return Ambiguous, nil, a.sqlReference(i, name, nil), Span{}, ""
		}
		return Column, nil, a.sqlReference(i, name, nil), Span{}, ""
	}
	if procIndex >= 0 {
		if duplicate {
			return Ambiguous, nil, nil, Span{}, ""
		}
		if symbol != nil {
			return Local, symbol, nil, Span{}, ""
		}
	}
	return Other, nil, nil, Span{}, ""
}

func (a *Analysis) sqlReference(index int, name Name, qualifier *Name) *SQLReference {
	var scopes [][]RelationRef
	if index >= 0 && index < len(a.sqlScopes) {
		scopes = a.sqlScopes[index]
	}
	return &SQLReference{Name: name, Qualifier: qualifier, Scopes: scopes}
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
