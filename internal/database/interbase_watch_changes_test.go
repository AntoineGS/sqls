package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"interbase-go/schema"
)

type watchedRecordedQuery struct {
	sql  string
	args []any
}
type watchedRecordingQueryer struct {
	db      *sql.DB
	queries []watchedRecordedQuery
}

func (q *watchedRecordingQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.queries = append(q.queries, watchedRecordedQuery{query, append([]any(nil), args...)})
	return q.db.QueryContext(ctx, query, args...)
}

func TestWatchedChangesProcedureSourceIsolation(t *testing.T) {
	db := openInterBaseScalableFixture(t, 1)
	insertMetadataProcedureFixtures(t, db, 2)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	q := &watchedRecordingQueryer{db: db}
	patch, err := repo.readWatchedCategory(context.Background(), q, 127, catalog.MetadataProcedures, []string{"BULK_PROC_001"})
	if err != nil {
		t.Fatal(err)
	}
	if patch.Count != 1 {
		t.Fatalf("queried unexpected procedures: %+v", patch)
	}
	for _, query := range q.queries {
		if strings.Contains(query.sql, "RDB$PROCEDURE_SOURCE") {
			if !strings.Contains(query.sql, "p.RDB$PROCEDURE_NAME IN (?)") || !reflect.DeepEqual(query.args, []any{"BULK_PROC_001"}) || strings.Contains(query.sql, "BULK_PROC_001") {
				t.Fatalf("unbounded/unparameterized source query: %+v", query)
			}
		}
	}
}

func TestWatchedChangesMissingEligibleHeaderAndExcludedRemoval(t *testing.T) {
	db := openInterBaseScalableFixture(t, 1)
	insertMetadataProcedureFixtures(t, db, 2)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	base, _, _, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id := catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "MISSING"}
	sel := watchedSelection{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataProcedures: {id}}, expected: map[catalog.MetadataKind]map[catalog.ObjectID]bool{catalog.MetadataProcedures: {id: true}}, Affected: 1}
	delta, err := repo.ReadWatchedChanges(context.Background(), base, sel)
	if !errors.Is(err, metadata.ErrWatchIncomplete) || len(delta.Patches) != 0 {
		t.Fatalf("missing eligible header: %+v %v", delta, err)
	}
	id.Name = "BULK_PROC_001"
	sel = watchedSelection{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataProcedures: {id}}, Deleted: []catalog.ObjectID{id}, Affected: 1}
	if _, err := db.Exec(`UPDATE "RDB$PROCEDURES" SET "RDB$SYSTEM_FLAG"=1 WHERE "RDB$PROCEDURE_NAME"=?`, id.Name); err != nil {
		t.Fatal(err)
	}
	delta, err = repo.ReadWatchedChanges(context.Background(), base, sel)
	if err != nil {
		t.Fatal(err)
	}
	got, err := metadata.MergeObjects(context.Background(), base, delta)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.ProcedureByID(id.Name); ok {
		t.Fatal("excluded procedure retained")
	}
	if _, ok := base.ProcedureByID(id.Name); !ok {
		t.Fatal("base mutated")
	}
}

func TestWatchedChangesFKIncomplete(t *testing.T) {
	db := openInterBaseScalableFixture(t, 2)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	q := &watchedRecordingQueryer{db: db}
	patch, err := repo.readWatchedCategory(context.Background(), q, 127, catalog.MetadataForeignKeys, []string{"T001"})
	if err != nil || patch.Count == 0 {
		t.Fatalf("fixture keys: %+v %v", patch, err)
	}
	if _, err := db.Exec(`DELETE FROM "RDB$INDEX_SEGMENTS" WHERE "RDB$INDEX_NAME" LIKE 'IDX_PK_%'`); err != nil {
		t.Fatal(err)
	}
	_, err = repo.readWatchedCategory(context.Background(), q, 127, catalog.MetadataForeignKeys, []string{"T001"})
	if err == nil {
		t.Fatal("incomplete FK silently dropped")
	}
}

