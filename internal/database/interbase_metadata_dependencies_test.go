package database

import (
	"context"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"interbase-go/schema"
)

func TestInterBaseMetadataDependenciesResolveObjects(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`INSERT INTO "RDB$RELATION_CONSTRAINTS" ("RDB$CONSTRAINT_NAME","RDB$CONSTRAINT_TYPE","RDB$RELATION_NAME") VALUES (?,?,?)`,
		interBaseFixed("CK_AMOUNT"), "CHECK", interBaseFixed("CUSTOMER"))
	exec(`INSERT INTO "RDB$CHECK_CONSTRAINTS" ("RDB$CONSTRAINT_NAME","RDB$TRIGGER_NAME") VALUES (?,?)`,
		interBaseFixed("CK_AMOUNT"), interBaseFixed("CHECK_9"))
	exec(`INSERT INTO "RDB$FIELDS" ("RDB$FIELD_NAME","RDB$COMPUTED_SOURCE","RDB$FIELD_TYPE","RDB$SYSTEM_FLAG") VALUES (?,?,?,?)`,
		interBaseFixed("RDB$900"), "(AMOUNT * 2)", 8, 0)
	exec(`INSERT INTO "RDB$RELATION_FIELDS" ("RDB$FIELD_NAME","RDB$RELATION_NAME","RDB$FIELD_SOURCE","RDB$FIELD_POSITION","RDB$SYSTEM_FLAG") VALUES (?,?,?,?,?)`,
		interBaseFixed("DOUBLED"), interBaseFixed("CUSTOMER"), interBaseFixed("RDB$900"), 99, 0)
	for _, row := range []struct {
		dependent     string
		dependentType int
		field         any
		on            string
		onType        int
	}{
		{"CUSTOMER_VIEW", 1, interBaseFixed("NAME"), "CUSTOMER", 0},
		{"ADD_CUSTOMER", 5, nil, "CUSTOMER_VIEW", 0},
		{"ADD_CUSTOMER", 5, interBaseFixed("NAME"), "CUSTOMER", 0},
		{"ADD_CUSTOMER", 5, interBaseFixed("NAME"), "CUSTOMER", 0},
		{"CHECK_9", 2, interBaseFixed("AMOUNT"), "CUSTOMER", 0},
		{"RDB$900", 3, interBaseFixed("AMOUNT"), "CUSTOMER", 0},
		{"ADD_CUSTOMER", 5, nil, "GEN_X", 11},
		{"ADD_CUSTOMER", 5, nil, "MYSTERY", 99},
	} {
		exec(`INSERT INTO "RDB$DEPENDENCIES" ("RDB$DEPENDENT_NAME","RDB$DEPENDENT_TYPE","RDB$FIELD_NAME","RDB$DEPENDED_ON_NAME","RDB$DEPENDED_ON_TYPE") VALUES (?,?,?,?,?)`,
			interBaseFixed(row.dependent), row.dependentType, row.field, interBaseFixed(row.on), row.onType)
	}
	cache := &DBCache{Catalog: &CatalogCache{ViewsByID: map[string]*ViewDesc{
		interBaseByIDKey(ObjectKindView, "CUSTOMER_VIEW"): {Name: "CUSTOMER_VIEW"},
	}}}
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	patch, err := repo.runMetadataRead(context.Background(), func(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
		return repo.readMetadataDependencies(ctx, q, width, cache)
	})
	if err != nil {
		t.Fatal(err)
	}
	graph := patch.Cache.Dependencies
	if graph.Len() != 7 || patch.Count != 7 {
		t.Fatalf("edges = %d, count = %d, want 7 (one duplicate removed)", graph.Len(), patch.Count)
	}
	customer := catalog.DependencyRef{Kind: catalog.ObjectKindTable, Name: "CUSTOMER"}
	got := map[catalog.DependencyRef]bool{}
	for _, edge := range graph.UsedBy(customer, "AMOUNT") {
		got[edge.From] = true
	}
	for _, want := range []catalog.DependencyRef{
		{Kind: catalog.ObjectKindConstraint, Name: "CK_AMOUNT", Parent: "CUSTOMER"},
		{Kind: catalog.ObjectKindColumn, Name: "DOUBLED", Parent: "CUSTOMER"},
	} {
		if !got[want] {
			t.Errorf("UsedBy(CUSTOMER.AMOUNT) lacks %+v; got %v", want, got)
		}
	}
	viewUsers := graph.UsedBy(catalog.DependencyRef{Kind: catalog.ObjectKindView, Name: "CUSTOMER_VIEW"}, "")
	if len(viewUsers) != 1 || viewUsers[0].From != (catalog.DependencyRef{Kind: catalog.ObjectKindProcedure, Name: "ADD_CUSTOMER"}) {
		t.Errorf("UsedBy(CUSTOMER_VIEW) = %+v", viewUsers)
	}
	kinds := map[catalog.ObjectKind]bool{}
	for _, edge := range graph.Uses(catalog.DependencyRef{Kind: catalog.ObjectKindProcedure, Name: "ADD_CUSTOMER"}) {
		kinds[edge.To.Kind] = true
	}
	for _, want := range []catalog.ObjectKind{catalog.ObjectKindView, catalog.ObjectKindTable, catalog.ObjectKindGenerator, "type 99"} {
		if !kinds[want] {
			t.Errorf("Uses(ADD_CUSTOMER) lacks kind %q; got %v", want, kinds)
		}
	}
	found := false
	for _, name := range graph.CheckConstraints("CUSTOMER") {
		found = found || name == "CK_AMOUNT"
	}
	if !found {
		t.Errorf("CheckConstraints(CUSTOMER) = %v, want CK_AMOUNT", graph.CheckConstraints("CUSTOMER"))
	}
}
