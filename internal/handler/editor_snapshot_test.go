package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
)

type partialCatalogRepository struct{ *database.MockDBRepository }

func (partialCatalogRepository) MetadataPlan() database.MetadataPlan {
	return database.MetadataPlan{Parallelism: 2, Jobs: []database.MetadataJob{
		{Kind: database.MetadataViews, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{}, errors.New("views unavailable")
		}},
		{Kind: database.MetadataProcedures, Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			return database.MetadataPatch{Cache: &database.DBCache{Catalog: &database.CatalogCache{Procedures: map[string]*database.ProcedureDesc{
				"READYPROC": {Name: "READYPROC", InputParameters: []*database.ProcedureParameterDesc{{Name: "P_IN", Type: "INTEGER"}}},
			}}}}, nil
		}},
	}}
}

type gatedProcedureRepository struct {
	*database.MockDBRepository
	entered chan struct{}
	release chan struct{}
}

func (r gatedProcedureRepository) MetadataPlan() database.MetadataPlan {
	return database.MetadataPlan{Parallelism: 1, Jobs: []database.MetadataJob{{
		Kind: database.MetadataProcedures,
		Run: func(context.Context, *database.DBCache) (database.MetadataPatch, error) {
			close(r.entered)
			<-r.release
			return database.MetadataPatch{Cache: &database.DBCache{Catalog: &database.CatalogCache{Procedures: map[string]*database.ProcedureDesc{
				"ONLY_A": {Name: "ONLY_A"},
			}}}}, nil
		},
	}}}
}

func TestEditorSnapshotCopiesDocumentAndConfiguredDialect(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	s.WSCfg = &config.Config{Connections: []*database.DBConfig{{Driver: dialect.DatabaseDriverInterBase, Dialect: 1}}}
	if err := s.openFileAtVersion("file:///query.sql", "sql", "SELECT 1", 7); err != nil {
		t.Fatal(err)
	}

	snapshot, err := s.captureEditorSnapshot("file:///query.sql")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.URI != "file:///query.sql" || snapshot.Text != "SELECT 1" || snapshot.Version != 7 || snapshot.Revision == 0 {
		t.Fatalf("document snapshot = %#v", snapshot)
	}
	if snapshot.Variant != (dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase1}) {
		t.Fatalf("variant = %#v", snapshot.Variant)
	}
	if !snapshot.DialectResolved {
		t.Fatal("explicitly configured dialect should be resolved before attachment")
	}
	if snapshot.Repository != nil {
		t.Fatal("repository exists without an attached connection")
	}
}

func TestDiagnosticsSnapshotCurrentAcceptsConfiguredVariantWithoutConnection(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	s.WSCfg = &config.Config{Connections: []*database.DBConfig{{Driver: dialect.DatabaseDriverInterBase, Dialect: 1}}}
	s.stateMu.Lock()
	s.connGeneration = 1
	s.stateMu.Unlock()
	bindMetadataRepoForTest(t, s, 1, readinessPlanRepository{DBRepository: &database.MockDBRepository{}, plan: database.MetadataPlan{Parallelism: 1}})
	if err := s.openFileAtVersion("file:///configured.sql", "sql", "SELECT 1", 1); err != nil {
		t.Fatal(err)
	}
	snapshot, ok := s.diagnosticsSnapshot("file:///configured.sql")
	if !ok {
		t.Fatal("configured document snapshot missing")
	}
	if !s.diagnosticsSnapshotCurrent(snapshot) {
		t.Fatalf("snapshot with configured variant %#v is not current before attachment", snapshot.variant)
	}
}

func TestEditorSnapshotRejectsMismatchedMetadataGeneration(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	if err := s.openFileAtVersion("file:///query.sql", "sql", "SELECT 1", 1); err != nil {
		t.Fatal(err)
	}
	s.stateMu.Lock()
	s.connGeneration = 2
	s.stateMu.Unlock()
	// Simulate a loader/server transition observed mid-publication: a
	// metadata lease bound to generation 3 while the connection has already
	// moved on to generation 2 (metaBoundGeneration deliberately mismatched
	// against connGeneration, the equivalent of the old fused-generation
	// race this test predates).
	bindMetadataRepoForTest(t, s, 3, readinessPlanRepository{DBRepository: &database.MockDBRepository{}, plan: database.MetadataPlan{Parallelism: 1}})

	snapshot, err := s.captureEditorSnapshot("file:///query.sql")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Metadata != nil && snapshot.Metadata.Generation != uint64(snapshot.Generation) && snapshot.Cache.HasCatalog() {
		t.Fatal("mismatched metadata cache was exposed")
	}
	if snapshot.Cache == nil {
		t.Fatal("snapshot must always provide a nil-safe cache")
	}
	if snapshot.Cache.HasCatalog() || snapshot.Cache.ColumnsReady() {
		t.Fatal("mismatched generation must use an empty cache")
	}
}

