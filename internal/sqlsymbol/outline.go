package sqlsymbol

import (
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/token"
)

// OutlineQuery is one discovered query with source offsets into the original
// SQL text. Its spans are half-open byte ranges.
type OutlineQuery struct {
	ID        int
	Parent    int
	Kind      string
	Role      string
	RoleName  string
	Predicate string
	Range     Span
	Relations []OutlineRelation
}

// OutlineRelation is a source relation occurrence in an outline query.
type OutlineRelation struct {
	Name         Name
	NameRange    *Span
	Alias        *Name
	AliasRange   Span
	DerivedQuery int
}

// QueryOutline discovers SQL queries and their source relations. It is a
// best-effort source-shape pass: syntax accepted by the lexer is outlined even
// when the statement is incomplete.
func QueryOutline(text string, dv dialect.DriverVariant) ([]OutlineQuery, error) {
	if dv.Driver != dialect.DatabaseDriverInterBase {
		return []OutlineQuery{}, nil
	}

	items, err := lex(text, dv)
	if err != nil {
		return nil, err
	}
	items = significantLexemes(items)
	contexts, _ := buildContexts(&Analysis{Text: text}, items)
	depths, matching := sqlDepths(items)
	queries := discoverSQLQueries(items, contexts, depths)

	result := make([]OutlineQuery, len(queries))
	for i, query := range queries {
		span := Span{}
		if query.start >= 0 && query.start < query.end && query.end <= len(items) {
			span = Span{Start: items[query.start].Span.Start, End: items[query.end-1].Span.End}
		}
		result[i] = OutlineQuery{
			ID:        i,
			Parent:    query.parent,
			Kind:      query.kind,
			Range:     span,
			Relations: make([]OutlineRelation, 0),
		}
	}

	for i, query := range queries {
		relations, _, _ := queryRelationOccurrences(text, items, query, depths, matching)
		for _, occurrence := range relations {
			relation := OutlineRelation{
				Name:         occurrence.ref.Name,
				Alias:        occurrence.ref.Alias,
				AliasRange:   occurrence.ref.AliasDeclaration,
				DerivedQuery: -1,
			}
			if occurrence.nameSpan.End > occurrence.nameSpan.Start {
				nameSpan := occurrence.nameSpan
				relation.NameRange = &nameSpan
			}
			if occurrence.start >= 0 && occurrence.start < len(items) && items[occurrence.start].Token.Kind == token.LParen {
				for childID, child := range queries {
					if child.parent == i && child.start == occurrence.start+1 {
						relation.DerivedQuery = childID
						break
					}
				}
			}
			result[i].Relations = append(result[i].Relations, relation)
		}
	}

	for i, query := range queries {
		role, roleName, predicate := outlineQueryRole(text, i, query, queries, result, items, depths, matching)
		result[i].Role = role
		result[i].RoleName = roleName
		result[i].Predicate = predicate
	}
	return result, nil
}

type relationOccurrence struct {
	ref      RelationRef
	nameSpan Span
	start    int
}

func queryRelationOccurrences(text string, items []lexeme, query sqlQuery, depths []int, matching map[int]int) ([]relationOccurrence, relationOccurrence, bool) {
	if query.start >= query.end {
		return nil, relationOccurrence{}, false
	}
	seen := make(map[int]bool)
	addAt := func(index int, relations *[]relationOccurrence) int {
		ref, nameSpan, next, ok := relationAtWithNameSpan(text, items, index, query.end, depths, matching, true)
		if !ok || seen[index] {
			return index
		}
		seen[index] = true
		*relations = append(*relations, relationOccurrence{ref: ref, nameSpan: nameSpan, start: index})
		return next - 1
	}

	var relations []relationOccurrence
	var target relationOccurrence
	hasTarget := false
	if query.kind == "UPDATE" {
		if index := nextName(items, query.start+1, query.end); index >= 0 {
			ref, nameSpan, _, ok := relationAtWithNameSpan(text, items, index, query.end, depths, matching, false)
			if ok {
				target = relationOccurrence{ref: ref, nameSpan: nameSpan, start: index}
				relations = append(relations, target)
				seen[index] = true
				hasTarget = true
			}
		}
	} else if query.kind == "INSERT" {
		if index := wordAfter(items, query.start+1, query.end, "INTO"); index >= 0 {
			ref, nameSpan, _, ok := relationAtWithNameSpan(text, items, index, query.end, depths, matching, false)
			if ok {
				target = relationOccurrence{ref: ref, nameSpan: nameSpan, start: index}
				relations = append(relations, target)
				seen[index] = true
				hasTarget = true
			}
		}
	} else if query.kind == "DELETE" {
		if index := wordAfter(items, query.start+1, query.end, "FROM"); index >= 0 {
			ref, nameSpan, _, ok := relationAtWithNameSpan(text, items, index, query.end, depths, matching, false)
			if ok {
				target = relationOccurrence{ref: ref, nameSpan: nameSpan, start: index}
				relations = append(relations, target)
				seen[index] = true
				hasTarget = true
			}
		}
	}

	inFrom := false
	for i := query.start + 1; i < query.end; i++ {
		if depths[i] != query.baseDepth {
			continue
		}
		if close, ok := matching[i]; ok {
			i = close
			continue
		}
		switch {
		case isWord(items[i], "FROM"), isWord(items[i], "USING"):
			inFrom = true
			if isWord(items[i], "FROM") && query.kind == "DELETE" {
				inFrom = false
			}
		case isWord(items[i], "JOIN"):
			inFrom = true
			i = addAt(i+1, &relations)
		case inFrom && items[i].Token.Kind == token.Comma:
			i = addAt(i+1, &relations)
		case inFrom && isQueryClause(items[i]):
			inFrom = false
		default:
			if inFrom && query.kind == "SELECT" && i == query.start+1 {
				i = addAt(i, &relations)
			}
		}
		if isWord(items[i], "FROM") && query.kind == "SELECT" {
			i = addAt(i+1, &relations)
		}
	}
	return relations, target, hasTarget
}

