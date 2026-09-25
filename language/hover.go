package language

import (
	"context"
	"errors"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/ast/astutil"
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/parser"
	"github.com/sqls-server/sqls/parser/parseutil"
	"github.com/sqls-server/sqls/token"
)

// ErrNoHover reports that no hover-worthy identifier is under position. It
// is the cache-only counterpart of a host's own live-augmentation failure;
// a host distinguishes the two only if it needs to.
var ErrNoHover = errNoHover{}

type errNoHover struct{}

func (errNoHover) Error() string { return "language: no hover information found" }

// Hover returns cache-derived hover information, augmented with database
// source when the selected relation has a supported detail. Detail loading is
// always through the request's shared metadata lease.
func (s *Service) Hover(ctx context.Context, req Request, position Position) (Result[Hover], error) {
	result, cacheErr := s.hoverCached(ctx, req, position)
	if ctx.Err() != nil {
		return Result[Hover]{}, ErrCanceled
	}
	target, selectedRange, ok, err := relationAt(req, position)
	if err != nil {
		return Result[Hover]{}, err
	}
	if !ok || req.Context.Details == nil {
		if cacheErr != nil {
			return Result[Hover]{}, cacheErr
		}
		return result, nil
	}
	detail, err := req.Context.Details.Detail(ctx, metadata.Request{Object: target, Facet: "ddl", Dialect: req.Context.Variant.Variant.InterBaseSQLDialect(), Consistency: metadata.Committed})
	if err != nil {
		if ctx.Err() != nil {
			return Result[Hover]{}, ErrCanceled
		}
		if errors.Is(err, metadata.ErrUnsupported) {
			if cacheErr != nil {
				return Result[Hover]{}, cacheErr
			}
			return result, nil
		}
		return Result[Hover]{}, err
	}
	if ctx.Err() != nil {
		return Result[Hover]{}, ErrCanceled
	}
	markdown := "```sql\n" + detail.Text + "\n```"
	if cacheErr == nil && result.Value.Markdown != "" {
		markdown = result.Value.Markdown + "\n\n" + markdown
	}
	return Result[Hover]{Value: Hover{Markdown: markdown, Range: &selectedRange}, Stamp: stampFor(req)}, nil
}

func (s *Service) hoverCached(ctx context.Context, req Request, position Position) (Result[Hover], error) {
	if err := ctx.Err(); err != nil {
		return Result[Hover]{}, ErrCanceled
	}

	var cache *catalog.Cache
	if req.Context.Snapshot != nil {
		cache = req.Context.Snapshot.Cache
	}
	if cache == nil {
		return Result[Hover]{}, ErrNoHover
	}

	text := req.Document.Text
	pos := token.Pos{Line: position.Line, Col: position.UTF16Column + 1}
	parsed, err := parser.ParseWithDriverVariant(text, req.Context.Variant)
	if err != nil {
		return Result[Hover]{}, err
	}

	nodeWalker := parseutil.NewNodeWalker(parsed, pos)
	hoverTargetMatcher := astutil.NodeMatcher{
		NodeTypes: []ast.NodeType{
			ast.TypeMemberIdentifier,
			ast.TypeIdentifier,
		},
	}
	focusedIdentNodes := nodeWalker.CurNodeMatches(hoverTargetMatcher)
	if len(focusedIdentNodes) == 0 {
		return Result[Hover]{}, ErrNoHover
	}
	ident, memIdent := findIdent(focusedIdentNodes)

	hoverEnv, err := collectHoverEnvironment(parsed, pos)
	if err != nil {
		return Result[Hover]{}, err
	}

	hctx := getHoverTypes(nodeWalker, hoverEnv)

	var hoverContent string
	var hasContent bool
	switch {
	case ident != nil && memIdent != nil:
		identName := ident.NoQuoteString()
		parentName := memIdent.ParentTok.NoQuoteString()
		childName := memIdent.ChildTok.NoQuoteString()
		switch identName {
		case parentName:
			hoverContent, hasContent = hoverContentFromParentIdent(hctx, identName, cache, hoverEnv)
		case childName:
			hoverContent, hasContent = hoverContentFromChildIdent(hctx, identName, cache, hoverEnv)
		}
	case ident == nil && memIdent != nil:
		hoverContent, hasContent = hoverContentFromChildIdent(hctx, memIdent.ChildTok.NoQuoteString(), cache, hoverEnv)
	case ident != nil && memIdent == nil:
		hoverContent, hasContent = hoverContentFromIdent(hctx, ident.NoQuoteString(), cache, hoverEnv)
	}
	if !hasContent {
		return Result[Hover]{}, ErrNoHover
	}

	var posIdent ast.Node = ident
	if ident == nil && memIdent != nil {
		posIdent = memIdent
	}
	res := Hover{
		Markdown: hoverContent,
		Range: &Range{
			Start: Position{Line: posIdent.Pos().Line, UTF16Column: posIdent.Pos().Col},
			End:   Position{Line: posIdent.End().Line, UTF16Column: posIdent.End().Col},
		},
	}
	return Result[Hover]{Value: res, Stamp: stampFor(req)}, nil
}

