package language

import (
	"context"
	"errors"
	"strings"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
	"github.com/sqls-server/sqls/metadata"
)

type navigationDetail struct {
	source     *Source
	range_     Range
	executable bool
	// unsupported is true only when a typed DDL refusal was returned. Hosts
	// may explain that executable DDL is unavailable without labeling fallback
	// source or descriptions as executable.
	unsupported bool
}

func (s *Service) loadNavigationDetail(ctx context.Context, req Request, object catalog.ObjectID, position Position) (navigationDetail, bool, error) {
	if req.Context.Details == nil {
		return navigationDetail{}, false, nil
	}
	request := metadata.Request{Object: object, Facet: "ddl", Dialect: req.Context.Variant.Variant.InterBaseSQLDialect(), Consistency: metadata.Committed}
	detail, err := req.Context.Details.Detail(ctx, request)
	if err == nil {
		if ctx.Err() != nil {
			return navigationDetail{}, false, ErrCanceled
		}
		if detail.Object != object || detail.Text == "" {
			return navigationDetail{}, false, nil
		}
		rng, ok := detailBodyRange(detail.Text, object, selectedNavigationColumn(req, object, position))
		if !ok {
			return navigationDetail{}, false, nil
		}
		return navigationDetail{source: &Source{Object: detail.Object, Text: detail.Text, Provenance: detail.Provenance}, range_: rng, executable: true}, true, nil
	}
	if ctx.Err() != nil {
		return navigationDetail{}, false, ErrCanceled
	}
	if errors.Is(err, database.ErrObjectNotFound) {
		return navigationDetail{}, false, nil
	}
	if !errors.Is(err, database.ErrUnsupportedDDL) {
		if errors.Is(err, metadata.ErrUnsupported) {
			return navigationDetail{}, false, nil
		}
		return navigationDetail{}, false, err
	}
	unsupportedErr := err

	if object.Kind == catalog.ObjectKindTable {
		description, err := req.Context.Details.Detail(ctx, metadata.Request{Object: object, Facet: "description", Dialect: request.Dialect, Consistency: request.Consistency})
		if ctx.Err() != nil {
			return navigationDetail{}, false, ErrCanceled
		}
		if err != nil {
			if errors.Is(err, database.ErrObjectNotFound) {
				return navigationDetail{}, true, nil
			}
			if errors.Is(err, metadata.ErrUnsupported) {
				return navigationDetail{}, true, nil
			}
			return navigationDetail{}, false, err
		}
		if description.Object != object || description.Description.Body == "" {
			return navigationDetail{}, true, nil
		}
		span, ok := descriptionSpanFor(description.Description, object.Name, selectedNavigationColumn(req, object, position))
		if !ok {
			return navigationDetail{}, true, nil
		}
		rng, ok := byteSpanRange(description.Description.Body, span.Start, span.End)
		if !ok {
			return navigationDetail{}, true, nil
		}
		return navigationDetail{source: &Source{Object: description.Object, Text: description.Description.Body, Provenance: "catalog-description", Note: "Catalog reconstruction; unsupported table facets may be incomplete."}, range_: rng, unsupported: true}, true, nil
	}

	if object.Kind != catalog.ObjectKindProcedure && object.Kind != catalog.ObjectKindView && object.Kind != catalog.ObjectKindTrigger {
		return navigationDetail{}, true, nil
	}
	source, err := req.Context.Details.Detail(ctx, metadata.Request{Object: object, Facet: "source", Dialect: request.Dialect, Consistency: request.Consistency})
	if ctx.Err() != nil {
		return navigationDetail{}, false, ErrCanceled
	}
	if err != nil {
		if errors.Is(err, database.ErrObjectNotFound) || errors.Is(err, metadata.ErrUnsupported) {
			return navigationDetail{}, true, nil
		}
		return navigationDetail{}, false, err
	}
	if source.Object != object || source.Text == "" {
		return navigationDetail{}, true, nil
	}
	rng, ok := snapshotNameRange(source.Text, object.Name)
	if !ok {
		return navigationDetail{}, true, nil
	}
	return navigationDetail{source: &Source{Object: source.Object, Text: source.Text, Provenance: source.Provenance, Note: unsupportedDDLNote(unsupportedErr)}, range_: rng, unsupported: true}, true, nil
}

func unsupportedDDLNote(err error) string {
	object, name, feature, ok := database.UnsupportedDDLDetail(err)
	if !ok {
		return "Executable DDL could not be reproduced. The verbatim catalog source follows."
	}
	return "Executable DDL could not be reproduced: " + object + " \"" + name + "\": " + strings.TrimRight(feature, ".") + ".\nThe verbatim catalog source follows."
}

