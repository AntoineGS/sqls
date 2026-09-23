package sqlsymbol

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sqls-server/sqls/token"
)

type widthDestination struct {
	span     Span
	typeName string
	label    string
}

// widthDiagnostics pairs only source and destination expressions whose
// ownership is proven by the procedure binder or explicit SQL target lists.
func (a *Analysis) widthDiagnostics(c Catalog) []Finding {
	if a == nil || len(a.lexemes) == 0 || len(a.contexts) == 0 {
		return nil
	}
	items := significantLexemes(a.lexemes)
	findings := make([]Finding, 0)
	findings = append(findings, a.procedureAssignmentFindings(items, c)...)
	findings = append(findings, a.updateFindings(items, c)...)
	findings = append(findings, a.insertFindings(items, c)...)
	findings = append(findings, a.selectIntoFindings(items, c)...)
	sort.SliceStable(findings, func(i, j int) bool {
		return findings[i].Span.Start < findings[j].Span.Start
	})
	return findings
}

func (a *Analysis) procedureAssignmentFindings(items []lexeme, c Catalog) []Finding {
	indices := make(map[Span]int, len(items))
	for i, item := range items {
		indices[item.Span] = i
	}
	var findings []Finding
	for _, symbol := range a.Symbols {
		destinationWidth, known := stringTypeWidth(symbol.Type)
		if !known {
			continue
		}
		for _, write := range symbol.Writes {
			index, ok := indices[write]
			if !ok || index+2 >= len(items) || items[index+1].Token.Kind != token.Eq {
				continue
			}
			end, complete := completeStatementEnd(items, index+2)
			if !complete || end == index+2 || !balancedExpression(items[index+2:end]) {
				continue
			}
			source := items[index+2 : end]
			sourceWidth, known := a.expressionWidth(source, c)
			if !known || sourceWidth <= destinationWidth {
				continue
			}
			findings = append(findings, a.widthFinding(source, write, symbol.Name.Text, sourceWidth, destinationWidth))
		}
	}
	return findings
}

func (a *Analysis) updateFindings(items []lexeme, c Catalog) []Finding {
	depths, matching := statementSQLDepths(items)
	var findings []Finding
	for i, item := range items {
		if !isWord(item, "UPDATE") || !a.inSQLContext(i) {
			continue
		}
		end, complete := completeStatementEnd(items, i+1)
		if !complete {
			continue
		}
		if depths[i] != 0 {
			continue
		}
		targetIndex := nextName(items, i+1, end)
		if targetIndex < 0 {
			continue
		}
		target, afterTarget, ok := relationAt(a.Text, items, targetIndex, end, depths, matching, false)
		if !ok || target.Name.Key() == "" {
			continue
		}
		set := topLevelWordIndex(items[i+1:end], "SET")
		if set < 0 {
			continue
		}
		set += i + 1
		if set < afterTarget || set+1 >= end {
			continue
		}
		setEnd := end
		for _, clause := range []string{"WHERE", "ORDER", "ROWS", "RETURNING", "PLAN"} {
			if relative := topLevelWordIndex(items[set+1:end], clause); relative >= 0 {
				candidate := set + 1 + relative
				if candidate < setEnd {
					setEnd = candidate
				}
			}
		}
		assignments, ok := splitTopLevel(items[set+1:setEnd], token.Comma)
		if !ok || !completeAssignmentList(a.Text, assignments) || !completeSelectTail(a.Text, items[setEnd:end]) {
			continue
		}
		statementFindings := make([]Finding, 0)
		for _, assignment := range assignments {
			eq := topLevelTokenIndex(assignment, token.Eq)
			destination, ok := updateDestination(a.Text, assignment[:eq], target, c)
			if !ok {
				continue
			}
			statementFindings = append(statementFindings, a.findingForDestination(assignment[eq+1:], destination, c)...)
		}
		findings = append(findings, statementFindings...)
		i = end
	}
	return findings
}

