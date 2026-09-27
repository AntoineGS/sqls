package language

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

func TestSemanticCatalogRetainsOnlySnapshotPointer(t *testing.T) {
	typ := reflect.TypeOf(snapshotSemanticCatalog{})
	if typ.NumField() != 1 || typ.Field(0).Type != reflect.TypeOf((*catalog.MetadataSnapshot)(nil)) {
		t.Fatalf("snapshotSemanticCatalog shape=%v; want only *catalog.MetadataSnapshot, no copied descriptor maps", typ)
	}
}

func readySnapshot(states map[catalog.MetadataKind]catalog.MetadataState, cc *catalog.CatalogCache) *catalog.MetadataSnapshot {
	return &catalog.MetadataSnapshot{Cache: &catalog.Cache{Catalog: cc, Metadata: states}}
}

func TestSemanticSnapshotRequiresCompleteCompositeRelationNamespace(t *testing.T) {
	for _, kind := range []catalog.MetadataKind{catalog.MetadataRelations, catalog.MetadataViews, catalog.MetadataProcedures} {
		for _, state := range []catalog.MetadataState{catalog.MetadataPending, catalog.MetadataLoading, catalog.MetadataFailed} {
			t.Run(string(kind)+"/"+string(state), func(t *testing.T) {
				states := map[catalog.MetadataKind]catalog.MetadataState{
					catalog.MetadataRelations:  catalog.MetadataReady,
					catalog.MetadataViews:      catalog.MetadataReady,
					catalog.MetadataProcedures: catalog.MetadataReady,
				}
				states[kind] = state
				view := snapshotSemanticCatalog{snapshot: readySnapshot(states, &catalog.CatalogCache{
					ViewsByID: map[string]*catalog.ViewDesc{}, ProceduresByID: map[string]*catalog.ProcedureDesc{},
				})}
				if _, knowledge := view.RelationInfo(sqlsymbol.Name{Text: "ABSENT"}); knowledge != sqlsymbol.Unknown {
					t.Fatalf("incomplete %s namespace (%s) knowledge=%v, want Unknown", kind, state, knowledge)
				}
			})
		}
		t.Run(string(kind)+"/ready-empty with sibling namespaces pending", func(t *testing.T) {
			states := map[catalog.MetadataKind]catalog.MetadataState{
				catalog.MetadataRelations:  catalog.MetadataPending,
				catalog.MetadataViews:      catalog.MetadataPending,
				catalog.MetadataProcedures: catalog.MetadataPending,
			}
			states[kind] = catalog.MetadataReady
			view := snapshotSemanticCatalog{snapshot: readySnapshot(states, &catalog.CatalogCache{
				ViewsByID: map[string]*catalog.ViewDesc{}, ProceduresByID: map[string]*catalog.ProcedureDesc{},
			})}
			if _, knowledge := view.RelationInfo(sqlsymbol.Name{Text: "ABSENT"}); knowledge != sqlsymbol.Unknown {
				t.Fatalf("ready-empty %s namespace with pending siblings knowledge=%v, want Unknown", kind, knowledge)
			}
		})
	}
	t.Run("all namespaces ready-empty", func(t *testing.T) {
		states := map[catalog.MetadataKind]catalog.MetadataState{
			catalog.MetadataRelations:  catalog.MetadataReady,
			catalog.MetadataViews:      catalog.MetadataReady,
			catalog.MetadataProcedures: catalog.MetadataReady,
		}
		view := snapshotSemanticCatalog{snapshot: readySnapshot(states, &catalog.CatalogCache{
			ViewsByID: map[string]*catalog.ViewDesc{}, ProceduresByID: map[string]*catalog.ProcedureDesc{},
		})}
		if _, knowledge := view.RelationInfo(sqlsymbol.Name{Text: "ABSENT"}); knowledge != sqlsymbol.Missing {
			t.Fatalf("complete empty relation namespace knowledge=%v, want Missing", knowledge)
		}
	})
	t.Run("known selectable procedure is not missing as a relation", func(t *testing.T) {
		procKey := catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "SEL_PROC"}.Key()
		view := snapshotSemanticCatalog{snapshot: readySnapshot(map[catalog.MetadataKind]catalog.MetadataState{
			catalog.MetadataRelations:  catalog.MetadataReady,
			catalog.MetadataViews:      catalog.MetadataReady,
			catalog.MetadataProcedures: catalog.MetadataReady,
		}, &catalog.CatalogCache{
			ViewsByID:      map[string]*catalog.ViewDesc{},
			ProceduresByID: map[string]*catalog.ProcedureDesc{procKey: {Name: "SEL_PROC"}},
		})}
		if fact, knowledge := view.RelationInfo(sqlsymbol.Name{Text: "SEL_PROC"}); knowledge != sqlsymbol.Unknown {
			t.Fatalf("known selectable procedure relation fact=%+v knowledge=%v, want Unknown (not Missing)", fact, knowledge)
		}
	})
}