func selectedNavigationColumn(req Request, object catalog.ObjectID, position Position) *sqlsymbol.Name {
	if object.Kind != catalog.ObjectKindTable && object.Kind != catalog.ObjectKindView {
		return nil
	}
	offset, err := ByteOffset(req.Document.Text, position)
	if err != nil {
		return nil
	}
	analysis, err := sqlsymbol.Analyze(req.Document.Text, req.Context.Variant)
	if err != nil {
		return nil
	}
	resolution := analysis.Resolve(offset)
	if resolution.Role != sqlsymbol.Column || resolution.SQL == nil {
		return nil
	}
	column := resolution.SQL.Name
	return &column
}

func detailBodyRange(body string, object catalog.ObjectID, column *sqlsymbol.Name) (Range, bool) {
	var span sqlsymbol.Span
	var ok bool
	if object.Kind == catalog.ObjectKindTable || object.Kind == catalog.ObjectKindView && column != nil {
		if column != nil {
			span, ok = sqlsymbol.ColumnDeclaration(body, sqlsymbol.Name{Text: object.Name, Quoted: true}, *column)
		} else {
			span, ok = sqlsymbol.TableDeclaration(body, sqlsymbol.Name{Text: object.Name, Quoted: true})
		}
	} else {
		return snapshotNameRange(body, object.Name)
	}
	if !ok {
		return Range{}, false
	}
	return byteSpanRange(body, span.Start, span.End)
}

func descriptionSpanFor(description catalog.TableDescription, table string, column *sqlsymbol.Name) (sqlsymbol.Span, bool) {
	body := description.Body
	tableSpan, ok := catalogDescriptionSpan(body, description.Table, table)
	if !ok {
		return sqlsymbol.Span{}, false
	}
	declaredTable, ok := sqlsymbol.TableDeclaration(body, sqlsymbol.Name{Text: table, Quoted: true})
	if !ok || declaredTable != tableSpan {
		return sqlsymbol.Span{}, false
	}
	if column == nil {
		return tableSpan, true
	}
	var selected sqlsymbol.Span
	matches := 0
	for _, candidate := range description.Columns {
		if !column.MatchesCatalogName(candidate.Name) {
			continue
		}
		matches++
		selected, ok = catalogDescriptionSpan(body, candidate.Span, candidate.Name)
		if !ok {
			return sqlsymbol.Span{}, false
		}
		declared, found := sqlsymbol.ColumnDeclaration(body, sqlsymbol.Name{Text: table, Quoted: true}, sqlsymbol.Name{Text: candidate.Name, Quoted: true})
		if !found || declared != selected {
			return sqlsymbol.Span{}, false
		}
	}
	return selected, matches == 1
}

func catalogDescriptionSpan(body string, span catalog.DescriptionSpan, name string) (sqlsymbol.Span, bool) {
	if span.Start < 0 || span.End <= span.Start || span.End > len(body) {
		return sqlsymbol.Span{}, false
	}
	actual := body[span.Start:span.End]
	quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	if actual != quoted && actual != name {
		return sqlsymbol.Span{}, false
	}
	if actual == name && !validDescriptionIdentifier(name) {
		return sqlsymbol.Span{}, false
	}
	if span.Start > 0 && descriptionIdentifierByte(body[span.Start-1]) || span.End < len(body) && descriptionIdentifierByte(body[span.End]) {
		return sqlsymbol.Span{}, false
	}
	return sqlsymbol.Span{Start: span.Start, End: span.End}, true
}

func snapshotNameRange(body, name string) (Range, bool) {
	for _, candidate := range []string{`"` + strings.ReplaceAll(name, `"`, `""`) + `"`, name} {
		start := strings.Index(body, candidate)
		if start < 0 || candidate == name && (start > 0 && descriptionIdentifierByte(body[start-1]) || start+len(candidate) < len(body) && descriptionIdentifierByte(body[start+len(candidate)])) {
			continue
		}
		return byteSpanRange(body, start, start)
	}
	return Range{}, false
}

func validDescriptionIdentifier(name string) bool {
	if len(name) == 0 || len(name) > 67 || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	for index := 1; index < len(name); index++ {
		char := name[index]
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' && char != '$' {
			return false
		}
	}
	return true
}

func descriptionIdentifierByte(value byte) bool {
	return value == '_' || value == '$' || value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func byteSpanRange(body string, start, end int) (Range, bool) {
	if start < 0 || end < start || end > len(body) {
		return Range{}, false
	}
	left, err := PositionAt(body, start)
	if err != nil {
		return Range{}, false
	}
	right, err := PositionAt(body, end)
	if err != nil {
		return Range{}, false
	}
	return Range{Start: left, End: right}, true
}
