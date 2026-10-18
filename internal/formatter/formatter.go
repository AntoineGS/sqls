package formatter

import (
	"errors"
	"slices"
	"strings"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/ast/astutil"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/token"
)

func Format(text string, params lsp.DocumentFormattingParams, cfg *config.Config) ([]lsp.TextEdit, error) {
	return formatWithDialect(text, params, cfg, &dialect.GenericSQLDialect{})
}

// FormatWithDriver formats text using a driver's default variant. It is
// retained with its exact signature; see FormatWithDriverVariant.
func FormatWithDriver(text string, params lsp.DocumentFormattingParams, cfg *config.Config, driver dialect.DatabaseDriver) ([]lsp.TextEdit, error) {
	return FormatWithDriverVariant(text, params, cfg, dialect.DriverVariant{Driver: driver})
}

func FormatWithDriverVariant(text string, params lsp.DocumentFormattingParams, cfg *config.Config, dv dialect.DriverVariant) ([]lsp.TextEdit, error) {
	return formatWithDialect(text, params, cfg, dialect.DialectForDriverVariant(dv))
}

// formatWithDialect returns ErrUnsafeFormat, and no edit, when formatting
// panics or would change anything but whitespace and keyword case.
func formatWithDialect(text string, params lsp.DocumentFormattingParams, cfg *config.Config, d dialect.Dialect) ([]lsp.TextEdit, error) {
	if text == "" {
		return nil, errors.New("empty")
	}
	return recoverFormat(func() ([]lsp.TextEdit, error) {
		// The edit spans the text between leading and trailing whitespace.
		// Offsets come from the text, not from the lexer, which does not
		// count line breaks inside string literals.
		from := len(text) - len(strings.TrimLeft(text, " \t\r\n"))
		to := len(strings.TrimRight(text, " \t\r\n"))
		if from >= to {
			return nil, errors.New("empty")
		}
		st, en := offsetToUTF16(text, from), offsetToUTF16(text, to)
		newText, err := formatSource(text[from:to], params, cfg, d)
		if err != nil {
			return nil, err
		}
		if err := verifyFormatted(text[from:to], newText, d); err != nil {
			return nil, err
		}
		res := []lsp.TextEdit{
			{
				Range: lsp.Range{
					Start: st,
					End:   en,
				},
				NewText: newText,
			},
		}
		return res, nil
	})
}

// offsetToUTF16 converts a byte offset in text into the UTF-16 code-unit
// position required by LSP.
func offsetToUTF16(text string, offset int) lsp.Position {
	line, lineStart := 0, 0
	for {
		delim, delimLen := nextLineBreak(text, lineStart)
		if delim < 0 || delim >= offset {
			break
		}
		line++
		lineStart = delim + delimLen
	}
	column := 0
	for _, r := range text[lineStart:offset] {
		column++
		if r > 0xFFFF {
			column++
		}
	}
	return lsp.Position{Line: line, Character: column}
}