func TestSemanticSnapshotQuotedAndFoldedIdentityStayDistinct(t *testing.T) {
	quoted := catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "Orders"}.Key()
	upper := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "ORDERS"}.Key()
	cache := &catalog.Cache{
		Catalog:      &catalog.CatalogCache{ViewsByID: map[string]*catalog.ViewDesc{quoted: {Name: "Orders", Columns: []*catalog.ColumnDesc{{ColumnBase: catalog.ColumnBase{Name: "CustomerId", Table: "Orders"}, Type: "INTEGER", Null: "NO"}}}}},
		ColumnsByID:  map[string][]*catalog.ColumnDesc{upper: {{ColumnBase: catalog.ColumnBase{Name: "ID", Table: "ORDERS"}, Type: "INTEGER", Null: "NO"}}},
		SchemaTables: map[string][]string{"": {"Orders", "ORDERS"}},
		Metadata:     map[catalog.MetadataKind]catalog.MetadataState{catalog.MetadataViews: catalog.MetadataReady, catalog.MetadataColumnsCurrent: catalog.MetadataReady, catalog.MetadataRelations: catalog.MetadataReady, catalog.MetadataProcedures: catalog.MetadataReady},
	}
	view := snapshotSemanticCatalog{snapshot: &catalog.MetadataSnapshot{Cache: cache}}
	quotedFact, quotedKnowledge := view.RelationInfo(sqlsymbol.Name{Text: "Orders", Quoted: true})
	if quotedKnowledge != sqlsymbol.Present || !quotedFact.ColumnsKnown || len(quotedFact.Columns) != 1 || quotedFact.Columns[0].Name != "CustomerId" {
		t.Fatalf("quoted Orders = %+v, %v", quotedFact, quotedKnowledge)
	}
	if _, knowledge := view.RelationInfo(sqlsymbol.Name{Text: "orders"}); knowledge != sqlsymbol.Present {
		t.Fatalf("folded ORDERS knowledge=%v, want Present", knowledge)
	}
	quotedColumns, ok := view.Columns(sqlsymbol.Name{Text: "Orders", Quoted: true})
	if !ok || len(quotedColumns) != 1 || quotedColumns[0].Name != "CustomerId" {
		t.Fatalf("quoted view columns=%v ok=%v, want exact view columns", quotedColumns, ok)
	}
}

func TestSemanticSnapshotStaleAndSystemFactsAreUnknown(t *testing.T) {
	snapshot := readySnapshot(map[catalog.MetadataKind]catalog.MetadataState{catalog.MetadataRelations: catalog.MetadataReady, catalog.MetadataViews: catalog.MetadataReady, catalog.MetadataProcedures: catalog.MetadataReady, catalog.MetadataDomains: catalog.MetadataReady}, &catalog.CatalogCache{})
	view := snapshotSemanticCatalog{snapshot: snapshot}
	if _, got := view.RelationInfo(sqlsymbol.Name{Text: "RDB$RELATIONS"}); got != sqlsymbol.Unknown {
		t.Fatalf("system relation knowledge=%v", got)
	}
	snapshot.Stale = true
	if _, got := view.RelationInfo(sqlsymbol.Name{Text: "ABSENT"}); got != sqlsymbol.Unknown {
		t.Fatalf("stale relation knowledge=%v", got)
	}
}