func updateDestination(text string, items []lexeme, target RelationRef, c Catalog) (widthDestination, bool) {
	var nameItem lexeme
	var name Name
	switch len(items) {
	case 1:
		nameItem = items[0]
	case 3:
		if items[1].Token.Kind != token.Period {
			return widthDestination{}, false
		}
		qualifier, ok := nameFromLexeme(text, items[0])
		if !ok || !relationMatchesQualifier(target, qualifier) {
			return widthDestination{}, false
		}
		nameItem = items[2]
	default:
		return widthDestination{}, false
	}
	if !isNameToken(nameItem) {
		return widthDestination{}, false
	}
	name, ok := nameFromLexeme(text, nameItem)
	if !ok {
		return widthDestination{}, false
	}
	typeName, ok := catalogColumnType(c, target.Name, name)
	if !ok {
		return widthDestination{}, false
	}
	return widthDestination{
		span:     nameItem.Span,
		typeName: typeName,
		label:    target.Name.Text + "." + name.Text,
	}, true
}

func (a *Analysis) insertFindings(items []lexeme, c Catalog) []Finding {
	depths, matching := statementSQLDepths(items)
	var findings []Finding
	for i, item := range items {
		if !isWord(item, "INSERT") || !a.inSQLContext(i) || depths[i] != 0 {
			continue
		}
		end, complete := completeStatementEnd(items, i+1)
		if !complete {
			continue
		}
		insertFindings, ok := a.parseInsert(items, i, end, depths, matching, c)
		if ok {
			findings = append(findings, insertFindings...)
		}
		i = end
	}
	return findings
}

func (a *Analysis) parseInsert(items []lexeme, start, end int, depths []int, matching map[int]int, c Catalog) ([]Finding, bool) {
	if start+2 >= end || !isWord(items[start+1], "INTO") {
		return nil, false
	}
	targetIndex := nextName(items, start+2, end)
	if targetIndex < 0 {
		return nil, false
	}
	target, next, ok := relationAt(a.Text, items, targetIndex, end, depths, matching, false)
	if !ok || target.Name.Key() == "" || next >= end || items[next].Token.Kind != token.LParen {
		return nil, false
	}
	columnItems, afterColumns, ok := enclosedList(items, next, end)
	if !ok {
		return nil, false
	}
	columnParts, ok := splitTopLevel(columnItems, token.Comma)
	if !ok {
		return nil, false
	}
	columns := make([]widthDestination, len(columnParts))
	for i, part := range columnParts {
		if len(part) != 1 || !isNameToken(part[0]) {
			return nil, false
		}
		name, ok := nameFromLexeme(a.Text, part[0])
		if !ok {
			return nil, false
		}
		typeName, known := catalogColumnType(c, target.Name, name)
		columns[i] = widthDestination{
			span:     part[0].Span,
			typeName: typeName,
			label:    target.Name.Text + "." + name.Text,
		}
		if !known {
			columns[i].typeName = ""
		}
	}
	if afterColumns >= end {
		return nil, false
	}
	switch {
	case isWord(items[afterColumns], "VALUES"):
		return a.insertValuesFindings(items, afterColumns+1, end, columns, c)
	case isWord(items[afterColumns], "SELECT"):
		return a.insertSelectFindings(items, afterColumns, end, columns, c)
	default:
		return nil, false
	}
}

func (a *Analysis) insertValuesFindings(items []lexeme, start, end int, columns []widthDestination, c Catalog) ([]Finding, bool) {
	cursor := start
	var findings []Finding
	for cursor < end {
		if items[cursor].Token.Kind != token.LParen {
			return nil, false
		}
		values, next, ok := enclosedList(items, cursor, end)
		if !ok {
			return nil, false
		}
		parts, ok := splitTopLevel(values, token.Comma)
		if !ok || len(parts) != len(columns) {
			return nil, false
		}
		for i, value := range parts {
			findings = append(findings, a.findingForDestination(value, columns[i], c)...)
		}
		cursor = next
		if cursor == end {
			return findings, true
		}
		if items[cursor].Token.Kind != token.Comma || cursor+1 >= end {
			return nil, false
		}
		cursor++
	}
	return nil, false
}

