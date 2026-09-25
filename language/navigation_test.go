package language_test

import (
	"context"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

func TestSourceSharingBetweenHoverAndDefinition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	registry := metadata.NewRegistry(ctx, metadata.Options{})
	defer registry.Close(ctx)
	provider := metadatatest.New()
	provider.DetailValue = metadata.Detail{
		Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"},
		Text:   "CREATE TABLE T (ID INTEGER)", Provenance: "generated-ddl",
	}
	key := metadata.Key{Engine: "interbase", URL: "ib://host/db"}
	lease, err := registry.Acquire(ctx, key, metadata.Scope{Principal: "USER"}, provider.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := metadatatest.WaitReady(ctx, lease); err != nil {
		t.Fatal(err)
	}
	req := language.Request{
		Document: language.Document{URI: "file:///query.sql", Text: "SELECT * FROM T", Version: 1, Binding: 1},
		Context: language.Context{Key: key, Incarnation: lease.Incarnation(), Snapshot: lease.Snapshot(),
			Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}, DialectResolved: true, Details: lease},
	}
	service := language.NewService()
	position := language.Position{Line: 0, UTF16Column: 14}
	if _, err := service.Hover(ctx, req, position); err != nil {
		t.Fatalf("Hover: %v", err)
	}
	definition, err := service.Definition(ctx, req, position)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(definition.Value) == 0 || definition.Value[0].Source == nil {
		t.Fatalf("Definition did not return source: %#v", definition.Value)
	}
	if got := provider.DetailCalls.Load(); got != 1 {
		t.Fatalf("Hover+Definition loaded detail %d times, want one", got)
	}
}

func TestOldBindingDefinitionResultRejected(t *testing.T) {
	ctx := context.Background()
	gate := make(chan struct{})
	provider := metadatatest.New()
	provider.DetailGate = gate
	provider.DetailValue = metadata.Detail{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Text: "CREATE TABLE T (ID INTEGER)", Provenance: "generated-ddl"}
	registry := metadata.NewRegistry(ctx, metadata.Options{})
	defer registry.Close(ctx)
	key := metadata.Key{Engine: "interbase", URL: "ib://host/db"}
	lease, err := registry.Acquire(ctx, key, metadata.Scope{Principal: "USER"}, provider.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := metadatatest.WaitReady(ctx, lease); err != nil {
		t.Fatal(err)
	}
	req := language.Request{Document: language.Document{URI: "file:///q.sql", Text: "SELECT * FROM T", Version: 4, Binding: 8},
		Context: language.Context{Key: key, Incarnation: lease.Incarnation(), Snapshot: lease.Snapshot(),
			Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}, Details: lease}}
	done := make(chan struct{})
	var result language.Result[[]language.Location]
	var callErr error
	go func() {
		result, callErr = language.NewService().Definition(ctx, req, language.Position{Line: 0, UTF16Column: 14})
		close(done)
	}()
	select {
	case <-provider.DetailEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("definition did not request detail")
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("definition did not finish")
	}
	if callErr != nil {
		t.Fatalf("Definition: %v", callErr)
	}
	completedDefinition := result
	currentDocument := req.Document
	currentDocument.Binding++
	if language.Current(completedDefinition.Stamp, currentDocument, req.Context) {
		t.Fatal("definition from old database binding accepted")
	}
}
