package formatter

import (
	"slices"
	"strings"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/ast/astutil"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/token"
)

// formatContextItem lays out the keywords whose layout depends on what
// precedes or follows them. It returns nil to apply the general rules of
// formatItem.
func formatContextItem(item *ast.Item, env *formatEnvironment) ast.Node {
	tok := item.GetToken()
	if tok.Kind == token.Comma && env.inline > 0 {
		return &ast.ItemWith{Toks: []ast.Node{item, whitespaceNode}}
	}
	keyword := keywordOf(item)
	// Only the SET right after "UPDATE <table> [alias]" starts its list.
	if env.lastKeyword == "UPDATE" && keyword != "" && keyword != "UPDATE" && keyword != "SET" {
		env.lastKeyword = ""
	}
	switch keyword {
	case "CASE":
		env.blocks = append(env.blocks, false)
	case "BEGIN":
		env.blocks = append(env.blocks, true)
		if env.inline == 0 {
			env.indentLevelReset()
			return &ast.ItemWith{Toks: []ast.Node{linebreakNode, item, linebreakNode}}
		}
	case "END":
		if n := len(env.blocks); n > 0 {
			block := env.blocks[n-1]
			env.blocks = env.blocks[:n-1]
			if block && env.inline == 0 {
				env.indentLevelReset()
				toks := []ast.Node{linebreakNode, item}
				if _, next := env.reader.PeekNode(true); !isItemKind(next, token.Semicolon) {
					toks = append(toks, linebreakNode)
				}
				return &ast.ItemWith{Toks: toks}
			}
		}
	case "BETWEEN":
		env.betweenPending = true
	case "AND":
		if env.betweenPending {
			env.betweenPending = false
			return &ast.ItemWith{Toks: []ast.Node{item, whitespaceNode}}
		}
	case "SET":
		if env.lastKeyword == "UPDATE" && env.inline == 0 {
			env.lastKeyword = ""
			env.indentLevelDown()
			toks := append([]ast.Node{linebreakNode}, env.genIndent()...)
			toks = append(toks, item, linebreakNode)
			env.indentLevelUp()
			return &ast.ItemWith{Toks: append(toks, env.genIndent()...)}
		}
	case "UNION":
		if env.inline == 0 {
			env.lastKeyword = "UNION"
			env.indentLevelDown()
			toks := append([]ast.Node{linebreakNode}, env.genIndent()...)
			toks = append(toks, item)
			if _, next := env.reader.PeekNode(true); !isKeywordNode(next, "ALL", "DISTINCT") {
				toks = append(toks, linebreakNode)
				toks = append(toks, env.genIndent()...)
			}
			return &ast.ItemWith{Toks: toks}
		}
	case "ALL", "DISTINCT":
		if env.lastKeyword == "UNION" {
			env.lastKeyword = ""
			return &ast.ItemWith{Toks: append([]ast.Node{item, linebreakNode}, env.genIndent()...)}
		}
	case "ROWS":
		if env.inline == 0 && takesValue(env.reader) {
			env.indentLevelDown()
			toks := append([]ast.Node{linebreakNode}, env.genIndent()...)
			return &ast.ItemWith{Toks: append(toks, item, whitespaceNode)}
		}
	}
	if env.inline > 0 && keyword != "" {
		// On one line: no line breaks, but these keywords still bring the
		// spaces the general rules expect from them.
		if keyword == "BETWEEN" || keyword == "USING" || keyword == "THEN" {
			return &ast.ItemWith{Toks: []ast.Node{whitespaceNode, item, whitespaceNode}}
		}
		return item
	}
	return nil
}

// formatInline formats node on one line, as function arguments and IN
// lists are, unless it holds a subquery.
func formatInline(node ast.Node, env *formatEnvironment) ast.Node {
	env.inline++
	defer func() { env.inline-- }()
	return Eval(node, env)
}

// keywordOf returns the upper-case keyword of an unquoted keyword item.
func keywordOf(node ast.Node) string {
	item, ok := node.(*ast.Item)
	if !ok || item.GetToken().Kind != token.SQLKeyword {
		return ""
	}
	word, ok := item.GetToken().Value.(*token.SQLWord)
	if !ok || word.QuoteStyle != 0 || word.Kind == dialect.Unmatched {
		return ""
	}
	return strings.ToUpper(word.Value)
}

func isKeywordNode(node ast.Node, keywords ...string) bool {
	keyword := keywordOf(node)
	return keyword != "" && slices.Contains(keywords, keyword)
}

// isDeclarationName reports the name a parameter or column list follows: the
// object of PROCEDURE, TABLE, VIEW or INSERT INTO, or RETURNS.
func isDeclarationName(node, previous ast.Node) bool {
	if isKeywordNode(node, "RETURNS") {
		return true
	}
	if _, ok := node.(*ast.Identifier); !ok {
		return false
	}
	if multi, ok := previous.(*ast.MultiKeyword); ok {
		keywords := multi.GetKeywords()
		return len(keywords) > 0 && isKeywordNode(keywords[len(keywords)-1], "INTO")
	}
	return isKeywordNode(previous, "PROCEDURE", "TABLE", "VIEW")
}

func isParenthesis(node ast.Node) bool {
	_, ok := node.(*ast.Parenthesis)
	return ok
}

