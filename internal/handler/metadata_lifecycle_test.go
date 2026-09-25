package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sourcegraph/jsonrpc2"
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

	const uri = "file:///explicit-reselect.sql"
	s.stateMu.Lock()
	s.files[uri] = &File{Text: "select 1"}
	s.stateMu.Unlock()

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
	if snapshot, err := s.captureEditorSnapshot(uri); err != nil || snapshot.Metadata == nil || snapshot.MetadataIncarnation != oldStore {
		t.Fatalf("metadata not visible to the editor after initial acquire: snapshot=%+v err=%v", snapshot, err)
	}
	if status := s.metadataStatus(); !status.Settled || status.Degraded {
		t.Fatalf("status not settled/clean after initial acquire: %+v", status)
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
	// Metadata must still be visible to the editor after the refresh — not
	// just internally acquired: a stale/blank metaBoundGeneration after a
	// successful refresh would make captureEditorSnapshot/metadataStatus
	// look like metadata was never bound to this connection.
	if snapshot, err := s.captureEditorSnapshot(uri); err != nil || snapshot.Metadata == nil || snapshot.MetadataIncarnation != newStore || snapshot.MetadataGeneration != secondLease.Snapshot().Generation {
		t.Fatalf("metadata not visible to the editor after explicit refresh: snapshot=%+v err=%v", snapshot, err)
	}
	if status := s.metadataStatus(); !status.Settled || status.Degraded {
		t.Fatalf("status not settled/clean after explicit refresh: %+v", status)
	}
}

// refreshFailureProvider succeeds its first Plan() call (the initial
// Acquire) and returns an invalid plan (Parallelism 0) on every call after
// that, so an explicit reselect's Lease.Refresh — which re-runs Plan() on
// the same Provider instance — synchronously fails validateMetadataPlan and
// Refresh itself returns a non-nil error, the only way to drive
// bindMetadata's refreshErr != nil branch (a job merely failing at runtime
// does not make Refresh return an error; see Lease.Refresh's doc comment).
type refreshFailureProvider struct {
	calls atomic.Int64
}

func (p *refreshFailureProvider) Plan() catalog.MetadataPlan {
	if p.calls.Add(1) == 1 {
		return catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{
			{Kind: database.MetadataSchemas, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
				return catalog.MetadataPatch{Cache: &catalog.Cache{Schemas: map[string]string{}}}, nil
			}},
		}}
	}
	return catalog.MetadataPlan{}
}

func (p *refreshFailureProvider) DetailPlan(metadata.Request) (metadata.DetailPlan, error) {
	return metadata.DetailPlan{}, metadata.ErrUnsupported
}

func (p *refreshFailureProvider) Close() error { return nil }

// TestExplicitRefreshFailureBindsGenerationAndReportsDegraded is the
// explicit-Refresh-failure regression test (Ruling R10): a failed explicit
// reselect must still bind metaBoundGeneration to the new connection
// generation (never leave it stale/blank, which would look identical to
// "metadata never attempted for this connection") and must surface as
// Degraded/metadata_load_failed through metadataStatus, not as the
// "not caught up yet" shape connGeneration != metaBoundGeneration produces.
func TestExplicitRefreshFailureBindsGenerationAndReportsDegraded(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	provider := &refreshFailureProvider{}
	key := metadata.Key{Engine: "test", URL: "explicit-refresh-failure"}
	scope := metadata.Scope{Principal: "test"}
	s.stateMu.Lock()
	s.metadataFactoryOverride = func(*database.DBConfig) (metadata.Key, metadata.Scope, metadata.Factory, bool) {
		return key, scope, func(context.Context) (metadata.Provider, error) { return provider, nil }, true
	}
	s.stateMu.Unlock()
	s.openConnection = func(context.Context, *database.DBConfig) (*database.DBConnection, error) {
		return &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}, nil
	}
	cfg := &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Alias: "refresh-failure"}

	if err := awaitConnectionIntent(context.Background(), s.coordinator.RequestExplicit(context.Background(), cfg, 0, "")); err != nil {
		t.Fatal(err)
	}
	waitCurrentMetadataSettledForTest(t, context.Background(), s)
	firstLease := currentLeaseForTest(s)
	if status := s.metadataStatus(); status.Degraded {
		t.Fatalf("initial acquire unexpectedly degraded: %+v", status)
	}

	// An explicit reselect of the identical target: bindMetadata takes the
	// Refresh branch, and provider.Plan()'s second call (Refresh re-running
	// Plan() on the same Provider) returns an invalid plan.
	if err := awaitConnectionIntent(context.Background(), s.coordinator.RequestExplicit(context.Background(), cfg, 0, "")); err != nil {
		t.Fatal(err)
	}

	s.stateMu.RLock()
	bound, connGeneration, lease := s.metaBoundGeneration, s.connGeneration, s.metaLease
	s.stateMu.RUnlock()
	if lease != firstLease {
		t.Fatal("explicit refresh acquired a different lease instead of refreshing the existing one")
	}
	if bound != connGeneration {
		t.Fatalf("metaBoundGeneration = %d after a failed explicit refresh, want it bound to the new connection generation %d — a stale/blank binding would look identical to metadata never having been attempted for this connection", bound, connGeneration)
	}

	status := s.metadataStatus()
	if !status.Settled || !status.Degraded || status.MetadataErrorCode != "metadata_load_failed" {
		t.Fatalf("status after failed explicit refresh = %+v, want Settled/Degraded with metadata_load_failed", status)
	}
	if status.ConnectionGeneration != uint64(connGeneration) {
		t.Fatalf("status generation = %d, want the new bound connection generation %d", status.ConnectionGeneration, connGeneration)
	}
}

