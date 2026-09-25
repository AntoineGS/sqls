package handler

import (
	"context"
	"encoding/json"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
)

func (s *Server) handleTextDocumentRename(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}
	var params lsp.RenameParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}
	snapshot, err := s.captureEditorSnapshot(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	request := languageRequest(snapshot)
	result, err := language.NewService().Rename(ctx, request, languagePosition(params.Position), params.NewName)
	if err != nil {
		return nil, err
	}
	if err := s.navigationResultCurrent(ctx, result.Stamp); err != nil {
		return nil, err
	}
	return lspWorkspaceRename(result.Value), nil
}

// localRename is retained for existing package tests. Its semantic result is
// provided by language.Service; only the legacy helper's handled flag is
// inferred from the service's transport-neutral rename discriminator.
func localRename(text string, params lsp.RenameParams, dv dialect.DriverVariant) (*lsp.WorkspaceEdit, bool, error) {
	request := languageRequest(editorSnapshot{URI: params.TextDocument.URI, Text: text, Variant: dv})
	result, err := language.NewService().Rename(context.Background(), request, languagePosition(params.Position), params.NewName)
	if err != nil {
		return nil, dv.Driver == dialect.DatabaseDriverInterBase, err
	}
	if len(result.Value) == 0 || result.Value[0].Shape != language.RenameChanges {
		return nil, false, nil
	}
	return lspWorkspaceRename(result.Value), true, nil
}

func rename(text string, params lsp.RenameParams) (*lsp.WorkspaceEdit, error) {
	return renameWithDriver(text, params, "")
}

func renameWithDriver(text string, params lsp.RenameParams, driver dialect.DatabaseDriver) (*lsp.WorkspaceEdit, error) {
	return renameWithDriverVariant(text, params, dialect.DriverVariant{Driver: driver})
}

func renameWithDriverVariant(text string, params lsp.RenameParams, dv dialect.DriverVariant) (*lsp.WorkspaceEdit, error) {
	request := languageRequest(editorSnapshot{URI: params.TextDocument.URI, Text: text, Variant: dv})
	result, err := language.NewService().Rename(context.Background(), request, languagePosition(params.Position), params.NewName)
	if err != nil {
		return nil, err
	}
	return lspWorkspaceRename(result.Value), nil
}

func lspWorkspaceRename(edits []language.RenameEdit) *lsp.WorkspaceEdit {
	if len(edits) == 0 {
		return nil
	}
	first := edits[0]
	converted := make([]lsp.TextEdit, 0, len(first.Edits))
	for _, edit := range first.Edits {
		converted = append(converted, lspTextEdit(edit))
	}
	if first.Shape == language.RenameDocumentChanges {
		if len(converted) == 0 {
			return nil
		}
		return &lsp.WorkspaceEdit{DocumentChanges: []lsp.TextDocumentEdit{{
			TextDocument: lsp.OptionalVersionedTextDocumentIdentifier{
				Version:                0,
				TextDocumentIdentifier: lsp.TextDocumentIdentifier{URI: first.URI},
			},
			Edits: converted,
		}}}
	}
	return &lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{first.URI: converted}}
}
