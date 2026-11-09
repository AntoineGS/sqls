package sqlsymbol

import (
	"fmt"
	"strings"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/token"
)

// ScriptToken is one significant token in an InterBase script statement.
// Span is a byte range in the original document. Word is the identifier key
// for identifier tokens and is empty for all other tokens.
type ScriptToken struct {
	Span   Span
	Word   string
	Quoted bool
}

// ScriptStatement is one preparable SQL statement from an InterBase script.
// Span covers its first through last significant lexeme, excluding the
// statement terminator.
type ScriptStatement struct {
	Span    Span
	Routine bool
	Tokens  []ScriptToken
}

// ScriptStatements splits an InterBase script into preparable SQL statements
// using isql's SET TERM delimiter semantics. isql-only commands and empty
// statements are omitted. A lexical error rejects the whole script rather
// than returning a partial result.
func ScriptStatements(text string, dv dialect.DriverVariant) ([]ScriptStatement, error) {
	var statements []ScriptStatement
	active := ";"
	offset := 0
	regionStart := 0
	for {
		end, delimiter, err := nextDelimiter(text, offset, active)
		if err != nil {
			return nil, err
		}
		segment := text[offset:end]
		next, isDirective := setTermDelimiter(segment)

		if active == ";" {
			if isDirective {
				statements, err = appendSemicolonRegion(statements, text, regionStart, offset, dv)
				if err != nil {
					return nil, err
				}
				active = next
				if delimiter == "" {
					break
				}
				offset = end + len(delimiter)
				regionStart = offset
				continue
			}

			if delimiter == "" {
				statements, err = appendSemicolonRegion(statements, text, regionStart, len(text), dv)
				if err != nil {
					return nil, err
				}
				break
			}
			offset = end + len(delimiter)
			continue
		}

		if isDirective {
			active = next
		} else {
			items, err := lexScriptRegion(segment, offset, dv)
			if err != nil {
				return nil, err
			}
			items = significantLexemes(items)
			statements = appendScriptStatement(statements, text, items, hasRoutineAS(items))
		}

		if delimiter == "" {
			break
		}
		offset = end + len(delimiter)
		if active == ";" {
			regionStart = offset
		}
	}
	return statements, nil
}

func appendSemicolonRegion(statements []ScriptStatement, text string, start, end int, dv dialect.DriverVariant) ([]ScriptStatement, error) {
	if start >= end {
		return statements, nil
	}
	items, err := lexScriptRegion(text[start:end], start, dv)
	if err != nil {
		return nil, err
	}
	items = significantLexemes(items)
	for i := 0; i < len(items); {
		statementStart := i
		routine := isRoutineStart(items, i)
		sawAS := false
		bodyStarted := false
		depth := 0
		terminated := false
		for i < len(items) {
			item := items[i]
			if item.Token.Kind == token.Semicolon {
				if !(routine && sawAS && !(bodyStarted && depth == 0)) {
					statements = appendScriptStatement(statements, text, items[statementStart:i], routine && sawAS)
					i++
					terminated = true
					break
				}
				i++
				continue
			}
			if routine {
				switch {
				case isWord(item, "AS"):
					sawAS = true
				case isWord(item, "BEGIN"):
					depth++
					bodyStarted = true
				case bodyStarted && isWord(item, "CASE"):
					depth++
				case isWord(item, "END") && depth > 0:
					depth--
				}
			}
			i++
		}
		if !terminated && statementStart < i {
			statements = appendScriptStatement(statements, text, items[statementStart:i], routine && sawAS)
		}
	}
	return statements, nil
}

func lexScriptRegion(text string, offset int, dv dialect.DriverVariant) ([]lexeme, error) {
	items, err := lexRaw(text, dv)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Span.Start += offset
		items[i].Span.End += offset
		if items[i].Token.Kind == token.MultilineComment && !strings.HasSuffix(text[items[i].Span.Start-offset:items[i].Span.End-offset], "*/") {
			return nil, fmt.Errorf("sqlsymbol: unterminated block comment")
		}
	}
	return items, nil
}

func appendScriptStatement(statements []ScriptStatement, text string, items []lexeme, routine bool) []ScriptStatement {
	if len(items) == 0 || isIsqlOnlyStatement(items) {
		return statements
	}
	statement := ScriptStatement{
		Span:    Span{Start: items[0].Span.Start, End: items[len(items)-1].Span.End},
		Routine: routine,
		Tokens:  make([]ScriptToken, 0, len(items)),
	}
	for _, item := range items {
		tok := ScriptToken{Span: item.Span}
		if name, ok := nameFromLexeme(text, item); ok {
			tok.Word = name.Key()
			tok.Quoted = name.Quoted
		}
		statement.Tokens = append(statement.Tokens, tok)
	}
	return append(statements, statement)
}

func isRoutineStart(items []lexeme, start int) bool {
	if start < 0 || start >= len(items) {
		return false
	}
	i := start
	switch {
	case isWord(items[i], "CREATE"):
		i++
		if i+1 < len(items) && isWord(items[i], "OR") && isWord(items[i+1], "ALTER") {
			i += 2
		}
	case isWord(items[i], "ALTER"), isWord(items[i], "RECREATE"):
		i++
	default:
		return false
	}
	return i < len(items) && (isWord(items[i], "PROCEDURE") || isWord(items[i], "TRIGGER"))
}

func hasRoutineAS(items []lexeme) bool {
	if !isRoutineStart(items, 0) {
		return false
	}
	for _, item := range items {
		if isWord(item, "AS") {
			return true
		}
	}
	return false
}

func isIsqlOnlyStatement(items []lexeme) bool {
	if len(items) == 0 {
		return false
	}
	first := items[0]
	if isWord(first, "SET") && len(items) > 1 {
		for _, command := range []string{
			"TERM", "SQL", "NAMES", "AUTODDL", "AUTO", "ECHO", "BAIL", "COUNT", "LIST", "PLAN",
			"PLANONLY", "STATS", "TIME", "BLOBDISPLAY", "HEADING", "WARNINGS", "ROWCOUNT",
		} {
			if isWord(items[1], command) {
				return true
			}
		}
	}
	for _, command := range []string{"CONNECT", "COMMIT", "ROLLBACK", "INPUT", "OUTPUT", "SHOW", "QUIT", "EXIT", "HELP"} {
		if isWord(first, command) {
			return true
		}
	}
	return len(items) > 1 &&
		((isWord(first, "CREATE") && (isWord(items[1], "DATABASE") || isWord(items[1], "SCHEMA"))) ||
			(isWord(first, "DROP") && isWord(items[1], "DATABASE")))
}
