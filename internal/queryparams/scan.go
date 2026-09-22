// Package queryparams compiles InterBase SQL batches that use named
// placeholders (":NAME") into ordinary "?" positional SQL plus the positional
// argument keys needed to bind values. It is a byte-offset scanner, not a
// general SQL parser: it only needs to tell quoted/commented text apart from
// ordinary SQL well enough to find genuine placeholders and statement
// boundaries.
package queryparams

import (
	"fmt"
	"strings"
)

// Parameter describes one unique named placeholder in a Batch.
type Parameter struct {
	Name string `json:"name"` // first spelling, without colon
	Key  string `json:"key"`  // ASCII uppercase
}

// Statement is one SQL statement from a Batch, with named placeholders
// rewritten to "?" and the canonical key for each "?" occurrence recorded in
// order.
type Statement struct {
	SQL  string   // original text, only genuine :name occurrences rewritten
	Keys []string // canonical key per ? occurrence
}

// Batch is the result of compiling a selection of SQL text: the unique
// parameters in first-appearance order, and the individual statements found
// in the selection.
type Batch struct {
	Parameters []Parameter // unique, first-appearance order across statements
	Statements []Statement
}

// scanState is the scanner's current lexical context. Only stateOrdinary
// recognizes parameter names and statement separators.
type scanState int

const (
	stateOrdinary scanState = iota
	stateSingleQuoted
	stateDoubleQuoted
	stateLineComment
	stateBlockComment
)

