package database

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"interbase-go/schema"
)

func assertWatchedGraphParity(t *testing.T, got, want *catalog.DependencyGraph) {
	t.Helper()
	set := func(edges []catalog.DependencyEdge) map[catalog.DependencyEdge]bool {
		out := map[catalog.DependencyEdge]bool{}
		for _, edge := range edges {
			out[edge] = true
		}
		return out
	}
	if !reflect.DeepEqual(set(got.Edges()), set(want.Edges())) {
		t.Fatal("dependency edge set differs from independent full reader")
	}
	for _, edge := range want.Edges() {
		for _, ref := range []catalog.DependencyRef{edge.From, edge.To} {
			if !reflect.DeepEqual(set(got.Uses(ref)), set(want.Uses(ref))) || !reflect.DeepEqual(set(got.UsedBy(ref, "")), set(want.UsedBy(ref, ""))) || !reflect.DeepEqual(set(got.UsedBy(ref, edge.Field)), set(want.UsedBy(ref, edge.Field))) {
				t.Fatal("outgoing/incident/field lookup differs")
			}
		}
	}
	checks := func(g *catalog.DependencyGraph) map[string][]string {
		m := g.CheckNames()
		for k, v := range m {
			if len(v) == 0 {
				delete(m, k)
			}
		}
		return m
	}
	if !reflect.DeepEqual(checks(got), checks(want)) {
		t.Fatal("CHECK name groups differ")
	}
}

