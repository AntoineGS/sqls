package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

// resolveRelationTarget maps a proven SQL relation/column reference to the
// catalog object that owns its declaration. Each scope is considered in
// nearest-first order; a binding shadows outer bindings even if its metadata
// cannot be resolved.
func resolveRelationTarget(ref sqlsymbol.SQLReference, role sqlsymbol.Role, cache *database.DBCache) (snapshotTarget, bool) {
	if cache == nil {
		return snapshotTarget{}, false
	}
	if role == sqlsymbol.Relation {
		return relationTarget(ref.Name, cache)
	}
	if role != sqlsymbol.Column {
		return snapshotTarget{}, false
	}
	if ref.Qualifier != nil {
		for _, scope := range ref.Scopes {
			var matches []sqlsymbol.RelationRef
			for _, binding := range scope {
				matched := false
				if binding.Alias != nil {
					matched = binding.Alias.Key() == ref.Qualifier.Key()
				} else {
					matched = binding.Name.Key() == ref.Qualifier.Key()
				}
				if matched {
					matches = append(matches, binding)
				}
			}
			if len(matches) > 0 {
				if len(matches) != 1 {
					return snapshotTarget{}, false
				}
				target, columns, metadata := relationColumns(matches[0].Name, cache)
				if !metadata {
					return snapshotTarget{}, false
				}
				for _, column := range columns {
					if ref.Name.MatchesCatalogName(column) {
						name := ref.Name
						target.column = &name
						return target, true
					}
				}
				return snapshotTarget{}, false
			}
		}
		return snapshotTarget{}, false
	}
	for _, scope := range ref.Scopes {
		if len(scope) == 0 {
			continue
		}
		var owner snapshotTarget
		matches := 0
		for _, binding := range scope {
			target, columns, metadata := relationColumns(binding.Name, cache)
			if !metadata {
				return snapshotTarget{}, false
			}
			found := false
			for _, column := range columns {
				if ref.Name.MatchesCatalogName(column) {
					found = true
					break
				}
			}
			if found {
				matches++
				owner = target
			}
		}
		if matches == 1 {
			column := ref.Name
			owner.column = &column
			return owner, true
		}
		if matches > 1 {
			return snapshotTarget{}, false
		}
		// If any binding is derived or lacks metadata, relationColumns above
		// already rejected the scope rather than falling through outward.
	}
	return snapshotTarget{}, false
}

func relationTarget(name sqlsymbol.Name, cache *database.DBCache) (snapshotTarget, bool) {
	for _, table := range cache.SortedTables() {
		if name.MatchesCatalogName(table) {
			return snapshotTarget{kind: database.ObjectKindTable, name: table}, true
		}
	}
	// Basic table caches may have tables indexed by schema rather than the
	// default-schema accessor; relation identity remains catalog spelling.
	for _, tables := range cache.SchemaTables {
		for _, table := range tables {
			if name.MatchesCatalogName(table) {
				return snapshotTarget{kind: database.ObjectKindTable, name: table}, true
			}
		}
	}
	for _, viewName := range cache.SortedViews() {
		if name.MatchesCatalogName(viewName) {
			view, _ := cache.View(viewName)
			return snapshotTarget{kind: database.ObjectKindView, name: view.Name, source: view.ViewSource}, true
		}
	}
	return snapshotTarget{}, false
}

// relationColumns reports whether catalog metadata for this candidate exists.
// A derived/unknown relation or absent column descriptor is not proof of no
// column: it makes ownership ambiguous and blocks resolution.
func relationColumns(name sqlsymbol.Name, cache *database.DBCache) (snapshotTarget, []string, bool) {
	target, ok := relationTarget(name, cache)
	if !ok {
		return snapshotTarget{}, nil, false
	}
	if target.kind == database.ObjectKindView {
		view, ok := cache.View(target.name)
		if !ok || view.Columns == nil {
			return target, nil, false
		}
		cols := make([]string, 0, len(view.Columns))
		for _, col := range view.Columns {
			if col != nil {
				cols = append(cols, col.Name)
			}
		}
		return target, cols, true
	}
	for _, descriptors := range cache.ColumnsWithParent {
		var cols []string
		matched := false
		for _, col := range descriptors {
			if col != nil && col.Table == target.name {
				matched = true
				cols = append(cols, col.Name)
			}
		}
		if matched {
			return target, cols, true
		}
	}
	return target, nil, false
}