// containsSelect reports a parenthesis that holds a subquery.
func containsSelect(node ast.Node) bool {
	paren, ok := node.(*ast.Parenthesis)
	if !ok {
		return false
	}
	return slices.ContainsFunc(paren.Inner().GetTokens(), func(tok ast.Node) bool { return isKeywordNode(tok, "SELECT") })
}

// takesValue reports whether the node after the current one, past comments,
// is a value, as in the InterBase "ROWS 1 TO 10", rather than a keyword or
// nothing, as in "OFFSET 10 ROWS" and "FETCH NEXT 1 ROWS ONLY".
func takesValue(reader *astutil.NodeReader) bool {
	next := reader.CopyReader()
	for next.NextNode(true) {
		if !isItemKind(next.CurNode, token.Comment, token.MultilineComment) {
			return keywordOf(next.CurNode) == "" && !isItemKind(next.CurNode, token.Semicolon)
		}
	}
	return false
}

// lineBreaks records whether the source breaks the line before and after a
// comment.
type lineBreaks struct{ before, after bool }

// commentLineBreaks returns the source line breaks around every comment in
// node. It reads the parsed tree before formatting rewrites it; a comment's
// neighbouring whitespace may belong to another node.
func commentLineBreaks(node ast.Node) map[ast.Node]lineBreaks {
	leaves := []ast.Node{}
	var walk func(ast.Node)
	walk = func(node ast.Node) {
		if list, ok := node.(ast.TokenList); ok {
			for _, child := range list.GetTokens() {
				walk(child)
			}
			return
		}
		leaves = append(leaves, node)
	}
	walk(node)
	breaks := map[ast.Node]lineBreaks{}
	for i, leaf := range leaves {
		if !isItemKind(leaf, token.Comment, token.MultilineComment) {
			continue
		}
		var b lineBreaks
		for j := i - 1; j >= 0 && isItemKind(leaves[j], token.Whitespace); j-- {
			b.before = b.before || isLineBreak(leaves[j])
		}
		for j := i + 1; j < len(leaves) && isItemKind(leaves[j], token.Whitespace); j++ {
			b.after = b.after || isLineBreak(leaves[j])
		}
		breaks[leaf] = b
	}
	return breaks
}

// formatComment lays out a comment. A comment the source puts on a line of
// its own keeps it, at the current indent; one that shares a line with code
// stays on that line. A line comment always ends its line. done holds the
// output so far.
func formatComment(comment ast.Node, done []ast.Node, env *formatEnvironment) ast.Node {
	var b lineBreaks
	if env.inline == 0 {
		b = env.commentBreaks[comment]
	}
	results := []ast.Node{}
	if b.before && !atLineStart(done) {
		results = append(append(results, linebreakNode), env.genIndent()...)
	}
	results = append(results, comment)
	if b.after || isItemKind(comment, token.Comment) {
		results = append(append(results, linebreakNode), env.genIndent()...)
	}
	return &ast.ItemWith{Toks: results}
}

// atLineStart reports whether nodes end with a line break and indent.
func atLineStart(nodes []ast.Node) bool {
	for i := len(nodes) - 1; i >= 0; i-- {
		if text := strings.TrimRight(nodes[i].Render(&ast.RenderOptions{}), " \t"); text != "" {
			return strings.HasSuffix(text, "\n")
		}
	}
	return false
}

func isLineBreak(node ast.Node) bool {
	item, ok := node.(*ast.Item)
	return ok && item.GetToken().Kind == token.Whitespace && item.GetToken().Value == "\n"
}

// joinsConditions reports an AND or OR among toks, other than the AND of
// "BETWEEN 1 AND 2".
func joinsConditions(toks []ast.Node) bool {
	between := false
	for _, tok := range toks {
		switch keywordOf(tok) {
		case "BETWEEN":
			between = true
		case "AND":
			if !between {
				return true
			}
			between = false
		case "OR":
			return true
		}
	}
	return false
}

// isDanglingInfix reports a comparison or operator whose right operand the
// parser left as the next node, as CASE in "X = CASE ... END".
func isDanglingInfix(node ast.Node) bool {
	switch node := node.(type) {
	case *ast.Comparison:
		return node.Right == nil
	case *ast.Operator:
		return node.Right == nil
	}
	return false
}

var valueKeywords = []string{"CURRENT_DATE", "CURRENT_TIME", "CURRENT_TIMESTAMP", "CURRENT_USER", "CURRENT_ROLE", "NULL", "USER"}

// isOperand reports whether node ends with an operand, which makes a
// following "-" or "+" binary.
func isOperand(node ast.Node) bool {
	if node == nil {
		return false
	}
	switch last := lastNode(node).(type) {
	case *ast.Identifier:
		return true
	case *ast.Item:
		switch last.GetToken().Kind {
		case token.Number, token.SingleQuotedString, token.NationalStringLiteral, token.RParen:
			return true
		case token.SQLKeyword:
			if word, ok := last.GetToken().Value.(*token.SQLWord); ok && (word.QuoteStyle != 0 || word.Kind == dialect.Unmatched) {
				return true
			}
			return isKeywordNode(last, valueKeywords...)
		}
	}
	return false
}