func TestWatchedGraphDependencyOnlyFullClassificationAndImmutableGroups(t *testing.T) {
	fault := &interBaseFixtureFault{}
	db := openInterBaseScalableFixtureWithFault(t, 1, fault)
	insertMetadataProcedureFixtures(t, db, 2)
	for _, query := range []string{
		`INSERT INTO "RDB$RELATIONS" ("RDB$RELATION_NAME","RDB$VIEW_BLR","RDB$VIEW_SOURCE","RDB$SYSTEM_FLAG") VALUES ('WV',X'01','SELECT KEY_A FROM T001',0)`,
		`INSERT INTO "RDB$FIELDS" ("RDB$FIELD_NAME","RDB$FIELD_TYPE","RDB$FIELD_LENGTH","RDB$COMPUTED_SOURCE","RDB$SYSTEM_FLAG") VALUES ('RDB$WC',8,4,'KEY_A + 1',1)`,
		`INSERT INTO "RDB$RELATION_FIELDS" ("RDB$RELATION_NAME","RDB$FIELD_NAME","RDB$FIELD_SOURCE","RDB$FIELD_POSITION") VALUES ('T001','CALC','RDB$WC',6)`,
		`INSERT INTO "RDB$RELATION_CONSTRAINTS" ("RDB$CONSTRAINT_NAME","RDB$CONSTRAINT_TYPE","RDB$RELATION_NAME") VALUES ('WCK','CHECK','T001')`,
		`INSERT INTO "RDB$CHECK_CONSTRAINTS" ("RDB$CONSTRAINT_NAME","RDB$TRIGGER_NAME") VALUES ('WCK','WTR')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	common := []watchedMappingRow{
		{Family: "RDB$RELATIONS", Values: map[string]any{"RDB$RELATION_NAME": "T001"}},
		{Family: "RDB$RELATIONS", Values: map[string]any{"RDB$RELATION_NAME": "WV", "RDB$VIEW_BLR": schema.CatalogBlobIdentity{Low: 1}}},
		{Family: "RDB$FIELDS", Values: map[string]any{"RDB$FIELD_NAME": "RDB$WC", "RDB$SYSTEM_FLAG": int64(1), "RDB$COMPUTED_SOURCE": schema.CatalogBlobIdentity{Low: 2}}},
		{Family: "RDB$RELATION_FIELDS", Values: map[string]any{"RDB$RELATION_NAME": "T001", "RDB$FIELD_NAME": "CALC", "RDB$FIELD_SOURCE": "RDB$WC"}},
		{Family: "RDB$RELATION_CONSTRAINTS", Values: map[string]any{"RDB$CONSTRAINT_NAME": "WCK", "RDB$CONSTRAINT_TYPE": "CHECK   ", "RDB$RELATION_NAME": "T001"}},
		{Family: "RDB$CHECK_CONSTRAINTS", Values: map[string]any{"RDB$CONSTRAINT_NAME": "WCK", "RDB$TRIGGER_NAME": "WTR"}},
	}
	type edgeRow struct {
		name   string
		code   int64
		to     string
		target int64
		field  any
	}
	edges := []edgeRow{{"BULK_PROC_001", 5, "T001", 0, "KEY_A"}, {"BULK_PROC_002", 5, "BULK_PROC_001", 5, nil}, {"WV", 0, "T001", 0, "KEY_A"}, {"WTR", 2, "T001", 0, "KEY_A"}, {"RDB$WC", 3, "T001", 0, "KEY_A"}, {"UNKNOWN", 99, "T001", 0, nil}}
	row := func(e edgeRow) watchedMappingRow {
		return watchedMappingRow{Family: "RDB$DEPENDENCIES", Values: map[string]any{"RDB$DEPENDENT_NAME": e.name, "RDB$DEPENDENT_TYPE": e.code, "RDB$DEPENDED_ON_NAME": e.to, "RDB$DEPENDED_ON_TYPE": e.target, "RDB$FIELD_NAME": e.field}}
	}
	finger := func() *schema.CatalogFingerprint {
		rs := append([]watchedMappingRow(nil), common...)
		for _, edge := range edges {
			rs = append(rs, row(edge))
		}
		return watchedMappingFingerprint(t, rs...)
	}
	for _, edge := range edges {
		if _, err := db.Exec(`INSERT INTO "RDB$DEPENDENCIES" ("RDB$DEPENDENT_NAME","RDB$DEPENDENT_TYPE","RDB$DEPENDED_ON_NAME","RDB$DEPENDED_ON_TYPE","RDB$FIELD_NAME") VALUES (?,?,?,?,?)`, edge.name, edge.code, edge.to, edge.target, edge.field); err != nil {
			t.Fatal(err)
		}
	}
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	base, _, _, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	previous := finger()
	for _, index := range []int{0, 2, 3, 4} {
		t.Run(edges[index].name, func(t *testing.T) {
			oldGraph := base.Dependencies
			oldEdges := oldGraph.Edges()
			oldP2, _ := base.ProcedureByID("BULK_PROC_002")
			edges[index].field = "KEY_B"
			if _, err := db.Exec(`UPDATE "RDB$DEPENDENCIES" SET "RDB$FIELD_NAME"=? WHERE "RDB$DEPENDENT_NAME"=?`, edges[index].field, edges[index].name); err != nil {
				t.Fatal(err)
			}
			candidate := finger()
			reads := 0
			fault.onQuery = func(string) { reads++ }
			delta, err := repo.ReadWatchedDelta(context.Background(), base, previous, candidate)
			if err != nil {
				t.Fatal(err)
			}
			fault.onQuery = nil
			if delta.FullReason != "" || delta.Affected != 0 || len(delta.Patches) != 0 || reads != 0 || delta.Metrics.Queries != 0 {
				t.Fatalf("dependency-only did descriptor SQL: affected=%d reads=%d reason=%s", delta.Affected, reads, delta.FullReason)
			}
			got, err := metadata.MergeObjects(context.Background(), base, delta)
			if err != nil {
				t.Fatal(err)
			}
			want, _, _, err := repo.ReadWatchedFull(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			assertWatchedGraphParity(t, got.Dependencies, want.Dependencies)
			if !reflect.DeepEqual(oldGraph.Edges(), oldEdges) {
				t.Fatal("published graph mutated")
			}
			newP2, _ := got.ProcedureByID("BULK_PROC_002")
			if newP2 != oldP2 {
				t.Fatal("unrelated caller descriptor replaced")
			}
			if len(got.Dependencies.Uses(catalog.DependencyRef{Kind: "type 99", Name: "UNKNOWN"})) != 1 {
				t.Fatal("unchanged unknown code no longer displayable")
			}
			base, previous = got, candidate
		})
	}
}

func TestWatchedGraphPhysicalRenameAndDrop(t *testing.T) {
	db := openInterBaseScalableFixture(t, 1)
	insertMetadataProcedureFixtures(t, db, 2)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	if _, err := db.Exec(`INSERT INTO "RDB$DEPENDENCIES" ("RDB$DEPENDENT_NAME","RDB$DEPENDENT_TYPE","RDB$DEPENDED_ON_NAME","RDB$DEPENDED_ON_TYPE") VALUES ('BULK_PROC_002',5,'BULK_PROC_001',5)`); err != nil {
		t.Fatal(err)
	}
	base, _, _, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	finger := func(name string) *schema.CatalogFingerprint {
		rows := []watchedMappingRow{{Family: "RDB$PROCEDURES", Values: map[string]any{"RDB$PROCEDURE_NAME": "BULK_PROC_002"}}}
		if name != "" {
			rows = append(rows, watchedMappingRow{Family: "RDB$PROCEDURES", Values: map[string]any{"RDB$PROCEDURE_NAME": name}}, watchedMappingRow{Family: "RDB$DEPENDENCIES", Values: map[string]any{"RDB$DEPENDENT_NAME": "BULK_PROC_002", "RDB$DEPENDENT_TYPE": int64(5), "RDB$DEPENDED_ON_NAME": name, "RDB$DEPENDED_ON_TYPE": int64(5)}})
		}
		return watchedMappingFingerprint(t, rows...)
	}
	old := finger("BULK_PROC_001")
	for _, next := range []string{"QuotedProc", ""} {
		if next != "" {
			if _, err := db.Exec(`UPDATE "RDB$PROCEDURES" SET "RDB$PROCEDURE_NAME"=? WHERE "RDB$PROCEDURE_NAME"='BULK_PROC_001'`, next); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE "RDB$DEPENDENCIES" SET "RDB$DEPENDED_ON_NAME"=? WHERE "RDB$DEPENDENT_NAME"='BULK_PROC_002'`, next); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := db.Exec(`DELETE FROM "RDB$PROCEDURES" WHERE "RDB$PROCEDURE_NAME"='QuotedProc'`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`DELETE FROM "RDB$DEPENDENCIES" WHERE "RDB$DEPENDED_ON_NAME"='QuotedProc'`); err != nil {
				t.Fatal(err)
			}
		}
		candidate := finger(next)
		delta, err := repo.ReadWatchedDelta(context.Background(), base, old, candidate)
		if err != nil {
			t.Fatal(err)
		}
		if delta.FullReason != "" || len(delta.Deleted) != 1 {
			t.Fatalf("physical rename/drop lost exact old header: %+v", delta)
		}
		got, err := metadata.MergeObjects(context.Background(), base, delta)
		if err != nil {
			t.Fatal(err)
		}
		want, _, _, err := repo.ReadWatchedFull(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		assertWatchedGraphParity(t, got.Dependencies, want.Dependencies)
		if !reflect.DeepEqual(got.Catalog.ProceduresByID, want.Catalog.ProceduresByID) {
			t.Fatal("rename/drop descriptor parity")
		}
		base, old = got, candidate
	}
}

