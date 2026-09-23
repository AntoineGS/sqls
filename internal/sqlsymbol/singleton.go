package sqlsymbol

import (
	"strconv"

	"github.com/sqls-server/sqls/token"
)

type singletonRelation struct {
	ref     RelationRef
	columns []ColumnType
	keys    [][]string
}

// A scalar has relation -1; a column identifies a particular relation instance
// (not a table name, since self joins must keep their keys separate).
type singletonValue struct {
	relation int
	column   string
}

type singletonEquality struct {
	left, right singletonValue
	// -1 for WHERE/inner ON; a left-join ON can constrain only its new side.
	onlyRelation int
}

type singletonQuery struct {
	analysis   *Analysis
	relations  []singletonRelation
	equalities []singletonEquality
}

func (a *Analysis) singletonDiagnostics(c Catalog) []Finding {
	keys, ok := c.(UniqueKeyCatalog)
	if !ok || a == nil {
		return nil
	}
	items := significantLexemes(a.lexemes)
	depths, _ := statementSQLDepths(items)
	var findings []Finding
	for i := 0; i < len(items); i++ {
		if !isWord(items[i], "SELECT") || !a.inSQLContext(i) || depths[i] != 0 ||
			i >= len(a.procedureAt) || a.procedureAt[i] < 0 {
			continue
		}
		if i > 0 && isWord(items[i-1], "FOR") {
			continue
		}
		end, complete := completeStatementEnd(items, i+1)
		if !complete {
			continue
		}
		query := singletonQuery{analysis: a}
		if safe, supported := query.analyze(items[i:end], keys); supported && !safe {
			findings = append(findings, Finding{
				Span: items[i].Span, Code: "interbase-singleton-select", Severity: 2,
				Message: "Possible multiple rows in singleton SELECT: predicates do not constrain a complete primary key or unique index for every relation",
			})
		}
		// Consume the whole statement, including unsupported UNION arms and
		// nested queries, rather than diagnosing them out of context.
		i = end
	}
	return findings
}

func (q *singletonQuery) analyze(items []lexeme, c UniqueKeyCatalog) (safe, supported bool) {
	if !balancedExpression(items) {
		return false, false
	}
	into := topLevelWordIndex(items, "INTO")
	from := topLevelWordIndex(items, "FROM")
	if into < 2 || from < 2 || into == from {
		return false, false
	}
	projectionEnd, targetEnd := from, len(items)
	var tail []lexeme
	if into < from {
		projectionEnd, targetEnd = into, from
		tail = items[from+1:]
	} else {
		tail = items[from+1 : into]
	}
	projections, ok := splitTopLevel(items[1:projectionEnd], token.Comma)
	if !ok {
		return false, false
	}
	targets, ok := splitTopLevel(items[into+1:targetEnd], token.Comma)
	if !ok || len(targets) != len(projections) {
		return false, false
	}
	for _, target := range targets {
		if len(target) == 2 && target[0].Token.Kind == token.Colon {
			target = target[1:]
		}
		if len(target) != 1 || !isNameToken(target[0]) {
			return false, false
		}
		resolution := q.analysis.Resolve(target[0].Span.Start)
		if resolution.Role != Local || resolution.Symbol == nil {
			return false, false
		}
	}

	// Only this ordered subset of SELECT clauses is interpreted. Unsupported
	// syntax is unknown, never evidence of a multi-row result.
	clauses, ok := singletonClauses(tail)
	if !ok || !q.parseRelations(clauses["FROM"], c) {
		return false, false
	}
	if where, exists := clauses["WHERE"]; exists && !q.predicate(where, -1) {
		return false, false
	}
	if group, exists := clauses["GROUP"]; exists && !q.columnList(group, false) {
		return false, false
	}
	if order, exists := clauses["ORDER"]; exists && !q.columnList(order, true) {
		return false, false
	}
	limited := false
	if rows, exists := clauses["ROWS"]; exists {
		if len(rows) != 1 || rows[0].Token.Kind != token.Number {
			return false, false
		}
		n, err := strconv.Atoi(q.analysis.Text[rows[0].Span.Start:rows[0].Span.End])
		if err != nil || n < 1 {
			return false, false
		}
		limited = n == 1
	}
	distinct := len(projections[0]) > 0 && isWord(projections[0][0], "DISTINCT")
	if len(projections[0]) > 0 && (distinct || isWord(projections[0][0], "ALL")) {
		projections[0] = projections[0][1:]
	}
	aggregates, columnProjection := false, false
	for _, projection := range projections {
		aggregate, column, ok := q.projection(projection)
		if !ok {
			return false, false
		}
		aggregates = aggregates || aggregate
		columnProjection = columnProjection || column
	}
	_, grouped := clauses["GROUP"]
	if aggregates && columnProjection && !grouped {
		return false, false
	}
	return limited || (aggregates && !grouped) || (distinct && !aggregates && !columnProjection) || q.keysBound(), true
}

