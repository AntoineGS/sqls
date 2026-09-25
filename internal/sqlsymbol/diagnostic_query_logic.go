package sqlsymbol

import (
	"sort"
	"strings"

	"github.com/sqls-server/sqls/token"
)

const (
	codeNullableAssignment = "interbase-nullable-assignment"
	codeNullableNotIn      = "interbase-nullable-not-in"
	codeOuterJoinFilter    = "interbase-outer-join-filter"
)

// queryLogicFindings reports bounded, opt-in NULL-flow advisories. Each
// analysis is deliberately local to a recognized query shape; unsupported
// source trees, predicates, and metadata remain unknown.
func (m *diagnosticModel) queryLogicFindings(options DiagnosticOptions) []Finding {
	if m == nil || m.analysis == nil || len(m.items) == 0 ||
		!options.anyOn(codeNullableAssignment, codeNullableNotIn, codeOuterJoinFilter) {
		return nil
	}
	var findings []Finding
	if !options.off(codeNullableAssignment) {
		findings = append(findings, m.nullableAssignmentFindings()...)
	}
	for qi, q := range m.queries {
		if q.kind != "SELECT" || q.malformedProjection || m.Unsupported(q.start) {
			continue
		}
		if !options.off(codeNullableNotIn) {
			findings = append(findings, m.nullableNotInFindings(qi)...)
		}
		if !options.off(codeOuterJoinFilter) {
			if f := m.outerJoinFilterFinding(qi); f != nil {
				findings = append(findings, *f)
			}
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Span.Start == findings[j].Span.Start {
			return findings[i].Code < findings[j].Code
		}
		return findings[i].Span.Start < findings[j].Span.Start
	})
	return findings
}

func (m *diagnosticModel) nullableAssignmentFindings() []Finding {
	var findings []Finding
	for _, edge := range m.analysis.assignmentEdges(m.items, m.catalog) {
		if len(edge.source) == 0 || m.staleCatalogDestination(edge) {
			continue
		}
		destination, ok := m.destinationNullability(edge)
		if !ok || destination != NotNullable {
			continue
		}
		fact := m.expressionFact(Span{Start: edge.source[0].Span.Start, End: edge.source[len(edge.source)-1].Span.End})
		if fact.Nullability != Nullable || m.sourceLocallyNotNull(edge.source, m.queryForSpan(edge.source[0].Span)) {
			continue
		}
		findings = append(findings, Finding{Span: Span{Start: edge.source[0].Span.Start, End: edge.source[len(edge.source)-1].Span.End}, Code: codeNullableAssignment,
			Message: "possibly NULL value is assigned to a NOT NULL destination; engine-side defaults or triggers may supply or change the value", Severity: 2})
	}
	return findings
}

func (m *diagnosticModel) destinationNullability(edge assignmentEdge) (Nullability, bool) {
	if edge.destination.relation.Key() == "" || m.semantic == nil {
		return NullUnknown, false
	}
	if m.Unsupported(edge.destination.relationAt) || m.DDLInvalidated(edge.destination.relationAt, edge.destination.relation) {
		return NullUnknown, false
	}
	fact, knowledge := m.semantic.RelationInfo(edge.destination.relation)
	if knowledge != Present || !fact.ColumnsKnown {
		return NullUnknown, false
	}
	columnName := edge.destination.label
	if dot := strings.LastIndexByte(columnName, '.'); dot >= 0 {
		columnName = columnName[dot+1:]
	}
	for _, column := range fact.Columns {
		if strings.EqualFold(column.Name, columnName) && column.Nullability == NotNullable {
			return NotNullable, true
		}
	}
	return NullUnknown, false
}

