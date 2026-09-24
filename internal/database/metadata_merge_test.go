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

func TestMetadataMergeRejectsMissingCategoryPayload(t *testing.T) {
	tests := []struct {
		kind  MetadataKind
		cache *DBCache
	}{
		{MetadataSchemas, &DBCache{}},
		{MetadataRelations, &DBCache{}},
		{MetadataColumnsCurrent, &DBCache{}},
		{MetadataColumnsAll, &DBCache{}},
		{MetadataPrimaryKeys, &DBCache{}},
		{MetadataForeignKeys, &DBCache{}},
		{MetadataViews, &DBCache{Catalog: &CatalogCache{}}},
		{MetadataProcedures, &DBCache{Catalog: &CatalogCache{}}},
		{MetadataGenerators, &DBCache{Catalog: &CatalogCache{}}},
		{MetadataDomains, &DBCache{Catalog: &CatalogCache{}}},
		{MetadataFunctions, &DBCache{Catalog: &CatalogCache{}}},
		{MetadataIndexes, &DBCache{Catalog: &CatalogCache{}}},
		{MetadataTriggers, &DBCache{Catalog: &CatalogCache{}}},
	}
	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			merged, err := mergeMetadata(newMetadataCache(), tc.kind, MetadataPatch{Cache: tc.cache})
			if err == nil {
				t.Fatalf("mergeMetadata() = (%#v, nil), want missing payload error", merged)
			}
		})
	}
}

func TestMetadataMergeAcceptsAllocatedEmptyCategoryPayload(t *testing.T) {
	patches := map[MetadataKind]*DBCache{
		MetadataSchemas:        {Schemas: map[string]string{}},
		MetadataRelations:      {SchemaTables: map[string][]string{}},
		MetadataColumnsCurrent: {ColumnsWithParent: map[string][]*ColumnDesc{}},
		MetadataColumnsAll:     {ColumnsWithParent: map[string][]*ColumnDesc{}},
		MetadataPrimaryKeys:    {PrimaryKeyColumns: map[string]map[string]struct{}{}},
		MetadataForeignKeys:    {ForeignKeys: map[string]map[string][]*ForeignKey{}},
		MetadataViews:          {Catalog: &CatalogCache{Views: map[string]*ViewDesc{}}},
		MetadataProcedures:     {Catalog: &CatalogCache{Procedures: map[string]*ProcedureDesc{}}},
		MetadataGenerators:     {Catalog: &CatalogCache{Generators: map[string]*GeneratorDesc{}}},
		MetadataDomains:        {Catalog: &CatalogCache{Domains: map[string]*DomainDesc{}}},
		MetadataFunctions:      {Catalog: &CatalogCache{Functions: map[string]*FunctionDesc{}}},
		MetadataIndexes:        {Catalog: &CatalogCache{Indexes: map[string]*IndexDesc{}}},
		MetadataTriggers:       {Catalog: &CatalogCache{Triggers: map[string]*TriggerDesc{}}},
	}
	for kind, cache := range patches {
		t.Run(string(kind), func(t *testing.T) {
			merged, err := mergeMetadata(newMetadataCache(), kind, MetadataPatch{Cache: cache})
			if err != nil {
				t.Fatalf("mergeMetadata() error = %v", err)
			}
			if !merged.MetadataReady(kind) {
				t.Fatal("allocated empty payload should be ready")
			}
		})
	}
}

func TestMetadataMergeRejectsMalformedForeignKeyLaterPair(t *testing.T) {
	key := &ForeignKey{{&ColumnBase{Table: "child"}, &ColumnBase{Table: "parent"}}, {&ColumnBase{Table: "child"}, nil}}
	_, err := mergeMetadata(newMetadataCache(), MetadataForeignKeys, MetadataPatch{Cache: &DBCache{
		ForeignKeys: map[string]map[string][]*ForeignKey{"child": {"parent": {key}}},
	}})
	if err == nil {
		t.Fatal("mergeMetadata() accepted malformed later foreign-key pair")
	}
}

func TestMetadataMergeRebuildsGroupedCatalogMaps(t *testing.T) {
	index := &IndexDesc{Name: "IX_T", RelationName: "t"}
	indexA := &IndexDesc{Name: "A_IX_T", RelationName: "T"}
	indexZ := &IndexDesc{Name: "Z_IX_T", RelationName: "T"}
	blankIndex := &IndexDesc{Name: "IX_BLANK", RelationName: "   "}
	trigger := &TriggerDesc{Name: "TR_T", RelationName: sql.NullString{String: "T", Valid: true}}
	triggerA := &TriggerDesc{Name: "A_TR_T", RelationName: sql.NullString{String: "T", Valid: true}}
	triggerZ := &TriggerDesc{Name: "Z_TR_T", RelationName: sql.NullString{String: "T", Valid: true}}
	fragment := &DBCache{Catalog: &CatalogCache{
		Indexes:         map[string]*IndexDesc{"IX_T": index, "A_IX_T": indexA, "Z_IX_T": indexZ, "IX_BLANK": blankIndex},
		IndexesByTable:  map[string][]*IndexDesc{"WRONG": {index}},
		Triggers:        map[string]*TriggerDesc{"TR_T": trigger, "A_TR_T": triggerA, "Z_TR_T": triggerZ},
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
	if got := withTriggers.IndexesForTable("T"); len(got) != 3 || got[0] != indexA || got[1] != index || got[2] != indexZ {
		t.Fatalf("rebuilt index group = %#v", got)
	}
	if got := withTriggers.TriggersForTable("T"); len(got) != 3 || got[0] != triggerA || got[1] != trigger || got[2] != triggerZ {
		t.Fatalf("rebuilt trigger group = %#v", got)
	}
	if len(withTriggers.IndexesForTable("WRONG")) != 0 || len(withTriggers.TriggersForTable("WRONG")) != 0 || len(withTriggers.IndexesForTable("")) != 0 {
		t.Fatal("accepted inconsistent source grouping")
	}
	if len(fragment.Catalog.IndexesByTable["WRONG"]) != 1 {
		t.Fatal("mutated source grouping")
	}
}

func TestMetadataMergeClonesPublishedCatalogMap(t *testing.T) {
	fragmentProcedures := map[string]*ProcedureDesc{"P": {Name: "P"}}
	merged, err := mergeMetadata(newMetadataCache(), MetadataProcedures, MetadataPatch{Cache: &DBCache{
		Catalog: &CatalogCache{Procedures: fragmentProcedures},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fragmentProcedures["P"] = &ProcedureDesc{Name: "MUTATED"}
	fragmentProcedures["LATE"] = &ProcedureDesc{Name: "LATE"}
	if procedure, ok := merged.Procedure("P"); !ok || procedure.Name != "P" {
		t.Fatalf("published procedure changed through fragment map: %#v, %v", procedure, ok)
	}
	if _, ok := merged.Procedure("LATE"); ok {
		t.Fatal("published map observed key added to fragment map")
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
