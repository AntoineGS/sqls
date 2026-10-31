package language

import (
	"context"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
	"github.com/sqls-server/sqls/parser"
)

// CatalogReferences returns, in source order, the ranges in the document that
// refer to object, or to its column when column is not empty. Each occurrence
// is resolved with Definition's rules, so same-named procedure locals,
// comments, literals and other relations' columns are excluded; trigger
// NEW/OLD row columns belong to the trigger's FOR relation. Object and column
// use exact catalog spellings. Only InterBase with current metadata resolves.
func (s *Service) CatalogReferences(ctx context.Context, req Request, object catalog.ObjectID, column string) (Result[[]Range], error) {
	if err := ctx.Err(); err != nil {
		return Result[[]Range]{}, ErrCanceled
	}
	ranges := []Range{}
	cache := req.Context.SnapshotCache()
	if req.Context.Variant.Driver != dialect.DatabaseDriverInterBase || cache == nil {
		return Result[[]Range]{Value: ranges, Stamp: stampFor(req)}, nil
	}
	text := req.Document.Text
	analysis, err := sqlsymbol.Analyze(text, req.Context.Variant)
	if err != nil {
		return Result[[]Range]{}, err
	}
	parsed, err := parser.ParseWithDriverVariant(text, req.Context.Variant)
	if err != nil {
		return Result[[]Range]{}, err
	}
	same := func(id catalog.ObjectID) bool { return id.Kind == object.Kind && id.Name == object.Name }
	name := object.Name
	if column != "" {
		name = column
	}
	for _, span := range analysis.CatalogNameSpans(name) {
		if err := ctx.Err(); err != nil {
			return Result[[]Range]{}, ErrCanceled
		}
		position, err := PositionAt(text, span.Start)
		if err != nil {
			continue
		}
		resolution := analysis.Resolve(span.Start)
		found := false
		switch {
		case column != "":
			found, err = columnReferenceTo(req, analysis, resolution, span, position, cache, same)
		case resolution.Role != sqlsymbol.Column:
			var target catalog.ObjectID
			target, _, found, err = navigationTargetAtWithAnalysis(req, position, parsed, analysis)
			found = found && same(target)
		}
		if err != nil {
			return Result[[]Range]{}, err
		}
		if r, ok := navigationSpanRange(text, span); found && ok {
			ranges = append(ranges, r)
		}
	}
	return Result[[]Range]{Value: ranges, Stamp: stampFor(req)}, nil
}

func columnReferenceTo(req Request, analysis *sqlsymbol.Analysis, resolution sqlsymbol.Resolution, span sqlsymbol.Span,
	position Position, cache *catalog.Cache, same func(catalog.ObjectID) bool) (bool, error) {
	if relation, ok := analysis.TriggerRowRelation(span.Start); ok {
		owner, ok := resolveCatalogRelation(relation, cache)
		return ok && same(owner), nil
	}
	switch resolution.Role {
	case sqlsymbol.Column:
		if resolution.SQL == nil || len(resolution.SQL.Scopes) == 0 {
			return false, nil
		}
		owner, ok := resolveCatalogColumn(*resolution.SQL, cache)
		return ok && same(owner), nil
	case sqlsymbol.Ambiguous:
		owner, _, ok, err := provenDefinitionColumnAt(req, position)
		return ok && same(owner), err
	}
	return false, nil
}
