package sqlsymbol

import (
	"strings"

	"github.com/sqls-server/sqls/token"
)

const expressionReadTokenBudget = 128
const expressionReadDepthBudget = 32

// proceduralExpressionReadFindings recognizes only complete arithmetic and
// comparison expressions in procedural conditions and assignment RHSs.
func (a *Analysis) proceduralExpressionReadFindings(m *diagnosticModel) []Finding {
	items := significantLexemes(a.lexemes)
	_, matching := sqlDepths(items)
	bySpan := make(map[Span]int, len(items))
	for i, item := range items {
		bySpan[item.Span] = i
	}
	var findings []Finding
	check := func(start, end, proc int) {
		if proc < 0 || start >= end || end-start > expressionReadTokenBudget {
			return
		}
		for idx := start; idx < end; idx++ {
			if idx >= len(a.contexts) || a.contexts[idx].kind != contextProcedure || m.Unsupported(idx) || withinAny(a.malformedDeclarations, items[idx].Span) {
				return
			}
		}
		ids, ok := parseDiagnosticReadExpression(items[start:end])
		if !ok {
			return
		}
		for i := range ids {
			ids[i] += start
		}
		groups := a.procedures[proc].Symbols
		declared := make(map[string]int)
		candidates := procedureCandidateNames(a, proc)
		for _, group := range groups {
			if len(group) > 0 {
				declared[group[0].Name.Key()] = len(group)
			}
		}
		for _, id := range ids {
			name, valid := nameFromLexeme(a.Text, items[id])
			if !valid || isNonVariableExpressionName(name) {
				continue
			}
			if declared[name.Key()] > 0 || m.Unsupported(id) {
				continue
			}
			findings = append(findings, unknownVariableFinding(items[id].Span, name, candidates))
		}
	}
	for i, item := range items {
		proc := a.procedureAt[i]
		if proc < 0 || (a.contexts[i].kind != contextProcedure) || m.Unsupported(i) {
			continue
		}
		if (isWord(item, "IF") || isWord(item, "WHILE")) && i+1 < len(items) && items[i+1].Token.Kind == token.LParen {
			close, ok := matching[i+1]
			if !ok || close <= i+2 || close+1 >= len(items) {
				continue
			}
			if isWord(item, "IF") && !isWord(items[close+1], "THEN") || isWord(item, "WHILE") && !isWord(items[close+1], "DO") {
				continue
			}
			check(i+2, close, proc)
		}
	}
	for _, symbol := range a.Symbols {
		for _, span := range symbol.Writes {
			i, ok := bySpan[span]
			if !ok || i+2 >= len(items) || items[i+1].Token.Kind != token.Eq {
				continue
			}
			proc := a.procedureAt[i]
			if proc < 0 || m.Unsupported(i) || a.contexts[i].kind != contextProcedure {
				continue
			}
			end, complete := completeStatementEnd(items, i+2)
			if !complete {
				continue
			}
			check(i+2, end, proc)
		}
	}
	return findings
}

