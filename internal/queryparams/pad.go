package queryparams

import "strings"

// PadNamedMarkers replaces genuine named markers with a question mark and
// enough spaces to preserve every byte offset in the statement. It only pads
// statement forms supported by Compile and returns the original text when no
// marker can safely be padded.
func PadNamedMarkers(statement string) (padded string, ok bool) {
	if !supportedStatementStart(strings.TrimSpace(statement)) {
		return statement, false
	}

	var out strings.Builder
	out.Grow(len(statement))
	state := stateOrdinary
	found := false
	for i, n := 0, len(statement); i < n; {
		c := statement[i]
		switch state {
		case stateOrdinary:
			switch {
			case c == '\'':
				out.WriteByte(c)
				i++
				state = stateSingleQuoted
			case c == '"':
				out.WriteByte(c)
				i++
				state = stateDoubleQuoted
			case c == '-' && i+1 < n && statement[i+1] == '-':
				out.WriteString("--")
				i += 2
				state = stateLineComment
			case c == '/' && i+1 < n && statement[i+1] == '*':
				out.WriteString("/*")
				i += 2
				state = stateBlockComment
			case c == ':' && i+1 < n && statement[i+1] == ':':
				out.WriteString("::")
				i += 2
			case c == ':' && i+1 < n && statement[i+1] == '=':
				out.WriteString(":=")
				i += 2
			case c == ':' && i+1 < n && isNameStartByte(statement[i+1]):
				j := i + 1
				for j < n && isNamePartByte(statement[j]) {
					j++
				}
				out.WriteByte('?')
				out.WriteString(strings.Repeat(" ", j-i-1))
				i = j
				found = true
			default:
				out.WriteByte(c)
				i++
			}
		case stateSingleQuoted:
			out.WriteByte(c)
			i++
			if c == '\'' {
				if i < n && statement[i] == '\'' {
					out.WriteByte('\'')
					i++
				} else {
					state = stateOrdinary
				}
			}
		case stateDoubleQuoted:
			out.WriteByte(c)
			i++
			if c == '"' {
				if i < n && statement[i] == '"' {
					out.WriteByte('"')
					i++
				} else {
					state = stateOrdinary
				}
			}
		case stateLineComment:
			out.WriteByte(c)
			i++
			if c == '\n' {
				state = stateOrdinary
			}
		case stateBlockComment:
			if c == '*' && i+1 < n && statement[i+1] == '/' {
				out.WriteString("*/")
				i += 2
				state = stateOrdinary
			} else {
				out.WriteByte(c)
				i++
			}
		}
	}

	if state == stateSingleQuoted || state == stateDoubleQuoted || state == stateBlockComment || !found {
		return statement, false
	}
	return out.String(), true
}
