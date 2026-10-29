package metadata

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"testing"

	"github.com/sqls-server/sqls/catalog"
)

func TestMergeObjectsKeepsCaseCollidingSiblingImmutable(t *testing.T) {
	lower := catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "Foo"}
	upper := catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "FOO"}
	old := &catalog.ProcedureDesc{Name: "Foo", Source: sql.NullString{String: "old", Valid: true}}
	sibling := &catalog.ProcedureDesc{Name: "FOO"}
	base := NewCache()
	base.Catalog = &catalog.CatalogCache{ProceduresByID: map[string]*catalog.ProcedureDesc{lower.Key(): old, upper.Key(): sibling}}
	replacement := &catalog.ProcedureDesc{Name: "Foo", Source: sql.NullString{String: "new", Valid: true}}
	delta := ObjectDelta{
		Owners:  map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataProcedures: {lower}},
		Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataProcedures: {Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{ProceduresByID: map[string]*catalog.ProcedureDesc{lower.Key(): replacement}}}}},
	}
	got, err := MergeObjects(context.Background(), base, delta)
	if err != nil {
		t.Fatal(err)
	}
	if got.Catalog.ProceduresByID[upper.Key()] != sibling || old.Source.String != "old" {
		t.Fatal("unchanged sibling or published descriptor mutated")
	}
	if got.Catalog.ProceduresByID[lower.Key()].Source.String != "new" {
		t.Fatal("patch missing")
	}
	if got.Catalog.Procedures["FOO"].Name != "Foo" {
		t.Fatal("bulk collision winner changed")
	}
}

func TestMergeObjectsPreservesTableQualifiedMySQLIndexes(t *testing.T) {
	const schema = "mde_fixture"
	itemsID := catalog.IndexObjectID(schema, "items", "PRIMARY")
	compositeID := catalog.IndexObjectID(schema, "composite_items", "PRIMARY")
	items := &catalog.IndexDesc{Schema: schema, Name: "PRIMARY", RelationName: "items", Columns: []string{"id"}}
	composite := &catalog.IndexDesc{Schema: schema, Name: "PRIMARY", RelationName: "composite_items", Columns: []string{"tenant_id", "item_id"}}
	base := NewCache()
	base.Catalog = &catalog.CatalogCache{
		Indexes:             map[string]*catalog.IndexDesc{"PRIMARY": items},
		IndexesByID:         map[string]*catalog.IndexDesc{itemsID.Key(): items, compositeID.Key(): composite},
		IndexesByTable:      map[string][]*catalog.IndexDesc{"ITEMS": {items}, "COMPOSITE_ITEMS": {composite}},
		ViewsByID:           map[string]*catalog.ViewDesc{},
		ProceduresByID:      map[string]*catalog.ProcedureDesc{},
		GeneratorsByID:      map[string]*catalog.GeneratorDesc{},
		DomainsByID:         map[string]*catalog.DomainDesc{},
		FunctionsByID:       map[string]*catalog.FunctionDesc{},
		TriggersByID:        map[string]*catalog.TriggerDesc{},
		SystemRelationsByID: map[string]*catalog.SystemRelationDesc{},
		ExceptionsByID:      map[string]*catalog.ExceptionDesc{},
	}
	base.Catalog.RebuildObjects(nil)
	replacement := &catalog.IndexDesc{Schema: schema, Name: "PRIMARY", RelationName: "items", Columns: []string{"id", "new_column"}}
	delta := ObjectDelta{
		Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataIndexes: {itemsID}},
		Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataIndexes: {Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{
			Indexes:     map[string]*catalog.IndexDesc{"PRIMARY": replacement},
			IndexesByID: map[string]*catalog.IndexDesc{itemsID.Key(): replacement},
		}}}},
	}
	got, err := MergeObjects(context.Background(), base, delta)
	if err != nil {
		t.Fatal(err)
	}
	if index := got.Catalog.IndexesByID[itemsID.Key()]; index == nil || !reflect.DeepEqual(index.Columns, []string{"id", "new_column"}) {
		t.Fatalf("replaced items PRIMARY = %#v", index)
	}
	if index := got.Catalog.IndexesByID[compositeID.Key()]; index != composite {
		t.Fatalf("unmodified composite PRIMARY = %#v, want original descriptor %#v", index, composite)
	}
	if len(got.Catalog.IndexesByTable["ITEMS"]) != 1 || len(got.Catalog.IndexesByTable["COMPOSITE_ITEMS"]) != 1 {
		t.Fatalf("watch-merged table index groups = %#v", got.Catalog.IndexesByTable)
	}
	if _, ok := got.Catalog.Objects.Lookup(itemsID); !ok {
		t.Fatal("merged object index omitted the items PRIMARY identity")
	}
	if _, ok := got.Catalog.Objects.Lookup(compositeID); !ok {
		t.Fatal("merged object index omitted the composite_items PRIMARY identity")
	}
}

func watchID(kind catalog.ObjectKind, name string) catalog.ObjectID {
	return catalog.ObjectID{Kind: kind, Name: name}
}

func TestWatchParseIDAcceptsTableQualifiedIndexIdentity(t *testing.T) {
	key := watchTableQualifiedIndexKey("mde_fixture", "items", "PRIMARY")
	id, err := watchParseID(key)
	if err != nil {
		t.Fatal(err)
	}
	if id.Key() != key {
		t.Fatalf("parsed index identity key = %q, want %q", id.Key(), key)
	}
}

