package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

type diagnosticsNotification struct {
	URI         string           `json:"uri"`
	Version     *int             `json:"version"`
	Diagnostics []lsp.Diagnostic `json:"diagnostics"`
}

type diagnosticsClient struct {
	mu            sync.Mutex
	notifications []diagnosticsNotification
	changed       chan struct{}
	barrier       chan struct{}
}

func newDiagnosticsClient() *diagnosticsClient {
	return &diagnosticsClient{changed: make(chan struct{}, 1), barrier: make(chan struct{}, 1)}
}

func (c *diagnosticsClient) Handle(_ context.Context, _ *jsonrpc2.Conn, req *jsonrpc2.Request) {
	if req.Method == "test/notificationBarrier" {
		select {
		case c.barrier <- struct{}{}:
		default:
		}
		return
	}
	if req.Method != "textDocument/publishDiagnostics" || req.Params == nil {
		return
	}
	var params diagnosticsNotification
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return
	}
	c.mu.Lock()
	c.notifications = append(c.notifications, params)
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *diagnosticsClient) next(t *testing.T, uri string, matches func(diagnosticsNotification) bool) diagnosticsNotification {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		c.mu.Lock()
		for i, notification := range c.notifications {
			if notification.URI == uri && matches(notification) {
				c.notifications = append(c.notifications[:i], c.notifications[i+1:]...)
				c.mu.Unlock()
				return notification
			}
		}
		c.mu.Unlock()
		select {
		case <-c.changed:
		case <-deadline:
			t.Fatalf("timed out waiting for diagnostics notification for %s", uri)
		}
	}
}

type diagnosticsTestContext struct {
	server     *Server
	clientConn *jsonrpc2.Conn
	serverConn *jsonrpc2.Conn
	client     *diagnosticsClient
	ctx        context.Context
}

func newDiagnosticsTestContext(t *testing.T, driver dialect.DatabaseDriver) *diagnosticsTestContext {
	t.Helper()
	ctx := context.Background()
	server := NewServer()
	server.worker = database.NewWorker()
	server.worker.Start()
	t.Cleanup(func() { server.worker.Stop() })
	server.stateMu.Lock()
	server.dbConn = &database.DBConnection{Driver: driver}
	server.connectionState = connectionReady
	server.connGeneration = 1
	server.stateMu.Unlock()
	server.metadata.Reset(1)
	clientEnd, serverEnd := net.Pipe()
	client := newDiagnosticsClient()
	clientConn := jsonrpc2.NewConn(ctx, jsonrpc2.NewBufferedStream(clientEnd, jsonrpc2.VSCodeObjectCodec{}), client)
	serverConn := jsonrpc2.NewConn(ctx, jsonrpc2.NewBufferedStream(serverEnd, jsonrpc2.VSCodeObjectCodec{}), NewDispatcher(jsonrpc2.HandlerWithError(server.Handle)))
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	return &diagnosticsTestContext{server: server, clientConn: clientConn, serverConn: serverConn, client: client, ctx: ctx}
}

func diagnosticsRepository(types map[string]string) *database.MockDBRepository {
	columns := make([]*database.ColumnDesc, 0, len(types))
	for key, typeName := range types {
		parts := strings.SplitN(key, ".", 2)
		if len(parts) != 2 {
			continue
		}
		columns = append(columns, &database.ColumnDesc{
			ColumnBase: database.ColumnBase{Schema: "TEST", Table: parts[0], Name: parts[1]},
			Type:       typeName,
		})
	}
	return &database.MockDBRepository{
		MockDatabase: func(context.Context) (string, error) { return "TEST", nil },
		MockDatabases: func(context.Context) ([]string, error) {
			return []string{"TEST"}, nil
		},
		MockDatabaseTables: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"TEST": {"DST", "SRC"}}, nil
		},
		MockDescribeDatabaseTable: func(context.Context) ([]*database.ColumnDesc, error) {
			return columns, nil
		},
		MockDescribeDatabaseTableBySchema: func(context.Context, string) ([]*database.ColumnDesc, error) {
			return columns, nil
		},
		MockDescribeForeignKeysBySchema: func(context.Context, string) ([]*database.ForeignKey, error) {
			return nil, nil
		},
	}
}

