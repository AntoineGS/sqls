package sqlsymbol

import "github.com/sqls-server/sqls/token"

type sqlQuery struct {
	start, end int
	baseDepth  int
	parent     int
	kind       string
	relations  []RelationRef
	target     RelationRef
	hasTarget  bool
}

// buildSQLOwnership records the relations visible at every SQL token. It is
// intentionally a source-shape pass: it does not try to infer column lineage
// from a derived SELECT or decide whether a catalog column exists.
func buildSQLOwnership(text string, items []lexeme, contexts []tokenContext) [][][]RelationRef {
	depths, matching := sqlDepths(items)
	queries := discoverSQLQueries(items, contexts, depths)
	for i := range queries {
		queries[i].relations, queries[i].target, queries[i].hasTarget = queryRelations(text, items, queries[i], depths, matching)
	}

	queryAt := make([]int, len(items))
	for i := range queryAt {
		queryAt[i] = -1
	}
	for qi, query := range queries {
		for i := query.start; i < query.end; i++ {
			if queryAt[i] < 0 || queryLength(queries[queryAt[i]]) > queryLength(query) {
				queryAt[i] = qi
			}
		}
	}

	scopes := make([][][]RelationRef, len(items))
	targetScopes := make([][][]RelationRef, len(items))
	for i, qi := range queryAt {
		if qi < 0 {
			continue
		}
		for current := qi; current >= 0; current = queries[current].parent {
			scopes[i] = append(scopes[i], append([]RelationRef(nil), queries[current].relations...))
		}
		if contexts[i].insertColumn || contexts[i].updateTarget {
			if queries[qi].hasTarget {
				targetScopes[i] = [][]RelationRef{{queries[qi].target}}
			}
		}
	}

	// A target-column scope is only useful for references classified as a SQL
	// column. Keep it separate so relation and alias navigation still exposes
	// the full query context.
	result := make([][][]RelationRef, len(items))
	for i := range scopes {
		result[i] = scopes[i]
		if targetScopes[i] != nil {
			result[i] = targetScopes[i]
		}
	}
	return result
}

func queryLength(query sqlQuery) int { return query.end - query.start }

func sqlDepths(items []lexeme) ([]int, map[int]int) {
	depths := make([]int, len(items))
	matching := make(map[int]int)
	stack := make([]int, 0, 8)
	depth := 0
	for i, item := range items {
		depths[i] = depth
		switch item.Token.Kind {
		case token.LParen:
			stack = append(stack, i)
			depth++
		case token.RParen:
			if depth > 0 {
				depth--
			}
			if len(stack) > 0 {
				open := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				matching[open] = i
			}
		}
	}
	return depths, matching
}

func discoverSQLQueries(items []lexeme, contexts []tokenContext, depths []int) []sqlQuery {
	queries := make([]sqlQuery, 0, 4)
	active := make([]int, 0, 4)
	for i, item := range items {
		if item.Token.Kind == token.Semicolon {
			closeQueries(queries, &active, i)
			continue
		}
		if item.Token.Kind == token.RParen {
			for len(active) > 0 {
				qi := active[len(active)-1]
				if depths[i] > queries[qi].baseDepth {
					break
				}
				queries[qi].end = i
				active = active[:len(active)-1]
			}
		}
		inSQL := contexts[i].kind == contextSQL || contexts[i].kind == contextExecute
		if !inSQL {
			// FOR SELECT returns to procedural context at DO. The SELECT
			// is not a lexical parent of statements in that body.
			closeQueries(queries, &active, i)
			continue
		}
		if !isQueryStart(item) {
			continue
		}
		// A query at the same parenthesis depth is a sibling, not a
		// correlated subquery. This includes UNION arms and an INSERT
		// ... SELECT source query.
		closeQueriesAtDepth(queries, &active, i, depths[i])
		parent := -1
		if len(active) > 0 {
			parent = active[len(active)-1]
		}
		query := sqlQuery{start: i, end: len(items), baseDepth: depths[i], parent: parent, kind: itemWord(item)}
		queries = append(queries, query)
		active = append(active, len(queries)-1)
	}
	for _, qi := range active {
		queries[qi].end = len(items)
	}
	return queries
}

func closeQueries(queries []sqlQuery, active *[]int, end int) {
	for len(*active) > 0 {
		qi := (*active)[len(*active)-1]
		queries[qi].end = end
		*active = (*active)[:len(*active)-1]
	}
}

func closeQueriesAtDepth(queries []sqlQuery, active *[]int, end, depth int) {
	for len(*active) > 0 {
		qi := (*active)[len(*active)-1]
		if queries[qi].baseDepth < depth {
			return
		}
		queries[qi].end = end
		*active = (*active)[:len(*active)-1]
	}
}

