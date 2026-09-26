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
	for _, tc := range []struct {
		name   string
		states map[catalog.MetadataKind]catalog.MetadataState
		want   sqlsymbol.Knowledge
	}{
		{"views pending", map[catalog.MetadataKind]catalog.MetadataState{catalog.MetadataRelations: catalog.MetadataReady, catalog.MetadataViews: catalog.MetadataLoading, catalog.MetadataProcedures: catalog.MetadataReady}, sqlsymbol.Unknown},
		{"procedures failed", map[catalog.MetadataKind]catalog.MetadataState{catalog.MetadataRelations: catalog.MetadataReady, catalog.MetadataViews: catalog.MetadataReady, catalog.MetadataProcedures: catalog.MetadataFailed}, sqlsymbol.Unknown},
		{"all namespaces complete and empty", map[catalog.MetadataKind]catalog.MetadataState{catalog.MetadataRelations: catalog.MetadataReady, catalog.MetadataViews: catalog.MetadataReady, catalog.MetadataProcedures: catalog.MetadataReady}, sqlsymbol.Missing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, knowledge := (snapshotSemanticCatalog{snapshot: readySnapshot(tc.states, &catalog.CatalogCache{ViewsByID: map[string]*catalog.ViewDesc{}, ProceduresByID: map[string]*catalog.ProcedureDesc{}})}).RelationInfo(sqlsymbol.Name{Text: "ABSENT"})
			if knowledge != tc.want {
				t.Fatalf("RelationInfo knowledge=%v want %v (fact %+v)", knowledge, tc.want, got)
			}
		})
	}
}

func TestSemanticSnapshotQuotedAndFoldedIdentityStayDistinct(t *testing.T) {
	quoted := catalog.ObjectID{Kind: catalog.ObjectKindView, Name: "Orders"}.Key()
	upper := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "ORDERS"}.Key()
	cache := &catalog.Cache{
		Catalog:     &catalog.CatalogCache{ViewsByID: map[string]*catalog.ViewDesc{quoted: {Name: "Orders", Columns: []*catalog.ColumnDesc{{ColumnBase: catalog.ColumnBase{Name: "CustomerId", Table: "Orders"}, Type: "INTEGER", Null: "NO"}}}}},
		ColumnsByID: map[string][]*catalog.ColumnDesc{upper: {{ColumnBase: catalog.ColumnBase{Name: "ID", Table: "ORDERS"}, Type: "INTEGER", Null: "NO"}}},
		Metadata:    map[catalog.MetadataKind]catalog.MetadataState{catalog.MetadataViews: catalog.MetadataReady, catalog.MetadataColumnsCurrent: catalog.MetadataReady, catalog.MetadataRelations: catalog.MetadataReady, catalog.MetadataProcedures: catalog.MetadataReady},
	}
	view := snapshotSemanticCatalog{snapshot: &catalog.MetadataSnapshot{Cache: cache}}
	quotedFact, quotedKnowledge := view.RelationInfo(sqlsymbol.Name{Text: "Orders", Quoted: true})
	if quotedKnowledge != sqlsymbol.Present || !quotedFact.ColumnsKnown || len(quotedFact.Columns) != 1 || quotedFact.Columns[0].Name != "CustomerId" {
		t.Fatalf("quoted Orders = %+v, %v", quotedFact, quotedKnowledge)
	}
	if _, knowledge := view.RelationInfo(sqlsymbol.Name{Text: "orders"}); knowledge != sqlsymbol.Present {
		t.Fatalf("folded ORDERS knowledge=%v, want Present", knowledge)
	}
	if _, ok := view.Columns(sqlsymbol.Name{Text: "Orders", Quoted: true}); ok {
		t.Fatal("quoted Orders borrowed ORDERS columns from folded identity")
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
			cache := &catalog.Cache{Catalog: &catalog.CatalogCache{ViewsByID: map[string]*catalog.ViewDesc{viewID: {Name: "V", Columns: []*catalog.ColumnDesc{{ColumnBase: catalog.ColumnBase{Name: "ID", Table: "V"}}}}}}, Metadata: tc.states}
			fact, knowledge := (snapshotSemanticCatalog{snapshot: &catalog.MetadataSnapshot{Cache: cache}}).RelationInfo(sqlsymbol.Name{Text: "V"})
			if tc.name == "views pending" {
				if knowledge != sqlsymbol.Unknown {
					t.Fatalf("pending view knowledge=%v", knowledge)
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
		ColumnsByID:       map[string][]*catalog.ColumnDesc{exact: {}, upper: {{ColumnBase: catalog.ColumnBase{Name: "ID", Table: "ORDERS"}, Type: "INTEGER"}}},
		ColumnsWithParent: map[string][]*catalog.ColumnDesc{"\tORDERS": {{ColumnBase: catalog.ColumnBase{Name: "ID", Table: "ORDERS"}, Type: "INTEGER"}}},
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
