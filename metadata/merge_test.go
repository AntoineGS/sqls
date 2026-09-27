package metadata

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/sqls-server/sqls/catalog"
)

func TestMetadataMergeKeepsIndependentResults(t *testing.T) {
	before := newMetadataCache()
	first, err := mergeMetadata(before, catalog.MetadataProcedures, catalog.MetadataPatch{Cache: &catalog.Cache{
		Catalog: &catalog.CatalogCache{Procedures: map[string]*catalog.ProcedureDesc{
			"P": {Name: "P"},
		}},
	}, Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := mergeMetadata(first, catalog.MetadataViews, catalog.MetadataPatch{Cache: &catalog.Cache{
		Catalog: &catalog.CatalogCache{Views: map[string]*catalog.ViewDesc{}},
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
	if first.MetadataReady(catalog.MetadataViews) {
		t.Fatal("mutated prior states")
	}
}

func TestMetadataMergePrimaryKeysAndColumnsInEitherOrder(t *testing.T) {
	for _, order := range []string{"keys-first", "columns-first"} {
		t.Run(order, func(t *testing.T) {
			base := newMetadataCache()
			originalColumn := &catalog.ColumnDesc{ColumnBase: catalog.ColumnBase{Schema: "S", Table: "T", Name: "ID"}, Key: "generic"}
			base.ColumnsWithParent[catalog.ColumnDatabaseKey("S", "T")] = []*catalog.ColumnDesc{originalColumn}
			base.PrimaryKeyColumns[catalog.ColumnDatabaseKey("S", "T")] = map[string]struct{}{}
			var current *catalog.Cache
			var err error
			if order == "keys-first" {
				current, err = mergeMetadata(base, catalog.MetadataPrimaryKeys, catalog.MetadataPatch{Cache: &catalog.Cache{
					PrimaryKeyColumns: map[string]map[string]struct{}{catalog.ColumnDatabaseKey("S", "T"): {"ID": {}}},
				}})
				if err == nil {
					current, err = mergeMetadata(current, catalog.MetadataColumnsCurrent, catalog.MetadataPatch{Cache: &catalog.Cache{
						ColumnsWithParent: map[string][]*catalog.ColumnDesc{catalog.ColumnDatabaseKey("S", "T"): {originalColumn}},
					}})
				}
			} else {
				current, err = mergeMetadata(base, catalog.MetadataColumnsCurrent, catalog.MetadataPatch{Cache: &catalog.Cache{
					ColumnsWithParent: map[string][]*catalog.ColumnDesc{catalog.ColumnDatabaseKey("S", "T"): {originalColumn}},
				}})
				if err == nil {
					current, err = mergeMetadata(current, catalog.MetadataPrimaryKeys, catalog.MetadataPatch{Cache: &catalog.Cache{
						PrimaryKeyColumns: map[string]map[string]struct{}{catalog.ColumnDatabaseKey("S", "T"): {"ID": {}}},
					}})
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			column := current.ColumnsWithParent[catalog.ColumnDatabaseKey("S", "T")][0]
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

func TestMetadataMergePrimaryKeysByIDPreservesExactOrderAndIdentity(t *testing.T) {
	mixed := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "Foo"}.Key()
	upper := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "FOO"}.Key()
	fragment := &catalog.Cache{PrimaryKeyColumns: map[string]map[string]struct{}{}, PrimaryKeysByID: map[string][]string{
		mixed: {"B", "A"}, upper: {"ID"},
	}}
	published, err := Merge(NewCache(), catalog.MetadataPrimaryKeys, catalog.MetadataPatch{Cache: fragment})
	if err != nil {
		t.Fatal(err)
	}
	fragment.PrimaryKeysByID[mixed][0] = "changed"
	if !reflect.DeepEqual(published.PrimaryKeysByID[mixed], []string{"B", "A"}) {
		t.Fatal("PK order/ownership lost")
	}
	if !reflect.DeepEqual(published.PrimaryKeysByID[upper], []string{"ID"}) {
		t.Fatal("exact identities merged")
	}
}

func TestMetadataMergePrimaryKeysByIDNilWhenNotSupplied(t *testing.T) {
	merged, err := mergeMetadata(newMetadataCache(), catalog.MetadataPrimaryKeys, catalog.MetadataPatch{Cache: &catalog.Cache{
		PrimaryKeyColumns: map[string]map[string]struct{}{},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if merged.PrimaryKeysByID != nil {
		t.Fatalf("PrimaryKeysByID = %#v, want nil when the provider did not supply it", merged.PrimaryKeysByID)
	}
}

func TestMetadataMergePKEnrichmentPreservesOlderSnapshot(t *testing.T) {
	key := catalog.ColumnDatabaseKey("S", "T")
	column := &catalog.ColumnDesc{ColumnBase: catalog.ColumnBase{Schema: "S", Table: "T", Name: "ID"}, Key: ""}
	base := newMetadataCache()
	base.ColumnsWithParent[key] = []*catalog.ColumnDesc{column}
	withColumns, err := mergeMetadata(base, catalog.MetadataColumnsCurrent, catalog.MetadataPatch{Cache: &catalog.Cache{
		ColumnsWithParent: map[string][]*catalog.ColumnDesc{key: {column}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	withKeys, err := mergeMetadata(withColumns, catalog.MetadataPrimaryKeys, catalog.MetadataPatch{Cache: &catalog.Cache{
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
			currentData := map[string][]*catalog.ColumnDesc{"CURRENT\tT": {{ColumnBase: catalog.ColumnBase{Schema: "CURRENT", Table: "T", Name: "C"}}}}
			allData := map[string][]*catalog.ColumnDesc{"OTHER\tT": {{ColumnBase: catalog.ColumnBase{Schema: "OTHER", Table: "T", Name: "A"}}}}
			firstKind, secondKind := catalog.MetadataColumnsCurrent, catalog.MetadataColumnsAll
			firstData, secondData := currentData, allData
			if order == "all-first" {
				firstKind, secondKind = secondKind, firstKind
				firstData, secondData = secondData, firstData
			}
			first, err := mergeMetadata(base, firstKind, catalog.MetadataPatch{Cache: &catalog.Cache{ColumnsWithParent: firstData}})
			if err != nil {
				t.Fatal(err)
			}
			second, err := mergeMetadata(first, secondKind, catalog.MetadataPatch{Cache: &catalog.Cache{ColumnsWithParent: secondData}})
			if err != nil {
				t.Fatal(err)
			}
			if !second.MetadataReady(catalog.MetadataColumnsCurrent, catalog.MetadataColumnsAll) {
				t.Fatal("both column kinds should be ready")
			}
			if _, ok := second.ColumnDatabase("OTHER", "T"); !ok {
				t.Fatal("all-column result did not win")
			}
		})
	}
}

func TestMetadataMergeZeroColumnRelation(t *testing.T) {
	merged, err := mergeMetadata(newMetadataCache(), catalog.MetadataColumnsCurrent, catalog.MetadataPatch{Cache: &catalog.Cache{ColumnsWithParent: map[string][]*catalog.ColumnDesc{}}})
	if err != nil {
		t.Fatal(err)
	}
	if !merged.MetadataReady(catalog.MetadataColumnsCurrent) || len(merged.ColumnsWithParent) != 0 {
		t.Fatalf("zero-column category not ready: cache=%+v", merged)
	}
}

func TestMetadataMergeRejectsInvalidPatches(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  catalog.MetadataKind
		patch catalog.MetadataPatch
	}{
		{name: "unknown kind", kind: "not-a-kind", patch: catalog.MetadataPatch{Cache: &catalog.Cache{}}},
		{name: "nil cache", kind: catalog.MetadataSchemas},
		{name: "negative count", kind: catalog.MetadataSchemas, patch: catalog.MetadataPatch{Cache: &catalog.Cache{}, Count: -1}},
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
		kind  catalog.MetadataKind
		cache *catalog.Cache
	}{
		{catalog.MetadataSchemas, &catalog.Cache{}},
		{catalog.MetadataRelations, &catalog.Cache{}},
		{catalog.MetadataColumnsCurrent, &catalog.Cache{}},
		{catalog.MetadataColumnsAll, &catalog.Cache{}},
		{catalog.MetadataPrimaryKeys, &catalog.Cache{}},
		{catalog.MetadataForeignKeys, &catalog.Cache{}},
		{catalog.MetadataViews, &catalog.Cache{Catalog: &catalog.CatalogCache{}}},
		{catalog.MetadataProcedures, &catalog.Cache{Catalog: &catalog.CatalogCache{}}},
		{catalog.MetadataGenerators, &catalog.Cache{Catalog: &catalog.CatalogCache{}}},
		{catalog.MetadataDomains, &catalog.Cache{Catalog: &catalog.CatalogCache{}}},
		{catalog.MetadataFunctions, &catalog.Cache{Catalog: &catalog.CatalogCache{}}},
		{catalog.MetadataIndexes, &catalog.Cache{Catalog: &catalog.CatalogCache{}}},
		{catalog.MetadataTriggers, &catalog.Cache{Catalog: &catalog.CatalogCache{}}},
	}
	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			merged, err := mergeMetadata(newMetadataCache(), tc.kind, catalog.MetadataPatch{Cache: tc.cache})
			if err == nil {
				t.Fatalf("mergeMetadata() = (%#v, nil), want missing payload error", merged)
			}
		})
	}
}

func TestMetadataMergeAcceptsAllocatedEmptyCategoryPayload(t *testing.T) {
	patches := map[catalog.MetadataKind]*catalog.Cache{
		catalog.MetadataSchemas:        {Schemas: map[string]string{}},
		catalog.MetadataRelations:      {SchemaTables: map[string][]string{}},
		catalog.MetadataColumnsCurrent: {ColumnsWithParent: map[string][]*catalog.ColumnDesc{}},
		catalog.MetadataColumnsAll:     {ColumnsWithParent: map[string][]*catalog.ColumnDesc{}},
		catalog.MetadataPrimaryKeys:    {PrimaryKeyColumns: map[string]map[string]struct{}{}},
		catalog.MetadataForeignKeys:    {ForeignKeys: map[string]map[string][]*catalog.ForeignKey{}},
		catalog.MetadataViews:          {Catalog: &catalog.CatalogCache{Views: map[string]*catalog.ViewDesc{}}},
		catalog.MetadataProcedures:     {Catalog: &catalog.CatalogCache{Procedures: map[string]*catalog.ProcedureDesc{}}},
		catalog.MetadataGenerators:     {Catalog: &catalog.CatalogCache{Generators: map[string]*catalog.GeneratorDesc{}}},
		catalog.MetadataDomains:        {Catalog: &catalog.CatalogCache{Domains: map[string]*catalog.DomainDesc{}}},
		catalog.MetadataFunctions:      {Catalog: &catalog.CatalogCache{Functions: map[string]*catalog.FunctionDesc{}}},
		catalog.MetadataIndexes:        {Catalog: &catalog.CatalogCache{Indexes: map[string]*catalog.IndexDesc{}}},
		catalog.MetadataTriggers:       {Catalog: &catalog.CatalogCache{Triggers: map[string]*catalog.TriggerDesc{}}},
	}
	for kind, cache := range patches {
		t.Run(string(kind), func(t *testing.T) {
			merged, err := mergeMetadata(newMetadataCache(), kind, catalog.MetadataPatch{Cache: cache})
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
	key := &catalog.ForeignKey{{&catalog.ColumnBase{Table: "child"}, &catalog.ColumnBase{Table: "parent"}}, {&catalog.ColumnBase{Table: "child"}, nil}}
	_, err := mergeMetadata(newMetadataCache(), catalog.MetadataForeignKeys, catalog.MetadataPatch{Cache: &catalog.Cache{
		ForeignKeys: map[string]map[string][]*catalog.ForeignKey{"child": {"parent": {key}}},
	}})
	if err == nil {
		t.Fatal("mergeMetadata() accepted malformed later foreign-key pair")
	}
}

func TestMetadataMergeRebuildsGroupedCatalogMaps(t *testing.T) {
	index := &catalog.IndexDesc{Name: "IX_T", RelationName: "t"}
	indexA := &catalog.IndexDesc{Name: "A_IX_T", RelationName: "T"}
	indexZ := &catalog.IndexDesc{Name: "Z_IX_T", RelationName: "T"}
	blankIndex := &catalog.IndexDesc{Name: "IX_BLANK", RelationName: "   "}
	trigger := &catalog.TriggerDesc{Name: "TR_T", RelationName: sql.NullString{String: "T", Valid: true}}
	triggerA := &catalog.TriggerDesc{Name: "A_TR_T", RelationName: sql.NullString{String: "T", Valid: true}}
	triggerZ := &catalog.TriggerDesc{Name: "Z_TR_T", RelationName: sql.NullString{String: "T", Valid: true}}
	fragment := &catalog.Cache{Catalog: &catalog.CatalogCache{
		Indexes:         map[string]*catalog.IndexDesc{"IX_T": index, "A_IX_T": indexA, "Z_IX_T": indexZ, "IX_BLANK": blankIndex},
		IndexesByTable:  map[string][]*catalog.IndexDesc{"WRONG": {index}},
		Triggers:        map[string]*catalog.TriggerDesc{"TR_T": trigger, "A_TR_T": triggerA, "Z_TR_T": triggerZ},
		TriggersByTable: map[string][]*catalog.TriggerDesc{"WRONG": {trigger}},
	}}
	withIndexes, err := mergeMetadata(newMetadataCache(), catalog.MetadataIndexes, catalog.MetadataPatch{Cache: fragment})
	if err != nil {
		t.Fatal(err)
	}
	withTriggers, err := mergeMetadata(withIndexes, catalog.MetadataTriggers, catalog.MetadataPatch{Cache: fragment})
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
	fragmentProcedures := map[string]*catalog.ProcedureDesc{"P": {Name: "P"}}
	merged, err := mergeMetadata(newMetadataCache(), catalog.MetadataProcedures, catalog.MetadataPatch{Cache: &catalog.Cache{
		Catalog: &catalog.CatalogCache{Procedures: fragmentProcedures},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fragmentProcedures["P"] = &catalog.ProcedureDesc{Name: "MUTATED"}
	fragmentProcedures["LATE"] = &catalog.ProcedureDesc{Name: "LATE"}
	if procedure, ok := merged.Procedure("P"); !ok || procedure.Name != "P" {
		t.Fatalf("published procedure changed through fragment map: %#v, %v", procedure, ok)
	}
	if _, ok := merged.Procedure("LATE"); ok {
		t.Fatal("published map observed key added to fragment map")
	}
}

func TestMetadataMergePreservesGenericKeyWhenPKNotReady(t *testing.T) {
	key := catalog.ColumnDatabaseKey("S", "T")
	column := &catalog.ColumnDesc{ColumnBase: catalog.ColumnBase{Schema: "S", Table: "T", Name: "ID"}, Key: "PRI"}
	merged, err := mergeMetadata(newMetadataCache(), catalog.MetadataColumnsCurrent, catalog.MetadataPatch{Cache: &catalog.Cache{
		ColumnsWithParent: map[string][]*catalog.ColumnDesc{key: {column}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := merged.ColumnsWithParent[key][0].Key; got != "PRI" {
		t.Fatalf("generic key flag = %q, want PRI", got)
	}
}
