package sqlsymbol

import (
	"fmt"
	"sort"

	"github.com/sqls-server/sqls/token"
)

// codeTargetCount marks a provable mismatch between a statement's own
// target list (INSERT column list, SELECT ... INTO targets, a UNION arm's
// projection, or an EXECUTE PROCEDURE RETURNING_VALUES list) and the number
// of values/columns/outputs it is matched against.
const codeTargetCount = "interbase-target-count"

// codeProcedureArity marks a provable mismatch between the number of
// arguments supplied to a procedure call (EXECUTE PROCEDURE, or a
// selectable procedure used in a FROM clause) and its known declared input
// count.
const codeProcedureArity = "interbase-procedure-arity"

// shapeFindings reports target-count and procedure-arity mismatches. Every
// check here withholds rather than guesses: a malformed statement
// (Statements()' own Malformed flag) never contributes a finding, and an
// unprovable count (an ambiguous star projection, an uncatalogued
// procedure, metadata that has not finished loading) is silently skipped
// rather than compared.
func (m *diagnosticModel) shapeFindings() []Finding {
	if m == nil || m.analysis == nil {
		return nil
	}
	depths, matching := sqlDepths(m.items)
	var findings []Finding
	findings = append(findings, m.insertTargetCountFindings(depths, matching)...)
	findings = append(findings, m.selectIntoTargetCountFindings()...)
	findings = append(findings, m.unionTargetCountFindings()...)
	findings = append(findings, m.executeProcedureFindings()...)
	findings = append(findings, m.fromCallableArityFindings(depths, matching)...)
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Span.Start < findings[j].Span.Start })
	return findings
}

// queryStatementMalformed reports whether q belongs to a statement
// Statements() marked Malformed (recovery had to skip over it), the hard
// rule every check in this file obeys: a malformed statement's internal
// shape was never modeled, so nothing about it can be safely compared.
func (m *diagnosticModel) queryStatementMalformed(q modelQuery) bool {
	si := q.statementIndex
	return si < 0 || si >= len(m.statements) || m.statements[si].Malformed
}

// --- INSERT: column list vs VALUES/SELECT count ---

// insertTargetCountFindings scans every top-level INSERT statement
// directly from the token stream (INSERT is never itself a modelQuery
// worth walking for this purpose: an INSERT ... SELECT's SELECT arm is its
// own sibling modelQuery, per queryRelations' own doc comment, and
// modelQuery.end for the INSERT stops exactly at that sibling's start, one
// token short of the shared statement's own end -- so parsing bounds here
// use the owning statement's own End throughout instead).
func (m *diagnosticModel) insertTargetCountFindings(depths []int, matching map[int]int) []Finding {
	a := m.analysis
	items := m.items
	var findings []Finding
	for i, item := range items {
		if !isWord(item, "INSERT") || !a.inSQLContext(i) || depths[i] != 0 {
			continue
		}
		si := statementIndexAt(m.statements, i)
		if si < 0 || m.statements[si].Malformed || m.Unsupported(i) {
			continue
		}
		findings = append(findings, m.insertTargetCountFindingAt(items, i, m.statements[si].End, depths, matching)...)
	}
	return findings
}

func (m *diagnosticModel) insertTargetCountFindingAt(items []lexeme, start, limit int, depths []int, matching map[int]int) []Finding {
	a := m.analysis
	if start+2 >= limit || !isWord(items[start+1], "INTO") {
		return nil
	}
	targetIndex := nextName(items, start+2, limit)
	if targetIndex < 0 {
		return nil
	}
	target, next, ok := relationAt(a.Text, items, targetIndex, limit, depths, matching, false)
	if !ok || target.Name.Key() == "" {
		return nil
	}
	var columnCount, afterColumns int
	if next < limit && items[next].Token.Kind == token.LParen {
		columnItems, after, ok := enclosedList(items, next, limit)
		if !ok {
			return nil
		}
		columnParts, ok := splitTopLevel(columnItems, token.Comma)
		if !ok {
			return nil
		}
		columnCount, afterColumns = len(columnParts), after
	} else {
		// No explicit column list: only usable when the target's full,
		// known column order proves the count (RelationOutput), per the
		// task brief -- otherwise withhold rather than guess an order.
		cols, ok := m.RelationOutput(next, target)
		if !ok {
			return nil
		}
		columnCount, afterColumns = len(cols), next
	}
	if afterColumns >= limit {
		return nil
	}
	switch {
	case isWord(items[afterColumns], "VALUES"):
		return m.insertValuesTargetCountFindings(items, afterColumns+1, limit, columnCount, target.Name)
	case isWord(items[afterColumns], "SELECT"):
		return m.insertSelectTargetCountFinding(items, afterColumns, columnCount, target.Name)
	default:
		return nil
	}
}

