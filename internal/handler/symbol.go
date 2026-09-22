package handler

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

// symbolOffset converts an LSP position (zero-based UTF-16 units) to a byte
// offset in the original source. Positions in the middle of a surrogate pair,
// past a line, or on a missing line are rejected rather than rounded.
func symbolOffset(text string, pos lsp.Position) (int, bool) {
	if pos.Line < 0 || pos.Character < 0 {
		return 0, false
	}

	lineStart := 0
	for line := 0; line < pos.Line; line++ {
		next := strings.IndexByte(text[lineStart:], '\n')
		if next < 0 {
			return 0, false
		}
		lineStart += next + 1
	}
	lineEnd := len(text)
	if next := strings.IndexByte(text[lineStart:], '\n'); next >= 0 {
		lineEnd = lineStart + next
	}
	if lineEnd > lineStart && text[lineEnd-1] == '\r' {
		lineEnd--
	}
	offset, ok := utf16ByteOffset(text[lineStart:lineEnd], pos.Character)
	if !ok {
		return 0, false
	}
	return lineStart + offset, true
}

func utf16ByteOffset(text string, character int) (int, bool) {
	if character < 0 {
		return 0, false
	}
	units := 0
	for offset := 0; offset < len(text); {
		if character == units {
			return offset, true
		}
		r, size := utf8.DecodeRuneInString(text[offset:])
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		if character < units+width {
			return 0, false
		}
		units += width
		offset += size
	}
	if character == units {
		return len(text), true
	}
	return 0, false
}

// symbolRange converts a source span to an LSP range without using parser
// token columns. This keeps UTF-16 positions correct when the source contains
// supplementary runes or CRLF line endings.
func symbolRange(text string, span sqlsymbol.Span) (lsp.Range, bool) {
	if span.Start < 0 || span.End < span.Start || span.End > len(text) {
		return lsp.Range{}, false
	}
	start, ok := symbolPosition(text, span.Start)
	if !ok {
		return lsp.Range{}, false
	}
	end, ok := symbolPosition(text, span.End)
	if !ok {
		return lsp.Range{}, false
	}
	return lsp.Range{Start: start, End: end}, true
}

func symbolPosition(text string, target int) (lsp.Position, bool) {
	if target < 0 || target > len(text) {
		return lsp.Position{}, false
	}
	position := lsp.Position{}
	for offset := 0; offset < len(text); {
		if offset == target {
			return position, true
		}
		if text[offset] == '\r' {
			if offset+1 < len(text) && text[offset+1] == '\n' {
				if target == offset+1 {
					return lsp.Position{}, false
				}
				offset += 2
			} else {
				offset++
			}
			position.Line++
			position.Character = 0
			continue
		}
		if text[offset] == '\n' {
			offset++
			position.Line++
			position.Character = 0
			continue
		}
		r, size := utf8.DecodeRuneInString(text[offset:])
		if target > offset && target < offset+size {
			return lsp.Position{}, false
		}
		if r > 0xFFFF {
			position.Character += 2
		} else {
			position.Character++
		}
		offset += size
	}
	if target == len(text) {
		return position, true
	}
	return lsp.Position{}, false
}

// localDefinition resolves only procedure-local and intentionally ambiguous
// procedural targets. Returning handled=true prevents unknown locals from
// falling through to a same-spelled catalog object.
func localDefinition(uri, text string, pos lsp.Position, dv dialect.DriverVariant) (lsp.Definition, bool, error) {
	if dv.Driver != dialect.DatabaseDriverInterBase {
		return nil, false, nil
	}
	offset, ok := symbolOffset(text, pos)
	if !ok {
		return nil, false, nil
	}
	analysis, err := sqlsymbol.Analyze(text, dv)
	if err != nil {
		return nil, false, err
	}
	resolution := analysis.Resolve(offset)
	if !resolution.InProcedure {
		return nil, false, nil
	}
	switch resolution.Role {
	case sqlsymbol.Local:
		if resolution.Symbol == nil {
			return []lsp.Location{}, true, nil
		}
		rangeValue, ok := symbolRange(text, resolution.Symbol.Declaration)
		if !ok {
			return nil, true, fmt.Errorf("invalid local declaration span")
		}
		return []lsp.Location{{URI: uri, Range: rangeValue}}, true, nil
	case sqlsymbol.Other, sqlsymbol.Ambiguous:
		return []lsp.Location{}, true, nil
	default:
		return nil, false, nil
	}
}

// contextualSQLTarget identifies relation/column roles that must be handed to
// contextual catalog navigation, not to the legacy spelling-based alias scan.
// Task 7 can attach its catalog resolver at this seam.
func contextualSQLTarget(text string, pos lsp.Position, dv dialect.DriverVariant) (bool, error) {
	if dv.Driver != dialect.DatabaseDriverInterBase {
		return false, nil
	}
	offset, ok := symbolOffset(text, pos)
	if !ok {
		return false, nil
	}
	analysis, err := sqlsymbol.Analyze(text, dv)
	if err != nil {
		return false, err
	}
	resolution := analysis.Resolve(offset)
	return resolution.Role == sqlsymbol.Relation || resolution.Role == sqlsymbol.Column, nil
}
