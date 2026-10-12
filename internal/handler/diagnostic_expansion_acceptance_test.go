package handler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/metadata"
)

func TestDiagnosticsRejectOldResultAfterStatusOnlyRevision(t *testing.T) {
	const uri = "file:///diagnostics-status-revision.sql"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	if err := tx.server.openFileAtVersion(uri, "sql", "SELECT MISSING_COL FROM T;", 1); err != nil {
		t.Fatal(err)
	}
	domainEntered, releaseDomain := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseDomain:
		default:
			close(releaseDomain)
		}
	})
	plan := readinessPlanRepository{
		DBRepository: database.NewMockCapabilityRepository(),
		plan: database.MetadataPlan{Parallelism: 3, Jobs: []database.MetadataJob{
			{Kind: database.MetadataRelations, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
				return database.MetadataPatch{Cache: &database.DBCache{SchemaTables: map[string][]string{"": {"T"}}}}, nil
			}},
			{Kind: database.MetadataViews, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
				return database.MetadataPatch{Cache: &database.DBCache{Catalog: &database.CatalogCache{Views: map[string]*database.ViewDesc{}, ViewsByID: map[string]*database.ViewDesc{}}}}, nil
			}},
			{Kind: database.MetadataProcedures, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
				return database.MetadataPatch{Cache: &database.DBCache{Catalog: &database.CatalogCache{Procedures: map[string]*database.ProcedureDesc{}, ProceduresByID: map[string]*database.ProcedureDesc{}}}}, nil
			}},
			{Kind: database.MetadataColumnsCurrent, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
				return database.MetadataPatch{Cache: &database.DBCache{ColumnsByID: map[string][]*database.ColumnDesc{
					catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}.Key(): {{ColumnBase: catalog.ColumnBase{Table: "T", Name: "ID"}, Type: "INTEGER", Null: "NO"}},
				}, ColumnsWithParent: map[string][]*database.ColumnDesc{
					catalog.ColumnDatabaseKey("", "T"): {{ColumnBase: catalog.ColumnBase{Table: "T", Name: "ID"}, Type: "INTEGER", Null: "NO"}},
				}}}, nil
			}},
			{Kind: database.MetadataDomains, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
				close(domainEntered)
				<-releaseDomain
				return database.MetadataPatch{}, errors.New("simulated status-only domain failure")
			}},
		}},
	}
	lease := bindMetadataRepoForTest(t, tx.server, 1, plan)
	select {
	case <-domainEntered:
	case <-time.After(3 * time.Second):
		t.Fatalf("domain category did not enter its gate: snapshot=%+v", lease.Snapshot())
	}
	old := waitForDiagnosticReadiness(t, lease)
	if old.Status[database.MetadataDomains].State != catalog.MetadataLoading {
		t.Fatalf("domain status = %s, want loading before status-only publication", old.Status[database.MetadataDomains].State)
	}

	enteredAnalysis, releaseAnalysis := make(chan struct{}), make(chan struct{})
	var analysisOnce sync.Once
	tx.server.diagnosticAnalyzer = func(snapshot documentDiagnosticsSnapshot) []lsp.Diagnostic {
		analysisOnce.Do(func() { close(enteredAnalysis) })
		<-releaseAnalysis
		return diagnosticsForSnapshot(context.Background(), snapshot)
	}
	done := make(chan struct{})
	go func() {
		tx.server.publishDocumentDiagnostics(tx.ctx, tx.serverConn, uri)
		close(done)
	}()
	select {
	case <-enteredAnalysis:
	case <-time.After(2 * time.Second):
		t.Fatal("diagnostic computation did not enter its gate")
	}

	close(releaseDomain)
	current := waitForStatusRevision(t, lease, old.Revision)
	if current.Generation != old.Generation || current.Revision <= old.Revision || current.Cache != old.Cache {
		t.Fatalf("expected status-only metadata publication (old generation/revision/cache %d/%d/%p, current %d/%d/%p)",
			old.Generation, old.Revision, old.Cache, current.Generation, current.Revision, current.Cache)
	}
	if current.Status[database.MetadataDomains].State != catalog.MetadataFailed {
		t.Fatalf("domain status = %s, want failed", current.Status[database.MetadataDomains].State)
	}
	close(releaseAnalysis)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("old diagnostic computation did not finish")
	}
	assertNoDiagnosticNotification(t, tx.client, uri)

	tx.server.diagnosticAnalyzer = nil
	tx.server.publishDocumentDiagnostics(tx.ctx, tx.serverConn, uri)
	notification := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil })
	if len(notification.Diagnostics) != 1 || diagnosticCode(notification.Diagnostics[0]) != "interbase-unknown-column" {
		t.Fatalf("current readiness diagnostics = %+v, want one unknown-column finding", notification.Diagnostics)
	}
}

