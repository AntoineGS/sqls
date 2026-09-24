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
	uri               string
	text              string
	version           int
	revision          uint64
	variant           dialect.DriverVariant
	generation        int
	cache             *database.DBCache
	cacheSnapshot     sqlsymbol.SemanticCatalog
	dialectResolved   bool
	diagnosticOptions sqlsymbol.DiagnosticOptions
	policyRevision    uint64
}

type diagnosticCatalog struct {
	keys map[string][][]string

	relations       map[string]sqlsymbol.RelationFact
	relationsKnown  bool
	procedures      map[string]sqlsymbol.ProcedureFact
	proceduresKnown bool
	domains         map[string]sqlsymbol.DomainFact
	domainsKnown    bool
}

// Columns preserves its pre-SemanticCatalog contract: a table is reported
// only once its relation existence and its complete column list are both
// known, which RelationInfo's Present+ColumnsKnown combination captures.
func (c *diagnosticCatalog) Columns(table sqlsymbol.Name) ([]sqlsymbol.ColumnType, bool) {
	fact, knowledge := c.RelationInfo(table)
	if knowledge != sqlsymbol.Present || !fact.ColumnsKnown {
		return nil, false
	}
	columns := make([]sqlsymbol.ColumnType, len(fact.Columns))
	for i, column := range fact.Columns {
		columns[i] = sqlsymbol.ColumnType{Name: column.Name, Type: column.Type}
	}
	return columns, true
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
// keeps this analysis independent of subsequent metadata publications. Each
// fact category is copied as soon as its own metadata is ready; the whole
// cache no longer needs to be columns-ready before a snapshot exists.
func snapshotDiagnosticCatalog(cache *database.DBCache) sqlsymbol.SemanticCatalog {
	if cache == nil {
		return nil
	}
	proceduresKnown := cache.HasCatalog() && cache.MetadataReady(database.MetadataProcedures)
	catalog := &diagnosticCatalog{
		keys:      make(map[string][][]string),
		relations: buildDiagnosticRelations(cache),
		// A relation lookup can only prove Missing once procedures are also
		// known: an absent table/view name might still be a selectable
		// procedure (a callable relation form).
		relationsKnown:  relationsNamespaceReady(cache) && proceduresKnown,
		procedures:      buildDiagnosticProcedures(cache),
		proceduresKnown: proceduresKnown,
		domains:         buildDiagnosticDomains(cache),
		domainsKnown:    cache.HasCatalog() && cache.MetadataReady(database.MetadataDomains),
	}
	// Catalog metadata preserves the database's actual spelling. Quoted
	// names therefore match exactly; unquoted InterBase names naturally
	// match the upper-case names returned by its catalog.
	for table, fact := range catalog.relations {
		if !fact.ColumnsKnown {
			continue
		}
		columns := make([]sqlsymbol.ColumnType, len(fact.Columns))
		for i, column := range fact.Columns {
			columns[i] = sqlsymbol.ColumnType{Name: column.Name, Type: column.Type}
		}
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
	editor, err := s.captureEditorSnapshot(uri)
	if err != nil {
		return documentDiagnosticsSnapshot{}, false
	}
	snapshot := documentDiagnosticsSnapshot{
		uri:               editor.URI,
		text:              editor.Text,
		version:           editor.Version,
		revision:          editor.Revision,
		generation:        editor.Generation,
		variant:           editor.Variant,
		cache:             editor.Cache,
		dialectResolved:   editor.DialectResolved,
		diagnosticOptions: editor.DiagnosticOptions,
		policyRevision:    editor.PolicyRevision,
	}
	if snapshot.CacheReadyForDiagnostics() {
		snapshot.cacheSnapshot = s.diagnosticCatalogFor(snapshot.cache)
	}
	return snapshot, true
}

// CacheReadyForDiagnostics gates catalog construction on the connection's
// dialect alone. Each fact category's own readiness (see snapshotDiagnosticCatalog)
// decides what a particular lookup can prove; the whole cache no longer needs
// to be columns-ready before the adapter exists.
func (snapshot documentDiagnosticsSnapshot) CacheReadyForDiagnostics() bool {
	return snapshot.cache != nil && snapshot.variant.Driver == dialect.DatabaseDriverInterBase
}

func (s *Server) diagnosticsSnapshotCurrent(snapshot documentDiagnosticsSnapshot) bool {
	s.stateMu.RLock()
	file, open := s.files[snapshot.uri]
	current := open && file.Version == snapshot.version && file.Revision == snapshot.revision &&
		s.connGeneration == snapshot.generation && s.policyRevision == snapshot.policyRevision
	if s.dbConn == nil {
		cfg := cloneConnectionConfig(s.curDBCfg)
		if cfg == nil {
			cfg = selectedConfig(s.effectiveConfigLocked(), s.initOptionDBConfig, s.curConnectionIndex)
		}
		configured := configuredDriverVariant(cfg)
		current = current && configured == snapshot.variant
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

// configuredDriverVariant mirrors the pre-attachment parser selection used by
// captureEditorSnapshot. It is deliberately local-only so the diagnostics
// publication fence can compare identity without I/O or recursively acquiring
// stateMu.
func configuredDriverVariant(cfg *database.DBConfig) dialect.DriverVariant {
	if cfg == nil {
		return dialect.DriverVariant{}
	}
	variant := dialect.DriverVariant{Driver: cfg.Driver}
	if cfg.Driver == dialect.DatabaseDriverInterBase {
		switch cfg.Dialect {
		case 1:
			variant.Variant = dialect.SQLVariantInterBase1
		case 3:
			variant.Variant = dialect.SQLVariantInterBase3
		default:
			variant.Variant = dialect.SQLVariantInterBase3
		}
	}
	return variant
}

func (s *Server) publishDocumentDiagnostics(ctx context.Context, conn *jsonrpc2.Conn, uri string) {
	snapshot, ok := s.diagnosticsSnapshot(uri)
	if !ok {
		return
	}
	var diagnostics []lsp.Diagnostic
	if snapshot.dialectResolved || snapshot.variant.Driver != dialect.DatabaseDriverInterBase {
		if s.diagnosticAnalyzer != nil {
			diagnostics = s.diagnosticAnalyzer(snapshot)
		} else {
			diagnostics = diagnosticsForSnapshot(snapshot)
		}
	}
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

	found := analysis.DiagnosticsWithOptions(snapshot.cacheSnapshot, snapshot.diagnosticOptions)
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
	// LSP requires an array even when there are no findings. A nil Go slice
	// encodes as null, which clients cannot treat as a diagnostics list.
	if diagnostics == nil {
		diagnostics = []lsp.Diagnostic{}
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
		if ctx.Err() != nil {
			return
		}
		s.publishDocumentDiagnostics(ctx, conn, uri)
	}
}

// diagnosticCatalogFor memoizes the immutable diagnostic projection for the
// current copy-on-write cache. Metadata status-only revisions retain the same
// cache pointer; data changes publish a new one and replace this single entry.
func (s *Server) diagnosticCatalogFor(cache *database.DBCache) sqlsymbol.SemanticCatalog {
	s.diagnosticCatalogMu.Lock()
	defer s.diagnosticCatalogMu.Unlock()
	if cache == nil {
		s.diagnosticCache = nil
		s.derivedCatalog = nil
		return nil
	}
	if s.diagnosticCache != cache {
		s.derivedCatalog = snapshotDiagnosticCatalog(cache)
		s.diagnosticCache = cache
	}
	return s.derivedCatalog
}

// signalDiagnostics coalesces refresh requests into one bounded wake. Dirty
// state must be marked before this method is called.
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
