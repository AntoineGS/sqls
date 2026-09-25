package language

import (
	"context"

	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/formatter"
	"github.com/sqls-server/sqls/internal/lsp"
)

// Format returns the text edits that reformat req.Document under options,
// rendered for req.Context's driver variant. It carries no per-connection
// keyword-case setting of its own; a host supplies its current
// UppercaseKeywords choice in options for each call.
func (s *Service) Format(ctx context.Context, req Request, options FormatOptions) (Result[[]TextEdit], error) {
	if err := ctx.Err(); err != nil {
		return Result[[]TextEdit]{}, ErrCanceled
	}
	stamp := stampFor(req)
	params := lsp.DocumentFormattingParams{
		Options: lsp.FormattingOptions{
			TabSize:      float64(options.TabSize),
			InsertSpaces: options.InsertSpaces,
		},
	}
	cfg := &config.Config{LowercaseKeywords: !options.UppercaseKeywords}
	edits, err := formatter.FormatWithDriverVariant(req.Document.Text, params, cfg, req.Context.Variant)
	if err != nil {
		return Result[[]TextEdit]{}, err
	}
	result := make([]TextEdit, 0, len(edits))
	for _, edit := range edits {
		result = append(result, TextEdit{
			Range: Range{
				Start: Position{Line: edit.Range.Start.Line, UTF16Column: edit.Range.Start.Character},
				End:   Position{Line: edit.Range.End.Line, UTF16Column: edit.Range.End.Character},
			},
			Text: edit.NewText,
		})
	}
	return Result[[]TextEdit]{Value: result, Stamp: stamp}, nil
}
