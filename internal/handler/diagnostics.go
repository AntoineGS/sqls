package handler

import (
	"context"
	"log"
	"sort"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

const publishDiagnosticsMethod = "textDocument/publishDiagnostics"

func isServerNotification(method string) bool {
	switch method {
	case publishDiagnosticsMethod, "window/showMessage", "window/logMessage":
		return true
	default:
		return false
	}
}

type documentDiagnosticsSnapshot struct {
	uri           string
	text          string
	version       int
	revision      uint64
	variant       dialect.DriverVariant
	generation    int
	cache         *database.DBCache
	cacheSnapshot sqlsymbol.Catalog
}

type diagnosticCatalog struct {
	columns map[string][]sqlsymbol.ColumnType
	keys    map[string][][]string
}

func (c *diagnosticCatalog) Columns(table sqlsymbol.Name) ([]sqlsymbol.ColumnType, bool) {
	if c == nil {
		return nil, false
	}
	columns, ok := c.columns[table.Key()]
	if !ok {
		return nil, false
	}
	return append([]sqlsymbol.ColumnType(nil), columns...), true
}

func (c *diagnosticCatalog) UniqueKeys(table sqlsymbol.Name) ([][]string, bool) {
	if c == nil {
		return nil, false
	}
	keys, ok := c.keys[table.Key()]
	result := make([][]string, len(keys))
	for i, key := range keys {
		result[i] = append([]string(nil), key...)
	}
	return result, ok
}

// snapshotDiagnosticCatalog copies the cache metadata used by analysis. The
// DBCache is copy-on-write, but copying names, types, and their available order
// keeps this analysis independent of subsequent worker refreshes.
func snapshotDiagnosticCatalog(cache *database.DBCache) sqlsymbol.Catalog {
	if cache == nil {
		return nil
	}
	catalog := &diagnosticCatalog{
		columns: make(map[string][]sqlsymbol.ColumnType),
		keys:    make(map[string][][]string),
	}
	for _, table := range cache.SortedTables() {
		descriptions, ok := cache.ColumnDescs(table)
		if !ok {
			continue
		}
		columns := make([]sqlsymbol.ColumnType, 0, len(descriptions))
		for _, description := range descriptions {
			if description == nil {
				continue
			}
			columns = append(columns, sqlsymbol.ColumnType{Name: description.Name, Type: description.Type})
		}
		// Catalog metadata preserves the database's actual spelling. Quoted
		// names therefore match exactly; unquoted InterBase names naturally
		// match the upper-case names returned by its catalog.
		catalog.columns[table] = columns
		if keys, known := diagnosticUniqueKeys(cache, table, columns); known {
			catalog.keys[table] = keys
		}
	}
	return catalog
}

// Primary-key and UNIQUE-constraint enforcing indexes are present alongside
// standalone unique indexes in the extended catalog. Wait for that complete
// catalog: column primary-key flags alone cannot rule out other unique keys.
func diagnosticUniqueKeys(cache *database.DBCache, table string, columns []sqlsymbol.ColumnType) ([][]string, bool) {
	if !cache.HasCatalog() || !cache.ColumnsReady() || !cache.MetadataReady(database.MetadataViews, database.MetadataIndexes) {
		return nil, false
	}
	for _, view := range cache.Catalog.Views {
		if view != nil && view.Name == table {
			return nil, false
		}
	}
	var keys [][]string
	for _, index := range cache.IndexesForTable(table) {
		// The cache lookup folds case, but quoted relation identities do not.
		if index == nil || index.RelationName != table {
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

func (s *Server) diagnosticsSnapshot(uri string) (documentDiagnosticsSnapshot, bool) {
	s.stateMu.RLock()
	file, ok := s.files[uri]
	if !ok {
		s.stateMu.RUnlock()
		return documentDiagnosticsSnapshot{}, false
	}
	snapshot := documentDiagnosticsSnapshot{
		uri:        uri,
		text:       file.Text,
		version:    file.Version,
		revision:   file.Revision,
		generation: s.connGeneration,
	}
	if s.dbConn != nil {
		snapshot.variant = s.dbConn.DriverVariant()
	}
	s.stateMu.RUnlock()

	meta := s.metadata.Snapshot()
	if meta != nil && meta.Generation == uint64(snapshot.generation) {
		snapshot.cache = meta.Cache
		if snapshot.cache != nil && snapshot.variant.Driver == dialect.DatabaseDriverInterBase && snapshot.cache.ColumnsReady() {
			snapshot.cacheSnapshot = snapshotDiagnosticCatalog(snapshot.cache)
		}
	}
	return snapshot, true
}

func (s *Server) diagnosticsSnapshotCurrent(snapshot documentDiagnosticsSnapshot) bool {
	s.stateMu.RLock()
	file, open := s.files[snapshot.uri]
	current := open && file.Version == snapshot.version && file.Revision == snapshot.revision &&
		s.connGeneration == snapshot.generation
	if s.dbConn == nil {
		current = current && snapshot.variant == (dialect.DriverVariant{})
	} else {
		current = current && s.dbConn.DriverVariant() == snapshot.variant
	}
	s.stateMu.RUnlock()
	if !current {
		return false
	}
	meta := s.metadata.Snapshot()
	return meta != nil && meta.Generation == uint64(snapshot.generation) && meta.Cache == snapshot.cache
}

func (s *Server) publishDocumentDiagnostics(ctx context.Context, conn *jsonrpc2.Conn, uri string) {
	snapshot, ok := s.diagnosticsSnapshot(uri)
	if !ok {
		return
	}
	diagnostics := diagnosticsForSnapshot(snapshot)
	s.publishDiagnosticsSnapshot(ctx, conn, snapshot, diagnostics)
}

func diagnosticsForSnapshot(snapshot documentDiagnosticsSnapshot) []lsp.Diagnostic {
	diagnostics := make([]lsp.Diagnostic, 0)
	if snapshot.variant.Driver != dialect.DatabaseDriverInterBase {
		return diagnostics
	}
	analysis, err := sqlsymbol.AnalyzeDiagnostics(snapshot.text, snapshot.variant)
	if err != nil {
		// In-progress malformed SQL must not fail didOpen/didChange. The
		// analyzer's conservative failure means no finding is currently proven.
		log.Printf("sqls: static diagnostics skipped for %s: %v", snapshot.uri, err)
		return diagnostics
	}

	found := analysis.Diagnostics(snapshot.cacheSnapshot)
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
		rangeValue, ok := symbolRange(snapshot.text, finding.Span)
		if !ok {
			continue
		}
		code := finding.Code
		source := "sqls"
		diagnostics = append(diagnostics, lsp.Diagnostic{
			Range:    rangeValue,
			Severity: finding.Severity,
			Code:     &code,
			Source:   &source,
			Message:  finding.Message,
		})
	}
	return diagnostics
}

func (s *Server) publishDiagnosticsSnapshot(ctx context.Context, conn *jsonrpc2.Conn, snapshot documentDiagnosticsSnapshot, diagnostics []lsp.Diagnostic) {
	if conn == nil {
		return
	}
	s.diagnosticsPublishMu.Lock()
	defer s.diagnosticsPublishMu.Unlock()
	if !s.diagnosticsSnapshotCurrent(snapshot) {
		return
	}
	version := snapshot.version
	params := lsp.PublishDiagnosticsParams{
		URI:         snapshot.uri,
		Version:     &version,
		Diagnostics: diagnostics,
	}
	if err := conn.Notify(ctx, publishDiagnosticsMethod, params); err != nil {
		log.Printf("sqls: publish diagnostics for %s: %v", snapshot.uri, err)
	}
}

func (s *Server) clearClosedDiagnostics(ctx context.Context, conn *jsonrpc2.Conn, uri string) {
	if conn == nil {
		return
	}
	s.diagnosticsPublishMu.Lock()
	defer s.diagnosticsPublishMu.Unlock()
	s.stateMu.RLock()
	_, stillOpen := s.files[uri]
	s.stateMu.RUnlock()
	if stillOpen {
		return
	}
	params := lsp.PublishDiagnosticsParams{URI: uri, Diagnostics: []lsp.Diagnostic{}}
	if err := conn.Notify(ctx, publishDiagnosticsMethod, params); err != nil {
		log.Printf("sqls: clear diagnostics for %s: %v", uri, err)
	}
}

func (s *Server) republishOpenDiagnostics(ctx context.Context) {
	s.stateMu.RLock()
	conn := s.notificationConn
	uris := make([]string, 0, len(s.files))
	for uri := range s.files {
		uris = append(uris, uri)
	}
	s.stateMu.RUnlock()
	if conn == nil {
		return
	}
	sort.Strings(uris)
	for _, uri := range uris {
		s.publishDocumentDiagnostics(ctx, conn, uri)
	}
}

// signalDiagnostics coalesces refresh requests into one bounded wake. The
// single worker below performs client notifications without a connection
// lifecycle lock held and prevents metadata bursts from spawning goroutines.
func (s *Server) signalDiagnostics() {
	select {
	case <-s.lifecycleCtx.Done():
		return
	default:
	}
	select {
	case s.diagnosticsWake <- struct{}{}:
	default:
	}
}

func (s *Server) runDiagnosticSignals() {
	for {
		select {
		case <-s.lifecycleCtx.Done():
			return
		case <-s.diagnosticsWake:
			s.republishOpenDiagnostics(s.lifecycleCtx)
		}
	}
}