// interBaseRelationDefinition resolves a contextual SQL symbol and materialises
// its table/view DDL snapshot. It deliberately never falls through to generic
// name-based matching when ownership is unavailable or ambiguous.
func (s *Server) interBaseRelationDefinition(ctx context.Context, repo database.DBRepository, cache *database.DBCache, text string, pos lsp.Position, dv dialect.DriverVariant) (lsp.Definition, error) {
	if s.snapshots == nil || repo == nil {
		return nil, nil
	}
	if _, ok := repo.(database.DDLRepository); !ok {
		return nil, nil
	}
	analysis, err := sqlsymbol.Analyze(text, dv)
	if err != nil {
		return nil, err
	}
	return s.interBaseRelationDefinitionWithAnalysis(ctx, repo, cache, text, pos, dv, analysis)
}

func (s *Server) interBaseRelationDefinitionWithAnalysis(ctx context.Context, repo database.DBRepository, cache *database.DBCache, text string, pos lsp.Position, dv dialect.DriverVariant, analysis *sqlsymbol.Analysis) (lsp.Definition, error) {
	if s.snapshots == nil || repo == nil {
		return nil, nil
	}
	ddlRepo, ok := repo.(database.DDLRepository)
	if !ok {
		return nil, nil
	}
	offset, ok := symbolOffset(text, pos)
	if !ok {
		return nil, nil
	}
	resolution := analysis.Resolve(offset)
	if resolution.SQL == nil {
		return nil, nil
	}
	target, ok := resolveRelationTarget(*resolution.SQL, resolution.Role, cache)
	if !ok {
		return nil, nil
	}
	ddlCtx, cancel := context.WithTimeout(ctx, definitionDDLTimeout)
	defer cancel()
	ddl, ddlErr := ddlRepo.ObjectDDL(ddlCtx, target.kind, target.name)
	var body, note string
	var descriptionSpan *sqlsymbol.Span
	if target.kind == database.ObjectKindTable && errors.Is(ddlErr, database.ErrUnsupportedDDL) {
		var ok bool
		body, note, descriptionSpan, ok = unsupportedTableDescription(ddlCtx, repo, target, ddlErr)
		if !ok {
			return nil, nil
		}
	} else {
		var ok bool
		body, note, ok = snapshotBodyFor(ddl, ddlErr, target)
		if !ok {
			return nil, nil
		}
	}
	var span sqlsymbol.Span
	if descriptionSpan != nil {
		span = *descriptionSpan
		ok = true
	} else if target.column != nil {
		span, ok = sqlsymbol.ColumnDeclaration(body, sqlsymbol.Name{Text: target.name, Quoted: true}, *target.column)
	} else {
		span, ok = sqlsymbol.TableDeclaration(body, sqlsymbol.Name{Text: target.name, Quoted: true})
	}
	if !ok {
		return nil, nil
	}
	sc := s.snapshotContext()
	content, bannerLines := renderSnapshot(target, sc, s.snapshots.now(), body, note)
	path, err := s.snapshots.write(sc, string(target.kind), target.name, content)
	if err != nil {
		return nil, nil
	}
	rangeValue, ok := symbolRange(content, sqlsymbol.Span{Start: span.Start + snapshotBodyOffset(content, bannerLines), End: span.End + snapshotBodyOffset(content, bannerLines)})
	if !ok {
		return nil, nil
	}
	return []lsp.Location{{URI: snapshotURI(path), Range: rangeValue}}, nil
}

