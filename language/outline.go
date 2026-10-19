package language

import (
	"context"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

// Outline is the query-level source outline for a SQL document.
type Outline struct {
	Queries []OutlineQuery
}

// OutlineQuery is one query and its source relations.
type OutlineQuery struct {
	ID        int
	Parent    int
	Kind      string
	Role      string
	RoleName  string
	Predicate string
	Range     Range
	Relations []OutlineRelation
}

// OutlineRelation is one base or derived relation occurrence in a query.
type OutlineRelation struct {
	Name         string
	NameText     string
	Alias        string
	AliasText    string
	NameRange    *Range
	AliasRange   Range
	DerivedQuery int
}

// QueryOutline returns an InterBase query and relation outline for text.
// Other drivers return an empty outline. Positions use the LSP coordinate
// system used by the rest of this package.
func QueryOutline(ctx context.Context, text string, dv dialect.DriverVariant) (Outline, error) {
	if err := ctx.Err(); err != nil {
		return Outline{}, ErrCanceled
	}
	queries, err := sqlsymbol.QueryOutline(text, dv)
	if err != nil {
		return Outline{}, err
	}
	outline := Outline{Queries: make([]OutlineQuery, len(queries))}
	for i, query := range queries {
		item := OutlineQuery{
			ID:        query.ID,
			Parent:    query.Parent,
			Kind:      query.Kind,
			Role:      query.Role,
			RoleName:  query.RoleName,
			Predicate: query.Predicate,
			Relations: make([]OutlineRelation, len(query.Relations)),
		}
		if r, ok := spanRange(text, query.Range); ok {
			item.Range = r
		}
		for j, relation := range query.Relations {
			converted := OutlineRelation{DerivedQuery: relation.DerivedQuery}
			if relation.Name != (sqlsymbol.Name{}) {
				converted.Name = relation.Name.Key()
			}
			if relation.NameRange != nil {
				if r, ok := spanRange(text, *relation.NameRange); ok {
					converted.NameRange = &r
					converted.NameText = text[relation.NameRange.Start:relation.NameRange.End]
				}
			}
			if relation.Alias != nil {
				converted.Alias = relation.Alias.Key()
				if relation.AliasRange.End > relation.AliasRange.Start && relation.AliasRange.End <= len(text) {
					if r, ok := spanRange(text, relation.AliasRange); ok {
						converted.AliasRange = r
						converted.AliasText = text[relation.AliasRange.Start:relation.AliasRange.End]
					}
				}
			}
			item.Relations[j] = converted
		}
		outline.Queries[i] = item
	}
	return outline, nil
}