func TestWatchedChangesExcludedProcedureRetainsCatalogDependencies(t *testing.T) {
	db := openInterBaseScalableFixture(t, 1)
	insertMetadataProcedureFixtures(t, db, 1)
	if _, err := db.Exec(`INSERT INTO "RDB$DEPENDENCIES" ("RDB$DEPENDENT_NAME","RDB$DEPENDENT_TYPE","RDB$DEPENDED_ON_NAME","RDB$DEPENDED_ON_TYPE","RDB$FIELD_NAME") VALUES ('BULK_PROC_001',5,'T001',0,NULL)`); err != nil {
		t.Fatal(err)
	}
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	base, _, _, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	header := func(flag int64) watchedMappingRow {
		return watchedMappingRow{Family: "RDB$PROCEDURES", Values: map[string]any{"RDB$PROCEDURE_NAME": "BULK_PROC_001", "RDB$SYSTEM_FLAG": flag}}
	}
	dep := watchedMappingRow{Family: "RDB$DEPENDENCIES", Values: map[string]any{"RDB$DEPENDENT_NAME": "BULK_PROC_001", "RDB$DEPENDENT_TYPE": int64(5), "RDB$DEPENDED_ON_NAME": "T001", "RDB$DEPENDED_ON_TYPE": int64(0)}}
	old, new := watchedMappingFingerprint(t, header(0), dep), watchedMappingFingerprint(t, header(1), dep)
	if _, err := db.Exec(`UPDATE "RDB$PROCEDURES" SET "RDB$SYSTEM_FLAG"=1 WHERE "RDB$PROCEDURE_NAME"='BULK_PROC_001'`); err != nil {
		t.Fatal(err)
	}
	delta, err := repo.ReadWatchedDelta(context.Background(), base, old, new)
	if err != nil {
		t.Fatal(err)
	}
	if delta.Metrics.Duration <= 0 || !reflect.DeepEqual(delta, metadata.ObjectDelta{FullReason: "mapping", Metrics: metadata.WatchMetrics{DurationKnown: true, Duration: delta.Metrics.Duration}}) {
		t.Fatalf("persistent exclusion must be full-safe without fragments: %+v", delta)
	}
	got, _, _, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want, _, _, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.ProcedureByID("BULK_PROC_001"); ok {
		t.Fatal("excluded descriptor retained")
	}
	if !reflect.DeepEqual(got.Dependencies.Edges(), want.Dependencies.Edges()) {
		t.Fatal("excluded descriptor's catalog dependency graph differs from full")
	}
	if len(got.Dependencies.Uses(catalog.DependencyRef{Kind: catalog.ObjectKindProcedure, Name: "BULK_PROC_001"})) != 1 {
		t.Fatal("full-safe path erased physically present procedure edge")
	}
	if _, ok := base.ProcedureByID("BULK_PROC_001"); !ok {
		t.Fatal("fallback mutated base")
	}
}

