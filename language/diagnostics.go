package language

import (
	"context"
	"sort"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

// cacheDiagnosticCatalog answers sqlsymbol's Catalog/UniqueKeyCatalog
// interfaces directly from a *catalog.Cache, on demand, per lookup. It never
// builds a second copy of the cache's column or index data: Columns and
// UniqueKeys resolve straight from cache.ColumnDescs/cache.IndexesForTable
// each time they are called.
type cacheDiagnosticCatalog struct {
	cache *catalog.Cache
}

func (c cacheDiagnosticCatalog) Columns(table sqlsymbol.Name) ([]sqlsymbol.ColumnType, bool) {
	if c.cache == nil {
		return nil, false
	}
	descriptions, ok := c.cache.ColumnDescs(table.Key())
	if !ok {
		return nil, false
	}
	columns := make([]sqlsymbol.ColumnType, 0, len(descriptions))
	for _, description := range descriptions {
		if description == nil {
			continue
		}
		columns = append(columns, sqlsymbol.ColumnType{Name: description.Name, Type: description.Type})
	}
	return columns, true
}

// UniqueKeys reports the enforced unique/primary keys covering table, using
// the same catalog readiness gate as Columns: primary-key and
// UNIQUE-constraint enforcing indexes are present alongside standalone
// unique indexes only once views and indexes metadata are both ready.
func (c cacheDiagnosticCatalog) UniqueKeys(table sqlsymbol.Name) ([][]string, bool) {
	cache := c.cache
	if !cache.HasCatalog() || !cache.ColumnsReady() || !cache.MetadataReady(catalog.MetadataViews, catalog.MetadataIndexes) {
		return nil, false
	}
	key := table.Key()
	for _, view := range cache.Catalog.Views {
		if view != nil && view.Name == key {
			return nil, false
		}
	}
	columns, ok := c.Columns(table)
	if !ok {
		return nil, false
	}
	var keys [][]string
	for _, index := range cache.IndexesForTable(key) {
		// The cache lookup folds case, but quoted relation identities do not.
		if index == nil || index.RelationName != key {
			continue
		}
		if !index.Unique.Valid {
			return nil, false
		}
		if !index.Unique.Bool {
			continue
		}
		if !index.Active.Valid {
			return nil, false
		}
		if !index.Active.Bool {
			continue
		}
		if index.Expression.Valid || len(index.Columns) == 0 {
			return nil, false
		}
		for _, segment := range index.Columns {
			matches := 0
			for _, column := range columns {
				if column.Name == segment {
					matches++
				}
			}
			if matches != 1 {
				return nil, false
			}
		}
		keys = append(keys, append([]string(nil), index.Columns...))
	}
	return keys, true
}

// diagnosticCatalogReady reports whether snapshot's cache is populated
// enough for InterBase static diagnostics to consult catalog metadata at
// all. Diagnostics.Diagnostics still runs analyses that need no catalog
// data when this is false; it only gates whether a Catalog is attached.
func diagnosticCatalogReady(snapshot *catalog.MetadataSnapshot, variant dialect.DriverVariant) bool {
	return snapshot != nil && snapshot.Cache != nil && variant.Driver == dialect.DatabaseDriverInterBase && snapshot.Cache.ColumnsReady()
}

// Diagnostics returns static diagnostics for req.Document, interpreted
// against req.Context. It only analyzes InterBase-dialect documents;
// other dialects, and documents whose dialect is not yet resolved, yield
// an empty (never nil) diagnostics list. Malformed SQL that the analyzer
// cannot parse also yields an empty list rather than an error: in-progress
// edits must not surface a diagnostics failure.
func (s *Service) Diagnostics(ctx context.Context, req Request) (Result[[]Diagnostic], error) {
	if err := ctx.Err(); err != nil {
		return Result[[]Diagnostic]{}, ErrCanceled
	}
	stamp := stampFor(req)
	diagnostics := make([]Diagnostic, 0)
	if req.Context.Variant.Driver != dialect.DatabaseDriverInterBase || !req.Context.DialectResolved {
		return Result[[]Diagnostic]{Value: diagnostics, Stamp: stamp}, nil
	}
	analysis, err := sqlsymbol.AnalyzeDiagnostics(req.Document.Text, req.Context.Variant)
	if err != nil {
		return Result[[]Diagnostic]{Value: diagnostics, Stamp: stamp}, nil
	}
	var catalogView sqlsymbol.Catalog
	if diagnosticCatalogReady(req.Context.Snapshot, req.Context.Variant) {
		catalogView = cacheDiagnosticCatalog{cache: req.Context.Snapshot.Cache}
	}
	found := analysis.Diagnostics(catalogView)
	sort.SliceStable(found, func(i, j int) bool {
		left, right := found[i], found[j]
		if left.Span.Start != right.Span.Start {
			return left.Span.Start < right.Span.Start
		}
		if left.Span.End != right.Span.End {
			return left.Span.End < right.Span.End
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		if left.Message != right.Message {
			return left.Message < right.Message
		}
		return left.Severity < right.Severity
	})
	for _, finding := range found {
		rangeValue, ok := spanRange(req.Document.Text, finding.Span)
		if !ok {
			continue
		}
		diagnostics = append(diagnostics, Diagnostic{
			Range:    rangeValue,
			Severity: finding.Severity,
			Code:     finding.Code,
			Source:   "sqls",
			Message:  finding.Message,
		})
	}
	return Result[[]Diagnostic]{Value: diagnostics, Stamp: stamp}, nil
}