func TestWatchedGraphPhysicalRelationDropClearsIncidentGroups(t *testing.T) {
	db := openInterBaseScalableFixture(t, 1)
	insertMetadataProcedureFixtures(t, db, 1)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	if _, err := db.Exec(`INSERT INTO "RDB$DEPENDENCIES" ("RDB$DEPENDENT_NAME","RDB$DEPENDENT_TYPE","RDB$DEPENDED_ON_NAME","RDB$DEPENDED_ON_TYPE") VALUES ('BULK_PROC_001',5,'T001',0)`); err != nil {
		t.Fatal(err)
	}
	base, _, _, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	old := watchedMappingFingerprint(t, watchedMappingRow{Family: "RDB$RELATIONS", Values: map[string]any{"RDB$RELATION_NAME": "T001"}}, watchedMappingRow{Family: "RDB$INDICES", Values: map[string]any{"RDB$INDEX_NAME": "IDX_PK_T001", "RDB$RELATION_NAME": "T001"}}, watchedMappingRow{Family: "RDB$DEPENDENCIES", Values: map[string]any{"RDB$DEPENDENT_NAME": "BULK_PROC_001", "RDB$DEPENDENT_TYPE": int64(5), "RDB$DEPENDED_ON_NAME": "T001", "RDB$DEPENDED_ON_TYPE": int64(0)}})
	for _, query := range []string{`DELETE FROM "RDB$RELATIONS" WHERE "RDB$RELATION_NAME"='T001'`, `DELETE FROM "RDB$RELATION_FIELDS" WHERE "RDB$RELATION_NAME"='T001'`, `DELETE FROM "RDB$RELATION_CONSTRAINTS" WHERE "RDB$RELATION_NAME"='T001'`, `DELETE FROM "RDB$INDICES" WHERE "RDB$RELATION_NAME"='T001'`, `DELETE FROM "RDB$INDEX_SEGMENTS" WHERE "RDB$INDEX_NAME"='IDX_PK_T001'`, `DELETE FROM "RDB$DEPENDENCIES" WHERE "RDB$DEPENDED_ON_NAME"='T001'`} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	delta, err := repo.ReadWatchedDelta(context.Background(), base, old, watchedMappingFingerprint(t))
	if err != nil {
		t.Fatal(err)
	}
	got, err := metadata.MergeObjects(context.Background(), base, delta)
	if err != nil {
		t.Fatal(err)
	}
	want, _, _, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertWatchedGraphParity(t, got.Dependencies, want.Dependencies)
	if !slices.Equal(got.SchemaTables[""], want.SchemaTables[""]) {
		t.Fatal("physical relation drop relation-list parity")
	}
	if !reflect.DeepEqual(got.ColumnsByID, want.ColumnsByID) || !reflect.DeepEqual(got.PrimaryKeysByID, want.PrimaryKeysByID) {
		t.Fatal("physical relation drop exact projection parity")
	}
}

