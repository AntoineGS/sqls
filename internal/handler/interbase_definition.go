package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/ast/astutil"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
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
	column *sqlsymbol.Name
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
	return resolveSnapshotTargetLegacy(text, params, dbCache, dialect.DriverVariant{Driver: driver})
}

func resolveSnapshotTargetWithVariant(text string, params lsp.DefinitionParams, dbCache *database.DBCache, dv dialect.DriverVariant) (snapshotTarget, bool) {
	driver := dv.Driver
	if driver != dialect.DatabaseDriverInterBase || dbCache == nil {
		return snapshotTarget{}, false
	}
	if offset, valid := symbolOffset(text, params.Position); valid {
		if analysis, err := sqlsymbol.Analyze(text, dv); err == nil {
			resolution := analysis.Resolve(offset)
			if resolution.Role == sqlsymbol.Relation || (resolution.Role == sqlsymbol.Column && resolution.SQL != nil && len(resolution.SQL.Scopes) > 0) {
				if resolution.SQL == nil {
					return snapshotTarget{}, false
				}
				return resolveRelationTarget(*resolution.SQL, resolution.Role, dbCache)
			}
		}
	}
	return resolveSnapshotTargetLegacy(text, params, dbCache, dv)
}

func resolveSnapshotTargetLegacy(text string, params lsp.DefinitionParams, dbCache *database.DBCache, dv dialect.DriverVariant) (snapshotTarget, bool) {
	driver := dv.Driver
	if driver != dialect.DatabaseDriverInterBase || !dbCache.HasCatalog() {
		return snapshotTarget{}, false
	}
	parsed, err := parser.ParseWithDriverVariant(text, dv)
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

// snapshotBodyFor chooses the snapshot body from the ObjectDDL outcome. A false
// ok means no file may be written at all — not a banner, not an empty body.
func snapshotBodyFor(ddl string, ddlErr error, target snapshotTarget) (body, note string, ok bool) {
	switch {
	case ddlErr == nil:
		return ddl, "", true

	case errors.Is(ddlErr, database.ErrObjectNotFound):
		// The cache named an object the catalog does not have, so the cache is
		// stale. Writing a snapshot of nothing is worse than navigating nowhere.
		return "", "", false

	case errors.Is(ddlErr, database.ErrUnsupportedDDL):
		// The normal case for procedures: schema/README.md states that
		// parameter nullability is usually unknown, and only a non-nullable
		// domain can prove otherwise.
		if !target.source.Valid || target.source.String == "" {
			return "", "", false
		}
		return target.source.String, snapshotUnsupportedDDLNote(ddlErr), true

	default:
		log.Printf("sqls: object DDL for %s %q: %v", target.kind, target.name, ddlErr)
		return "", "", false
	}
}

// snapshotUnsupportedDDLNote names what blocked executable DDL, from the
// structured detail and never from the driver's message text. No CREATE header
// is synthesized anywhere: what follows this note is the catalog's own source.
//
// This is plain prose because it is rendered into a SQL comment banner. Hover
// renders the same condition as markdown; see hoverUnsupportedDDLNote. The two
// are deliberately worded for their own surface rather than shared.
func snapshotUnsupportedDDLNote(err error) string {
	object, name, feature, ok := database.UnsupportedDDLDetail(err)
	if !ok {
		return "Executable DDL could not be reproduced. The verbatim catalog source follows."
	}
	return fmt.Sprintf("Executable DDL could not be reproduced: %s %q: %s.\nThe verbatim catalog source follows.",
		object, name, feature)
}

// singleLine collapses CR and LF into spaces. The connection label is the
// user's own config alias rather than attacker-controlled data, so this is not
// closing a live hole — it applies the same rule commentLines applies to the
// driver's detail strings, so "nothing interpolated into the banner can end a
// comment" is a property of renderSnapshot rather than a case-by-case argument.
// It also keeps bannerLines honest: a multi-line label would make the count
// wrong and push snapshotRange into the banner.
func singleLine(text string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(text)
}

// commentLines prefixes every line with "-- ". The detail strings it renders
// come from the driver, so a newline in one must not be able to end the comment
// and leave text that reads like SQL.
func commentLines(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		b.WriteString("-- ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// renderSnapshot builds the file content and reports how many leading lines are
// banner, so the range computation can skip them.
func renderSnapshot(target snapshotTarget, sc snapshotContext, now time.Time, body, note string) (string, int) {
	var b strings.Builder
	fmt.Fprintf(&b, "-- sqls: read-only snapshot of InterBase %s %q\n",
		strings.ToUpper(string(target.kind)), target.name)
	fmt.Fprintf(&b, "-- connection: %s    generated: %s\n",
		singleLine(sc.label), now.UTC().Format(time.RFC3339))
	b.WriteString("-- Editing this file does not change the database.\n")
	bannerLines := 3

	if note != "" {
		commented := commentLines(note)
		b.WriteString(commented)
		bannerLines += strings.Count(commented, "\n")
	}

	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteByte('\n')
	}
	return b.String(), bannerLines
}

// snapshotRange is the zero-width range at the first occurrence of the object
// name in the body. The banner is skipped so the name in the banner never wins.
// It is (0,0) when the name does not appear, which happens when the generated
// DDL quotes the name differently than the catalog spells it.
func snapshotRange(content string, bannerLines int, name string) lsp.Range {
	lines := strings.Split(content, "\n")
	for i := bannerLines; i < len(lines); i++ {
		offset := strings.Index(lines[i], name)
		if offset < 0 {
			continue
		}
		// LSP character offsets are UTF-16 code units, not bytes or runes.
		pos := lsp.Position{Line: i, Character: utf16Len(lines[i][:offset])}
		return lsp.Range{Start: pos, End: pos}
	}
	return lsp.Range{}
}

// definitionDDLTimeout bounds the catalog round trip. textDocument/definition
// stays on the inline dispatch path — Plan 1 moves only workspace/executeCommand
// off it — so an unbounded catalog read would freeze the whole server. The
// bound matches the one hover uses for the same reason.
const definitionDDLTimeout = 3 * time.Second

// interBaseDefinition materialises the source of the catalog object under the
// cursor and returns its location. Every miss returns (nil, nil): the user
// asked to navigate, not to be told about the catalog, so nothing here surfaces
// as a request error.
//
// repo and dbCache are parameters rather than reads off the server so this
// method takes stateMu exactly once, for snapshotContext, and never across the
// file write that follows.
func (s *Server) interBaseDefinition(ctx context.Context, repo database.DBRepository, dbCache *database.DBCache, params lsp.DefinitionParams, text string) (lsp.Definition, error) {
	return s.interBaseDefinitionWithVariant(ctx, repo, dbCache, params, text, s.parserDriverVariant())
}

func (s *Server) interBaseDefinitionWithVariant(ctx context.Context, repo database.DBRepository, dbCache *database.DBCache, params lsp.DefinitionParams, text string, dv dialect.DriverVariant) (lsp.Definition, error) {
	if s.snapshots == nil || repo == nil {
		return nil, nil
	}
	ddlRepo, ok := repo.(database.DDLRepository)
	if !ok {
		return nil, nil
	}
	target, ok := resolveSnapshotTargetWithVariant(text, params, dbCache, dv)
	if !ok {
		return nil, nil
	}

	ddlCtx, cancel := context.WithTimeout(ctx, definitionDDLTimeout)
	defer cancel()
	ddl, ddlErr := ddlRepo.ObjectDDL(ddlCtx, target.kind, target.name)

	body, note, ok := snapshotBodyFor(ddl, ddlErr, target)
	if !ok {
		return nil, nil
	}

	sc := s.snapshotContext()
	content, bannerLines := renderSnapshot(target, sc, s.snapshots.now(), body, note)
	path, err := s.snapshots.write(sc, string(target.kind), target.name, content)
	if err != nil {
		log.Printf("sqls: write %s snapshot for %q: %v", target.kind, target.name, err)
		return nil, nil
	}

	return []lsp.Location{{
		URI:   snapshotURI(path),
		Range: snapshotRange(content, bannerLines, target.name),
	}}, nil
}

// snapshotURI turns an absolute path into a file:// URI. The path can contain
// percent signs, because escapeSnapshotName puts them there; url.URL.String
// encodes them as %25, so the URI decodes back to the real file name.
func snapshotURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}