func (m *diagnosticModel) insertValuesTargetCountFindings(items []lexeme, start, limit, columnCount int, targetName Name) []Finding {
	cursor := start
	var findings []Finding
	for cursor < limit {
		if items[cursor].Token.Kind != token.LParen {
			return nil
		}
		values, next, ok := enclosedList(items, cursor, limit)
		if !ok {
			return nil
		}
		parts, ok := splitTopLevel(values, token.Comma)
		if !ok {
			return nil
		}
		if len(parts) != columnCount {
			findings = append(findings, Finding{
				Span:     Span{Start: items[cursor].Span.Start, End: items[next-1].Span.End},
				Code:     codeTargetCount,
				Message:  fmt.Sprintf("%s has %d column(s) declared but %d value(s) supplied", targetName.Text, columnCount, len(parts)),
				Severity: 1,
			})
		}
		cursor = next
		if cursor == limit {
			return findings
		}
		if items[cursor].Token.Kind != token.Comma || cursor+1 >= limit {
			return nil
		}
		cursor++
	}
	return nil
}

// insertSelectTargetCountFinding compares the INSERT's own column count
// against the SELECT arm's own provable output shape (TargetOutput,
// Task 2/4's existing projection-counting machinery, including its star-
// expansion and malformed-projection handling) rather than re-parsing the
// SELECT's projection list here.
func (m *diagnosticModel) insertSelectTargetCountFinding(items []lexeme, selectIndex, columnCount int, targetName Name) []Finding {
	if selectIndex+1 >= len(items) {
		return nil
	}
	out, ok := m.TargetOutput(selectIndex + 1)
	if !ok || !out.CountKnown || len(out.Columns) == columnCount {
		return nil
	}
	span := Span{Start: items[selectIndex].Span.Start, End: items[selectIndex].Span.End}
	if qi := m.queryAt[selectIndex+1]; qi >= 0 {
		q := m.queries[qi]
		if q.targetStart < q.targetEnd {
			span = Span{Start: items[q.targetStart].Span.Start, End: items[q.targetEnd-1].Span.End}
		}
	}
	return []Finding{{
		Span:     span,
		Code:     codeTargetCount,
		Message:  fmt.Sprintf("%s has %d column(s) declared but the SELECT projects %d", targetName.Text, columnCount, len(out.Columns)),
		Severity: 1,
	}}
}

// --- SELECT ... INTO: projection count vs target count ---

func (m *diagnosticModel) selectIntoTargetCountFindings() []Finding {
	var findings []Finding
	for _, q := range m.queries {
		if q.kind != "SELECT" || m.queryStatementMalformed(q) || q.malformedProjection || m.Unsupported(q.start) {
			continue
		}
		if !q.output.CountKnown {
			continue
		}
		body := m.items[q.start+1 : q.end]
		intoRelative := topLevelWordIndex(body, "INTO")
		if intoRelative < 0 {
			continue
		}
		into := q.start + 1 + intoRelative
		targetEnd := q.end
		if fromRelative := topLevelWordIndex(m.items[into+1:q.end], "FROM"); fromRelative >= 0 {
			targetEnd = into + 1 + fromRelative
		}
		if into+1 >= targetEnd {
			continue
		}
		targets, ok := splitTopLevel(m.items[into+1:targetEnd], token.Comma)
		if !ok || len(targets) == len(q.output.Columns) {
			continue
		}
		findings = append(findings, Finding{
			Span:     Span{Start: m.items[into+1].Span.Start, End: m.items[targetEnd-1].Span.End},
			Code:     codeTargetCount,
			Message:  fmt.Sprintf("SELECT projects %d column(s) but INTO supplies %d target(s)", len(q.output.Columns), len(targets)),
			Severity: 1,
		})
	}
	return findings
}