// genericLiveConnClosedFlag stands in for a closed connection without a real
// *sql.DB: DBConnection.Close() closes its Tunnel field (an io.Closer) same
// as any other resource it owns, so setting Tunnel to one of these lets a
// test detect "this exact *database.DBConnection was closed" without a
// database driver.
type genericLiveConnClosedFlag struct{ closed atomic.Bool }

func (f *genericLiveConnClosedFlag) Close() error { f.closed.Store(true); return nil }

const genericLiveConnTestDriver = dialect.DatabaseDriver("sqls-test-generic-live-conn")

var genericLiveConnFactoryOnce sync.Once

// registerGenericLiveConnFactory registers a ConnFactory for a fake non-
// InterBase driver once per test binary (database.RegisterConnFactory panics
// on a second registration): every CreateRepositoryFromConnection call for
// genericLiveConnTestDriver returns a repository whose only job fails if the
// *database.DBConnection it was built from is closed. genericMetadataProvider
// calls CreateRepositoryFromConnection with whatever liveConn() returns at
// Plan() time, so this repo, run through Refresh, proves which connection
// object Plan() actually used.
func registerGenericLiveConnFactory() {
	genericLiveConnFactoryOnce.Do(func() {
		database.RegisterConnFactory(genericLiveConnTestDriver, func(conn *database.DBConnection) database.DBRepository {
			return genericLiveConnRepo{DBRepository: &database.MockDBRepository{}, conn: conn}
		})
	})
}

type genericLiveConnRepo struct {
	database.DBRepository
	conn *database.DBConnection
}

func (r genericLiveConnRepo) MetadataPlan() database.MetadataPlan {
	conn := r.conn
	return database.MetadataPlan{Parallelism: 1, Jobs: []database.MetadataJob{
		{Kind: database.MetadataSchemas, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			if flag, ok := conn.Tunnel.(*genericLiveConnClosedFlag); ok && flag.closed.Load() {
				return database.MetadataPatch{}, fmt.Errorf("generic metadata plan ran its catalog query against a closed connection")
			}
			return database.MetadataPatch{Cache: &database.DBCache{DefaultSchema: "ok", Schemas: map[string]string{"OK": "ok"}}, Count: 1}, nil
		}},
	}}
}