func TestDiagnosticExpansionFailedColumnRefreshKeepsOnlyTextFinding(t *testing.T) {
	const uri = "file:///diagnostic-column-refresh-failure.sql"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	provider := &failedColumnRefreshProvider{}
	key := metadata.Key{Engine: "interbase", URL: "fixture://failed-column-refresh"}
	cfg := &database.DBConfig{
		Driver: dialect.DatabaseDriverInterBase, DataSourceName: "fixture://failed-column-refresh", Dialect: 3,
		InterBase: &database.InterBaseConfig{Role: "DIAGNOSTIC_ROLE"},
	}
	tx.server.openConnection = func(context.Context, *database.DBConfig) (*database.DBConnection, error) {
		return &database.DBConnection{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}, nil
	}
	tx.server.stateMu.Lock()
	tx.server.metadataFactoryOverride = func(*database.DBConfig) (metadata.Key, metadata.Scope, metadata.Factory, bool) {
		return key, metadata.Scope{Principal: "diagnostic-user", Role: "DIAGNOSTIC_ROLE"}, func(context.Context) (metadata.Provider, error) {
			return provider, nil
		}, true
	}
	tx.server.stateMu.Unlock()
	if err := awaitConnectionIntent(context.Background(), tx.server.coordinator.RequestExplicit(context.Background(), cfg, 0, "")); err != nil {
		t.Fatalf("initial attach: %v", err)
	}
	waitCurrentMetadataSettledForTest(t, context.Background(), tx.server)
	lease := currentLeaseForTest(tx.server)
	if err := tx.clientConn.Call(tx.ctx, "textDocument/didOpen", lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{
		URI: uri, LanguageID: "sql", Version: 1, Text: "SELECT MISSING_COL FROM T WHERE V = NULL;",
	}}, nil); err != nil {
		t.Fatalf("open document: %v", err)
	}
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	if !hasDiagnosticCode(initial.Diagnostics, "interbase-unknown-column") || !hasDiagnosticCode(initial.Diagnostics, "interbase-null-comparison") {
		t.Fatalf("initial ready-catalog findings = %+v, want unknown-column and null-comparison", initial.Diagnostics)
	}
	tx.client.drain(uri)
	tx.server.queueAllDiagnostics()
	duplicate := tx.client.waitPending(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	if !hasDiagnosticCode(duplicate.Diagnostics, "interbase-unknown-column") || !hasDiagnosticCode(duplicate.Diagnostics, "interbase-null-comparison") {
		t.Fatalf("duplicate ready-catalog findings = %+v, want unknown-column and null-comparison", duplicate.Diagnostics)
	}
	initialGeneration := lease.Snapshot().Generation
	if err := tx.serverConn.Notify(tx.ctx, "test/notificationBarrier", struct{}{}); err != nil {
		t.Fatal("send pre-refresh barrier:", err)
	}
	select {
	case <-tx.client.barrier:
	case <-time.After(time.Second):
		t.Fatal("pre-refresh notification barrier timed out")
	}
	tx.client.drain(uri)
	tx.server.stateMu.Lock()
	tx.server.notificationConn = nil // Assert the final serialized publication explicitly below.
	tx.server.stateMu.Unlock()
	if err := awaitConnectionIntent(context.Background(), tx.server.coordinator.RequestExplicit(context.Background(), cfg, 0, "")); err != nil {
		t.Fatalf("same-target refresh: %v", err)
	}
	waitCurrentMetadataSettledForTest(t, context.Background(), tx.server)
	if currentLeaseForTest(tx.server) != lease {
		t.Fatal("same-target refresh replaced the active registry lease")
	}
	refreshed := lease.Snapshot()
	if refreshed.Generation <= initialGeneration || refreshed.Status[catalog.MetadataRelations].State != catalog.MetadataReady ||
		refreshed.Status[catalog.MetadataViews].State != catalog.MetadataReady || refreshed.Status[catalog.MetadataProcedures].State != catalog.MetadataReady ||
		refreshed.Status[catalog.MetadataColumnsCurrent].State != catalog.MetadataFailed {
		t.Fatalf("refresh categories/generation = %d %+v; want ready namespaces, failed columns, generation > %d", refreshed.Generation, refreshed.Status, initialGeneration)
	}
	tx.server.publishDocumentDiagnostics(tx.ctx, tx.serverConn, uri)
	if err := tx.serverConn.Notify(tx.ctx, "test/notificationBarrier", struct{}{}); err != nil {
		t.Fatal("send post-refresh notification barrier:", err)
	}
	select {
	case <-tx.client.barrier:
	case <-time.After(time.Second):
		t.Fatal("post-refresh notification barrier timed out")
	}
	final := tx.client.last(t, uri)
	if final.Version == nil || *final.Version != 1 {
		t.Fatalf("last post-refresh diagnostics version = %v, want 1", final.Version)
	}
	if len(final.Diagnostics) != 1 || diagnosticCode(final.Diagnostics[0]) != "interbase-null-comparison" {
		t.Fatalf("final diagnostics after failed column refresh = %+v, want only independent text finding", final.Diagnostics)
	}
}

