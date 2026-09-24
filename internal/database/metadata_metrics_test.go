package database

import (
	"context"
	"testing"
)

func TestMetadataMetricsGenericRepositoriesMarkQueryCountUnknown(t *testing.T) {
	loader := NewMetadataLoader()
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	load, err := loader.Start(context.Background(), 1, NewMockDBRepository(nil))
	if err != nil {
		t.Fatal(err)
	}
	waitLoad(t, load)
	status := loader.Snapshot().Status[MetadataRelations]
	if status.QueriesKnown || status.Queries != 0 {
		t.Fatalf("generic query accounting = %+v, want unknown", status)
	}
}