func unsupportedTableDescription(ctx context.Context, repo database.DBRepository, target snapshotTarget, ddlErr error) (body, note string, span *sqlsymbol.Span, ok bool) {
	source, ok := repo.(database.TableDescriptionRepository)
	if !ok {
		return "", "", nil, false
	}
	description, err := source.TableDescription(ctx, target.name)
	if err != nil || description.Body == "" || len(description.Columns) == 0 || !commentOnlyDescription(description.Body) {
		return "", "", nil, false
	}
	tableSpan, ok := catalogDescriptionSpan(description.Body, description.Table, target.name)
	if !ok {
		return "", "", nil, false
	}
	selected := tableSpan
	if target.column != nil {
		matches := 0
		for _, column := range description.Columns {
			if !target.column.MatchesCatalogName(column.Name) {
				continue
			}
			matches++
			selected, ok = catalogDescriptionSpan(description.Body, column.Span, column.Name)
			if !ok {
				return "", "", nil, false
			}
		}
		if matches != 1 {
			return "", "", nil, false
		}
	}
	return description.Body, snapshotUnsupportedTableDDLNote(ddlErr, target.name), &selected, true
}

func catalogDescriptionSpan(body string, span database.DescriptionSpan, name string) (sqlsymbol.Span, bool) {
	if span.Start < 0 || span.End < span.Start || span.End > len(body) {
		return sqlsymbol.Span{}, false
	}
	want := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	if body[span.Start:span.End] != want {
		return sqlsymbol.Span{}, false
	}
	return sqlsymbol.Span{Start: span.Start, End: span.End}, true
}

func commentOnlyDescription(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "--") {
			return false
		}
	}
	return body != ""
}

func snapshotUnsupportedTableDDLNote(err error, table string) string {
	if object, name, feature, ok := database.UnsupportedDDLDetail(err); ok && object != "" && name != "" && feature != "" {
		return fmt.Sprintf("Strict executable DDL could not be reproduced: %s %q: %s.\nThe following comment-only catalog description is informational and not executable SQL.", object, name, feature)
	}
	return fmt.Sprintf("Strict executable DDL could not be reproduced for table %q.\nThe following comment-only catalog description is informational and not executable SQL.", table)
}

// interBaseContextualDefinition preserves the existing view snapshot behavior
// for relation names: when executable DDL is unavailable, a view's verbatim
// catalog source remains a useful definition target. Column references still
// require a proven declaration span in generated DDL.
func (s *Server) interBaseContextualDefinition(ctx context.Context, repo database.DBRepository, cache *database.DBCache, text string, pos lsp.Position, dv dialect.DriverVariant) (lsp.Definition, error) {
	if s.snapshots == nil || repo == nil {
		return nil, nil
	}
	if _, ok := repo.(database.DDLRepository); !ok {
		return nil, nil
	}
	analysis, err := sqlsymbol.Analyze(text, dv)
	if err != nil {
		return nil, err
	}
	return s.interBaseContextualDefinitionWithAnalysis(ctx, repo, cache, text, pos, dv, analysis)
}

func (s *Server) interBaseContextualDefinitionWithAnalysis(ctx context.Context, repo database.DBRepository, cache *database.DBCache, text string, pos lsp.Position, dv dialect.DriverVariant, analysis *sqlsymbol.Analysis) (lsp.Definition, error) {
	params := lsp.DefinitionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{Position: pos}}
	target, ok := resolveSnapshotTargetWithAnalysis(text, params, cache, dv, analysis)
	if ok && target.kind == database.ObjectKindView && target.column == nil {
		return s.interBaseDefinitionWithVariant(ctx, repo, cache, params, text, dv)
	}
	return s.interBaseRelationDefinitionWithAnalysis(ctx, repo, cache, text, pos, dv, analysis)
}

func snapshotBodyOffset(content string, bannerLines int) int {
	line := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			line++
			if line == bannerLines {
				return i + 1
			}
		}
	}
	return len(content)
}
