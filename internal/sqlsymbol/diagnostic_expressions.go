package sqlsymbol

import (
	"math/big"
	"sort"

	"github.com/sqls-server/sqls/token"
)

// maxExpressionDepth bounds recursive expression-fact evaluation
// (arithmetic operand recursion, CAST's inner expression, and CASE/COALESCE
// branch recursion). 30 is comfortably deeper than any expression a human
// author writes by hand, while still cheaply bounding stack depth and CPU
// time against a pathological or maliciously deep nested expression. When
// the budget is exceeded, the offending sub-expression reports unknown
// rather than continuing to recurse.
const maxExpressionDepth = 30

// expressionFact reports span's best-provable expressionFact, memoized by
// span within this diagnosticModel instance. The cache is per-analysis
// (per diagnosticModel), never shared across documents or requests.
func (m *diagnosticModel) expressionFact(span Span) expressionFact {
	if m.exprFactCache == nil {
		m.exprFactCache = make(map[Span]expressionFact)
	}
	if fact, ok := m.exprFactCache[span]; ok {
		return fact
	}
	fact := m.computeExpressionFact(m.itemsInSpan(span), 0)
	m.exprFactCache[span] = fact
	return fact
}

// itemsInSpan returns the contiguous run of m.items whose own spans fall
// entirely within span, assuming m.items is sorted by Span.Start (true of
// every diagnosticModel, built from significantLexemes in document order).
func (m *diagnosticModel) itemsInSpan(span Span) []lexeme {
	start := sort.Search(len(m.items), func(i int) bool {
		return m.items[i].Span.Start >= span.Start
	})
	end := start
	for end < len(m.items) && m.items[end].Span.End <= span.End {
		end++
	}
	return m.items[start:end]
}

func isArithmeticOperatorToken(kind token.Kind) bool {
	switch kind {
	case token.Plus, token.Minus, token.Mult, token.Div:
		return true
	default:
		return false
	}
}

// computeExpressionFact evaluates items (already isolated to one
// expression's own lexemes) at recursion depth, returning the all-zero-value
// expressionFact for anything unrecognized, unbalanced, or past
// maxExpressionDepth -- never guessing, never panicking.
func (m *diagnosticModel) computeExpressionFact(items []lexeme, depth int) expressionFact {
	if depth > maxExpressionDepth {
		return expressionFact{}
	}
	items = trimExpressionParens(items)
	if len(items) == 0 || !balancedExpression(items) {
		return expressionFact{}
	}

	// A unary minus immediately before a numeric literal is a negative
	// constant, not a binary operator: its exact value is known directly,
	// with no arithmetic type-inference needed.
	if len(items) == 2 && items[0].Token.Kind == token.Minus && items[1].Token.Kind == token.Number {
		return negatedNumberFact(m.analysis.Text, items[1])
	}

	if fact, ok := m.concatenationFact(items, depth); ok {
		return fact
	}
	if hasTopLevelConcatenation(m.analysis.Text, items) {
		return expressionFact{}
	}

	// Additive (+/-) is tried before multiplicative (*//) so the outermost
	// split happens at the lower-precedence operator first, matching
	// ordinary operator precedence; operands between splits are themselves
	// recursively evaluated, which is where */ within them gets handled.
	if fact, ok := m.additiveFact(items, depth); ok {
		return fact
	}
	if fact, ok := m.multiplicativeFact(items, depth); ok {
		return fact
	}

	if len(items) == 1 {
		return m.singleTokenFact(items[0])
	}

	if len(items) >= 2 && isWord(items[0], "CASE") && isWord(items[len(items)-1], "END") {
		fact, ok, recognized := m.caseFact(items, depth)
		if recognized {
			if ok {
				return fact
			}
			return expressionFact{}
		}
	}

	if fact, ok, recognized := m.callFact(items, depth); recognized {
		if ok {
			return fact
		}
		return expressionFact{}
	}

	if fact, ok := m.identifierFact(items); ok {
		return fact
	}
	return expressionFact{}
}

