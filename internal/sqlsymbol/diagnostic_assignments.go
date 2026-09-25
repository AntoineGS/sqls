package sqlsymbol

import (
	"fmt"
	"sort"

	"github.com/sqls-server/sqls/token"
)

// codeInvalidAssignment marks an assignment whose source is proven, by
// Task 8's assignmentCompatibility, to never fit its destination -- for
// example a literal outside the destination's exact engine-enforced
// storage range. It fires only for compatibilityOutcome == outcomeDefinitelyInvalid;
// outcomePossibleLoss (a narrower-but-not-provably-violated range or scale)
// is a later task's concern, and outcomeUnknown/outcomeSafe never produce a
// finding at all.
const codeInvalidAssignment = "interbase-invalid-assignment"

// assignmentFindings reports every proven-invalid assignment across six
// contexts: the four assignmentEdges already shares with widthDiagnostics
// (procedure locals, UPDATE ... SET, INSERT ... VALUES/SELECT, SELECT ...
// INTO), plus two contexts unique to this rule: EXECUTE PROCEDURE input
// arguments/RETURNING_VALUES output targets, and trigger NEW.<col> = <expr>
// write-position assignments. Every edge is checked exactly once, from its
// own full source span, so an invalid CAST nested inside a larger
// assignment source is never reported twice (once for the CAST itself,
// once for the outer assignment): there is no separate scan that treats a
// CAST expression as its own assignment edge.
func (m *diagnosticModel) assignmentFindings() []Finding {
	if m == nil || m.analysis == nil || len(m.items) == 0 {
		return nil
	}
	a := m.analysis
	items := m.items
	_, matching := sqlDepths(items)

	var findings []Finding
	for _, edge := range a.assignmentEdges(items, m.catalog) {
		if f := m.invalidAssignmentFinding(edge); f != nil {
			findings = append(findings, *f)
		}
	}
	for _, edge := range m.procedureInputEdges(matching) {
		if f := m.invalidAssignmentFinding(edge); f != nil {
			findings = append(findings, *f)
		}
	}
	findings = append(findings, m.procedureOutputFindings(matching)...)
	for _, trg := range discoverTriggers(a.Text, items) {
		for _, edge := range m.triggerNewFieldEdges(trg, items) {
			if f := m.invalidAssignmentFinding(edge); f != nil {
				findings = append(findings, *f)
			}
		}
	}

	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Span.Start < findings[j].Span.Start })
	return findings
}

// invalidAssignmentFinding judges one expression-sourced edge (an edge
// whose source is real SQL text this model can evaluate an expressionFact
// for), reporting a finding only for compatibilityOutcome ==
// outcomeDefinitelyInvalid. A shape mismatch, ambiguous binding, or unknown
// destination type (parseDiagnosticType returning ok=false) skips the edge
// entirely -- no finding at all -- matching every other diagnostic rule's
// "withhold rather than guess" bias.
func (m *diagnosticModel) invalidAssignmentFinding(edge assignmentEdge) *Finding {
	if len(edge.source) == 0 || !balancedExpression(edge.source) {
		return nil
	}
	span := Span{Start: edge.source[0].Span.Start, End: edge.source[len(edge.source)-1].Span.End}
	fact := m.expressionFact(span)
	return m.invalidAssignmentVerdict(fact, edge.destination.typeName, span, edge.destination.label)
}

// invalidAssignmentVerdict is the shared destination-resolution and
// message-building core both invalidAssignmentFinding (a real source
// expression) and procedureOutputFindings (a procedure's declared OUTPUT
// type, which has no source expression of its own in this document) build
// their finding from. The message cites compatibility.Reason plus
// destinationLabel only -- never the source SQL text -- unlike
// widthFinding's string-truncation message.
func (m *diagnosticModel) invalidAssignmentVerdict(source expressionFact, destinationType string, span Span, destinationLabel string) *Finding {
	destination, ok := parseDiagnosticType(destinationType, m.analysis.Variant, m.catalog)
	if !ok {
		return nil
	}
	verdict := assignmentCompatibility(source, destination, m.analysis.Variant)
	if verdict.Outcome != outcomeDefinitelyInvalid {
		return nil
	}
	return &Finding{
		Span:     span,
		Code:     codeInvalidAssignment,
		Message:  fmt.Sprintf("%s: %s", destinationLabel, verdict.Reason),
		Severity: 1,
	}
}