func TestWatchedChangesSystemRelationEligibilityTransfer(t *testing.T) {
	for _, flags := range [][2]int64{{0, 1}, {1, 0}} {
		t.Run(strings.Join([]string{string(rune('0' + flags[0])), string(rune('0' + flags[1]))}, "->"), func(t *testing.T) {
			db := openInterBaseScalableFixture(t, 1)
			repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
			if _, err := db.Exec(`UPDATE "RDB$RELATIONS" SET "RDB$SYSTEM_FLAG"=? WHERE "RDB$RELATION_NAME"='T001'`, flags[0]); err != nil {
				t.Fatal(err)
			}
			base, _, _, err := repo.ReadWatchedFull(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			header := func(flag int64) watchedMappingRow {
				return watchedMappingRow{Family: "RDB$RELATIONS", Values: map[string]any{"RDB$RELATION_NAME": "T001", "RDB$SYSTEM_FLAG": flag}}
			}
			old, new := watchedMappingFingerprint(t, header(flags[0])), watchedMappingFingerprint(t, header(flags[1]))
			if _, err := db.Exec(`UPDATE "RDB$RELATIONS" SET "RDB$SYSTEM_FLAG"=? WHERE "RDB$RELATION_NAME"='T001'`, flags[1]); err != nil {
				t.Fatal(err)
			}
			delta, err := repo.ReadWatchedDelta(context.Background(), base, old, new)
			if err != nil {
				t.Fatal(err)
			}
			if delta.Metrics.Duration <= 0 || !reflect.DeepEqual(delta, metadata.ObjectDelta{FullReason: "mapping", Metrics: metadata.WatchMetrics{DurationKnown: true, Duration: delta.Metrics.Duration}}) {
				t.Fatalf("category transfer must be full-safe without fragments: %+v", delta)
			}
			got, _, _, err := repo.ReadWatchedFull(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want, _, _, err := repo.ReadWatchedFull(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.SchemaTables, want.SchemaTables) || !reflect.DeepEqual(got.Catalog.SystemRelationsByID, want.Catalog.SystemRelationsByID) {
				t.Fatal("eligibility category transfer differs from full")
			}
		})
	}
}

func TestWatchedChangesActual50NameBatches(t *testing.T) {
	for _, size := range []int{50, 51, 100} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			fault := &interBaseFixtureFault{}
			db := openInterBaseScalableFixtureWithFault(t, 1, fault)
			insertMetadataProcedureFixtures(t, db, 100)
			repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
			base, _, _, err := repo.ReadWatchedFull(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var oldRows, newRows []watchedMappingRow
			for i := 1; i <= 100; i++ {
				name := fmt.Sprintf("BULK_PROC_%03d", i)
				old := watchedMappingRow{Family: "RDB$PROCEDURES", Values: map[string]any{"RDB$PROCEDURE_NAME": name, "RDB$PROCEDURE_SOURCE": schema.CatalogBlobIdentity{Low: uint32(i)}}}
				oldRows = append(oldRows, old)
				newValue := uint32(i)
				if i <= size {
					newValue += 100
					if _, err := db.Exec(`UPDATE "RDB$PROCEDURES" SET "RDB$PROCEDURE_SOURCE"='BEGIN EXIT; END' WHERE "RDB$PROCEDURE_NAME"=?`, name); err != nil {
						t.Fatal(err)
					}
				}
				newRows = append(newRows, watchedMappingRow{Family: "RDB$PROCEDURES", Values: map[string]any{"RDB$PROCEDURE_NAME": name, "RDB$PROCEDURE_SOURCE": schema.CatalogBlobIdentity{Low: newValue}}})
			}
			var sourceBatches [][]string
			fault.resetTransactions()
			fault.onQueryArgs = func(query string, args []driver.NamedValue) {
				if strings.Contains(query, "p.RDB$PROCEDURE_SOURCE") {
					if strings.Contains(query, "BULK_PROC_") {
						t.Error("object names concatenated into SQL")
					}
					var names []string
					for _, arg := range args {
						name, ok := arg.Value.(string)
						if !ok {
							t.Error("non-name source bind")
						}
						names = append(names, name)
					}
					sourceBatches = append(sourceBatches, names)
				}
			}
			delta, err := repo.ReadWatchedDelta(context.Background(), base, watchedMappingFingerprint(t, oldRows...), watchedMappingFingerprint(t, newRows...))
			if err != nil {
				t.Fatal(err)
			}
			if delta.FullReason != "" || delta.Affected != size || len(sourceBatches) != (size+49)/50 || delta.Metrics.Queries != int64(3*((size+49)/50)) {
				t.Fatalf("batch/result bound size=%d affected=%d batches=%v queries=%d reason=%s", size, delta.Affected, sourceBatches, delta.Metrics.Queries, delta.FullReason)
			}
			var all []string
			for _, batch := range sourceBatches {
				if len(batch) > 50 || len(batch) == 0 {
					t.Fatal("batch exceeds fixed bound")
				}
				all = append(all, batch...)
			}
			if len(all) != size {
				t.Fatal("duplicate/missing queried names")
			}
			for i, name := range all {
				if name != fmt.Sprintf("BULK_PROC_%03d", i+1) {
					t.Fatal("wrong exact source name")
				}
			}
			options, commits, rollbacks := fault.transactionSnapshot()
			if len(options) != len(sourceBatches) || commits != 0 || rollbacks != len(options) || db.Stats().InUse != 0 {
				t.Fatal("batches did not end serially")
			}
			for _, option := range options {
				if !option.ReadOnly || option.Isolation != driver.IsolationLevel(sql.LevelReadCommitted) {
					t.Fatal("wrong maintenance transaction")
				}
			}
			got, err := metadata.MergeObjects(context.Background(), base, delta)
			if err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 100; i++ {
				name := fmt.Sprintf("BULK_PROC_%03d", i)
				p, ok := got.ProcedureByID(name)
				if !ok {
					t.Fatal("replacement lost header")
				}
				original, _ := base.ProcedureByID(name)
				if i <= size {
					if p.Source.String != "BEGIN EXIT; END" || original.Source.String != "BEGIN END" {
						t.Fatal("replacement/base source mismatch")
					}
				} else if p != original {
					t.Fatal("unrelated immutable descriptor copied")
				}
			}
		})
	}
}

