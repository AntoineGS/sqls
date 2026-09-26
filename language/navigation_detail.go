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
	hasRange   bool
	executable bool
	// unsupported is true only when a typed DDL refusal was returned. Hosts
	// may explain that executable DDL is unavailable without labeling fallback
	// source or descriptions as executable.
	unsupported bool
}

func (s *Service) loadNavigationDetail(ctx context.Context, req Request, object catalog.ObjectID, position Position, provenColumn *sqlsymbol.Name) (navigationDetail, bool, error) {
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
		rng, hasRange := detailBodyRange(detail.Text, object, navigationColumn(req, object, position, provenColumn))
		return navigationDetail{source: &Source{Object: detail.Object, Text: detail.Text, Provenance: detail.Provenance}, range_: rng, hasRange: hasRange, executable: true}, true, nil
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
		span, ok := descriptionSpanFor(description.Description, object.Name, navigationColumn(req, object, position, provenColumn))
		if !ok {
			return navigationDetail{}, true, nil
		}
		rng, ok := byteSpanRange(description.Description.Body, span.Start, span.End)
		if !ok {
			return navigationDetail{}, true, nil
		}
		return navigationDetail{source: &Source{Object: description.Object, Text: description.Description.Body, Provenance: "catalog-description", Note: "Catalog reconstruction; unsupported table facets may be incomplete."}, range_: rng, hasRange: true, unsupported: true}, true, nil
	}

	if object.Kind != catalog.ObjectKindProcedure && object.Kind != catalog.ObjectKindView && object.Kind != catalog.ObjectKindTrigger {
		return navigationDetail{unsupported: true}, true, nil
	}
	source, err := req.Context.Details.Detail(ctx, metadata.Request{Object: object, Facet: "source", Dialect: request.Dialect, Consistency: request.Consistency})
	if ctx.Err() != nil {
		return navigationDetail{}, false, ErrCanceled
	}
	if err != nil {
		if errors.Is(err, database.ErrObjectNotFound) || errors.Is(err, metadata.ErrUnsupported) {
			return navigationDetail{unsupported: true}, true, nil
		}
		return navigationDetail{}, false, err
	}
	if source.Object != object || source.Text == "" {
		return navigationDetail{unsupported: true}, true, nil
	}
	column := navigationColumn(req, object, position, provenColumn)
	rng, hasRange := detailBodyRange(source.Text, object, column)
	if !hasRange && column == nil && (object.Kind == catalog.ObjectKindView || object.Kind == catalog.ObjectKindProcedure || object.Kind == catalog.ObjectKindTrigger) && !ddlCreateTokenExists(source.Text) {
		// Handler snapshotRange historically returned (0,0) when a native body
		// omitted the catalog object's name. Preserve that safe body-start
		// location only when the body has no CREATE token; never use it for a
		// selected column or malformed/mismatched declaration-like text.
		rng, hasRange = Range{}, true
	}
	return navigationDetail{source: &Source{Object: source.Object, Text: source.Text, Provenance: source.Provenance, Note: unsupportedDDLNote(unsupportedErr)}, range_: rng, hasRange: hasRange, unsupported: true}, true, nil
}

func navigationColumn(req Request, object catalog.ObjectID, position Position, proven *sqlsymbol.Name) *sqlsymbol.Name {
	if proven != nil {
		column := *proven
		return &column
	}
	return selectedNavigationColumn(req, object, position)
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
		return ddlDeclarationNameRange(body, object)
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

type declarationToken struct {
	text   string
	start  int
	quoted bool
	ident  bool
}

// ddlDeclarationNameRange finds the declared name after CREATE, ignoring
// comments and string literals. Native InterBase source facets commonly omit
// their CREATE header; in that case there is intentionally no navigable name.
func ddlDeclarationNameRange(body string, object catalog.ObjectID) (Range, bool) {
	tokens := declarationTokens(body)
	for i := 0; i < len(tokens); i++ {
		if !declarationWord(tokens[i], "CREATE") {
			continue
		}
		kindIndex, ok := declarationKindIndex(tokens, i, object.Kind)
		if !ok || kindIndex+1 >= len(tokens) {
			continue
		}
		nameIndex := kindIndex + 1
		if nameIndex+2 < len(tokens) && tokens[nameIndex+1].text == "." && tokens[nameIndex+2].ident {
			nameIndex += 2
		}
		name := tokens[nameIndex]
		if !name.ident || !(sqlsymbol.Name{Text: name.text, Quoted: name.quoted}).MatchesCatalogName(object.Name) {
			continue
		}
		return byteSpanRange(body, name.start, name.start)
	}
	return Range{}, false
}

func ddlCreateTokenExists(body string) bool {
	for _, token := range declarationTokens(body) {
		if declarationWord(token, "CREATE") {
			return true
		}
	}
	return false
}

func declarationKindIndex(tokens []declarationToken, createIndex int, kind catalog.ObjectKind) (int, bool) {
	j := createIndex + 1
	if j+1 < len(tokens) && declarationWord(tokens[j], "OR") && (declarationWord(tokens[j+1], "ALTER") || declarationWord(tokens[j+1], "REPLACE")) {
		j += 2
	}
	if j+1 < len(tokens) && declarationWord(tokens[j], "GLOBAL") && declarationWord(tokens[j+1], "TEMPORARY") {
		j += 2
	}
	if j >= len(tokens) || !declarationWord(tokens[j], strings.ToUpper(string(kind))) {
		return 0, false
	}
	return j, true
}

func declarationWord(token declarationToken, word string) bool {
	return token.ident && !token.quoted && strings.EqualFold(token.text, word)
}

func declarationTokens(text string) []declarationToken {
	tokens := make([]declarationToken, 0, 32)
	for i := 0; i < len(text); {
		switch {
		case text[i] == ' ' || text[i] == '\t' || text[i] == '\r' || text[i] == '\n' || text[i] == '\f':
			i++
		case i+1 < len(text) && text[i] == '-' && text[i+1] == '-':
			i += 2
			for i < len(text) && text[i] != '\n' && text[i] != '\r' {
				i++
			}
		case i+1 < len(text) && text[i] == '/' && text[i+1] == '*':
			i += 2
			for i+1 < len(text) && !(text[i] == '*' && text[i+1] == '/') {
				i++
			}
			if i+1 < len(text) {
				i += 2
			}
		case text[i] == '\'':
			start := i
			i++
			for i < len(text) {
				if text[i] == '\'' {
					i++
					if i < len(text) && text[i] == '\'' {
						i++
						continue
					}
					break
				}
				i++
			}
			tokens = append(tokens, declarationToken{text: text[start:i]})
		case text[i] == '"':
			start := i
			i++
			var value strings.Builder
			closed := false
			for i < len(text) {
				if text[i] == '"' {
					i++
					if i < len(text) && text[i] == '"' {
						value.WriteByte('"')
						i++
						continue
					}
					closed = true
					break
				}
				value.WriteByte(text[i])
				i++
			}
			if closed {
				tokens = append(tokens, declarationToken{text: value.String(), start: start, quoted: true, ident: true})
			}
		case declarationIdentifierByte(text[i]):
			start := i
			i++
			for i < len(text) && declarationIdentifierByte(text[i]) {
				i++
			}
			tokens = append(tokens, declarationToken{text: text[start:i], start: start, ident: true})
		default:
			tokens = append(tokens, declarationToken{text: string(text[i]), start: i})
			i++
		}
	}
	return tokens
}

func declarationIdentifierByte(value byte) bool {
	return value == '_' || value == '$' || value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= 0x80
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