func watchTableQualifiedIndexKey(schema, table, name string) string {
	components := []string{"", schema, "", string(catalog.ObjectKindIndex), name, table}
	key := ""
	for _, component := range components {
		key += strconv.Itoa(len(component)) + ":" + component
	}
	return key
}

func watchCol(table, name string) *catalog.ColumnDesc {
	return &catalog.ColumnDesc{ColumnBase: catalog.ColumnBase{Table: table, Name: name}, Key: "YES", Facts: &catalog.ColumnTypeFacts{UserDomain: true}}
}
func watchFK(left, right string) *catalog.ForeignKey {
	k := catalog.ForeignKey{{&catalog.ColumnBase{Table: left, Name: "ID"}, &catalog.ColumnBase{Table: right, Name: "ID"}}}
	return &k
}
func watchBase() *catalog.Cache {
	b := NewCache()
	b.SchemaTables[""] = []string{"A", "B", "C", "D", "FOO", "Foo", "V"}
	b.ColumnsByID = map[string][]*catalog.ColumnDesc{watchID(catalog.ObjectKindTable, "A").Key(): {watchCol("A", "X"), watchCol("A", "Y")}, watchID(catalog.ObjectKindTable, "V").Key(): {watchCol("V", "OLD")}, watchID(catalog.ObjectKindTable, "Foo").Key(): {watchCol("Foo", "MIX")}, watchID(catalog.ObjectKindTable, "FOO").Key(): {watchCol("FOO", "UP")}}
	b.PrimaryKeysByID = map[string][]string{watchID(catalog.ObjectKindTable, "A").Key(): {"X", "Y"}}
	b.PrimaryKeyColumns[catalog.ColumnDatabaseKey("", "A")] = map[string]struct{}{"X": {}, "Y": {}}
	for _, cols := range b.ColumnsByID {
		for _, c := range cols {
			key := catalog.ColumnDatabaseKey(c.Schema, c.Table)
			b.ColumnsWithParent[key] = append(b.ColumnsWithParent[key], c)
		}
	}
	for _, cols := range b.ColumnsWithParent {
		sort.SliceStable(cols, func(i, j int) bool { return cols[i].Table < cols[j].Table })
	}
	b.ForeignKeys, _ = catalog.GroupForeignKeys([]*catalog.ForeignKey{watchFK("A", "B"), watchFK("C", "D")})
	// InterBase's folded keys include the schema separator (GroupForeignKeys does not).
	folded := make(map[string]map[string][]*catalog.ForeignKey)
	for a, refs := range b.ForeignKeys {
		folded[catalog.ColumnDatabaseKey("", a)] = make(map[string][]*catalog.ForeignKey)
		for z, keys := range refs {
			folded[catalog.ColumnDatabaseKey("", a)][catalog.ColumnDatabaseKey("", z)] = keys
		}
	}
	b.ForeignKeys = folded
	b.Catalog = &catalog.CatalogCache{
		ViewsByID:      map[string]*catalog.ViewDesc{watchID(catalog.ObjectKindView, "V").Key(): {Name: "V", Columns: []*catalog.ColumnDesc{watchCol("V", "OLD")}}},
		ProceduresByID: map[string]*catalog.ProcedureDesc{watchID(catalog.ObjectKindProcedure, "P").Key(): {Name: "P"}},
		IndexesByID:    map[string]*catalog.IndexDesc{watchID(catalog.ObjectKindIndex, "IA").Key(): {Name: "IA", RelationName: "A", Columns: []string{"X", "Y"}}, watchID(catalog.ObjectKindIndex, "IC").Key(): {Name: "IC", RelationName: "C"}},
		TriggersByID:   map[string]*catalog.TriggerDesc{watchID(catalog.ObjectKindTrigger, "TA").Key(): {Name: "TA", RelationName: sql.NullString{String: "A", Valid: true}}, watchID(catalog.ObjectKindTrigger, "TC").Key(): {Name: "TC", RelationName: sql.NullString{String: "C", Valid: true}}},
	}
	b.Dependencies = catalog.NewDependencyGraph([]catalog.DependencyEdge{{From: catalog.DependencyRef{Kind: catalog.ObjectKindProcedure, Name: "P"}, To: catalog.DependencyRef{Kind: catalog.ObjectKindTable, Name: "A"}}, {From: catalog.DependencyRef{Kind: catalog.ObjectKindProcedure, Name: "Q"}, To: catalog.DependencyRef{Kind: catalog.ObjectKindTable, Name: "C"}}}, map[string][]string{"A": {"CK_A"}, "C": {"CK_C"}})
	b.Metadata[catalog.MetadataPrimaryKeys] = catalog.MetadataReady
	b.Metadata[catalog.MetadataColumnsAll] = catalog.MetadataReady
	b.Metadata[catalog.MetadataProcedures] = catalog.MetadataFailed
	return b
}

