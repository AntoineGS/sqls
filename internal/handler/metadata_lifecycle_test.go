package handler

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

// fixedMetadataTarget installs a metadataFactoryOverride that always returns
// key/scope/provider.Factory regardless of the connection config passed to
// it — modelling "two different connections that happen to point at the
// same actual database" (the central proof this task is required to make),
// without depending on the generic driver's own Key/Scope derivation.
func fixedMetadataTarget(s *Server, key metadata.Key, scope metadata.Scope, provider *metadatatest.Provider) {
	s.stateMu.Lock()
	s.metadataFactoryOverride = func(*database.DBConfig) (metadata.Key, metadata.Scope, metadata.Factory, bool) {
		return key, scope, provider.Factory, true
	}
	s.stateMu.Unlock()
}

func TestSameTargetConsumerDoesNotRestartMetadata(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop() })
	provider := metadatatest.New()
	key := metadata.Key{Engine: "test", URL: "same-target"}
	scope := metadata.Scope{Principal: "test"}
	fixedMetadataTarget(s, key, scope, provider)
	s.openConnection = func(context.Context, *database.DBConfig) (*database.DBConnection, error) {
		return &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}, nil
	}

	first := &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Alias: "alias-one"}
	if err := awaitConnectionIntent(context.Background(), s.coordinator.Request(context.Background(), first, 0, "")); err != nil {
		t.Fatal(err)
	}
	waitCurrentMetadataSettledForTest(t, context.Background(), s)
	oldStore := currentLeaseForTest(s).Incarnation()
	if provider.Calls.Load() != 1 {
		t.Fatalf("initial acquire ran the catalog fetch %d times, want 1", provider.Calls.Load())
	}

	// A different config (a different alias/connection index) that this
	// increment's override resolves to the SAME shared-metadata Key/Scope —
	// the same-URL, different-connection scenario the whole plan is about.
	// The coordinator's own intentKey dedup does not suppress this request
	// (the configs differ), so attachIntent genuinely runs bindMetadata's
	// Acquire branch again for the identical (Key, Scope).
	second := &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Alias: "alias-two"}
	if err := awaitConnectionIntent(context.Background(), s.coordinator.Request(context.Background(), second, 0, "")); err != nil {
		t.Fatal(err)
	}
	waitCurrentMetadataSettledForTest(t, context.Background(), s)
	newStore := currentLeaseForTest(s).Incarnation()

	if provider.Calls.Load() != 1 {
		t.Fatal("connection binding restarted catalog")
	}
	if oldStore != newStore {
		t.Fatal("same URL acquired a different live store")
	}
}

func TestExplicitReselectRefreshesSharedMetadata(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop() })
	provider := metadatatest.New()
	key := metadata.Key{Engine: "test", URL: "explicit-refresh-target"}
	scope := metadata.Scope{Principal: "test"}
	fixedMetadataTarget(s, key, scope, provider)
	s.openConnection = func(context.Context, *database.DBConfig) (*database.DBConnection, error) {
		return &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}, nil
	}
	cfg := &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Alias: "explicit"}

	if err := awaitConnectionIntent(context.Background(), s.coordinator.RequestExplicit(context.Background(), cfg, 0, "")); err != nil {
		t.Fatal(err)
	}
	waitCurrentMetadataSettledForTest(t, context.Background(), s)
	firstLease := currentLeaseForTest(s)
	oldStore := firstLease.Incarnation()
	firstGeneration := firstLease.Snapshot().Generation
	if provider.Calls.Load() != 1 {
		t.Fatalf("initial acquire ran the catalog fetch %d times, want 1", provider.Calls.Load())
	}

	// An EXPLICIT reselect of the identical target must call Lease.Refresh,
	// not Acquire a fresh lease: same incarnation, advanced generation.
	if err := awaitConnectionIntent(context.Background(), s.coordinator.RequestExplicit(context.Background(), cfg, 0, "")); err != nil {
		t.Fatal(err)
	}
	waitCurrentMetadataSettledForTest(t, context.Background(), s)
	secondLease := currentLeaseForTest(s)
	newStore := secondLease.Incarnation()

	if oldStore != newStore {
		t.Fatal("explicit reselect of the same target acquired a different live store, want a refresh of the same one")
	}
	if secondLease.Snapshot().Generation <= firstGeneration {
		t.Fatalf("explicit reselect did not advance the metadata generation: first=%d second=%d", firstGeneration, secondLease.Snapshot().Generation)
	}
	if provider.Calls.Load() != 2 {
		t.Fatalf("provider calls after explicit refresh = %d, want 2 (Refresh re-runs the plan once)", provider.Calls.Load())
	}
}