func waitForDiagnosticCacheReplacement(t *testing.T, worker *database.Worker, previous *database.DBCache) *database.DBCache {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cache := worker.Cache(); cache != nil && cache != previous {
			return cache
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for asynchronous cache replacement")
	return nil
}

func diagnosticTypes(destinationType string) map[string]string {
	return map[string]string{
		"DST.VALUE": destinationType,
		"SRC.VALUE": "VARCHAR(40)",
	}
}

func (tx *diagnosticsTestContext) call(t *testing.T, method string, params interface{}) {
	t.Helper()
	if err := tx.clientConn.Call(tx.ctx, method, params, nil); err != nil {
		t.Fatalf("Call %s: %v", method, err)
	}
}

func (tx *diagnosticsTestContext) open(t *testing.T, uri, text string, version int) {
	t.Helper()
	tx.call(t, "textDocument/didOpen", lsp.DidOpenTextDocumentParams{
		TextDocument: lsp.TextDocumentItem{URI: uri, LanguageID: "sql", Version: version, Text: text},
	})
}

func (tx *diagnosticsTestContext) change(t *testing.T, uri, text string, version int) {
	t.Helper()
	tx.call(t, "textDocument/didChange", lsp.DidChangeTextDocumentParams{
		TextDocument:   lsp.VersionedTextDocumentIdentifier{URI: uri, Version: version},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: text}},
	})
}

func assertVersion(t *testing.T, notification diagnosticsNotification, want int) {
	t.Helper()
	if notification.Version == nil || *notification.Version != want {
		t.Fatalf("diagnostic version = %v, want %d", notification.Version, want)
	}
}

func diagnosticCode(diagnostic lsp.Diagnostic) string {
	if diagnostic.Code == nil {
		return ""
	}
	return *diagnostic.Code
}

func TestDiagnosticUniqueKeysRequiresCompleteCatalogInputs(t *testing.T) {
	cache := &database.DBCache{
		Catalog: &database.CatalogCache{
			Views: map[string]*database.ViewDesc{},
			Indexes: map[string]*database.IndexDesc{
				"UQ_T": {Name: "UQ_T", RelationName: "T", Columns: []string{"ID"}, Unique: sql.NullBool{Bool: true, Valid: true}, Active: sql.NullBool{Bool: true, Valid: true}},
			},
			IndexesByTable: map[string][]*database.IndexDesc{"T": {{Name: "UQ_T", RelationName: "T", Columns: []string{"ID"}, Unique: sql.NullBool{Bool: true, Valid: true}, Active: sql.NullBool{Bool: true, Valid: true}}}},
		},
		Metadata: map[database.MetadataKind]database.MetadataState{
			database.MetadataColumnsCurrent: database.MetadataReady,
			database.MetadataViews:          database.MetadataLoading,
			database.MetadataIndexes:        database.MetadataReady,
		},
	}
	columns := []sqlsymbol.ColumnType{{Name: "ID", Type: "INTEGER"}}
	if _, known := diagnosticUniqueKeys(cache, "T", columns); known {
		t.Fatal("incomplete view classification cannot prove table keys")
	}
	cache.Metadata[database.MetadataViews] = database.MetadataReady
	cache.Metadata[database.MetadataIndexes] = database.MetadataLoading
	if _, known := diagnosticUniqueKeys(cache, "T", columns); known {
		t.Fatal("loading indexes cannot prove a complete unique-key set")
	}
	cache.Metadata[database.MetadataIndexes] = database.MetadataReady
	cache.Metadata[database.MetadataColumnsCurrent] = database.MetadataFailed
	if _, known := diagnosticUniqueKeys(cache, "T", columns); known {
		t.Fatal("failed columns cannot prove a complete unique-key set")
	}
	cache.Metadata[database.MetadataColumnsCurrent] = database.MetadataReady
	keys, known := diagnosticUniqueKeys(cache, "T", columns)
	if !known || len(keys) != 1 || len(keys[0]) != 1 || keys[0][0] != "ID" {
		t.Fatalf("complete catalog keys = %v, known=%v; want [[ID]], true", keys, known)
	}
	cache.Catalog.Indexes = map[string]*database.IndexDesc{}
	cache.Catalog.IndexesByTable = map[string][]*database.IndexDesc{}
	keys, known = diagnosticUniqueKeys(cache, "T", columns)
	if !known || len(keys) != 0 {
		t.Fatalf("ready-empty indexes keys = %v, known=%v; want empty, true", keys, known)
	}
	cache.Metadata[database.MetadataColumnsCurrent] = database.MetadataFailed
	cache.Metadata[database.MetadataColumnsAll] = database.MetadataReady
	if _, known := diagnosticUniqueKeys(cache, "T", columns); !known {
		t.Fatal("all-columns readiness should satisfy the columns requirement")
	}
}