func TestMergeObjectsProjections(t *testing.T) {
	a := watchID(catalog.ObjectKindTable, "A")
	v := watchID(catalog.ObjectKindTable, "V")
	cases := []struct {
		name  string
		delta ObjectDelta
		check func(*testing.T, *catalog.Cache)
	}{
		{"delete exact sibling", ObjectDelta{Deleted: []catalog.ObjectID{watchID(catalog.ObjectKindTable, "Foo")}}, func(t *testing.T, g *catalog.Cache) {
			if _, ok := g.ColumnsByID[watchID(catalog.ObjectKindTable, "Foo").Key()]; ok {
				t.Fatal("deleted columns retained")
			}
			if len(g.ColumnsWithParent["\tFOO"]) != 1 || g.ColumnsWithParent["\tFOO"][0].Name != "UP" {
				t.Fatal("sibling columns lost")
			}
			if !reflect.DeepEqual(g.SchemaTables[""], []string{"A", "B", "C", "D", "FOO", "V"}) {
				t.Fatal(g.SchemaTables)
			}
		}},
		{"view columns", ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataColumnsAll: {v}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataColumnsAll: {Cache: &catalog.Cache{ColumnsByID: map[string][]*catalog.ColumnDesc{v.Key(): {watchCol("V", "NEW")}}}}}}, func(t *testing.T, g *catalog.Cache) {
			d := g.Catalog.ViewsByID[watchID(catalog.ObjectKindView, "V").Key()]
			if len(d.Columns) != 1 || d.Columns[0].Name != "NEW" || d.Columns[0].Key != "NO" {
				t.Fatal(d)
			}
		}},
		{"reverse PK", ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataPrimaryKeys: {a}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataPrimaryKeys: {Cache: &catalog.Cache{PrimaryKeysByID: map[string][]string{a.Key(): {"Y", "X"}}}}}}, func(t *testing.T, g *catalog.Cache) {
			if !reflect.DeepEqual(g.PrimaryKeysByID[a.Key()], []string{"Y", "X"}) || g.ColumnsByID[a.Key()][0].Key != "YES" {
				t.Fatal("PK order/enrichment")
			}
		}},
		{"clear PK", ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataPrimaryKeys: {a}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataPrimaryKeys: {Cache: &catalog.Cache{PrimaryKeysByID: map[string][]string{}}}}}, func(t *testing.T, g *catalog.Cache) {
			if len(g.PrimaryKeysByID[a.Key()]) != 0 || g.ColumnsByID[a.Key()][0].Key != "NO" {
				t.Fatal("old PK retained")
			}
		}},
		{"remove FK at both ends", ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataForeignKeys: {a}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataForeignKeys: {Cache: &catalog.Cache{ForeignKeys: map[string]map[string][]*catalog.ForeignKey{}}}}}, func(t *testing.T, g *catalog.Cache) {
			if len(g.ForeignKeys["\tA"]) != 0 || len(g.ForeignKeys["\tB"]) != 0 || len(g.ForeignKeys["\tC"]["\tD"]) != 1 || len(g.ForeignKeys["\tD"]["\tC"]) != 1 {
				t.Fatal(g.ForeignKeys)
			}
		}},
		{"clear trigger group", ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataTriggers: {a}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataTriggers: {Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{TriggersByID: map[string]*catalog.TriggerDesc{}}}}}}, func(t *testing.T, g *catalog.Cache) {
			if len(g.Catalog.TriggersByTable["A"]) != 0 || len(g.Catalog.TriggersByTable["C"]) != 1 {
				t.Fatal(g.Catalog.TriggersByTable)
			}
		}},
		{"delete relation cleanup", ObjectDelta{Deleted: []catalog.ObjectID{a}}, func(t *testing.T, g *catalog.Cache) {
			if len(g.Catalog.IndexesByTable["A"]) != 0 || len(g.Catalog.TriggersByTable["A"]) != 0 || len(g.PrimaryKeysByID[a.Key()]) != 0 || len(g.Dependencies.UsedBy(catalog.DependencyRef{Kind: catalog.ObjectKindTable, Name: "A"}, "")) != 0 || len(g.Dependencies.CheckConstraints("A")) != 0 {
				t.Fatal("dangling projections")
			}
		}},
		{"replace outgoing only", ObjectDelta{DependencyOwners: []catalog.DependencyRef{{Kind: catalog.ObjectKindProcedure, Name: "P"}}, DependencyEdges: []catalog.DependencyEdge{{From: catalog.DependencyRef{Kind: catalog.ObjectKindProcedure, Name: "P"}, To: catalog.DependencyRef{Kind: catalog.ObjectKindTable, Name: "B"}}}, Affected: 0}, func(t *testing.T, g *catalog.Cache) {
			if g.Dependencies.Len() != 2 || len(g.Dependencies.UsedBy(catalog.DependencyRef{Kind: catalog.ObjectKindTable, Name: "A"}, "")) != 0 || len(g.Dependencies.UsedBy(catalog.DependencyRef{Kind: catalog.ObjectKindTable, Name: "B"}, "")) != 1 {
				t.Fatal(g.Dependencies.Edges())
			}
			if g.Metadata[catalog.MetadataProcedures] != catalog.MetadataFailed {
				t.Fatal("failed category recovered implicitly")
			}
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			base := watchBase()
			before := watchBase()
			if !reflect.DeepEqual(base, before) {
				t.Fatalf("fixture itself differs: %v / %v", base.ColumnsWithParent["\tFOO"], before.ColumnsWithParent["\tFOO"])
			}
			g, e := MergeObjects(context.Background(), base, tt.delta)
			if e != nil {
				t.Fatal(e)
			}
			tt.check(t, g)
			if !reflect.DeepEqual(base, before) {
				t.Fatal("published cache mutated")
			}
		})
	}
}

