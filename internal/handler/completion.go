package handler

import (
	"context"
	"encoding/json"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/internal/completer"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
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
	c := completer.NewCompleter(snapshot.Cache)
	c.Driver, c.Variant = snapshot.Variant.Driver, snapshot.Variant.Variant
	completionItems, err := c.Complete(snapshot.Text, params, snapshot.LowercaseKeywords)
	if err != nil {
		return nil, err
	}
	return lsp.CompletionList{Items: completionItems, IsIncomplete: completionMetadataIncomplete(snapshot)}, nil
}

func completionMetadataIncomplete(snapshot editorSnapshot) bool {
	if snapshot.Attaching {
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