func TestWatchedChangesFKCrossBatchDistinctConstraintDedup(t *testing.T) {
	fault := &interBaseFixtureFault{}
	db := openInterBaseScalableFixtureWithFault(t, 100, fault)
	for _, statement := range []string{
		`INSERT INTO "RDB$INDICES" ("RDB$INDEX_NAME","RDB$RELATION_NAME","RDB$SEGMENT_COUNT","RDB$UNIQUE_FLAG","RDB$INDEX_INACTIVE","RDB$SYSTEM_FLAG") VALUES ('IDX_FK_DUP','T051',2,0,0,0)`,
		`INSERT INTO "RDB$INDEX_SEGMENTS" ("RDB$INDEX_NAME","RDB$FIELD_NAME","RDB$FIELD_POSITION") VALUES ('IDX_FK_DUP','REF_A',0),('IDX_FK_DUP','REF_B',1)`,
		`INSERT INTO "RDB$RELATION_CONSTRAINTS" ("RDB$CONSTRAINT_NAME","RDB$CONSTRAINT_TYPE","RDB$RELATION_NAME","RDB$INDEX_NAME") VALUES ('FK_DUP','FOREIGN KEY','T051','IDX_FK_DUP')`,
		`INSERT INTO "RDB$REF_CONSTRAINTS" ("RDB$CONSTRAINT_NAME","RDB$CONST_NAME_UQ") VALUES ('FK_DUP','PK_T050')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	base, _, _, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []catalog.ObjectID
	for i := 1; i <= 100; i++ {
		ids = append(ids, catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: interBaseScalableRelationName(i)})
	}
	var binds []int
	fault.onQueryArgs = func(query string, args []driver.NamedValue) {
		if strings.Contains(query, "fs.RDB$FIELD_POSITION, fi.RDB$SEGMENT_COUNT") {
			binds = append(binds, len(args))
			if len(args) != 100 {
				t.Error("FK name batches are not 50 logical owners")
			}
			for i := 0; i < len(args)/2; i++ {
				if args[i].Value != args[i+len(args)/2].Value {
					t.Error("FK endpoint filters disagree")
				}
			}
		}
	}
	delta, err := repo.ReadWatchedChanges(context.Background(), base, watchedSelection{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataForeignKeys: ids}, Affected: 100})
	if err != nil {
		t.Fatal(err)
	}
	patch := delta.Patches[catalog.MetadataForeignKeys]
	if patch.Count != 100 || !reflect.DeepEqual(binds, []int{100, 100}) {
		t.Fatalf("cross-batch constraint dedup count=%d binds=%v", patch.Count, binds)
	}
	a, b := columnDatabaseKey("", "T050"), columnDatabaseKey("", "T051")
	left, right := patch.Cache.ForeignKeys[a][b], patch.Cache.ForeignKeys[b][a]
	if len(left) != 2 || len(right) != 2 || left[0] == left[1] {
		t.Fatal("distinct constraints with identical pairs were collapsed")
	}
	for _, pair := range left {
		found := false
		for _, reverse := range right {
			if reverse == pair {
				found = true
			}
		}
		if !found {
			t.Fatal("folded directions lost canonical pointer")
		}
	}
	got, err := metadata.MergeObjects(context.Background(), base, delta)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.ForeignKeys, base.ForeignKeys) {
		t.Fatal("complete FK groups differ from independent full groups")
	}
}

func TestWatchedChangesMissingHeadersAllCategories(t *testing.T) {
	for _, item := range []struct {
		category catalog.MetadataKind
		kind     catalog.ObjectKind
	}{
		{catalog.MetadataRelations, catalog.ObjectKindTable}, {catalog.MetadataViews, catalog.ObjectKindView}, {catalog.MetadataProcedures, catalog.ObjectKindProcedure}, {catalog.MetadataFunctions, catalog.ObjectKindFunction}, {catalog.MetadataDomains, catalog.ObjectKindDomain}, {catalog.MetadataIndexes, catalog.ObjectKindIndex}, {catalog.MetadataTriggers, catalog.ObjectKindTrigger}, {catalog.MetadataGenerators, catalog.ObjectKindGenerator}, {catalog.MetadataExceptions, catalog.ObjectKindException}, {catalog.MetadataSystemRelations, catalog.ObjectKindTable},
	} {
		t.Run(string(item.category), func(t *testing.T) {
			db := openInterBaseScalableFixture(t, 1)
			repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
			base, _, _, err := repo.ReadWatchedFull(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			id := catalog.ObjectID{Kind: item.kind, Name: "MISSING"}
			selection := watchedSelection{Owners: map[catalog.MetadataKind][]catalog.ObjectID{item.category: {id}}, expected: map[catalog.MetadataKind]map[catalog.ObjectID]bool{item.category: {id: true}}, Affected: 1}
			delta, err := repo.ReadWatchedChanges(context.Background(), base, selection)
			if !errors.Is(err, metadata.ErrWatchIncomplete) || len(delta.Patches) != 0 || delta.FullReason != "" {
				t.Fatalf("missing eligible %s header became deletion/full: %+v %v", item.category, delta, err)
			}
		})
	}
}

func TestWatchedChangesUDFArgumentOrderingIncomplete(t *testing.T) {
	for _, value := range []string{"NULL", "-1", "0"} {
		t.Run(value, func(t *testing.T) {
			db := openInterBaseSchemaFixture(t)
			repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
			base, _, _, err := repo.ReadWatchedFull(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			result, err := db.Exec(`UPDATE "RDB$FUNCTION_ARGUMENTS" SET "RDB$ARGUMENT_POSITION"=` + value + ` WHERE "RDB$FUNCTION_NAME"='F_LTRIM'`)
			if err != nil {
				t.Fatal(err)
			}
			if count, _ := result.RowsAffected(); count != 3 {
				t.Fatal("ordering fixture did not mutate three argument rows")
			}
			id := catalog.ObjectID{Kind: catalog.ObjectKindFunction, Name: "F_LTRIM"}
			delta, err := repo.ReadWatchedChanges(context.Background(), base, watchedSelection{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataFunctions: {id}}, Affected: 1})
			if !errors.Is(err, metadata.ErrWatchIncomplete) || len(delta.Patches) != 0 {
				t.Fatalf("bad UDF ordering accepted: %v", err)
			}
		})
	}
}

func TestWatchedChangesPKOrderingIncomplete(t *testing.T) {
	db := openInterBaseScalableFixture(t, 1)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	base, _, _, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE "RDB$INDEX_SEGMENTS" SET "RDB$FIELD_POSITION"=2 WHERE "RDB$INDEX_NAME"='IDX_PK_T001' AND "RDB$FIELD_POSITION"=1`); err != nil {
		t.Fatal(err)
	}
	id := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T001"}
	delta, err := repo.ReadWatchedChanges(context.Background(), base, watchedSelection{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataPrimaryKeys: {id}}, Affected: 1})
	if !errors.Is(err, metadata.ErrWatchIncomplete) || len(delta.Patches) != 0 {
		t.Fatalf("unordered/incomplete PK segments published: %+v %v", delta, err)
	}
}

