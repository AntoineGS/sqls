package sqlsymbol

import (
	"strconv"
	"strings"

	"github.com/sqls-server/sqls/token"
)

// stringTypeWidth returns the declared maximum character width for bounded
// character types. Unknown, unbounded, and malformed types return ok=false.
func stringTypeWidth(typeName string) (int, bool) {
	s := strings.TrimSpace(typeName)
	word, rest := takeTypeWord(s)
	word = strings.ToUpper(word)
	switch word {
	case "CHAR", "CHARACTER", "VARCHAR":
		if word == "CHARACTER" {
			next, remaining := takeTypeWord(rest)
			if strings.EqualFold(next, "VARYING") {
				rest = remaining
			}
		}
	case "":
		return 0, false
	default:
		return 0, false
	}

	rest = strings.TrimSpace(rest)
	if len(rest) == 0 || rest[0] != '(' {
		return 0, false
	}
	close := strings.IndexByte(rest, ')')
	if close < 0 {
		return 0, false
	}
	widthText := strings.TrimSpace(rest[1:close])
	if widthText == "" {
		return 0, false
	}
	for _, r := range widthText {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	width, err := strconv.Atoi(widthText)
	if err != nil || width <= 0 {
		return 0, false
	}

	// Character set and collation clauses do not change a character-count
	// declaration's maximum. Do not accept unrelated suffixes as part of a
	// bounded string type.
	suffix := strings.TrimSpace(rest[close+1:])
	if suffix != "" {
		modifier, remaining := takeTypeWord(suffix)
		if !strings.EqualFold(modifier, "CHARACTER") {
			return 0, false
		}
		modifier, remaining = takeTypeWord(remaining)
		if !strings.EqualFold(modifier, "SET") {
			return 0, false
		}
		charset, remaining := takeTypeWord(remaining)
		if charset == "" || strings.TrimSpace(remaining) != "" {
			return 0, false
		}
	}
	return width, true
}

func takeTypeWord(s string) (string, string) {
	s = strings.TrimLeft(s, " \t\r\n")
	end := 0
	for end < len(s) {
		b := s[end]
		if !isTypeWordByte(b) {
			break
		}
		end++
	}
	return s[:end], s[end:]
}

func isTypeWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}

// expressionWidth infers a proven maximum character width for a complete
// expression. Unsupported or ambiguous expressions return ok=false.
func (a *Analysis) expressionWidth(items []lexeme, c Catalog) (int, bool) {
	items = significantLexemes(items)
	if len(items) == 0 {
		return 0, false
	}
	return a.expressionWidthRange(items, c)
}

func (a *Analysis) expressionWidthRange(items []lexeme, c Catalog) (int, bool) {
	items = trimExpressionParens(items)
	if len(items) == 0 || !balancedExpression(items) {
		return 0, false
	}

	if width, ok := a.concatenationWidth(items, c); ok {
		return width, true
	} else if hasTopLevelConcatenation(a.Text, items) {
		return 0, false
	}

	if len(items) == 1 {
		item := items[0]
		if item.Token.Kind == token.SingleQuotedString || item.Token.Kind == token.NationalStringLiteral {
			return stringLiteralWidth(a.Text, item)
		}
	}

	if width, ok, recognized := a.callWidth(items, c); recognized {
		return width, ok
	}

	if width, ok := a.identifierWidth(items, c); ok {
		return width, true
	}
	return 0, false
}

func trimExpressionParens(items []lexeme) []lexeme {
	for len(items) >= 2 && items[0].Token.Kind == token.LParen && items[len(items)-1].Token.Kind == token.RParen {
		depth := 0
		wraps := true
		for i, item := range items {
			switch item.Token.Kind {
			case token.LParen:
				depth++
			case token.RParen:
				depth--
				if depth == 0 && i != len(items)-1 {
					wraps = false
				}
				if depth < 0 {
					return nil
				}
			}
		}
		if !wraps || depth != 0 {
			break
		}
		items = items[1 : len(items)-1]
	}
	return items
}