func hasDiagnosticCode(diagnostics []lsp.Diagnostic, want string) bool {
	for _, diagnostic := range diagnostics {
		if diagnosticCode(diagnostic) == want {
			return true
		}
	}
	return false
}

type failedColumnRefreshProvider struct{ plans atomic.Int32 }

func (p *failedColumnRefreshProvider) Plan() catalog.MetadataPlan {
	refresh := p.plans.Add(1) > 1
	return catalog.MetadataPlan{Parallelism: 3, Jobs: []catalog.MetadataJob{
		{Kind: catalog.MetadataRelations, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{Cache: &catalog.Cache{SchemaTables: map[string][]string{"": {"T"}}}}, nil
		}},
		{Kind: catalog.MetadataViews, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Views: map[string]*catalog.ViewDesc{}}}}, nil
		}},
		{Kind: catalog.MetadataProcedures, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Procedures: map[string]*catalog.ProcedureDesc{}}}}, nil
		}},
		{Kind: catalog.MetadataColumnsCurrent, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			if refresh {
				return catalog.MetadataPatch{}, errors.New("simulated columns refresh failure")
			}
			column := &catalog.ColumnDesc{ColumnBase: catalog.ColumnBase{Table: "T", Name: "ID"}, Type: "INTEGER", Null: "NO"}
			return catalog.MetadataPatch{Cache: &catalog.Cache{
				ColumnsByID:       map[string][]*catalog.ColumnDesc{catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}.Key(): {column}},
				ColumnsWithParent: map[string][]*catalog.ColumnDesc{catalog.ColumnDatabaseKey("", "T"): {column}},
			}}, nil
		}},
	}}
}

func (*failedColumnRefreshProvider) DetailPlan(metadata.Request) (metadata.DetailPlan, error) {
	return metadata.DetailPlan{}, metadata.ErrUnsupported
}

func (*failedColumnRefreshProvider) Close() error { return nil }

