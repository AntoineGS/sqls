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

// codeLossyAssignment marks an assignment whose source is proven, by Task
// 8's assignmentCompatibility, to be accepted by the engine but to narrow,
// round, or truncate some (not necessarily this specific) value of the
// source's own declared range or scale -- compatibilityOutcome ==
// outcomePossibleLoss. Unlike every other code in diagnosticRegistry, this
// one defaults to off (diagnosticDefaultOff): it fires across the exact
// same eight assignment contexts as codeInvalidAssignment. A CAST(...)
// source is judged like any other expression here: expressionFact already
// gives it the CAST's own declared type/value (Task 9), so the outer
// assignment check naturally sees whatever the CAST actually produces --
// there is no special-casing for a CAST source, unlike codeInvalidAssignment's
// separate castLocalFinding, which judges the CAST's own inner conversion
// independently and is unaffected by this rule.
const codeLossyAssignment = "interbase-lossy-assignment"

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
	if m.staleCatalogDestination(edge) {
		return nil
	}
	span := Span{Start: edge.source[0].Span.Start, End: edge.source[len(edge.source)-1].Span.End}
	if f := m.castLocalFinding(edge.source, span); f != nil {
		return f
	}
	fact := m.expressionFact(span)
	return m.invalidAssignmentVerdict(fact, edge.destination.typeName, span, edge.destination.label)
}

// staleCatalogDestination reports whether edge's destination is sourced
// from a catalog column lookup (UPDATE/INSERT's relation-column targets,
// procedureInputEdges/triggerNewFieldEdges's own catalog-sourced
// destinations) whose owning relation's metadata is not safe to trust here
// -- either because it has since been invalidated by an earlier
// CREATE/ALTER/DROP in this same document, or the reference itself falls in
// a deliberately unsupported region. Shared by invalidAssignmentFinding and
// lossyAssignmentFinding, the two per-edge judges that both apply this same
// guard before evaluating anything. A procedure-local or SELECT ... INTO
// local write has no relation to invalidate at all (relation.Key() == "")
// and is never stale.
func (m *diagnosticModel) staleCatalogDestination(edge assignmentEdge) bool {
	if edge.destination.relation.Key() == "" {
		return false
	}
	return m.Unsupported(edge.destination.relationAt) || m.DDLInvalidated(edge.destination.relationAt, edge.destination.relation)
}

// castLocalFinding recognizes when edge's whole source span is, as its
// outermost syntactic form, an explicit CAST(<inner> AS <type>) -- not a
// CAST nested arbitrarily deep inside a larger expression, which is left
// unaddressed (see this function's own recognition helper,
// wholeCastExpression) -- and judges the CAST's own conversion: the inner
// expression's natural value against the CAST's own declared type,
// independent of the edge's real destination. Per the brief, when the
// CAST's own conversion is proven outcomeDefinitelyInvalid, that is the one
// finding for this edge, reported at the CAST's own span (span, which here
// is identical to the whole source span); the caller must skip the outer
// destination check entirely in that case, since the CAST would fail
// before the assignment ever ran. Returning nil covers every other case --
// not a whole CAST, or the CAST's own conversion is safe/possible-loss/
// unknown -- and the caller falls back to the ordinary outer check
// unaffected.
func (m *diagnosticModel) castLocalFinding(source []lexeme, span Span) *Finding {
	inner, typeSpan, ok := wholeCastExpression(source)
	if !ok {
		return nil
	}
	text := m.analysis.Text
	castType, ok := parseDiagnosticType(text[typeSpan.Start:typeSpan.End], m.analysis.Variant, m.catalog)
	if !ok {
		return nil
	}
	innerSpan := Span{Start: inner[0].Span.Start, End: inner[len(inner)-1].Span.End}
	innerFact := m.expressionFact(innerSpan)
	verdict := assignmentCompatibility(innerFact, castType, m.analysis.Variant)
	if verdict.Outcome != outcomeDefinitelyInvalid {
		return nil
	}
	return &Finding{
		Span:     span,
		Code:     codeInvalidAssignment,
		Message:  fmt.Sprintf("invalid CAST: %s", verdict.Reason),
		Severity: 1,
	}
}