func TestWatchedChangesUnavailableExactMapsFullSafe(t *testing.T) {
	for _, kind := range []catalog.MetadataKind{catalog.MetadataProcedures, catalog.MetadataFunctions, catalog.MetadataDomains, catalog.MetadataIndexes, catalog.MetadataTriggers, catalog.MetadataGenerators, catalog.MetadataExceptions, catalog.MetadataSystemRelations, catalog.MetadataForeignKeys} {
		t.Run(string(kind), func(t *testing.T) {
			fault := &interBaseFixtureFault{}
			db := openInterBaseScalableFixtureWithFault(t, 1, fault)
			repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
			base, _, _, err := repo.ReadWatchedFull(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case catalog.MetadataProcedures:
				base.Catalog.ProceduresByID = nil
			case catalog.MetadataFunctions:
				base.Catalog.FunctionsByID = nil
			case catalog.MetadataDomains:
				base.Catalog.DomainsByID = nil
			case catalog.MetadataIndexes:
				base.Catalog.IndexesByID = nil
			case catalog.MetadataTriggers:
				base.Catalog.TriggersByID = nil
			case catalog.MetadataGenerators:
				base.Catalog.GeneratorsByID = nil
			case catalog.MetadataExceptions:
				base.Catalog.ExceptionsByID = nil
			case catalog.MetadataSystemRelations:
				base.Catalog.SystemRelationsByID = nil
			case catalog.MetadataForeignKeys:
				base.ForeignKeys = nil
			}
			reads := 0
			fault.onQuery = func(string) { reads++ }
			id := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T001"}
			delta, err := repo.ReadWatchedChanges(context.Background(), base, watchedSelection{Owners: map[catalog.MetadataKind][]catalog.ObjectID{kind: {id}}, Affected: 1})
			if err != nil || !reflect.DeepEqual(delta, metadata.ObjectDelta{FullReason: "mapping"}) || reads != 0 {
				t.Fatalf("unavailable base became authoritative: kind=%s reads=%d reason=%s err=%v", kind, reads, delta.FullReason, err)
			}
		})
	}
}