func TestOldBindingCannotPublishNewStoreDiagnostics(t *testing.T) {
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	const uri = "file:///old-binding.sql"
	tx.open(t, uri, "select * from city", 1)

	oldSnapshot, ok := tx.server.diagnosticsSnapshot(uri)
	if !ok {
		t.Fatal("could not capture old diagnostics snapshot")
	}
	oldBinding := oldSnapshot.generation

	// Switch connections: a genuinely new generation and a new metadata
	// binding, exactly as bindMetadata's Acquire branch does for a
	// different target.
	tx.server.diagnosticsPublishMu.Lock()
	tx.server.stateMu.Lock()
	tx.server.connGeneration++
	newGeneration := tx.server.connGeneration
	tx.server.stateMu.Unlock()
	tx.server.diagnosticsPublishMu.Unlock()
	bindMetadataRepoForTest(t, tx.server, newGeneration, readinessPlanRepository{DBRepository: &database.MockDBRepository{}, plan: database.MetadataPlan{Parallelism: 1}})

	newSnapshot, ok := tx.server.diagnosticsSnapshot(uri)
	if !ok {
		t.Fatal("could not capture new diagnostics snapshot")
	}
	newBinding := newSnapshot.generation

	if oldBinding == newBinding {
		t.Fatal("switch did not invalidate editor binding")
	}
	if tx.server.diagnosticsSnapshotCurrent(oldSnapshot) {
		t.Fatal("old binding's diagnostics snapshot is still current after the switch; a late-arriving old-generation result could publish against the new connection's state")
	}
}

func TestExecuteExplainDoNotUseMetadataReadTransaction(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	t.Cleanup(tx.tearDown)
	t.Cleanup(func() { _ = tx.server.Stop() })

	backend := installStubBackend(t)
	backend.setProcedures(nil)
	provider := metadatatest.New()
	key := metadata.Key{Engine: "test", URL: "execute-explain-target"}
	scope := metadata.Scope{Principal: "test"}
	fixedMetadataTarget(tx.server, key, scope, provider)

	tx.addWorkspaceConfig(t, stubConnections("primary"))
	waitCurrentMetadataSettledForTest(t, context.Background(), tx.server)

	callsBefore := provider.Calls.Load()
	detailCallsBefore := provider.DetailCalls.Load()

	tx.textDocumentDidOpen(t, testFileURI, "select 1")
	var got string
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExecuteQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call executeQuery:", err)
	}

	// explainQuery may legitimately fail against a repository that does not
	// implement ExplainRepository (the stub backend does not); that failure
	// is irrelevant to this test, which only asserts that whatever ran never
	// touched the metadata provider's counters.
	_ = tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExplainQuery,
		Arguments: []interface{}{testFileURI},
	}, &got)

	if provider.Calls.Load() != callsBefore {
		t.Fatalf("execute/explain touched the metadata provider's Calls counter: before=%d after=%d", callsBefore, provider.Calls.Load())
	}
	if provider.DetailCalls.Load() != detailCallsBefore {
		t.Fatalf("execute/explain touched the metadata provider's DetailCalls counter: before=%d after=%d", detailCallsBefore, provider.DetailCalls.Load())
	}
}

// procedureDDLDetailProvider is a minimal metadata.Provider fixture whose only
// DetailPlan covers one InterBase procedure's "ddl" facet, using the exact
// Coverage contract (object key + facet) real InterBase detail plans use (see
// internal/database/interbase_metadata_detail.go's detailCoverage), so a
// hover's and a definition's Load calls for the SAME object/facet join the
// same detail-store entry instead of each starting their own.
type procedureDDLDetailProvider struct {
	ddl       string
	loadCalls atomic.Int64
}

