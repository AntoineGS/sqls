package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/ast/astutil"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
	"github.com/sqls-server/sqls/parser"
	"github.com/sqls-server/sqls/parser/parseutil"
	"github.com/sqls-server/sqls/token"
)

func (s *Server) handleTextDocumentRename(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.RenameParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	snapshot, err := s.captureEditorSnapshot(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	text := snapshot.Text

	res, handled, err := localRename(text, params, snapshot.Variant)
	if err != nil {
		return nil, err
	}
	if handled {
		return res, nil
	}
	res, err = renameWithDriverVariant(text, params, snapshot.Variant)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// localRename handles procedure-local InterBase symbols. The boolean is false
// only when the cursor is outside a recognized procedure and the legacy
// spelling-based rename may safely try the request.
func localRename(text string, params lsp.RenameParams, dv dialect.DriverVariant) (*lsp.WorkspaceEdit, bool, error) {
	if dv.Driver != dialect.DatabaseDriverInterBase {
		return nil, false, nil
	}
	offset, ok := symbolOffset(text, params.Position)
	if !ok {
		return &lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{params.TextDocument.URI: {}}}, true, nil
	}
	analysis, err := sqlsymbol.Analyze(text, dv)
	if err != nil {
		return nil, true, err
	}
	resolution := analysis.Resolve(offset)
	if !resolution.InProcedure {
		return nil, false, nil
	}

	switch resolution.Role {
	case sqlsymbol.Local:
		if resolution.Symbol == nil {
			return nil, true, fmt.Errorf("cannot rename procedure local: unresolved symbol")
		}
		edits, err := analysis.Rename(resolution.Symbol, params.NewName)
		if err != nil {
			return nil, true, err
		}
		return workspaceRename(params.TextDocument.URI, text, edits)
	case sqlsymbol.Ambiguous:
		return nil, true, fmt.Errorf("cannot rename procedure target: ambiguous symbol")
	case sqlsymbol.Relation, sqlsymbol.Column:
		return nil, true, fmt.Errorf("cannot rename SQL target: database column or relation")
	case sqlsymbol.Callable:
		return nil, true, fmt.Errorf("cannot rename procedure target: other procedure or callable")
	case sqlsymbol.Other:
		return &lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{params.TextDocument.URI: {}}}, true, nil
	default:
		return nil, true, fmt.Errorf("cannot rename procedure target: unsupported symbol")
	}
}

func workspaceRename(uri, text string, edits []sqlsymbol.Edit) (*lsp.WorkspaceEdit, bool, error) {
	result := make([]lsp.TextEdit, 0, len(edits))
	for _, edit := range edits {
		rangeValue, ok := symbolRange(text, edit.Span)
		if !ok {
			return nil, true, fmt.Errorf("invalid local rename span")
		}
		result = append(result, lsp.TextEdit{Range: rangeValue, NewText: edit.NewText})
	}
	return &lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{uri: result}}, true, nil
}

func rename(text string, params lsp.RenameParams) (*lsp.WorkspaceEdit, error) {
	return renameWithDriver(text, params, "")
}

func renameWithDriver(text string, params lsp.RenameParams, driver dialect.DatabaseDriver) (*lsp.WorkspaceEdit, error) {
	return renameWithDriverVariant(text, params, dialect.DriverVariant{Driver: driver})
}

func renameWithDriverVariant(text string, params lsp.RenameParams, dv dialect.DriverVariant) (*lsp.WorkspaceEdit, error) {
	parsed, err := parser.ParseWithDriverVariant(text, dv)
	if err != nil {
		return nil, err
	}

	pos := token.Pos{
		Line: params.Position.Line,
		Col:  params.Position.Character,
	}

	// Get the identifier on focus
	nodeWalker := parseutil.NewNodeWalker(parsed, pos)
	m := astutil.NodeMatcher{
		NodeTypes: []ast.NodeType{ast.TypeIdentifier},
	}
	currentVariable := nodeWalker.CurNodeBottomMatched(m)
	if currentVariable == nil {
		return nil, nil
	}

	// Get all identifiers in the statement
	idents, err := parseutil.ExtractIdenfiers(parsed, pos)
	if err != nil {
		return nil, err
	}

	// Extract only those with matching names
	renameTarget := []ast.Node{}
	for _, ident := range idents {
		if ident.String() == currentVariable.String() {
			renameTarget = append(renameTarget, ident)
		}
	}
	if len(renameTarget) == 0 {
		return nil, nil
	}

	edits := make([]lsp.TextEdit, len(renameTarget))
	for i, target := range renameTarget {
		edit := lsp.TextEdit{
			Range: lsp.Range{
				Start: lsp.Position{
					Line:      target.Pos().Line,
					Character: target.Pos().Col,
				},
				End: lsp.Position{
					Line:      target.End().Line,
					Character: target.End().Col,
				},
			},
			NewText: params.NewName,
		}
		edits[i] = edit
	}

	res := &lsp.WorkspaceEdit{
		DocumentChanges: []lsp.TextDocumentEdit{
			{
				TextDocument: lsp.OptionalVersionedTextDocumentIdentifier{
					Version: 0,
					TextDocumentIdentifier: lsp.TextDocumentIdentifier{
						URI: params.TextDocument.URI,
					},
				},
				Edits: edits,
			},
		},
	}

	return res, nil
}
