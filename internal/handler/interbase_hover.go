package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/ast/astutil"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/parser"
	"github.com/sqls-server/sqls/parser/parseutil"
	"github.com/sqls-server/sqls/token"
)

// hoverDDLTimeout bounds the catalog round trip. textDocument/hover stays on
// the inline dispatch path — Plan 1 moves only workspace/executeCommand off it
// — so an unbounded catalog read would freeze the whole server.
const hoverDDLTimeout = 3 * time.Second

// hoverTarget is a catalog object the cursor is on.
type hoverTarget struct {
	kind database.ObjectKind
	name string
}

var hoverIdentifierMatcher = astutil.NodeMatcher{
	NodeTypes: []ast.NodeType{ast.TypeIdentifier},
}

var hoverMemberIdentifierMatcher = astutil.NodeMatcher{
	NodeTypes: []ast.NodeType{ast.TypeMemberIdentifier},
}

// resolveInterBaseHoverTarget classifies the identifier under the cursor
// against the catalog and returns the range to highlight. It is pure: no I/O,
// no server state.
//
// Views are resolved before tables. A view is in SchemaTables too, so a
// table-first order would ask ObjectDDL for CREATE TABLE of a view.
func resolveInterBaseHoverTarget(text string, params lsp.HoverParams, dbCache *database.DBCache, driver dialect.DatabaseDriver) (hoverTarget, lsp.Range, bool) {
	if driver != dialect.DatabaseDriverInterBase || !dbCache.HasCatalog() {
		return hoverTarget{}, lsp.Range{}, false
	}

	parsed, err := parser.ParseWithDriver(text, driver)
	if err != nil {
		return hoverTarget{}, lsp.Range{}, false
	}
	pos := token.Pos{
		Line: params.Position.Line,
		Col:  params.Position.Character + 1,
	}
	nodeWalker := parseutil.NewNodeWalker(parsed, pos)
	node := nodeWalker.CurNodeBottomMatched(hoverIdentifierMatcher)
	if node == nil {
		return hoverTarget{}, lsp.Range{}, false
	}
	ident, ok := node.(*ast.Identifier)
	if !ok {
		return hoverTarget{}, lsp.Range{}, false
	}
	name := ident.NoQuoteString()

	// A member identifier's child is a column, not an object. Columns and
	// aliases produce no target.
	if member := nodeWalker.CurNodeTopMatched(hoverMemberIdentifierMatcher); member != nil {
		if mi, ok := member.(*ast.MemberIdentifier); ok && mi.ChildTok != nil && mi.ChildTok.NoQuoteString() == name {
			return hoverTarget{}, lsp.Range{}, false
		}
	}

	identRange := lsp.Range{
		Start: lsp.Position{Line: ident.Pos().Line, Character: ident.Pos().Col},
		End:   lsp.Position{Line: ident.End().Line, Character: ident.End().Col},
	}

	// The accessors normalise the name they are given, so the identifier text
	// goes in exactly as the user typed it. Never upper-case here.
	if desc, ok := dbCache.View(name); ok {
		return hoverTarget{kind: database.ObjectKindView, name: desc.Name}, identRange, true
	}
	if desc, ok := dbCache.Procedure(name); ok {
		return hoverTarget{kind: database.ObjectKindProcedure, name: desc.Name}, identRange, true
	}
	if desc, ok := dbCache.Trigger(name); ok {
		return hoverTarget{kind: database.ObjectKindTrigger, name: desc.Name}, identRange, true
	}
	if desc, ok := dbCache.Generator(name); ok {
		return hoverTarget{kind: database.ObjectKindGenerator, name: desc.Name}, identRange, true
	}
	if desc, ok := dbCache.Function(name); ok {
		return hoverTarget{kind: database.ObjectKindFunction, name: desc.Name}, identRange, true
	}
	if cols, ok := dbCache.ColumnDescs(name); ok {
		if !dbCache.MetadataReady(database.MetadataViews) {
			return hoverTarget{}, lsp.Range{}, false
		}
		canonical := name
		if len(cols) > 0 {
			canonical = cols[0].Table
		}
		return hoverTarget{kind: database.ObjectKindTable, name: canonical}, identRange, true
	}
	return hoverTarget{}, lsp.Range{}, false
}

// interBaseHoverSummary renders what the catalog knows and the pure,
// cache-only hover could not. It returns "" for a table, whose markdown column
// table hoverWithDriver already produced and which must never be replaced.
func interBaseHoverSummary(target hoverTarget, dbCache *database.DBCache) string {
	switch target.kind {
	case database.ObjectKindView:
		if desc, ok := dbCache.View(target.name); ok {
			return database.ViewDoc(desc)
		}
	case database.ObjectKindProcedure:
		if desc, ok := dbCache.Procedure(target.name); ok {
			return database.ProcedureDoc(desc)
		}
	case database.ObjectKindTrigger:
		if desc, ok := dbCache.Trigger(target.name); ok {
			return database.TriggerDoc(desc)
		}
	case database.ObjectKindGenerator:
		if desc, ok := dbCache.Generator(target.name); ok {
			return database.GeneratorDoc(desc)
		}
	case database.ObjectKindFunction:
		if desc, ok := dbCache.Function(target.name); ok {
			return database.FunctionDoc(desc)
		}
	}
	return ""
}