func TestWatchedChangesCompleteEmptyGroups(t *testing.T) {
	db := openInterBaseScalableFixture(t, 1)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	if _, err := db.Exec(`INSERT INTO "RDB$TRIGGERS" ("RDB$TRIGGER_NAME","RDB$RELATION_NAME","RDB$TRIGGER_TYPE","RDB$TRIGGER_SOURCE","RDB$SYSTEM_FLAG") VALUES ('TR_WATCH','T001',1,'BEGIN EXIT; END',0)`); err != nil {
		t.Fatal(err)
	}
	base, _, _, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	head := func(description any) watchedMappingRow {
		return watchedMappingRow{Family: "RDB$RELATIONS", Values: map[string]any{"RDB$RELATION_NAME": "T001", "RDB$DESCRIPTION": description}}
	}
	old := watchedMappingFingerprint(t, head(nil), watchedMappingRow{Family: "RDB$INDICES", Values: map[string]any{"RDB$INDEX_NAME": "IDX_PK_T001", "RDB$RELATION_NAME": "T001"}}, watchedMappingRow{Family: "RDB$TRIGGERS", Values: map[string]any{"RDB$TRIGGER_NAME": "TR_WATCH", "RDB$RELATION_NAME": "T001"}})
	for _, query := range []string{`DELETE FROM "RDB$INDEX_SEGMENTS" WHERE "RDB$INDEX_NAME"='IDX_PK_T001'`, `DELETE FROM "RDB$RELATION_CONSTRAINTS" WHERE "RDB$RELATION_NAME"='T001'`, `DELETE FROM "RDB$INDICES" WHERE "RDB$RELATION_NAME"='T001'`, `DELETE FROM "RDB$TRIGGERS" WHERE "RDB$RELATION_NAME"='T001'`} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	delta, err := repo.ReadWatchedDelta(context.Background(), base, old, watchedMappingFingerprint(t, head("changed")))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []catalog.MetadataKind{catalog.MetadataPrimaryKeys, catalog.MetadataIndexes, catalog.MetadataTriggers} {
		patch, ok := delta.Patches[kind]
		if !ok || patch.Count != 0 || len(delta.Owners[kind]) == 0 {
			t.Fatalf("empty group lost complete coverage %s", kind)
		}
	}
	got, err := metadata.MergeObjects(context.Background(), base, delta)
	if err != nil {
		t.Fatal(err)
	}
	want, _, _, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.PrimaryKeysByID, want.PrimaryKeysByID) || !reflect.DeepEqual(got.PrimaryKeyColumns, want.PrimaryKeyColumns) || !reflect.DeepEqual(got.Catalog.IndexesByID, want.Catalog.IndexesByID) || !reflect.DeepEqual(got.Catalog.TriggersByID, want.Catalog.TriggersByID) || !reflect.DeepEqual(got.ColumnsByID, want.ColumnsByID) {
		t.Fatal("empty group did not match independent full cache")
	}
	if base.Catalog.TriggersByID[interBaseByIDKey(ObjectKindTrigger, "TR_WATCH")] == nil || len(base.PrimaryKeysByID[interBaseByIDKey(ObjectKindTable, "T001")]) != 2 {
		t.Fatal("empty replacement mutated base")
	}
}