func (p *procedureDDLDetailProvider) Plan() catalog.MetadataPlan {
	return catalog.MetadataPlan{
		Parallelism: 1,
		Jobs: []catalog.MetadataJob{
			{Kind: database.MetadataProcedures, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
				return catalog.MetadataPatch{Cache: &catalog.Cache{
					Catalog: &database.CatalogCache{Procedures: map[string]*database.ProcedureDesc{}},
				}}, nil
			}},
		},
	}
}

func (p *procedureDDLDetailProvider) DetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	if req.Object.Kind != database.ObjectKindProcedure || req.Facet != "ddl" {
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
	return metadata.DetailPlan{
		Requires: []catalog.MetadataKind{database.MetadataProcedures},
		Coverage: req.Object.Key() + "\x00" + req.Facet,
		Load: func(context.Context, *catalog.Cache) (metadata.Detail, error) {
			p.loadCalls.Add(1)
			return metadata.Detail{Object: req.Object, Text: p.ddl}, nil
		},
	}, nil
}

func (p *procedureDDLDetailProvider) Close() error { return nil }

// TestHoverAndDefinitionShareObjectLoad proves hover and go-to-definition
// against the same InterBase object share exactly one Detail load through the
// shared metadata lease, via leaseDDLRepository (see
// interbase_metadata_lease_repository.go) — the production wiring hover.go
// and definition.go now use, not a bespoke test-only path.
func TestHoverAndDefinitionShareObjectLoad(t *testing.T) {
	server := newDefinitionServer(t)
	cache := definitionCatalog()
	const wantDDL = "CREATE PROCEDURE \"MYPROC\" AS\nBEGIN\n  SUSPEND;\nEND"
	provider := &procedureDDLDetailProvider{ddl: wantDDL}

	lease, err := server.registry.Acquire(context.Background(), metadata.Key{Engine: "test", URL: "hover-definition-share"}, metadata.Scope{Principal: "test"}, func(context.Context) (metadata.Provider, error) {
		return provider, nil
	})
	if err != nil {
		t.Fatal("acquire test metadata lease:", err)
	}
	t.Cleanup(lease.Release)
	if err := metadatatest.WaitReady(context.Background(), lease); err != nil {
		t.Fatal("wait for metadata readiness:", err)
	}

	// The raw repository must never be asked: every call in this test is for
	// an object kind the lease's "ddl" facet covers, so leaseDDLRepository
	// must route both through Detail.
	rawRepo := newStubDDLRepository(func(context.Context, database.ObjectKind, string) (string, error) {
		t.Fatal("ObjectDDL called on the raw repository; want the shared lease Detail load instead")
		return "", nil
	})
	repo := wrapRepositoryWithLease(rawRepo, lease, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})

	text := "execute procedure myproc"
	hoverParams := lsp.HoverParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
		Position:     lsp.Position{Line: 0, Character: 20},
	}}
	base, err := hoverWithDriver(text, hoverParams, cache, dialect.DatabaseDriverInterBase)
	if err != nil && !errors.Is(err, ErrNoHover) {
		t.Fatal("hoverWithDriver:", err)
	}
	hover := server.interBaseHover(context.Background(), repo, cache, hoverParams, text, base, server.connectionGeneration(), server.parserDriverVariant())
	if hover == nil {
		t.Fatal("interBaseHover returned nil, want a hover with the DDL appendix")
	}
	if !strings.Contains(hover.Contents.Value, wantDDL) {
		t.Fatalf("hover contents = %q, want it to contain %q", hover.Contents.Value, wantDDL)
	}

	got, err := server.interBaseDefinition(context.Background(), repo, cache, definitionParamsAt(20), text)
	if err != nil {
		t.Fatal("interBaseDefinition:", err)
	}
	if len(got) != 1 {
		t.Fatalf("definition locations = %d, want 1", len(got))
	}

	if calls := provider.loadCalls.Load(); calls != 1 {
		t.Fatalf("Detail Load ran %d times for hover+definition on the same object, want 1 (shared load)", calls)
	}
}
