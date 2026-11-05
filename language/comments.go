package language

import (
	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/parser/parseutil"
	"github.com/sqls-server/sqls/token"
)

func cursorInUnterminatedComment(nw *parseutil.NodeWalker) bool {
	for _, node := range nw.CurNodes() {
		item, ok := node.(ast.Token)
		if !ok {
			continue
		}
		tok := item.GetToken()
		if tok.Kind == token.MultilineComment && tok.Unterminated {
			return true
		}
	}
	return false
}