func (trg *triggerModel) expressionReadFindings(text string, items []lexeme, m *diagnosticModel) []Finding {
	_, matching := sqlDepths(items)
	ctx := newTriggerBodyContext(items, trg.bodyStart, trg.bodyEnd)
	var findings []Finding
	check := func(start, end int) {
		if start >= end || end-start > expressionReadTokenBudget {
			return
		}
		for idx := start; idx < end; idx++ {
			rel := idx - trg.bodyStart
			if rel < 0 || rel >= len(ctx.inSQL) || ctx.inSQL[rel] || ctx.unsupported[rel] || m.Unsupported(idx) {
				return
			}
		}
		ids, ok := parseDiagnosticReadExpression(items[start:end])
		if !ok {
			return
		}
		for i := range ids {
			ids[i] += start
		}
		for _, id := range ids {
			name, valid := nameFromLexeme(text, items[id])
			if !valid || isNonVariableExpressionName(name) || m.Unsupported(id) || ctx.unsupported[id-trg.bodyStart] {
				continue
			}
			if m.analysis != nil && id < len(m.analysis.contexts) && m.analysis.contexts[id].kind == contextSQL {
				continue
			}
			if _, ambiguous := trg.locals[name.Key()]; ambiguous {
				continue
			}
			if _, ok := trg.symbolFor(name); ok {
				continue
			}
			findings = append(findings, unknownVariableFinding(items[id].Span, name, trg.candidateNames()))
		}
	}
	for i := trg.bodyStart + 1; i < trg.bodyEnd; i++ {
		rel := i - trg.bodyStart
		if ctx.inSQL[rel] || ctx.unsupported[rel] || m.Unsupported(i) {
			continue
		}
		if (isWord(items[i], "IF") || isWord(items[i], "WHILE")) && i+1 < trg.bodyEnd && items[i+1].Token.Kind == token.LParen {
			close, ok := matching[i+1]
			if !ok || close >= trg.bodyEnd {
				continue
			}
			if isWord(items[i], "IF") && (close+1 >= trg.bodyEnd || !isWord(items[close+1], "THEN")) || isWord(items[i], "WHILE") && (close+1 >= trg.bodyEnd || !isWord(items[close+1], "DO")) {
				continue
			}
			check(i+2, close)
		}
		if isNameToken(items[i]) && i+2 < trg.bodyEnd && items[i+1].Token.Kind == token.Eq && isTriggerAssignmentBoundary(items[i-1]) {
			end, ok := completeStatementEnd(items, i+2)
			if ok && end <= trg.bodyEnd {
				check(i+2, end)
			}
		}
	}
	return findings
}

func isNonVariableExpressionName(name Name) bool {
	if name.Quoted {
		return false
	}
	switch strings.ToUpper(name.Key()) {
	case "NEW", "OLD", "NULL", "TRUE", "FALSE", "UNKNOWN", "CURRENT_DATE", "CURRENT_TIME", "CURRENT_TIMESTAMP", "CURRENT_USER", "USER", "CURRENT_ROLE", "SQLCODE", "GDSCODE", "ROW_COUNT", "SQLSTATE":
		return true
	}
	return false
}

// parseDiagnosticReadExpression is a deliberately closed Pratt parser. It
// returns identifier operand indexes only if every token belongs to the
// supported grammar; calls, strings, binds, qualifiers and partial parses
// fail transactionally.
func parseDiagnosticReadExpression(items []lexeme) ([]int, bool) {
	if len(items) == 0 || len(items) > expressionReadTokenBudget {
		return nil, false
	}
	p := diagnosticReadParser{items: items}
	if !p.expr(0, 0) || p.pos != len(items) {
		return nil, false
	}
	return p.ids, true
}

type diagnosticReadParser struct {
	items []lexeme
	pos   int
	ids   []int
	steps int
}

func (p *diagnosticReadParser) expr(min, depth int) bool {
	if depth > expressionReadDepthBudget || p.pos >= len(p.items) {
		return false
	}
	p.steps++
	if p.steps > expressionReadTokenBudget*2 {
		return false
	}
	item := p.items[p.pos]
	switch item.Token.Kind {
	case token.Plus, token.Minus:
		p.pos++
		if !p.expr(4, depth+1) {
			return false
		}
	case token.Number:
		p.pos++
	case token.SQLKeyword:
		if !isNameToken(item) {
			return false
		}
		p.ids = append(p.ids, p.pos)
		p.pos++
	case token.LParen:
		p.pos++
		if !p.expr(0, depth+1) || p.pos >= len(p.items) || p.items[p.pos].Token.Kind != token.RParen {
			return false
		}
		p.pos++
	default:
		return false
	}
	for p.pos < len(p.items) {
		op := p.items[p.pos].Token.Kind
		prec := diagnosticReadPrecedence(op)
		if prec < min {
			break
		}
		p.pos++
		if !p.expr(prec+1, depth+1) {
			return false
		}
	}
	return true
}

func diagnosticReadPrecedence(k token.Kind) int {
	switch k {
	case token.Eq, token.Neq, token.Lt, token.Gt, token.LtEq, token.GtEq:
		return 1
	case token.Plus, token.Minus:
		return 2
	case token.Mult, token.Div:
		return 3
	default:
		return -1
	}
}