func TestMergeObjectsRejectsUnknown(t *testing.T) {
	p := watchID(catalog.ObjectKindProcedure, "P")
	cases := []ObjectDelta{
		{Owners: map[catalog.MetadataKind][]catalog.ObjectID{"unknown": {p}}},
		{Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataProcedures: {Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{ProceduresByID: map[string]*catalog.ProcedureDesc{p.Key(): {Name: "P"}}}}}}},
		{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataProcedures: {p}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataProcedures: {Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{ProceduresByID: map[string]*catalog.ProcedureDesc{watchID(catalog.ObjectKindProcedure, "Q").Key(): {Name: "Q"}}}}}}},
		{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataProcedures: {p}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataProcedures: {Cache: &catalog.Cache{Metadata: map[catalog.MetadataKind]catalog.MetadataState{catalog.MetadataProcedures: catalog.MetadataFailed}, Catalog: &catalog.CatalogCache{ProceduresByID: map[string]*catalog.ProcedureDesc{}}}}}},
		{DependencyEdges: []catalog.DependencyEdge{{From: catalog.DependencyRef{Kind: catalog.ObjectKindProcedure, Name: "P"}, To: catalog.DependencyRef{Kind: catalog.ObjectKindTable, Name: "A"}}}},
	}
	for i, d := range cases {
		if _, e := MergeObjects(context.Background(), watchBase(), d); !errors.Is(e, ErrWatchIncomplete) {
			t.Fatalf("case %d: %v", i, e)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := MergeObjects(ctx, watchBase(), ObjectDelta{}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestMergeObjectsRejectsIncompleteProjections(t *testing.T) {
	a := watchID(catalog.ObjectKindTable, "A")
	p := watchID(catalog.ObjectKindProcedure, "P")
	cases := []struct {
		name  string
		base  func() *catalog.Cache
		delta ObjectDelta
	}{
		{"PK without exact columns", func() *catalog.Cache { b := watchBase(); b.ColumnsByID = nil; return b }, ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataPrimaryKeys: {a}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataPrimaryKeys: {Cache: &catalog.Cache{PrimaryKeysByID: map[string][]string{a.Key(): {"Y"}}}}}}},
		{"columns without exact PK", func() *catalog.Cache { b := watchBase(); b.PrimaryKeysByID = nil; return b }, ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataColumnsAll: {a}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataColumnsAll: {Cache: &catalog.Cache{ColumnsByID: map[string][]*catalog.ColumnDesc{a.Key(): {watchCol("A", "X")}}}}}}},
		{"empty coverage with column rows", watchBase, ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataColumnsAll: {}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataColumnsAll: {Cache: &catalog.Cache{ColumnsByID: map[string][]*catalog.ColumnDesc{a.Key(): {watchCol("A", "X")}}}}}}},
		{"unknown trigger group row", watchBase, ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataTriggers: {a}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataTriggers: {Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{TriggersByID: map[string]*catalog.TriggerDesc{}, TriggersByTable: map[string][]*catalog.TriggerDesc{"C": {{Name: "TC", RelationName: sql.NullString{String: "C", Valid: true}}}}}}}}}},
		{"procedure unknown parameter", watchBase, ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataProcedures: {p}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataProcedures: {Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{ProceduresByID: map[string]*catalog.ProcedureDesc{p.Key(): {Name: "P", InputParameters: []*catalog.ProcedureParameterDesc{nil}}}}}}}}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := MergeObjects(context.Background(), tt.base(), tt.delta); !errors.Is(err, ErrWatchIncomplete) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestMergeObjectsRemovedTriggerDropsIncidentDependencies(t *testing.T) {
	b := watchBase()
	trigger := catalog.DependencyRef{Kind: catalog.ObjectKindTrigger, Name: "TA"}
	b.Dependencies = catalog.NewDependencyGraph([]catalog.DependencyEdge{{From: trigger, To: catalog.DependencyRef{Kind: catalog.ObjectKindTable, Name: "C"}}, {From: catalog.DependencyRef{Kind: catalog.ObjectKindProcedure, Name: "P"}, To: trigger}}, nil)
	d := ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataTriggers: {watchID(catalog.ObjectKindTable, "A")}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataTriggers: {Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{TriggersByID: map[string]*catalog.TriggerDesc{}}}}}}
	g, e := MergeObjects(context.Background(), b, d)
	if e != nil {
		t.Fatal(e)
	}
	if g.Dependencies.Len() != 0 || b.Dependencies.Len() != 2 {
		t.Fatal("removed trigger retains incident edges or old graph changed")
	}
}

func TestMergeObjectsGraphCopiesAndCheckReplacement(t *testing.T) {
	b := watchBase()
	edges := b.Dependencies.Edges()
	checks := b.Dependencies.CheckNames()
	edges[0].Field = "mutated"
	checks["A"][0] = "mutated"
	delete(checks, "C")
	if b.Dependencies.Edges()[0].Field != "" || b.Dependencies.CheckConstraints("A")[0] != "CK_A" || len(b.Dependencies.CheckConstraints("C")) != 1 {
		t.Fatal("graph exports alias published storage")
	}
	g, e := MergeObjects(context.Background(), b, ObjectDelta{CheckNames: map[string][]string{"A": {}}})
	if e != nil {
		t.Fatal(e)
	}
	if len(g.Dependencies.CheckConstraints("A")) != 0 || len(g.Dependencies.CheckConstraints("C")) != 1 || len(b.Dependencies.CheckConstraints("A")) != 1 {
		t.Fatal("check coverage did not replace independently")
	}
}