func numberFact(text string, item lexeme) expressionFact {
	raw, ok := item.Token.Value.(string)
	if !ok {
		raw = text[item.Span.Start:item.Span.End]
	}
	value, ok := new(big.Rat).SetString(raw)
	if !ok {
		return expressionFact{}
	}
	return expressionFact{Value: value, Nullability: NotNullable}
}

func negatedNumberFact(text string, item lexeme) expressionFact {
	fact := numberFact(text, item)
	if fact.Value == nil {
		return expressionFact{}
	}
	fact.Value = new(big.Rat).Neg(fact.Value)
	return fact
}

// singleTokenFact evaluates a one-lexeme expression: a literal, NULL, or a
// bare identifier.
func (m *diagnosticModel) singleTokenFact(item lexeme) expressionFact {
	text := m.analysis.Text
	switch item.Token.Kind {
	case token.Number:
		return numberFact(text, item)
	case token.SingleQuotedString, token.NationalStringLiteral:
		width, ok := stringLiteralWidth(text, item)
		if !ok {
			return expressionFact{}
		}
		return expressionFact{Type: sqlType{Family: familyCharacter, CharacterWidth: width}, Nullability: NotNullable}
	case token.SQLKeyword:
		if isWord(item, "NULL") {
			return expressionFact{Nullability: Nullable}
		}
	}
	if fact, ok := m.identifierFact([]lexeme{item}); ok {
		return fact
	}
	return expressionFact{}
}

// additiveFact finds every top-level (paren-depth 0, relative to items) +
// or - operator that is a genuine binary operator -- not a unary sign
// immediately following another operator -- and folds the operands left to
// right using exact big.Rat arithmetic. ok is false when no such operator
// is present at all (the caller then tries the next precedence level).
func (m *diagnosticModel) additiveFact(items []lexeme, depth int) (expressionFact, bool) {
	splits := topLevelArithmeticSplits(items, token.Plus, token.Minus)
	if len(splits) == 0 {
		return expressionFact{}, false
	}
	if depth+1 > maxExpressionDepth {
		return expressionFact{}, true
	}
	value := m.computeExpressionFact(items[:splits[0]], depth+1).Value
	known := value != nil
	for k, splitIdx := range splits {
		end := len(items)
		if k+1 < len(splits) {
			end = splits[k+1]
		}
		operand := m.computeExpressionFact(items[splitIdx+1:end], depth+1)
		if known && operand.Value != nil {
			if items[splitIdx].Token.Kind == token.Plus {
				value = new(big.Rat).Add(value, operand.Value)
			} else {
				value = new(big.Rat).Sub(value, operand.Value)
			}
		} else {
			known = false
		}
	}
	if !known {
		return expressionFact{}, true
	}
	return expressionFact{Value: value}, true
}

// multiplicativeFact mirrors additiveFact for * and /. Division has no
// citable InterBase rule in this session's available grounding material for
// its exact result on two exact operands (see report), so any top-level /
// makes the whole expression unknown rather than guessing a truncation
// rule; multiplication-only chains fold exactly via big.Rat.
func (m *diagnosticModel) multiplicativeFact(items []lexeme, depth int) (expressionFact, bool) {
	splits := topLevelArithmeticSplits(items, token.Mult, token.Div)
	if len(splits) == 0 {
		return expressionFact{}, false
	}
	for _, idx := range splits {
		if items[idx].Token.Kind == token.Div {
			return expressionFact{}, true
		}
	}
	if depth+1 > maxExpressionDepth {
		return expressionFact{}, true
	}
	value := m.computeExpressionFact(items[:splits[0]], depth+1).Value
	known := value != nil
	for k, splitIdx := range splits {
		end := len(items)
		if k+1 < len(splits) {
			end = splits[k+1]
		}
		operand := m.computeExpressionFact(items[splitIdx+1:end], depth+1)
		if known && operand.Value != nil {
			value = new(big.Rat).Mul(value, operand.Value)
		} else {
			known = false
		}
	}
	if !known {
		return expressionFact{}, true
	}
	return expressionFact{Value: value}, true
}

