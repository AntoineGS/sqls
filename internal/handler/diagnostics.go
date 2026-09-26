package handler

import (
	"context"
	"log"
	"sort"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/metadata"
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
	uri                 string
	text                string
	version             int
	revision            uint64
	variant             dialect.DriverVariant
	generation          int
	cache               *database.DBCache
	metadata            *database.MetadataSnapshot
	dialectResolved     bool
	policy              language.DiagnosticPolicy
	policyRevision      uint64
	metadataLease       *metadata.Lease
	metadataKey         metadata.Key
	metadataIncarnation uint64
	metadataGeneration  uint64
}

func (s *Server) diagnosticsSnapshot(uri string) (documentDiagnosticsSnapshot, bool) {
	editor, err := s.captureEditorSnapshot(uri)
	if err != nil {
		return documentDiagnosticsSnapshot{}, false
	}
	return documentDiagnosticsSnapshot{
		uri:                 editor.URI,
		text:                editor.Text,
		version:             editor.Version,
		revision:            editor.Revision,
		generation:          editor.Generation,
		variant:             editor.Variant,
		cache:               editor.Cache,
		metadata:            editor.Metadata,
		dialectResolved:     editor.DialectResolved,
		policy:              editor.DiagnosticPolicy,
		policyRevision:      editor.PolicyRevision,
		metadataLease:       editor.MetadataLease,
		metadataKey:         editor.MetadataKey,
		metadataIncarnation: editor.MetadataIncarnation,
		metadataGeneration:  editor.MetadataGeneration,
	}, true
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
	lease, bound := s.metaLease, s.metaBoundGeneration
	key, incarnation := s.metaKey, uint64(0)
	if lease != nil {
		incarnation = lease.Incarnation()
	}
	s.stateMu.RUnlock()
	if !current {
		return false
	}
	if snapshot.generation != bound {
		return false
	}
	if lease == nil {
		// Before a connection has acquired a metadata lease, text-only
		// diagnostics still have a coherent nil-catalog binding.
		return snapshot.metadataLease == nil && snapshot.metadata == nil &&
			snapshot.metadataKey == (metadata.Key{}) && snapshot.metadataIncarnation == 0 &&
			snapshot.metadataGeneration == 0
	}
	if lease != snapshot.metadataLease || key != snapshot.metadataKey || incarnation != snapshot.metadataIncarnation {
		// The active lease was rebound (a connection switch) since this
		// diagnostic snapshot was captured: a late-arriving old-generation
		// result must never publish against the new connection's state.
		return false
	}
	meta := lease.Snapshot()
	return meta != nil && meta.Cache == snapshot.cache && meta.Generation == snapshot.metadataGeneration
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
			diagnostics = diagnosticsForSnapshot(ctx, snapshot)
		}
	}
	s.publishDiagnosticsSnapshot(ctx, conn, snapshot, diagnostics)
}

func diagnosticsForSnapshot(ctx context.Context, snapshot documentDiagnosticsSnapshot) []lsp.Diagnostic {
	editor := editorSnapshot{URI: snapshot.uri, Text: snapshot.text, Version: snapshot.version,
		Revision: snapshot.revision, Generation: snapshot.generation, Variant: snapshot.variant,
		DialectResolved: snapshot.dialectResolved, Metadata: snapshot.metadata,
		MetadataKey:   snapshot.metadataKey,
		MetadataLease: snapshot.metadataLease, MetadataIncarnation: snapshot.metadataIncarnation,
		MetadataGeneration: snapshot.metadataGeneration, DiagnosticPolicy: snapshot.policy,
		PolicyRevision: snapshot.policyRevision}
	req := languageRequest(editor)
	res, err := language.NewService().Diagnostics(ctx, req)
	if err != nil {
		// In-progress malformed SQL must not fail didOpen/didChange. A
		// canceled request means no finding is currently proven.
		log.Printf("sqls: static diagnostics skipped for %s: %v", snapshot.uri, err)
		return []lsp.Diagnostic{}
	}
	return lspDiagnostics(res.Value)
}

func lspDiagnostics(diags []language.Diagnostic) []lsp.Diagnostic {
	diagnostics := make([]lsp.Diagnostic, len(diags))
	for i, d := range diags {
		code, source := d.Code, d.Source
		diagnostics[i] = lsp.Diagnostic{
			Range:    lspRange(d.Range),
			Severity: d.Severity,
			Code:     &code,
			Source:   &source,
			Message:  d.Message,
		}
	}
	return diagnostics
}

// DiagnosticsWithDriverVariant is a thin wrapper over
// language.Service.Diagnostics, in the same family as
// HoverWithDriverVariant, SignatureHelpWithDriverVariant and
// CompletionWithDriverVariant: it delegates the diagnostics algorithm
// directly (diagnostics need no metadata snapshot) and converts the result
// back to lsp types through the same lspDiagnostics conversion
// diagnosticsForSnapshot uses in production.
func DiagnosticsWithDriverVariant(ctx context.Context, text string, dv dialect.DriverVariant, dialectResolved bool) ([]lsp.Diagnostic, error) {
	req := language.Request{
		Document: language.Document{Text: text},
		Context: language.Context{
			Variant:         dv,
			DialectResolved: dialectResolved,
		},
	}
	res, err := language.NewService().Diagnostics(ctx, req)
	if err != nil {
		return nil, err
	}
	return lspDiagnostics(res.Value), nil
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
