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
	predicate, ok := m.queryWherePredicate(qi)
	if !ok || !isIsNotNullPredicate(predicate) {
		return false
	}
	return m.sameSingleRelationColumn(source, predicate[:len(predicate)-3], qi)
}

// queryWherePredicate returns only a deliberately small predicate fragment:
// the query's complete top-level WHERE body, when it contains no nested
// expressions, boolean composition, or unrecognized tail. Query-local
// refinements and outer-join warnings both consume this proof boundary.
func (m *diagnosticModel) queryWherePredicate(qi int) ([]lexeme, bool) {
	if qi < 0 || qi >= len(m.queries) {
		return nil, false
	}
	q := m.queries[qi]
	depths := m.depths
	where := -1
	for i := q.start; i < q.end; i++ {
		if depths[i] == q.baseDepth && isWord(m.items[i], "WHERE") {
			if where >= 0 {
				return nil, false
			}
			where = i
		}
	}
	if where < 0 {
		return nil, false
	}
	end := q.end
	for i := where + 1; i < end; i++ {
		if depths[i] != q.baseDepth {
			return nil, false
		}
		if m.items[i].Token.Kind == token.Semicolon || isWord(m.items[i], "INTO") || isWord(m.items[i], "DO") ||
			isWord(m.items[i], "ORDER") || isWord(m.items[i], "GROUP") || isWord(m.items[i], "HAVING") ||
			isWord(m.items[i], "ROWS") || isWord(m.items[i], "UNION") || isWord(m.items[i], "PLAN") {
			end = i
			break
		}
	}
	if end <= where+1 {
		return nil, false
	}
	return m.items[where+1 : end], true
}

func isIsNotNullPredicate(predicate []lexeme) bool {
	_, width, ok := predicateColumn(predicate)
	return ok && len(predicate) == width+3 && isWord(predicate[width], "IS") &&
		isWord(predicate[width+1], "NOT") && isWord(predicate[width+2], "NULL")
}

func predicateColumn(predicate []lexeme) ([]lexeme, int, bool) {
	if len(predicate) >= 3 && isNameToken(predicate[0]) && predicate[1].Token.Kind == token.Period && isNameToken(predicate[2]) {
		return predicate[:3], 3, true
	}
	if len(predicate) >= 1 && isNameToken(predicate[0]) {
		return predicate[:1], 1, true
	}
	return nil, 0, false
}

func (m *diagnosticModel) sameSingleRelationColumn(source, predicate []lexeme, qi int) bool {
	q := m.queries[qi]
	if len(q.relations) != 1 || len(m.RelationCandidates(qi)) != 1 || q.relations[0].Name.Key() == "" {
		return false
	}
	sourceColumn, _, sourceOK := predicateColumn(source)
	predicateColumn, _, predicateOK := predicateColumn(predicate)
	if !sourceOK || !predicateOK {
		return false
	}
	columnName := func(column []lexeme) (Name, bool) { return nameFromLexeme(m.analysis.Text, column[len(column)-1]) }
	sourceName, ok := columnName(sourceColumn)
	if !ok {
		return false
	}
	predicateName, ok := columnName(predicateColumn)
	if !ok || sourceName.Key() != predicateName.Key() {
		return false
	}
	relation := m.RelationCandidates(qi)[0].ref
	effectiveQualifier := relation.Name
	if relation.Alias != nil {
		effectiveQualifier = *relation.Alias
	}
	qualifierMatches := func(column []lexeme) bool {
		if len(column) == 1 {
			return true
		}
		qualifier, ok := nameFromLexeme(m.analysis.Text, column[0])
		return ok && qualifier.Key() == effectiveQualifier.Key()
	}
	return qualifierMatches(sourceColumn) && qualifierMatches(predicateColumn)
}

func (m *diagnosticModel) nullableNotInFindings(qi int) []Finding {
	q := m.queries[qi]
	if q.targetStart < 0 || q.targetEnd <= q.targetStart {
		return nil
	}
	var out []Finding
	depths := m.depths
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
	depths := m.depths
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
	body, ok := m.queryWherePredicate(qi)
	if !ok {
		return nil
	}
	column, width, ok := predicateColumn(body)
	if !ok || len(column) != 3 {
		return nil
	}
	qualifier, ok := nameFromLexeme(m.analysis.Text, column[0])
	if !ok {
		return nil
	}
	alias := right.ref.Name
	if right.ref.Alias != nil {
		alias = *right.ref.Alias
	}
	if qualifier.Key() != alias.Key() {
		return nil
	}
	if isIsNotNullPredicate(body) {
		return queryFinding(body[0].Span.Start, body[len(body)-1].Span.End, codeOuterJoinFilter, "WHERE filters out NULL-extended rows from the LEFT JOIN; this may make it behave like an inner join")
	}
	if len(body) == width+2 && isNullRejectingComparison(body[width]) && isSimplePredicateValue(body[width+1]) {
		return queryFinding(body[0].Span.Start, body[len(body)-1].Span.End, codeOuterJoinFilter, "WHERE predicate rejects NULL-extended rows from the LEFT JOIN; this may make it behave like an inner join")
	}
	return nil
}

func isNullRejectingComparison(item lexeme) bool {
	switch item.Token.Kind {
	case token.Eq, token.Neq, token.Lt, token.Gt, token.LtEq, token.GtEq:
		return true
	}
	return false
}

func isSimplePredicateValue(item lexeme) bool {
	return isNameToken(item) || isWord(item, "NULL") || item.Token.Kind == token.Number || item.Token.Kind == token.SingleQuotedString || item.Token.Kind == token.NationalStringLiteral
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