func diagnosticSource(diagnostic lsp.Diagnostic) string {
	if diagnostic.Source == nil {
		return ""
	}
	return *diagnostic.Source
}

func utf16Units(text string) int {
	return len(utf16.Encode([]rune(text)))
}

func TestPublishInterBaseDiagnosticsAndLifecycle(t *testing.T) {
	const uri = "file:///diagnostics-lifecycle.sql"
	text := "/*😀*/ CREATE PROCEDURE P AS DECLARE VARIABLE UNUSED VARCHAR(10); BEGIN UNUSED = 'x'; INSERT INTO DST (VALUE) SELECT SRC.VALUE FROM SRC; END"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	loadMetadataForTest(t, tx.server, diagnosticsRepository(diagnosticTypes("VARCHAR(20)")))
	tx.open(t, uri, text, 1)

	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 2
	})
	assertVersion(t, initial, 1)
	byCode := map[string]lsp.Diagnostic{}
	for _, diagnostic := range initial.Diagnostics {
		byCode[diagnosticCode(diagnostic)] = diagnostic
		if diagnosticSource(diagnostic) != "sqls" {
			t.Errorf("diagnostic source = %q, want sqls", diagnosticSource(diagnostic))
		}
	}
	unused, ok := byCode["interbase-unused"]
	if !ok {
		t.Fatalf("diagnostics = %+v, missing interbase-unused", initial.Diagnostics)
	}
	if unused.Severity != 4 {
		t.Errorf("unused severity = %d, want 4", unused.Severity)
	}
	truncation, ok := byCode["interbase-string-truncation"]
	if !ok {
		t.Fatalf("diagnostics = %+v, missing interbase-string-truncation", initial.Diagnostics)
	}
	if truncation.Severity != 2 {
		t.Errorf("truncation severity = %d, want 2", truncation.Severity)
	}
	projectionStart := strings.Index(text, "SRC.VALUE")
	wantStart := utf16Units(text[:projectionStart])
	if truncation.Range.Start != (lsp.Position{Character: wantStart}) || truncation.Range.End != (lsp.Position{Character: wantStart + len("SRC.VALUE")}) {
		t.Errorf("truncation range = %+v, want character range %d..%d after supplementary rune", truncation.Range, wantStart, wantStart+len("SRC.VALUE"))
	}
	unusedStart := strings.Index(text, "UNUSED")
	wantUnusedStart := utf16Units(text[:unusedStart])
	if unused.Range.Start != (lsp.Position{Character: wantUnusedStart}) {
		t.Errorf("unused range starts at %+v, want character %d after supplementary rune", unused.Range.Start, wantUnusedStart)
	}

	fixed := "/*😀*/ CREATE PROCEDURE P AS DECLARE VARIABLE UNUSED VARCHAR(10); BEGIN UNUSED = UNUSED; INSERT INTO DST (VALUE) VALUES ('x'); END"
	tx.change(t, uri, fixed, 2)
	cleared := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 2 && len(n.Diagnostics) == 0
	})
	assertVersion(t, cleared, 2)

	saved := "/*😀*/ CREATE PROCEDURE P AS DECLARE VARIABLE SAVED_UNUSED VARCHAR(4); BEGIN SAVED_UNUSED = 'x'; END"
	tx.call(t, "textDocument/didSave", lsp.DidSaveTextDocumentParams{
		Text:         saved,
		TextDocument: lsp.TextDocumentIdentifier{URI: uri},
	})
	saveDiagnostics := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 2 && len(n.Diagnostics) == 1
	})
	if diagnosticCode(saveDiagnostics.Diagnostics[0]) != "interbase-unused" {
		t.Fatalf("save diagnostics = %+v, want unused hint", saveDiagnostics.Diagnostics)
	}
	tx.call(t, "textDocument/didSave", lsp.DidSaveTextDocumentParams{
		Text:         "",
		TextDocument: lsp.TextDocumentIdentifier{URI: uri},
	})
	emptySave := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 2 && len(n.Diagnostics) == 0
	})
	assertVersion(t, emptySave, 2)

	tx.call(t, "textDocument/didClose", lsp.DidCloseTextDocumentParams{TextDocument: lsp.TextDocumentIdentifier{URI: uri}})
	closed := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version == nil && len(n.Diagnostics) == 0 })
	if len(closed.Diagnostics) != 0 {
		t.Fatalf("close diagnostics = %+v, want empty", closed.Diagnostics)
	}
	if closed.Diagnostics == nil {
		t.Fatal("close diagnostics is null, want an empty list")
	}
}

