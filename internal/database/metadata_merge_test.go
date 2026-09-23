package database

import (
	"database/sql"
	"testing"
)

func TestMetadataMergeKeepsIndependentResults(t *testing.T) {
	before := newMetadataCache()
	first, err := mergeMetadata(before, MetadataProcedures, MetadataPatch{Cache: &DBCache{
		Catalog: &CatalogCache{Procedures: map[string]*ProcedureDesc{
			"P": {Name: "P"},
		}},
	}, Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := mergeMetadata(first, MetadataViews, MetadataPatch{Cache: &DBCache{
		Catalog: &CatalogCache{Views: map[string]*ViewDesc{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := second.Procedure("P"); !ok {
		t.Fatal("lost sibling result")
	}
	if before.HasCatalog() {
		t.Fatal("mutated prior cache")
	}
	if first.MetadataReady(MetadataViews) {
		t.Fatal("mutated prior states")
	}
}

func TestMetadataMergePrimaryKeysAndColumnsInEitherOrder(t *testing.T) {
	for _, order := range []string{"keys-first", "columns-first"} {
		t.Run(order, func(t *testing.T) {
			base := newMetadataCache()
			originalColumn := &ColumnDesc{ColumnBase: ColumnBase{Schema: "S", Table: "T", Name: "ID"}, Key: "generic"}
			base.ColumnsWithParent[columnDatabaseKey("S", "T")] = []*ColumnDesc{originalColumn}
			base.PrimaryKeyColumns[columnDatabaseKey("S", "T")] = map[string]struct{}{}
			var current *DBCache
			var err error
			if order == "keys-first" {
				current, err = mergeMetadata(base, MetadataPrimaryKeys, MetadataPatch{Cache: &DBCache{
					PrimaryKeyColumns: map[string]map[string]struct{}{columnDatabaseKey("S", "T"): {"ID": {}}},
				}})
				if err == nil {
					current, err = mergeMetadata(current, MetadataColumnsCurrent, MetadataPatch{Cache: &DBCache{
						ColumnsWithParent: map[string][]*ColumnDesc{columnDatabaseKey("S", "T"): {originalColumn}},
					}})
				}
			} else {
				current, err = mergeMetadata(base, MetadataColumnsCurrent, MetadataPatch{Cache: &DBCache{
					ColumnsWithParent: map[string][]*ColumnDesc{columnDatabaseKey("S", "T"): {originalColumn}},
				}})
				if err == nil {
					current, err = mergeMetadata(current, MetadataPrimaryKeys, MetadataPatch{Cache: &DBCache{
						PrimaryKeyColumns: map[string]map[string]struct{}{columnDatabaseKey("S", "T"): {"ID": {}}},
					}})
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			column := current.ColumnsWithParent[columnDatabaseKey("S", "T")][0]
			if column == nil || column.Key != "YES" {
				t.Fatalf("merged column key = %v, want YES", column)
			}
			if originalColumn.Key != "generic" {
				t.Fatalf("mutated retained descriptor: Key = %q", originalColumn.Key)
			}
			if column == originalColumn {
				t.Fatal("published enriched descriptor aliases retained descriptor")
			}
		})
	}
}

func TestMetadataMergePKEnrichmentPreservesOlderSnapshot(t *testing.T) {
	key := columnDatabaseKey("S", "T")
	column := &ColumnDesc{ColumnBase: ColumnBase{Schema: "S", Table: "T", Name: "ID"}, Key: ""}
	base := newMetadataCache()
	base.ColumnsWithParent[key] = []*ColumnDesc{column}
	withColumns, err := mergeMetadata(base, MetadataColumnsCurrent, MetadataPatch{Cache: &DBCache{
		ColumnsWithParent: map[string][]*ColumnDesc{key: {column}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	withKeys, err := mergeMetadata(withColumns, MetadataPrimaryKeys, MetadataPatch{Cache: &DBCache{
		PrimaryKeyColumns: map[string]map[string]struct{}{key: {"ID": {}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := withColumns.ColumnsWithParent[key][0].Key; got != "" {
		t.Fatalf("old snapshot Key = %q, want unknown", got)
	}
	if got := withKeys.ColumnsWithParent[key][0].Key; got != "YES" {
		t.Fatalf("new snapshot Key = %q, want YES", got)
	}
}

func TestMetadataMergeColumnsCurrentAndAllCompletionOrders(t *testing.T) {
	for _, order := range []string{"current-first", "all-first"} {
		t.Run(order, func(t *testing.T) {
			base := newMetadataCache()
			currentData := map[string][]*ColumnDesc{"CURRENT\tT": {{ColumnBase: ColumnBase{Schema: "CURRENT", Table: "T", Name: "C"}}}}
			allData := map[string][]*ColumnDesc{"OTHER\tT": {{ColumnBase: ColumnBase{Schema: "OTHER", Table: "T", Name: "A"}}}}
			firstKind, secondKind := MetadataColumnsCurrent, MetadataColumnsAll
			firstData, secondData := currentData, allData
			if order == "all-first" {
				firstKind, secondKind = secondKind, firstKind
				firstData, secondData = secondData, firstData
			}
			first, err := mergeMetadata(base, firstKind, MetadataPatch{Cache: &DBCache{ColumnsWithParent: firstData}})
			if err != nil {
				t.Fatal(err)
			}
			second, err := mergeMetadata(first, secondKind, MetadataPatch{Cache: &DBCache{ColumnsWithParent: secondData}})
			if err != nil {
				t.Fatal(err)
			}
			if !second.MetadataReady(MetadataColumnsCurrent, MetadataColumnsAll) {
				t.Fatal("both column kinds should be ready")
			}
			if _, ok := second.ColumnDatabase("OTHER", "T"); !ok {
				t.Fatal("all-column result did not win")
			}
		})
	}
}

func TestMetadataMergeZeroColumnRelation(t *testing.T) {
	merged, err := mergeMetadata(newMetadataCache(), MetadataColumnsCurrent, MetadataPatch{Cache: &DBCache{ColumnsWithParent: map[string][]*ColumnDesc{}}})
	if err != nil {
		t.Fatal(err)
	}
	if !merged.MetadataReady(MetadataColumnsCurrent) || len(merged.ColumnsWithParent) != 0 {
		t.Fatalf("zero-column category not ready: cache=%+v", merged)
	}
}

func TestMetadataMergeRejectsInvalidPatches(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  MetadataKind
		patch MetadataPatch
	}{
		{name: "unknown kind", kind: "not-a-kind", patch: MetadataPatch{Cache: &DBCache{}}},
		{name: "nil cache", kind: MetadataSchemas},
		{name: "negative count", kind: MetadataSchemas, patch: MetadataPatch{Cache: &DBCache{}, Count: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := mergeMetadata(newMetadataCache(), tc.kind, tc.patch); err == nil {
				t.Fatal("mergeMetadata() error = nil, want error")
			}
		})
	}
}

func TestMetadataMergeRebuildsGroupedCatalogMaps(t *testing.T) {
	index := &IndexDesc{Name: "IX_T", RelationName: "t"}
	trigger := &TriggerDesc{Name: "TR_T", RelationName: sql.NullString{String: "T", Valid: true}}
	fragment := &DBCache{Catalog: &CatalogCache{
		Indexes:         map[string]*IndexDesc{"IX_T": index},
		IndexesByTable:  map[string][]*IndexDesc{"WRONG": {index}},
		Triggers:        map[string]*TriggerDesc{"TR_T": trigger},
		TriggersByTable: map[string][]*TriggerDesc{"WRONG": {trigger}},
	}}
	withIndexes, err := mergeMetadata(newMetadataCache(), MetadataIndexes, MetadataPatch{Cache: fragment})
	if err != nil {
		t.Fatal(err)
	}
	withTriggers, err := mergeMetadata(withIndexes, MetadataTriggers, MetadataPatch{Cache: fragment})
	if err != nil {
		t.Fatal(err)
	}
	if got := withTriggers.IndexesForTable("T"); len(got) != 1 || got[0] != index {
		t.Fatalf("rebuilt index group = %#v", got)
	}
	if got := withTriggers.TriggersForTable("T"); len(got) != 1 || got[0] != trigger {
		t.Fatalf("rebuilt trigger group = %#v", got)
	}
	if len(withTriggers.IndexesForTable("WRONG")) != 0 || len(withTriggers.TriggersForTable("WRONG")) != 0 {
		t.Fatal("accepted inconsistent source grouping")
	}
	if len(fragment.Catalog.IndexesByTable["WRONG"]) != 1 {
		t.Fatal("mutated source grouping")
	}
}

func TestMetadataMergePreservesGenericKeyWhenPKNotReady(t *testing.T) {
	key := columnDatabaseKey("S", "T")
	column := &ColumnDesc{ColumnBase: ColumnBase{Schema: "S", Table: "T", Name: "ID"}, Key: "PRI"}
	merged, err := mergeMetadata(newMetadataCache(), MetadataColumnsCurrent, MetadataPatch{Cache: &DBCache{
		ColumnsWithParent: map[string][]*ColumnDesc{key: {column}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := merged.ColumnsWithParent[key][0].Key; got != "PRI" {
		t.Fatalf("generic key flag = %q, want PRI", got)
	}
}