// procedureInputEdges pairs an EXECUTE PROCEDURE call's own supplied
// argument expressions against name's known declared input types, mirroring
// diagnostic_shape.go's executeProcedureFindings scan (EXECUTE is never a
// modelQuery kind, so there is no existing per-query shape to walk here).
// Per the task brief, pairing only happens when the supplied argument count
// already matches the procedure's known input count -- an arity mismatch
// (already reported separately by codeProcedureArity) withholds every edge
// for that call rather than guessing which argument lines up with which
// parameter.
func (m *diagnosticModel) procedureInputEdges(matching map[int]int) []assignmentEdge {
	if m.semantic == nil {
		return nil
	}
	a := m.analysis
	items := m.items
	var edges []assignmentEdge
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
		if !ok || m.DDLInvalidated(nameIdx, name) {
			continue
		}
		fact, knowledge := m.semantic.ProcedureInfo(name)
		if knowledge != Present || !fact.InputsKnown {
			continue
		}
		limit := m.statements[si].End
		argsEnd := limit
		if relative := topLevelWordIndex(items[nameIdx+1:limit], "RETURNING_VALUES"); relative >= 0 {
			argsEnd = nameIdx + 1 + relative
		}
		args, ok := parseExecuteCallArgs(items, nameIdx+1, argsEnd, matching)
		if !ok || len(args) != len(fact.Inputs) {
			continue
		}
		for idx, arg := range args {
			if len(arg) == 0 || !balancedExpression(arg) {
				continue
			}
			edges = append(edges, assignmentEdge{
				source: arg,
				destination: widthDestination{
					span:     Span{Start: arg[0].Span.Start, End: arg[len(arg)-1].Span.End},
					typeName: fact.Inputs[idx].Type,
					label:    fmt.Sprintf("%s.%s", name.Text, fact.Inputs[idx].Name),
				},
			})
		}
	}
	return edges
}

// procedureOutputFindings judges an EXECUTE PROCEDURE call's own
// RETURNING_VALUES targets. Unlike every other context, the direction here
// is reversed: the procedure's declared OUTPUT column is the source (a
// type-only fact -- a called procedure's output has no literal value or
// expression text of its own in this document to build an expressionFact
// span from) and the RETURNING_VALUES local variable is the destination.
// Pairing only happens when the target count already matches the
// procedure's known output count, mirroring procedureInputEdges; ruling I4
// (diagnostic_shape.go's executeReturningValuesFindings) withholds a single
// parenthesized target group the same way here.
func (m *diagnosticModel) procedureOutputFindings(matching map[int]int) []Finding {
	if m.semantic == nil {
		return nil
	}
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
		returningRelative := topLevelWordIndex(items[nameIdx+1:limit], "RETURNING_VALUES")
		if returningRelative < 0 {
			continue
		}
		returningIdx := nameIdx + 1 + returningRelative
		if m.DDLInvalidated(nameIdx, name) {
			continue
		}
		fact, knowledge := m.semantic.ProcedureInfo(name)
		if knowledge != Present || !fact.OutputsKnown {
			continue
		}
		start := returningIdx + 1
		if start >= limit {
			continue
		}
		if items[start].Token.Kind == token.LParen {
			if close, ok := matching[start]; ok && close == limit-1 {
				continue // ruling I4: withhold for a single parenthesized target group
			}
		}
		targets, ok := splitTopLevel(items[start:limit], token.Comma)
		if !ok || len(targets) != len(fact.Outputs) {
			continue
		}
		for idx, target := range targets {
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
			sourceType, ok := parseDiagnosticType(fact.Outputs[idx].Type, a.Variant, m.catalog)
			if !ok {
				continue
			}
			sourceFact := expressionFact{Type: sourceType}
			if f := m.invalidAssignmentVerdict(sourceFact, resolution.Symbol.Type, resolution.Span, resolution.Symbol.Name.Text); f != nil {
				findings = append(findings, *f)
			}
		}
	}
	return findings
}

// triggerNewFieldEdges finds every NEW.<col> = <expr> write-position
// assignment in trg's own procedural (non-SQL) region, using
// isTriggerAssignmentBoundary (diagnostic_locals.go) to prove the "=" is a
// genuine statement-starting assignment rather than a comparison, the same
// boundary check unknownVariableFindings already relies on for a bare
// local's own write position. OLD is always read-only and never scanned:
// only NEW is a legal trigger-body assignment target.
func (m *diagnosticModel) triggerNewFieldEdges(trg triggerModel, items []lexeme) []assignmentEdge {
	ctx := newTriggerBodyContext(items, trg.bodyStart, trg.bodyEnd)
	var edges []assignmentEdge
	for idx := trg.bodyStart + 1; idx < trg.bodyEnd; idx++ {
		i := idx - trg.bodyStart
		if ctx.inSQL[i] {
			continue
		}
		if !isWord(items[idx], "NEW") {
			continue
		}
		if idx+3 >= trg.bodyEnd {
			continue
		}
		if items[idx+1].Token.Kind != token.Period || !isNameToken(items[idx+2]) || items[idx+3].Token.Kind != token.Eq {
			continue
		}
		if !isTriggerAssignmentBoundary(items[idx-1]) {
			continue
		}
		if m.Unsupported(idx) || m.DDLInvalidated(idx, trg.relation) {
			continue
		}
		colName, ok := nameFromLexeme(m.analysis.Text, items[idx+2])
		if !ok {
			continue
		}
		end, complete := completeStatementEnd(items, idx+4)
		if !complete || end == idx+4 || !balancedExpression(items[idx+4:end]) {
			continue
		}
		typeName, known := catalogColumnType(m.catalog, trg.relation, colName)
		if !known {
			continue
		}
		edges = append(edges, assignmentEdge{
			source: items[idx+4 : end],
			destination: widthDestination{
				span:     items[idx+2].Span,
				typeName: typeName,
				label:    "NEW." + colName.Text,
			},
		})
	}
	return edges
}