func TestInterBaseDiagnosticsAcceptance(t *testing.T) {
	const uri = "file:///diagnostics-acceptance.sql"
	initialText := "CREATE PROCEDURE P AS DECLARE VARIABLE LOCAL_VALUE VARCHAR(10); BEGIN INSERT INTO DST (VALUE) SELECT SRC.VALUE FROM SRC; END"
	fixedText := "CREATE PROCEDURE P AS DECLARE VARIABLE LOCAL_VALUE VARCHAR(10); BEGIN LOCAL_VALUE = LOCAL_VALUE; INSERT INTO DST (VALUE) SELECT CAST(SRC.VALUE AS VARCHAR(20)) FROM SRC; END"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	loadMetadataForTest(t, tx.server, diagnosticsRepository(diagnosticTypes("VARCHAR(20)")))
	tx.open(t, uri, initialText, 1)

	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1
	})
	assertVersion(t, initial, 1)
	if len(initial.Diagnostics) != 2 {
		t.Fatalf("initial diagnostics = %+v, want unused hint and truncation warning", initial.Diagnostics)
	}
	byCode := make(map[string]lsp.Diagnostic, len(initial.Diagnostics))
	for _, diagnostic := range initial.Diagnostics {
		byCode[diagnosticCode(diagnostic)] = diagnostic
		if diagnosticSource(diagnostic) != "sqls" {
			t.Errorf("diagnostic source = %q, want sqls", diagnosticSource(diagnostic))
		}
	}
	if unused, ok := byCode["interbase-unused"]; !ok {
		t.Errorf("initial diagnostics = %+v, missing interbase-unused", initial.Diagnostics)
	} else if unused.Severity != 4 {
		t.Errorf("unused severity = %d, want 4", unused.Severity)
	}
	if truncation, ok := byCode["interbase-string-truncation"]; !ok {
		t.Errorf("initial diagnostics = %+v, missing interbase-string-truncation", initial.Diagnostics)
	} else if truncation.Severity != 2 {
		t.Errorf("truncation severity = %d, want 2", truncation.Severity)
	}

	tx.change(t, uri, fixedText, 2)
	cleared := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 2
	})
	assertVersion(t, cleared, 2)
	if len(cleared.Diagnostics) != 0 {
		t.Fatalf("fixed diagnostics = %+v, want an empty list", cleared.Diagnostics)
	}
	if cleared.Diagnostics == nil {
		t.Fatal("fixed diagnostics is null, want an empty list")
	}
}

