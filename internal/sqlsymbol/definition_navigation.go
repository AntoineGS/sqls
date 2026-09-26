package sqlsymbol

import "github.com/sqls-server/sqls/token"

// DefinitionColumn identifies a catalog-backed base relation column. It is
// intentionally a definition-only proof and does not alter symbol roles.
type DefinitionColumn struct {
	Relation Name
	Column   Name
}

// ProvenDefinitionColumn returns the base relation and column for an eligible
// local/column collision. It is deliberately stricter than diagnostics or
// ordinary column resolution: only a top-level, single-source SELECT with a
// complete projection and complete semantic relation metadata is accepted.
// Unsupported, correlated, derived, callable, CTE, stale, or ambiguous shapes
// return false. The result never changes Analysis.Resolve's Role.
func (a *Analysis) ProvenDefinitionColumn(offset int, catalog SemanticCatalog) (DefinitionColumn, bool) {
	if a == nil || catalog == nil {
		return DefinitionColumn{}, false
	}
	resolution := a.Resolve(offset)
	if resolution.Role != Ambiguous || !resolution.DefinitionColumnCandidate || resolution.SQL == nil {
		return DefinitionColumn{}, false
	}
	model := a.diagnosticModel(catalog)
	item := model.itemIndexAt(resolution.Span.Start)
	if item < 0 || model.Unsupported(item) {
		return DefinitionColumn{}, false
	}
	qi := model.queryAt[item]
	if qi < 0 || qi >= len(model.queries) {
		return DefinitionColumn{}, false
	}
	query := model.queries[qi]
	if query.kind != "SELECT" || query.parent != -1 || query.malformedProjection || !query.output.CountKnown || model.unionOf[qi] >= 0 {
		return DefinitionColumn{}, false
	}
	if query.statementIndex < 0 || query.statementIndex >= len(model.statements) {
		return DefinitionColumn{}, false
	}
	statement := model.statements[query.statementIndex]
	if statement.Malformed || statement.Start >= statement.End || isWord(model.items[statement.Start], "WITH") {
		return DefinitionColumn{}, false
	}
	statementQueries := 0
	for _, candidate := range model.queries {
		if candidate.statementIndex == query.statementIndex {
			statementQueries++
		}
	}
	if statementQueries != 1 {
		return DefinitionColumn{}, false
	}
	details := model.RelationCandidates(qi)
	if len(details) != 1 || len(query.relations) != 1 || details[0].ref.Name.Key() == "" {
		return DefinitionColumn{}, false
	}
	relation := details[0].ref
	_, isCTE := model.cteFor(qi, relation.Name.Key())
	if relation.Name.Key() == "" || isCTE || model.DDLInvalidated(item, relation.Name) {
		return DefinitionColumn{}, false
	}
	if details[0].start < 0 || details[0].start >= len(model.items) || model.items[details[0].start].Token.Kind == token.LParen {
		return DefinitionColumn{}, false
	}
	// A parenthesized FROM callable has a name and is not a base relation.
	if details[0].start+1 < len(model.items) && model.items[details[0].start+1].Token.Kind == token.LParen {
		return DefinitionColumn{}, false
	}
	fact, knowledge := catalog.RelationInfo(relation.Name)
	if knowledge != Present || !fact.ColumnsKnown {
		return DefinitionColumn{}, false
	}
	columnMatches := 0
	for _, column := range fact.Columns {
		if resolution.SQL.Name.MatchesCatalogName(column.Name) {
			columnMatches++
		}
	}
	if columnMatches != 1 {
		return DefinitionColumn{}, false
	}
	return DefinitionColumn{Relation: relation.Name, Column: resolution.SQL.Name}, true
}