func TestEditorSnapshotAutoDialectUsesDefaultUntilResolved(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	s.WSCfg = &config.Config{Connections: []*database.DBConfig{{Driver: dialect.DatabaseDriverInterBase}}}
	if err := s.openFileAtVersion("file:///query.sql", "sql", "SELECT 1", 1); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.captureEditorSnapshot("file:///query.sql")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Variant != (dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}) || snapshot.DialectResolved {
		t.Fatalf("auto dialect snapshot = %#v, resolved=%v", snapshot.Variant, snapshot.DialectResolved)
	}
}

func TestEditorSnapshotPreservesConfiguredDriverWhenConnectionOmitsIt(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	if err := s.openFileAtVersion("file:///postgres.sql", "sql", `select "col" from t`, 1); err != nil {
		t.Fatal(err)
	}
	s.stateMu.Lock()
	s.WSCfg = &config.Config{Connections: []*database.DBConfig{{Driver: dialect.DatabaseDriverPostgreSQL}}}
	s.curDBCfg = &database.DBConfig{Driver: dialect.DatabaseDriverPostgreSQL}
	s.dbConn = &database.DBConnection{Variant: dialect.SQLVariantDefault}
	s.connectionState = connectionReady
	s.stateMu.Unlock()

	snapshot, err := s.captureEditorSnapshot("file:///postgres.sql")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Variant.Driver != dialect.DatabaseDriverPostgreSQL {
		t.Fatalf("attached driver = %q, want configured PostgreSQL", snapshot.Variant.Driver)
	}
	if snapshot.Repository == nil {
		t.Fatal("ready PostgreSQL connection should produce a repository")
	}
	snapshot.Text = "ILI"
	res, err := language.NewService().Complete(context.Background(), languageRequest(snapshot), language.Position{Line: 0, UTF16Column: 3})
	if err != nil {
		t.Fatalf("PostgreSQL completion parse: %v", err)
	}
	for _, item := range res.Value.Items {
		if item.Label == "ILIKE" {
			return
		}
	}
	t.Fatal("configured PostgreSQL keyword ILIKE missing after attached driver omitted its name")
}

func TestEditorSnapshotUsesAttachedInterBaseDialectAfterAutoDetection(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	if err := s.openFileAtVersion("file:///interbase.sql", "sql", `select "text" from t`, 1); err != nil {
		t.Fatal(err)
	}
	s.stateMu.Lock()
	s.curDBCfg = &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Dialect: 0}
	s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase1}
	s.connectionState = connectionReady
	s.stateMu.Unlock()
	snapshot, err := s.captureEditorSnapshot("file:///interbase.sql")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Variant != (dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase1}) || !snapshot.DialectResolved {
		t.Fatalf("resolved attached variant = %#v, resolved=%v", snapshot.Variant, snapshot.DialectResolved)
	}
}

func TestCompletionRemainsIncompleteBeforeMetadataStart(t *testing.T) {
	snapshot := editorSnapshot{
		ConnectionReady: true,
		Metadata: &database.MetadataSnapshot{
			Started: false,
			Status: map[database.MetadataKind]database.MetadataStatus{
				database.MetadataProcedures: {State: database.MetadataUnsupported},
			},
		},
	}
	if !completionIncomplete(snapshot, language.CompletionIncomplete(snapshot.Metadata)) {
		t.Fatal("ready attachment before MetadataLoader.Start must remain incomplete")
	}
	snapshot.Metadata.StartFailed = true
	if completionIncomplete(snapshot, language.CompletionIncomplete(snapshot.Metadata)) {
		t.Fatal("terminal metadata start failure must not remain incomplete")
	}
}

