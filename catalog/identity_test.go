package catalog_test

import (
	"testing"

	"github.com/sqls-server/sqls/catalog"
)

func TestObjectIdentityPreservesCaseAndNamespace(t *testing.T) {
	a := catalog.ObjectID{Schema: "S", Kind: catalog.ObjectKindTable, Name: "Orders"}
	b := a
	b.Name = "ORDERS"
	c := a
	c.Schema = "OTHER"
	if a.Key() == b.Key() || a.Key() == c.Key() {
		t.Fatal("identity collapsed")
	}
}

func TestObjectIdentityLengthPrefixedComponentsDoNotCollide(t *testing.T) {
	// Naive concatenation of Schema+Name would let Schema="A",Name="BC" collide
	// with Schema="AB",Name="C". Length-prefixing each component defuses that.
	a := catalog.ObjectID{Schema: "A", Kind: catalog.ObjectKindTable, Name: "BC"}
	b := catalog.ObjectID{Schema: "AB", Kind: catalog.ObjectKindTable, Name: "C"}
	if a.Key() == b.Key() {
		t.Fatal("length-prefixed identity collided across a schema/name boundary shift")
	}
}

func TestIndexObjectIdentityIncludesRelationOnlyForSchemaScopedIndexes(t *testing.T) {
	items := catalog.IndexObjectID("mde_fixture", "items", "PRIMARY")
	composite := catalog.IndexObjectID("mde_fixture", "composite_items", "PRIMARY")
	if items == composite || items.Key() == composite.Key() {
		t.Fatal("same-named schema-scoped indexes on different tables collapsed")
	}
	index := catalog.NewObjectIndex(nil)
	index.Insert(items)
	index.Insert(composite)
	if got, ok := index.Lookup(items); !ok || got != items {
		t.Fatalf("Lookup(items PRIMARY) = %+v, %v; want %+v", got, ok, items)
	}
	if got, ok := index.Lookup(composite); !ok || got != composite {
		t.Fatalf("Lookup(composite PRIMARY) = %+v, %v; want %+v", got, ok, composite)
	}
	if got := index.LookupUnquoted(catalog.ObjectKindIndex, "PRIMARY"); len(got) != 2 {
		t.Fatalf("unquoted PRIMARY identities = %#v, want both table-qualified identities", got)
	}

	legacy := catalog.IndexObjectID("", "T", "IDX_T")
	want := catalog.ObjectID{Kind: catalog.ObjectKindIndex, Name: "IDX_T"}
	if legacy != want || legacy.Key() != want.Key() {
		t.Fatalf("schema-less index identity = %+v, key %q; want legacy %+v, key %q", legacy, legacy.Key(), want, want.Key())
	}
}
