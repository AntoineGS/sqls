package language

import (
	"unicode/utf8"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/ast/astutil"
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
	"github.com/sqls-server/sqls/parser"
	"github.com/sqls-server/sqls/parser/parseutil"
	"github.com/sqls-server/sqls/token"
)

// navigationTargetAt interprets an InterBase symbol before any DetailReader
// call. Local, alias, non-target, unknown and ambiguous names stop here; only a
// catalog identity proven by the request snapshot can proceed to materializing
// details. Other engines retain their historical relation interpretation.
func navigationTargetAt(req Request, position Position) (catalog.ObjectID, Range, bool, error) {
	if req.Context.Variant.Driver != dialect.DatabaseDriverInterBase {
		return relationAt(req, position)
	}
	if _, err := ByteOffset(req.Document.Text, position); err != nil {
		return catalog.ObjectID{}, Range{}, false, nil
	}
	parsed, err := parser.ParseWithDriverVariant(req.Document.Text, req.Context.Variant)
	if err != nil {
		return catalog.ObjectID{}, Range{}, false, err
	}
	analysis, err := sqlsymbol.Analyze(req.Document.Text, req.Context.Variant)
	if err != nil {
		return catalog.ObjectID{}, Range{}, false, err
	}
	return navigationTargetAtWithAnalysis(req, position, parsed, analysis)
}

func navigationTargetAtWithAnalysis(req Request, position Position, parsed ast.TokenList, analysis *sqlsymbol.Analysis) (catalog.ObjectID, Range, bool, error) {
	if req.Context.Variant.Driver != dialect.DatabaseDriverInterBase {
		return relationAt(req, position)
	}
	offset, err := ByteOffset(req.Document.Text, position)
	if err != nil {
		return catalog.ObjectID{}, Range{}, false, nil
	}
	if analysis == nil {
		return catalog.ObjectID{}, Range{}, false, nil
	}
	resolution := analysis.Resolve(offset)
	if resolution.InProcedure {
		switch resolution.Role {
		case sqlsymbol.Local, sqlsymbol.Other, sqlsymbol.Ambiguous:
			return catalog.ObjectID{}, Range{}, false, nil
		}
	}
	switch resolution.Role {
	case sqlsymbol.Relation:
		if resolution.SQL == nil {
			return catalog.ObjectID{}, Range{}, false, nil
		}
		target, ok := resolveCatalogRelation(resolution.SQL.Name, req.Context.SnapshotCache())
		return target, rangeForResolution(req.Document.Text, resolution), ok, nil
	case sqlsymbol.Column:
		if resolution.SQL == nil || len(resolution.SQL.Scopes) == 0 {
			return catalog.ObjectID{}, Range{}, false, nil
		}
		target, ok := resolveCatalogColumn(*resolution.SQL, req.Context.SnapshotCache())
		return target, rangeForResolution(req.Document.Text, resolution), ok, nil
	case sqlsymbol.Alias, sqlsymbol.Local, sqlsymbol.Ambiguous:
		return catalog.ObjectID{}, Range{}, false, nil
	}
	// Procedure/view/trigger definitions are valid on ordinary identifier
	// tokens outside a relation expression. Use the parser's identifier node to
	// exclude comments and literals, while the exact cache resolver enforces
	// Procedure > View > Trigger precedence.
	name, quoted, span, ok, err := identifierAtParsed(req.Document.Text, position, parsed)
	if err != nil || !ok {
		return catalog.ObjectID{}, Range{}, false, err
	}
	cache := req.Context.SnapshotCache()
	if cache == nil || !cache.MetadataReady(catalog.MetadataProcedures, catalog.MetadataViews, catalog.MetadataTriggers) {
		return catalog.ObjectID{}, Range{}, false, nil
	}
	for _, kind := range []catalog.ObjectKind{catalog.ObjectKindProcedure, catalog.ObjectKindView, catalog.ObjectKindTrigger} {
		resolved, found, ambiguous := resolveCatalogObjectName(kind, sqlsymbol.Name{Text: name, Quoted: quoted}, cache)
		if len(ambiguous) != 0 {
			return catalog.ObjectID{}, Range{}, false, nil
		}
		if found {
			return catalog.ObjectID{Kind: kind, Name: resolved}, span, true, nil
		}
	}
	return catalog.ObjectID{}, Range{}, false, nil
}