func (a *Analysis) insertSelectFindings(items []lexeme, selectIndex, end int, columns []widthDestination, c Catalog) ([]Finding, bool) {
	fromRelative := topLevelWordIndex(items[selectIndex+1:end], "FROM")
	if fromRelative <= 0 {
		return nil, false
	}
	from := selectIndex + 1 + fromRelative
	if from+1 >= end || (!isNameToken(items[from+1]) && items[from+1].Token.Kind != token.LParen) {
		return nil, false
	}
	if !completeSelectTail(a.Text, items[from+1:end]) || !completeFromRelations(items[from+1:end]) {
		return nil, false
	}
	projections, ok := splitTopLevel(items[selectIndex+1:from], token.Comma)
	if !ok || len(projections) != len(columns) {
		return nil, false
	}
	var findings []Finding
	for i, projection := range projections {
		destination := columns[i]
		destination.span = Span{Start: projection[0].Span.Start, End: projection[len(projection)-1].Span.End}
		findings = append(findings, a.findingForDestination(projection, destination, c)...)
	}
	return findings, true
}

func (a *Analysis) selectIntoFindings(items []lexeme, c Catalog) []Finding {
	var findings []Finding
	for i, item := range items {
		if !isWord(item, "SELECT") || !a.inSQLContext(i) {
			continue
		}
		end, complete := completeStatementEnd(items, i+1)
		if !complete {
			continue
		}
		selectFindings, ok := a.parseSelectInto(items, i, end, c)
		if ok {
			findings = append(findings, selectFindings...)
		}
	}
	return findings
}

func (a *Analysis) parseSelectInto(items []lexeme, selectIndex, end int, c Catalog) ([]Finding, bool) {
	intoRelative := topLevelWordIndex(items[selectIndex+1:end], "INTO")
	if intoRelative <= 0 {
		return nil, false
	}
	into := selectIndex + 1 + intoRelative
	fromRelative := topLevelWordIndex(items[into+1:end], "FROM")
	if fromRelative < 0 {
		return nil, false
	}
	from := into + 1 + fromRelative
	if from+1 >= end || (!isNameToken(items[from+1]) && items[from+1].Token.Kind != token.LParen) {
		return nil, false
	}
	if !completeSelectTail(a.Text, items[from+1:end]) || !completeFromRelations(items[from+1:end]) {
		return nil, false
	}
	projections, ok := splitTopLevel(items[selectIndex+1:into], token.Comma)
	if !ok {
		return nil, false
	}
	targets, ok := splitTopLevel(items[into+1:from], token.Comma)
	if !ok || len(projections) != len(targets) {
		return nil, false
	}
	destinations := make([]widthDestination, len(targets))
	for i, target := range targets {
		nameIndex := 0
		if len(target) == 2 && target[0].Token.Kind == token.Colon {
			nameIndex = 1
		} else if len(target) != 1 {
			continue
		}
		if !isNameToken(target[nameIndex]) {
			continue
		}
		resolution := a.Resolve(target[nameIndex].Span.Start)
		if resolution.Role != Local || resolution.Symbol == nil {
			continue
		}
		destinations[i] = widthDestination{
			span:     resolution.Span,
			typeName: resolution.Symbol.Type,
			label:    resolution.Symbol.Name.Text,
		}
	}
	var findings []Finding
	for i, projection := range projections {
		findings = append(findings, a.findingForDestination(projection, destinations[i], c)...)
	}
	return findings, true
}

func (a *Analysis) findingForDestination(source []lexeme, destination widthDestination, c Catalog) []Finding {
	destinationWidth, known := stringTypeWidth(destination.typeName)
	if !known || len(source) == 0 || !balancedExpression(source) {
		return nil
	}
	sourceWidth, known := a.expressionWidth(source, c)
	if !known || sourceWidth <= destinationWidth {
		return nil
	}
	return []Finding{a.widthFinding(source, destination.span, destination.label, sourceWidth, destinationWidth)}
}

func (a *Analysis) widthFinding(source []lexeme, destination Span, destinationName string, sourceWidth, destinationWidth int) Finding {
	sourceText := strings.TrimSpace(a.Text[source[0].Span.Start:source[len(source)-1].Span.End])
	return Finding{
		Span:     destination,
		Code:     "interbase-string-truncation",
		Message:  fmt.Sprintf("Possible string truncation assigning %s (width %d) to %s (width %d)", sourceText, sourceWidth, destinationName, destinationWidth),
		Severity: 2,
	}
}

