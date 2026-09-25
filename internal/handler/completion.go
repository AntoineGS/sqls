package handler

import (
	"context"
	"encoding/json"

	"github.com/sourcegraph/jsonrpc2"
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
		IsIncomplete: completionMetadataIncomplete(snapshot),
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

func completionMetadataIncomplete(snapshot editorSnapshot) bool {
	if snapshot.Attaching {
		return true
	}
	if snapshot.ConnectionReady && (snapshot.Metadata == nil || (!snapshot.Metadata.Started && !snapshot.Metadata.StartFailed)) {
		return true
	}
	if snapshot.Metadata == nil {
		return false
	}
	// These are the categories that can contribute catalog candidates. A
	// failed/blocked/cancelled category is terminal; it must not keep clients
	// spinning forever when progressive loading has degraded.
	relevant := []database.MetadataKind{
		database.MetadataSchemas, database.MetadataRelations,
		database.MetadataColumnsCurrent, database.MetadataColumnsAll,
		database.MetadataViews, database.MetadataProcedures,
		database.MetadataGenerators, database.MetadataFunctions,
	}
	for _, kind := range relevant {
		state := snapshot.Metadata.Status[kind].State
		if state == database.MetadataPending || state == database.MetadataLoading {
			return true
		}
	}
	return false
}
