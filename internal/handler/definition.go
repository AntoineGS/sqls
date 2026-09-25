package handler

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
)

func (s *Server) handleDefinition(ctx context.Context, _ *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
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