func (c Context) SnapshotCache() *catalog.Cache {
	if c.Snapshot == nil || c.Snapshot.Stale {
		return nil
	}
	return c.Snapshot.Cache
}

func rangeForResolution(text string, resolution sqlsymbol.Resolution) Range {
	if r, ok := navigationSpanRange(text, resolution.Span); ok {
		return r
	}
	return Range{}
}

func resolveCatalogRelation(name sqlsymbol.Name, cache *catalog.Cache) (catalog.ObjectID, bool) {
	if cache == nil || !cache.MetadataReady(catalog.MetadataRelations, catalog.MetadataViews) {
		return catalog.ObjectID{}, false
	}
	if cache.HasCatalog() {
		view, found, ambiguous := resolveCatalogObjectName(catalog.ObjectKindView, name, cache)
		if len(ambiguous) > 0 {
			return catalog.ObjectID{}, false
		}
		if found {
			return catalog.ObjectID{Kind: catalog.ObjectKindView, Name: view}, true
		}
	}
	var matches []string
	for _, tables := range cache.SchemaTables {
		for _, table := range tables {
			if name.MatchesCatalogName(table) {
				matches = append(matches, table)
			}
		}
	}
	if len(matches) != 1 {
		return catalog.ObjectID{}, false
	}
	return catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: matches[0]}, true
}

func resolveCatalogObjectName(kind catalog.ObjectKind, name sqlsymbol.Name, cache *catalog.Cache) (string, bool, []catalog.ObjectID) {
	if cache == nil || !cache.HasCatalog() || cache.Catalog.Objects == nil {
		return "", false, nil
	}
	if name.Quoted {
		id, found := cache.Catalog.Objects.Lookup(catalog.ObjectID{Kind: kind, Name: name.Text})
		if !found {
			return "", false, nil
		}
		return id.Name, true, nil
	}
	matches := cache.Catalog.Objects.LookupUnquoted(kind, name.Text)
	switch len(matches) {
	case 0:
		return "", false, nil
	case 1:
		return matches[0].Name, true, nil
	default:
		return "", false, matches
	}
}

func resolveCatalogColumn(ref sqlsymbol.SQLReference, cache *catalog.Cache) (catalog.ObjectID, bool) {
	if cache == nil || !cache.MetadataReady(catalog.MetadataRelations, catalog.MetadataViews) ||
		!(cache.MetadataReady(catalog.MetadataColumnsCurrent) || cache.MetadataReady(catalog.MetadataColumnsAll)) || len(cache.ColumnsByID) == 0 {
		return catalog.ObjectID{}, false
	}
	if ref.Qualifier != nil {
		for _, scope := range ref.Scopes {
			var bindings []sqlsymbol.RelationRef
			for _, binding := range scope {
				matched := binding.Name.Key() == ref.Qualifier.Key()
				if binding.Alias != nil {
					matched = binding.Alias.Key() == ref.Qualifier.Key()
				}
				if matched {
					bindings = append(bindings, binding)
				}
			}
			if len(bindings) == 0 {
				continue
			}
			if len(bindings) != 1 {
				return catalog.ObjectID{}, false
			}
			target, columns, known := catalogRelationColumns(bindings[0].Name, cache)
			return target, known && hasCatalogColumn(ref.Name, columns)
		}
		return catalog.ObjectID{}, false
	}
	for _, scope := range ref.Scopes {
		if len(scope) == 0 {
			continue
		}
		var owner catalog.ObjectID
		matches := 0
		for _, binding := range scope {
			target, columns, known := catalogRelationColumns(binding.Name, cache)
			if !known {
				// Unknown or derived ownership makes the nearest scope unsafe.
				return catalog.ObjectID{}, false
			}
			if hasCatalogColumn(ref.Name, columns) {
				matches++
				owner = target
			}
		}
		if matches == 1 {
			return owner, true
		}
		if matches == 0 {
			continue
		}
		return catalog.ObjectID{}, false
	}
	return catalog.ObjectID{}, false
}

