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

func (s *Server) handleTextDocumentCompletion(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.CompletionParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	snapshot, err := s.captureEditorSnapshot(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	res, err := language.NewService().Complete(ctx, languageRequest(snapshot), languagePosition(params.Position))
	if err != nil {
		return nil, err
	}
	return lsp.CompletionList{
		Items:        lspCompletionItems(res.Value.Items),
		IsIncomplete: completionIncomplete(snapshot, res.Value.Incomplete),
	}, nil
}

// CompletionWithDriverVariant is a thin wrapper over language.Service.Complete,
// in the same family as HoverWithDriverVariant and
// SignatureHelpWithDriverVariant: it builds a minimal metadata snapshot
// around dbCache, delegates the cache-only completion algorithm, and
// converts the result back to lsp types through the same lspCompletionItems
// conversion production code uses. It has no host connection-lifecycle
// facts to compose (see completionIncomplete's doc comment), so
// IsIncomplete here is exactly language.CompletionIncomplete's verdict for
// the snapshot it built, not the composed rule handleTextDocumentCompletion
// applies.
func CompletionWithDriverVariant(ctx context.Context, text string, params lsp.CompletionParams, dbCache *database.DBCache, dv dialect.DriverVariant) (*lsp.CompletionList, error) {
	if dbCache == nil {
		return nil, nil
	}
	snap := &catalog.MetadataSnapshot{Cache: dbCache}
	req := language.Request{
		Document: language.Document{Text: text},
		Context: language.Context{
			Snapshot: snap,
			Variant:  dv,
		},
	}
	res, err := language.NewService().Complete(ctx, req, languagePosition(params.Position))
	if err != nil {
		return nil, err
	}
	return &lsp.CompletionList{
		Items:        lspCompletionItems(res.Value.Items),
		IsIncomplete: language.CompletionIncomplete(snap) || res.Value.Incomplete,
	}, nil
}

func lspCompletionItems(items []language.CompletionItem) []lsp.CompletionItem {
	out := make([]lsp.CompletionItem, len(items))
	for i, item := range items {
		out[i] = lspCompletionItem(item)
	}
	return out
}

func lspCompletionItem(item language.CompletionItem) lsp.CompletionItem {
	out := lsp.CompletionItem{
		Label:      item.Label,
		Kind:       lsp.CompletionItemKind(item.Kind),
		Detail:     item.Detail,
		SortText:   item.SortText,
		FilterText: item.FilterText,
		InsertText: item.InsertText,
	}
	if item.Documentation != "" {
		out.Documentation = &lsp.MarkupContent{Kind: lsp.Markdown, Value: item.Documentation}
	}
	if item.Snippet {
		out.InsertTextFormat = lsp.SnippetTextFormat
	}
	if item.Edit != nil {
		edit := lspTextEdit(*item.Edit)
		out.TextEdit = &edit
	}
	if item.AdditionalEdits != nil {
		additional := make([]lsp.TextEdit, len(item.AdditionalEdits))
		for i, e := range item.AdditionalEdits {
			additional[i] = lspTextEdit(e)
		}
		out.AdditionalTextEdits = additional
	}
	return out
}

// completionIncomplete composes the host's own connection-lifecycle facts
// (Attaching, ConnectionReady — concepts a catalog.MetadataSnapshot has no
// representation for) with metadataIncomplete, the metadata-readiness half
// of the rule. metadataIncomplete must be language.CompletionIncomplete's
// verdict for snapshot.Metadata — res.Value.Incomplete from the same
// Service.Complete call already is that verdict, so callers pass it
// through rather than recomputing it here. This is the single place the
// two halves combine; there is exactly one metadata-readiness rule,
// encoded once in language.CompletionIncomplete.
//
// ConnectionReady only decides what an ABSENT Metadata means: waiting for a
// lease to bind on a ready connection (true — a bind is imminent) versus no
// connection to bind one from at all (false). Once a Metadata snapshot
// exists, ConnectionReady is not consulted again: a lease bound under a
// still-ready connection generation stays live — and can still report a
// pending/loading category — through a later attachIntent(nil) that drops
// the CURRENT connection attempt back to idle without bumping connGeneration
// (see handler.go's attachIntent early-return). Gating on ConnectionReady
// here would settle completion to false while that lease is still loading,
// which drops the client's retry and permanently loses whatever candidates
// the lease was about to produce.
func completionIncomplete(snapshot editorSnapshot, metadataIncomplete bool) bool {
	if snapshot.Attaching {
		return true
	}
	if snapshot.Metadata == nil {
		return snapshot.ConnectionReady
	}
	return metadataIncomplete
}