func TestInterBaseDiagnosticsAcceptanceWithoutCatalog(t *testing.T) {
	const uri = "file:///diagnostics-acceptance-no-catalog.sql"
	text := "CREATE PROCEDURE P AS DECLARE VARIABLE LOCAL_VALUE VARCHAR(10); BEGIN INSERT INTO DST (VALUE) SELECT SRC.VALUE FROM SRC; END"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, text, 1)

	notification := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1
	})
	assertVersion(t, notification, 1)
	if len(notification.Diagnostics) != 1 || diagnosticCode(notification.Diagnostics[0]) != "interbase-unused" {
		t.Fatalf("missing-catalog diagnostics = %+v, want only interbase-unused", notification.Diagnostics)
	}
	if notification.Diagnostics[0].Severity != 4 {
		t.Errorf("unused severity = %d, want 4", notification.Diagnostics[0].Severity)
	}
}

func TestPublishDiagnosticsUsesFreshCacheSnapshot(t *testing.T) {
	const uri = "file:///diagnostics-cache.sql"
	text := "CREATE PROCEDURE P AS BEGIN INSERT INTO DST (VALUE) SELECT SRC.VALUE FROM SRC; END"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	loadMetadataForTest(t, tx.server, diagnosticsRepository(diagnosticTypes("VARCHAR(20)")))
	tx.open(t, uri, text, 1)
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 1
	})
	if diagnosticCode(initial.Diagnostics[0]) != "interbase-string-truncation" {
		t.Fatalf("initial diagnostics = %+v, want truncation warning", initial.Diagnostics)
	}

	oldCache := tx.server.metadata.Cache()
	loadMetadataForTest(t, tx.server, diagnosticsRepository(diagnosticTypes("VARCHAR(50)")))
	primaryCache := tx.server.metadata.Cache()
	if primaryCache == oldCache {
		t.Fatal("metadata cache pointer did not refresh")
	}
	refreshed := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 0
	})
	if len(refreshed.Diagnostics) != 0 {
		t.Fatalf("refreshed diagnostics = %+v, want empty with fresh fitting cache", refreshed.Diagnostics)
	}
}

func TestPublishDiagnosticsOmitsInterBaseFindingsForOtherDialects(t *testing.T) {
	const uri = "file:///diagnostics-other-dialect.sql"
	text := "CREATE PROCEDURE P AS DECLARE VARIABLE UNUSED VARCHAR(10); BEGIN UNUSED = 'x'; END"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverMySQL)
	tx.open(t, uri, text, 1)
	notification := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1
	})
	if len(notification.Diagnostics) != 0 {
		t.Fatalf("non-InterBase diagnostics = %+v, want none", notification.Diagnostics)
	}
	if notification.Diagnostics == nil {
		t.Fatal("non-InterBase diagnostics is null, want an empty list")
	}
}

func TestPublishMalformedEditClearsFindingsWithoutFailingChange(t *testing.T) {
	const uri = "file:///diagnostics-malformed.sql"
	valid := "CREATE PROCEDURE P AS DECLARE VARIABLE UNUSED VARCHAR(10); BEGIN UNUSED = 'x'; END"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, valid, 1)
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1
	})
	if len(initial.Diagnostics) != 1 {
		t.Fatalf("initial diagnostics = %+v, want unused hint", initial.Diagnostics)
	}

	// The document notification succeeds even though its incomplete declaration
	// prevents the current source from proving any findings.
	tx.change(t, uri, "CREATE PROCEDURE P AS DECLARE VARIABLE UNUSED BEGIN", 2)
	malformed := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 2 && len(n.Diagnostics) == 0
	})
	assertVersion(t, malformed, 2)
	if malformed.Diagnostics == nil {
		t.Fatal("malformed edit diagnostics is null, want an empty list")
	}
}

