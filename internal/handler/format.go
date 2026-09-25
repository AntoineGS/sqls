package handler

import (
	"context"
	"encoding/json"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
)

func (s *Server) handleTextDocumentFormatting(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.DocumentFormattingParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	snapshot, err := s.captureEditorSnapshot(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	options := language.FormatOptions{
		TabSize:           int(params.Options.TabSize),
		InsertSpaces:      params.Options.InsertSpaces,
		UppercaseKeywords: !snapshot.LowercaseKeywords,
	}
	res, err := language.NewService().Format(ctx, languageRequest(snapshot), options)
	if err != nil {
		return nil, err
	}
	if len(res.Value) > 0 {
		textEdits := make([]lsp.TextEdit, len(res.Value))
		for i, edit := range res.Value {
			textEdits[i] = lspTextEdit(edit)
		}
		return textEdits, nil
	}
	return nil, nil
}

func (s *Server) handleTextDocumentRangeFormatting(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.DocumentRangeFormattingParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	if _, err := s.captureEditorSnapshot(params.TextDocument.URI); err != nil {
		return nil, err
	}

	textEdits := []lsp.TextEdit{}
	if len(textEdits) > 0 {
		return textEdits, nil
	}
	return nil, nil
}