type hoverEnvironment struct {
	aliases    []ast.Node
	tables     []*parseutil.TableInfo
	subQueries []*parseutil.SubQueryInfo
}

func (e *hoverEnvironment) getTableRealName(aliasName string) (string, bool) {
	for _, table := range e.tables {
		if table.Alias == aliasName {
			return table.Name, true
		}
	}
	return "", false
}

func (e *hoverEnvironment) getColumnRealName(aliasedName string) (string, bool) {
	for _, v := range e.aliases {
		alias, _ := v.(*ast.Aliased)

		if alias.AliasedName.String() == aliasedName {
			switch v := alias.RealName.(type) {
			case *ast.Identifier:
				return v.String(), true
			case *ast.MemberIdentifier:
				return v.Child.String(), true
			}
		}
	}
	return "", false
}

func (e *hoverEnvironment) getSubQueryView(name string) (*parseutil.SubQueryInfo, bool) {
	for _, subQuery := range e.subQueries {
		if subQuery.Name == name {
			return subQuery, true
		}
	}
	return nil, false
}

func (e *hoverEnvironment) getSubQueryViewOne() (*parseutil.SubQueryInfo, bool) {
	if len(e.subQueries) == 1 {
		return e.subQueries[0], true
	}
	return nil, false
}

func (e *hoverEnvironment) isSubQuery(name string) bool {
	_, ok := e.getSubQueryView(name)
	return ok
}

func collectHoverEnvironment(parsed ast.TokenList, pos token.Pos) (*hoverEnvironment, error) {
	aliases := parseutil.ExtractAliasedIdentifier(parsed)
	definedTables, err := parseutil.ExtractTable(parsed, pos)
	if err != nil {
		return nil, err
	}
	subQueries, err := parseutil.ExtractSubQueryViews(parsed, pos)
	if err != nil {
		return nil, err
	}
	environment := &hoverEnvironment{
		aliases:    aliases,
		tables:     definedTables,
		subQueries: subQueries,
	}
	return environment, nil
}

func findIdent(nodes []ast.Node) (*ast.Identifier, *ast.MemberIdentifier) {
	var (
		ident    *ast.Identifier
		memIdent *ast.MemberIdentifier
	)
	for _, node := range nodes {
		switch v := node.(type) {
		case *ast.Identifier:
			ident = v
		case *ast.MemberIdentifier:
			memIdent = v
		}
	}
	return ident, memIdent
}

