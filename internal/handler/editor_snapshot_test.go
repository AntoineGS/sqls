package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/completer"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
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

func TestEditorSnapshotRejectsMismatchedMetadataGeneration(t *testing.T) {
	s := NewServer()
	t.Cleanup(func() { _ = s.Stop(); <-s.cleanupDone })
	if err := s.openFileAtVersion("file:///query.sql", "sql", "SELECT 1", 1); err != nil {
		t.Fatal(err)
	}
	s.stateMu.Lock()
	s.connGeneration = 2
	s.stateMu.Unlock()
	s.metadata.Reset(3) // simulate a loader/server transition observed mid-publication

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
	c := completer.NewCompleter(snapshot.Cache)
	c.Driver, c.Variant = snapshot.Variant.Driver, snapshot.Variant.Variant
	items, err := c.Complete("ILI", lsp.CompletionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		Position: lsp.Position{Line: 0, Character: 3},
	}}, false)
	if err != nil {
		t.Fatalf("PostgreSQL completion parse: %v", err)
	}
	for _, item := range items {
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
	if !completionMetadataIncomplete(snapshot) {
		t.Fatal("ready attachment before MetadataLoader.Start must remain incomplete")
	}
	snapshot.Metadata.StartFailed = true
	if completionMetadataIncomplete(snapshot) {
		t.Fatal("terminal metadata start failure must not remain incomplete")
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
	s.metadata.Reset(1)
	s.stateMu.Unlock()
	ready, start := make(chan struct{}), make(chan struct{})
	started := make(chan error, 1)
	go func() {
		close(ready) // attachment is visible; the coordinator has not called Start yet
		<-start
		load, err := s.metadata.Start(context.Background(), 1, partialCatalogRepository{&database.MockDBRepository{}})
		if err == nil {
			select {
			case <-load.Done:
			case <-time.After(time.Second):
				err = errors.New("metadata jobs did not settle")
			}
		}
		started <- err
	}()
	<-ready
	snapshot, err := s.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.ConnectionReady || snapshot.Metadata == nil || snapshot.Metadata.Started {
		t.Fatalf("gated pre-start snapshot = ready:%v metadata:%#v", snapshot.ConnectionReady, snapshot.Metadata)
	}
	if !completionMetadataIncomplete(snapshot) {
		t.Fatal("completion incorrectly settled in attachment-to-Start interval")
	}
	close(start)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	snapshot, err = s.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal(err)
	}
	if completionMetadataIncomplete(snapshot) {
		t.Fatal("terminal failed/ready categories should settle completion")
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
	s.metadata.Reset(1)
	load, err := s.metadata.Start(context.Background(), 1, partialCatalogRepository{&database.MockDBRepository{}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-load.Done:
	case <-time.After(time.Second):
		t.Fatal("metadata plan did not settle")
	}

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
	params := lsp.CompletionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: snapshot.URI},
		Position:     lsp.Position{Line: 0, Character: len(snapshot.Text)},
	}}
	c := completer.NewCompleter(snapshot.Cache)
	c.Driver, c.Variant = snapshot.Variant.Driver, snapshot.Variant.Variant
	items, err := c.Complete(snapshot.Text, params, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.Label == "READYPROC" {
			found = true
		}
	}
	if !found {
		t.Fatal("procedure completion missing while unrelated views category failed")
	}
	if completionMetadataIncomplete(snapshot) {
		t.Fatal("terminal failed category must not keep completion incomplete")
	}
	signatureText := "execute procedure readyproc("
	signature, err := SignatureHelpWithDriverVariant(signatureText, lsp.SignatureHelpParams{
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
	s.stateMu.Unlock()
	s.metadata.Reset(1)
	entered, release := make(chan struct{}), make(chan struct{})
	load, err := s.metadata.Start(context.Background(), 1, gatedProcedureRepository{
		MockDBRepository: &database.MockDBRepository{}, entered: entered, release: release,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("A metadata job did not start")
	}
	loading, err := s.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal(err)
	}
	if !completionMetadataIncomplete(loading) {
		t.Fatal("completion should be incomplete while a relevant category is loading")
	}

	// Beginning B advances both generation owners before the late A result is
	// released, exactly as the coordinator's transition fence does.
	s.stateMu.Lock()
	s.connGeneration = 2
	s.curDBCfg = &database.DBConfig{Driver: dialect.DatabaseDriverPostgreSQL, Alias: "B"}
	s.connectionState = connectionConnecting
	s.metadata.Reset(2)
	s.stateMu.Unlock()
	close(release)
	select {
	case <-load.Done:
	case <-time.After(time.Second):
		t.Fatal("superseded A metadata generation did not settle")
	}
	if err := s.metadata.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

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
