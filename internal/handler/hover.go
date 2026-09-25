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
	res, err := hoverWithDriverVariant(snapshot.Text, params, snapshot.Cache, snapshot.Variant)
	if err != nil && !errors.Is(err, ErrNoHover) {
		return nil, err
	}

	// Not having a repository is not a hover failure: the catalog summary
	// needs none, and a DDL problem must never surface as a JSON-RPC error.
	repo := wrapRepositoryWithLease(snapshot.Repository, snapshot.MetadataLease, snapshot.Variant)
	if augmented := s.interBaseHover(ctx, repo, snapshot.Cache, params, snapshot.Text, res, snapshot.Generation, snapshot.Variant); augmented != nil {
		return augmented, nil
	}
	if err != nil {
		return nil, nil
	}
	return res, nil
}

func hover(text string, params lsp.HoverParams, dbCache *database.DBCache) (*lsp.Hover, error) {
	return hoverWithDriver(text, params, dbCache, "")
}

func hoverWithDriver(text string, params lsp.HoverParams, dbCache *database.DBCache, driver dialect.DatabaseDriver) (*lsp.Hover, error) {
	return hoverWithDriverVariant(text, params, dbCache, dialect.DriverVariant{Driver: driver})
}

// hoverWithDriverVariant is a thin wrapper over language.Service.Hover: it
// builds a minimal metadata snapshot around dbCache, delegates the actual
// cache-only hover algorithm, and converts the result back to lsp types.
// language.ErrNoHover is translated to this package's own ErrNoHover so
// existing callers (interBaseHover's errors.Is check, and this package's
// tests) keep their sentinel identity.
func hoverWithDriverVariant(text string, params lsp.HoverParams, dbCache *database.DBCache, dv dialect.DriverVariant) (*lsp.Hover, error) {
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
	res, err := language.NewService().Hover(context.Background(), req, languagePosition(params.Position))
	if err != nil {
		if errors.Is(err, language.ErrNoHover) {
			return nil, ErrNoHover
		}
		return nil, err
	}
	out := &lsp.Hover{
		Contents: lsp.MarkupContent{Kind: lsp.Markdown, Value: res.Value.Markdown},
	}
	if res.Value.Range != nil {
		out.Range = lspRange(*res.Value.Range)
	}
	return out, nil
}