func TestSemanticSnapshotIncompleteViewOrColumnCategoryWithholdsFacts(t *testing.T) {
	viewID := catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "V"}.Key()
	for _, tc := range []struct {
		name   string
		states map[catalog.MetadataKind]catalog.MetadataState
	}{
		{"views pending", map[catalog.MetadataKind]catalog.MetadataState{catalog.MetadataRelations: catalog.MetadataReady, catalog.MetadataViews: catalog.MetadataLoading, catalog.MetadataProcedures: catalog.MetadataReady, catalog.MetadataColumnsCurrent: catalog.MetadataReady}},
		{"columns failed", map[catalog.MetadataKind]catalog.MetadataState{catalog.MetadataRelations: catalog.MetadataReady, catalog.MetadataViews: catalog.MetadataReady, catalog.MetadataProcedures: catalog.MetadataReady, catalog.MetadataColumnsCurrent: catalog.MetadataFailed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := &catalog.Cache{Catalog: &catalog.CatalogCache{ViewsByID: map[string]*catalog.ViewDesc{}}, Metadata: tc.states}
			name := sqlsymbol.Name{Text: "V"}
			if tc.name == "views pending" {
				cache.Catalog.ViewsByID[viewID] = &catalog.ViewDesc{Name: "V", Columns: []*catalog.ColumnDesc{{ColumnBase: catalog.ColumnBase{Name: "ID", Table: "V"}}}}
				cache.SchemaTables = map[string][]string{"": {"V"}}
				cache.ColumnsByID = map[string][]*catalog.ColumnDesc{catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "V"}.Key(): {{ColumnBase: catalog.ColumnBase{Name: "ID", Table: "V"}}}}
			} else {
				cache.Catalog.ViewsByID = map[string]*catalog.ViewDesc{}
				cache.SchemaTables = map[string][]string{"": {"T"}}
				cache.ColumnsByID = map[string][]*catalog.ColumnDesc{catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}.Key(): {{ColumnBase: catalog.ColumnBase{Name: "ID", Table: "T"}}}}
				name = sqlsymbol.Name{Text: "T"}
			}
			fact, knowledge := (snapshotSemanticCatalog{snapshot: &catalog.MetadataSnapshot{Cache: cache}}).RelationInfo(name)
			if tc.name == "views pending" {
				if knowledge != sqlsymbol.Present || fact.ColumnsKnown {
					t.Fatalf("pending view kind provided table-only proof: fact=%+v knowledge=%v", fact, knowledge)
				}
				return
			}
			if knowledge != sqlsymbol.Present || fact.ColumnsKnown {
				t.Fatalf("failed columns produced fact=%+v knowledge=%v", fact, knowledge)
			}
		})
	}
}

func TestSemanticSnapshotExactEmptyColumnsDoNotBorrowFoldedNeighbor(t *testing.T) {
	exact := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "Orders"}.Key()
	upper := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "ORDERS"}.Key()
	cache := &catalog.Cache{
		Catalog:           &catalog.CatalogCache{ViewsByID: map[string]*catalog.ViewDesc{}},
		ColumnsByID:       map[string][]*catalog.ColumnDesc{exact: {}, upper: {{ColumnBase: catalog.ColumnBase{Name: "ID", Table: "ORDERS"}, Type: "INTEGER"}}},
		ColumnsWithParent: map[string][]*catalog.ColumnDesc{"\tORDERS": {{ColumnBase: catalog.ColumnBase{Name: "ID", Table: "ORDERS"}, Type: "INTEGER"}}},
		SchemaTables:      map[string][]string{"": {"Orders", "ORDERS"}},
		Metadata:          map[catalog.MetadataKind]catalog.MetadataState{catalog.MetadataColumnsCurrent: catalog.MetadataReady, catalog.MetadataRelations: catalog.MetadataReady, catalog.MetadataViews: catalog.MetadataReady, catalog.MetadataProcedures: catalog.MetadataReady},
	}
	view := snapshotSemanticCatalog{snapshot: &catalog.MetadataSnapshot{Cache: cache}}
	columns, ok := view.Columns(sqlsymbol.Name{Text: "Orders", Quoted: true})
	if !ok || len(columns) != 0 {
		t.Fatalf("exact empty quoted columns=%v,%v want empty,true", columns, ok)
	}
	fact, knowledge := view.RelationInfo(sqlsymbol.Name{Text: "Orders", Quoted: true})
	if knowledge != sqlsymbol.Present || !fact.ColumnsKnown || len(fact.Columns) != 0 {
		t.Fatalf("quoted empty relation fact=%+v knowledge=%v", fact, knowledge)
	}
}

