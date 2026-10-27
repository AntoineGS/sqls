package database

import (
	"testing"

	"interbase-go/schema"
)

func TestDDLOptionsCarryFirebird(t *testing.T) {
	ib := &InterBaseDBRepository{}
	fb := &InterBaseDBRepository{Firebird: true}
	if ib.ddlOptions(schema.Dialect3).Firebird || !fb.ddlOptions(schema.Dialect3).Firebird {
		t.Fatal("ddlOptions does not carry the Firebird profile")
	}
	if fb.ddlOptions(schema.Dialect1).Dialect != schema.Dialect1 {
		t.Fatal("ddlOptions lost the dialect")
	}
}
