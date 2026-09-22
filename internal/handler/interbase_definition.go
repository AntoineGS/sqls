package handler

import (
	"database/sql"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/ast/astutil"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/parser"
	"github.com/sqls-server/sqls/parser/parseutil"
	"github.com/sqls-server/sqls/token"
)

// snapshotTarget is a catalog object go-to-definition can materialise. source
// is the descriptor's verbatim catalog text, carried along because it is the
// body used when the catalog cannot reproduce executable DDL.
type snapshotTarget struct {
	kind   database.ObjectKind
	name   string
	source sql.NullString
}

// resolveSnapshotTarget classifies the identifier under the cursor against the
// catalog. It is pure: no I/O, no server state.
//
// Triggers are resolved here although they are excluded from completion. They
// are never named in DML, so there is no completion context for them, but the
// identifier under the cursor is a perfectly good way to reach one.
//
// A name can exist in more than one family (a procedure and a trigger sharing
// a name is legal in InterBase). The check order below is the precedence:
// Procedure, then View, then Trigger. It is a fixed sequence of accessor
// calls, not a map iteration, so it does not depend on Go's randomised map
// order.
func resolveSnapshotTarget(text string, params lsp.DefinitionParams, dbCache *database.DBCache, driver dialect.DatabaseDriver) (snapshotTarget, bool) {
	if driver != dialect.DatabaseDriverInterBase || !dbCache.HasCatalog() {
		return snapshotTarget{}, false
	}

	parsed, err := parser.ParseWithDriver(text, driver)
	if err != nil {
		return snapshotTarget{}, false
	}
	pos := token.Pos{
		Line: params.Position.Line,
		Col:  params.Position.Character + 1,
	}
	nodeWalker := parseutil.NewNodeWalker(parsed, pos)
	node := nodeWalker.CurNodeBottomMatched(astutil.NodeMatcher{
		NodeTypes: []ast.NodeType{ast.TypeIdentifier},
	})
	ident, ok := node.(*ast.Identifier)
	if !ok {
		return snapshotTarget{}, false
	}

	// NoQuoteString, not String: a delimited identifier's String() includes
	// the surrounding quotes (token.SQLWord.String()), but CatalogCache's map
	// keys never do — they come from catalogCacheKey applied to the
	// descriptor's own Name field, which the catalog reports unquoted. The
	// accessors normalise case, so the unquoted text goes in exactly as
	// written; never upper-case here.
	name := ident.NoQuoteString()
	if desc, ok := dbCache.Procedure(name); ok {
		return snapshotTarget{kind: database.ObjectKindProcedure, name: desc.Name, source: desc.Source}, true
	}
	if desc, ok := dbCache.View(name); ok {
		return snapshotTarget{kind: database.ObjectKindView, name: desc.Name, source: desc.ViewSource}, true
	}
	if desc, ok := dbCache.Trigger(name); ok {
		return snapshotTarget{kind: database.ObjectKindTrigger, name: desc.Name, source: desc.Source}, true
	}
	return snapshotTarget{}, false
}