func TestSemanticSnapshotViewKindPreventsTableColumnsByIDProof(t *testing.T) {
	viewColumnsID := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "V"}.Key()
	viewID := catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "V"}.Key()
	for _, viewState := range []catalog.MetadataState{catalog.MetadataLoading, catalog.MetadataFailed} {
		t.Run(string(viewState), func(t *testing.T) {
			cache := &catalog.Cache{
				SchemaTables: map[string][]string{"": {"V"}},
				ColumnsByID: map[string][]*catalog.ColumnDesc{
					viewColumnsID: {{ColumnBase: catalog.ColumnBase{Table: "V", Name: "VIEW_COL"}, Type: "INTEGER"}},
				},
				Catalog: &catalog.CatalogCache{ViewsByID: map[string]*catalog.ViewDesc{viewID: {Name: "V", Columns: []*catalog.ColumnDesc{{ColumnBase: catalog.ColumnBase{Table: "V", Name: "VIEW_COL"}, Type: "INTEGER"}}}}},
				Metadata: map[catalog.MetadataKind]catalog.MetadataState{
					catalog.MetadataRelations: catalog.MetadataReady, catalog.MetadataViews: viewState,
					catalog.MetadataColumnsCurrent: catalog.MetadataReady,
				},
			}
			fact, knowledge := (snapshotSemanticCatalog{snapshot: &catalog.MetadataSnapshot{Cache: cache}}).RelationInfo(sqlsymbol.Name{Text: "V"})
			if knowledge == sqlsymbol.Present && fact.ColumnsKnown {
				t.Fatalf("view's table-kind ColumnsByID entry proved base-table columns: %+v", fact)
			}
			if knowledge == sqlsymbol.Missing {
				t.Fatal("listed relation became Missing while view kind was unresolved")
			}
		})
	}
}

func TestSemanticSnapshotQuotedRelationExistenceDoesNotRequireColumns(t *testing.T) {
	cache := &catalog.Cache{
		SchemaTables: map[string][]string{"": {"Orders"}},
		Catalog:      &catalog.CatalogCache{ViewsByID: map[string]*catalog.ViewDesc{}, ProceduresByID: map[string]*catalog.ProcedureDesc{}},
		Metadata:     map[catalog.MetadataKind]catalog.MetadataState{catalog.MetadataRelations: catalog.MetadataReady, catalog.MetadataViews: catalog.MetadataReady, catalog.MetadataProcedures: catalog.MetadataReady},
	}
	fact, knowledge := (snapshotSemanticCatalog{snapshot: &catalog.MetadataSnapshot{Cache: cache}}).RelationInfo(sqlsymbol.Name{Text: "Orders", Quoted: true})
	if knowledge != sqlsymbol.Present || fact.ColumnsKnown {
		t.Fatalf("quoted listed relation without columns = %+v, %v; want Present with unknown columns", fact, knowledge)
	}
}