func TestWatchedMappingSameEligibilityControls(t *testing.T) {
	for _, flags := range [][2]any{{nil, int64(0)}, {int64(0), nil}, {int64(1), int64(-7)}} {
		rows := func(flag any, source uint32) watchedMappingRow {
			return watchedMappingRow{Family: "RDB$PROCEDURES", Values: map[string]any{"RDB$PROCEDURE_NAME": "P", "RDB$SYSTEM_FLAG": flag, "RDB$PROCEDURE_SOURCE": schema.CatalogBlobIdentity{Low: source}}}
		}
		got, err := mapWatchedChanges(context.Background(), metadata.NewCache(), watchedMappingFingerprint(t, rows(flags[0], 1)), watchedMappingFingerprint(t, rows(flags[1], 2)))
		if err != nil || got.FullReason != "" {
			t.Fatalf("same eligibility overbroad fallback: %v %s", flags, got.FullReason)
		}
		want := 1
		if flags[0] == int64(1) {
			want = 0
		}
		if got.Affected != want {
			t.Fatal("wrong eligible descriptor count")
		}
	}
	for _, flags := range [][2]int64{{0, 1}, {1, 0}} {
		row := func(flag int64, length int64) watchedMappingRow {
			return watchedMappingRow{Family: "RDB$FIELDS", Values: map[string]any{"RDB$FIELD_NAME": "RDB$GENERATED", "RDB$SYSTEM_FLAG": flag, "RDB$FIELD_LENGTH": length}}
		}
		consumer := watchedMappingRow{Family: "RDB$PROCEDURE_PARAMETERS", Values: map[string]any{"RDB$PROCEDURE_NAME": "P", "RDB$PARAMETER_NAME": "A", "RDB$PARAMETER_TYPE": int64(0), "RDB$PARAMETER_NUMBER": int64(0), "RDB$FIELD_SOURCE": "RDB$GENERATED"}}
		got, err := mapWatchedChanges(context.Background(), metadata.NewCache(), watchedMappingFingerprint(t, row(flags[0], 4), consumer), watchedMappingFingerprint(t, row(flags[1], 8), consumer))
		if err != nil || got.FullReason != "" || got.Affected != 1 || len(got.Owners[catalog.MetadataDomains]) != 0 {
			t.Fatalf("excluded support row lost consumer: %+v %v", got, err)
		}
	}
}

