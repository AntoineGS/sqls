package handler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

func TestCaptureEditorSnapshotDeepCopiesDiagnosticRules(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop() })
	const uri = "file:///policy-snapshot.sql"
	s.stateMu.Lock()
	s.WSCfg = &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: map[string]string{"interbase-null-comparison": "error"}}}
	s.files[uri] = &File{Text: "SELECT 1"}
	s.stateMu.Unlock()

	snapshot, err := s.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal(err)
	}
	request := languageRequest(snapshot)
	if got := request.Policy.Rules["interbase-null-comparison"]; got != "error" {
		t.Fatalf("captured rule = %q, want error", got)
	}
	s.stateMu.Lock()
	s.WSCfg.Diagnostics.Rules["interbase-null-comparison"] = "off"
	s.stateMu.Unlock()
	if got := request.Policy.Rules["interbase-null-comparison"]; got != "error" {
		t.Fatalf("snapshot policy was mutated through live config: got %q", got)
	}
}

func TestDiagnosticsConfigChangeValidationAndRevision(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop() })
	const uri = "file:///policy-change.sql"
	s.stateMu.Lock()
	s.WSCfg = &config.Config{}
	s.files[uri] = &File{Text: "SELECT 1"}
	before := s.policyRevision
	s.stateMu.Unlock()

	bad := &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: map[string]string{"unknown-rule": "error"}}}
	raw, _ := json.Marshal(map[string]any{"settings": map[string]any{"sqls": bad}})
	params := json.RawMessage(raw)
	if _, err := s.handleWorkspaceDidChangeConfiguration(context.Background(), nil, &jsonrpc2.Request{Params: &params}); err == nil {
		t.Fatal("invalid configuration was accepted")
	}
	s.stateMu.RLock()
	if s.policyRevision != before || s.WSCfg == nil || len(s.WSCfg.Diagnostics.Rules) != 0 {
		s.stateMu.RUnlock()
		t.Fatal("invalid configuration partially changed live workspace policy")
	}
	s.stateMu.RUnlock()

	good := &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: map[string]string{"interbase-null-comparison": "error"}}}
	raw, _ = json.Marshal(map[string]any{"settings": map[string]any{"sqls": good}})
	params = json.RawMessage(raw)
	if _, err := s.handleWorkspaceDidChangeConfiguration(context.Background(), nil, &jsonrpc2.Request{Params: &params}); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	s.stateMu.RLock()
	after := s.policyRevision
	s.stateMu.RUnlock()
	if after != before+1 {
		t.Fatalf("policy revision = %d, want %d", after, before+1)
	}
	s.diagnosticWorkMu.Lock()
	queued := s.diagnosticAllOpen
	s.diagnosticWorkMu.Unlock()
	if !queued {
		t.Fatal("configuration change did not queue all open documents")
	}
}

func TestDiagnosticPolicyConfigPrecedence(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop() })
	const uri = "file:///policy-precedence.sql"
	s.stateMu.Lock()
	s.DefaultFileCfg = &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: map[string]string{"interbase-null-comparison": "hint"}}}
	s.WSCfg = &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: map[string]string{"interbase-null-comparison": "warning"}}}
	s.SpecificFileCfg = &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: map[string]string{"interbase-null-comparison": "error"}}}
	s.files[uri] = &File{Text: "SELECT 1"}
	s.stateMu.Unlock()

	for _, tc := range []struct {
		name      string
		clear     func()
		wantLevel string
	}{
		{name: "specific file", wantLevel: "error"},
		{name: "workspace", clear: func() { s.SpecificFileCfg = nil }, wantLevel: "warning"},
		{name: "default file", clear: func() { s.SpecificFileCfg, s.WSCfg = nil, nil }, wantLevel: "hint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.clear != nil {
				s.stateMu.Lock()
				tc.clear()
				s.stateMu.Unlock()
			}
			snapshot, err := s.captureEditorSnapshot(uri)
			if err != nil {
				t.Fatal(err)
			}
			if got := languageRequest(snapshot).Policy.Rules["interbase-null-comparison"]; got != tc.wantLevel {
				t.Fatalf("captured diagnostic level = %q, want %q", got, tc.wantLevel)
			}
		})
	}
}