func (a *Analysis) inSQLContext(index int) bool {
	return index >= 0 && index < len(a.contexts) && a.contexts[index].kind == contextSQL
}

func catalogColumnType(c Catalog, table, column Name) (string, bool) {
	if c == nil {
		return "", false
	}
	columns, ok := c.Columns(table)
	if !ok {
		return "", false
	}
	var match string
	matches := 0
	for _, candidate := range columns {
		if column.MatchesCatalogName(candidate.Name) {
			matches++
			match = candidate.Type
		}
	}
	return match, matches == 1
}

func completeStatementEnd(items []lexeme, start int) (int, bool) {
	depth := 0
	for i := start; i < len(items); i++ {
		switch items[i].Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
			if depth < 0 {
				return 0, false
			}
		case token.Semicolon:
			if depth != 0 {
				return 0, false
			}
			return i, true
		}
	}
	return 0, false
}

func enclosedList(items []lexeme, open, limit int) ([]lexeme, int, bool) {
	if open < 0 || open >= limit || items[open].Token.Kind != token.LParen {
		return nil, open, false
	}
	depth := 0
	for i := open; i < limit; i++ {
		switch items[i].Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
			if depth == 0 {
				if i == open+1 {
					return nil, i + 1, false
				}
				return items[open+1 : i], i + 1, true
			}
			if depth < 0 {
				return nil, open, false
			}
		case token.Semicolon:
			return nil, open, false
		}
	}
	return nil, open, false
}

func splitTopLevel(items []lexeme, separator token.Kind) ([][]lexeme, bool) {
	if len(items) == 0 {
		return nil, false
	}
	depth := 0
	start := 0
	parts := make([][]lexeme, 0, 2)
	for i, item := range items {
		switch item.Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
			if depth < 0 {
				return nil, false
			}
		}
		if depth == 0 && item.Token.Kind == separator {
			if start == i {
				return nil, false
			}
			parts = append(parts, items[start:i])
			start = i + 1
		}
	}
	if depth != 0 || start == len(items) {
		return nil, false
	}
	parts = append(parts, items[start:])
	return parts, true
}

func topLevelTokenIndex(items []lexeme, kind token.Kind) int {
	depth := 0
	for i, item := range items {
		switch item.Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
		}
		if depth == 0 && item.Token.Kind == kind {
			return i
		}
	}
	return -1
}

func completeAssignmentList(text string, assignments [][]lexeme) bool {
	if len(assignments) == 0 {
		return false
	}
	for _, assignment := range assignments {
		if len(assignment) < 3 {
			return false
		}
		eq := topLevelTokenIndex(assignment, token.Eq)
		if eq <= 0 || eq >= len(assignment)-1 || !completeExpression(text, assignment[eq+1:]) {
			return false
		}
	}
	return true
}

func completeSelectTail(text string, items []lexeme) bool {
	if topLevelWordIndex(items, "UNION") >= 0 || topLevelWordIndex(items, "RETURNING") >= 0 || topLevelWordIndex(items, "RETURNING_VALUES") >= 0 {
		return false
	}
	if !completeClauseExpression(text, items, "WHERE", "", []string{"GROUP", "HAVING", "ORDER", "ROWS", "PLAN"}) ||
		!completeClauseExpression(text, items, "GROUP", "BY", []string{"HAVING", "ORDER", "ROWS", "PLAN"}) ||
		!completeClauseExpression(text, items, "HAVING", "", []string{"ORDER", "ROWS", "PLAN"}) ||
		!completeClauseExpression(text, items, "ORDER", "BY", []string{"ROWS", "PLAN"}) ||
		!completeClauseExpression(text, items, "ROWS", "", []string{"PLAN"}) ||
		!completeClauseExpression(text, items, "PLAN", "", nil) ||
		!completeClauseExpression(text, items, "ON", "", []string{"JOIN", "WHERE", "GROUP", "HAVING", "ORDER", "ROWS", "PLAN"}) {
		return false
	}
	depth := 0
	for i, item := range items {
		switch item.Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
		}
		if depth == 0 && isWord(item, "JOIN") {
			if i+1 >= len(items) || (!isNameToken(items[i+1]) && items[i+1].Token.Kind != token.LParen) {
				return false
			}
		}
	}
	return true
}