func catalogRelationColumns(relation sqlsymbol.Name, cache *catalog.Cache) (catalog.ObjectID, []*catalog.ColumnDesc, bool) {
	target, ok := resolveCatalogRelation(relation, cache)
	if !ok {
		return catalog.ObjectID{}, nil, false
	}
	var columns []*catalog.ColumnDesc
	if target.Kind == catalog.ObjectKindView {
		view, ok := cache.ViewByID(target.Name)
		if !ok || view.Columns == nil {
			return target, nil, false
		}
		columns = view.Columns
	} else {
		var ok bool
		columns, ok = cache.ColumnDescsByID(target.Name)
		if !ok {
			return target, nil, false
		}
	}
	return target, columns, true
}

func hasCatalogColumn(name sqlsymbol.Name, columns []*catalog.ColumnDesc) bool {
	for _, candidate := range columns {
		if candidate != nil && name.MatchesCatalogName(candidate.Name) {
			return true
		}
	}
	return false
}

func identifierAt(text string, position Position, dv dialect.DriverVariant) (string, bool, Range, bool, error) {
	if _, err := ByteOffset(text, position); err != nil {
		return "", false, Range{}, false, nil
	}
	parsed, err := parser.ParseWithDriverVariant(text, dv)
	if err != nil {
		return "", false, Range{}, false, err
	}
	return identifierAtParsed(text, position, parsed)
}

func identifierAtParsed(text string, position Position, parsed ast.TokenList) (string, bool, Range, bool, error) {
	offset, err := ByteOffset(text, position)
	if err != nil {
		return "", false, Range{}, false, nil
	}
	lineStart := offset
	for lineStart > 0 && text[lineStart-1] != '\n' && text[lineStart-1] != '\r' {
		lineStart--
	}
	codePointColumn := 0
	for i := lineStart; i < offset; {
		_, size := utf8.DecodeRuneInString(text[i:])
		i += size
		codePointColumn++
	}
	nw := parseutil.NewNodeWalker(parsed, token.Pos{Line: position.Line, Col: codePointColumn + 1})
	identifier, ok := nw.CurNodeBottomMatched(astutil.NodeMatcher{NodeTypes: []ast.NodeType{ast.TypeIdentifier}}).(*ast.Identifier)
	if !ok || identifier == nil || identifier.Tok == nil {
		return "", false, Range{}, false, nil
	}
	span, ok := astSpan(text, identifier)
	start, startErr := ByteOffset(text, span.Start)
	end, endErr := ByteOffset(text, span.End)
	if !ok || startErr != nil || endErr != nil || offset < start || offset >= end {
		return "", false, Range{}, false, nil
	}
	quoted := false
	if word, ok := identifier.Tok.Value.(*token.SQLWord); ok {
		quoted = word.QuoteStyle != 0
	}
	return identifier.NoQuoteString(), quoted, span, true, nil
}

func astSpan(text string, node ast.Node) (Range, bool) {
	r, err := astRange(text, node)
	return r, err == nil
}

// relationAt is kept as the shared relation-only seam for References/Rename.
// It now follows InterBase's contextual role and cache ownership rules instead
// of guessing from whichever table the parser happens to expose.
func relationAt(req Request, position Position) (catalog.ObjectID, Range, bool, error) {
	if req.Context.Variant.Driver == dialect.DatabaseDriverInterBase {
		offset, err := ByteOffset(req.Document.Text, position)
		if err != nil {
			return catalog.ObjectID{}, Range{}, false, nil
		}
		a, err := sqlsymbol.Analyze(req.Document.Text, req.Context.Variant)
		if err != nil {
			return catalog.ObjectID{}, Range{}, false, err
		}
		r := a.Resolve(offset)
		if r.Role != sqlsymbol.Relation || r.SQL == nil {
			return catalog.ObjectID{}, Range{}, false, nil
		}
		id, ok := resolveCatalogRelation(r.SQL.Name, req.Context.SnapshotCache())
		return id, rangeForResolution(req.Document.Text, r), ok, nil
	}
	return legacyRelationAt(req, position)
}