func TestMergeObjectsFKExactEndpointsAndCompositePairs(t *testing.T) {
	b := watchBase()
	b.ForeignKeys["\tFOO"] = map[string][]*catalog.ForeignKey{"\tB": {watchFK("FOO", "B"), watchFK("Foo", "B")}}
	incoming := catalog.ForeignKey{{&catalog.ColumnBase{Table: "Foo", Name: "X"}, &catalog.ColumnBase{Table: "D", Name: "DX"}}, {&catalog.ColumnBase{Table: "Foo", Name: "Y"}, &catalog.ColumnBase{Table: "D", Name: "DY"}}}
	owner := watchID(catalog.ObjectKindTable, "Foo")
	d := ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataForeignKeys: {owner}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataForeignKeys: {Cache: &catalog.Cache{ForeignKeys: map[string]map[string][]*catalog.ForeignKey{"\tFOO": {"\tD": {&incoming}}}}}}}
	g, e := MergeObjects(context.Background(), b, d)
	if e != nil {
		t.Fatal(e)
	}
	if len(g.ForeignKeys["\tFOO"]["\tB"]) != 1 || (*g.ForeignKeys["\tFOO"]["\tB"][0])[0][0].Table != "FOO" {
		t.Fatal("case-colliding exact FK lost")
	}
	pairs := *g.ForeignKeys["\tD"]["\tFOO"][0]
	if len(pairs) != 2 || pairs[0][0].Name != "X" || pairs[0][1].Name != "DX" || pairs[1][0].Name != "Y" || pairs[1][1].Name != "DY" {
		t.Fatal("composite pair order/endpoints lost")
	}
	incoming[0][0].Name = "changed"
	if pairs[0][0].Name != "X" {
		t.Fatal("new FK aliases patch")
	}
	incoming[1][1].Table = "OTHER"
	if _, e := MergeObjects(context.Background(), b, d); !errors.Is(e, ErrWatchIncomplete) {
		t.Fatal("inconsistent FK accepted", e)
	}
}

func TestMergeObjectsNewDescriptorGraphsAreDetached(t *testing.T) {
	p := watchID(catalog.ObjectKindProcedure, "Foo")
	q := watchID(catalog.ObjectKindProcedure, "FOO")
	untouched := &catalog.ProcedureDesc{Name: "FOO"}
	replacement := &catalog.ProcedureDesc{Name: "Foo", InputParameters: []*catalog.ProcedureParameterDesc{{Name: "IN", Position: 0, Facts: &catalog.ColumnTypeFacts{UserDomain: true}}}}
	b := NewCache()
	b.Catalog = &catalog.CatalogCache{ProceduresByID: map[string]*catalog.ProcedureDesc{q.Key(): untouched}}
	d := ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataProcedures: {p}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataProcedures: {Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{ProceduresByID: map[string]*catalog.ProcedureDesc{p.Key(): replacement}}}}}}
	g, e := MergeObjects(context.Background(), b, d)
	if e != nil {
		t.Fatal(e)
	}
	replacement.InputParameters[0].Name = "MUTATED"
	replacement.InputParameters[0].Facts.UserDomain = false
	if g.Catalog.ProceduresByID[p.Key()].InputParameters[0].Name != "IN" || !g.Catalog.ProceduresByID[p.Key()].InputParameters[0].Facts.UserDomain || g.Catalog.ProceduresByID[q.Key()] != untouched {
		t.Fatal("descriptor graph alias or unrelated copy")
	}
	h, e := MergeObjects(context.Background(), g, ObjectDelta{Deleted: []catalog.ObjectID{p}})
	if e != nil {
		t.Fatal(e)
	}
	if len(h.Catalog.ProceduresByID) != 1 || h.Catalog.Procedures["FOO"] != untouched || g.Catalog.Procedures["FOO"].Name != "Foo" {
		t.Fatal("exact delete changed sibling/old folded winner")
	}
	if _, ok := h.Catalog.Objects.Lookup(p); ok {
		t.Fatal("dangling object index")
	}
}

func TestMergeObjectsRejectsUnknownCheckOwner(t *testing.T) {
	if _, e := MergeObjects(context.Background(), watchBase(), ObjectDelta{CheckNames: map[string][]string{"UNKNOWN": {"CK"}}}); !errors.Is(e, ErrWatchIncomplete) {
		t.Fatal(e)
	}
}

func TestMergeObjectsPreservesUnknownExactProjection(t *testing.T) {
	b := NewCache()
	b.SchemaTables[""] = []string{"A"}
	b.Metadata[catalog.MetadataPrimaryKeys] = catalog.MetadataReady
	a := watchID(catalog.ObjectKindTable, "A")
	d := ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataColumnsAll: {a}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataColumnsAll: {Cache: &catalog.Cache{ColumnsByID: map[string][]*catalog.ColumnDesc{a.Key(): {watchCol("A", "X")}}}}}}
	if _, e := MergeObjects(context.Background(), b, d); !errors.Is(e, ErrWatchIncomplete) {
		t.Fatal("nil exact PK treated as known empty", e)
	}
}

func TestMergeObjectsPreservesSchemaInObjectIndex(t *testing.T) {
	id := catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Schema: "S", Name: "P"}
	d := ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataProcedures: {id}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataProcedures: {Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{ProceduresByID: map[string]*catalog.ProcedureDesc{id.Key(): {Schema: "S", Name: "P"}}}}}}}
	g, e := MergeObjects(context.Background(), NewCache(), d)
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := g.Catalog.Objects.Lookup(id); !ok {
		t.Fatal("object index discarded schema")
	}
}