func completeFromRelations(items []lexeme) bool {
	relationEnd := len(items)
	for _, clause := range []string{"WHERE", "GROUP", "HAVING", "ORDER", "ROWS", "PLAN", "UNION", "RETURNING", "RETURNING_VALUES"} {
		if index := topLevelWordIndex(items, clause); index >= 0 && index < relationEnd {
			relationEnd = index
		}
	}
	items = items[:relationEnd]
	if len(items) == 0 || (!isNameToken(items[0]) && items[0].Token.Kind != token.LParen) {
		return false
	}

	depth := 0
	hasJoinPredicate := false
	for i, item := range items {
		switch item.Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
		}
		if depth != 0 {
			continue
		}
		if item.Token.Kind == token.Comma {
			if i == 0 || i+1 >= len(items) || !relationEndToken(items[i-1]) || !relationStartToken(items[i+1]) {
				return false
			}
		}
		if isWord(item, "JOIN") && (i == 0 || i+1 >= len(items) || !relationStartToken(items[i+1])) {
			return false
		}
		if isWord(item, "ON") {
			hasJoinPredicate = true
		}
	}
	last := items[len(items)-1]
	if last.Token.Kind == token.Comma || last.Token.Kind == token.Period ||
		isWord(last, "JOIN") || isWord(last, "ON") || isWord(last, "AS") ||
		(!hasJoinPredicate && !relationEndToken(last)) {
		return false
	}
	return depth == 0
}

func relationStartToken(item lexeme) bool {
	return isNameToken(item) || item.Token.Kind == token.LParen
}

func relationEndToken(item lexeme) bool {
	return isNameToken(item) || item.Token.Kind == token.RParen
}

func completeClauseExpression(text string, items []lexeme, clause, requiredWord string, boundaries []string) bool {
	clauseIndex := topLevelWordIndex(items, clause)
	if clauseIndex < 0 {
		return true
	}
	bodyStart := clauseIndex + 1
	if requiredWord != "" {
		if bodyStart >= len(items) || !isWord(items[bodyStart], requiredWord) {
			return false
		}
		bodyStart++
	}
	bodyEnd := len(items)
	for _, boundary := range boundaries {
		if relative := topLevelWordIndex(items[bodyStart:], boundary); relative >= 0 && bodyStart+relative < bodyEnd {
			bodyEnd = bodyStart + relative
		}
	}
	return completeExpression(text, items[bodyStart:bodyEnd])
}

func completeExpression(text string, items []lexeme) bool {
	if len(items) == 0 || !balancedExpression(items) {
		return false
	}
	first, last := items[0], items[len(items)-1]
	switch first.Token.Kind {
	case token.Eq, token.Neq, token.Lt, token.Gt, token.LtEq, token.GtEq, token.Period, token.Comma:
		return false
	case token.Char:
		if text[first.Span.Start:first.Span.End] == "|" {
			return false
		}
	}
	switch last.Token.Kind {
	case token.Eq, token.Neq, token.Lt, token.Gt, token.LtEq, token.GtEq,
		token.Plus, token.Minus, token.Mult, token.Div, token.Caret, token.Mod,
		token.Comma, token.Period, token.Colon, token.DoubleColon:
		return false
	case token.Char:
		if text[last.Span.Start:last.Span.End] == "|" {
			return false
		}
	case token.SQLKeyword:
		for _, word := range []string{"AND", "OR", "IS", "LIKE", "NOT", "IN", "BETWEEN", "FROM", "FOR", "AS", "BY", "TO"} {
			if isWord(last, word) {
				return false
			}
		}
	}
	return true
}

func statementSQLDepths(items []lexeme) ([]int, map[int]int) {
	depths := make([]int, len(items))
	matching := make(map[int]int)
	stack := make([]int, 0, 8)
	depth := 0
	for i, item := range items {
		depths[i] = depth
		if item.Token.Kind == token.Semicolon {
			depth = 0
			stack = stack[:0]
			continue
		}
		switch item.Token.Kind {
		case token.LParen:
			stack = append(stack, i)
			depth++
		case token.RParen:
			if depth == 0 {
				continue
			}
			depth--
			open := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			matching[open] = i
		}
	}
	return depths, matching
}