func TestDiagnosticsConfigChangeRequeuesAndChangesPublishedSeverity(t *testing.T) {
	const uri = "file:///policy-severity-publication.sql"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.server.stateMu.Lock()
	tx.server.files[uri] = &File{Text: "SELECT ID FROM T WHERE V = NULL;", Version: 1}
	tx.server.stateMu.Unlock()
	entered, release := make(chan struct{}), make(chan struct{})
	var gated atomic.Bool
	tx.server.diagnosticAnalyzer = func(snapshot documentDiagnosticsSnapshot) []lsp.Diagnostic {
		if gated.Load() && snapshot.policyRevision == 0 {
			close(entered)
			<-release
		}
		return diagnosticsForSnapshot(context.Background(), snapshot)
	}
	gated.Store(true)
	done := make(chan struct{})
	go func() {
		tx.server.publishDocumentDiagnostics(tx.ctx, tx.serverConn, uri)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("old-policy analysis did not enter gate")
	}

	updated := &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: map[string]string{"interbase-null-comparison": "error"}}}
	raw, _ := json.Marshal(map[string]any{"settings": map[string]any{"sqls": updated}})
	params := json.RawMessage(raw)
	if _, err := tx.server.handleWorkspaceDidChangeConfiguration(context.Background(), nil, &jsonrpc2.Request{Params: &params}); err != nil {
		t.Fatalf("apply policy update: %v", err)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("old-policy analysis did not finish")
	}
	tx.server.diagnosticAnalyzer = nil
	tx.server.publishDocumentDiagnostics(tx.ctx, tx.serverConn, uri)
	updatedNotification := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil })
	if len(updatedNotification.Diagnostics) != 1 || updatedNotification.Diagnostics[0].Severity != 1 {
		t.Fatalf("new-policy publication = %+v, want one error-severity finding", updatedNotification.Diagnostics)
	}
	tx.client.mu.Lock()
	defer tx.client.mu.Unlock()
	for _, n := range tx.client.notifications {
		if n.URI == uri && n.Version != nil && len(n.Diagnostics) > 0 && n.Diagnostics[0].Severity == 2 {
			t.Fatalf("old warning publication survived policy revision change: %+v", n)
		}
	}
}

func TestDiagnosticsSnapshotCurrentRejectsStalePolicyRevision(t *testing.T) {
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	const uri = "file:///policy-revision.sql"
	tx.server.stateMu.Lock()
	tx.server.files[uri] = &File{Text: "SELECT 1", Version: 1}
	tx.server.stateMu.Unlock()
	snapshot, ok := tx.server.diagnosticsSnapshot(uri)
	if !ok || !tx.server.diagnosticsSnapshotCurrent(snapshot) {
		t.Fatal("fresh diagnostic snapshot should be current")
	}
	tx.server.stateMu.Lock()
	tx.server.policyRevision++
	tx.server.stateMu.Unlock()
	if tx.server.diagnosticsSnapshotCurrent(snapshot) {
		t.Fatal("snapshot captured under old policy must be stale")
	}
}

func TestDiagnosticsForSnapshotAppliesPolicy(t *testing.T) {
	text := "SELECT ID FROM T WHERE V = NULL;"
	snapshot := documentDiagnosticsSnapshot{uri: "file:///policy.sql", text: text, variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}}
	snapshot.policy = language.DiagnosticPolicy{Rules: map[string]string{"interbase-null-comparison": "error"}}
	snapshot.dialectResolved = true
	got := diagnosticsForSnapshot(context.Background(), snapshot)
	if len(got) != 1 || diagnosticCode(got[0]) != "interbase-null-comparison" || got[0].Severity != 1 {
		t.Fatalf("diagnostics = %+v, want one error-severity null-comparison finding", got)
	}
	snapshot.policy.Rules["interbase-null-comparison"] = "off"
	got = diagnosticsForSnapshot(context.Background(), snapshot)
	if len(got) != 0 {
		t.Fatalf("diagnostics with rule off = %+v, want none", got)
	}
}

func TestDiagnosticsPublishTextOnlyWithoutMetadataLease(t *testing.T) {
	const uri = "file:///policy-without-lease.sql"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	tx.server.stateMu.Lock()
	tx.server.dbConn = nil
	tx.server.metaLease = nil
	tx.server.metaSub = nil
	tx.server.metaBoundGeneration = tx.server.connGeneration
	tx.server.WSCfg = &config.Config{Connections: []*database.DBConfig{{Driver: dialect.DatabaseDriverInterBase, Dialect: 3}}}
	tx.server.files[uri] = &File{Text: "SELECT ID FROM T WHERE V = NULL;", Version: 1}
	tx.server.stateMu.Unlock()

	tx.server.publishDocumentDiagnostics(tx.ctx, tx.serverConn, uri)
	notification := tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil })
	if len(notification.Diagnostics) != 1 || diagnosticCode(notification.Diagnostics[0]) != "interbase-null-comparison" {
		t.Fatalf("metadata-free text diagnostic = %+v, want null-comparison finding", notification.Diagnostics)
	}
}