func TestDiagnosticsPreserveValidProcedureAfterMalformedDeclaration(t *testing.T) {
	text := `CREATE PROCEDURE BROKEN AS
DECLARE VARIABLE INVALID BEGIN
END;
CREATE PROCEDURE VALID AS
DECLARE VARIABLE UNUSED VARCHAR(10);
BEGIN UNUSED = 'x'; END`
	variant := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}
	if _, err := sqlsymbol.Analyze(text, variant); err == nil {
		t.Fatal("navigation analysis must retain its strict parse-error behavior")
	}

	got := diagnosticsForSnapshot(documentDiagnosticsSnapshot{
		uri:     "file:///malformed-and-valid.sql",
		text:    text,
		variant: variant,
	})
	if len(got) != 1 {
		t.Fatalf("diagnostics = %+v, want the valid procedure's unused hint", got)
	}
	if diagnosticCode(got[0]) != "interbase-unused" {
		t.Fatalf("diagnostic code = %q, want interbase-unused", diagnosticCode(got[0]))
	}
	want := lsp.Range{Start: lsp.Position{Line: 4, Character: 17}, End: lsp.Position{Line: 4, Character: 23}}
	if got[0].Range != want {
		t.Fatalf("valid procedure diagnostic range = %+v, want %+v", got[0].Range, want)
	}
}

func TestPublishDiagnosticsDoesNotSendDelayedOlderVersion(t *testing.T) {
	const uri = "file:///diagnostics-versions.sql"
	text := "CREATE PROCEDURE P AS DECLARE VARIABLE UNUSED VARCHAR(10); BEGIN UNUSED = 'x'; END"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, text, 3)
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 3
	})
	if len(initial.Diagnostics) != 1 {
		t.Fatalf("version 3 diagnostics = %+v, want unused hint", initial.Diagnostics)
	}

	// Keep the old request's computed result, then let the newer edit finish
	// before attempting that delayed publication.
	oldSnapshot, ok := tx.server.diagnosticsSnapshot(uri)
	if !ok {
		t.Fatal("could not snapshot open document")
	}
	oldDiagnostics := diagnosticsForSnapshot(oldSnapshot)
	fixed := "CREATE PROCEDURE P AS DECLARE VARIABLE UNUSED VARCHAR(10); BEGIN UNUSED = UNUSED; END"
	tx.change(t, uri, fixed, 4)
	latest := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 4 && len(n.Diagnostics) == 0
	})
	assertVersion(t, latest, 4)

	tx.server.publishDiagnosticsSnapshot(tx.ctx, tx.serverConn, oldSnapshot, oldDiagnostics)
	if err := tx.serverConn.Notify(tx.ctx, "test/notificationBarrier", struct{}{}); err != nil {
		t.Fatal("send notification barrier:", err)
	}
	select {
	case <-tx.client.barrier:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for notification barrier")
	}
	tx.client.mu.Lock()
	defer tx.client.mu.Unlock()
	for _, notification := range tx.client.notifications {
		if notification.URI == uri && notification.Version != nil && *notification.Version == 3 {
			t.Fatalf("delayed version 3 diagnostics were published after version 4: %+v", notification)
		}
	}
}

func TestPublishDiagnosticsDoesNotSendPriorConnectionGeneration(t *testing.T) {
	const uri = "file:///diagnostics-generation.sql"
	text := "CREATE PROCEDURE P AS DECLARE VARIABLE UNUSED VARCHAR(10); BEGIN UNUSED = 'x'; END"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, text, 1)
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 1
	})
	if len(initial.Diagnostics) != 1 {
		t.Fatalf("initial diagnostics = %+v, want unused hint", initial.Diagnostics)
	}
	oldSnapshot, ok := tx.server.diagnosticsSnapshot(uri)
	if !ok {
		t.Fatal("could not snapshot open document")
	}
	oldDiagnostics := diagnosticsForSnapshot(oldSnapshot)

	// Model the fenced state transition used by reconnectionDB. The new
	// attachment is non-InterBase, so republishing must clear its old findings.
	tx.server.diagnosticsPublishMu.Lock()
	tx.server.stateMu.Lock()
	tx.server.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverMySQL}
	tx.server.connGeneration++
	tx.server.stateMu.Unlock()
	tx.server.diagnosticsPublishMu.Unlock()
	tx.server.republishOpenDiagnostics(tx.ctx)
	cleared := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) == 0
	})
	assertVersion(t, cleared, 1)

	tx.server.publishDiagnosticsSnapshot(tx.ctx, tx.serverConn, oldSnapshot, oldDiagnostics)
	if err := tx.serverConn.Notify(tx.ctx, "test/notificationBarrier", struct{}{}); err != nil {
		t.Fatal("send notification barrier:", err)
	}
	select {
	case <-tx.client.barrier:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for notification barrier")
	}
	tx.client.mu.Lock()
	defer tx.client.mu.Unlock()
	for _, notification := range tx.client.notifications {
		if notification.URI == uri && len(notification.Diagnostics) != 0 {
			t.Fatalf("prior-generation diagnostics were published after connection switch: %+v", notification)
		}
	}
}