func TestMergeObjectsCatalogCategories(t *testing.T) {
	cases := []struct {
		kind    catalog.MetadataKind
		object  catalog.ObjectKind
		name    string
		catalog *catalog.CatalogCache
		check   func(*catalog.Cache) bool
	}{
		{catalog.MetadataGenerators, catalog.ObjectKindGenerator, "G", &catalog.CatalogCache{GeneratorsByID: map[string]*catalog.GeneratorDesc{watchID(catalog.ObjectKindGenerator, "G").Key(): {Name: "G"}}}, func(c *catalog.Cache) bool { return c.Catalog.Generators["G"].Name == "G" }},
		{catalog.MetadataDomains, catalog.ObjectKindDomain, "DOM", &catalog.CatalogCache{DomainsByID: map[string]*catalog.DomainDesc{watchID(catalog.ObjectKindDomain, "DOM").Key(): {Name: "DOM", Facts: &catalog.ColumnTypeFacts{UserDomain: true}}}}, func(c *catalog.Cache) bool { return c.Catalog.Domains["DOM"].Facts.UserDomain }},
		{catalog.MetadataFunctions, catalog.ObjectKindFunction, "F", &catalog.CatalogCache{FunctionsByID: map[string]*catalog.FunctionDesc{watchID(catalog.ObjectKindFunction, "F").Key(): {Name: "F", Arguments: []*catalog.FunctionArgumentDesc{{FunctionName: "F", Type: "INTEGER"}}}}}, func(c *catalog.Cache) bool { return c.Catalog.Functions["F"].Arguments[0].Type == "INTEGER" }},
		{catalog.MetadataIndexes, catalog.ObjectKindIndex, "IA", &catalog.CatalogCache{IndexesByID: map[string]*catalog.IndexDesc{watchID(catalog.ObjectKindIndex, "IA").Key(): {Name: "IA", RelationName: "A", Columns: []string{"Y", "X"}}}}, func(c *catalog.Cache) bool {
			return reflect.DeepEqual(c.Catalog.IndexesByTable["A"][0].Columns, []string{"Y", "X"})
		}},
		{catalog.MetadataSystemRelations, catalog.ObjectKindTable, "SYS", &catalog.CatalogCache{SystemRelationsByID: map[string]*catalog.SystemRelationDesc{watchID(catalog.ObjectKindTable, "SYS").Key(): {Name: "SYS", IsSystem: true}}}, func(c *catalog.Cache) bool {
			return c.Catalog.SystemRelationsByID[watchID(catalog.ObjectKindTable, "SYS").Key()].IsSystem
		}},
		{catalog.MetadataExceptions, catalog.ObjectKindException, "E", &catalog.CatalogCache{ExceptionsByID: map[string]*catalog.ExceptionDesc{watchID(catalog.ObjectKindException, "E").Key(): {Name: "E", Message: sql.NullString{String: "message", Valid: true}}}}, func(c *catalog.Cache) bool {
			return c.Catalog.ExceptionsByID[watchID(catalog.ObjectKindException, "E").Key()].Message.String == "message"
		}},
	}
	for _, tt := range cases {
		t.Run(string(tt.kind), func(t *testing.T) {
			d := ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{tt.kind: {watchID(tt.object, tt.name)}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{tt.kind: {Cache: &catalog.Cache{Catalog: tt.catalog}}}}
			g, e := MergeObjects(context.Background(), watchBase(), d)
			if e != nil {
				t.Fatal(e)
			}
			if !tt.check(g) {
				t.Fatal("category projection missing")
			}
			if _, ok := g.Catalog.Objects.Lookup(watchID(tt.object, tt.name)); !ok {
				t.Fatal("missing object identity")
			}
		})
	}
}

func TestMergeObjectsCannotReplaceUnknownDependencyGraph(t *testing.T) {
	b := watchBase()
	b.Dependencies = nil
	d := ObjectDelta{DependencyOwners: []catalog.DependencyRef{{Kind: catalog.ObjectKindProcedure, Name: "P"}}}
	if _, e := MergeObjects(context.Background(), b, d); !errors.Is(e, ErrWatchIncomplete) {
		t.Fatal("unknown old graph became authoritative", e)
	}
}

func TestMergeObjectsDeletionPreservesUnavailableProjections(t *testing.T) {
	b := NewCache()
	b.ColumnsByID = nil
	g, e := MergeObjects(context.Background(), b, ObjectDelta{Deleted: []catalog.ObjectID{watchID(catalog.ObjectKindTable, "A")}})
	if e != nil {
		t.Fatal(e)
	}
	if g.ColumnsByID != nil || g.PrimaryKeysByID != nil || g.Dependencies != nil {
		t.Fatal("unavailable projection became known empty")
	}
}

func TestMergeObjectsEmptyViewReplacementCleansRelationChildren(t *testing.T) {
	b := watchBase()
	b.Catalog.IndexesByID[watchID(catalog.ObjectKindIndex, "IV").Key()] = &catalog.IndexDesc{Name: "IV", RelationName: "V"}
	b.Catalog.TriggersByID[watchID(catalog.ObjectKindTrigger, "TV").Key()] = &catalog.TriggerDesc{Name: "TV", RelationName: sql.NullString{String: "V", Valid: true}}
	d := ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataViews: {watchID(catalog.ObjectKindView, "V")}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataViews: {Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{ViewsByID: map[string]*catalog.ViewDesc{}}}}}}
	g, e := MergeObjects(context.Background(), b, d)
	if e != nil {
		t.Fatal(e)
	}
	if g.Catalog.IndexesByID[watchID(catalog.ObjectKindIndex, "IV").Key()] != nil || g.Catalog.TriggersByID[watchID(catalog.ObjectKindTrigger, "TV").Key()] != nil || len(g.Catalog.IndexesByTable["V"]) != 0 || len(g.Catalog.TriggersByTable["V"]) != 0 {
		t.Fatal("empty view replacement leaves dangling groups")
	}
}

