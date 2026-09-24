package catalog_test

import (
	"testing"

	"github.com/sqls-server/sqls/catalog"
)

func TestObjectIndexExactLookupIgnoresFoldCollisions(t *testing.T) {
	idx := catalog.NewObjectIndex(nil)
	a := catalog.ObjectID{Schema: "S1", Kind: catalog.ObjectKindDomain, Name: "Foo"}
	b := catalog.ObjectID{Schema: "S2", Kind: catalog.ObjectKindDomain, Name: "FOO"}
	idx.Insert(a)
	idx.Insert(b)

	gotA, ok := idx.Lookup(a)
	if !ok || gotA != a {
		t.Fatalf("Lookup(a) = %+v, %v; want %+v, true", gotA, ok, a)
	}
	gotB, ok := idx.Lookup(b)
	if !ok || gotB != b {
		t.Fatalf("Lookup(b) = %+v, %v; want %+v, true", gotB, ok, b)
	}
}

// TestObjectIndexUnquotedLookupDoesNotCollapseAmbiguity is the required
// regression for Ruling R6: two exact-identity objects that legitimately
// share the same case-folded name in different namespaces must both remain
// independently resolvable by exact ObjectID, and the unquoted-lookup path
// must surface the ambiguity rather than silently picking one.
func TestObjectIndexUnquotedLookupDoesNotCollapseAmbiguity(t *testing.T) {
	idx := catalog.NewObjectIndex(nil)
	a := catalog.ObjectID{Schema: "S1", Kind: catalog.ObjectKindDomain, Name: "Foo"}
	b := catalog.ObjectID{Schema: "S2", Kind: catalog.ObjectKindDomain, Name: "FOO"}
	idx.Insert(a)
	idx.Insert(b)

	matches := idx.LookupUnquoted(catalog.ObjectKindDomain, "foo")
	if len(matches) != 2 {
		t.Fatalf("LookupUnquoted matches = %v, want 2 ambiguous matches", matches)
	}
	found := map[catalog.ObjectID]bool{}
	for _, m := range matches {
		found[m] = true
	}
	if !found[a] || !found[b] {
		t.Fatalf("LookupUnquoted = %v, want both %+v and %+v", matches, a, b)
	}

	// Exact lookup must still resolve each one independently, undisturbed by
	// the ambiguity in the folded index.
	if got, ok := idx.Lookup(a); !ok || got != a {
		t.Fatalf("Lookup(a) after ambiguous insert = %+v, %v; want %+v, true", got, ok, a)
	}
	if got, ok := idx.Lookup(b); !ok || got != b {
		t.Fatalf("Lookup(b) after ambiguous insert = %+v, %v; want %+v, true", got, ok, b)
	}
}

func TestObjectIndexUnquotedLookupUnambiguousSingleMatch(t *testing.T) {
	idx := catalog.NewObjectIndex(nil)
	a := catalog.ObjectID{Schema: "S1", Kind: catalog.ObjectKindTable, Name: "Orders"}
	idx.Insert(a)

	matches := idx.LookupUnquoted(catalog.ObjectKindTable, "ORDERS")
	if len(matches) != 1 || matches[0] != a {
		t.Fatalf("LookupUnquoted = %v, want exactly [%+v]", matches, a)
	}

	if matches := idx.LookupUnquoted(catalog.ObjectKindTable, "NOSUCH"); matches != nil {
		t.Fatalf("LookupUnquoted(missing) = %v, want nil", matches)
	}
}

func TestObjectIndexDifferentKindsDoNotCollide(t *testing.T) {
	idx := catalog.NewObjectIndex(nil)
	table := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "FOO"}
	proc := catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "FOO"}
	idx.Insert(table)
	idx.Insert(proc)

	if matches := idx.LookupUnquoted(catalog.ObjectKindTable, "foo"); len(matches) != 1 || matches[0] != table {
		t.Fatalf("LookupUnquoted(table) = %v, want [%+v]", matches, table)
	}
	if matches := idx.LookupUnquoted(catalog.ObjectKindProcedure, "foo"); len(matches) != 1 || matches[0] != proc {
		t.Fatalf("LookupUnquoted(procedure) = %v, want [%+v]", matches, proc)
	}
}

func TestObjectIndexInsertIsIdempotent(t *testing.T) {
	idx := catalog.NewObjectIndex(nil)
	a := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}
	idx.Insert(a)
	idx.Insert(a)

	if matches := idx.LookupUnquoted(catalog.ObjectKindTable, "T"); len(matches) != 1 {
		t.Fatalf("LookupUnquoted after duplicate insert = %v, want exactly one match", matches)
	}
}
