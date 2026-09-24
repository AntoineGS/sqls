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