func TestSemanticSnapshotQuotedLegacyDescriptorsRequireExactSpelling(t *testing.T) {
	legacyView := &catalog.ViewDesc{Name: "Orders", Columns: []*catalog.ColumnDesc{{ColumnBase: catalog.ColumnBase{Name: "ViewColumn", Table: "Orders"}, Type: "INTEGER"}}}
	legacyProc := &catalog.ProcedureDesc{Name: "ProcCase", InputParameters: []*catalog.ProcedureParameterDesc{{Name: "P", Type: "INTEGER"}}}
	legacyDomain := &catalog.DomainDesc{Name: "DomainCase", Type: "INTEGER", Nullable: sql.NullBool{Bool: false, Valid: true}}
	cache := &catalog.Cache{
		Catalog: &catalog.CatalogCache{
			Views:      map[string]*catalog.ViewDesc{"ORDERS": legacyView},
			Procedures: map[string]*catalog.ProcedureDesc{"PROCCASE": legacyProc},
			Domains:    map[string]*catalog.DomainDesc{"DOMAINCASE": legacyDomain},
		},
		SchemaTables: map[string][]string{},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{
			catalog.MetadataRelations: catalog.MetadataReady, catalog.MetadataViews: catalog.MetadataReady,
			catalog.MetadataProcedures: catalog.MetadataReady, catalog.MetadataDomains: catalog.MetadataReady,
			catalog.MetadataColumnsCurrent: catalog.MetadataReady,
		},
	}
	view := snapshotSemanticCatalog{snapshot: &catalog.MetadataSnapshot{Cache: cache}}
	if fact, knowledge := view.RelationInfo(sqlsymbol.Name{Text: "Orders", Quoted: true}); knowledge != sqlsymbol.Present || !fact.ColumnsKnown || fact.Columns[0].Name != "ViewColumn" {
		t.Fatalf("quoted legacy view=%+v knowledge=%v", fact, knowledge)
	}
	if fact, knowledge := view.ProcedureInfo(sqlsymbol.Name{Text: "ProcCase", Quoted: true}); knowledge != sqlsymbol.Present || len(fact.Inputs) != 1 {
		t.Fatalf("quoted legacy procedure=%+v knowledge=%v", fact, knowledge)
	}
	if fact, knowledge := view.DomainInfo(sqlsymbol.Name{Text: "DomainCase", Quoted: true}); knowledge != sqlsymbol.Present || fact.Type != "INTEGER" {
		t.Fatalf("quoted legacy domain=%+v knowledge=%v", fact, knowledge)
	}
	if _, knowledge := view.ProcedureInfo(sqlsymbol.Name{Text: "ProcCaseX", Quoted: true}); knowledge != sqlsymbol.Missing {
		t.Fatalf("complete category with no exact legacy name = %v, want Missing", knowledge)
	}
	if _, knowledge := view.ProcedureInfo(sqlsymbol.Name{Text: "PROCCASE", Quoted: true}); knowledge != sqlsymbol.Unknown {
		t.Fatalf("colliding legacy procedure spelling = %v, want Unknown", knowledge)
	}
	if _, knowledge := view.DomainInfo(sqlsymbol.Name{Text: "DOMAINCASE", Quoted: true}); knowledge != sqlsymbol.Unknown {
		t.Fatalf("colliding legacy domain spelling = %v, want Unknown", knowledge)
	}
	cache.Catalog.Views["ORDERS"] = &catalog.ViewDesc{Name: "ORDERS"}
	if _, knowledge := view.RelationInfo(sqlsymbol.Name{Text: "Orders", Quoted: true}); knowledge == sqlsymbol.Present {
		t.Fatal("folded legacy neighbor falsely proved quoted view identity")
	}
}

func TestSemanticSnapshotViewColumnsUseViewCategoryAlone(t *testing.T) {
	viewID := catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "V"}.Key()
	cache := &catalog.Cache{
		Catalog:  &catalog.CatalogCache{ViewsByID: map[string]*catalog.ViewDesc{viewID: {Name: "V", Columns: []*catalog.ColumnDesc{{ColumnBase: catalog.ColumnBase{Name: "ID", Table: "V"}, Type: "INTEGER"}}}}},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{catalog.MetadataViews: catalog.MetadataReady, catalog.MetadataColumnsCurrent: catalog.MetadataFailed},
	}
	fact, knowledge := (snapshotSemanticCatalog{snapshot: &catalog.MetadataSnapshot{Cache: cache}}).RelationInfo(sqlsymbol.Name{Text: "V"})
	if knowledge != sqlsymbol.Present || !fact.ColumnsKnown || len(fact.Columns) != 1 {
		t.Fatalf("complete view descriptor blocked by unrelated table columns category: %+v %v", fact, knowledge)
	}
}

