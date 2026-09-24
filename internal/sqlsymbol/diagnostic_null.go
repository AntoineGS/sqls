package sqlsymbol

import "github.com/sqls-server/sqls/token"

const nullComparisonMessage = "comparison with NULL yields UNKNOWN; use IS NULL/IS NOT NULL if testing nullness"

// nullComparisonFindings warns about a direct `= NULL` / `<> NULL` / `!= NULL`
// comparison, which InterBase always evaluates to UNKNOWN rather than
// true/false. This is pure lexical pattern matching over the boolean
// condition of WHERE, JOIN...ON, HAVING, CASE WHEN, and procedural IF: no
// catalog or query-scope model is needed, since a NULL literal and a
// comparison operator are both recognized syntactically wherever they occur.
// `IS [NOT] NULL` never matches (it tokenizes as the IS keyword, not
// token.Eq/token.Neq); an assignment target's `=` (a procedural `v = expr;`
// statement or a SQL `UPDATE ... SET col = expr`) is never scanned at all,
// since it never appears inside one of these condition clauses.
func (a *Analysis) nullComparisonFindings() []Finding {
	if a == nil || len(a.lexemes) == 0 {
		return nil
	}
	items := significantLexemes(a.lexemes)
	if len(items) == 0 {
		return nil
	}
	depths, matching := statementSQLDepths(items)
	var findings []Finding
	for i, item := range items {
		switch {
		case isWord(item, "WHERE"):
			findings = append(findings, a.clauseNullFindings(items, depths, i,
				[]string{"GROUP", "HAVING", "ORDER", "ROWS", "PLAN", "UNION", "RETURNING", "RETURNING_VALUES", "INTO", "DO", "FOR"},
				true, true)...)
		case isWord(item, "HAVING"):
			findings = append(findings, a.clauseNullFindings(items, depths, i,
				[]string{"ORDER", "ROWS", "PLAN", "INTO", "DO", "FOR"}, true, true)...)
		case isWord(item, "ON"):
			findings = append(findings, a.clauseNullFindings(items, depths, i,
				[]string{"JOIN", "WHERE", "GROUP", "HAVING", "ORDER", "ROWS", "PLAN", "INTO", "DO", "FOR",
					"LEFT", "RIGHT", "INNER", "FULL", "OUTER"}, true, true)...)
		case isWord(item, "WHEN"):
			findings = append(findings, a.clauseNullFindings(items, depths, i, []string{"THEN"}, false, false)...)
		case isWord(item, "IF"):
			if i+1 < len(items) && items[i+1].Token.Kind == token.LParen {
				if close, ok := matching[i+1]; ok {
					findings = append(findings, a.scanNullClauseBody(items[i+2:close])...)
				}
			}
		}
	}
	return findings
}

// clauseBody bounds a condition clause introduced by the keyword at
// keywordIndex, from just past that keyword to the first of: a boundary
// keyword at that same nesting depth, a top-level (same-depth) semicolon
// when semicolonOK, or the point where depth drops below the keyword's own
// depth (leaving an enclosing subquery/derived-table's parentheses) when
// depthDropOK -- the natural close of a WHERE/HAVING/ON clause nested inside
// EXISTS/IN/a derived table, which has no boundary keyword or semicolon of
// its own. Reaching the end of items without any of these is reported
// incomplete (ok=false): a clause that never proves its own extent is never
// scanned, matching this package's existing width/singleton diagnostics,
// which likewise withhold findings for a statement completeStatementEnd
// cannot bound.
func clauseBody(items []lexeme, depths []int, keywordIndex int, boundaryWords []string, semicolonOK, depthDropOK bool) (bodyStart, bodyEnd int, ok bool) {
	depth := depths[keywordIndex]
	bodyStart = keywordIndex + 1
	for i := bodyStart; i < len(items); i++ {
		// A RParen's own recorded depth is its interior (content) depth, one
		// more than the depth its matching LParen was opened at (see
		// statementSQLDepths); the RParen closing the scope enclosing this
		// whole clause therefore has depths[i] == depth here, not depth-1.
		if items[i].Token.Kind == token.RParen && depths[i] <= depth {
			if depthDropOK {
				return bodyStart, i, true
			}
			return 0, 0, false
		}
		if depths[i] != depth {
			continue
		}
		if items[i].Token.Kind == token.Semicolon {
			if semicolonOK {
				return bodyStart, i, true
			}
			return 0, 0, false
		}
		for _, word := range boundaryWords {
			if boundaryWordMatches(items, i, word) {
				return bodyStart, i, true
			}
		}
	}
	return 0, 0, false
}

