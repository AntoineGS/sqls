package formatter

import (
	"slices"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/token"
)

func unshift(slice []ast.Node, node ...ast.Node) []ast.Node {
	return append(node, slice...)
}

var whitespaceNode = ast.NewItem(&token.Token{
	Kind:  token.Whitespace,
	Value: " ",
})

func whiteSpaceNodes(num int) []ast.Node {
	res := make([]ast.Node, num)
	for i := 0; i < num; i++ {
		res[i] = whitespaceNode
	}
	return res
}

var linebreakNode = ast.NewItem(&token.Token{
	Kind:  token.Whitespace,
	Value: "\n",
})

var tabNode = ast.NewItem(&token.Token{
	Kind:  token.Whitespace,
	Value: "\t",
})

var periodNode = ast.NewItem(&token.Token{
	Kind:  token.Period,
	Value: ".",
})

var lparenNode = ast.NewItem(&token.Token{
	Kind:  token.LParen,
	Value: "(",
})

var rparenNode = ast.NewItem(&token.Token{
	Kind:  token.RParen,
	Value: ")",
})

var commaNode = ast.NewItem(&token.Token{
	Kind:  token.Comma,
	Value: ",",
})

var comparisonKinds = []token.Kind{token.Eq, token.Neq, token.Lt, token.Gt, token.LtEq, token.GtEq}

func isItemKind(node ast.Node, kinds ...token.Kind) bool {
	item, ok := node.(*ast.Item)
	return ok && slices.Contains(kinds, item.GetToken().Kind)
}

// isMemberName reports a keyword written directly after a member identifier
// such as "a." that the parser left without a name, as "active" in "a.active".
func isMemberName(member ast.Node, item *ast.Item) bool {
	m, ok := member.(*ast.MemberIdentifier)
	if !ok || m.Child != nil || item.GetToken().Kind != token.SQLKeyword {
		return false
	}
	_, ok = item.GetToken().Value.(*token.SQLWord)
	return ok && token.ComparePos(m.End(), item.Pos()) == 0
}

// memberKeyword renders a keyword used as a member name as written.
func memberKeyword(item *ast.Item) ast.Node {
	tok := item.GetToken()
	word := *tok.Value.(*token.SQLWord)
	word.Kind = dialect.Unmatched
	return ast.NewItem(&token.Token{Kind: tok.Kind, Value: &word, From: tok.From, To: tok.To})
}

// startsWithWord reports whether node begins with a token that must stay
// separated from a preceding word-like token.
func startsWithWord(node ast.Node) bool {
	switch node := node.(type) {
	case *ast.Item:
		return wordLike(node.GetToken().Kind)
	case *ast.Identifier, *ast.MemberIdentifier, *ast.Aliased, *ast.FunctionLiteral:
		return true
	case *ast.IdentifierList:
		if toks := node.GetTokens(); len(toks) > 0 {
			return startsWithWord(toks[0])
		}
	}
	return false
}

func isCharItem(node ast.Node, value string) bool {
	item, ok := node.(*ast.Item)
	return ok && item.GetToken().Kind == token.Char && item.GetToken().Value == value
}

// lastNode returns the last leaf of node, as the "_" ending the list "'a', _".
func lastNode(node ast.Node) ast.Node {
	if list, ok := node.(ast.TokenList); ok {
		if toks := list.GetTokens(); len(toks) > 0 {
			return lastNode(toks[len(toks)-1])
		}
	}
	return node
}
