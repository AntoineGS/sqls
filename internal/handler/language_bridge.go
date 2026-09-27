package handler

import (
	"context"

	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
)

// languageRequest builds the language.Request a language.Service call needs
// from one editorSnapshot: the document identity/text and the metadata
// context to interpret it against. There is no per-connection cache here —
// every call derives its Context fresh from the snapshot it is given.
func languageRequest(snapshot editorSnapshot) language.Request {
	lctx := language.Context{
		Key:               snapshot.MetadataKey,
		Snapshot:          snapshot.Metadata,
		Variant:           snapshot.Variant,
		DialectResolved:   snapshot.DialectResolved,
		LowercaseKeywords: snapshot.LowercaseKeywords,
		Incarnation:       snapshot.MetadataIncarnation,
	}
	if snapshot.MetadataLease != nil {
		lctx.Details = snapshot.MetadataLease
	}
	return language.Request{
		Document: language.Document{
			URI:     snapshot.URI,
			Text:    snapshot.Text,
			Version: int64(snapshot.Version),
			Binding: uint64(snapshot.Generation),
		},
		Context: lctx,
		Policy:  snapshot.DiagnosticPolicy,
	}
}

func languagePosition(p lsp.Position) language.Position {
	return language.Position{Line: p.Line, UTF16Column: p.Character}
}

func lspPosition(p language.Position) lsp.Position {
	return lsp.Position{Line: p.Line, Character: p.UTF16Column}
}

func lspRange(r language.Range) lsp.Range {
	return lsp.Range{Start: lspPosition(r.Start), End: lspPosition(r.End)}
}

func lspTextEdit(e language.TextEdit) lsp.TextEdit {
	return lsp.TextEdit{Range: lspRange(e.Range), NewText: e.Text}
}

// navigationResultCurrent fences a computed navigation result against the
// latest editor/metadata snapshot immediately before publishing it.
func (s *Server) navigationResultCurrent(ctx context.Context, captured editorSnapshot, stamp language.Stamp) error {
	if err := ctx.Err(); err != nil {
		return language.ErrCanceled
	}
	snapshot, err := s.captureEditorSnapshot(captured.URI)
	if err != nil {
		return language.ErrStale
	}
	if snapshot.Revision != captured.Revision || snapshot.Text != captured.Text ||
		snapshot.Variant != captured.Variant || snapshot.DialectResolved != captured.DialectResolved {
		return language.ErrStale
	}
	current := languageRequest(snapshot)
	if !language.Current(stamp, current.Document, current.Context) {
		return language.ErrStale
	}
	return nil
}