func waitForStatusRevision(t *testing.T, lease interface {
	Snapshot() *catalog.MetadataSnapshot
}, after uint64) *catalog.MetadataSnapshot {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		snapshot := lease.Snapshot()
		if snapshot != nil && snapshot.Revision > after && snapshot.Status[catalog.MetadataDomains].State == catalog.MetadataFailed {
			return snapshot
		}
		select {
		case <-deadline:
			t.Fatalf("status-only metadata update not observed after revision %d: %+v", after, snapshot)
			return nil
		case <-time.After(time.Millisecond):
		}
	}
}

func waitForDiagnosticReadiness(t *testing.T, lease interface {
	Snapshot() *catalog.MetadataSnapshot
}) *catalog.MetadataSnapshot {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		snapshot := lease.Snapshot()
		if snapshot != nil &&
			snapshot.Status[catalog.MetadataRelations].State == catalog.MetadataReady &&
			snapshot.Status[catalog.MetadataViews].State == catalog.MetadataReady &&
			snapshot.Status[catalog.MetadataProcedures].State == catalog.MetadataReady &&
			snapshot.Status[catalog.MetadataColumnsCurrent].State == catalog.MetadataReady {
			return snapshot
		}
		select {
		case <-deadline:
			t.Fatalf("metadata categories did not become diagnostically ready: %+v", snapshot)
			return nil
		case <-time.After(time.Millisecond):
		}
	}
}

func assertNoDiagnosticNotification(t *testing.T, client *diagnosticsClient, uri string) {
	t.Helper()
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, notification := range client.notifications {
		if notification.URI == uri && notification.Version != nil {
			t.Fatalf("stale diagnostic result was published after a metadata status revision: %+v", notification)
		}
	}
}

func TestDiagnosticExpansionMixedDocumentIndependentFindingsAndUTF16(t *testing.T) {
	const uri = "file:///diagnostic-expansion-mixed.sql"
	text := "/*😀*/ SELECT ID FROM T WHERE V = NULL;\n" +
		"SELECT FROM;\n" +
		"INSERT INTO T (ID) SELECT ID FROM T WHERE V = NULL;\n" +
		"UPDATE T SET ID = 1 WHERE V = NULL;\n" +
		"CREATE PROCEDURE P AS BEGIN BAD_PROC = 1; END\n" +
		"CREATE TRIGGER TR FOR T BEFORE INSERT AS BEGIN BAD_TRIGGER = 1; END"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, text, 1)
	got := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	want := []struct {
		code             string
		line, start, end int
	}{
		{"interbase-null-comparison", 0, 30, 38},
		{"interbase-null-comparison", 2, 42, 50},
		{"interbase-null-comparison", 3, 26, 34},
		{"interbase-unknown-variable", 4, 28, 36},
		{"interbase-unknown-variable", 5, 47, 58},
	}
	if len(got.Diagnostics) != len(want) {
		t.Fatalf("findings = %+v; want %d independent results", got.Diagnostics, len(want))
	}
	for _, expected := range want {
		found := false
		for _, actual := range got.Diagnostics {
			if diagnosticCode(actual) == expected.code && actual.Range.Start.Line == expected.line {
				found = true
				wantRange := lsp.Range{Start: lsp.Position{Line: expected.line, Character: expected.start}, End: lsp.Position{Line: expected.line, Character: expected.end}}
				if actual.Range != wantRange {
					t.Errorf("%s span = %+v, want exact UTF-16 span %+v", expected.code, actual.Range, wantRange)
				}
			}
		}
		if !found {
			t.Errorf("missing %s at line %d: %+v", expected.code, expected.line, got.Diagnostics)
		}
	}
	for _, actual := range got.Diagnostics {
		if actual.Range.Start.Line == 1 {
			t.Fatalf("incomplete neighbor produced diagnostic: %+v", actual)
		}
	}
	if len(got.rawDiagnostics) == 0 || got.rawDiagnostics[0] != '[' {
		t.Fatalf("diagnostics wire payload = %s, want JSON array", got.rawDiagnostics)
	}
}

