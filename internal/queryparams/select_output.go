package queryparams

import "strings"

// ExecutableSelects turns a selected InterBase PSQL SELECT fragment into SQL
// that can be run for its rows. It removes a leading FOR and a trailing
// INTO :output[, ...] [DO] only on standalone SELECT fragments; procedure
// bodies and other SQL are left to the normal execution path. Keep this before
// both discovery and execution so outputs are never prompted as inputs.
func ExecutableSelects(text string) string {
	tokens, ok := selectTokens(text)
	if !ok {
		return text
	}
	type fragment struct {
		start, end int
		tokens     []selectToken
	}
	var fragments []fragment
	start, first := 0, 0
	for i, tok := range tokens {
		if tok.text != ";" {
			continue
		}
		fragments = append(fragments, fragment{start, tok.start, tokens[first:i]})
		start, first = tok.end, i+1
	}
	fragments = append(fragments, fragment{start, len(text), tokens[first:]})

	// A PSQL definition contains internal semicolons. Never interpret a SELECT
	// inside one of its fragments as an independently executable statement.
	for _, f := range fragments {
		if len(f.tokens) == 0 {
			continue
		}
		switch strings.ToUpper(f.tokens[0].text) {
		case "SELECT", "FOR", "WITH", "INSERT", "UPDATE", "DELETE":
		case "EXECUTE":
			if len(f.tokens) < 2 || !f.tokens[1].is("PROCEDURE") {
				return text
			}
		default:
			return text
		}
	}

	var out strings.Builder
	last := 0
	for _, f := range fragments {
		toks := f.tokens
		if len(toks) == 0 {
			continue
		}
		selectIndex := 0
		if toks[0].is("FOR") {
			selectIndex = 1
		}
		if selectIndex >= len(toks) || !toks[selectIndex].is("SELECT") {
			continue
		}

		into := -1
		depth := 0
		for i := selectIndex + 1; i < len(toks); i++ {
			switch toks[i].text {
			case "(":
				depth++
			case ")":
				depth--
			}
			if depth == 0 && toks[i].is("INTO") {
				into = i
			}
		}
		if into >= 0 && !outputVariableSuffix(toks[into+1:]) {
			continue
		}
		if selectIndex == 0 && into < 0 {
			continue
		}
		// A FOR loop with a body is not a standalone SELECT. Only remove FOR
		// on its own when there is no DO body in the selected fragment.
		if selectIndex == 1 && into < 0 {
			for _, tok := range toks[1:] {
				if tok.is("DO") {
					into = -2
					break
				}
			}
			if into == -2 {
				continue
			}
		}
		out.WriteString(text[last:f.start])
		if selectIndex == 1 {
			out.WriteString(text[f.start:toks[0].start])
			last = toks[1].start
		} else {
			last = f.start
		}
		if into >= 0 {
			out.WriteString(strings.TrimRight(text[last:toks[into].start], " \t\r\n"))
			last = f.end
		}
	}
	out.WriteString(text[last:])
	return out.String()
}

type selectToken struct {
	text       string
	start, end int
	quoted     bool
}

func (t selectToken) is(word string) bool {
	return !t.quoted && strings.EqualFold(t.text, word)
}

func outputVariableSuffix(tokens []selectToken) bool {
	if len(tokens) == 0 {
		return false
	}
	for i := 0; i < len(tokens); {
		if tokens[i].quoted || len(tokens[i].text) < 2 || tokens[i].text[0] != ':' || !isNameStartByte(tokens[i].text[1]) {
			return false
		}
		i++
		if i == len(tokens) {
			return true
		}
		if tokens[i].text != "," {
			return tokens[i].is("DO") && i == len(tokens)-1
		}
		i++
	}
	return false
}

// selectTokens records positions of ordinary words and punctuation while
// ignoring whitespace and comments. Strings and delimited identifiers remain
// opaque tokens; their contents cannot act as FOR, INTO, DO, or semicolons.
func selectTokens(text string) ([]selectToken, bool) {
	var tokens []selectToken
	for i := 0; i < len(text); {
		start := i
		switch {
		case text[i] == '-' && i+1 < len(text) && text[i+1] == '-':
			i += 2
			for i < len(text) && text[i] != '\n' {
				i++
			}
		case text[i] == '/' && i+1 < len(text) && text[i+1] == '*':
			end := strings.Index(text[i+2:], "*/")
			if end < 0 {
				return nil, false
			}
			i += end + 4
		case text[i] == '\'' || text[i] == '"':
			quote := text[i]
			i++
			closed := false
			for i < len(text) {
				if text[i] == quote {
					i++
					if i < len(text) && text[i] == quote {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, false
			}
			tokens = append(tokens, selectToken{text: text[start:i], start: start, end: i, quoted: true})
		case isNameStartByte(text[i]) || text[i] >= '0' && text[i] <= '9':
			i++
			for i < len(text) && isNamePartByte(text[i]) {
				i++
			}
			tokens = append(tokens, selectToken{text: text[start:i], start: start, end: i})
		case text[i] == ':' && i+1 < len(text) && isNameStartByte(text[i+1]):
			i += 2
			for i < len(text) && isNamePartByte(text[i]) {
				i++
			}
			tokens = append(tokens, selectToken{text: text[start:i], start: start, end: i})
		case text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r':
			i++
		default:
			i++
			tokens = append(tokens, selectToken{text: text[start:i], start: start, end: i})
		}
	}
	return tokens, true
}