// TestGenericExplicitReselectUsesLiveConnectionNotClosedOne is the Critical-
// fix regression test: genericMetadataProvider must resolve its connection
// dynamically at Plan() time (via liveConn), never capture one statically
// when its Factory runs, because an explicit same-target reselect calls
// Lease.Refresh on the SAME Provider instance after attachIntent has already
// swapped s.dbConn to a new connection and enqueued the old one to be
// closed. Unlike TestExplicitReselectRefreshesSharedMetadata (which installs
// a metadataFactoryOverride and so never exercises genericMetadataTarget at
// all), this test drives the real, un-overridden metadataTargetFor dispatch
// for a non-InterBase driver.
func TestGenericExplicitReselectUsesLiveConnectionNotClosedOne(t *testing.T) {
	registerGenericLiveConnFactory()
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })

	var mu sync.Mutex
	var connections []*database.DBConnection
	s.openConnection = func(context.Context, *database.DBConfig) (*database.DBConnection, error) {
		conn := &database.DBConnection{Driver: genericLiveConnTestDriver, Tunnel: &genericLiveConnClosedFlag{}}
		mu.Lock()
		connections = append(connections, conn)
		mu.Unlock()
		return conn, nil
	}
	cfg := &database.DBConfig{Driver: genericLiveConnTestDriver, Alias: "generic-explicit", Host: "live-conn-host", Port: 1234, DBName: "livedb"}

	if err := awaitConnectionIntent(context.Background(), s.coordinator.RequestExplicit(context.Background(), cfg, 0, "")); err != nil {
		t.Fatal(err)
	}
	waitCurrentMetadataSettledForTest(t, context.Background(), s)
	if status := s.metadataStatus(); status.Degraded {
		t.Fatalf("initial acquire unexpectedly degraded: %+v", status)
	}

	mu.Lock()
	if len(connections) != 1 {
		mu.Unlock()
		t.Fatalf("expected 1 connection opened by the initial acquire, got %d", len(connections))
	}
	firstConn := connections[0]
	mu.Unlock()

	// Poison the first connection the way production's real
	// enqueueDetachedConnection/cleanupConnections close would (see
	// metadataTargetFor's doc comment): closing it directly here, rather
	// than waiting on the async cleanup worker, makes the assertion below
	// deterministic instead of racing that worker's scheduling.
	if err := firstConn.Close(); err != nil {
		t.Fatal(err)
	}

	// An EXPLICIT reselect of the identical target: same Key/Scope (same
	// cfg), so bindMetadata takes the Refresh branch and re-runs Plan() on
	// the SAME genericMetadataProvider instance the first acquire built —
	// exactly the window a static connection capture would be unsafe in.
	if err := awaitConnectionIntent(context.Background(), s.coordinator.RequestExplicit(context.Background(), cfg, 0, "")); err != nil {
		t.Fatal(err)
	}
	waitCurrentMetadataSettledForTest(t, context.Background(), s)

	mu.Lock()
	if len(connections) != 2 {
		mu.Unlock()
		t.Fatalf("expected 2 connections opened after the explicit reselect, got %d", len(connections))
	}
	secondConn := connections[1]
	mu.Unlock()
	if secondConn == firstConn {
		t.Fatal("test setup broken: explicit reselect reused the same connection object")
	}

	status := s.metadataStatus()
	if status.Degraded {
		t.Fatalf("explicit reselect's refresh ran its catalog query against the closed first connection instead of the live second one: %+v", status)
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
	newLeaseCache := currentLeaseCacheForTest(tx.server)

	if oldBinding == newBinding {
		t.Fatal("switch did not invalidate editor binding")
	}
	if tx.server.diagnosticsSnapshotCurrent(oldSnapshot) {
		t.Fatal("old binding's diagnostics snapshot is still current after the switch; a late-arriving old-generation result could publish against the new connection's state")
	}

	// The check above is already satisfied by the pre-existing
	// connGeneration == snapshot.generation comparison alone (oldBinding !=
	// the now-current connGeneration), so it does not by itself prove the
	// metaBoundGeneration fence is load-bearing. Falsify that directly: model
	// the real production window (handler.go's attachIntent bumps
	// connGeneration BEFORE bindMetadata's Acquire/Refresh ever runs) where
	// connGeneration has already advanced to a new value but the SAME lease
	// from the previous generation is still bound, unchanged, because this
	// generation's bindMetadata call has not completed. Construct a snapshot
	// for that exact window: its generation matches the (already advanced)
	// current connGeneration, and its cache pointer matches the
	// still-bound, unchanged lease's cache — so both the pre-existing
	// connGeneration check AND the cache-pointer check below it would pass.
	// Only the metaBoundGeneration fence can reject it.
	tx.server.diagnosticsPublishMu.Lock()
	tx.server.stateMu.Lock()
	tx.server.connGeneration++
	inFlightGeneration := tx.server.connGeneration
	tx.server.stateMu.Unlock()
	tx.server.diagnosticsPublishMu.Unlock()
	// bindMetadata for inFlightGeneration is deliberately never called: the
	// server's metaLease/metaBoundGeneration stay bound to newGeneration,
	// exactly like the window between attachIntent's connGeneration++ and
	// bindMetadata's Acquire/Refresh completing.

	if got := currentLeaseCacheForTest(tx.server); got != newLeaseCache {
		t.Fatal("test setup broken: expected the newGeneration lease's cache to still be bound and unchanged")
	}
	inFlightSnapshot := newSnapshot
	inFlightSnapshot.generation = inFlightGeneration
	inFlightSnapshot.cache = newLeaseCache
	if tx.server.connGeneration != inFlightSnapshot.generation {
		t.Fatal("test setup broken: connGeneration must equal the in-flight snapshot's generation")
	}
	if tx.server.diagnosticsSnapshotCurrent(inFlightSnapshot) {
		t.Fatal("a snapshot generation whose bindMetadata has not completed was treated as current even though connGeneration and the cache pointer both matched; metaBoundGeneration guard is not load-bearing")
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

	// explainQuery legitimately fails against a repository that does not
	// implement ExplainRepository (the stub backend does not) — the exact
	// "explain is not supported by the ... driver" error explainRepositoryFor
	// returns for that case, asserted below so this test does not silently
	// swallow some OTHER failure (for example, one caused by touching a
	// metadata read transaction) that would defeat its own premise.
	explainErr := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExplainQuery,
		Arguments: []interface{}{testFileURI},
	}, &got)
	if explainErr == nil || !strings.Contains(explainErr.Error(), "explain is not supported by the") {
		t.Fatalf("explainQuery error = %v, want the ExplainRepository-unsupported error", explainErr)
	}

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
					Catalog: &database.CatalogCache{Procedures: map[string]*database.ProcedureDesc{"MYPROC": {Name: "MYPROC", InputParameters: []*database.ProcedureParameterDesc{{Name: "P", Position: 0, Direction: database.ParameterInput, Type: "INTEGER"}}}}},
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