// wholeCastExpression reports whether items, trimmed of any wrapping
// parentheses, is shaped exactly like CAST(<inner> AS <type>) spanning its
// entire length -- mirroring callFact's own CAST shape recognition
// (diagnostic_expressions.go, read-only for this task) so a source
// expression's outermost form can be identified the same way, without
// evaluating it as an expressionFact first. ok is false for anything else,
// including a CAST that is only part of a larger expression (for example
// CAST(...) + 1): items in that case is never shaped like CAST(...AS...)
// end to end, since the trailing "+ 1" falls outside the matched
// parentheses.
func wholeCastExpression(items []lexeme) (inner []lexeme, typeSpan Span, ok bool) {
	items = trimExpressionParens(items)
	if len(items) < 3 || !isWord(items[0], "CAST") || items[1].Token.Kind != token.LParen {
		return nil, Span{}, false
	}
	close, matched := matchingExpressionParen(items, 1)
	if !matched || close != len(items)-1 {
		return nil, Span{}, false
	}
	arguments := items[2:close]
	as := topLevelWordIndex(arguments, "AS")
	if as <= 0 || as >= len(arguments)-1 || topLevelWordIndex(arguments[as+1:], "AS") >= 0 {
		return nil, Span{}, false
	}
	typeSpan = Span{Start: arguments[as+1].Span.Start, End: arguments[len(arguments)-1].Span.End}
	return arguments[:as], typeSpan, true
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

// lossyAssignmentVerdict is codeLossyAssignment's own counterpart to
// invalidAssignmentVerdict, sharing the same destination-resolution and
// message-building shape but checking compatibilityOutcome ==
// outcomePossibleLoss instead.
func (m *diagnosticModel) lossyAssignmentVerdict(source expressionFact, destinationType string, span Span, destinationLabel string) *Finding {
	destination, ok := parseDiagnosticType(destinationType, m.analysis.Variant, m.catalog)
	if !ok {
		return nil
	}
	verdict := assignmentCompatibility(source, destination, m.analysis.Variant)
	if verdict.Outcome != outcomePossibleLoss {
		return nil
	}
	return &Finding{
		Span:     span,
		Code:     codeLossyAssignment,
		Message:  fmt.Sprintf("%s: %s", destinationLabel, verdict.Reason),
		Severity: 2,
	}
}

// lossyAssignmentFinding is codeLossyAssignment's own counterpart to
// invalidAssignmentFinding, sharing the same shape/staleCatalogDestination
// guard. Unlike invalidAssignmentFinding, there is no CAST-local special
// case here: a CAST(...) source is judged exactly like any other source
// expression, from expressionFact's own CAST-produced type/value (Task 9)
// against the edge's real destination. A CAST does not, by itself, justify
// narrowing into a destination unrelated to the CAST's own declared type --
// for example CAST(A AS INTEGER) assigned into a SMALLINT destination is
// still exactly as lossy as a bare A would be.
func (m *diagnosticModel) lossyAssignmentFinding(edge assignmentEdge) *Finding {
	if len(edge.source) == 0 || !balancedExpression(edge.source) {
		return nil
	}
	if m.staleCatalogDestination(edge) {
		return nil
	}
	span := Span{Start: edge.source[0].Span.Start, End: edge.source[len(edge.source)-1].Span.End}
	fact := m.expressionFact(span)
	return m.lossyAssignmentVerdict(fact, edge.destination.typeName, span, edge.destination.label)
}

// lossyAssignmentFindings is codeLossyAssignment's own counterpart to
// assignmentFindings, reusing the exact same edge sources across the same
// eight assignment contexts, but judging each edge with
// lossyAssignmentFinding/procedureOutputLossyFindings instead.
func (m *diagnosticModel) lossyAssignmentFindings() []Finding {
	if m == nil || m.analysis == nil || len(m.items) == 0 {
		return nil
	}
	a := m.analysis
	items := m.items
	_, matching := sqlDepths(items)

	var findings []Finding
	for _, edge := range a.assignmentEdges(items, m.catalog) {
		if f := m.lossyAssignmentFinding(edge); f != nil {
			findings = append(findings, *f)
		}
	}
	for _, edge := range m.procedureInputEdges(matching) {
		if f := m.lossyAssignmentFinding(edge); f != nil {
			findings = append(findings, *f)
		}
	}
	findings = append(findings, m.procedureOutputLossyFindings(matching)...)
	for _, trg := range discoverTriggers(a.Text, items) {
		for _, edge := range m.triggerNewFieldEdges(trg, items) {
			if f := m.lossyAssignmentFinding(edge); f != nil {
				findings = append(findings, *f)
			}
		}
	}

	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Span.Start < findings[j].Span.Start })
	return findings
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

