package database

import (
	"context"
	"testing"

	"github.com/sqls-server/sqls/metadata"
)

func TestMetadataMetricsGenericRepositoriesMarkQueryCountUnknown(t *testing.T) {
	loader := metadata.NewLoader(nil)
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	load, err := loader.Start(context.Background(), 1, metadataPlanFor(NewMockDBRepository(nil)))
	if err != nil {
		t.Fatal(err)
	}
	waitLoad(t, load)
	status := loader.Snapshot().Status[MetadataRelations]
	if status.QueriesKnown || status.Queries != 0 {
		t.Fatalf("generic query accounting = %+v, want unknown", status)
	}
}
