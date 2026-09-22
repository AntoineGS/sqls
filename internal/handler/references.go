package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

func (s *Server) handleReferences(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.ReferenceParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}
	text, ok := s.fileText(params.TextDocument.URI)
	if !ok {
		return nil, fmt.Errorf("document not found: %s", params.TextDocument.URI)
	}
	return localReferences(params.TextDocument.URI, text, params, s.parserDriverVariant())
}

func localReferences(uri, text string, params lsp.ReferenceParams, dv dialect.DriverVariant) ([]lsp.Location, error) {
	result := []lsp.Location{}
	if dv.Driver != dialect.DatabaseDriverInterBase {
		return result, nil
	}
	offset, ok := symbolOffset(text, params.Position)
	if !ok {
		return result, nil
	}
	analysis, err := sqlsymbol.Analyze(text, dv)
	if err != nil {
		return nil, err
	}
	resolution := analysis.Resolve(offset)
	if resolution.Role != sqlsymbol.Local || resolution.Symbol == nil {
		return result, nil
	}
	for _, span := range analysis.References(resolution.Symbol, params.Context.IncludeDeclaration) {
		rangeValue, ok := symbolRange(text, span)
		if !ok {
			return nil, fmt.Errorf("invalid local reference span")
		}
		result = append(result, lsp.Location{URI: uri, Range: rangeValue})
	}
	return result, nil
}
