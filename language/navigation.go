package language

import (
	"context"
	"fmt"
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

// Definition resolves a relation name at position. Database-resident source is
// returned as a value; the host remains responsible for presenting it as a URI.
func (s *Service) Definition(ctx context.Context, req Request, position Position) (Result[[]Location], error) {
	if err := ctx.Err(); err != nil {
		return Result[[]Location]{}, ErrCanceled
	}
	if local, handled, err := localDefinition(req, position); err != nil {
		return Result[[]Location]{}, err
	} else if handled {
		return Result[[]Location]{Value: local, Stamp: stampFor(req)}, nil
	}
	role := sqlsymbol.Outside
	if req.Context.Variant.Driver == dialect.DatabaseDriverInterBase {
		_, resolution, _, err := localAnalysis(req, position)
		if err != nil {
			return Result[[]Location]{}, err
		}
		role = resolution.Role
	}
	if req.Context.Variant.Driver != dialect.DatabaseDriverInterBase || role == sqlsymbol.Alias {
		if alias, ok, err := aliasDefinition(req, position); err != nil {
			return Result[[]Location]{}, err
		} else if ok {
			return Result[[]Location]{Value: []Location{alias}, Stamp: stampFor(req)}, nil
		}
		if req.Context.Variant.Driver == dialect.DatabaseDriverInterBase && role == sqlsymbol.Alias {
			return Result[[]Location]{Value: []Location{}, Stamp: stampFor(req)}, nil
		}
	}
	target, _, ok, err := navigationTargetAt(req, position)
	if err != nil {
		return Result[[]Location]{}, err
	}
	if !ok {
		return Result[[]Location]{Value: []Location{}, Stamp: stampFor(req)}, nil
	}
	detail, available, err := s.loadNavigationDetail(ctx, req, target, position)
	if err != nil {
		return Result[[]Location]{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result[[]Location]{}, ErrCanceled
	}
	if !available || detail.source == nil || !detail.hasRange {
		return Result[[]Location]{Value: []Location{}, Stamp: stampFor(req)}, nil
	}
	return Result[[]Location]{Value: []Location{{Range: detail.range_, Source: detail.source}}, Stamp: stampFor(req)}, nil
}

// References returns local relation-name references in the document.
func (s *Service) References(ctx context.Context, req Request, position Position, includeDeclaration bool) (Result[[]Location], error) {
	if err := ctx.Err(); err != nil {
		return Result[[]Location]{}, ErrCanceled
	}
	if locations, handled, err := localReferences(req, position, includeDeclaration); err != nil {
		return Result[[]Location]{}, err
	} else if handled {
		return Result[[]Location]{Value: locations, Stamp: stampFor(req)}, nil
	}
	_, r, ok, err := relationAt(req, position)
	if err != nil {
		return Result[[]Location]{}, err
	}
	locations := []Location{}
	if ok {
		parsed, err := parser.ParseWithDriverVariant(req.Document.Text, req.Context.Variant)
		if err != nil {
			return Result[[]Location]{}, err
		}
		pos := token.Pos{Line: position.Line, Col: position.UTF16Column + 1}
		tables, err := parseutil.ExtractTable(parsed, pos)
		if err != nil {
			return Result[[]Location]{}, err
		}
		for _, table := range tables {
			if table.Name == "" {
				continue
			}
			locations = append(locations, Location{URI: req.Document.URI, Range: r})
		}
	}
	if err := ctx.Err(); err != nil {
		return Result[[]Location]{}, ErrCanceled
	}
	return Result[[]Location]{Value: locations, Stamp: stampFor(req)}, nil
}

// Rename creates a local relation-name edit. Database-wide rename remains an
// execution/workbench operation and is intentionally not performed here.
func (s *Service) Rename(ctx context.Context, req Request, position Position, newName string) (Result[[]RenameEdit], error) {
	if edits, handled, err := localRename(req, position, newName); err != nil {
		return Result[[]RenameEdit]{}, err
	} else if handled {
		return Result[[]RenameEdit]{Value: edits, Stamp: stampFor(req)}, nil
	}
	refs, err := s.References(ctx, req, position, true)
	if err != nil {
		return Result[[]RenameEdit]{}, err
	}
	if len(refs.Value) == 0 {
		return Result[[]RenameEdit]{Value: []RenameEdit{}, Stamp: refs.Stamp}, nil
	}
	edits := make([]TextEdit, 0, len(refs.Value))
	for _, location := range refs.Value {
		edits = append(edits, TextEdit{Range: location.Range, Text: newName})
	}
	return Result[[]RenameEdit]{Value: []RenameEdit{{URI: req.Document.URI, Edits: edits}}, Stamp: refs.Stamp}, nil
}

func localAnalysis(req Request, position Position) (*sqlsymbol.Analysis, sqlsymbol.Resolution, bool, error) {
	if req.Context.Variant.Driver != dialect.DatabaseDriverInterBase {
		return nil, sqlsymbol.Resolution{}, false, nil
	}
	offset, err := ByteOffset(req.Document.Text, position)
	if err != nil {
		return nil, sqlsymbol.Resolution{}, true, nil
	}
	a, err := sqlsymbol.Analyze(req.Document.Text, req.Context.Variant)
	if err != nil {
		return nil, sqlsymbol.Resolution{}, true, err
	}
	r := a.Resolve(offset)
	handled := false
	if r.InProcedure {
		switch r.Role {
		case sqlsymbol.Local, sqlsymbol.Other, sqlsymbol.Ambiguous:
			handled = true
		}
	}
	return a, r, handled, nil
}

func localDefinition(req Request, position Position) ([]Location, bool, error) {
	_, r, handled, err := localAnalysis(req, position)
	if err != nil || !handled {
		return nil, handled, err
	}
	if r.Role != sqlsymbol.Local || r.Symbol == nil {
		return []Location{}, true, nil
	}
	rng, ok := navigationSpanRange(req.Document.Text, r.Symbol.Declaration)
	if !ok {
		return nil, true, fmt.Errorf("invalid local declaration span")
	}
	return []Location{{URI: req.Document.URI, Range: rng}}, true, nil
}

func localReferences(req Request, position Position, includeDeclaration bool) ([]Location, bool, error) {
	a, r, handled, err := localAnalysis(req, position)
	if err != nil || !handled {
		return nil, handled, err
	}
	if r.Role != sqlsymbol.Local || r.Symbol == nil {
		return []Location{}, true, nil
	}
	spans := a.References(r.Symbol, includeDeclaration)
	locations := make([]Location, 0, len(spans))
	for _, span := range spans {
		rng, ok := navigationSpanRange(req.Document.Text, span)
		if !ok {
			return nil, true, fmt.Errorf("invalid local reference span")
		}
		locations = append(locations, Location{URI: req.Document.URI, Range: rng})
	}
	return locations, true, nil
}

func localRename(req Request, position Position, newName string) ([]RenameEdit, bool, error) {
	a, r, handled, err := localAnalysis(req, position)
	if err != nil || !handled {
		return nil, handled, err
	}
	if r.Role != sqlsymbol.Local || r.Symbol == nil {
		return []RenameEdit{}, true, nil
	}
	changes, err := a.Rename(r.Symbol, newName)
	if err != nil {
		return nil, true, err
	}
	edits := make([]TextEdit, 0, len(changes))
	for _, change := range changes {
		rng, ok := navigationSpanRange(req.Document.Text, change.Span)
		if !ok {
			return nil, true, fmt.Errorf("invalid local rename span")
		}
		edits = append(edits, TextEdit{Range: rng, Text: change.NewText})
	}
	return []RenameEdit{{URI: req.Document.URI, Edits: edits}}, true, nil
}

func navigationSpanRange(text string, span sqlsymbol.Span) (Range, bool) {
	if span.Start < 0 || span.End < span.Start || span.End > len(text) {
		return Range{}, false
	}
	start, err := PositionAt(text, span.Start)
	if err != nil {
		return Range{}, false
	}
	end, err := PositionAt(text, span.End)
	if err != nil {
		return Range{}, false
	}
	return Range{Start: start, End: end}, true
}

func aliasDefinition(req Request, position Position) (Location, bool, error) {
	offset, err := ByteOffset(req.Document.Text, position)
	if err != nil {
		return Location{}, false, nil
	}
	var resolution sqlsymbol.Resolution
	if req.Context.Variant.Driver == dialect.DatabaseDriverInterBase {
		analysis, err := sqlsymbol.Analyze(req.Document.Text, req.Context.Variant)
		if err != nil {
			return Location{}, false, err
		}
		resolution = analysis.Resolve(offset)
		if resolution.Role != sqlsymbol.Alias {
			return Location{}, false, nil
		}
		return interBaseAliasDefinition(req, resolution)
	}
	parsed, err := parser.ParseWithDriverVariant(req.Document.Text, req.Context.Variant)
	if err != nil {
		return Location{}, false, err
	}
	lineStart := offset
	for lineStart > 0 && req.Document.Text[lineStart-1] != '\n' && req.Document.Text[lineStart-1] != '\r' {
		lineStart--
	}
	codePointCol := 0
	for i := lineStart; i < offset; {
		_, size := utf8.DecodeRuneInString(req.Document.Text[i:])
		i += size
		codePointCol++
	}
	nw := parseutil.NewNodeWalker(parsed, token.Pos{Line: position.Line, Col: codePointCol + 1})
	current := nw.CurNodeBottomMatched(astutil.NodeMatcher{NodeTypes: []ast.NodeType{ast.TypeIdentifier}})
	if current == nil {
		return Location{}, false, nil
	}
	var statement ast.Node
	findContainingStatement(parsed, req.Document.Text, offset, &statement)
	if statement == nil {
		return Location{}, false, nil
	}
	statementRange, err := astRange(req.Document.Text, statement)
	if err != nil {
		return Location{}, false, err
	}
	candidates := make([]*ast.Aliased, 0, 1)
	for _, candidate := range parseutil.ExtractAliased(parsed) {
		aliased, ok := candidate.(*ast.Aliased)
		if !ok || aliased.AliasedName == nil || aliased.AliasedName.String() != current.String() {
			continue
		}
		aliasRange, err := astRange(req.Document.Text, aliased.AliasedName)
		if err != nil {
			return Location{}, false, err
		}
		if !rangeWithin(aliasRange, statementRange) {
			continue
		}
		candidates = append(candidates, aliased)
	}
	// Duplicate declarations that cannot be uniquely associated with this
	// statement's SQL ownership scopes are intentionally not guessed.
	if len(candidates) == 1 {
		aliased := candidates[0]
		rng, err := astRange(req.Document.Text, aliased.AliasedName)
		if err != nil {
			return Location{}, false, err
		}
		return Location{URI: req.Document.URI, Range: rng}, true, nil
	}
	return Location{}, false, nil
}

func interBaseAliasDefinition(req Request, resolution sqlsymbol.Resolution) (Location, bool, error) {
	if resolution.SQL == nil {
		return Location{}, false, nil
	}
	for _, scope := range resolution.SQL.Scopes { // SQL scopes are nearest-first.
		matches := make([]sqlsymbol.RelationRef, 0, 1)
		for _, relation := range scope {
			if relation.Alias != nil && relation.Alias.Key() == resolution.SQL.Name.Key() {
				matches = append(matches, relation)
			}
		}
		if len(matches) == 0 {
			continue
		}
		// The nearest matching scope owns this name. Ambiguous or malformed
		// bindings do not fall through to an outer same-spelled declaration.
		if len(matches) != 1 {
			return Location{}, false, nil
		}
		span := matches[0].AliasDeclaration
		if span.Start < 0 || span.End <= span.Start || span.End > len(req.Document.Text) {
			return Location{}, false, nil
		}
		rng, ok := navigationSpanRange(req.Document.Text, span)
		if !ok {
			return Location{}, false, nil
		}
		return Location{URI: req.Document.URI, Range: rng}, true, nil
	}
	return Location{}, false, nil
}

func findContainingStatement(root ast.Node, text string, offset int, best *ast.Node) {
	if statement, ok := root.(*ast.Statement); ok {
		rng, err := astRange(text, statement)
		if err == nil {
			start, startErr := ByteOffset(text, rng.Start)
			end, endErr := ByteOffset(text, rng.End)
			if startErr == nil && endErr == nil && start <= offset && offset < end {
				if *best == nil {
					*best = statement
				} else if prior, ok := (*best).(*ast.Statement); ok {
					priorRange, rangeErr := astRange(text, prior)
					if rangeErr == nil {
						priorStart, _ := ByteOffset(text, priorRange.Start)
						priorEnd, _ := ByteOffset(text, priorRange.End)
						if end-start < priorEnd-priorStart {
							*best = statement
						}
					}
				}
			}
		}
	}
	if list, ok := root.(ast.TokenList); ok {
		for _, child := range list.GetTokens() {
			findContainingStatement(child, text, offset, best)
		}
	}
}

func rangeWithin(child, parent Range) bool {
	return positionLE(parent.Start, child.Start) && positionLE(child.End, parent.End)
}

func positionLE(left, right Position) bool {
	return left.Line < right.Line || left.Line == right.Line && left.UTF16Column <= right.UTF16Column
}

func astRange(text string, node ast.Node) (Range, error) {
	start, err := codePointColumnToUTF16(text, node.Pos().Line, node.Pos().Col)
	if err != nil {
		return Range{}, err
	}
	end, err := codePointColumnToUTF16(text, node.End().Line, node.End().Col)
	if err != nil {
		return Range{}, err
	}
	return Range{Start: Position{Line: node.Pos().Line, UTF16Column: start}, End: Position{Line: node.End().Line, UTF16Column: end}}, nil
}

func legacyRelationAt(req Request, position Position) (catalog.ObjectID, Range, bool, error) {
	parsed, err := parser.ParseWithDriverVariant(req.Document.Text, req.Context.Variant)
	if err != nil {
		return catalog.ObjectID{}, Range{}, false, err
	}
	pos := token.Pos{Line: position.Line, Col: position.UTF16Column + 1}
	nw := parseutil.NewNodeWalker(parsed, pos)
	m := astutil.NodeMatcher{NodeTypes: []ast.NodeType{ast.TypeMemberIdentifier, ast.TypeIdentifier}}
	nodes := nw.CurNodeMatches(m)
	var name string
	var schema string
	var node ast.Node
	for _, candidate := range nodes {
		switch n := candidate.(type) {
		case *ast.MemberIdentifier:
			name = n.ChildTok.NoQuoteString()
			schema = n.ParentTok.NoQuoteString()
			node = n
		case *ast.Identifier:
			if name == "" {
				name = n.NoQuoteString()
				node = n
			}
		}
	}
	if name == "" || node == nil {
		return catalog.ObjectID{}, Range{}, false, nil
	}
	tables, err := parseutil.ExtractTable(parsed, pos)
	if err != nil {
		return catalog.ObjectID{}, Range{}, false, err
	}
	for _, table := range tables {
		if table.Name != name && (req.Context.Variant.Driver == dialect.DatabaseDriverInterBase || table.Alias != name) {
			continue
		}
		if schema == "" {
			schema = table.DatabaseSchema
		}
		if table.Alias == name {
			name = table.Name
		}
		r, err := astRange(req.Document.Text, node)
		if err != nil {
			return catalog.ObjectID{}, Range{}, false, err
		}
		return catalog.ObjectID{Schema: schema, Kind: catalog.ObjectKindTable, Name: name}, r, true, nil
	}
	return catalog.ObjectID{}, Range{}, false, nil
}