// --- UNION: per-arm projection count ---

// unionTargetCountFindings walks each union group's arms directly (rather
// than relying on UnionOutput, whose own contract collapses "an arm's
// count is unknown" and "two arms' known counts disagree" into the same
// ok=false) so a mismatch is only ever reported when two specific arms
// both have a provably known count and those counts differ -- never merely
// because some other arm in the group is unprovable.
func (m *diagnosticModel) unionTargetCountFindings() []Finding {
	var findings []Finding
	for _, group := range m.unionGroups {
		baseCount := -1
		for _, qi := range group {
			q := m.queries[qi]
			if m.queryStatementMalformed(q) || q.malformedProjection || m.Unsupported(q.start) || !q.output.CountKnown {
				continue
			}
			count := len(q.output.Columns)
			if baseCount < 0 {
				baseCount = count
				continue
			}
			if count == baseCount {
				continue
			}
			span := Span{Start: m.items[q.start].Span.Start, End: m.items[q.start].Span.End}
			if q.targetStart < q.targetEnd {
				span = Span{Start: m.items[q.targetStart].Span.Start, End: m.items[q.targetEnd-1].Span.End}
			}
			findings = append(findings, Finding{
				Span:     span,
				Code:     codeTargetCount,
				Message:  fmt.Sprintf("UNION arm has %d column(s), expected %d to match an earlier arm", count, baseCount),
				Severity: 1,
			})
		}
	}
	return findings
}

// --- procedure calls: input arity and (where required) output count ---

// parseCallArgs parses an optional parenthesized, comma-separated argument
// list starting at open. No parentheses at all (open is not "(") reports a
// zero-argument call, matching InterBase's EXECUTE PROCEDURE/selectable-
// procedure syntax where the argument list itself is optional; an explicit
// empty "()" is likewise zero arguments. Any other shape that does not
// parse as a clean comma-separated list (an unbalanced paren, a genuinely
// empty element) reports ok=false so the caller withholds rather than
// miscounts.
func parseCallArgs(items []lexeme, open, limit int) (args [][]lexeme, next int, ok bool) {
	if open >= limit || items[open].Token.Kind != token.LParen {
		return nil, open, true
	}
	if open+1 < limit && items[open+1].Token.Kind == token.RParen {
		return nil, open + 2, true
	}
	body, after, ok := enclosedList(items, open, limit)
	if !ok {
		return nil, open, false
	}
	parts, ok := splitTopLevel(body, token.Comma)
	if !ok {
		return nil, open, false
	}
	return parts, after, true
}

// procedureInputArityFindings compares a call's supplied argument count
// against name's known declared shape. Per the task brief, InputsKnown and
// MinInputsKnown are used independently: a known full parameter list
// (InputsKnown) proves a maximum, so "too many" is always diagnosable from
// it alone; a known minimum (MinInputsKnown, which real catalog-sourced
// procedures with any parameters typically cannot prove, since defaulted
// trailing parameters are invisible to it) is required separately before
// "too few" can be diagnosed. This deliberately does not attempt to model
// optional/default parameters itself -- it only ever trusts what the two
// flags already independently claim to know.
func (m *diagnosticModel) procedureInputArityFindings(name Name, count, at int) []Finding {
	if m.semantic == nil || m.DDLInvalidated(at, name) {
		return nil
	}
	fact, knowledge := m.semantic.ProcedureInfo(name)
	if knowledge != Present {
		return nil
	}
	var findings []Finding
	if fact.InputsKnown && count > len(fact.Inputs) {
		findings = append(findings, Finding{
			Span:     m.items[at].Span,
			Code:     codeProcedureArity,
			Message:  fmt.Sprintf("%s accepts at most %d input(s), %d supplied", name.Text, len(fact.Inputs), count),
			Severity: 1,
		})
	}
	if fact.MinInputsKnown && count < fact.MinInputs {
		findings = append(findings, Finding{
			Span:     m.items[at].Span,
			Code:     codeProcedureArity,
			Message:  fmt.Sprintf("%s requires at least %d input(s), %d supplied", name.Text, fact.MinInputs, count),
			Severity: 1,
		})
	}
	return findings
}