func TestWatchedChangesCandidateChildCountMissingIsIncomplete(t *testing.T) {
	db := openInterBaseScalableFixture(t, 1)
	insertMetadataProcedureFixtures(t, db, 1)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	base, _, _, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	header := watchedMappingRow{Family: "RDB$PROCEDURES", Values: map[string]any{"RDB$PROCEDURE_NAME": "BULK_PROC_001"}}
	parameter := watchedMappingRow{Family: "RDB$PROCEDURE_PARAMETERS", Values: map[string]any{"RDB$PROCEDURE_NAME": "BULK_PROC_001", "RDB$PARAMETER_NAME": "A", "RDB$PARAMETER_TYPE": int64(0), "RDB$PARAMETER_NUMBER": int64(0), "RDB$FIELD_SOURCE": "D_KEY"}}
	delta, err := repo.ReadWatchedDelta(context.Background(), base, watchedMappingFingerprint(t, header), watchedMappingFingerprint(t, header, parameter))
	if !errors.Is(err, metadata.ErrWatchIncomplete) || len(delta.Patches) != 0 {
		t.Fatalf("missing candidate child silently emptied signature: %v", err)
	}
}

func TestWatchedChangesCandidateGroupHeaderMissingIsIncomplete(t *testing.T) {
	db := openInterBaseScalableFixture(t, 1)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	for _, statement := range []string{`INSERT INTO "RDB$INDICES" ("RDB$INDEX_NAME","RDB$RELATION_NAME","RDB$SEGMENT_COUNT","RDB$SYSTEM_FLAG") VALUES ('WATCH_IX','T001',1,0)`, `INSERT INTO "RDB$INDEX_SEGMENTS" ("RDB$INDEX_NAME","RDB$FIELD_NAME","RDB$FIELD_POSITION") VALUES ('WATCH_IX','LABEL',0)`} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	base, _, _, err := repo.ReadWatchedFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	header := func(description any) watchedMappingRow {
		return watchedMappingRow{Family: "RDB$RELATIONS", Values: map[string]any{"RDB$RELATION_NAME": "T001", "RDB$DESCRIPTION": description}}
	}
	index := watchedMappingRow{Family: "RDB$INDICES", Values: map[string]any{"RDB$INDEX_NAME": "WATCH_IX", "RDB$RELATION_NAME": "T001"}}
	old, new := watchedMappingFingerprint(t, header(nil), index), watchedMappingFingerprint(t, header("changed"), index)
	for _, query := range []string{`DELETE FROM "RDB$INDICES" WHERE "RDB$INDEX_NAME"='WATCH_IX'`, `DELETE FROM "RDB$INDEX_SEGMENTS" WHERE "RDB$INDEX_NAME"='WATCH_IX'`} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	delta, err := repo.ReadWatchedDelta(context.Background(), base, old, new)
	if !errors.Is(err, metadata.ErrWatchIncomplete) || len(delta.Patches) != 0 {
		t.Fatalf("missing candidate group member guessed deletion: %v", err)
	}
}