// nextLineBreak matches the lexer's supported line endings: LF, CRLF and a
// lone CR. The delimiter is excluded from AST line columns.
func nextLineBreak(text string, from int) (int, int) {
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

type formatEnvironment struct {
	reader      *astutil.NodeReader
	indentLevel int
	options     lsp.FormattingOptions
	lastKeyword string // Track last processed keyword
	// inline counts the enclosing function arguments and IN lists, which
	// are laid out on one line.
	inline int
	// blocks holds the open BEGIN (true) and CASE (false) of the statement.
	blocks []bool
	// betweenPending is set from BETWEEN to the AND of its range.
	betweenPending bool
}

func (e *formatEnvironment) indentLevelReset() {
	e.indentLevel = 0
}

func (e *formatEnvironment) indentLevelUp() {
	e.indentLevel++
}

func (e *formatEnvironment) indentLevelDown() {
	e.indentLevel--
	if e.indentLevel < 0 {
		e.indentLevel = 0
	}
}

func (e *formatEnvironment) genIndent() []ast.Node {
	indent := whiteSpaceNodes(int(e.options.TabSize))
	if !e.options.InsertSpaces {
		indent = []ast.Node{tabNode}
	}
	nodes := []ast.Node{}
	for i := 0; i < e.indentLevel; i++ {
		nodes = append(nodes, indent...)
	}
	return nodes
}

func Eval(node ast.Node, env *formatEnvironment) ast.Node {
	switch node := node.(type) {
	// case *ast.Query:
	// 	return formatQuery(node, env)
	// case *ast.Statement:
	// 	return formatStatement(node, env)
	case *ast.Item:
		return formatItem(node, env)
	case *ast.MultiKeyword:
		return formatMultiKeyword(node, env)
	case *ast.Aliased:
		return formatAliased(node, env)
	case *ast.Identifier:
		return formatIdentifier(node, env)
	case *ast.MemberIdentifier:
		return formatMemberIdentifier(node, env)
	case *ast.Operator:
		return formatOperator(node, env)
	case *ast.Comparison:
		return formatComparison(node, env)
	case *ast.Parenthesis:
		return formatParenthesis(node, env)
	// case *ast.ParenthesisInner:
	case *ast.FunctionLiteral:
		return formatFunctionLiteral(node, env)
	case *ast.IdentifierList:
		return formatIdentifierList(node, env)
	// case *ast.SwitchCase:
	// case *ast.Null:
	default:
		if list, ok := node.(ast.TokenList); ok {
			return formatTokenList(list, env)
		}
		return formatNode(node, env)
	}
}

func formatItem(node ast.Node, env *formatEnvironment) ast.Node {
	results := []ast.Node{node}

	// Track certain keywords that affect formatting of subsequent keywords
	if item, ok := node.(*ast.Item); ok {
		if tok := item.GetToken(); tok.Kind == token.SQLKeyword {
			if sqlWord, ok := tok.Value.(*token.SQLWord); ok {
				kw := sqlWord.Keyword
				// Only track keywords that matter for context-sensitive formatting
				if kw == "JOIN" || kw == "DELETE" || kw == "INSERT" || kw == "CREATE" || kw == "UPDATE" {
					env.lastKeyword = kw
				}
			}
		}
		if formatted := formatContextItem(item, env); formatted != nil {
			return formatted
		}
	}

	whitespaceAfterMatcher := astutil.NodeMatcher{
		ExpectKeyword: []string{
			"JOIN",
			"ON",
			"AND",
			"OR",
			"LIMIT",
			"WHEN",
			"ELSE",
			"AS",
			"DELETE",
			"UPDATE",
			"SET",
			"BEFORE",
			"AFTER",
			"TRIGGER",
			"INDEX",
			"ALTER",
			"ADD",
			"DROP",
			"MODIFY",
			"COLUMN",
		},
	}
	if whitespaceAfterMatcher.IsMatch(node) {
		results = append(results, whitespaceNode)
	}
	whitespaceAroundMatcher := astutil.NodeMatcher{
		ExpectKeyword: []string{
			"BETWEEN",
			"USING",
			"THEN",
		},
	}
	if whitespaceAroundMatcher.IsMatch(node) {
		results = unshift(results, whitespaceNode)
		results = append(results, whitespaceNode)
	}

	// Add an adjustment indent before the cursor
	outdentBeforeMatcher := astutil.NodeMatcher{
		ExpectKeyword: []string{
			"FROM",
			"JOIN",
			"WHERE",
			"HAVING",
			"LIMIT",
			"UNION",
			"VALUES",
			"SET",
			"EXCEPT",
			"END",
		},
	}
	if outdentBeforeMatcher.IsMatch(node) {
		env.indentLevelDown()
		results = unshift(results, env.genIndent()...)
		results = unshift(results, linebreakNode)
	}
	indentBeforeMatcher := astutil.NodeMatcher{
		ExpectKeyword: []string{
			"ON",
		},
	}
	if indentBeforeMatcher.IsMatch(node) {
		// Only indent ON if preceded by JOIN keyword
		// CREATE INDEX ON and CREATE TRIGGER ON should not be indented
		shouldIndent := false
		if env.lastKeyword == "JOIN" {
			shouldIndent = true
		}

		if shouldIndent {
			env.indentLevelUp()
			results = unshift(results, env.genIndent()...)
			results = unshift(results, linebreakNode)
		} else {
			// Still add linebreak for ON keyword, just without indent
			results = unshift(results, linebreakNode)
		}
	}
	linebreakBeforeMatcher := astutil.NodeMatcher{
		ExpectKeyword: []string{
			"AND",
			"OR",
			"WHEN",
			"ELSE",
		},
	}
	if linebreakBeforeMatcher.IsMatch(node) {
		results = unshift(results, env.genIndent()...)
		results = unshift(results, linebreakNode)
	}

	// Add an adjustment indent after the cursor
	linebreakWithIndentAfterMatcher := astutil.NodeMatcher{
		ExpectKeyword: []string{
			"SELECT",
			"FROM",
			"WHERE",
			"HAVING",
		},
		ExpectTokens: []token.Kind{
			token.LParen,
		},
	}
	if linebreakWithIndentAfterMatcher.IsMatch(node) {
		results = append(results, linebreakNode)
		env.indentLevelUp()
		results = append(results, env.genIndent()...)
	}
	indentAfterMatcher := astutil.NodeMatcher{
		ExpectKeyword: []string{
			"CASE",
		},
	}
	if indentAfterMatcher.IsMatch(node) {
		env.indentLevelUp()
	}
	linebreakAfterMatcher := astutil.NodeMatcher{
		ExpectTokens: []token.Kind{
			token.Comma,
		},
	}
	if linebreakAfterMatcher.IsMatch(node) {
		results = append(results, linebreakNode)
		results = append(results, env.genIndent()...)
	}
	commentAfterMatcher := astutil.NodeMatcher{
		ExpectTokens: []token.Kind{
			token.Comment,
		},
	}
	if commentAfterMatcher.IsMatch(node) {
		results = append(results, linebreakNode)
		env.indentLevelDown()
	}

	breakStatementAfterMatcher := astutil.NodeMatcher{
		ExpectTokens: []token.Kind{
			token.Semicolon,
		},
	}
	if breakStatementAfterMatcher.IsMatch(node) {
		results = append(results, linebreakNode)
		env.indentLevelReset()
		env.lastKeyword = ""
		env.betweenPending = false
	}

	return &ast.ItemWith{Toks: results}
}

func formatMultiKeyword(node *ast.MultiKeyword, env *formatEnvironment) ast.Node {
	results := []ast.Node{}

	keywords := node.GetKeywords()
	for i, kw := range keywords {
		results = append(results, kw)
		if i != len(keywords)-1 {
			results = append(results, whitespaceNode)
		}
	}

	// Track last keyword in MultiKeyword
	if len(keywords) > 0 {
		lastKw := keywords[len(keywords)-1]
		if item, ok := lastKw.(*ast.Item); ok {
			if tok := item.GetToken(); tok.Kind == token.SQLKeyword {
				if sqlWord, ok := tok.Value.(*token.SQLWord); ok {
					env.lastKeyword = sqlWord.Keyword
				}
			}
		}
	}

	insertKeyword := "INSERT INTO"
	joinKeywords := []string{
		"INNER JOIN",
		"CROSS JOIN",
		"OUTER JOIN",
		"LEFT JOIN",
		"RIGHT JOIN",
		"LEFT OUTER JOIN",
		"RIGHT OUTER JOIN",
	}
	byKeywords := []string{
		"GROUP BY",
		"ORDER BY",
	}

	whitespaceAfterMatcher := astutil.NodeMatcher{
		ExpectKeyword: append(joinKeywords, insertKeyword),
	}
	if whitespaceAfterMatcher.IsMatch(node) {
		results = append(results, whitespaceNode)
	}

	// Add an adjustment indent before the cursor
	outdentBeforeMatcher := astutil.NodeMatcher{
		ExpectKeyword: append(joinKeywords, byKeywords...),
	}
	if outdentBeforeMatcher.IsMatch(node) {
		env.indentLevelDown()
		results = unshift(results, env.genIndent()...)
		results = unshift(results, linebreakNode)
	}

	// Add an adjustment indent after the cursor
	linebreakWithIndentAfterMatcher := astutil.NodeMatcher{
		ExpectKeyword: byKeywords,
	}
	if linebreakWithIndentAfterMatcher.IsMatch(node) {
		results = append(results, linebreakNode)
		env.indentLevelUp()
		results = append(results, env.genIndent()...)
	}

	return &ast.ItemWith{Toks: results}
}

func formatAliased(node *ast.Aliased, env *formatEnvironment) ast.Node {
	var results []ast.Node
	// Handle real name which may be a FunctionLiteral (e.g., INT(11))
	realNameTokens := Eval(node.RealName, env)
	results = append(results, realNameTokens)

	// Add whitespace before alias
	if node.IsAs || node.AliasedName != nil {
		results = append(results, whitespaceNode)
	}

	// Add AS keyword and aliased name
	if node.IsAs && node.As != nil {
		results = append(results,
			node.As,
			whitespaceNode,
			Eval(node.AliasedName, env),
		)
	} else if node.AliasedName != nil {
		results = append(results,
			Eval(node.AliasedName, env),
		)
	}
	return &ast.ItemWith{Toks: results}
}

// identifierKeywords are keywords the parser reads as identifiers and the
// formatter renders as keywords.
var identifierKeywords = []string{"IF"}

func formatIdentifier(node ast.Node, env *formatEnvironment) ast.Node {
	// Handle special case: IF keyword parsed as Identifier
	if ident, ok := node.(*ast.Identifier); ok {
		keyword := ident.String()
		if slices.Contains(identifierKeywords, keyword) {
			// Convert to lowercase keyword Item
			lowerItem := ast.NewItem(&token.Token{
				Kind: token.SQLKeyword,
				Value: &token.SQLWord{
					Value:   strings.ToLower(keyword),
					Keyword: strings.ToLower(keyword),
				},
			})
			return formatItem(lowerItem, env)
		}
	}

	results := []ast.Node{node}
	return &ast.ItemWith{Toks: results}
}

func formatMemberIdentifier(node *ast.MemberIdentifier, env *formatEnvironment) ast.Node {
	results := []ast.Node{
		Eval(node.Parent, env),
		periodNode,
	}
	// The parser leaves Child nil when a keyword follows the period, as in
	// "a.active"; that keyword is the next sibling node.
	if node.Child != nil {
		results = append(results, Eval(node.Child, env))
	}
	return &ast.ItemWith{Toks: results}
}

func formatOperator(node *ast.Operator, env *formatEnvironment) ast.Node {
	return formatInfix(node.Left, node.Operator, node.Right, env)
}

func formatComparison(node *ast.Comparison, env *formatEnvironment) ast.Node {
	return formatInfix(node.Left, node.Comparison, node.Right, env)
}

// formatInfix tolerates a nil operand: the parser leaves one when it is a
// parameter, keyword or signed number, as in "id = :id", and that operand is
// the next sibling node.
func formatInfix(left, operator, right ast.Node, env *formatEnvironment) ast.Node {
	results := []ast.Node{}
	if left != nil {
		results = append(results, Eval(left, env), whitespaceNode)
	}
	results = append(results, operator)
	if right != nil {
		results = append(results, whitespaceNode, Eval(right, env))
	}
	return &ast.ItemWith{Toks: results}
}

func formatParenthesis(node *ast.Parenthesis, env *formatEnvironment) ast.Node {
	results := []ast.Node{}
	results = append(results, lparenNode)

	inner := node.Inner()
	if containsSelect(node) {
		// A subquery is laid out on its own lines, even within arguments.
		inline := env.inline
		env.inline = 0
		defer func() { env.inline = inline }()
	} else if env.inline > 0 {
		return &ast.ItemWith{Toks: []ast.Node{lparenNode, Eval(inner, env), rparenNode}}
	}

	// Check if inner contains IdentifierList
	innerToks := inner.GetTokens()
	if len(innerToks) == 1 {
		if idList, ok := innerToks[0].(*ast.IdentifierList); ok {
			// IdentifierList - format with linebreaks and indentation
			startIndentLevel := env.indentLevel
			env.indentLevelUp()
			results = append(results, linebreakNode)
			results = append(results, env.genIndent()...)
			results = append(results, Eval(idList, env))
			env.indentLevel = startIndentLevel
			results = append(results, linebreakNode)
			results = append(results, env.genIndent()...)
			results = append(results, rparenNode)
			return &ast.ItemWith{Toks: results}
		}
	}

	// Check if should format multi-line (has comma, SELECT keyword or a
	// condition joined by AND or OR)
	shouldMultiLine := joinsConditions(innerToks)

	for _, tok := range innerToks {
		if _, ok := tok.(*ast.IdentifierList); ok {
			shouldMultiLine = true
			break
		}
		if item, ok := tok.(*ast.Item); ok {
			tokKind := item.GetToken().Kind
			// Multi-line if has comma
			if tokKind == token.Comma {
				shouldMultiLine = true
				break
			}
			// Multi-line if has SELECT keyword (subquery)
			if tokKind == token.SQLKeyword {
				if sqlWord, ok := item.GetToken().Value.(*token.SQLWord); ok {
					if sqlWord.Keyword == "SELECT" {
						shouldMultiLine = true
						break
					}
				}
			}
		}
	}

	if shouldMultiLine {
		startIndentLevel := env.indentLevel
		env.indentLevelUp()
		results = append(results, linebreakNode)
		results = append(results, env.genIndent()...)
		results = append(results, Eval(inner, env))
		env.indentLevel = startIndentLevel
		results = append(results, linebreakNode)
		results = append(results, env.genIndent()...)
	} else {
		// Single line for simple cases like VARCHAR(100), function args
		results = append(results, Eval(inner, env))
	}

	results = append(results, rparenNode)
	return &ast.ItemWith{Toks: results}
}

func formatFunctionLiteral(node *ast.FunctionLiteral, env *formatEnvironment) ast.Node {
	// Check if this is a simple function call like SUM(x) or COUNT(y)
	// In this case, keep it on one line
	if len(node.Toks) == 2 {
		// First token should be the function name (Item)
		// Second token should be a Parenthesis
		if _, ok := node.Toks[0].(*ast.Item); ok {
			if paren, ok := node.Toks[1].(*ast.Parenthesis); ok {
				inner := paren.Inner()
				// Check if inner contains only a single identifier
				innerToks := inner.GetTokens()
				if len(innerToks) == 1 {
					if _, ok := innerToks[0].(*ast.Identifier); ok {
						// Simple function call like SUM(x)
						results := []ast.Node{
							node.Toks[0], // function name
							lparenNode,
							Eval(innerToks[0], env), // identifier
							rparenNode,
						}
						return &ast.ItemWith{Toks: results}
					}
				}
			}
		}
	}

	// Default: format recursively, arguments on one line
	results := []ast.Node{}
	for _, tok := range node.Toks {
		if isParenthesis(tok) {
			results = append(results, formatInline(tok, env))
		} else {
			results = append(results, Eval(tok, env))
		}
	}
	return &ast.ItemWith{Toks: results}
}

// formatIdentifierList lays out the list's tokens rather than its
// identifiers, which omit the comments that follow a comma. A trailing comma
// precedes a list item the parser left outside the list, as the keyword in
// "(1, current_date)".
func formatIdentifierList(identifierList *ast.IdentifierList, env *formatEnvironment) ast.Node {
	results := []ast.Node{}
	toks := identifierList.GetTokens()
	for i, tok := range toks {
		switch {
		case isItemKind(tok, token.Whitespace):
		case isItemKind(tok, token.Comma):
			if env.inline > 0 {
				results = append(results, commaNode, whitespaceNode)
				continue
			}
			results = append(results, commaNode, linebreakNode)
			results = append(results, env.genIndent()...)
		case isItemKind(tok, token.Comment, token.MultilineComment):
			comment := formatComment(toks, i, results, env)
			results = append(results, comment)
			if !endsWithWhitespace(comment) {
				results = append(results, whitespaceNode)
			}
		default:
			results = append(results, Eval(tok, env))
		}
	}
	return &ast.ItemWith{Toks: results}
}

func formatTokenList(list ast.TokenList, env *formatEnvironment) ast.Node {
	results := []ast.Node{}
	reader := astutil.NewNodeReader(list)
	var previous ast.Node
	for reader.NextNode(true) {
		env.reader = reader
		var formatted ast.Node
		item, isItem := reader.CurNode.(*ast.Item)
		switch {
		case isItem && isMemberName(previous, item):
			formatted = memberKeyword(item)
		case isItemKind(reader.CurNode, token.Comment, token.MultilineComment):
			formatted = formatComment(list.GetTokens(), reader.Index-1, results, env)
		case isKeywordNode(previous, "IN") && isParenthesis(reader.CurNode):
			formatted = formatInline(reader.CurNode, env)
		case isKeywordNode(reader.CurNode, "SELECT") && isParenthesis(previous) && env.inline == 0:
			// INSERT INTO t (...) SELECT
			formatted = &ast.ItemWith{Toks: []ast.Node{linebreakNode, Eval(reader.CurNode, env)}}
		default:
			formatted = Eval(reader.CurNode, env)
		}
		before := previous
		previous = reader.CurNode
		results = append(results, formatted)

		// Add whitespace between adjacent tokens when needed
		// Check if we should add space after current token
		if shouldAddSpaceAfter(reader.CurNode, before, reader, formatted, env) {
			results = append(results, whitespaceNode)
		}
	}
	reader.Node.SetTokens(results)
	return reader.Node
}

func shouldAddSpaceAfter(curNode, previous ast.Node, reader *astutil.NodeReader, formatted ast.Node, env *formatEnvironment) bool {
	// Don't add space if already ends with whitespace/linebreak
	if endsWithWhitespace(formatted) {
		return false
	}

	// Check next node (ignoreWhitespace=true to skip linebreaks)
	_, nextNode := reader.PeekNode(true)
	if nextNode == nil {
		return false
	}

	// The member's keyword name follows directly: "a.active"
	if item, ok := nextNode.(*ast.Item); ok && isMemberName(curNode, item) {
		return false
	}

	// A line comment ends with its own linebreak; a block comment is
	// separated from what follows it, and comments from what precedes them.
	if item, ok := curNode.(*ast.Item); ok && item.GetToken().Kind == token.MultilineComment {
		return !isItemKind(nextNode, token.Comma, token.RParen, token.Semicolon)
	}
	if isItemKind(nextNode, token.Comment, token.MultilineComment) {
		return true
	}

	// "X = CASE", "IF (cond)", "IN (1, 2)"; "INSERT INTO t (a) SELECT"
	if isDanglingInfix(curNode) || isParenthesis(nextNode) && (isKeywordNode(curNode, "IN") || isIdentifierKeyword(curNode)) {
		return true
	}
	if isParenthesis(curNode) && isKeywordNode(nextNode, "SELECT") && env.inline == 0 {
		return false
	}

	// A binary "-" or "+" is spaced from its right operand
	if isItemKind(curNode, token.Minus, token.Plus) && isOperand(previous) {
		return true
	}

	// Comparisons the parser left ungrouped, as in "a.active = 1"
	if isItemKind(curNode, comparisonKinds...) || isItemKind(nextNode, comparisonKinds...) {
		return true
	}

	// "- -1" and "/ *" must not become comments
	if (isItemKind(curNode, token.Minus) && isItemKind(nextNode, token.Minus)) ||
		(isItemKind(curNode, token.Div) && isItemKind(nextNode, token.Mult)) {
		return true
	}

	// The two characters of "||" stay together; the operator they form is
	// spaced from its operands. Other unrecognized characters, such as the
	// "_" of "_utf8'abc'", stay attached to what follows.
	if isCharItem(curNode, "|") || isCharItem(nextNode, "|") {
		return !(isCharItem(curNode, "|") && isCharItem(nextNode, "|"))
	}
	if last := lastNode(curNode); isItemKind(last, token.Char) && !isCharItem(last, "?") {
		return false
	}

	// Don't add space before MultiKeyword (e.g., "GROUP BY") - always
	if _, ok := nextNode.(*ast.MultiKeyword); ok {
		return false
	}

	// Don't add space before punctuation
	if item, ok := nextNode.(*ast.Item); ok {
		tok := item.GetToken()
		if tok.Kind == token.Comma || tok.Kind == token.RParen ||
			tok.Kind == token.Semicolon || tok.Kind == token.Period ||
			tok.Kind == token.LParen {
			return false
		}

		// Don't add space before BETWEEN, USING, THEN (they add their own space before)
		if tok.Kind == token.SQLKeyword {
			if sqlWord, ok := tok.Value.(*token.SQLWord); ok {
				kw := sqlWord.Keyword
				if kw == "BETWEEN" || kw == "USING" || kw == "THEN" {
					return false
				}
			}
		}

		// Don't add space before keywords that add linebreak before themselves
		if tok.Kind == token.SQLKeyword {
			if sqlWord, ok := tok.Value.(*token.SQLWord); ok {
				kw := sqlWord.Keyword
				// These keywords add linebreak before (via linebreakBeforeMatcher, outdentBeforeMatcher, etc)
				linebreakBeforeKeywords := []string{
					"FROM", "JOIN", "WHERE", "HAVING", "LIMIT", "UNION", "VALUES", "SET", "EXCEPT", "END",
					"ON", "AND", "OR", "WHEN", "ELSE",
				}
				for _, lbkw := range linebreakBeforeKeywords {
					if kw == lbkw {
						// On one line, or the AND of BETWEEN, it stays on this line
						if env.inline > 0 || kw == "AND" && env.betweenPending {
							return true
						}
						// Exception: if current keyword is DELETE and next is FROM, always add space
						// because "DELETE FROM" should be on one line
						if curItem, ok := curNode.(*ast.Item); ok {
							if curTok := curItem.GetToken(); curTok.Kind == token.SQLKeyword {
								if curSqlWord, ok := curTok.Value.(*token.SQLWord); ok {
									if curSqlWord.Keyword == "DELETE" && kw == "FROM" {
										return true // DELETE FROM should have space
									}
								}
							}
						}
						return false
					}
				}
			}
		}
	}

	// Add space after Item (keyword) unless specific exceptions
	if curItem, ok := curNode.(*ast.Item); ok {
		curTok := curItem.GetToken()

		// Add space before AS keyword for literals
		if nextItem, ok := nextNode.(*ast.Item); ok {
			if nextTok := nextItem.GetToken(); nextTok.Kind == token.SQLKeyword {
				if sqlWord, ok := nextTok.Value.(*token.SQLWord); ok {
					if sqlWord.Keyword == "AS" {
						// Add space before AS for any literal (number, string, boolean, etc.)
						if curTok.Kind != token.SQLKeyword {
							return true
						}
					}
				}
			}
		}

		// A literal is separated from a following word: "2 DESC"
		if curTok.Kind != token.SQLKeyword {
			return wordLike(curTok.Kind) && startsWithWord(nextNode)
		}

		// Check if current item is BETWEEN, USING, THEN (handled by whitespaceAroundMatcher)
		if sqlWord, ok := curTok.Value.(*token.SQLWord); ok {
			kw := sqlWord.Keyword
			if kw == "BETWEEN" || kw == "USING" || kw == "THEN" {
				return false // Already handled by formatItem
			}
		}

		// Don't add space before Parenthesis (e.g., VALUES(...))
		if _, ok := nextNode.(*ast.Parenthesis); ok {
			return false
		}

		// Don't add space before punctuation (already checked above)
		// Don't add space before BETWEEN, USING, THEN
		if item, ok := nextNode.(*ast.Item); ok {
			if tok := item.GetToken(); tok.Kind == token.SQLKeyword {
				if sqlWord, ok := tok.Value.(*token.SQLWord); ok {
					kw := sqlWord.Keyword
					if kw == "BETWEEN" || kw == "USING" || kw == "THEN" {
						return false
					}
				}
			}
		}

		// Default: add space after any SQL keyword
		return true
	}

	// Add space after Identifier if next is Item (keyword) or FunctionLiteral
	if _, ok := curNode.(*ast.Identifier); ok {
		if item, ok := nextNode.(*ast.Item); ok {
			// Don't add space before BETWEEN, USING, THEN (handled by whitespaceAroundMatcher)
			if tok := item.GetToken(); tok.Kind == token.SQLKeyword {
				if sqlWord, ok := tok.Value.(*token.SQLWord); ok {
					kw := sqlWord.Keyword
					if kw == "BETWEEN" || kw == "USING" || kw == "THEN" {
						return false
					}
				}
			}
			return true
		}
		// Don't add space before MultiKeyword (e.g., "GROUP BY") - it adds its own linebreak
		if _, ok := nextNode.(*ast.MultiKeyword); ok {
			return false
		}
		if _, ok := nextNode.(*ast.FunctionLiteral); ok {
			return true
		}
		if _, ok := nextNode.(*ast.IdentifierList); ok {
			return startsWithWord(nextNode)
		}
	}

	// Add space after FunctionLiteral if next is Item (keyword)
	if _, ok := curNode.(*ast.FunctionLiteral); ok {
		if _, ok := nextNode.(*ast.Item); ok {
			return true
		}
	}

	// Add space after ItemWith (e.g., MultiKeyword result like "DELETE FROM")
	if _, ok := formatted.(*ast.ItemWith); ok {
		// Don't add space before punctuation
		if item, ok := nextNode.(*ast.Item); ok {
			tok := item.GetToken()
			if tok.Kind == token.Comma || tok.Kind == token.RParen ||
				tok.Kind == token.Semicolon || tok.Kind == token.Period ||
				tok.Kind == token.LParen || tok.Kind == token.Comment {
				return false
			}
		}

		// Don't add space before Parenthesis (e.g., VALUES(...))
		if _, ok := nextNode.(*ast.Parenthesis); ok {
			return false
		}

		// Don't add space before MultiKeyword (e.g., "GROUP BY")
		if _, ok := nextNode.(*ast.MultiKeyword); ok {
			return false
		}

		// Add space before most node types
		switch nextNode.(type) {
		case *ast.Identifier, *ast.Item, *ast.Aliased, *ast.MemberIdentifier, *ast.FunctionLiteral, *ast.IdentifierList:
			return true
		}
	}

	// Add space after MultiKeyword (e.g., "DELETE FROM") if it has multiple keywords
	if mk, ok := curNode.(*ast.MultiKeyword); ok {
		// Only add space if MultiKeyword has multiple keywords (e.g., "DELETE FROM")
		// Single keyword MultiKeywords (e.g., just "FROM") are handled by formatItem
		if len(mk.GetKeywords()) > 1 {
			// Don't add space if already ends with whitespace
			if endsWithWhitespace(formatted) {
				return false
			}
			// Add space before most node types
			switch nextNode.(type) {
			case *ast.Identifier, *ast.Item, *ast.Aliased:
				return true
			}
		}
	}

	return false
}

func endsWithWhitespace(node ast.Node) bool {
	if itemWith, ok := node.(*ast.ItemWith); ok {
		toks := itemWith.GetTokens()
		if len(toks) > 0 {
			return endsWithWhitespace(toks[len(toks)-1])
		}
	}
	if item, ok := node.(*ast.Item); ok {
		return item.GetToken().Kind == token.Whitespace
	}
	return false
}

func formatNode(node ast.Node, env *formatEnvironment) ast.Node {
	return node
}