// procedureOutputCountFinding compares a syntax that actually demands
// procedure outputs (RETURNING_VALUES) against name's known output count.
// Callers only invoke this when such a target list is actually present:
// standalone EXECUTE PROCEDURE with neither RETURNING_VALUES nor INTO never
// reaches this check at all.
func (m *diagnosticModel) procedureOutputCountFinding(name Name, at, count int, span Span) (Finding, bool) {
	if m.semantic == nil || m.DDLInvalidated(at, name) {
		return Finding{}, false
	}
	fact, knowledge := m.semantic.ProcedureInfo(name)
	if knowledge != Present || !fact.OutputsKnown || count == len(fact.Outputs) {
		return Finding{}, false
	}
	return Finding{
		Span:     span,
		Code:     codeTargetCount,
		Message:  fmt.Sprintf("%s returns %d output(s), %d target(s) supplied", name.Text, len(fact.Outputs), count),
		Severity: 1,
	}, true
}

// executeProcedureFindings scans every "EXECUTE PROCEDURE name(...)"
// call directly from the token stream: unlike SELECT/UPDATE/INSERT/DELETE,
// EXECUTE is never a modelQuery kind (isQueryStart in sql.go does not
// recognize it), so there is no existing per-query shape to walk here.
func (m *diagnosticModel) executeProcedureFindings() []Finding {
	a := m.analysis
	items := m.items
	var findings []Finding
	for i := 0; i < len(items); i++ {
		if !isWord(items[i], "EXECUTE") || i+1 >= len(items) || !isWord(items[i+1], "PROCEDURE") {
			continue
		}
		si := statementIndexAt(m.statements, i)
		if si < 0 || m.statements[si].Malformed || m.Unsupported(i) {
			continue
		}
		nameIdx := i + 2
		if nameIdx >= len(items) || !isNameToken(items[nameIdx]) {
			continue
		}
		name, ok := nameFromLexeme(a.Text, items[nameIdx])
		if !ok {
			continue
		}
		limit := m.statements[si].End
		args, next, ok := parseCallArgs(items, nameIdx+1, limit)
		if !ok {
			continue
		}
		findings = append(findings, m.procedureInputArityFindings(name, len(args), nameIdx)...)
		if next < limit && isWord(items[next], "RETURNING_VALUES") && next+1 < limit {
			if targets, ok := splitTopLevel(items[next+1:limit], token.Comma); ok {
				span := Span{Start: items[next+1].Span.Start, End: items[limit-1].Span.End}
				if f, has := m.procedureOutputCountFinding(name, nameIdx, len(targets), span); has {
					findings = append(findings, f)
				}
			}
		}
	}
	return findings
}

// fromCallableArityFindings checks a selectable procedure's own input
// arity when used in a FROM clause, using the same per-query relation
// candidate list (RelationCandidates) and procedureNameFor recovery
// unknownRelationFindings already relies on to identify which alias-only
// relation is actually a procedure call.
func (m *diagnosticModel) fromCallableArityFindings(depths []int, matching map[int]int) []Finding {
	a := m.analysis
	var findings []Finding
	for qi, q := range m.queries {
		if m.queryStatementMalformed(q) || q.malformedProjection || m.Unsupported(q.start) {
			continue
		}
		for _, detail := range m.RelationCandidates(qi) {
			if detail.ref.Name.Key() != "" {
				continue // a real table/view/CTE name: not the alias-only callable form
			}
			procName, ok := m.procedureNameFor[relationSourceKey{owner: qi, alias: aliasKeyOf(detail.ref.Alias)}]
			if !ok {
				continue // a derived table: nothing callable to check
			}
			if m.Unsupported(detail.start) {
				continue
			}
			_, next, ok := relationAt(a.Text, m.items, detail.start, q.end, depths, matching, false)
			if !ok {
				continue
			}
			args, _, ok := parseCallArgs(m.items, next, q.end)
			if !ok {
				continue
			}
			findings = append(findings, m.procedureInputArityFindings(procName, len(args), detail.start)...)
		}
	}
	return findings
}