func (m *diagnosticModel) sourceLocallyNotNull(source []lexeme, qi int) bool {
	if qi < 0 || qi >= len(m.queries) {
		return false
	}
	if len(source) != 1 && !(len(source) == 3 && isNameToken(source[0]) && source[1].Token.Kind == token.Period && isNameToken(source[2])) {
		return false
	}
	q := m.queries[qi]
	depths, _ := sqlDepths(m.items)
	for i := q.start; i < q.end; i++ {
		if depths[i] != q.baseDepth || !isWord(m.items[i], "WHERE") {
			continue
		}
		end := clauseEnd(m.items, i+1)
		if end > q.end {
			end = q.end
		}
		for j := i + 1; j < end; j++ {
			if isWord(m.items[j], "OR") {
				return false
			}
		}
		for j := i + 1; j < end; j++ {
			predicateWidth := len(source)
			if j+predicateWidth+2 < end && m.sameBoundColumn(source, m.items, j) && isWord(m.items[j+predicateWidth], "IS") && isWord(m.items[j+predicateWidth+1], "NOT") && isWord(m.items[j+predicateWidth+2], "NULL") {
				return true
			}
		}
	}
	return false
}

func (m *diagnosticModel) sameBoundColumn(source, items []lexeme, index int) bool {
	// Compare the source and predicate's complete identifier shape, not
	// merely a column spelling.
	if index < 0 || index >= len(items) {
		return false
	}
	predicate := items[index:]
	if len(source) == 1 {
		return isNameToken(predicate[0]) && strings.EqualFold(m.analysis.Text[predicate[0].Span.Start:predicate[0].Span.End], m.analysis.Text[source[0].Span.Start:source[0].Span.End])
	}
	return len(predicate) >= 3 && isNameToken(predicate[0]) && predicate[1].Token.Kind == token.Period && isNameToken(predicate[2]) &&
		strings.EqualFold(m.analysis.Text[predicate[0].Span.Start:predicate[0].Span.End], m.analysis.Text[source[0].Span.Start:source[0].Span.End]) &&
		strings.EqualFold(m.analysis.Text[predicate[2].Span.Start:predicate[2].Span.End], m.analysis.Text[source[2].Span.Start:source[2].Span.End])
}

func (m *diagnosticModel) nullableNotInFindings(qi int) []Finding {
	q := m.queries[qi]
	if q.targetStart < 0 || q.targetEnd <= q.targetStart {
		return nil
	}
	var out []Finding
	depths, _ := sqlDepths(m.items)
	for i := q.start; i+3 < q.end; i++ {
		if depths[i] != q.baseDepth || !isWord(m.items[i], "NOT") || !isWord(m.items[i+1], "IN") || m.items[i+2].Token.Kind != token.LParen {
			continue
		}
		close, ok := matchingExpressionParen(m.items, i+2)
		if !ok || close >= q.end || close <= i+3 || !isWord(m.items[i+3], "SELECT") {
			continue
		}
		child, exists := m.queryByStart[i+3]
		if !exists || m.queries[child].parent != qi || m.queries[child].malformedProjection || len(m.queries[child].relations) != 1 {
			continue
		}
		if m.queries[child].relations[0].Name.Key() == "" || m.hasOuterCorrelation(qi, child) {
			continue
		}
		projection := m.items[m.queries[child].targetStart:m.queries[child].targetEnd]
		parts, ok := splitTopLevel(projection, token.Comma)
		if !ok || len(parts) != 1 {
			continue
		}
		span := Span{Start: parts[0][0].Span.Start, End: parts[0][len(parts[0])-1].Span.End}
		fact := m.expressionFact(span)
		if fact.Nullability != Nullable || m.sourceLocallyNotNull(parts[0], child) {
			continue
		}
		out = append(out, Finding{Span: span, Code: codeNullableNotIn, Message: "NOT IN may evaluate to UNKNOWN when the subquery projection contains NULL", Severity: 2})
	}
	return out
}

func (m *diagnosticModel) queryForSpan(span Span) int {
	for i, item := range m.items {
		if item.Span == span && i < len(m.queryAt) {
			return m.queryAt[i]
		}
	}
	return -1
}

func (m *diagnosticModel) hasOuterCorrelation(parent, child int) bool {
	q := m.queries[child]
	for _, relation := range m.queries[parent].relations {
		keys := []string{relation.Name.Key()}
		if relation.Alias != nil {
			keys = append(keys, relation.Alias.Key())
		}
		for i := q.start; i+1 < q.end; i++ {
			if m.items[i+1].Token.Kind != token.Period {
				continue
			}
			name := strings.ToUpper(m.analysis.Text[m.items[i].Span.Start:m.items[i].Span.End])
			for _, key := range keys {
				if name == key && key != "" {
					return true
				}
			}
		}
	}
	return false
}

