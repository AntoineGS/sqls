package formatter

import (
	"slices"
	"strings"

	"github.com/sqls-server/sqls/ast"
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
	}
	if env.inline > 0 && keyword != "" {
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