func TestSemanticSnapshotQuotedLegacyIndexesAndNonUniqueUnknownActive(t *testing.T) {
	key := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "Orders"}.Key()
	nonUnique := &catalog.IndexDesc{Name: "NON_UNIQUE", RelationName: "Orders", Unique: sql.NullBool{Bool: false, Valid: true}}
	unique := &catalog.IndexDesc{Name: "UNIQUE", RelationName: "Orders", Columns: []string{"ID"}, Unique: sql.NullBool{Bool: true, Valid: true}, Active: sql.NullBool{Bool: true, Valid: true}}
	cache := &catalog.Cache{
		SchemaTables: map[string][]string{"": {"Orders"}},
		ColumnsByID:  map[string][]*catalog.ColumnDesc{key: {{ColumnBase: catalog.ColumnBase{Table: "Orders", Name: "ID"}, Type: "INTEGER"}}},
		Catalog: &catalog.CatalogCache{
			Views:          map[string]*catalog.ViewDesc{},
			Indexes:        map[string]*catalog.IndexDesc{"NON_UNIQUE": nonUnique, "UNIQUE": unique},
			IndexesByTable: map[string][]*catalog.IndexDesc{"ORDERS": {nonUnique, unique}},
			IndexesByID:    map[string]*catalog.IndexDesc{},
		},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{
			catalog.MetadataViews: catalog.MetadataReady, catalog.MetadataIndexes: catalog.MetadataReady,
			catalog.MetadataColumnsCurrent: catalog.MetadataReady, catalog.MetadataRelations: catalog.MetadataReady,
		},
	}
	keys, known := (snapshotSemanticCatalog{snapshot: &catalog.MetadataSnapshot{Cache: cache}}).UniqueKeys(sqlsymbol.Name{Text: "Orders", Quoted: true})
	if !known || len(keys) != 1 || len(keys[0]) != 1 || keys[0][0] != "ID" {
		t.Fatalf("legacy quoted indexes yielded keys=%v known=%v; want [[ID]] despite unknown Active on non-unique index", keys, known)
	}
}

func TestSemanticSnapshotColumnReadinessUsesSnapshotStatus(t *testing.T) {
	key := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}.Key()
	cache := &catalog.Cache{
		ColumnsByID:  map[string][]*catalog.ColumnDesc{key: {{ColumnBase: catalog.ColumnBase{Table: "T", Name: "ID"}, Type: "INTEGER"}}},
		Catalog:      &catalog.CatalogCache{ViewsByID: map[string]*catalog.ViewDesc{}},
		SchemaTables: map[string][]string{"": {"T"}},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{
			catalog.MetadataColumnsCurrent: catalog.MetadataReady,
			catalog.MetadataRelations:      catalog.MetadataReady,
			catalog.MetadataViews:          catalog.MetadataReady,
		},
	}
	snapshot := &catalog.MetadataSnapshot{Cache: cache, Status: map[catalog.MetadataKind]catalog.MetadataStatus{catalog.MetadataColumnsCurrent: {State: catalog.MetadataFailed}}}
	if _, ok := (snapshotSemanticCatalog{snapshot: snapshot}).Columns(sqlsymbol.Name{Text: "T"}); ok {
		t.Fatal("cached-ready columns overrode failed snapshot status")
	}
	cache.Metadata[catalog.MetadataColumnsCurrent] = catalog.MetadataFailed
	snapshot.Status[catalog.MetadataColumnsCurrent] = catalog.MetadataStatus{State: catalog.MetadataFailed}
	snapshot.Status[catalog.MetadataColumnsAll] = catalog.MetadataStatus{State: catalog.MetadataReady}
	if _, ok := (snapshotSemanticCatalog{snapshot: snapshot}).Columns(sqlsymbol.Name{Text: "T"}); !ok {
		t.Fatal("ready all-columns status did not satisfy column readiness")
	}
}

