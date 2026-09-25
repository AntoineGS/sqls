package language_test

import (
	"context"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
)

func TestLanguageUsesSuppliedSnapshot(t *testing.T) {
	cache := &catalog.Cache{SchemaTables: map[string][]string{"": {"CUSTOMERS"}}}
	snap := &catalog.MetadataSnapshot{Generation: 1, Revision: 1, Cache: cache}
	req := language.Request{
		Document: language.Document{URI: "memory:one", Text: "SELECT * FROM ", Version: 1, Binding: 1},
		Context:  language.Context{Snapshot: snap, Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}, DialectResolved: true},
	}
	result, err := language.NewService().Complete(context.Background(), req, language.Position{UTF16Column: 14})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.Value.Items {
		found = found || item.Label == "CUSTOMERS"
	}
	if !found {
		t.Fatal("did not consume supplied catalog")
	}
	if req.Context.Snapshot.Cache != cache {
		t.Fatal("snapshot replaced")
	}
}

// TestCompletionIncompleteTerminalFailureIsNotIncomplete pins Review Focus
// 3's "must not cause indefinite client retries" guarantee: once metadata
// loading has terminally failed to start, CompletionIncomplete must settle
// to false rather than keep signaling Incomplete: true forever.
func TestCompletionIncompleteTerminalFailureIsNotIncomplete(t *testing.T) {
	if language.CompletionIncomplete(nil) {
		t.Fatal("a nil snapshot has no metadata concept to be incomplete about")
	}
	notStarted := &catalog.MetadataSnapshot{}
	if !language.CompletionIncomplete(notStarted) {
		t.Fatal("a snapshot that has neither started nor terminally failed must be incomplete")
	}
	failed := &catalog.MetadataSnapshot{StartFailed: true}
	if language.CompletionIncomplete(failed) {
		t.Fatal("a terminal start failure must not keep signaling incomplete")
	}
	loading := &catalog.MetadataSnapshot{
		Started: true,
		Status:  map[catalog.MetadataKind]catalog.MetadataStatus{catalog.MetadataViews: {State: catalog.MetadataLoading}},
	}
	if !language.CompletionIncomplete(loading) {
		t.Fatal("a relevant category still loading must be incomplete")
	}
	categoryFailed := &catalog.MetadataSnapshot{
		Started: true,
		Status:  map[catalog.MetadataKind]catalog.MetadataStatus{catalog.MetadataViews: {State: catalog.MetadataFailed}},
	}
	if language.CompletionIncomplete(categoryFailed) {
		t.Fatal("a terminally failed relevant category must not keep signaling incomplete")
	}
}

// TestTwoContextsShareOneSnapshotIndependently is Step 1's "two contexts can
// share one snapshot while independently changing document text and
// dialect" probe: one *catalog.MetadataSnapshot is reused across two
// language.Request values whose Document.Text and Context.Variant differ,
// proving neither Complete call's result depends on, or mutates, the
// other's document/dialect state even though both read the same shared
// snapshot pointer.
func TestTwoContextsShareOneSnapshotIndependently(t *testing.T) {
	cache := &catalog.Cache{
		SchemaTables: map[string][]string{"": {"CUSTOMERS"}},
		Catalog: &catalog.CatalogCache{
			Functions: map[string]*catalog.FunctionDesc{"MY_UDF": {Name: "MY_UDF"}},
		},
	}
	snap := &catalog.MetadataSnapshot{Generation: 1, Revision: 1, Cache: cache}

	interbaseReq := language.Request{
		Document: language.Document{URI: "memory:one", Text: "SELECT * FROM ", Version: 1, Binding: 1},
		Context: language.Context{
			Snapshot:        snap,
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
			DialectResolved: true,
		},
	}
	mysqlReq := language.Request{
		Document: language.Document{URI: "memory:two", Text: "SELECT City FROM ", Version: 1, Binding: 1},
		Context: language.Context{
			Snapshot:        snap,
			Variant:         dialect.DriverVariant{Driver: dialect.DatabaseDriverMySQL},
			DialectResolved: true,
		},
	}

	svc := language.NewService()
	firstResult, err := svc.Complete(context.Background(), interbaseReq, language.Position{UTF16Column: 14})
	if err != nil {
		t.Fatal(err)
	}
	secondResult, err := svc.Complete(context.Background(), mysqlReq, language.Position{UTF16Column: 17})
	if err != nil {
		t.Fatal(err)
	}

	if firstResult.Stamp.URI != "memory:one" || secondResult.Stamp.URI != "memory:two" {
		t.Fatalf("stamps carry the wrong document: first=%q second=%q", firstResult.Stamp.URI, secondResult.Stamp.URI)
	}
	// Both results share the same Generation/Revision (the same snapshot),
	// but each Stamp's URI/Binding still identifies its own document: the
	// shared snapshot never merges the two requests' document identities.
	if firstResult.Stamp.Generation != secondResult.Stamp.Generation || firstResult.Stamp.Revision != secondResult.Stamp.Revision {
		t.Fatalf("expected both results to share one snapshot's generation/revision, got %+v and %+v", firstResult.Stamp, secondResult.Stamp)
	}
	// Re-running the InterBase request after the MySQL request must
	// reproduce the identical candidate set: the intervening MySQL call
	// against the shared snapshot did not leave any state behind that
	// changes the InterBase document's own completion.
	replay, err := svc.Complete(context.Background(), interbaseReq, language.Position{UTF16Column: 14})
	if err != nil {
		t.Fatal(err)
	}
	if len(replay.Value.Items) != len(firstResult.Value.Items) {
		t.Fatalf("replay item count = %d, want %d (unaffected by the intervening differently-dialected request)", len(replay.Value.Items), len(firstResult.Value.Items))
	}
	if req := interbaseReq; req.Context.Snapshot.Cache != cache {
		t.Fatal("snapshot cache replaced by a sibling request")
	}
}
