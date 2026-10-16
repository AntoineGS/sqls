package handler

import (
	"context"
	"encoding/json"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
)

func (s *Server) handleTextDocumentSignatureHelp(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.SignatureHelpParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	snapshot, err := s.captureEditorSnapshot(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	res, err := s.languageService.SignatureHelp(ctx, languageRequest(snapshot), languagePosition(params.Position))
	if err != nil {
		return nil, err
	}
	return lspSignatureHelp(res.Value), nil
}

func SignatureHelp(text string, params lsp.SignatureHelpParams, dbCache *database.DBCache) (*lsp.SignatureHelp, error) {
	return SignatureHelpWithDriver(text, params, dbCache, "")
}

func SignatureHelpWithDriver(text string, params lsp.SignatureHelpParams, dbCache *database.DBCache, driver dialect.DatabaseDriver) (*lsp.SignatureHelp, error) {
	return SignatureHelpWithDriverVariant(context.Background(), text, params, dbCache, dialect.DriverVariant{Driver: driver})
}

// SignatureHelpWithDriverVariant is a thin wrapper over
// language.Service.SignatureHelp: it builds a minimal metadata snapshot
// around dbCache, delegates the cache-only signature-help algorithm, and
// converts the result back to lsp types (int -> float64 for
// ActiveSignature/ActiveParameter). ctx comes from the caller's own
// request context (handleTextDocumentSignatureHelp's ctx in production)
// rather than a fresh context.Background(), so a canceled request
// actually cancels SignatureHelp's own ctx.Err() check.
func SignatureHelpWithDriverVariant(ctx context.Context, text string, params lsp.SignatureHelpParams, dbCache *database.DBCache, dv dialect.DriverVariant) (*lsp.SignatureHelp, error) {
	if dbCache == nil {
		return nil, nil
	}
	req := language.Request{
		Document: language.Document{Text: text},
		Context: language.Context{
			Snapshot: &catalog.MetadataSnapshot{Cache: dbCache},
			Variant:  dv,
		},
	}
	res, err := language.NewService().SignatureHelp(ctx, req, languagePosition(params.Position))
	if err != nil {
		return nil, err
	}
	return lspSignatureHelp(res.Value), nil
}

func lspSignatureHelp(value language.SignatureHelp) *lsp.SignatureHelp {
	if value.Signatures == nil {
		return nil
	}
	signatures := make([]lsp.SignatureInformation, len(value.Signatures))
	for i, sig := range value.Signatures {
		params := make([]lsp.ParameterInformation, len(sig.Parameters))
		for j, p := range sig.Parameters {
			params[j] = lsp.ParameterInformation{Label: p.Label, Documentation: p.Documentation}
		}
		signatures[i] = lsp.SignatureInformation{
			Label:         sig.Label,
			Documentation: sig.Documentation,
			Parameters:    params,
		}
	}
	return &lsp.SignatureHelp{
		Signatures:      signatures,
		ActiveSignature: float64(value.ActiveSignature),
		ActiveParameter: float64(value.ActiveParameter),
	}
}