func singletonClauses(items []lexeme) (map[string][]lexeme, bool) {
	clauses := make(map[string][]lexeme)
	current, start, lastRank := "FROM", 0, 0
	depths, _ := sqlDepths(items)
	for i, item := range items {
		if depths[i] != 0 {
			continue
		}
		word, rank := itemWord(item), 0
		switch word {
		case "WHERE":
			rank = 1
		case "GROUP":
			rank = 2
		case "ORDER":
			rank = 3
		case "ROWS":
			rank = 4
		case "UNION", "HAVING", "PLAN", "INTO", "FROM", "RETURNING", "DO":
			return nil, false
		default:
			continue
		}
		if rank <= lastRank || i == start {
			return nil, false
		}
		clauses[current] = items[start:i]
		current, start, lastRank = word, i+1, rank
	}
	if start == len(items) {
		return nil, false
	}
	clauses[current] = items[start:]
	return clauses, true
}

func (q *singletonQuery) parseRelations(items []lexeme, c UniqueKeyCatalog) bool {
	depths, matching := sqlDepths(items)
	type joinPredicate struct {
		items   []lexeme
		only    int
		visible int
	}
	var predicates []joinPredicate
	leftSeen := false
	for cursor := 0; cursor < len(items); {
		left := false
		if len(q.relations) > 0 {
			if isWord(items[cursor], "LEFT") {
				left, leftSeen = true, true
				cursor++
				if cursor < len(items) && isWord(items[cursor], "OUTER") {
					cursor++
				}
			} else {
				// Mixed inner-after-outer joins need a richer null-rejection
				// model. Leave that shape unknown for now.
				if leftSeen {
					return false
				}
				if isWord(items[cursor], "INNER") {
					cursor++
				}
			}
			if cursor >= len(items) || !isWord(items[cursor], "JOIN") {
				return false
			}
			cursor++
		}
		if cursor >= len(items) || !isNameToken(items[cursor]) {
			return false
		}
		// InterBase has no table schema qualifier; don't discard components
		// and accidentally resolve unsupported qualified syntax as a table.
		if cursor+1 < len(items) && items[cursor+1].Token.Kind == token.Period {
			return false
		}
		ref, next, ok := relationAt(q.analysis.Text, items, cursor, len(items), depths, matching, true)
		if !ok || ref.Name.Key() == "" || isWord(items[next-1], "AS") {
			return false
		}
		keys, known := c.UniqueKeys(ref.Name)
		columns, columnsKnown := c.Columns(ref.Name)
		if !known || !columnsKnown {
			return false
		}
		for _, existing := range q.relations {
			name := ref.Name
			if ref.Alias != nil {
				name = *ref.Alias
			}
			if relationMatchesQualifier(existing.ref, name) {
				return false
			}
		}
		q.relations = append(q.relations, singletonRelation{ref: ref, columns: columns, keys: keys})
		cursor = next
		if len(q.relations) == 1 {
			continue
		}
		if cursor >= len(items) || !isWord(items[cursor], "ON") {
			return false
		}
		start := cursor + 1
		cursor = start
		for cursor < len(items) {
			if depths[cursor] == 0 && (isWord(items[cursor], "JOIN") || isWord(items[cursor], "INNER") ||
				isWord(items[cursor], "LEFT") || isWord(items[cursor], "RIGHT") || isWord(items[cursor], "FULL")) {
				break
			}
			cursor++
		}
		only := -1
		if left {
			only = len(q.relations) - 1
		}
		predicates = append(predicates, joinPredicate{items[start:cursor], only, len(q.relations)})
	}
	if len(q.relations) == 0 {
		return false
	}
	// ON clauses can reference only the relations introduced so far.
	relations := q.relations
	for _, predicate := range predicates {
		q.relations = relations[:predicate.visible]
		if !q.predicate(predicate.items, predicate.only) {
			q.relations = relations
			return false
		}
	}
	q.relations = relations
	return true
}

// predicate accepts conjunctions of simple comparisons and null tests. Only
// ordinary equality provides a binding; OR, functions, subqueries and casts
// are deliberately unknown (casts/collations can invalidate key uniqueness).
func (q *singletonQuery) predicate(items []lexeme, only int) bool {
	items = singletonUnwrap(items)
	if len(items) == 0 {
		return false
	}
	if topLevelWordIndex(items, "OR") >= 0 {
		return false
	}
	if and := topLevelWordIndex(items, "AND"); and >= 0 {
		return q.predicate(items[:and], only) && q.predicate(items[and+1:], only)
	}
	if is := topLevelWordIndex(items, "IS"); is >= 0 {
		if _, ok := q.value(items[:is]); !ok {
			return false
		}
		tail := items[is+1:]
		if len(tail) > 0 && isWord(tail[0], "NOT") {
			tail = tail[1:]
		}
		return len(tail) == 1 && isWord(tail[0], "NULL")
	}
	for _, kind := range []token.Kind{token.Eq, token.Neq, token.Lt, token.Gt, token.LtEq, token.GtEq} {
		index := topLevelTokenIndex(items, kind)
		if index < 0 {
			continue
		}
		left, lok := q.value(items[:index])
		right, rok := q.value(items[index+1:])
		if !lok || !rok {
			return false
		}
		if kind == token.Eq {
			q.equalities = append(q.equalities, singletonEquality{left, right, only})
		}
		return true
	}
	return false
}