// TestCompletionStaysIncompleteForStillLoadingLeaseAfterConnectionDropsToIdle
// pins the case reachable through attachIntent's nil-Config early return
// (handler.go: intent.Config == nil sets connectionState to idle and
// returns ErrNoConnection without bumping connGeneration): a Metadata
// lease bound to the still-current connGeneration keeps reporting its own
// pending/loading state even once ConnectionReady has gone false, because
// dropping the newest connection ATTEMPT back to idle does not retract a
// lease already bound to the generation still in effect. Gating on
// ConnectionReady first — as a prior version of completionIncomplete did —
// would settle completion to false here and the client would never
// re-request once that lease finishes loading and its candidates arrive.
func TestCompletionStaysIncompleteForStillLoadingLeaseAfterConnectionDropsToIdle(t *testing.T) {
	snapshot := editorSnapshot{
		ConnectionReady: false,
		Metadata: &database.MetadataSnapshot{
			Started: true,
			Status: map[database.MetadataKind]database.MetadataStatus{
				database.MetadataProcedures: {State: database.MetadataLoading},
			},
		},
	}
	if !completionIncomplete(snapshot, language.CompletionIncomplete(snapshot.Metadata)) {
		t.Fatal("a still-loading lease must keep completion incomplete even after ConnectionReady drops to false")
	}

	snapshot.Metadata.Status[database.MetadataProcedures] = database.MetadataStatus{State: database.MetadataReady}
	if completionIncomplete(snapshot, language.CompletionIncomplete(snapshot.Metadata)) {
		t.Fatal("a settled lease must not report incomplete just because ConnectionReady is false")
	}
}

func TestCompletionStaysIncompleteInGatedReadyBeforeMetadataStartInterval(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	const uri = "file:///ready-before-metadata.sql"
	if err := s.openFileAtVersion(uri, "sql", "execute procedure ", 1); err != nil {
		t.Fatal(err)
	}
	s.stateMu.Lock()
	s.connGeneration = 1
	s.curDBCfg = &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Dialect: 3}
	s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}
	s.connectionState = connectionReady
	s.stateMu.Unlock()

	// Before any metadata lease is bound, completion must remain incomplete —
	// the same "attachment ready, metadata not yet started" window the old
	// loader model produced via Reset-without-Start.
	preBind, err := s.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal(err)
	}
	if !preBind.ConnectionReady || preBind.Metadata != nil {
		t.Fatalf("pre-bind snapshot = ready:%v metadata:%#v", preBind.ConnectionReady, preBind.Metadata)
	}
	if !completionIncomplete(preBind, language.CompletionIncomplete(preBind.Metadata)) {
		t.Fatal("completion incorrectly settled before any metadata lease was bound")
	}

	entered, release := make(chan struct{}), make(chan struct{})
	lease := bindMetadataRepoForTest(t, s, 1, gatedProcedureRepository{
		MockDBRepository: &database.MockDBRepository{}, entered: entered, release: release,
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("gated metadata job did not start")
	}
	loading, err := s.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal(err)
	}
	if loading.Metadata == nil || !loading.Metadata.Started {
		t.Fatalf("loading snapshot = metadata:%#v", loading.Metadata)
	}
	if !completionIncomplete(loading, language.CompletionIncomplete(loading.Metadata)) {
		t.Fatal("completion incorrectly settled while the only relevant category is loading")
	}
	close(release)
	waitMetadataSettledForTest(t, context.Background(), lease)

	final, err := s.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal(err)
	}
	if completionIncomplete(final, language.CompletionIncomplete(final.Metadata)) {
		t.Fatal("terminal ready category should settle completion")
	}
}

func TestProgressiveEditorSnapshotKeepsReadyProcedureWhenViewsFail(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	s.WSCfg = &config.Config{Connections: []*database.DBConfig{{Driver: dialect.DatabaseDriverInterBase, Dialect: 3}}}
	if err := s.openFileAtVersion("file:///query.sql", "sql", "execute procedure ", 1); err != nil {
		t.Fatal(err)
	}
	s.stateMu.Lock()
	s.connGeneration = 1
	s.stateMu.Unlock()
	lease := bindMetadataRepoForTest(t, s, 1, partialCatalogRepository{&database.MockDBRepository{}})
	waitMetadataSettledForTest(t, context.Background(), lease)

	snapshot, err := s.captureEditorSnapshot("file:///query.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshot.Cache.Procedure("READYPROC"); !ok {
		t.Fatal("ready procedure missing from partial cache")
	}
	if snapshot.Metadata.Status[database.MetadataViews].State != database.MetadataFailed {
		t.Fatalf("views state = %s, want failed", snapshot.Metadata.Status[database.MetadataViews].State)
	}
	res, err := language.NewService().Complete(context.Background(), languageRequest(snapshot), language.Position{Line: 0, UTF16Column: len(snapshot.Text)})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range res.Value.Items {
		if item.Label == "READYPROC" {
			found = true
		}
	}
	if !found {
		t.Fatal("procedure completion missing while unrelated views category failed")
	}
	if completionIncomplete(snapshot, language.CompletionIncomplete(snapshot.Metadata)) {
		t.Fatal("terminal failed category must not keep completion incomplete")
	}
	signatureText := "execute procedure readyproc("
	signature, err := SignatureHelpWithDriverVariant(context.Background(), signatureText, lsp.SignatureHelpParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{Position: lsp.Position{Line: 0, Character: len(signatureText)}},
	}, snapshot.Cache, snapshot.Variant)
	if err != nil {
		t.Fatal(err)
	}
	if signature == nil || len(signature.Signatures) != 1 || len(signature.Signatures[0].Parameters) != 1 || signature.Signatures[0].Parameters[0].Label != "P_IN" {
		t.Fatalf("partial-ready procedure signature = %#v", signature)
	}
}