// topLevelArithmeticSplits returns the indices, in order, of every item in
// items matching one of kinds at paren-depth 0 relative to items, excluding
// index 0 and excluding any operator immediately following another
// arithmetic operator token (a unary sign attached to the next operand,
// never a binary split point).
func topLevelArithmeticSplits(items []lexeme, kinds ...token.Kind) []int {
	depth := 0
	var splits []int
	for i, item := range items {
		switch item.Token.Kind {
		case token.LParen:
			depth++
			continue
		case token.RParen:
			depth--
			continue
		}
		if depth != 0 || i == 0 {
			continue
		}
		matched := false
		for _, kind := range kinds {
			if item.Token.Kind == kind {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if isArithmeticOperatorToken(items[i-1].Token.Kind) {
			continue
		}
		splits = append(splits, i)
	}
	return splits
}

// concatenationFact mirrors width.go's concatenationWidth splitting
// technique exactly (the same top-level "||" scan), producing a
// familyCharacter fact whose CharacterWidth is the sum of every operand's
// own known width, or an unknown-width familyCharacter fact when any
// operand's width (though still character-typed) is not known.
func (m *diagnosticModel) concatenationFact(items []lexeme, depth int) (expressionFact, bool) {
	text := m.analysis.Text
	parenDepth := 0
	start := 0
	totalWidth := 0
	widthKnown := true
	found := false
	fold := func(part []lexeme) {
		fact := m.computeExpressionFact(part, depth+1)
		if fact.Type.Family != familyCharacter {
			widthKnown = false
			return
		}
		if !widthKnown {
			return
		}
		if fact.Type.CharacterWidth == 0 {
			widthKnown = false
			return
		}
		sum, ok := addWidthBounds(totalWidth, fact.Type.CharacterWidth)
		if !ok {
			widthKnown = false
			return
		}
		totalWidth = sum
	}
	for i := 0; i+1 < len(items); i++ {
		item := items[i]
		switch item.Token.Kind {
		case token.LParen:
			parenDepth++
		case token.RParen:
			parenDepth--
		}
		if parenDepth != 0 || item.Token.Kind != token.Char || items[i+1].Token.Kind != token.Char {
			continue
		}
		if item.Span.End != items[i+1].Span.Start || text[item.Span.Start:items[i+1].Span.End] != "||" {
			continue
		}
		if depth+1 > maxExpressionDepth {
			return expressionFact{}, true
		}
		fold(items[start:i])
		found = true
		start = i + 2
		i++
	}
	if !found {
		return expressionFact{}, false
	}
	fold(items[start:])
	if !widthKnown {
		return expressionFact{Type: sqlType{Family: familyCharacter}}, true
	}
	return expressionFact{Type: sqlType{Family: familyCharacter, CharacterWidth: totalWidth}}, true
}

// identifierFact mirrors identifierWidth's exact resolution pattern (bare,
// colon-prefixed, or qualified identifier), producing an expressionFact
// instead of a width. ok is false whenever the identifier's shape is not
// recognized at all, or a.Resolve does not resolve it to a Local variable or
// a SQL Column reference -- this is also how an unresolved identifier, a
// client bind parameter never bound to a declared local, and a bare "?"
// placeholder all fall through to unknown, per the brief.
func (m *diagnosticModel) identifierFact(items []lexeme) (expressionFact, bool) {
	var item lexeme
	switch len(items) {
	case 1:
		item = items[0]
	case 2:
		if items[0].Token.Kind != token.Colon {
			return expressionFact{}, false
		}
		item = items[1]
	case 3:
		if items[1].Token.Kind != token.Period {
			return expressionFact{}, false
		}
		item = items[2]
	default:
		return expressionFact{}, false
	}
	if _, ok := nameFromLexeme(m.analysis.Text, item); !ok || !isNameToken(item) {
		return expressionFact{}, false
	}
	resolution := m.analysis.Resolve(item.Span.Start)
	if resolution.Role == Local && resolution.Symbol != nil {
		t, ok := parseDiagnosticType(resolution.Symbol.Type, m.analysis.Variant, m.catalog)
		if !ok {
			return expressionFact{}, true
		}
		return expressionFact{Type: t}, true
	}
	if resolution.Role == Column && resolution.SQL != nil {
		return m.sqlColumnFact(resolution.SQL)
	}
	return expressionFact{}, false
}

// relationColumnFacts resolves name's column list, preferring
// SemanticCatalog.RelationInfo (which carries per-column Nullability) and
// falling back to plain Catalog.Columns (NullUnknown for every column) when
// the catalog does not implement SemanticCatalog. This mirrors
// sqlColumnWidth's own direct-catalog-lookup pattern; it does not attempt
// resolveRelationOutput's richer CTE/derived-table/DDL-invalidation
// handling, since a bare SQLReference's Scopes carries no query-index or
// item-position context for that.
func (m *diagnosticModel) relationColumnFacts(name Name) ([]ColumnFact, bool) {
	if m.semantic != nil {
		fact, knowledge := m.semantic.RelationInfo(name)
		if knowledge != Present || !fact.ColumnsKnown {
			return nil, false
		}
		return fact.Columns, true
	}
	if m.catalog == nil {
		return nil, false
	}
	cols, ok := m.catalog.Columns(name)
	if !ok {
		return nil, false
	}
	out := make([]ColumnFact, len(cols))
	for i, c := range cols {
		out[i] = ColumnFact{Name: c.Name, Type: c.Type, Nullability: NullUnknown}
	}
	return out, true
}

// sqlColumnFact mirrors sqlColumnWidth's scope-walking and ambiguity rules
// exactly, resolving to a ColumnFact (type string + Nullability) instead of
// only a width.
func (m *diagnosticModel) sqlColumnFact(reference *SQLReference) (expressionFact, bool) {
	if reference == nil {
		return expressionFact{}, false
	}
	for _, scope := range reference.Scopes {
		matches := 0
		var match ColumnFact
		allOwnersKnown := true
		eligibleOwners := 0
		for _, relation := range scope {
			if reference.Qualifier != nil && !relationMatchesQualifier(relation, *reference.Qualifier) {
				continue
			}
			eligibleOwners++
			if relation.Name.Key() == "" {
				allOwnersKnown = false
				continue
			}
			columns, ok := m.relationColumnFacts(relation.Name)
			if !ok {
				allOwnersKnown = false
				continue
			}
			for _, column := range columns {
				if !reference.Name.MatchesCatalogName(column.Name) {
					continue
				}
				matches++
				match = column
			}
		}
		if !allOwnersKnown {
			return expressionFact{}, false
		}
		if reference.Qualifier != nil && eligibleOwners == 0 {
			continue
		}
		if matches > 1 {
			return expressionFact{}, false
		}
		if matches == 0 && reference.Qualifier != nil {
			return expressionFact{}, false
		}
		if matches == 1 {
			t, ok := parseDiagnosticType(match.Type, m.analysis.Variant, m.catalog)
			if !ok {
				return expressionFact{Nullability: match.Nullability}, true
			}
			return expressionFact{Type: t, Nullability: match.Nullability}, true
		}
	}
	return expressionFact{}, false
}

// callFact recognizes CAST(expr AS type) and COALESCE(expr, ...) using
// callWidth's exact CAST argument-parsing pattern (topLevelWordIndex(...,
// "AS")); every other call syntax, including a genuine UDF/builtin this
// task does not model, is recognized (so the caller does not fall through
// to identifierFact) but reports unknown. recognized is false only when
// items is not call syntax at all.
func (m *diagnosticModel) callFact(items []lexeme, depth int) (fact expressionFact, ok bool, recognized bool) {
	if len(items) < 3 || items[0].Token.Kind != token.SQLKeyword || items[1].Token.Kind != token.LParen {
		return expressionFact{}, false, false
	}
	close, matched := matchingExpressionParen(items, 1)
	if !matched || close != len(items)-1 {
		return expressionFact{}, false, false
	}
	arguments := items[2:close]
	switch {
	case isWord(items[0], "CAST"):
		as := topLevelWordIndex(arguments, "AS")
		if as <= 0 || as >= len(arguments)-1 || topLevelWordIndex(arguments[as+1:], "AS") >= 0 {
			return expressionFact{}, false, true
		}
		typeStart, typeEnd := arguments[as+1].Span.Start, arguments[len(arguments)-1].Span.End
		t, typeOK := parseDiagnosticType(m.analysis.Text[typeStart:typeEnd], m.analysis.Variant, m.catalog)
		if !typeOK {
			return expressionFact{}, false, true
		}
		if depth+1 > maxExpressionDepth {
			return expressionFact{}, false, true
		}
		inner := m.computeExpressionFact(arguments[:as], depth+1)
		result := expressionFact{Type: t, ExplicitCast: true, Nullability: inner.Nullability}
		if inner.Value != nil {
			result.Value = inner.Value
		}
		return result, true, true
	case isWord(items[0], "COALESCE"):
		parts, split := splitTopLevel(arguments, token.Comma)
		if !split || len(parts) == 0 {
			return expressionFact{}, false, true
		}
		if depth+1 > maxExpressionDepth {
			return expressionFact{}, false, true
		}
		facts := make([]expressionFact, len(parts))
		for i, part := range parts {
			facts[i] = m.computeExpressionFact(part, depth+1)
		}
		return expressionFact{Type: combineFactTypes(facts), Nullability: coalesceNullability(facts)}, true, true
	default:
		return expressionFact{}, false, true
	}
}

// caseFact recognizes CASE ... END (both the simple and searched forms),
// scanning items for top-level WHEN/THEN/ELSE at paren-depth 0 while
// tracking nested CASE...END pairs (a nested CASE's own END must not be
// mistaken for this CASE's terminator, and its own WHEN/THEN/ELSE must not
// be mistaken for this CASE's branches). Only THEN and ELSE result
// expressions are evaluated as branches; WHEN conditions are never
// evaluated as expressionFact branches. recognized is false only when
// items is not shaped like WHEN...THEN (at least one, matched counts, and a
// terminating END already confirmed by the caller).
func (m *diagnosticModel) caseFact(items []lexeme, depth int) (fact expressionFact, ok bool, recognized bool) {
	body := items[1 : len(items)-1]
	var whenPositions, thenPositions []int
	elsePos := -1
	parenDepth := 0
	caseDepth := 0
	for i, item := range body {
		switch item.Token.Kind {
		case token.LParen:
			parenDepth++
			continue
		case token.RParen:
			parenDepth--
			continue
		}
		if parenDepth != 0 {
			continue
		}
		switch {
		case isWord(item, "CASE"):
			caseDepth++
		case isWord(item, "END"):
			if caseDepth > 0 {
				caseDepth--
			}
		case caseDepth == 0 && isWord(item, "WHEN"):
			whenPositions = append(whenPositions, i)
		case caseDepth == 0 && isWord(item, "THEN"):
			thenPositions = append(thenPositions, i)
		case caseDepth == 0 && isWord(item, "ELSE"):
			elsePos = i
		}
	}
	if len(whenPositions) == 0 || len(whenPositions) != len(thenPositions) {
		return expressionFact{}, false, true
	}
	if depth+1 > maxExpressionDepth {
		return expressionFact{}, false, true
	}

	var facts []expressionFact
	for k, thenPos := range thenPositions {
		end := len(body)
		if k+1 < len(whenPositions) {
			end = whenPositions[k+1]
		} else if elsePos >= 0 {
			end = elsePos
		}
		if thenPos+1 > end {
			return expressionFact{}, false, true
		}
		facts = append(facts, m.computeExpressionFact(body[thenPos+1:end], depth+1))
	}
	hasElse := elsePos >= 0
	if hasElse {
		facts = append(facts, m.computeExpressionFact(body[elsePos+1:], depth+1))
	}

	resultType := combineFactTypes(facts)
	var nullability Nullability
	if !hasElse {
		nullability = Nullable
	} else {
		nullability = caseNullability(facts)
	}
	return expressionFact{Type: resultType, Nullability: nullability}, true, true
}

// combineFactTypes is shared by CASE and COALESCE: when every branch shares
// the same known family, and that family is familyCharacter, the combined
// CharacterWidth is the MAX across branches (whichever branch's value ends
// up used at runtime, the wider one must be accommodated -- this is the
// safe direction, since overestimating width can only under-report, never
// falsely claim, a narrowing finding later). Any other agreement (a shared
// exact-numeric or date/time family, for instance) retreats to
// familyUnknown rather than speculatively combining ranges/precision --
// this task's deliberately conservative scope decision (see report).
// Disagreement, or any unknown branch, is always familyUnknown.
func combineFactTypes(facts []expressionFact) sqlType {
	if len(facts) == 0 {
		return sqlType{}
	}
	family := facts[0].Type.Family
	if family == familyUnknown {
		return sqlType{}
	}
	maxWidth := 0
	widthKnown := true
	for _, f := range facts {
		if f.Type.Family != family {
			return sqlType{}
		}
		if family == familyCharacter {
			if f.Type.CharacterWidth == 0 {
				widthKnown = false
			} else if f.Type.CharacterWidth > maxWidth {
				maxWidth = f.Type.CharacterWidth
			}
		}
	}
	if family != familyCharacter {
		return sqlType{}
	}
	if !widthKnown {
		return sqlType{Family: familyCharacter}
	}
	return sqlType{Family: familyCharacter, CharacterWidth: maxWidth}
}

// caseNullability requires every branch (WHEN...THEN results plus an
// explicit ELSE; the caller handles a missing ELSE separately as an
// implicit ELSE NULL) to be provably NotNullable before claiming
// NotNullable -- per the brief, "do not claim NotNullable unless every
// branch ... is provably non-nullable." Nullable is claimed only when every
// branch's nullability is actually known and at least one is Nullable;
// otherwise NullUnknown.
func caseNullability(facts []expressionFact) Nullability {
	allNotNull := true
	allKnown := true
	anyNullable := false
	for _, f := range facts {
		switch f.Nullability {
		case NotNullable:
		case Nullable:
			allNotNull = false
			anyNullable = true
		default:
			allNotNull = false
			allKnown = false
		}
	}
	switch {
	case allNotNull:
		return NotNullable
	case allKnown && anyNullable:
		return Nullable
	default:
		return NullUnknown
	}
}

// coalesceNullability uses COALESCE's real, well-defined semantics rather
// than CASE's more conservative all-branches rule: COALESCE returns the
// first non-null argument, so it is provably NotNullable as soon as ANY
// argument is provably NotNullable (a very common idiom, e.g.
// COALESCE(nullable_col, 0)), regardless of the other arguments'
// nullability or order. It is Nullable only when every argument is known
// and every one is individually Nullable (the only way every argument could
// simultaneously be null on some row); anything else is NullUnknown.
func coalesceNullability(facts []expressionFact) Nullability {
	allNullable := true
	for _, f := range facts {
		if f.Nullability == NotNullable {
			return NotNullable
		}
		if f.Nullability != Nullable {
			allNullable = false
		}
	}
	if allNullable {
		return Nullable
	}
	return NullUnknown
}