func balancedExpression(items []lexeme) bool {
	depth := 0
	for _, item := range items {
		switch item.Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

func hasTopLevelConcatenation(text string, items []lexeme) bool {
	depth := 0
	for i := 0; i+1 < len(items); i++ {
		item := items[i]
		if item.Token.Kind == token.LParen {
			depth++
		} else if item.Token.Kind == token.RParen {
			depth--
		}
		if depth != 0 || item.Token.Kind != token.Char || items[i+1].Token.Kind != token.Char {
			continue
		}
		if item.Span.End == items[i+1].Span.Start && text[item.Span.Start:items[i+1].Span.End] == "||" {
			return true
		}
	}
	return false
}

func (a *Analysis) concatenationWidth(items []lexeme, c Catalog) (int, bool) {
	depth := 0
	start := 0
	total := 0
	found := false
	for i := 0; i+1 < len(items); i++ {
		item := items[i]
		if item.Token.Kind == token.LParen {
			depth++
		} else if item.Token.Kind == token.RParen {
			depth--
		}
		if depth != 0 || item.Token.Kind != token.Char || items[i+1].Token.Kind != token.Char {
			continue
		}
		if item.Span.End != items[i+1].Span.Start || a.Text[item.Span.Start:items[i+1].Span.End] != "||" {
			continue
		}
		partWidth, ok := a.expressionWidthRange(items[start:i], c)
		if !ok {
			return 0, false
		}
		total, ok = addWidthBounds(total, partWidth)
		if !ok {
			return 0, false
		}
		found = true
		start = i + 2
		i++
	}
	if !found {
		return 0, false
	}
	partWidth, ok := a.expressionWidthRange(items[start:], c)
	if !ok {
		return 0, false
	}
	return addWidthBounds(total, partWidth)
}

func addWidthBounds(left, right int) (int, bool) {
	maxInt := int(^uint(0) >> 1)
	if left < 0 || right < 0 || right > maxInt-left {
		return 0, false
	}
	return left + right, true
}

func stringLiteralWidth(text string, item lexeme) (int, bool) {
	raw := []rune(text[item.Span.Start:item.Span.End])
	quoteAt := 0
	if item.Token.Kind == token.NationalStringLiteral {
		if len(raw) < 3 || (raw[0] != 'N' && raw[0] != 'n') || raw[1] != '\'' {
			return 0, false
		}
		quoteAt = 1
	}
	if len(raw) < quoteAt+2 || raw[quoteAt] != '\'' || raw[len(raw)-1] != '\'' {
		return 0, false
	}
	width := 0
	for i := quoteAt + 1; i < len(raw)-1; i++ {
		if raw[i] == '\'' {
			if i+1 >= len(raw)-1 || raw[i+1] != '\'' {
				return 0, false
			}
			i++
		}
		width++
	}
	return width, true
}

func (a *Analysis) callWidth(items []lexeme, c Catalog) (int, bool, bool) {
	if len(items) < 3 || items[0].Token.Kind != token.SQLKeyword || items[1].Token.Kind != token.LParen {
		return 0, false, false
	}
	close, ok := matchingExpressionParen(items, 1)
	if !ok || close != len(items)-1 {
		return 0, false, false
	}
	arguments := items[2:close]
	switch {
	case isWord(items[0], "CAST"):
		as := topLevelWordIndex(arguments, "AS")
		if as <= 0 || as >= len(arguments)-1 || topLevelWordIndex(arguments[as+1:], "AS") >= 0 {
			return 0, false, true
		}
		typeStart, typeEnd := arguments[as+1].Span.Start, arguments[len(arguments)-1].Span.End
		width, known := stringTypeWidth(a.Text[typeStart:typeEnd])
		return width, known, true
	case isWord(items[0], "SUBSTRING"):
		from := topLevelWordIndex(arguments, "FROM")
		if from <= 0 || from >= len(arguments)-2 {
			return 0, false, true
		}
		forRelative := topLevelWordIndex(arguments[from+1:], "FOR")
		if forRelative <= 0 {
			return 0, false, true
		}
		forIndex := from + 1 + forRelative
		if forIndex >= len(arguments)-1 {
			return 0, false, true
		}
		length, known := nonnegativeInteger(arguments[forIndex+1:])
		if !known {
			return 0, false, true
		}
		sourceWidth, ok := a.expressionWidthRange(arguments[:from], c)
		if !ok {
			return 0, false, true
		}
		if sourceWidth < length {
			length = sourceWidth
		}
		return length, true, true
	case isWord(items[0], "TRIM"):
		if topLevelWordIndex(arguments, "FROM") >= 0 {
			return 0, false, true
		}
		width, known := a.expressionWidthRange(arguments, c)
		return width, known, true
	default:
		return 0, false, true
	}
}

func matchingExpressionParen(items []lexeme, open int) (int, bool) {
	if open < 0 || open >= len(items) || items[open].Token.Kind != token.LParen {
		return 0, false
	}
	depth := 0
	for i := open; i < len(items); i++ {
		switch items[i].Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
			if depth == 0 {
				return i, true
			}
			if depth < 0 {
				return 0, false
			}
		}
	}
	return 0, false
}

func topLevelWordIndex(items []lexeme, word string) int {
	depth := 0
	for i, item := range items {
		switch item.Token.Kind {
		case token.LParen:
			depth++
		case token.RParen:
			depth--
		}
		if depth == 0 && isWord(item, word) {
			return i
		}
	}
	return -1
}

func nonnegativeInteger(items []lexeme) (int, bool) {
	if len(items) != 1 || items[0].Token.Kind != token.Number {
		return 0, false
	}
	raw, ok := items[0].Token.Value.(string)
	if !ok {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}

func (a *Analysis) identifierWidth(items []lexeme, c Catalog) (int, bool) {
	var item lexeme
	switch len(items) {
	case 1:
		item = items[0]
	case 2:
		if items[0].Token.Kind != token.Colon {
			return 0, false
		}
		item = items[1]
	case 3:
		if items[1].Token.Kind != token.Period {
			return 0, false
		}
		item = items[2]
	default:
		return 0, false
	}
	if _, ok := nameFromLexeme(a.Text, item); !ok || !isNameToken(item) {
		return 0, false
	}
	resolution := a.Resolve(item.Span.Start)
	if resolution.Role == Local && resolution.Symbol != nil {
		return stringTypeWidth(resolution.Symbol.Type)
	}
	if resolution.Role == Column && resolution.SQL != nil {
		return sqlColumnWidth(resolution.SQL, c)
	}
	return 0, false
}

func sqlColumnWidth(reference *SQLReference, c Catalog) (int, bool) {
	if reference == nil || c == nil {
		return 0, false
	}
	for _, scope := range reference.Scopes {
		matches := 0
		columnType := ""
		allOwnersKnown := true
		eligibleOwners := 0
		for _, relation := range scope {
			if reference.Qualifier != nil && !relationMatchesQualifier(relation, *reference.Qualifier) {
				continue
			}
			eligibleOwners++
			if relation.Name.Key() == "" {
				allOwnersKnown = false
				continue
			}
			columns, ok := c.Columns(relation.Name)
			if !ok {
				allOwnersKnown = false
				continue
			}
			for _, column := range columns {
				if !reference.Name.MatchesCatalogName(column.Name) {
					continue
				}
				matches++
				columnType = column.Type
			}
		}
		if !allOwnersKnown {
			return 0, false
		}
		if reference.Qualifier != nil && eligibleOwners == 0 {
			continue
		}
		if matches > 1 {
			return 0, false
		}
		if matches == 0 && reference.Qualifier != nil {
			return 0, false
		}
		if matches == 1 {
			return stringTypeWidth(columnType)
		}
	}
	return 0, false
}

func relationMatchesQualifier(relation RelationRef, qualifier Name) bool {
	if relation.Alias != nil {
		return relation.Alias.Key() == qualifier.Key()
	}
	return relation.Name.Key() == qualifier.Key()
}
