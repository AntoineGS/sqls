package parseutil

import (
	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/ast/astutil"
	"github.com/sqls-server/sqls/token"
)

// CallInfo describes the innermost function-style call the cursor is in.
//
// parseFunctions groups NAME( into an ast.FunctionLiteral whose first token is
// the name and whose second is the ast.Parenthesis, but only when the
// parenthesis follows the name with no whitespace. "MYPROC (1, 2)" is
// therefore not a call here, which matches sqls's existing behaviour for
// built-in functions.
type CallInfo struct {
	// Callee is the unquoted name of the called function or procedure.
	Callee string
	// Inside is true when the cursor is between the parentheses rather than
	// on the callee name.
	Inside bool

	args *ast.IdentifierList
}

var callLiteralMatcher = astutil.NodeMatcher{
	NodeTypes: []ast.NodeType{ast.TypeFunctionLiteral},
}

var callParenthesisMatcher = astutil.NodeMatcher{
	NodeTypes: []ast.NodeType{ast.TypeParenthesis},
}

var callArgumentsMatcher = astutil.NodeMatcher{
	NodeTypes: []ast.NodeType{ast.TypeIdentifierList},
}

// EnclosingCall reports the innermost call the cursor is within, if any. It is
// node-shaped rather than position-shaped, which is why it is not part of
// CheckSyntaxPosition: "the callee is a known procedure" and "the cursor is in
// a GEN_ID( argument list" are facts about the AST, not about a keyword that
// precedes the cursor.
func EnclosingCall(nw *NodeWalker) (CallInfo, bool) {
	node := nw.CurNodeBottomMatched(callLiteralMatcher)
	if node == nil {
		return CallInfo{}, false
	}
	list, ok := node.(ast.TokenList)
	if !ok {
		return CallInfo{}, false
	}
	toks := list.GetTokens()
	if len(toks) == 0 {
		return CallInfo{}, false
	}

	call := CallInfo{Callee: toks[0].String()}
	if ident, ok := toks[0].(*ast.Identifier); ok {
		call.Callee = ident.NoQuoteString()
	}
	call.Inside = nw.CurNodeIs(callParenthesisMatcher)

	if args := nw.CurNodeBottomMatched(callArgumentsMatcher); args != nil {
		if identifiers, ok := args.(*ast.IdentifierList); ok {
			call.args = identifiers
		}
	}
	return call, true
}

// ActiveParameter is the 0-based index of the argument the cursor is on,
// reusing the same helper the INSERT ... VALUES path uses.
//
// An empty argument list has no ast.IdentifierList, and GetIndex returns -1
// for a position it does not enclose. Both answer 0 here: the cursor is on the
// first parameter, and a negative index renders as "no active parameter" in
// the editor.
func (c CallInfo) ActiveParameter(pos token.Pos) int {
	if c.args == nil {
		return 0
	}
	if idx := c.args.GetIndex(pos); idx >= 0 {
		return idx
	}
	return 0
}
