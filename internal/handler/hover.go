package handler

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
)

var ErrNoHover = errors.New("no hover information found")

func (s *Server) handleTextDocumentHover(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.HoverParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	snapshot, err := s.captureEditorSnapshot(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	// Inline hover may read one demanded detail, but must never hold the LSP
	// dispatch path beyond its historical bound. The bounded context applies
	// only to this optional detail wait; cancellation of the original request
	// remains observable below.
	detailCtx, cancel := context.WithTimeout(ctx, hoverDDLTimeout)
	defer cancel()
	languageResult, err := language.NewService().Hover(detailCtx, languageRequest(snapshot), languagePosition(params.Position))
	if ctx.Err() != nil {
		return nil, language.ErrCanceled
	}
	if err != nil && !errors.Is(err, language.ErrNoHover) {
		return nil, err
	}
	if err != nil {
		return nil, nil
	}
	if err := s.navigationResultCurrent(ctx, snapshot, languageResult.Stamp); err != nil {
		return nil, err
	}
	return lspHover(languageResult.Value), nil
}

func lspHover(value language.Hover) *lsp.Hover {
	out := &lsp.Hover{Contents: lsp.MarkupContent{Kind: lsp.Markdown, Value: value.Markdown}}
	if value.Range != nil {
		out.Range = lspRange(*value.Range)
	}
	return out
}

func Hover(text string, params lsp.HoverParams, dbCache *database.DBCache) (*lsp.Hover, error) {
	return HoverWithDriver(text, params, dbCache, "")
}

func HoverWithDriver(text string, params lsp.HoverParams, dbCache *database.DBCache, driver dialect.DatabaseDriver) (*lsp.Hover, error) {
	return HoverWithDriverVariant(context.Background(), text, params, dbCache, dialect.DriverVariant{Driver: driver})
}

// HoverWithDriverVariant is a thin wrapper over language.Service.Hover: it
// builds a minimal metadata snapshot around dbCache, delegates the actual
// cache-only hover algorithm, and converts the result back to lsp types.
// language.ErrNoHover is translated to this package's own ErrNoHover so
// existing callers (interBaseHover's errors.Is check, and this package's
// tests) keep their sentinel identity. ctx comes from the caller's own
// request context (handleTextDocumentHover's ctx in production) rather
// than a fresh context.Background(), so a canceled request actually
// cancels Hover's own ctx.Err() check. Exported, like SignatureHelpWithDriverVariant,
// so a fixture-level test (or an embedding host) can route through the same
// LSP-shape conversion production code uses without a jsonrpc2 request.
func HoverWithDriverVariant(ctx context.Context, text string, params lsp.HoverParams, dbCache *database.DBCache, dv dialect.DriverVariant) (*lsp.Hover, error) {
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
	res, err := language.NewService().Hover(ctx, req, languagePosition(params.Position))
	if err != nil {
		if errors.Is(err, language.ErrNoHover) {
			return nil, ErrNoHover
		}
		return nil, err
	}
	return lspHover(res.Value), nil
}
