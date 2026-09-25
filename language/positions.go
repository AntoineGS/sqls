package language

import "unicode/utf8"

// ByteOffset converts pos to a byte offset into text. It returns
// ErrPosition when pos has a negative line or column, addresses a line
// text does not have, addresses a column past the UTF-16 length of its
// line, or addresses a column that falls inside a surrogate pair rather
// than on a code point boundary. It never clamps an out-of-range or
// mid-surrogate position into a neighboring token.
func ByteOffset(text string, pos Position) (int, error) {
	if pos.Line < 0 || pos.UTF16Column < 0 {
		return 0, ErrPosition
	}

	line := 0
	lineStart := 0
	for line < pos.Line {
		delim, delimLen := nextLineDelimiter(text, lineStart)
		if delim < 0 {
			return 0, ErrPosition
		}
		lineStart = delim + delimLen
		line++
	}
	lineEnd := len(text)
	if delim, _ := nextLineDelimiter(text, lineStart); delim >= 0 {
		lineEnd = delim
	}

	column := 0
	offset := lineStart
	for offset < lineEnd {
		r, size := utf8.DecodeRuneInString(text[offset:])
		if r == utf8.RuneError && size <= 1 {
			return 0, ErrPosition
		}
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		if column == pos.UTF16Column {
			return offset, nil
		}
		if pos.UTF16Column < column+width {
			// pos.UTF16Column addresses the low half of a surrogate pair.
			return 0, ErrPosition
		}
		column += width
		offset += size
	}
	if column == pos.UTF16Column {
		return offset, nil
	}
	return 0, ErrPosition
}

// PositionAt converts byteOffset into a Position. It returns ErrPosition
// when byteOffset is negative, past len(text), does not fall on a UTF-8
// rune boundary, or falls inside a multi-byte line delimiter (the second
// byte of a CRLF pair).
func PositionAt(text string, byteOffset int) (Position, error) {
	if byteOffset < 0 || byteOffset > len(text) {
		return Position{}, ErrPosition
	}
	if byteOffset < len(text) && !utf8.RuneStart(text[byteOffset]) {
		return Position{}, ErrPosition
	}

	line := 0
	lineStart := 0
	for {
		delim, delimLen := nextLineDelimiter(text, lineStart)
		lineEnd, nextLineStart := len(text), len(text)
		if delim >= 0 {
			lineEnd, nextLineStart = delim, delim+delimLen
		}
		if byteOffset <= lineEnd {
			break
		}
		if byteOffset < nextLineStart {
			// byteOffset lands inside the CRLF pair itself, not at a line
			// boundary.
			return Position{}, ErrPosition
		}
		lineStart = nextLineStart
		line++
	}

	column := 0
	offset := lineStart
	for offset < byteOffset {
		r, size := utf8.DecodeRuneInString(text[offset:])
		if r == utf8.RuneError && size <= 1 {
			return Position{}, ErrPosition
		}
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		column += width
		offset += size
	}
	return Position{Line: line, UTF16Column: column}, nil
}

// nextLineDelimiter returns the byte offset and length of the next line
// delimiter (\n, \r\n or a lone \r) at or after from, or (-1, 0) when text
// has no further delimiter. CRLF is reported as one two-byte delimiter, not
// two line breaks.
func nextLineDelimiter(text string, from int) (int, int) {
	for i := from; i < len(text); i++ {
		switch text[i] {
		case '\n':
			return i, 1
		case '\r':
			if i+1 < len(text) && text[i+1] == '\n' {
				return i, 2
			}
			return i, 1
		}
	}
	return -1, 0
}
