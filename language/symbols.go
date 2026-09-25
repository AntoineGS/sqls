package language

import (
	"context"

	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

// SymbolKindVariable is the LSP SymbolKind value used for every entry
// Symbols returns. LSP's SymbolKind enumeration has no dedicated
// "parameter" kind, so InterBase's input/output procedure parameters and
// local variables are reported alike, distinguishable only by name and
// range.
const SymbolKindVariable = 13

// Symbols returns an outline of req.Document's InterBase procedural
// declarations: one entry per input parameter, output parameter and local
// variable, in declaration order. It is a new capability with no prior LSP
// behavior to match; other dialects, where sqls has no procedural symbol
// analysis, yield an empty (never nil) list. Non-InterBase drivers return
// an empty list because sqlsymbol.Analyze itself only analyzes InterBase
// source.
func (s *Service) Symbols(ctx context.Context, req Request) (Result[[]DocumentSymbol], error) {
	if err := ctx.Err(); err != nil {
		return Result[[]DocumentSymbol]{}, ErrCanceled
	}
	stamp := stampFor(req)
	symbols := make([]DocumentSymbol, 0)
	analysis, err := sqlsymbol.Analyze(req.Document.Text, req.Context.Variant)
	if err != nil {
		return Result[[]DocumentSymbol]{}, err
	}
	for _, symbol := range analysis.Symbols {
		selection, ok := spanRange(req.Document.Text, symbol.Declaration)
		if !ok {
			continue
		}
		scope, ok := spanRange(req.Document.Text, symbol.Scope)
		if !ok {
			scope = selection
		}
		symbols = append(symbols, DocumentSymbol{
			Name:           symbol.Name.Text,
			Kind:           SymbolKindVariable,
			Range:          scope,
			SelectionRange: selection,
		})
	}
	return Result[[]DocumentSymbol]{Value: symbols, Stamp: stamp}, nil
}

// spanRange converts a source span to a Range using PositionAt, so UTF-16
// positions stay correct for supplementary runes and CRLF endings.
func spanRange(text string, span sqlsymbol.Span) (Range, bool) {
	if span.Start < 0 || span.End < span.Start || span.End > len(text) {
		return Range{}, false
	}
	start, err := PositionAt(text, span.Start)
	if err != nil {
		return Range{}, false
	}
	end, err := PositionAt(text, span.End)
	if err != nil {
		return Range{}, false
	}
	return Range{Start: start, End: end}, true
}