func TestDidOpenAndChangeDoNotRestoreInitialSnapshot(t *testing.T) {
	const uri = "file:///open-change-order.sql"
	server := &Server{files: make(map[string]*File)}
	initialText := "CREATE PROCEDURE P AS BEGIN INSERT INTO DST (VALUE) VALUES ('old'); END"
	if err := server.openFileAtVersion(uri, "sql", initialText, 1); err != nil {
		t.Fatal("install didOpen snapshot:", err)
	}
	got, ok := server.fileText(uri)
	if !ok || got != initialText {
		t.Fatalf("didOpen text = (%q, %v), want initial text installed atomically", got, ok)
	}
	server.stateMu.RLock()
	openVersion := server.files[uri].Version
	server.stateMu.RUnlock()
	if openVersion != 1 {
		t.Fatalf("didOpen version = %d, want 1", openVersion)
	}
	newerText := "CREATE PROCEDURE P AS BEGIN INSERT INTO DST (VALUE) VALUES ('new'); END"
	newerVersion := 2
	changed, err := server.updateFileVersion(uri, newerText, &newerVersion)
	if err != nil || !changed {
		t.Fatalf("apply newer didChange = (%v, %v), want (true, nil)", changed, err)
	}

	got, ok = server.fileText(uri)
	if !ok || got != newerText {
		t.Fatalf("document after ordered open/change = (%q, %v), want newer text", got, ok)
	}
	server.stateMu.RLock()
	version := server.files[uri].Version
	server.stateMu.RUnlock()
	if version != newerVersion {
		t.Fatalf("document version = %d, want %d", version, newerVersion)
	}
}

func TestDidSaveCannotOverwriteChangeAppliedAfterSaveSnapshot(t *testing.T) {
	const uri = "file:///save-change-order.sql"
	server := &Server{files: make(map[string]*File)}
	initialText := "CREATE PROCEDURE P AS BEGIN INSERT INTO DST (VALUE) VALUES ('initial'); END"
	if err := server.openFileAtVersion(uri, "sql", initialText, 1); err != nil {
		t.Fatal("install didOpen snapshot:", err)
	}
	server.stateMu.RLock()
	saveRevision := server.files[uri].Revision
	server.stateMu.RUnlock()

	newerText := "CREATE PROCEDURE P AS BEGIN INSERT INTO DST (VALUE) VALUES ('edited'); END"
	newerVersion := 2
	changed, err := server.updateFileVersion(uri, newerText, &newerVersion)
	if err != nil || !changed {
		t.Fatalf("apply newer didChange = (%v, %v), want (true, nil)", changed, err)
	}
	staleSavedText := "CREATE PROCEDURE P AS BEGIN INSERT INTO DST (VALUE) VALUES ('saved earlier'); END"
	applied, err := server.updateFileAtRevision(uri, staleSavedText, saveRevision)
	if err != nil {
		t.Fatal("apply delayed didSave text:", err)
	}
	if applied {
		t.Fatal("delayed didSave text replaced a document changed after the save snapshot")
	}
	got, ok := server.fileText(uri)
	if !ok || got != newerText {
		t.Fatalf("document after overlapping save/change = (%q, %v), want newer edit", got, ok)
	}
	server.stateMu.RLock()
	version := server.files[uri].Version
	server.stateMu.RUnlock()
	if version != newerVersion {
		t.Fatalf("document version = %d, want %d", version, newerVersion)
	}
}