// Compile scans text (one or more semicolon-separated SQL statements) for
// InterBase named placeholders (":NAME") and rewrites each statement to use
// "?" positional markers, following the SQL dialect's (1 or 3) lexical rules
// for quoted text and comments. It validates the entire selection before
// returning: a statement outside SELECT/INSERT/UPDATE/DELETE/EXECUTE
// PROCEDURE (WITH-prefixed SELECT included) that contains a named marker is
// rejected, and so is any bare "?" positional marker found outside a literal
// or comment.
func Compile(text string, sqlDialect int) (Batch, error) {
	type fragment struct {
		sql  string
		keys []string
	}

	var batch Batch
	seen := make(map[string]bool)
	var fragments []fragment

	var out strings.Builder
	var keys []string
	finishFragment := func() {
		fragments = append(fragments, fragment{sql: out.String(), keys: keys})
		out.Reset()
		keys = nil
	}

	state := stateOrdinary
	i, n := 0, len(text)
	for i < n {
		c := text[i]
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
			case c == '-' && i+1 < n && text[i+1] == '-':
				out.WriteString("--")
				i += 2
				state = stateLineComment
			case c == '/' && i+1 < n && text[i+1] == '*':
				out.WriteString("/*")
				i += 2
				state = stateBlockComment
			case c == ';':
				i++
				finishFragment()
			case c == '?':
				return Batch{}, fmt.Errorf("queryparams: unsupported positional parameter marker at byte %d; use named parameters", i)
			case c == ':' && i+1 < n && text[i+1] == ':':
				out.WriteString("::")
				i += 2
			case c == ':' && i+1 < n && text[i+1] == '=':
				out.WriteString(":=")
				i += 2
			case c == ':' && i+1 < n && isNameStartByte(text[i+1]):
				j := i + 1
				for j < n && isNamePartByte(text[j]) {
					j++
				}
				name := text[i+1 : j]
				key := strings.ToUpper(name)
				out.WriteByte('?')
				keys = append(keys, key)
				if !seen[key] {
					seen[key] = true
					batch.Parameters = append(batch.Parameters, Parameter{Name: name, Key: key})
				}
				i = j
			default:
				out.WriteByte(c)
				i++
			}
		case stateSingleQuoted:
			out.WriteByte(c)
			i++
			if c == '\'' {
				if i < n && text[i] == '\'' {
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
				if i < n && text[i] == '"' {
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
			if c == '*' && i+1 < n && text[i+1] == '/' {
				out.WriteString("*/")
				i += 2
				state = stateOrdinary
			} else {
				out.WriteByte(c)
				i++
			}
		}
	}

	switch state {
	case stateSingleQuoted:
		return Batch{}, fmt.Errorf("queryparams: unterminated string literal")
	case stateDoubleQuoted:
		return Batch{}, fmt.Errorf("queryparams: unterminated quoted identifier")
	case stateBlockComment:
		return Batch{}, fmt.Errorf("queryparams: unterminated block comment")
	}
	finishFragment()

	// Once any fragment in the batch carries a named marker, the contract
	// requires validating the entire selection before anything runs: a
	// marker-free CREATE PROCEDURE/EXECUTE BLOCK header or trailing END is
	// still part of the same PSQL batch as the marker-bearing fragment
	// between them, and must reject the batch just as surely as if the
	// marker were in that fragment itself. A batch with no marker anywhere
	// skips this check entirely and lets ordinary SQL (including DDL) pass
	// through unchanged.
	hasMarker := false
	for _, f := range fragments {
		if len(f.keys) > 0 {
			hasMarker = true
			break
		}
	}

	for _, f := range fragments {
		if TrimLeadingTrivia(f.sql) == "" {
			continue // whitespace/comment-only fragment: nothing to execute
		}
		sql := strings.Trim(f.sql, " \t\r\n")
		if hasMarker && !supportedStatementStart(sql) {
			return Batch{}, fmt.Errorf("queryparams: named parameters are not supported in this statement form")
		}
		batch.Statements = append(batch.Statements, Statement{SQL: sql, Keys: f.keys})
	}

	return batch, nil
}

// supportedStatementStart reports whether sql begins with a statement keyword
// that may carry named parameters: SELECT, INSERT, UPDATE, DELETE, a
// WITH-prefixed SELECT, or EXECUTE PROCEDURE. Other statement forms —
// including CREATE PROCEDURE and EXECUTE BLOCK PSQL bodies, where a leading
// colon marks a local variable reference rather than a user input — are
// rejected whenever they carry a named marker.
func supportedStatementStart(sql string) bool {
	first, rest := leadingWord(TrimLeadingTrivia(sql))
	switch strings.ToUpper(first) {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "WITH":
		return true
	case "EXECUTE":
		rest = strings.TrimLeft(rest, " \t\r\n")
		second, _ := leadingWord(rest)
		return strings.ToUpper(second) == "PROCEDURE"
	default:
		return false
	}
}

// leadingWord splits the maximal leading run of ASCII letters off s, which is
// enough to read a SQL keyword without needing a full tokenizer.
func leadingWord(s string) (word, rest string) {
	i := 0
	for i < len(s) {
		b := s[i]
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') {
			i++
			continue
		}
		break
	}
	return s[:i], s[i:]
}

// isNameStartByte matches the approved placeholder grammar's name-start
// class: ":" followed by [A-Za-z_][A-Za-z0-9_$]*.
func isNameStartByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '_'
}

func isNamePartByte(b byte) bool {
	return isNameStartByte(b) || (b >= '0' && b <= '9') || b == '$'
}

// TrimLeadingTrivia repeatedly strips leading whitespace and complete leading
// comments (line or block) from text, returning "" for input that is only
// trivia. It never touches trivia inside a statement, and it leaves an
// unclosed block comment untouched rather than guessing where it ends.
func TrimLeadingTrivia(text string) string {
	for {
		trimmed := strings.TrimLeft(text, " \t\r\n")
		text = trimmed
		switch {
		case strings.HasPrefix(text, "--"):
			if idx := strings.IndexByte(text, '\n'); idx >= 0 {
				text = text[idx+1:]
			} else {
				text = ""
			}
		case strings.HasPrefix(text, "/*"):
			idx := strings.Index(text[2:], "*/")
			if idx < 0 {
				return text // incomplete comment: not trivia, leave it
			}
			text = text[2+idx+2:]
		default:
			return text
		}
	}
}
