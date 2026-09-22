package sqlsymbol

import "strings"

type ddlToken struct {
	text       string
	quoted     bool
	start, end int
	kind       byte
}

// TableDeclaration returns the CREATE TABLE identifier span for table.
func TableDeclaration(text string, table Name) (Span, bool) {
	tokens := ddlTokens(text)
	for i := 0; i+2 < len(tokens); i++ {
		if !ddlWord(tokens[i], "CREATE") {
			continue
		}
		j := i + 1
		if ddlWord(tokens[j], "GLOBAL") && j+2 < len(tokens) && ddlWord(tokens[j+1], "TEMPORARY") {
			j += 2
		}
		if !ddlWord(tokens[j], "TABLE") || j+1 >= len(tokens) {
			continue
		}
		if ddlNameMatches(tokens[j+1], table) {
			return Span{tokens[j+1].start, tokens[j+1].end}, true
		}
	}
	return Span{}, false
}

// ColumnDeclaration finds a declared column in a CREATE TABLE column list or
// an explicit CREATE VIEW column list. It never infers SELECT expression lineage.
func ColumnDeclaration(text string, table, column Name) (Span, bool) {
	tokens := ddlTokens(text)
	for i := 0; i+2 < len(tokens); i++ {
		if !ddlWord(tokens[i], "CREATE") {
			continue
		}
		j := i + 1
		if ddlWord(tokens[j], "GLOBAL") && j+2 < len(tokens) && ddlWord(tokens[j+1], "TEMPORARY") {
			j += 2
		}
		if !(ddlWord(tokens[j], "TABLE") || ddlWord(tokens[j], "VIEW")) || j+1 >= len(tokens) || !ddlNameMatches(tokens[j+1], table) {
			continue
		}
		k := j + 2
		for k < len(tokens) && tokens[k].kind != '(' && !ddlWord(tokens[k], "AS") {
			k++
		}
		if k >= len(tokens) || tokens[k].kind != '(' {
			continue
		}
		close := ddlMatchingParen(tokens, k)
		if close < 0 {
			continue
		}
		depth := 0
		for entry := k + 1; entry < close; {
			end := entry
			for end < close {
				if tokens[end].kind == '(' {
					depth++
				}
				if tokens[end].kind == ')' {
					depth--
				}
				if tokens[end].kind == ',' && depth == 0 {
					break
				}
				end++
			}
			if entry < end && !ddlConstraint(tokens[entry]) && ddlNameMatches(tokens[entry], column) {
				return Span{tokens[entry].start, tokens[entry].end}, true
			}
			entry = end + 1
		}
	}
	return Span{}, false
}

func ddlConstraint(t ddlToken) bool {
	return ddlWord(t, "CONSTRAINT") || ddlWord(t, "PRIMARY") || ddlWord(t, "FOREIGN") || ddlWord(t, "UNIQUE") || ddlWord(t, "CHECK")
}

func ddlNameMatches(t ddlToken, name Name) bool {
	return t.kind == 'i' && name.MatchesCatalogName(t.text)
}

func ddlWord(t ddlToken, word string) bool {
	return t.kind == 'i' && !t.quoted && strings.EqualFold(t.text, word)
}

func ddlMatchingParen(tokens []ddlToken, open int) int {
	depth := 0
	for i := open; i < len(tokens); i++ {
		if tokens[i].kind == '(' {
			depth++
		}
		if tokens[i].kind == ')' {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// ddlTokens keeps only identifier and punctuation spans. Strings and comments
// are consumed atomically so keywords and names inside them cannot match.
func ddlTokens(text string) []ddlToken {
	out := make([]ddlToken, 0, 64)
	for i := 0; i < len(text); {
		if isDDLWhitespace(text[i]) {
			i++
			continue
		}
		if i+1 < len(text) && text[i] == '-' && text[i+1] == '-' {
			i += 2
			for i < len(text) && text[i] != '\n' {
				i++
			}
			continue
		}
		if i+1 < len(text) && text[i] == '/' && text[i+1] == '*' {
			i += 2
			for i+1 < len(text) && !(text[i] == '*' && text[i+1] == '/') {
				i++
			}
			if i+1 < len(text) {
				i += 2
			}
			continue
		}
		start := i
		if text[i] == '\'' {
			i++
			for i < len(text) {
				if text[i] == '\'' {
					i++
					if i < len(text) && text[i] == '\'' {
						i++
						continue
					}
					break
				}
				i++
			}
			out = append(out, ddlToken{start: start, end: i, kind: 's'})
			continue
		}
		if text[i] == '"' {
			i++
			var b strings.Builder
			for i < len(text) {
				if text[i] == '"' {
					i++
					if i < len(text) && text[i] == '"' {
						b.WriteByte('"')
						i++
						continue
					}
					break
				}
				b.WriteByte(text[i])
				i++
			}
			out = append(out, ddlToken{text: b.String(), quoted: true, start: start, end: i, kind: 'i'})
			continue
		}
		if isDDLIdentStart(text[i]) {
			i++
			for i < len(text) && isDDLIdentPart(text[i]) {
				i++
			}
			out = append(out, ddlToken{text: text[start:i], start: start, end: i, kind: 'i'})
			continue
		}
		kind := text[i]
		i++
		if kind == '(' || kind == ')' || kind == ',' {
			out = append(out, ddlToken{start: start, end: i, kind: kind})
		}
	}
	return out
}
func isDDLWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n' || b == '\f'
}
func isDDLIdentStart(b byte) bool {
	return b == '_' || b == '$' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= 0x80
}
func isDDLIdentPart(b byte) bool { return isDDLIdentStart(b) || b >= '0' && b <= '9' }