func hoverContentFromIdent(ctx *hoverContext, identName string, cache *catalog.Cache, hoverEnv *hoverEnvironment) (string, bool) {
	if hoverTypeIs(ctx.types, hoverTypeColumn) {
		columnName := identName
		if realName, ok := hoverEnv.getColumnRealName(columnName); ok {
			columnName = realName
		}
		var hoverContents []string
		for _, table := range hoverEnv.tables {
			colDesc, ok := cache.Column(table.Name, columnName)
			if ok {
				hoverContents = append(hoverContents, columnHoverInfo(table.Name, columnName, colDesc))
			}
		}
		if len(hoverContents) >= 2 {
			return "", false
		}
		if len(hoverContents) == 1 {
			return hoverContents[0], true
		}
	}
	if hoverTypeIs(ctx.types, hoverTypeTable) {
		tableName := identName
		for _, table := range hoverEnv.tables {
			if table.Alias == tableName {
				tableName = table.Name
			}
		}
		cols, ok := cache.ColumnDescs(tableName)
		if ok {
			// InterBase can publish relation columns before its view category
			// settles. In that interval a view is also present in the relation
			// column map, so columns alone cannot prove this is a table. Legacy
			// caches without explicit view status retain their prior behavior.
			if _, tracked := cache.Metadata[catalog.MetadataViews]; tracked && !cache.MetadataReady(catalog.MetadataViews) {
				return "", false
			}
			return tableHoverInfo(tableName, cols), true
		}
	}
	if hoverTypeIs(ctx.types, hoverTypeSubQueryColumn) {
		columnName := identName
		subQueryView, ok := hoverEnv.getSubQueryViewOne()
		if !ok {
			return "", false
		}
		return subqueryColumnHoverInfo(columnName, subQueryView, cache), true
	}
	return "", false
}

func hoverContentFromParentIdent(ctx *hoverContext, identName string, cache *catalog.Cache, hoverEnv *hoverEnvironment) (string, bool) {
	switch ctx.parent.Type {
	case parentTypeNone:
		return "", false
	case parentTypeSchema:
	case parentTypeTable:
		tableName := identName
		realName, ok := hoverEnv.getTableRealName(tableName)
		if ok {
			tableName = realName
		}
		columns, ok := cache.ColumnDescs(tableName)
		if ok {
			return tableHoverInfo(tableName, columns), true
		}
	case parentTypeSubQuery:
		subQueryName := identName
		subQueryView, ok := hoverEnv.getSubQueryView(subQueryName)
		if !ok {
			return "", false
		}
		return subqueryHoverInfo(subQueryView, cache), true
	}
	return "", false
}

func hoverContentFromChildIdent(ctx *hoverContext, identName string, cache *catalog.Cache, hoverEnv *hoverEnvironment) (string, bool) {
	switch ctx.parent.Type {
	case parentTypeNone:
		return "", false
	case parentTypeSchema:
		columns, ok := cache.ColumnDescs(identName)
		if ok {
			return tableHoverInfo(identName, columns), true
		}
	case parentTypeTable:
		tableName := ctx.parent.Name
		realName, ok := hoverEnv.getTableRealName(tableName)
		if ok {
			tableName = realName
		}
		if colDesc, ok := cache.Column(tableName, identName); ok {
			return columnHoverInfo(tableName, identName, colDesc), true
		}
		return "", false
	case parentTypeSubQuery:
		subQueryView, ok := hoverEnv.getSubQueryView(ctx.parent.Name)
		if !ok {
			return "", false
		}
		return subqueryColumnHoverInfo(identName, subQueryView, cache), true
	}
	return "", false
}

func columnHoverInfo(tableName, colName string, colDesc *catalog.ColumnDesc) string {
	return catalog.ColumnDoc(tableName, colDesc)
}

func tableHoverInfo(tableName string, cols []*catalog.ColumnDesc) string {
	return catalog.TableDoc(tableName, cols)
}

func subqueryHoverInfo(subQuery *parseutil.SubQueryInfo, cache *catalog.Cache) string {
	return catalog.SubqueryDoc(subQuery.Name, subQuery.Views, cache)
}

func subqueryColumnHoverInfo(identName string, subQuery *parseutil.SubQueryInfo, cache *catalog.Cache) string {
	return catalog.SubqueryColumnDoc(identName, subQuery.Views, cache)
}

func hoverTypeIs(hoverTypes []hoverType, expect hoverType) bool {
	for _, t := range hoverTypes {
		if t == expect {
			return true
		}
	}
	return false
}

type hoverType int

const (
	_ hoverType = iota
	hoverTypeKeyword
	hoverTypeFunction
	hoverTypeAlias
	hoverTypeColumn
	hoverTypeTable
	hoverTypeView
	hoverTypeSubQueryView
	hoverTypeSubQueryColumn
	hoverTypeChange
	hoverTypeUser
	hoverTypeSchema
)