// hoverUnsupportedDDLNote is the single italic line appended when the catalog
// cannot reproduce executable DDL. With no structured detail it degrades to a
// bare note rather than rendering a driver message: the user asked for
// documentation.
//
// The snapshot surface renders the same condition as plain prose for a SQL
// comment banner; see snapshotUnsupportedDDLNote. The two are deliberately
// worded for their own surface rather than shared.
func hoverUnsupportedDDLNote(err error) string {
	object, name, feature, ok := database.UnsupportedDDLDetail(err)
	if !ok {
		return "_DDL unavailable._"
	}
	return fmt.Sprintf("_DDL unavailable: %s %q: %s._", object, name, strings.TrimRight(feature, "."))
}

// renderObjectDDL returns the markdown to append after the summary, and
// whether the outcome is deterministic for this connection and therefore safe
// to memoise. A transport failure is not: it may succeed on the next hover.
func renderObjectDDL(ctx context.Context, repo database.DDLRepository, kind database.ObjectKind, name string) (string, bool) {
	ddlCtx, cancel := context.WithTimeout(ctx, hoverDDLTimeout)
	defer cancel()

	ddl, err := repo.ObjectDDL(ddlCtx, kind, name)
	switch {
	case err == nil:
		if strings.TrimSpace(ddl) == "" {
			return "", true
		}
		return fmt.Sprintf("\n\n---\n\n```sql\n%s\n```\n", strings.TrimRight(ddl, "\n")), true
	case errors.Is(err, database.ErrUnsupportedDDL):
		return "\n\n" + hoverUnsupportedDDLNote(err) + "\n", true
	case errors.Is(err, database.ErrObjectNotFound):
		// The cache named an object the catalog does not have, which means the
		// cache is stale. A stale-cache footnote on a hover popup is noise the
		// user cannot act on, so nothing at all is appended.
		return "", true
	default:
		log.Printf("sqls: object DDL for %s %q: %v", kind, name, err)
		return "", false
	}
}

// interBaseHover composes the catalog summary and the DDL appendix, returning
// nil when this path contributes nothing and the caller should use base.
//
// repo is a parameter rather than read off the server so a test can inject a
// capability-bearing repository: interbase_common.go's init already claims the
// InterBase driver name in database.driverFactories and RegisterFactory panics
// on a duplicate.
func (s *Server) interBaseHover(ctx context.Context, repo database.DBRepository, dbCache *database.DBCache, params lsp.HoverParams, text string, base *lsp.Hover, generation int, dv dialect.DriverVariant) *lsp.Hover {
	target, identRange, ok := resolveInterBaseHoverTarget(text, params, dbCache, dv.Driver)
	if !ok {
		return nil
	}

	value := interBaseHoverSummary(target, dbCache)
	if value == "" {
		if base == nil {
			return nil
		}
		value = base.Contents.Value
	}
	if base != nil {
		identRange = base.Range
	}

	value += s.objectDDLMarkdown(ctx, repo, target, generation)

	return &lsp.Hover{
		Contents: lsp.MarkupContent{Kind: lsp.Markdown, Value: value},
		Range:    identRange,
	}
}

// objectDDLMarkdown fetches and renders the DDL appendix, or "" when there is
// none to show. ObjectKindFunction is never attempted: the contract states it
// always returns ErrUnsupportedDDL, so calling it would guarantee a wasted
// round trip and a note line on every hover of an external function.
func (s *Server) objectDDLMarkdown(ctx context.Context, repo database.DBRepository, target hoverTarget, generation int) string {
	if target.kind == database.ObjectKindFunction || repo == nil {
		return ""
	}
	ddlRepo, ok := repo.(database.DDLRepository)
	if !ok {
		return ""
	}
	rendered := s.memoisedObjectDDL(ctx, ddlRepo, target, generation)
	return rendered
}

// ddlKey identifies one memoised DDL rendering. The generation is part of the
// key rather than a reason to clear the map, so a hover that started before a
// reconnect can never write a stale entry into the new connection's view.
type ddlKey struct {
	generation int
	kind       database.ObjectKind
	name       string
}

func (s *Server) connectionGeneration() int {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.connGeneration
}

// memoisedObjectDDL renders the DDL appendix, reusing a previous rendering for
// the same object on the same connection.
//
// The lock is taken twice and released around the round trip, never held
// across it: stateMu is a server-wide lock on the inline dispatch path, and
// ObjectDDL is bounded at three seconds. Two hovers on the same cold object
// can therefore both make the call; that duplicate is much cheaper than
// freezing every other request for the duration.
func (s *Server) memoisedObjectDDL(ctx context.Context, repo database.DDLRepository, target hoverTarget, generation int) string {
	key := ddlKey{generation: generation, kind: target.kind, name: target.name}

	s.stateMu.RLock()
	rendered, hit := s.ddlMemo[key]
	s.stateMu.RUnlock()
	if hit {
		return rendered
	}

	rendered, cacheable := renderObjectDDL(ctx, repo, target.kind, target.name)
	if !cacheable {
		// A transport failure or a timeout may succeed next time. Caching ""
		// for it would suppress this object's DDL until the next reconnect.
		return rendered
	}

	s.stateMu.Lock()
	if s.connGeneration == key.generation && s.ddlMemo != nil {
		s.ddlMemo[key] = rendered
	}
	s.stateMu.Unlock()
	return rendered
}