// outerJoinFilterFinding only handles one plain two-source LEFT JOIN and one
// direct, null-rejecting WHERE comparison. OR and function/CASE expressions
// are intentionally unknown rather than approximated.
func (m *diagnosticModel) outerJoinFilterFinding(qi int) *Finding {
	q := m.queries[qi]
	details := m.RelationCandidates(qi)
	if len(details) != 2 || len(q.relations) != 2 {
		return nil
	}
	left, right := details[0], details[1]
	depths, _ := sqlDepths(m.items)
	for i := q.start; i < q.end; i++ {
		if depths[i] == q.baseDepth && isWord(m.items[i], "JOIN") {
			if i == 0 || !isWord(m.items[i-1], "LEFT") && !(i >= 2 && isWord(m.items[i-1], "OUTER") && isWord(m.items[i-2], "LEFT")) {
				return nil
			}
		}
	}
	joinCount := 0
	for i := q.start; i < q.end; i++ {
		if depths[i] == q.baseDepth && isWord(m.items[i], "JOIN") {
			joinCount++
		}
	}
	if joinCount != 1 || left.ref.Name.Key() == "" || right.ref.Name.Key() == "" || left.ref.Alias != nil && right.ref.Alias != nil && left.ref.Alias.Key() == right.ref.Alias.Key() {
		return nil
	}
	where := -1
	for i := q.start; i < q.end; i++ {
		if depths[i] == q.baseDepth && isWord(m.items[i], "WHERE") && i > q.start {
			where = i
			break
		}
	}
	if where < 0 {
		return nil
	}
	end := clauseEnd(m.items, where+1)
	body := m.items[where+1 : end]
	for _, item := range body {
		if isWord(item, "OR") || isWord(item, "COALESCE") || isWord(item, "CASE") {
			return nil
		}
	}
	// Only a direct qualified right-side column followed by an ordinary
	// comparison is recognized. IS NULL is preserving; IS NOT NULL rejects.
	for i := 0; i+2 < len(body); i++ {
		if !isNameToken(body[i]) || body[i+1].Token.Kind != token.Period || !isNameToken(body[i+2]) {
			continue
		}
		qualifier := strings.ToUpper(m.analysis.Text[body[i].Span.Start:body[i].Span.End])
		alias := right.ref.Name.Key()
		if right.ref.Alias != nil {
			alias = right.ref.Alias.Key()
		}
		if qualifier != alias {
			continue
		}
		j := i + 3
		if j+1 < len(body) && isWord(body[j], "IS") {
			if isWord(body[j+1], "NOT") && j+2 < len(body) && isWord(body[j+2], "NULL") {
				return queryFinding(body[i].Span.Start, body[j+2].Span.End, codeOuterJoinFilter, "WHERE filters out NULL-extended rows from the LEFT JOIN; this may make it behave like an inner join")
			}
			continue
		}
		if j < len(body) && (body[j].Token.Kind == token.Gt || body[j].Token.Kind == token.Lt || body[j].Token.Kind == token.Eq || body[j].Token.Kind == token.Neq || body[j].Token.Kind == token.GtEq || body[j].Token.Kind == token.LtEq) && j+1 < len(body) && !isWord(body[j+1], "NULL") {
			return queryFinding(body[i].Span.Start, body[j+1].Span.End, codeOuterJoinFilter, "WHERE predicate rejects NULL-extended rows from the LEFT JOIN; this may make it behave like an inner join")
		}
	}
	return nil
}

func queryFinding(start, end int, code, message string) *Finding {
	return &Finding{Span: Span{Start: start, End: end}, Code: code, Message: message, Severity: 2}
}

func clauseEnd(items []lexeme, start int) int {
	depths, _ := sqlDepths(items)
	base := 0
	if start < len(depths) {
		base = depths[start]
	}
	for i := start; i < len(items); i++ {
		if depths[i] != base {
			continue
		}
		if items[i].Token.Kind == token.Semicolon || isWord(items[i], "ORDER") || isWord(items[i], "GROUP") || isWord(items[i], "HAVING") || isWord(items[i], "ROWS") || isWord(items[i], "UNION") {
			return i
		}
	}
	return len(items)
}