func outlineQueryRole(text string, id int, query sqlQuery, queries []sqlQuery, outline []OutlineQuery, items []lexeme, depths []int, matching map[int]int) (string, string, string) {
	if isUnionArm(query, items, depths) {
		return "union", "", ""
	}
	if query.parent < 0 {
		return "main", "", ""
	}
	for _, relation := range outline[query.parent].Relations {
		if relation.DerivedQuery == id {
			roleName := ""
			if relation.Alias != nil {
				roleName = relation.Alias.Key()
			}
			return "derived", roleName, ""
		}
	}

	parent := queries[query.parent]
	open := query.start - 1
	if parent.kind == "SELECT" && query.start > parent.start && open >= 0 && items[open].Token.Kind == token.LParen && isSelectListSubquery(query, parent, items, depths) {
		roleName := selectSubqueryAlias(text, open, parent.end, items, depths, matching)
		return "column", roleName, ""
	}

	clause := precedingQueryClause(parent, open, items, depths)
	predicate := ""
	if open > 0 {
		if isWord(items[open-1], "EXISTS") {
			predicate = "EXISTS"
		} else if isWord(items[open-1], "IN") {
			predicate = "IN"
		}
	}
	switch clause {
	case "WHERE":
		return "where", "", predicate
	case "HAVING":
		return "having", "", predicate
	case "ON":
		return "join", "", predicate
	default:
		return "other", "", ""
	}
}

func isUnionArm(query sqlQuery, items []lexeme, depths []int) bool {
	index := query.start - 1
	for index >= 0 && depths[index] == query.baseDepth && (isWord(items[index], "ALL") || isWord(items[index], "DISTINCT")) {
		index--
	}
	return index >= 0 && depths[index] == query.baseDepth && isWord(items[index], "UNION")
}

func isSelectListSubquery(query, parent sqlQuery, items []lexeme, depths []int) bool {
	for i := parent.start + 1; i < query.start-1 && i < parent.end; i++ {
		if depths[i] == parent.baseDepth && (isWord(items[i], "FROM") || isQueryClause(items[i])) {
			return false
		}
	}
	return true
}

func selectSubqueryAlias(text string, open, parentEnd int, items []lexeme, depths []int, matching map[int]int) string {
	close, ok := matching[open]
	if !ok || close+1 >= parentEnd {
		return ""
	}
	next := close + 1
	if isWord(items[next], "AS") {
		next++
	}
	if next >= parentEnd || depths[next] != depths[open] || !isNameToken(items[next]) || isSQLClause(items[next]) {
		return ""
	}
	name, ok := nameFromLexeme(text, items[next])
	if !ok {
		return ""
	}
	return name.Key()
}

func precedingQueryClause(parent sqlQuery, before int, items []lexeme, depths []int) string {
	clause := ""
	for i := parent.start + 1; i < before && i < parent.end; i++ {
		if depths[i] != parent.baseDepth {
			continue
		}
		switch {
		case isWord(items[i], "WHERE"):
			clause = "WHERE"
		case isWord(items[i], "HAVING"):
			clause = "HAVING"
		case isWord(items[i], "ON"):
			clause = "ON"
		}
	}
	return clause
}