func TestMergeObjectsDeletedOwnerMayHaveEmptyCoverage(t *testing.T) {
	a := watchID(catalog.ObjectKindTable, "A")
	d := ObjectDelta{Deleted: []catalog.ObjectID{a}, Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataColumnsAll: {a}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataColumnsAll: {Cache: &catalog.Cache{ColumnsByID: map[string][]*catalog.ColumnDesc{}}}}}
	g, e := MergeObjects(context.Background(), watchBase(), d)
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := g.ColumnsByID[a.Key()]; ok {
		t.Fatal("deleted owner retained")
	}
	d.Patches[catalog.MetadataColumnsAll] = catalog.MetadataPatch{Cache: &catalog.Cache{ColumnsByID: map[string][]*catalog.ColumnDesc{a.Key(): {watchCol("A", "X")}}}}
	if _, e := MergeObjects(context.Background(), watchBase(), d); !errors.Is(e, ErrWatchIncomplete) {
		t.Fatal("deleted owner resurrected", e)
	}
}

// fullPKParityCache uses the existing ordinary full-category Merge path, never
// MergeObjects or its enrichment helpers. Input rows are a settled full read.
func fullPKParityCache(t *testing.T, columns map[string][]string, keys map[string][]string, view string) *catalog.Cache {
	t.Helper()
	c := NewCache()
	c.Metadata[catalog.MetadataRelations] = catalog.MetadataReady
	c.Metadata[catalog.MetadataProcedures] = catalog.MetadataFailed
	c.Metadata[catalog.MetadataFunctions] = catalog.MetadataPending
	names := make([]string, 0, len(columns))
	for name := range columns {
		names = append(names, name)
	}
	sort.Strings(names)
	c.SchemaTables[""] = append([]string(nil), names...)
	var rows []*catalog.ColumnDesc
	exact := make(map[string][]*catalog.ColumnDesc)
	for _, name := range names {
		for _, column := range columns[name] {
			desc := watchCol(name, column)
			desc.Key = "" // Ordinary column jobs do not claim PK readiness.
			rows = append(rows, desc)
			exact[watchID(catalog.ObjectKindTable, name).Key()] = append(exact[watchID(catalog.ObjectKindTable, name).Key()], desc)
		}
	}
	var err error
	c, err = Merge(c, catalog.MetadataColumnsAll, catalog.MetadataPatch{Cache: &catalog.Cache{ColumnsByID: exact, ColumnsWithParent: catalog.GenColumnMap(rows)}})
	if err != nil {
		t.Fatal(err)
	}
	foldedKeys := make(map[string]map[string]struct{})
	exactKeys := make(map[string][]string)
	for name, ordered := range keys {
		if len(ordered) == 0 {
			continue
		}
		exactKeys[watchID(catalog.ObjectKindTable, name).Key()] = append([]string(nil), ordered...)
		fold := catalog.ColumnDatabaseKey("", name)
		if foldedKeys[fold] == nil {
			foldedKeys[fold] = make(map[string]struct{})
		}
		for _, column := range ordered {
			foldedKeys[fold][column] = struct{}{}
		}
	}
	c, err = Merge(c, catalog.MetadataPrimaryKeys, catalog.MetadataPatch{Cache: &catalog.Cache{PrimaryKeysByID: exactKeys, PrimaryKeyColumns: foldedKeys}})
	if err != nil {
		t.Fatal(err)
	}
	views := make(map[string]*catalog.ViewDesc)
	viewsByID := make(map[string]*catalog.ViewDesc)
	if view != "" {
		if _, present := columns[view]; present {
			desc := &catalog.ViewDesc{Name: view, Columns: make([]*catalog.ColumnDesc, 0)}
			for _, column := range c.ColumnsByID[watchID(catalog.ObjectKindTable, view).Key()] {
				copy := *column
				copy.Key = "NO" // readMetadataViewsFromColumns' ordinary view policy.
				desc.Columns = append(desc.Columns, &copy)
			}
			views[catalog.CatalogCacheKey(view)] = desc
			viewsByID[watchID(catalog.ObjectKindView, view).Key()] = desc
		}
	}
	c, err = Merge(c, catalog.MetadataViews, catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Views: views, ViewsByID: viewsByID}}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMergeObjectsPKFullProjectionParity(t *testing.T) {
	cases := []struct{ name, side, action, view string }{
		{"clear mixed", "Foo", "clear", ""}, {"clear upper", "FOO", "clear", ""},
		{"add mixed EqualFold", "Foo", "add", ""}, {"add upper EqualFold", "FOO", "add", ""},
		{"replace mixed", "Foo", "replace", ""}, {"replace upper", "FOO", "replace", ""},
		{"drop mixed", "Foo", "drop", ""}, {"drop upper", "FOO", "drop", ""},
		{"reverse ordinary composite", "A", "reverse", ""},
		{"table add affects mixed view bucket", "FOO", "add", "Foo"},
		{"table drop affects upper view bucket", "Foo", "drop", "FOO"},
		{"replace mixed view columns", "Foo", "columns", "Foo"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			columns := map[string][]string{"FOO": {"ID", "id", "ALT"}, "Foo": {"ID", "id", "ALT"}, "A": {"X", "Y"}}
			beforeKeys := map[string][]string{"FOO": {"ID"}, "Foo": {"ID"}, "A": {"X", "Y"}}
			if tt.action == "add" {
				delete(beforeKeys, "FOO")
				delete(beforeKeys, "Foo")
			}
			if tt.action == "drop" || tt.action == "replace" {
				delete(beforeKeys, "FOO")
				delete(beforeKeys, "Foo")
				beforeKeys[tt.side] = []string{"ID"}
			}
			if tt.view != "" {
				delete(beforeKeys, tt.view)
			}
			base := fullPKParityCache(t, columns, beforeKeys, tt.view)
			before := fullPKParityCache(t, columns, beforeKeys, tt.view)
			// ObjectIndex contains a fold function; retain that unchanged immutable
			// index pointer while independently copying all compared descriptors.
			before.Catalog.Objects = base.Catalog.Objects
			afterKeys := make(map[string][]string)
			for name, keys := range beforeKeys {
				afterKeys[name] = append([]string(nil), keys...)
			}
			afterColumns := make(map[string][]string)
			for name, cols := range columns {
				afterColumns[name] = append([]string(nil), cols...)
			}
			id := watchID(catalog.ObjectKindTable, tt.side)
			delta := ObjectDelta{}
			switch tt.action {
			case "drop":
				delete(afterKeys, tt.side)
				delete(afterColumns, tt.side)
				delta.Deleted = []catalog.ObjectID{id}
			case "columns":
				afterColumns[tt.side] = []string{"Id", "ALT"}
				delta.Owners = map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataColumnsAll: {id}}
				delta.Patches = map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataColumnsAll: {Cache: &catalog.Cache{ColumnsByID: map[string][]*catalog.ColumnDesc{id.Key(): {watchCol(tt.side, "Id"), watchCol(tt.side, "ALT")}}}}}
			default:
				switch tt.action {
				case "clear":
					delete(afterKeys, tt.side)
				case "add":
					afterKeys[tt.side] = []string{"id"}
				case "replace":
					afterKeys[tt.side] = []string{"ALT"}
				case "reverse":
					afterKeys[tt.side] = []string{"Y", "X"}
				}
				exact := make(map[string][]string)
				if keys := afterKeys[tt.side]; len(keys) > 0 {
					exact[id.Key()] = append([]string(nil), keys...)
				}
				delta.Owners = map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataPrimaryKeys: {id}}
				delta.Patches = map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataPrimaryKeys: {Cache: &catalog.Cache{PrimaryKeysByID: exact}}}
			}
			want := fullPKParityCache(t, afterColumns, afterKeys, tt.view)
			got, err := MergeObjects(context.Background(), base, delta)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(base, before) {
				t.Fatal("old cache mutated")
			}
			for key, expected := range want.ColumnsByID {
				actual := got.ColumnsByID[key]
				if len(actual) == len(expected) {
					for i, column := range expected {
						if actual[i].Key != column.Key {
							t.Errorf("%s.%s incremental Key=%q; ordinary full Key=%q", column.Table, column.Name, actual[i].Key, column.Key)
						}
					}
				}
				old := base.ColumnsByID[key]
				if reflect.DeepEqual(old, expected) && len(old) > 0 && len(actual) > 0 && actual[0] != old[0] {
					t.Errorf("unchanged exact column list %s was copied", expected[0].Table)
				}
			}
			for name, pair := range map[string][2]any{
				"exact columns": {got.ColumnsByID, want.ColumnsByID}, "legacy columns": {got.ColumnsWithParent, want.ColumnsWithParent},
				"ordered exact keys": {got.PrimaryKeysByID, want.PrimaryKeysByID}, "legacy key union": {got.PrimaryKeyColumns, want.PrimaryKeyColumns},
				"view columns": {got.Catalog.ViewsByID, want.Catalog.ViewsByID}, "folded views": {got.Catalog.Views, want.Catalog.Views},
				"relation membership": {got.SchemaTables, want.SchemaTables}, "category states": {got.Metadata, want.Metadata},
			} {
				if !reflect.DeepEqual(pair[0], pair[1]) {
					t.Errorf("%s differs from independent full Merge: got %#v; want %#v", name, pair[0], pair[1])
				}
			}
			if tt.side != "A" && got.ColumnsByID[watchID(catalog.ObjectKindTable, "A").Key()][0] != base.ColumnsByID[watchID(catalog.ObjectKindTable, "A").Key()][0] {
				t.Fatal("unrelated column descriptor copied")
			}
			if tt.action == "columns" {
				patchColumn := delta.Patches[catalog.MetadataColumnsAll].Cache.ColumnsByID[id.Key()][0]
				if patchColumn.Key != "YES" {
					t.Fatal("input column patch mutated")
				}
				patchColumn.Name = "MUTATED"
				patchColumn.Facts.UserDomain = false
				if got.ColumnsByID[id.Key()][0].Name != "Id" || !got.ColumnsByID[id.Key()][0].Facts.UserDomain || got.Catalog.ViewsByID[watchID(catalog.ObjectKindView, tt.view).Key()].Columns[0].Name != "Id" || !got.Catalog.ViewsByID[watchID(catalog.ObjectKindView, tt.view).Key()].Columns[0].Facts.UserDomain {
					t.Fatal("column/view graph aliases patch")
				}
			}
			if pkPatch, ok := delta.Patches[catalog.MetadataPrimaryKeys]; ok {
				if keys := pkPatch.Cache.PrimaryKeysByID[id.Key()]; len(keys) > 0 {
					if !reflect.DeepEqual(keys, afterKeys[tt.side]) {
						t.Fatal("input key patch mutated")
					}
					keys[0] = "MUTATED"
					if got.PrimaryKeysByID[id.Key()][0] != afterKeys[tt.side][0] {
						t.Fatal("ordered keys alias patch")
					}
				}
			}
		})
	}
}
