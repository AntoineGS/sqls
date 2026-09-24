package metadata_test

import (
	"testing"

	"github.com/sqls-server/sqls/catalog"
)

func TestPublicSnapshotSharesDescriptors(t *testing.T) {
	col := &catalog.ColumnDesc{ColumnBase: catalog.ColumnBase{Table: "T", Name: "ID"}}
	cache := &catalog.Cache{ColumnsWithParent: map[string][]*catalog.ColumnDesc{"\tT": {col}}}
	snapshot := &catalog.MetadataSnapshot{Cache: cache}
	got, ok := snapshot.Cache.ColumnDescs("T")
	if !ok || got[0] != col {
		t.Fatal("catalog projection copied descriptors")
	}
}