func singletonUnwrap(items []lexeme) []lexeme {
	for len(items) > 0 && items[0].Token.Kind == token.LParen {
		inside, next, ok := enclosedList(items, 0, len(items))
		if !ok || next != len(items) {
			break
		}
		items = inside
	}
	return items
}

func (q *singletonQuery) value(items []lexeme) (singletonValue, bool) {
	items = singletonUnwrap(items)
	scalar := singletonValue{relation: -1}
	if len(items) == 0 {
		return scalar, false
	}
	if len(items) == 2 && (items[0].Token.Kind == token.Plus || items[0].Token.Kind == token.Minus) && items[1].Token.Kind == token.Number {
		return scalar, true
	}
	if len(items) == 1 {
		switch items[0].Token.Kind {
		case token.Number, token.SingleQuotedString, token.NationalStringLiteral:
			return scalar, true
		}
		if isWord(items[0], "NULL") {
			return scalar, true
		}
	}
	if len(items) == 2 && items[0].Token.Kind == token.Colon {
		resolution := q.analysis.Resolve(items[1].Span.Start)
		return scalar, isNameToken(items[1]) && resolution.Role == Local && resolution.Symbol != nil
	}
	var qualifier *Name
	var name Name
	if len(items) == 3 && items[1].Token.Kind == token.Period && isNameToken(items[0]) {
		qual, _ := nameFromLexeme(q.analysis.Text, items[0])
		qualifier, items = &qual, items[2:]
	}
	if len(items) != 1 || !isNameToken(items[0]) {
		return scalar, false
	}
	name, _ = nameFromLexeme(q.analysis.Text, items[0])
	if qualifier == nil {
		resolution := q.analysis.Resolve(items[0].Span.Start)
		if resolution.Role == Local && resolution.Symbol != nil {
			return scalar, true
		}
	}
	value, matches := scalar, 0
	for ri, relation := range q.relations {
		if qualifier != nil && !relationMatchesQualifier(relation.ref, *qualifier) {
			continue
		}
		for _, column := range relation.columns {
			if name.MatchesCatalogName(column.Name) {
				value = singletonValue{ri, column.Name}
				matches++
			}
		}
	}
	return value, matches == 1
}

func (q *singletonQuery) projection(items []lexeme) (aggregate, column, ok bool) {
	if alias := topLevelWordIndex(items, "AS"); alias >= 0 {
		if alias+2 != len(items) || !isNameToken(items[alias+1]) {
			return false, false, false
		}
		items = items[:alias]
	}
	items = singletonUnwrap(items)
	if len(items) >= 4 && items[1].Token.Kind == token.LParen {
		switch itemWord(items[0]) {
		case "COUNT", "MAX", "MIN", "SUM", "AVG":
			args, end, valid := enclosedList(items, 1, len(items))
			if !valid || end != len(items) {
				return false, false, false
			}
			if isWord(items[0], "COUNT") && len(args) == 1 && args[0].Token.Kind == token.Mult {
				return true, false, true
			}
			if len(args) > 0 && (isWord(args[0], "DISTINCT") || isWord(args[0], "ALL")) {
				args = args[1:]
			}
			_, valid = q.value(args)
			return true, false, valid
		}
	}
	value, ok := q.value(items)
	return false, value.relation >= 0, ok
}

func (q *singletonQuery) columnList(items []lexeme, ordered bool) bool {
	if len(items) < 2 || !isWord(items[0], "BY") {
		return false
	}
	parts, ok := splitTopLevel(items[1:], token.Comma)
	if !ok {
		return false
	}
	for _, part := range parts {
		if ordered && len(part) > 1 && (isWord(part[len(part)-1], "ASC") || isWord(part[len(part)-1], "DESC")) {
			part = part[:len(part)-1]
		}
		if _, ok := q.value(part); !ok {
			return false
		}
	}
	return true
}

func (q *singletonQuery) keysBound() bool {
	bound := make([]bool, len(q.relations))
	columns := make(map[singletonValue]bool)
	for changed := true; changed; {
		changed = false
		for _, eq := range q.equalities {
			for _, pair := range [][2]singletonValue{{eq.left, eq.right}, {eq.right, eq.left}} {
				target, source := pair[0], pair[1]
				if target.relation < 0 || columns[target] || (eq.onlyRelation >= 0 && eq.onlyRelation != target.relation) {
					continue
				}
				if source.relation < 0 || bound[source.relation] || columns[source] {
					columns[target], changed = true, true
				}
			}
		}
		for ri, relation := range q.relations {
			if bound[ri] {
				continue
			}
			for _, key := range relation.keys {
				complete := len(key) > 0
				for _, column := range key {
					complete = complete && columns[singletonValue{ri, column}]
				}
				if complete {
					bound[ri], changed = true, true
					break
				}
			}
		}
	}
	for _, single := range bound {
		if !single {
			return false
		}
	}
	return true
}
