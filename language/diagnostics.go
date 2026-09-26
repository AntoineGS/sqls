package language

import (
	"context"
	"fmt"
	"sort"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

// ValidateDiagnosticPolicy validates rule codes and levels against SQLs's
// single diagnostic registry.
func ValidateDiagnosticPolicy(policy DiagnosticPolicy) error {
	if err := (sqlsymbol.DiagnosticOptions{Rules: policy.Rules}).Validate(); err != nil {
		return fmt.Errorf("language: invalid diagnostic policy: %w", err)
	}
	return nil
}

// cacheDiagnosticCatalog answers sqlsymbol's Catalog/UniqueKeyCatalog
// interfaces directly from a *catalog.Cache, on demand, per lookup. It never
// builds a second copy of the cache's column or index data: Columns and
// UniqueKeys resolve straight from cache.ColumnDescsByID/cache.IndexesForTable
// each time they are called.
type cacheDiagnosticCatalog struct {
	cache *catalog.Cache
}

// Columns resolves table by its exact catalog identity via
// cache.ColumnDescsByID whenever cache.ColumnsByID has been populated at
// all: the shared upper-cased ColumnDescs/ColumnDatabase accessors fold
// every lookup to upper-case, so a quoted table (table.Key() preserves
// its exact spelling) would otherwise silently collide with, and read the
// columns of, any other relation whose name happens to upper-case to the
// same key. A per-key miss from ColumnDescsByID is NOT reason enough to
// fall back to the folded accessor: once a repository has populated
// exact-identity storage at all, a miss is authoritative — "no relation
// exists under this exact identity" — and falling back would reintroduce
// the exact collision this adapter exists to prevent (see
// catalog.Cache.ColumnsByID's doc comment on nil vs. populated-but-absent).
//
// Columns tests len(cache.ColumnsByID) > 0, NOT cache.ColumnsByID != nil:
// metadata.Merge's cloneColumnSlices(nil) — reached whenever a repository's
// patch never sets ColumnsByID, which is every driver/test path but
// InterBase's real readMetadataColumns — returns a non-nil EMPTY map, not
// nil, so a nil-check is always true once MetadataColumnsCurrent/
// MetadataColumnsAll has merged at all and cannot tell "this repository
// populates exact-identity storage" apart from "this repository does not
// and the merge machinery's clone-on-nil just produced an empty map
// anyway" (confirmed against internal/handler's MockDBRepository-backed
// diagnostics tests, which regressed under the nil-check). A non-empty
// map is the reliable signal instead: readMetadataColumns adds every
// scanned column under its table's key in the same bulk job that
// populates ColumnsWithParent, so any real InterBase catalog with at
// least one column anywhere yields a non-empty map, while every path that
// never populates exact-identity data at all (mocks, other drivers, a
// merge of an empty fragment) stays at length zero forever.
func (c cacheDiagnosticCatalog) Columns(table sqlsymbol.Name) ([]sqlsymbol.ColumnType, bool) {
	if c.cache == nil {
		return nil, false
	}
	var descriptions []*catalog.ColumnDesc
	var ok bool
	if len(c.cache.ColumnsByID) > 0 {
		descriptions, ok = c.cache.ColumnDescsByID(table.Key())
	} else {
		descriptions, ok = c.cache.ColumnDescs(table.Key())
	}
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
	// ViewByID resolves key by its exact catalog identity, the same
	// requirement Columns above has: looping cache.Catalog.Views (the
	// shared upper-cased map) would miss a quoted view name that collides
	// with an unrelated relation's folded key. Mirror Columns' cache-wide
	// (not per-key) length check, not a nil check: metadata.Merge's
	// mapOrEmpty+cloneMap always produce a non-nil ViewsByID once
	// MetadataViews has merged at all, so nil-vs-populated is
	// indistinguishable the same way it is for ColumnsByID (see Columns'
	// doc comment) and only a non-empty map reliably means "this
	// repository populates exact-identity view storage."
	var isView bool
	if cache.HasCatalog() && len(cache.Catalog.ViewsByID) > 0 {
		_, isView = cache.ViewByID(key)
	} else {
		_, isView = cache.View(key)
	}
	if isView {
		return nil, false
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
	if err := ValidateDiagnosticPolicy(req.Policy); err != nil {
		return Result[[]Diagnostic]{}, err
	}
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
	// Text-only rules remain active regardless of catalog readiness. The
	// snapshot adapter independently gates each semantic namespace/category.
	var catalogView sqlsymbol.Catalog = snapshotSemanticCatalog{snapshot: req.Context.Snapshot}
	found := analysis.DiagnosticsWithOptions(catalogView, sqlsymbol.DiagnosticOptions{Rules: req.Policy.Rules})
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