func TestDiagnosticExpansionSlowOldVersionDoesNotBlockEditorRequests(t *testing.T) {
	const uri, otherURI = "file:///old-version.sql", "file:///unrelated.sql"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	entered, release := make(chan struct{}), make(chan struct{})
	var block atomic.Bool
	var once sync.Once
	tx.server.diagnosticAnalyzer = func(snapshot documentDiagnosticsSnapshot) []lsp.Diagnostic {
		if block.Load() && snapshot.uri == uri && snapshot.version == 1 {
			once.Do(func() { close(entered) })
			<-release
		}
		return diagnosticsForSnapshot(context.Background(), snapshot)
	}
	tx.open(t, uri, "SELECT ID FROM T WHERE V = NULL;", 1)
	tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	block.Store(true)
	tx.server.queueDiagnosticDocument(uri)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("old-version analysis did not enter its gate")
	}
	request := func(method string, params any) {
		t.Helper()
		ctx, cancel := context.WithTimeout(tx.ctx, 2*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- tx.clientConn.Call(ctx, method, params, nil) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s while diagnostics blocked: %v", method, err)
			}
		case <-ctx.Done():
			t.Fatalf("%s blocked behind old diagnostic work", method)
		}
	}
	request("textDocument/didChange", lsp.DidChangeTextDocumentParams{TextDocument: lsp.VersionedTextDocumentIdentifier{URI: uri, Version: 2}, ContentChanges: []lsp.TextDocumentContentChangeEvent{{Text: "SELECT 1;"}}})
	request("textDocument/hover", lsp.TextDocumentPositionParams{TextDocument: lsp.TextDocumentIdentifier{URI: uri}, Position: lsp.Position{Character: 1}})
	request("textDocument/didOpen", lsp.DidOpenTextDocumentParams{TextDocument: lsp.TextDocumentItem{URI: otherURI, LanguageID: "sql", Version: 1, Text: "SELECT 2;"}})
	block.Store(false)
	close(release)
	latest := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 2 })
	if len(latest.Diagnostics) != 0 || string(latest.rawDiagnostics) != "[]" {
		t.Fatalf("latest diagnostic publication = %+v (%s), want []", latest.Diagnostics, latest.rawDiagnostics)
	}
	if other := tx.client.next(t, otherURI, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 }); other.Version == nil {
		t.Fatal("unrelated open document was not diagnosed")
	}
}

