package handler

import (
	"context"
	"encoding/json"

	"github.com/sourcegraph/jsonrpc2"
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
func completionIncomplete(snapshot editorSnapshot, metadataIncomplete bool) bool {
	if snapshot.Attaching {
		return true
	}
	if !snapshot.ConnectionReady {
		return false
	}
	if snapshot.Metadata == nil {
		return true
	}
	return metadataIncomplete
}