func TestDiagnosticsNeverTriggerDatabaseIO(t *testing.T) {
	const uri = "file:///diagnostics-cache-only.sql"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	var detailCalls atomic.Int32
	provider := &genericMetadataProvider{repo: diagnosticsRepository(map[string]string{})}
	lease, err := tx.server.registry.Acquire(context.Background(),
		metadata.Key{Engine: "interbase", URL: "fixture://diagnostics-cache-only"},
		metadata.Scope{Principal: "diagnostic-test"},
		func(context.Context) (metadata.Provider, error) {
			return &countedDetailProvider{Provider: provider, calls: &detailCalls}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Release)
	waitMetadataSettledForTest(t, context.Background(), lease)
	rebindTestLease(tx.server, lease, "diagnostic-test", 1)
	tx.server.stateMu.Lock()
	tx.server.files[uri] = &File{Text: "SELECT ID FROM T WHERE V = NULL;", Version: 1}
	tx.server.stateMu.Unlock()
	tx.server.publishDocumentDiagnostics(tx.ctx, tx.serverConn, uri)
	_ = tx.client.next(t, uri, func(n diagnosticsNotification) bool { return n.Version != nil })
	if got := detailCalls.Load(); got != 0 {
		t.Fatalf("diagnostic request triggered %d metadata detail calls, want cache-only", got)
	}
}

type countedDetailProvider struct {
	metadata.Provider
	calls *atomic.Int32
}

func (p *countedDetailProvider) DetailPlan(request metadata.Request) (metadata.DetailPlan, error) {
	p.calls.Add(1)
	return p.Provider.DetailPlan(request)
}

func TestDiagnosticsEmptyResultIsJSONArray(t *testing.T) {
	// The transport-level contract is covered by diagnostics lifecycle tests;
	// retain this assertion to pin the [] conversion used by the LSP host.
	got := lspDiagnostics([]language.Diagnostic{})
	raw, err := json.Marshal(got)
	if err != nil || string(raw) != "[]" {
		t.Fatalf("empty diagnostics JSON = %s, error %v; want []", raw, err)
	}
}

func TestSlowDiagnosticsCannotPublishAcrossSameURLRoleRebind(t *testing.T) {
	const uri = "file:///role-policy.sql"
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	providerA := metadatatest.New()
	providerA.Cache = &catalog.Cache{SchemaTables: map[string][]string{"": {"ROLE_A_ONLY"}}}
	providerB := metadatatest.New()
	providerB.Cache = &catalog.Cache{SchemaTables: map[string][]string{"": {"ROLE_B_ONLY"}}}
	key := metadata.Key{Engine: "interbase", URL: "fixture://same-url-role-test"}
	tx.server.openConnection = func(context.Context, *database.DBConfig) (*database.DBConnection, error) {
		return &database.DBConnection{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}, nil
	}
	tx.server.stateMu.Lock()
	tx.server.metadataFactoryOverride = func(cfg *database.DBConfig) (metadata.Key, metadata.Scope, metadata.Factory, bool) {
		role := cfg.InterBase.Role
		provider := providerA
		if role == "ROLE_B" {
			provider = providerB
		}
		return key, metadata.Scope{Principal: "same-user", Role: role}, provider.Factory, true
	}
	tx.server.stateMu.Unlock()
	tx.server.snapshots = newSourceSnapshotStore(t.TempDir())
	tx.server.stateMu.Lock()
	tx.server.files[uri] = &File{Text: "SELECT 1", Version: 1}
	tx.server.stateMu.Unlock()
	roleConfig := func(role string) *database.DBConfig {
		return &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Alias: "same-target", DataSourceName: "fixture://same-url-role-test", Dialect: 3, InterBase: &database.InterBaseConfig{Role: role}}
	}
	if err := awaitConnectionIntent(context.Background(), tx.server.coordinator.Request(context.Background(), roleConfig("ROLE_A"), 0, "")); err != nil {
		t.Fatalf("attach ROLE_A: %v", err)
	}
	waitCurrentMetadataSettledForTest(t, context.Background(), tx.server)
	a := currentLeaseForTest(tx.server)
	tx.server.stateMu.RLock()
	keyA, scopeA := tx.server.metaKey, tx.server.metaScope
	tx.server.stateMu.RUnlock()
	if keyA != key || scopeA.Role != "ROLE_A" {
		t.Fatalf("production binding for A = key %+v scope %+v", keyA, scopeA)
	}
	tx.server.stateMu.RLock()
	capturedA, err := tx.server.captureEditorSnapshotLocked(uri)
	tx.server.stateMu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	generationA := capturedA.MetadataGeneration

	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseOld := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseOld)
	var once sync.Once
	tx.server.diagnosticAnalyzer = func(snapshot documentDiagnosticsSnapshot) []lsp.Diagnostic {
		if snapshot.metadataLease == a {
			once.Do(func() { close(entered) })
			<-release
		}
		code := "ROLE_B_ONLY"
		if snapshot.metadata != nil && snapshot.metadata.Cache.SchemaTables[""][0] == "ROLE_A_ONLY" {
			code = "ROLE_A_ONLY"
		}
		return []lsp.Diagnostic{{Code: &code}}
	}
	done := make(chan struct{})
	go func() {
		tx.server.publishDocumentDiagnostics(tx.ctx, tx.serverConn, uri)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("old-role diagnostic computation did not enter its gate")
	}

	if err := awaitConnectionIntent(context.Background(), tx.server.coordinator.Request(context.Background(), roleConfig("ROLE_B"), 0, "")); err != nil {
		t.Fatalf("attach ROLE_B: %v", err)
	}
	waitCurrentMetadataSettledForTest(t, context.Background(), tx.server)
	b := currentLeaseForTest(tx.server)
	tx.server.stateMu.RLock()
	keyB, scopeB := tx.server.metaKey, tx.server.metaScope
	tx.server.stateMu.RUnlock()
	if keyB != key || scopeB.Role != "ROLE_B" {
		t.Fatalf("production binding for B = key %+v scope %+v", keyB, scopeB)
	}
	if generationA != b.Snapshot().Generation {
		t.Fatalf("test precondition: metadata generations differ: %d vs %d", generationA, b.Snapshot().Generation)
	}
	oldRequest := languageRequest(capturedA)
	oldStamp := language.Stamp{
		URI: oldRequest.Document.URI, Version: oldRequest.Document.Version, Binding: oldRequest.Document.Binding,
		Incarnation: oldRequest.Context.Incarnation, Generation: oldRequest.Context.Snapshot.Generation,
		Revision: oldRequest.Context.Snapshot.Revision, Key: oldRequest.Context.Key,
	}
	if _, published, err := tx.server.publishDefinitionCandidate(tx.ctx, capturedA, oldStamp, "table", "ROLE_A_ONLY", "CREATE TABLE ROLE_A_ONLY (ID INTEGER)"); err != nil {
		t.Fatalf("publish stale role source: %v", err)
	} else if published {
		t.Fatal("stale role A source was published after rebinding to role B")
	}
	var staleFiles []string
	if err := filepath.WalkDir(tx.server.snapshots.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			staleFiles = append(staleFiles, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(staleFiles) != 0 {
		t.Fatalf("stale source candidate remained after role rebind: %v", staleFiles)
	}
	releaseOld()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("old-role diagnostic did not finish")
	}

	tx.serverConn.Notify(tx.ctx, "test/notificationBarrier", struct{}{})
	select {
	case <-tx.client.barrier:
	case <-time.After(time.Second):
		t.Fatal("notification barrier timed out")
	}
	tx.client.mu.Lock()
	for _, notification := range tx.client.notifications {
		if notification.URI == uri && notification.Version != nil {
			tx.client.mu.Unlock()
			t.Fatalf("stale diagnostics published across role rebind: %+v", notification)
		}
	}
	tx.client.mu.Unlock()

	current, err := tx.server.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal(err)
	}
	request := languageRequest(current)
	if request.Context.Details != b || request.Context.Key != b.Key() ||
		request.Context.Incarnation != b.Incarnation() || request.Context.Snapshot != b.Snapshot() {
		t.Fatal("language request mixed metadata provenance across the role-bound lease")
	}
	if got := current.Metadata.Cache.SchemaTables[""][0]; got != "ROLE_B_ONLY" {
		t.Fatalf("captured role catalog marker = %q, want ROLE_B_ONLY", got)
	}
}

func rebindTestLease(s *Server, lease *metadata.Lease, role string, generation int) {
	s.stateMu.Lock()
	old, oldSub := s.metaLease, s.metaSub
	s.metaLease = lease
	s.metaSub = lease.Subscribe()
	s.metaKey, s.metaScope = lease.Key(), metadata.Scope{Principal: "same-user", Role: role}
	s.metaBoundGeneration = generation
	s.connGeneration = generation
	s.curDBCfg = &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, DataSourceName: "fixture://same-url-role-test", User: role}
	s.connectionState = connectionReady
	s.stateMu.Unlock()
	if oldSub != nil {
		oldSub.Close()
	}
	if old != nil && old != lease {
		old.Release()
	}
}