func TestWatchedChangesProcedureAndIndexOrderingIncomplete(t *testing.T) {
	for _, item := range []struct {
		name, mutate string
		category     catalog.MetadataKind
		id           catalog.ObjectID
	}{
		{"parameter-null", `UPDATE "RDB$PROCEDURE_PARAMETERS" SET "RDB$PARAMETER_NUMBER"=NULL WHERE "RDB$PARAMETER_NAME"='B'`, catalog.MetadataProcedures, catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "BULK_PROC_001"}},
		{"parameter-negative", `UPDATE "RDB$PROCEDURE_PARAMETERS" SET "RDB$PARAMETER_NUMBER"=-1 WHERE "RDB$PARAMETER_NAME"='B'`, catalog.MetadataProcedures, catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "BULK_PROC_001"}},
		{"parameter-duplicate", `UPDATE "RDB$PROCEDURE_PARAMETERS" SET "RDB$PARAMETER_NUMBER"=0 WHERE "RDB$PARAMETER_NAME"='B'`, catalog.MetadataProcedures, catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: "BULK_PROC_001"}},
		{"index-gap", `UPDATE "RDB$INDEX_SEGMENTS" SET "RDB$FIELD_POSITION"=2 WHERE "RDB$INDEX_NAME"='IDX_PK_T001' AND "RDB$FIELD_POSITION"=1`, catalog.MetadataIndexes, catalog.ObjectID{Kind: catalog.ObjectKindIndex, Name: "IDX_PK_T001"}},
	} {
		t.Run(item.name, func(t *testing.T) {
			db := openInterBaseScalableFixture(t, 1)
			insertMetadataProcedureFixtures(t, db, 1)
			for i, name := range []string{"A", "B"} {
				if _, err := db.Exec(`INSERT INTO "RDB$PROCEDURE_PARAMETERS" ("RDB$PROCEDURE_NAME","RDB$PARAMETER_NAME","RDB$PARAMETER_TYPE","RDB$PARAMETER_NUMBER","RDB$FIELD_SOURCE") VALUES ('BULK_PROC_001',?,0,?,'D_KEY')`, name, i); err != nil {
					t.Fatal(err)
				}
			}
			repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
			base, _, _, err := repo.ReadWatchedFull(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(item.mutate); err != nil {
				t.Fatal(err)
			}
			delta, err := repo.ReadWatchedChanges(context.Background(), base, watchedSelection{Owners: map[catalog.MetadataKind][]catalog.ObjectID{item.category: {item.id}}, Affected: 1})
			if !errors.Is(err, metadata.ErrWatchIncomplete) || len(delta.Patches) != 0 || delta.FullReason != "" {
				t.Fatal("corrupt ordering became successful removal/full fallback")
			}
		})
	}
}

func TestWatchedChangesLaterBatchFailureAndCancellationNeverFull(t *testing.T) {
	for _, cancelRead := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRead), func(t *testing.T) {
			fault := &interBaseFixtureFault{}
			db := openInterBaseScalableFixtureWithFault(t, 1, fault)
			insertMetadataProcedureFixtures(t, db, 51)
			repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
			base, _, _, err := repo.ReadWatchedFull(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := errors.New("injected second batch failure")
			reads := 0
			fault.resetTransactions()
			fault.onQuery = func(query string) {
				if strings.Contains(query, "p.RDB$PROCEDURE_SOURCE") {
					reads++
					if reads == 2 {
						if cancelRead {
							cancel()
						} else {
							fault.match = "p.RDB$PROCEDURE_SOURCE"
							fault.stage = "query"
							fault.err = injected
						}
					}
				}
			}
			var ids []catalog.ObjectID
			for i := 1; i <= 51; i++ {
				ids = append(ids, catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: fmt.Sprintf("BULK_PROC_%03d", i)})
			}
			delta, err := repo.ReadWatchedChanges(ctx, base, watchedSelection{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataProcedures: ids}, Affected: 51})
			if !errors.Is(err, metadata.ErrWatchIncomplete) || len(delta.Patches) != 0 || delta.FullReason != "" {
				t.Fatalf("later failure published partial/full: %+v %v", delta, err)
			}
			if cancelRead && !errors.Is(err, context.Canceled) || !cancelRead && !errors.Is(err, injected) {
				t.Fatal("failure identity lost")
			}
			options, commits, rollbacks := fault.transactionSnapshot()
			if len(options) != 2 || commits != 0 || rollbacks != 2 || db.Stats().InUse != 0 {
				t.Fatal("failed batches did not drain")
			}
		})
	}
}