// procedureOutputPair is one EXECUTE PROCEDURE ... RETURNING_VALUES target's
// own pairing against its procedure's declared OUTPUT column, shared by
// procedureOutputFindings (codeInvalidAssignment) and
// procedureOutputLossyFindings (codeLossyAssignment) so the RETURNING_VALUES
// discovery/pairing walk itself is written once, not duplicated once per
// rule -- each rule still calls procedureOutputPairs independently, once per
// DiagnosticsWithOptions call that has it turned on.
type procedureOutputPair struct {
	source           expressionFact
	destinationType  string
	span             Span
	destinationLabel string
}

// procedureOutputPairs walks every EXECUTE PROCEDURE call's own
// RETURNING_VALUES targets, pairing each against its procedure's declared
// OUTPUT column. Unlike every other context, the direction here is
// reversed: the procedure's declared OUTPUT column is the source (a
// type-only fact -- a called procedure's output has no literal value or
// expression text of its own in this document to build an expressionFact
// span from) and the RETURNING_VALUES local variable is the destination.
// Pairing only happens when the target count already matches the
// procedure's known output count, mirroring procedureInputEdges; ruling I4
// (diagnostic_shape.go's executeReturningValuesFindings) withholds a single
// parenthesized target group the same way here.
func (m *diagnosticModel) procedureOutputPairs(matching map[int]int) []procedureOutputPair {
	if m.semantic == nil {
		return nil
	}
	a := m.analysis
	items := m.items
	var pairs []procedureOutputPair
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
			pairs = append(pairs, procedureOutputPair{
				source:           expressionFact{Type: sourceType},
				destinationType:  resolution.Symbol.Type,
				span:             resolution.Span,
				destinationLabel: resolution.Symbol.Name.Text,
			})
		}
	}
	return pairs
}

// procedureOutputFindings judges every procedureOutputPairs pairing for
// codeInvalidAssignment (outcomeDefinitelyInvalid).
func (m *diagnosticModel) procedureOutputFindings(matching map[int]int) []Finding {
	var findings []Finding
	for _, pair := range m.procedureOutputPairs(matching) {
		if f := m.invalidAssignmentVerdict(pair.source, pair.destinationType, pair.span, pair.destinationLabel); f != nil {
			findings = append(findings, *f)
		}
	}
	return findings
}

// procedureOutputLossyFindings judges every procedureOutputPairs pairing for
// codeLossyAssignment (outcomePossibleLoss). Unlike codeInvalidAssignment's
// own procedureOutputFindings, this is a live, exercisable case rather than
// a structurally dead path: outcomePossibleLoss's storage-range/scale branch
// (assignmentCompatibility, diagnostic_types.go lines ~593-622) reads
// source.Type, not source.Value, so it can fire for this type-only fact --
// for example a NUMERIC(9,2) procedure output paired against a narrower
// SMALLINT RETURNING_VALUES local.
func (m *diagnosticModel) procedureOutputLossyFindings(matching map[int]int) []Finding {
	var findings []Finding
	for _, pair := range m.procedureOutputPairs(matching) {
		if f := m.lossyAssignmentVerdict(pair.source, pair.destinationType, pair.span, pair.destinationLabel); f != nil {
			findings = append(findings, *f)
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
