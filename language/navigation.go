package language

import (
	"context"
	"errors"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/ast/astutil"
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
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
	target, r, ok, err := relationAt(req, position)
	if err != nil {
		return Result[[]Location]{}, err
	}
	if !ok {
		return Result[[]Location]{Value: []Location{}, Stamp: stampFor(req)}, nil
	}
	if req.Context.Details == nil {
		return Result[[]Location]{Value: []Location{}, Stamp: stampFor(req)}, nil
	}
	detail, err := req.Context.Details.Detail(ctx, metadata.Request{Object: target, Facet: "ddl", Dialect: req.Context.Variant.Variant.InterBaseSQLDialect(), Consistency: metadata.Committed})
	if err != nil {
		if errors.Is(err, metadata.ErrUnsupported) {
			return Result[[]Location]{Value: []Location{}, Stamp: stampFor(req)}, nil
		}
		if ctx.Err() != nil {
			return Result[[]Location]{}, ErrCanceled
		}
		return Result[[]Location]{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result[[]Location]{}, ErrCanceled
	}
	source := &Source{Object: detail.Object, Text: detail.Text, Provenance: detail.Provenance}
	return Result[[]Location]{Value: []Location{{Range: r, Source: source}}, Stamp: stampFor(req)}, nil
}

// References returns local relation-name references in the document.
func (s *Service) References(ctx context.Context, req Request, position Position, includeDeclaration bool) (Result[[]Location], error) {
	if err := ctx.Err(); err != nil {
		return Result[[]Location]{}, ErrCanceled
	}
	_, _, ok, err := relationAt(req, position)
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
			if includeDeclaration || table.Name != "" {
				end := position
				end.UTF16Column++
				locations = append(locations, Location{URI: req.Document.URI, Range: Range{Start: position, End: end}})
			}
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

func relationAt(req Request, position Position) (catalog.ObjectID, Range, bool, error) {
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
		if table.Name != name && table.Alias != name {
			continue
		}
		if schema == "" {
			schema = table.DatabaseSchema
		}
		if table.Alias == name {
			name = table.Name
		}
		return catalog.ObjectID{Schema: schema, Kind: catalog.ObjectKindTable, Name: name}, Range{
			Start: Position{Line: node.Pos().Line, UTF16Column: node.Pos().Col},
			End:   Position{Line: node.End().Line, UTF16Column: node.End().Col},
		}, true, nil
	}
	return catalog.ObjectID{}, Range{}, false, nil
}