func TestWatchedMappingActualExceptionAndDomainPrefixPredicates(t *testing.T) {
	for _, flags := range [][2]int64{{1, 1}, {0, 1}, {1, 0}} {
		row := func(flag int64, message string) watchedMappingRow {
			return watchedMappingRow{Family: "RDB$EXCEPTIONS", Values: map[string]any{"RDB$EXCEPTION_NAME": "E", "RDB$SYSTEM_FLAG": flag, "RDB$MESSAGE": message}}
		}
		got, err := mapWatchedChanges(context.Background(), metadata.NewCache(), watchedMappingFingerprint(t, row(flags[0], "old")), watchedMappingFingerprint(t, row(flags[1], "new")))
		if err != nil || got.FullReason != "" || got.Affected != 1 || len(got.Owners[catalog.MetadataExceptions]) != 1 {
			t.Fatalf("ordinary exception accessor lists all flags: %+v %v", got, err)
		}
	}
	row := func(length int64) watchedMappingRow {
		return watchedMappingRow{Family: "RDB$FIELDS", Values: map[string]any{"RDB$FIELD_NAME": "rdb$Quoted", "RDB$FIELD_LENGTH": length}}
	}
	got, err := mapWatchedChanges(context.Background(), metadata.NewCache(), watchedMappingFingerprint(t, row(4)), watchedMappingFingerprint(t, row(8)))
	if err != nil || got.FullReason != "" || got.Affected != 1 || len(got.Owners[catalog.MetadataDomains]) != 1 {
		t.Fatal("domain prefix rule was folded/approximated")
	}
}

func TestWatchedMappingDependencyMultisetAndUnknownCoverage(t *testing.T) {
	dep := func(code any, field string) watchedMappingRow {
		return watchedMappingRow{Family: "RDB$DEPENDENCIES", Values: map[string]any{"RDB$DEPENDENT_NAME": "P", "RDB$DEPENDENT_TYPE": code, "RDB$DEPENDED_ON_NAME": "T", "RDB$DEPENDED_ON_TYPE": int64(0), "RDB$FIELD_NAME": field}}
	}
	row := dep(int64(5), "C")
	got, err := mapWatchedChanges(context.Background(), metadata.NewCache(), watchedMappingFingerprint(t, row, row), watchedMappingFingerprint(t, row))
	if err != nil || got.FullReason != "" || got.Affected != 0 || len(got.DependencyOwners) != 1 || len(got.dependencyEdges) != 1 {
		t.Fatal("dependency multiset difference lost complete outgoing group")
	}
	for _, code := range []any{nil, int64(99)} {
		got, err = mapWatchedChanges(context.Background(), metadata.NewCache(), watchedMappingFingerprint(t), watchedMappingFingerprint(t, dep(code, "C")))
		if err != nil || !reflect.DeepEqual(got, watchedSelection{FullReason: "mapping"}) {
			t.Fatalf("unknown/NULL native owner guessed: %+v %v", got, err)
		}
	}
}