func TestDiagnosticExpansionMetadataCategoryOrdering(t *testing.T) {
	const uri = "file:///diagnostic-category-order.sql"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.open(t, uri, "SELECT MISSING_COL FROM T;", 1)
	initial := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	if len(initial.Diagnostics) != 0 {
		t.Fatalf("pre-metadata findings = %+v, want none", initial.Diagnostics)
	}
	columnsEntered, releaseColumns := make(chan struct{}), make(chan struct{})
	var once sync.Once
	plan := readinessPlanRepository{
		DBRepository: database.NewMockCapabilityRepository(),
		plan: database.MetadataPlan{Parallelism: 3, Jobs: []database.MetadataJob{
			{Kind: database.MetadataRelations, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
				return database.MetadataPatch{Cache: &database.DBCache{SchemaTables: map[string][]string{"": {"T"}}}}, nil
			}},
			{Kind: database.MetadataViews, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
				return database.MetadataPatch{Cache: &database.DBCache{Catalog: &database.CatalogCache{Views: map[string]*database.ViewDesc{}, ViewsByID: map[string]*database.ViewDesc{}}}}, nil
			}},
			{Kind: database.MetadataProcedures, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
				return database.MetadataPatch{Cache: &database.DBCache{Catalog: &database.CatalogCache{Procedures: map[string]*database.ProcedureDesc{}, ProceduresByID: map[string]*database.ProcedureDesc{}}}}, nil
			}},
			{Kind: database.MetadataColumnsCurrent, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
				once.Do(func() { close(columnsEntered) })
				<-releaseColumns
				column := &database.ColumnDesc{ColumnBase: catalog.ColumnBase{Table: "T", Name: "ID"}, Type: "INTEGER", Null: "NO"}
				return database.MetadataPatch{Cache: &database.DBCache{
					ColumnsByID:       map[string][]*database.ColumnDesc{catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}.Key(): {column}},
					ColumnsWithParent: map[string][]*database.ColumnDesc{catalog.ColumnDatabaseKey("", "T"): {column}},
				}}, nil
			}},
		}},
	}
	tx.server.stateMu.RLock()
	generation := tx.server.connGeneration
	tx.server.stateMu.RUnlock()
	lease := bindMetadataRepoForTest(t, tx.server, generation, plan)
	t.Cleanup(func() {
		select {
		case <-releaseColumns:
		default:
			close(releaseColumns)
		}
	})
	select {
	case <-columnsEntered:
	case <-time.After(2 * time.Second):
		t.Fatalf("columns did not enter gate: %+v", lease.Snapshot())
	}
	waitLeaseSnapshot(t, lease, func(snapshot *catalog.MetadataSnapshot) bool {
		return snapshot.Status[catalog.MetadataRelations].State == catalog.MetadataReady &&
			snapshot.Status[catalog.MetadataViews].State == catalog.MetadataReady &&
			snapshot.Status[catalog.MetadataProcedures].State == catalog.MetadataReady &&
			snapshot.Status[catalog.MetadataColumnsCurrent].State == catalog.MetadataLoading
	})
	tx.server.publishDocumentDiagnostics(tx.ctx, tx.serverConn, uri)
	whileLoading := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil && *n.Version == 1 })
	if len(whileLoading.Diagnostics) != 0 {
		t.Fatalf("loading columns produced an unknown-column claim: %+v", whileLoading.Diagnostics)
	}
	close(releaseColumns)
	waitLeaseSnapshot(t, lease, func(snapshot *catalog.MetadataSnapshot) bool { return snapshot.Settled() })
	tx.server.queueDiagnosticDocument(uri)
	ready := tx.client.next(t, uri, func(n diagnosticsNotification) bool {
		return n.Version != nil && *n.Version == 1 && len(n.Diagnostics) > 0
	})
	if len(ready.Diagnostics) != 1 || diagnosticCode(ready.Diagnostics[0]) != "interbase-unknown-column" {
		t.Fatalf("ready catalog findings = %+v, want one unknown-column finding", ready.Diagnostics)
	}
}

func waitLeaseSnapshot(t *testing.T, lease interface {
	Snapshot() *catalog.MetadataSnapshot
}, predicate func(*catalog.MetadataSnapshot) bool) *catalog.MetadataSnapshot {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		snapshot := lease.Snapshot()
		if snapshot != nil && predicate(snapshot) {
			return snapshot
		}
		select {
		case <-deadline:
			t.Fatalf("metadata snapshot condition not reached: %+v", snapshot)
			return nil
		case <-time.After(time.Millisecond):
		}
	}
}

func TestDiagnosticExpansionRejectsInvalidPolicyOverRealRPC(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules map[string]string
	}{
		{"unknown code", map[string]string{"unknown-rule": "error"}},
		{"unknown level", map[string]string{"interbase-null-comparison": "fatal"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
			original := &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: map[string]string{"interbase-null-comparison": "off"}}}
			tx.server.stateMu.Lock()
			tx.server.WSCfg = original
			before := tx.server.policyRevision
			tx.server.stateMu.Unlock()
			params := lsp.DidChangeConfigurationParams{Settings: struct {
				SQLS *config.Config `json:"sqls"`
			}{SQLS: &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: tc.rules}}}}
			ctx, cancel := context.WithTimeout(tx.ctx, 2*time.Second)
			defer cancel()
			if err := tx.clientConn.Call(ctx, "workspace/didChangeConfiguration", params, nil); err == nil {
				t.Fatal("invalid policy was accepted over JSON-RPC")
			}
			tx.server.stateMu.RLock()
			defer tx.server.stateMu.RUnlock()
			if tx.server.WSCfg != original || tx.server.policyRevision != before {
				t.Fatalf("invalid config mutated live state: cfg=%p revision=%d; want cfg=%p revision=%d", tx.server.WSCfg, tx.server.policyRevision, original, before)
			}
		})
	}
}
