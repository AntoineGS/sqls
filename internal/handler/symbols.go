package handler

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
)

func (s *Server) handleTextDocumentDocumentSymbol(ctx context.Context, _ *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}
	var params lsp.DocumentSymbolParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}
	if params.TextDocument.URI == "" {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	snapshot, err := s.captureEditorSnapshot(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	result, err := language.NewService().Symbols(ctx, languageRequest(snapshot))
	if err != nil {
		if errors.Is(err, language.ErrCanceled) && ctx.Err() != nil {
			return nil, language.ErrCanceled
		}
		return []lsp.DocumentSymbol{}, nil
	}
	if err := s.navigationResultCurrent(ctx, snapshot, result.Stamp); err != nil {
		if errors.Is(err, language.ErrCanceled) && ctx.Err() != nil {
			return nil, language.ErrCanceled
		}
		return []lsp.DocumentSymbol{}, nil
	}
	symbols := make([]lsp.DocumentSymbol, 0, len(result.Value))
	for _, symbol := range result.Value {
		symbols = append(symbols, lsp.DocumentSymbol{
			Name:           symbol.Name,
			Kind:           int(symbol.Kind),
			Range:          lspRange(symbol.Range),
			SelectionRange: lspRange(symbol.SelectionRange),
		})
	}
	return symbols, nil
}