type parentType int

const (
	parentTypeNone parentType = iota
	parentTypeSchema
	parentTypeTable
	parentTypeSubQuery
)

type hoverParent struct {
	Type parentType
	Name string
}

var noneHoverParent = &hoverParent{Type: parentTypeNone}

type hoverContext struct {
	types  []hoverType
	parent *hoverParent
}

func getHoverTypes(nw *parseutil.NodeWalker, hoverEnv *hoverEnvironment) *hoverContext {
	memberIdentifierMatcher := astutil.NodeMatcher{
		NodeTypes: []ast.NodeType{ast.TypeMemberIdentifier},
	}
	syntaxPos := parseutil.CheckSyntaxPosition(nw)

	t := []hoverType{hoverTypeKeyword}
	p := noneHoverParent
	switch {
	case syntaxPos == parseutil.ColName:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifier)
			t = []hoverType{
				hoverTypeColumn,
				hoverTypeSubQueryColumn,
				hoverTypeView,
				hoverTypeFunction,
			}
			name := mi.Parent.String()
			p = &hoverParent{
				Type: parentTypeTable,
				Name: name,
			}
			if hoverEnv.isSubQuery(name) {
				p = &hoverParent{
					Type: parentTypeSubQuery,
					Name: name,
				}
			}
		} else {
			t = []hoverType{
				hoverTypeColumn,
				hoverTypeTable,
				hoverTypeSubQueryColumn,
				hoverTypeSubQueryView,
				hoverTypeAlias,
				hoverTypeView,
				hoverTypeFunction,
				hoverTypeKeyword,
			}
		}
	case syntaxPos == parseutil.AliasName:
		// pass
	case syntaxPos == parseutil.SelectExpr || syntaxPos == parseutil.CaseValue:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifier)
			t = []hoverType{
				hoverTypeColumn,
				hoverTypeView,
				hoverTypeSubQueryColumn,
				hoverTypeFunction,
			}
			name := mi.Parent.String()
			p = &hoverParent{
				Type: parentTypeTable,
				Name: name,
			}
			if hoverEnv.isSubQuery(name) {
				p = &hoverParent{
					Type: parentTypeSubQuery,
					Name: name,
				}
			}
		} else {
			t = []hoverType{
				hoverTypeColumn,
				hoverTypeTable,
				hoverTypeAlias,
				hoverTypeView,
				hoverTypeSubQueryColumn,
				hoverTypeSubQueryView,
				hoverTypeFunction,
				hoverTypeKeyword,
			}
		}
	case syntaxPos == parseutil.TableReference:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifier)
			t = []hoverType{
				hoverTypeTable,
				hoverTypeView,
				hoverTypeSubQueryColumn,
				hoverTypeFunction,
			}
			p = &hoverParent{
				Type: parentTypeSchema,
				Name: mi.Parent.String(),
			}
		} else {
			t = []hoverType{
				hoverTypeColumn,
				hoverTypeTable,
				hoverTypeSchema,
				hoverTypeView,
				hoverTypeSubQueryColumn,
				hoverTypeSubQueryView,
				hoverTypeFunction,
				hoverTypeKeyword,
			}
		}
	case syntaxPos == parseutil.WhereCondition:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifier)
			t = []hoverType{
				hoverTypeTable,
				hoverTypeView,
				hoverTypeSubQueryColumn,
				hoverTypeFunction,
			}
			p = &hoverParent{
				Type: parentTypeTable,
				Name: mi.Parent.String(),
			}
		} else {
			t = []hoverType{
				hoverTypeColumn,
				hoverTypeTable,
				hoverTypeSchema,
				hoverTypeView,
				hoverTypeSubQueryColumn,
				hoverTypeSubQueryView,
				hoverTypeFunction,
				hoverTypeKeyword,
			}
		}
	case syntaxPos == parseutil.InsertColumn:
		t = []hoverType{
			hoverTypeColumn,
			hoverTypeTable,
			hoverTypeView,
		}
	default:
		// pass
	}
	return &hoverContext{
		types:  t,
		parent: p,
	}
}
