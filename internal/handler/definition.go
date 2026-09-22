package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sourcegraph/jsonrpc2"
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

func (s *Server) handleDefinition(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams}
	}

	var params lsp.DefinitionParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	text, ok := s.fileText(params.TextDocument.URI)
	if !ok {
		return nil, fmt.Errorf("document not found: %s", params.TextDocument.URI)
	}

	dv := s.parserDriverVariant()
	var analysis *sqlsymbol.Analysis
	var offset int
	if dv.Driver == dialect.DatabaseDriverInterBase {
		var valid bool
		offset, valid = symbolOffset(text, params.Position)
		if !valid {
			return []lsp.Location{}, nil
		}
		analysis, err = sqlsymbol.Analyze(text, dv)
		if err != nil {
			return nil, err
		}
	}
	var local lsp.Definition
	var handled bool
	if analysis != nil {
		local, handled, err = localDefinitionWithAnalysis(params.TextDocument.URI, text, offset, analysis)
	} else {
		local, handled, err = localDefinition(params.TextDocument.URI, text, params.Position, dv)
	}
	if err != nil {
		return nil, err
	}
	if handled {
		return local, nil
	}
	var contextual bool
	if analysis != nil {
		contextual, err = contextualSQLTargetWithAnalysis(text, params.Position, analysis)
	} else {
		contextual, err = contextualSQLTarget(text, params.Position, dv)
	}
	if err != nil {
		return nil, err
	}
	if contextual {
		dbCache := s.worker.Cache()
		repo, err := s.newDBRepository(ctx)
		if err != nil {
			return nil, nil
		}
		return s.interBaseContextualDefinitionWithAnalysis(ctx, repo, dbCache, text, params.Position, dv, analysis)
	}

	dbCache := s.worker.Cache()
	res, err := definitionWithDriverVariant(params.TextDocument.URI, text, params, dbCache, dv)
	if err != nil {
		return nil, err
	}
	// In-document aliases and subqueries win outright, for every driver.
	if len(res) > 0 {
		return res, nil
	}

	// Not having a repository is not a definition failure: for every driver
	// without a catalog, the alias path above is the whole feature.
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return nil, nil
	}
	return s.interBaseDefinition(ctx, repo, dbCache, params, text)
}

func definition(url, text string, params lsp.DefinitionParams, dbCache *database.DBCache) (lsp.Definition, error) {
	return definitionWithDriver(url, text, params, dbCache, "")
}

func definitionWithDriver(url, text string, params lsp.DefinitionParams, dbCache *database.DBCache, driver dialect.DatabaseDriver) (lsp.Definition, error) {
	return definitionWithDriverVariant(url, text, params, dbCache, dialect.DriverVariant{Driver: driver})
}

func definitionWithDriverVariant(url, text string, params lsp.DefinitionParams, dbCache *database.DBCache, dv dialect.DriverVariant) (lsp.Definition, error) {
	pos := token.Pos{
		Line: params.Position.Line,
		Col:  params.Position.Character + 1,
	}
	parsed, err := parser.ParseWithDriverVariant(text, dv)
	if err != nil {
		return nil, err
	}

	nodeWalker := parseutil.NewNodeWalker(parsed, pos)
	m := astutil.NodeMatcher{
		NodeTypes: []ast.NodeType{ast.TypeIdentifier},
	}
	currentVariable := nodeWalker.CurNodeBottomMatched(m)
	if currentVariable == nil {
		return nil, nil
	}

	aliases := parseutil.ExtractAliased(parsed)
	if len(aliases) == 0 {
		return nil, nil
	}

	var define ast.Node
	for _, v := range aliases {
		alias, _ := v.(*ast.Aliased)
		if alias.AliasedName.String() == currentVariable.String() {
			define = alias.AliasedName
			break
		}
	}

	if define == nil {
		return nil, nil
	}

	res := []lsp.Location{
		{
			URI: url,
			Range: lsp.Range{
				Start: lsp.Position{
					Line:      define.Pos().Line,
					Character: define.Pos().Col,
				},
				End: lsp.Position{
					Line:      define.End().Line,
					Character: define.End().Col,
				},
			},
		},
	}

	return res, nil
}