// boundaryWordMatches reports whether items[i] is the clause-boundary
// keyword word. LEFT and RIGHT are also InterBase scalar string functions
// (LEFT(str, n), RIGHT(str, n)); as a join qualifier (LEFT/RIGHT [OUTER]
// JOIN) neither is ever immediately followed by "(", so that one-token
// lookahead disambiguates a chained join's own qualifier from a call to
// either function appearing within the clause body itself.
func boundaryWordMatches(items []lexeme, i int, word string) bool {
	if !isWord(items[i], word) {
		return false
	}
	switch word {
	case "LEFT", "RIGHT":
		return i+1 >= len(items) || items[i+1].Token.Kind != token.LParen
	default:
		return true
	}
}

func (a *Analysis) clauseNullFindings(items []lexeme, depths []int, keywordIndex int, boundaryWords []string, semicolonOK, depthDropOK bool) []Finding {
	bodyStart, bodyEnd, ok := clauseBody(items, depths, keywordIndex, boundaryWords, semicolonOK, depthDropOK)
	if !ok {
		return nil
	}
	return a.scanNullClauseBody(items[bodyStart:bodyEnd])
}

// scanNullClauseBody withholds every finding unless body is itself a
// complete expression (completeExpression's existing balanced-parens and
// dangling-operator/keyword checks, reused from assignments.go): a body
// like "V = NULL AND" (nothing follows AND) proves nothing about the
// comparison it contains, so it is skipped whole rather than partially
// trusted.
func (a *Analysis) scanNullClauseBody(body []lexeme) []Finding {
	if !completeExpression(a.Text, body) {
		return nil
	}
	return scanClauseForNullComparisons(body)
}

// scanClauseForNullComparisons recursively splits a proven-complete boolean
// expression by its own top-level AND, then OR, and inspects each leaf for a
// direct `=`/`<>`/`!=` comparison against a bare NULL literal (either
// operand, redundant wrapping parens stripped via trimExpressionParens).
// This mirrors singleton.go's predicate/value recursion (also built to find
// simple comparisons and NULL tests within a WHERE/ON clause), but does not
// resolve operands against any relation or catalog: only whether one side is
// the literal NULL matters here.
func scanClauseForNullComparisons(items []lexeme) []Finding {
	items = trimExpressionParens(items)
	if len(items) == 0 {
		return nil
	}
	if or := topLevelWordIndex(items, "OR"); or > 0 {
		return append(scanClauseForNullComparisons(items[:or]), scanClauseForNullComparisons(items[or+1:])...)
	}
	if and := topLevelWordIndex(items, "AND"); and > 0 {
		return append(scanClauseForNullComparisons(items[:and]), scanClauseForNullComparisons(items[and+1:])...)
	}
	for _, kind := range []token.Kind{token.Eq, token.Neq} {
		index := topLevelTokenIndex(items, kind)
		if index <= 0 || index >= len(items)-1 {
			continue
		}
		left, right := items[:index], items[index+1:]
		if !isBareNullLiteral(left) && !isBareNullLiteral(right) {
			continue
		}
		return []Finding{{
			Span:     Span{Start: items[0].Span.Start, End: items[len(items)-1].Span.End},
			Code:     "interbase-null-comparison",
			Message:  nullComparisonMessage,
			Severity: 2,
		}}
	}
	return nil
}

// isBareNullLiteral reports whether items, once any redundant wrapping
// parens are stripped, is exactly the literal NULL token -- never true for a
// larger expression NULL merely appears within (a function call's argument,
// an arithmetic or concatenation expression), since trimExpressionParens
// only strips parens that wrap the entire slice as one balanced group.
func isBareNullLiteral(items []lexeme) bool {
	items = trimExpressionParens(items)
	return len(items) == 1 && isWord(items[0], "NULL")
}
