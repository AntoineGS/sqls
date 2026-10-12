package catalog

import (
	"reflect"
	"testing"
)

func TestDependencyGraphIndexesBothDirectionsAndDedupes(t *testing.T) {
	table := DependencyRef{Kind: ObjectKindTable, Name: "ITEM"}
	proc := DependencyRef{Kind: ObjectKindProcedure, Name: "P_GET"}
	check := DependencyRef{Kind: ObjectKindConstraint, Name: "CK_QTY", Parent: "ITEM"}
	gen := DependencyRef{Kind: ObjectKindGenerator, Name: "G_ITEM"}
	g := NewDependencyGraph([]DependencyEdge{
		{From: proc, To: table, Field: "CODE"},
		{From: proc, To: table, Field: "CODE"},
		{From: proc, To: table, Field: "PRICE"},
		{From: proc, To: gen},
		{From: check, To: table, Field: "QTY"},
	}, map[string][]string{"ITEM": {"CK_QTY", "CK_A", "CK_QTY"}})
	if g.Len() != 4 {
		t.Fatalf("Len = %d, want 4 (duplicate edge removed)", g.Len())
	}
	if got := g.Uses(proc); len(got) != 3 {
		t.Fatalf("Uses(proc) = %+v", got)
	}
	if got := g.UsedBy(table, ""); len(got) != 3 {
		t.Fatalf("UsedBy(table) = %+v", got)
	}
	if got := g.UsedBy(table, "QTY"); len(got) != 1 || got[0].From != check {
		t.Fatalf("UsedBy(table, QTY) = %+v", got)
	}
	if got := g.UsedBy(gen, ""); len(got) != 1 || got[0].From != proc || got[0].Field != "" {
		t.Fatalf("UsedBy(gen) = %+v", got)
	}
	if got := g.CheckConstraints("ITEM"); !reflect.DeepEqual(got, []string{"CK_A", "CK_QTY"}) {
		t.Fatalf("CheckConstraints = %v", got)
	}
	used := g.Uses(proc)
	used[0] = DependencyEdge{}
	if g.Uses(proc)[0] == (DependencyEdge{}) {
		t.Fatal("Uses returned the graph's own storage")
	}
	var empty *DependencyGraph
	if empty.Len() != 0 || empty.Uses(proc) != nil || empty.UsedBy(table, "") != nil || empty.CheckConstraints("ITEM") != nil {
		t.Fatal("nil graph must answer empty")
	}
}