func TestEditorSnapshotDoesNotMixLateConnectionAMetadataIntoB(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	uri := "file:///switch.sql"
	if err := s.openFileAtVersion(uri, "sql", "execute procedure ", 1); err != nil {
		t.Fatal(err)
	}
	s.stateMu.Lock()
	s.connGeneration = 1
	s.curDBCfg = &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Dialect: 3, Alias: "A"}
	s.connectionState = connectionReady
	s.stateMu.Unlock()
	entered, release := make(chan struct{}), make(chan struct{})
	aLease := bindMetadataRepoForTest(t, s, 1, gatedProcedureRepository{
		MockDBRepository: &database.MockDBRepository{}, entered: entered, release: release,
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("A metadata job did not start")
	}
	loading, err := s.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal(err)
	}
	if !completionIncomplete(loading, language.CompletionIncomplete(loading.Metadata)) {
		t.Fatal("completion should be incomplete while a relevant category is loading")
	}

	// Beginning B advances both connGeneration and metaBoundGeneration before
	// the late A result is released, exactly as bindMetadata's Acquire path
	// does on a genuinely different target: a new, independently tracked
	// store, never a mutation of A's.
	s.stateMu.Lock()
	s.connGeneration = 2
	s.curDBCfg = &database.DBConfig{Driver: dialect.DatabaseDriverPostgreSQL, Alias: "B"}
	s.connectionState = connectionConnecting
	s.stateMu.Unlock()
	bLease := bindMetadataRepoForTest(t, s, 2, readinessPlanRepository{DBRepository: &database.MockDBRepository{}, plan: database.MetadataPlan{Parallelism: 1}})
	s.stateMu.Lock()
	s.connectionState = connectionReady
	s.stateMu.Unlock()
	waitMetadataSettledForTest(t, context.Background(), bLease)

	close(release)
	waitMetadataSettledForTest(t, context.Background(), aLease)

	snapshot, err := s.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Generation != 2 || snapshot.Variant.Driver != dialect.DatabaseDriverPostgreSQL {
		t.Fatalf("B identity = generation %d / %s", snapshot.Generation, snapshot.Variant.Driver)
	}
	if _, ok := snapshot.Cache.Procedure("ONLY_A"); ok {
		t.Fatal("late A procedure leaked into B's cache")
	}
	if snapshot.MetadataIncarnation != bLease.Incarnation() {
		t.Fatal("B snapshot did not bind to B's own lease incarnation")
	}
}

func TestStaleDDLCompletionCannotPopulateNewGenerationMemo(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	s.stateMu.Lock()
	s.connGeneration = 1
	s.ddlMemo = make(map[ddlKey]string)
	s.stateMu.Unlock()
	entered, release := make(chan struct{}), make(chan struct{})
	repo := newStubDDLRepository(func(context.Context, database.ObjectKind, string) (string, error) {
		close(entered)
		<-release
		return "CREATE PROCEDURE P AS BEGIN SUSPEND; END", nil
	})
	done := make(chan struct{})
	go func() {
		s.memoisedObjectDDL(context.Background(), repo, hoverTarget{kind: database.ObjectKindProcedure, name: "P"}, 1)
		close(done)
	}()
	<-entered
	s.stateMu.Lock()
	s.connGeneration = 2
	s.ddlMemo = make(map[ddlKey]string)
	s.stateMu.Unlock()
	close(release)
	<-done
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if len(s.ddlMemo) != 0 {
		t.Fatalf("new generation memo contains stale entry: %#v", s.ddlMemo)
	}
}