func isQueryStart(item lexeme) bool {
	return isWord(item, "SELECT") || isWord(item, "UPDATE") || isWord(item, "INSERT") || isWord(item, "DELETE")
}

func itemWord(item lexeme) string {
	word, ok := item.Token.Value.(*token.SQLWord)
	if !ok {
		return ""
	}
	return word.Keyword
}

func queryRelations(text string, items []lexeme, query sqlQuery, depths []int, matching map[int]int) ([]RelationRef, RelationRef, bool) {
	occurrences, target, hasTarget := queryRelationOccurrences(text, items, query, depths, matching)
	relations := make([]RelationRef, 0, len(occurrences))
	for _, occurrence := range occurrences {
		relations = append(relations, occurrence.ref)
	}
	return relations, target.ref, hasTarget
}

func isQueryClause(item lexeme) bool {
	for _, word := range []string{"WHERE", "GROUP", "ORDER", "HAVING", "UNION", "RETURNING", "SET", "VALUES"} {
		if isWord(item, word) {
			return true
		}
	}
	return false
}

func nextName(items []lexeme, start, end int) int {
	for i := start; i < end; i++ {
		if isNameToken(items[i]) {
			return i
		}
		if items[i].Token.Kind == token.Semicolon || items[i].Token.Kind == token.LParen {
			return -1
		}
	}
	return -1
}

func wordAfter(items []lexeme, start, end int, word string) int {
	for i := start; i < end; i++ {
		if isWord(items[i], word) {
			return nextName(items, i+1, end)
		}
		if items[i].Token.Kind == token.Semicolon {
			return -1
		}
	}
	return -1
}

func relationAt(text string, items []lexeme, start, end int, depths []int, matching map[int]int, callable bool) (RelationRef, int, bool) {
	ref, _, next, ok := relationAtWithNameSpan(text, items, start, end, depths, matching, callable)
	return ref, next, ok
}

func relationAtWithNameSpan(text string, items []lexeme, start, end int, depths []int, matching map[int]int, callable bool) (RelationRef, Span, int, bool) {
	if start < 0 || start >= end {
		return RelationRef{}, Span{}, start, false
	}
	if items[start].Token.Kind == token.LParen {
		close, ok := matching[start]
		if !ok || close >= end {
			return RelationRef{}, Span{}, start, false
		}
		next := close + 1
		if next < end && isWord(items[next], "AS") {
			next++
		}
		var alias *Name
		var aliasDeclaration Span
		if next < end && depths[next] == depths[start] && isNameToken(items[next]) && !isSQLClause(items[next]) {
			name, _ := nameFromLexeme(text, items[next])
			alias = &name
			aliasDeclaration = items[next].Span
			next++
		}
		return RelationRef{Alias: alias, AliasDeclaration: aliasDeclaration}, Span{}, next, true
	}
	if !isNameToken(items[start]) {
		return RelationRef{}, Span{}, start, false
	}
	name, ok := nameFromLexeme(text, items[start])
	if !ok {
		return RelationRef{}, Span{}, start, false
	}
	nameSpan := items[start].Span
	next := start + 1
	for next+1 < end && items[next].Token.Kind == token.Period && isNameToken(items[next+1]) {
		name, _ = nameFromLexeme(text, items[next+1])
		nameSpan = items[next+1].Span
		next += 2
	}
	if callable && next < end && items[next].Token.Kind == token.LParen {
		close, ok := matching[next]
		if !ok || close >= end {
			return RelationRef{}, Span{}, start, false
		}
		next = close + 1
		if next < end && isWord(items[next], "AS") {
			next++
		}
		var alias *Name
		var aliasDeclaration Span
		if next < end && depths[next] == depths[start] && isNameToken(items[next]) && !isSQLClause(items[next]) {
			aliasName, _ := nameFromLexeme(text, items[next])
			alias = &aliasName
			aliasDeclaration = items[next].Span
			next++
		}
		return RelationRef{Alias: alias, AliasDeclaration: aliasDeclaration}, Span{}, next, true
	}
	var alias *Name
	var aliasDeclaration Span
	if next < end && isWord(items[next], "AS") {
		next++
	}
	if next < end && depths[next] == depths[start] && isNameToken(items[next]) && !isSQLClause(items[next]) {
		aliasName, _ := nameFromLexeme(text, items[next])
		alias = &aliasName
		aliasDeclaration = items[next].Span
		next++
	}
	return RelationRef{Name: name, Alias: alias, AliasDeclaration: aliasDeclaration}, nameSpan, next, true
}