func TestSemanticSnapshotUniqueKeysRequireCompleteInputs(t *testing.T) {
	key := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}.Key()
	index := &catalog.IndexDesc{Name: "PK_T", RelationName: "T", Columns: []string{"ID"}, Unique: sql.NullBool{Bool: true, Valid: true}, Active: sql.NullBool{Bool: true, Valid: true}}
	cache := &catalog.Cache{
		SchemaTables: map[string][]string{"": {"T"}},
		ColumnsByID:  map[string][]*catalog.ColumnDesc{key: {{ColumnBase: catalog.ColumnBase{Table: "T", Name: "ID"}, Type: "INTEGER"}}},
		Catalog: &catalog.CatalogCache{
			ViewsByID:      map[string]*catalog.ViewDesc{},
			Indexes:        map[string]*catalog.IndexDesc{"PK_T": index},
			IndexesByTable: map[string][]*catalog.IndexDesc{"T": {index}},
		},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{
			catalog.MetadataRelations: catalog.MetadataReady, catalog.MetadataViews: catalog.MetadataReady,
			catalog.MetadataColumnsCurrent: catalog.MetadataReady, catalog.MetadataIndexes: catalog.MetadataReady,
		},
	}
	snapshot := &catalog.MetadataSnapshot{Cache: cache}
	view := snapshotSemanticCatalog{snapshot: snapshot}
	if _, known := view.UniqueKeys(sqlsymbol.Name{Text: "T"}); !known {
		t.Fatal("complete table/view/column/index metadata did not prove key set")
	}
	snapshot.Status = map[catalog.MetadataKind]catalog.MetadataStatus{catalog.MetadataIndexes: {State: catalog.MetadataFailed}}
	if _, known := view.UniqueKeys(sqlsymbol.Name{Text: "T"}); known {
		t.Fatal("cache-ready index map overrode failed snapshot index status")
	}
	snapshot.Status = map[catalog.MetadataKind]catalog.MetadataStatus{catalog.MetadataColumnsCurrent: {State: catalog.MetadataLoading}}
	if _, known := view.UniqueKeys(sqlsymbol.Name{Text: "T"}); known {
		t.Fatal("cache-ready columns overrode loading snapshot column status")
	}
	snapshot.Status = nil
	cache.Metadata[catalog.MetadataIndexes] = catalog.MetadataReady
	cache.Catalog.Indexes = map[string]*catalog.IndexDesc{}
	cache.Catalog.IndexesByTable = map[string][]*catalog.IndexDesc{}
	keys, known := view.UniqueKeys(sqlsymbol.Name{Text: "T"})
	if !known || len(keys) != 0 {
		t.Fatalf("complete empty unique-key set=%v known=%v; want empty,true", keys, known)
	}
	cache.Metadata[catalog.MetadataColumnsCurrent] = catalog.MetadataFailed
	cache.Metadata[catalog.MetadataColumnsAll] = catalog.MetadataReady
	if _, known := view.UniqueKeys(sqlsymbol.Name{Text: "T"}); !known {
		t.Fatal("ready all-columns category did not satisfy key metadata input")
	}
}

func TestSemanticSnapshotProcedureAndDomainFacts(t *testing.T) {
	procID := catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "P"}.Key()
	domainID := catalog.ObjectID{Kind: catalog.ObjectKindDomain, Name: "D"}.Key()
	cache := &catalog.Cache{
		Catalog: &catalog.CatalogCache{
			ProceduresByID: map[string]*catalog.ProcedureDesc{procID: {Name: "P", InputParameters: []*catalog.ProcedureParameterDesc{{Name: "A", Type: "INTEGER", Nullable: sql.NullBool{Bool: false, Valid: true}}, {Name: "B", Type: "VARCHAR(5)"}}}},
			DomainsByID:    map[string]*catalog.DomainDesc{domainID: {Name: "D", Type: "INTEGER", Nullable: sql.NullBool{Bool: true, Valid: true}}},
		},
		Metadata: map[catalog.MetadataKind]catalog.MetadataState{catalog.MetadataProcedures: catalog.MetadataReady, catalog.MetadataDomains: catalog.MetadataReady},
	}
	view := snapshotSemanticCatalog{snapshot: &catalog.MetadataSnapshot{Cache: cache}}
	fact, knowledge := view.ProcedureInfo(sqlsymbol.Name{Text: "P"})
	if knowledge != sqlsymbol.Present || !fact.InputsKnown || !fact.OutputsKnown || fact.MinInputsKnown || len(fact.Inputs) != 2 || fact.Inputs[0].Nullability != sqlsymbol.NotNullable || fact.Inputs[1].Nullability != sqlsymbol.NullUnknown {
		t.Fatalf("procedure fact=%+v knowledge=%v", fact, knowledge)
	}
	domain, knowledge := view.DomainInfo(sqlsymbol.Name{Text: "D"})
	if knowledge != sqlsymbol.Present || domain.Type != "INTEGER" || domain.Nullability != sqlsymbol.Nullable {
		t.Fatalf("domain fact=%+v knowledge=%v", domain, knowledge)
	}
}
