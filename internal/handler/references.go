package handler

import (
	"context"
	"encoding/json"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
)

func (s *Server) handleReferences(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}
	var params lsp.ReferenceParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}
	snapshot, err := s.captureEditorSnapshot(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	request := languageRequest(snapshot)
	result, err := language.NewService().References(ctx, request, languagePosition(params.Position), params.Context.IncludeDeclaration)
	if err != nil {
		return nil, err
	}
	if err := s.navigationResultCurrent(ctx, result.Stamp); err != nil {
		return nil, err
	}
	locations := make([]lsp.Location, 0, len(result.Value))
	for _, location := range result.Value {
		locations = append(locations, lsp.Location{URI: location.URI, Range: lspRange(location.Range)})
	}
	return locations, nil
}

// localReferences remains as a compatibility helper for package tests. The
// result is produced by the same public language service used by the handler.
func localReferences(uri, text string, params lsp.ReferenceParams, dv dialect.DriverVariant) ([]lsp.Location, error) {
	request := languageRequest(editorSnapshot{URI: uri, Text: text, Variant: dv})
	result, err := language.NewService().References(context.Background(), request, languagePosition(params.Position), params.Context.IncludeDeclaration)
	if err != nil {
		return nil, err
	}
	locations := make([]lsp.Location, 0, len(result.Value))
	for _, location := range result.Value {
		locations = append(locations, lsp.Location{URI: location.URI, Range: lspRange(location.Range)})
	}
	return locations, nil
}