// TestHoverAndDefinitionShareObjectLoad proves the production hover and
// Definition handlers reuse one Detail load through the same metadata lease.
func TestHoverAndDefinitionShareObjectLoad(t *testing.T) {
	server := newDefinitionServer(t)
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
	leaseCache := lease.Snapshot().Cache
	leaseCache.Metadata[database.MetadataViews] = database.MetadataReady
	leaseCache.Metadata[database.MetadataTriggers] = database.MetadataReady
	leaseCache.Catalog.Views = map[string]*database.ViewDesc{}
	leaseCache.Catalog.Triggers = map[string]*database.TriggerDesc{}
	leaseCache.Catalog.ProceduresByID = map[string]*catalog.ProcedureDesc{
		(catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "MYPROC"}).Key(): leaseCache.Catalog.Procedures["MYPROC"],
	}
	leaseCache.Catalog.ViewsByID = map[string]*catalog.ViewDesc{}
	leaseCache.Catalog.TriggersByID = map[string]*catalog.TriggerDesc{}
	leaseCache.Catalog.RebuildObjects(strings.ToUpper)

	// Production Definition and Hover must obtain Detail through the same lease.

	text := "execute procedure myproc"
	hoverParams := lsp.HoverParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
		Position:     lsp.Position{Line: 0, Character: 20},
	}}
	server.files[testFileURI] = &File{Text: text, Version: 1, Revision: 1}
	server.connGeneration, server.metaBoundGeneration, server.metaLease = 1, 1, lease
	paramsJSON, err := json.Marshal(hoverParams)
	if err != nil {
		t.Fatal(err)
	}
	rawParams := json.RawMessage(paramsJSON)
	value, err := server.handleTextDocumentHover(context.Background(), nil, &jsonrpc2.Request{Params: &rawParams})
	if err != nil {
		t.Fatal("production hover handler:", err)
	}
	hover, ok := value.(*lsp.Hover)
	if !ok || hover == nil {
		t.Fatalf("hover result = %#v, want *lsp.Hover", value)
	}
	if !strings.Contains(hover.Contents.Value, wantDDL) {
		t.Fatalf("hover contents = %q, want it to contain %q", hover.Contents.Value, wantDDL)
	}
	value, err = server.handleTextDocumentHover(context.Background(), nil, &jsonrpc2.Request{Params: &rawParams})
	if err != nil {
		t.Fatal("repeated production hover:", err)
	}
	repeated, ok := value.(*lsp.Hover)
	if !ok || repeated == nil || !strings.Contains(repeated.Contents.Value, wantDDL) {
		t.Fatalf("repeated hover = %#v, want the cached DDL appendix", value)
	}

	got, err := invokeDefinition(server, text, 20)
	if err != nil {
		t.Fatal("production Definition handler:", err)
	}
	if len(got) != 1 {
		t.Fatalf("definition locations = %d, want 1", len(got))
	}

	if calls := provider.loadCalls.Load(); calls != 1 {
		t.Fatalf("Detail Load ran %d times for hover+definition on the same object, want 1 (shared load)", calls)
	}
}
