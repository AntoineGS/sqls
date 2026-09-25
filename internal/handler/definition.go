package handler

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/ast/astutil"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
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

	snapshot, err := s.captureEditorSnapshot(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	request := languageRequest(snapshot)
	detailCtx, cancel := context.WithTimeout(ctx, definitionDDLTimeout)
	defer cancel()
	languageResult, serviceErr := language.NewService().Definition(detailCtx, request, languagePosition(params.Position))
	if ctx.Err() != nil {
		return nil, language.ErrCanceled
	}
	if serviceErr != nil {
		// Definition is a navigation affordance. Detail transport/cache failures
		// must not become JSON-RPC errors or stale source publications.
		if errors.Is(serviceErr, language.ErrCanceled) && ctx.Err() != nil {
			return nil, language.ErrCanceled
		}
		return []lsp.Location{}, nil
	}
	if err := s.navigationResultCurrent(ctx, snapshot, languageResult.Stamp); err != nil {
		if errors.Is(err, language.ErrCanceled) && ctx.Err() != nil {
			return nil, language.ErrCanceled
		}
		return []lsp.Location{}, nil
	}
	locations := make([]lsp.Location, 0, len(languageResult.Value))
	for _, location := range languageResult.Value {
		if location.Source == nil {
			locations = append(locations, lsp.Location{URI: location.URI, Range: lspRange(location.Range)})
			continue
		}
		if s.snapshots == nil {
			return []lsp.Location{}, nil
		}
		source := location.Source
		target := snapshotTarget{kind: database.ObjectKind(source.Object.Kind), name: source.Object.Name, provenance: source.Provenance}
		content, bannerLines := renderSnapshot(target, snapshot.SourceContext, s.snapshots.now(), source.Text, source.Note)
		path, published, writeErr := s.publishDefinitionCandidate(ctx, snapshot, languageResult.Stamp, string(target.kind), target.name, content)
		if writeErr != nil || !published {
			if ctx.Err() != nil {
				return nil, language.ErrCanceled
			}
			return []lsp.Location{}, nil
		}
		rng := location.Range
		rng.Start.Line += bannerLines
		rng.End.Line += bannerLines
		locations = append(locations, lsp.Location{URI: snapshotURI(path), Range: lspRange(rng)})
	}
	return locations, nil
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
