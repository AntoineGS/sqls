package language

import (
	"context"
	"unicode/utf8"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

// CatalogReferences returns, in source order, the ranges in the document that
// refer to object, or to its column when column is not empty. Each occurrence
// is resolved with Definition's rules, so procedure and trigger locals,
// :variables, comments, literals and other relations' columns are excluded;
// trigger NEW/OLD row columns belong to the trigger's FOR relation. Object and
// column use exact catalog spellings. Only InterBase with current metadata
// resolves. The document is analyzed once, so the cost stays linear in its size.
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
	same := func(id catalog.ObjectID, ok bool) bool { return ok && id.Kind == object.Kind && id.Name == object.Name }
	triggers := analysis.Triggers()
	rowRelations := make(map[sqlsymbol.Span]sqlsymbol.Name)
	for _, trg := range triggers {
		for _, span := range trg.RowColumns {
			rowRelations[span] = trg.Relation
		}
	}
	triggerLocal := func(occurrence sqlsymbol.NameOccurrence) bool {
		for _, trg := range triggers {
			if trg.Span.Start <= occurrence.Span.Start && occurrence.Span.End <= trg.Span.End {
				return trg.Locals[occurrence.Name.Key()]
			}
		}
		return false
	}
	prove := analysis.DefinitionColumnProver(snapshotSemanticCatalog{snapshot: req.Context.Snapshot})
	name := object.Name
	if column != "" {
		name = column
	}
	positions := positionCursor{text: text}
	for _, occurrence := range analysis.CatalogNameSpans(name) {
		if err := ctx.Err(); err != nil {
			return Result[[]Range]{}, ErrCanceled
		}
		resolution := analysis.Resolve(occurrence.Span.Start)
		found := false
		if column != "" {
			if relation, ok := rowRelations[occurrence.Span]; ok {
				found = same(resolveCatalogRelation(relation, cache))
			} else if resolution.Role == sqlsymbol.Column && resolution.SQL != nil && len(resolution.SQL.Scopes) > 0 {
				found = same(resolveCatalogColumn(*resolution.SQL, cache))
			} else if proof, ok := prove(occurrence.Span.Start); ok {
				found = same(provenBaseTableColumn(req.Context.Snapshot, proof))
			}
		} else if resolution.Role == sqlsymbol.Relation || resolution.Role == sqlsymbol.Callable || !triggerLocal(occurrence) {
			found = same(catalogObjectFor(resolution, occurrence.Name, cache))
		}
		if !found {
			continue
		}
		start, startOK := positions.at(occurrence.Span.Start)
		end, endOK := positions.at(occurrence.Span.End)
		if startOK && endOK {
			ranges = append(ranges, Range{Start: start, End: end})
		}
	}
	return Result[[]Range]{Value: ranges, Stamp: stampFor(req)}, nil
}

// catalogObjectFor applies navigationTargetAtWithAnalysis's object rules to a
// resolved word: a relation by its SQL name; otherwise a procedure, view or
// trigger by name, never a column, alias or procedure local.
func catalogObjectFor(resolution sqlsymbol.Resolution, name sqlsymbol.Name, cache *catalog.Cache) (catalog.ObjectID, bool) {
	if resolution.InProcedure {
		switch resolution.Role {
		case sqlsymbol.Local, sqlsymbol.Other, sqlsymbol.Ambiguous:
			return catalog.ObjectID{}, false
		}
	}
	switch resolution.Role {
	case sqlsymbol.Relation:
		if resolution.SQL == nil {
			return catalog.ObjectID{}, false
		}
		return resolveCatalogRelation(resolution.SQL.Name, cache)
	case sqlsymbol.Column, sqlsymbol.Alias, sqlsymbol.Local, sqlsymbol.Ambiguous:
		return catalog.ObjectID{}, false
	}
	return catalogObjectByName(name, cache)
}

// positionCursor converts ascending byte offsets to Positions in one forward
// pass, with PositionAt's line delimiters and UTF-16 columns.
type positionCursor struct {
	text                 string
	offset, line, column int
}

func (c *positionCursor) at(target int) (Position, bool) {
	if target < c.offset || target > len(c.text) {
		return Position{}, false
	}
	for c.offset < target {
		switch c.text[c.offset] {
		case '\n':
			c.offset, c.line, c.column = c.offset+1, c.line+1, 0
		case '\r':
			if c.offset+1 < len(c.text) && c.text[c.offset+1] == '\n' {
				if target == c.offset+1 {
					return Position{}, false
				}
				c.offset++
			}
			c.offset, c.line, c.column = c.offset+1, c.line+1, 0
		default:
			r, size := utf8.DecodeRuneInString(c.text[c.offset:])
			if r == utf8.RuneError && size <= 1 {
				return Position{}, false
			}
			if r > 0xFFFF {
				c.column++
			}
			c.offset, c.column = c.offset+size, c.column+1
		}
	}
	return Position{Line: c.line, UTF16Column: c.column}, true
}
