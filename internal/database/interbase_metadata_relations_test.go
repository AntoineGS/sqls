package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"interbase-go/schema"
)

func TestInterBaseMetadataCoreReadersReturnIndependentFragments(t *testing.T) {
	db := openInterBaseScalableFixture(t, 2)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	tests := []struct {
		name string
		read interBaseMetadataRead
		want int
	}{
		{"relations", repo.readMetadataRelations, 2},
		{"columns", repo.readMetadataColumns, 10},
		{"primary keys", repo.readMetadataPrimaryKeys, 2},
		{"foreign keys", repo.readMetadataForeignKeys, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count := interBaseFixtureCountPrepares(t)
			patch, err := repo.runMetadataRead(ctx, tt.read)
			if err != nil {
				t.Fatal(err)
			}
			if patch.Count != tt.want {
				t.Fatalf("Count = %d, want %d", patch.Count, tt.want)
			}
			if got := count(); got != 2 {
				t.Fatalf("prepared queries = %d, want one width and one data query", got)
			}
		})
	}
}

func TestInterBaseMetadataCoreReaderQueryCountDoesNotGrowWithRelations(t *testing.T) {
	for _, size := range []int{1, 100} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			db := openInterBaseScalableFixture(t, size)
			repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
			for name, read := range map[string]interBaseMetadataRead{
				"relations":    repo.readMetadataRelations,
				"columns":      repo.readMetadataColumns,
				"primary keys": repo.readMetadataPrimaryKeys,
				"foreign keys": repo.readMetadataForeignKeys,
			} {
				count := interBaseFixtureCountPrepares(t)
				if _, err := repo.runMetadataRead(context.Background(), read); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if got := count(); got != 2 {
					t.Errorf("%s prepared queries = %d, want two", name, got)
				}
			}
		})
	}
}

func TestInterBaseMetadataCoreReadersMatchLegacySnapshot(t *testing.T) {
	db := openInterBaseScalableFixture(t, 3)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	loader := metadata.NewLoader(nil)
	loader.Reset(1)
	if _, err := loader.Start(ctx, 1, metadataPlanFor(&interBaseMetadataFailureRepository{InterBaseDBRepository: repo})); err != nil {
		t.Fatal(err)
	}
	if err := loader.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	got := loader.Snapshot()
	for _, kind := range []MetadataKind{MetadataRelations, MetadataColumnsCurrent, MetadataPrimaryKeys, MetadataForeignKeys} {
		if got.Status[kind].State != MetadataReady {
			t.Fatalf("core job %s state = %s, want ready", kind, got.Status[kind].State)
		}
	}

	legacy, err := NewDBCacheUpdater(repo).GenerateDBCachePrimary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Cache.SchemaTables, legacy.SchemaTables) {
		t.Errorf("SchemaTables differ\ncore:   %#v\nlegacy: %#v", got.Cache.SchemaTables, legacy.SchemaTables)
	}
	if !reflect.DeepEqual(got.Cache.ColumnsWithParent, legacy.ColumnsWithParent) {
		t.Errorf("ColumnsWithParent differ\ncore:   %#v\nlegacy: %#v", got.Cache.ColumnsWithParent, legacy.ColumnsWithParent)
	}
	if want := legacyPrimaryKeyColumns(legacy.ColumnsWithParent); !reflect.DeepEqual(got.Cache.PrimaryKeyColumns, want) {
		t.Errorf("PrimaryKeyColumns differ\ncore:   %#v\nlegacy: %#v", got.Cache.PrimaryKeyColumns, want)
	}
	if core, old := canonicalForeignKeyMappings(got.Cache.ForeignKeys), canonicalForeignKeyMappings(legacy.ForeignKeys); !reflect.DeepEqual(core, old) {
		t.Errorf("ForeignKeys differ\ncore:   %#v\nlegacy: %#v", core, old)
	}
}

func legacyPrimaryKeyColumns(columns map[string][]*ColumnDesc) map[string]map[string]struct{} {
	keys := make(map[string]map[string]struct{})
	for _, descriptors := range columns {
		for _, descriptor := range descriptors {
			if descriptor == nil || descriptor.Key != "YES" {
				continue
			}
			table := columnDatabaseKey("", descriptor.Table)
			if keys[table] == nil {
				keys[table] = make(map[string]struct{})
			}
			keys[table][descriptor.Name] = struct{}{}
		}
	}
	return keys
}

func canonicalForeignKeyMappings(foreignKeys map[string]map[string][]*ForeignKey) map[string]map[string][]string {
	canonical := make(map[string]map[string][]string)
	for table, references := range foreignKeys {
		for referencedTable, keys := range references {
			for _, foreignKey := range keys {
				if foreignKey == nil || len(*foreignKey) == 0 {
					continue
				}
				first := (*foreignKey)[0]
				left, right := columnDatabaseKey("", first[0].Table), columnDatabaseKey("", first[1].Table)
				cacheTable := table
				if strings.HasPrefix(cacheTable, "\t") {
					cacheTable = strings.TrimPrefix(cacheTable, "\t")
				}
				cacheReferenced := referencedTable
				if strings.HasPrefix(cacheReferenced, "\t") {
					cacheReferenced = strings.TrimPrefix(cacheReferenced, "\t")
				}
				if columnDatabaseKey("", cacheTable) != left || columnDatabaseKey("", cacheReferenced) != right {
					continue // each cache stores the mapping in both directions
				}
				mapping := strings.Builder{}
				for _, pair := range *foreignKey {
					fmt.Fprintf(&mapping, "%s.%s -> %s.%s;", pair[0].Table, pair[0].Name, pair[1].Table, pair[1].Name)
				}
				if canonical[left] == nil {
					canonical[left] = make(map[string][]string)
				}
				canonical[left][right] = append(canonical[left][right], mapping.String())
			}
		}
	}
	for _, references := range canonical {
		for table := range references {
			sort.Strings(references[table])
		}
	}
	return canonical
}

func TestInterBaseMetadataColumnsDoNotRequireRelationsAndPKCanMergeEitherOrder(t *testing.T) {
	db := openInterBaseScalableFixture(t, 1)
	if _, err := db.Exec(`INSERT INTO "RDB$RELATIONS" ("RDB$RELATION_NAME","RDB$RELATION_ID","RDB$SYSTEM_FLAG") VALUES (?,?,?)`, "EMPTY_RELATION", 9010, 0); err != nil {
		t.Fatal(err)
	}
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()
	relations, err := repo.runMetadataRead(ctx, repo.readMetadataRelations)
	if err != nil {
		t.Fatal(err)
	}
	foundEmpty := false
	for _, relation := range relations.Cache.SchemaTables[""] {
		foundEmpty = foundEmpty || relation == "EMPTY_RELATION"
	}
	if !foundEmpty {
		t.Fatal("relation with no columns was lost")
	}
	columns, err := repo.runMetadataRead(ctx, repo.readMetadataColumns)
	if err != nil {
		t.Fatal(err)
	}
	key := columnDatabaseKey("", "T001")
	if len(columns.Cache.ColumnsWithParent[key]) != 4 {
		t.Fatalf("columns = %#v", columns.Cache.ColumnsWithParent)
	}
	for _, column := range columns.Cache.ColumnsWithParent[key] {
		if column.Key != "" {
			t.Errorf("column %q Key = %q before PK metadata, want empty", column.Name, column.Key)
		}
	}
	primary, err := repo.runMetadataRead(ctx, repo.readMetadataPrimaryKeys)
	if err != nil {
		t.Fatal(err)
	}
	if got := primary.Cache.PrimaryKeyColumns[key]; len(got) != 2 {
		t.Fatalf("primary key fields = %#v", got)
	}

	for _, pkFirst := range []bool{false, true} {
		base := newMetadataCache()
		var merged *DBCache
		if pkFirst {
			merged, err = mergeMetadata(base, MetadataPrimaryKeys, primary)
			if err == nil {
				merged, err = mergeMetadata(merged, MetadataColumnsCurrent, columns)
			}
		} else {
			merged, err = mergeMetadata(base, MetadataColumnsCurrent, columns)
			if err == nil {
				merged, err = mergeMetadata(merged, MetadataPrimaryKeys, primary)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, column := range merged.ColumnsWithParent[key] {
			want := "NO"
			if column.Name == "KEY_A" || column.Name == "KEY_B" {
				want = "YES"
			}
			if column.Key != want {
				t.Errorf("pkFirst=%v column %s Key=%q, want %q", pkFirst, column.Name, column.Key, want)
			}
		}
	}
}

// TestInterBaseMetadataPrimaryKeysProduceExactOrderedProjection proves
// readMetadataPrimaryKeys's PrimaryKeysByID projection (see
// catalog.Cache.PrimaryKeysByID) preserves each relation's composite key in
// its exact segment order, and that two case-distinct relations ("Foo" and
// "FOO", which fold to the same legacy PrimaryKeyColumns key) are kept as
// separate exact identities rather than merged.
func TestInterBaseMetadataPrimaryKeysProduceExactOrderedProjection(t *testing.T) {
	db := openInterBaseScalableFixture(t, 1)
	insertInterBaseCaseDistinctPrimaryKeyFixtures(t, db)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	count := interBaseFixtureCountPrepares(t)
	patch, err := repo.runMetadataRead(ctx, repo.readMetadataPrimaryKeys)
	if err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 2 {
		t.Fatalf("prepared queries = %d, want one width and one primary key data query", got)
	}

	t001 := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T001"}.Key()
	mixed := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "Foo"}.Key()
	upper := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "FOO"}.Key()

	if !reflect.DeepEqual(patch.Cache.PrimaryKeysByID[t001], []string{"KEY_A", "KEY_B"}) {
		t.Fatalf("T001 primary keys = %#v, want [KEY_A KEY_B]", patch.Cache.PrimaryKeysByID[t001])
	}
	if !reflect.DeepEqual(patch.Cache.PrimaryKeysByID[mixed], []string{"B", "A"}) {
		t.Fatalf("Foo primary keys = %#v, want [B A]", patch.Cache.PrimaryKeysByID[mixed])
	}
	if !reflect.DeepEqual(patch.Cache.PrimaryKeysByID[upper], []string{"ID"}) {
		t.Fatalf("FOO primary keys = %#v, want [ID]", patch.Cache.PrimaryKeysByID[upper])
	}
}

// insertInterBaseCaseDistinctPrimaryKeyFixtures adds two relations whose
// names differ only by case, "Foo" and "FOO", each with its own composite or
// single-column primary key. "Foo"'s key segments are inserted out of
// column-name order (B before A) so a reader trusting insertion order
// instead of RDB$FIELD_POSITION would fail.
func insertInterBaseCaseDistinctPrimaryKeyFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	insert := func(statement string, args ...any) {
		t.Helper()
		if _, err := db.Exec(statement, args...); err != nil {
			t.Fatalf("insert case-distinct fixture row: %v: %s", err, statement)
		}
	}
	relation := func(name string, id int) {
		insert(`INSERT INTO "RDB$RELATIONS" ("RDB$RELATION_NAME","RDB$RELATION_ID","RDB$OWNER_NAME","RDB$SYSTEM_FLAG") VALUES (?,?,?,?)`,
			name, id, interBaseFixed("SYSDBA"), 0)
	}
	field := func(relationName, name string, position int) {
		insert(`INSERT INTO "RDB$RELATION_FIELDS" ("RDB$FIELD_NAME","RDB$RELATION_NAME","RDB$FIELD_SOURCE","RDB$FIELD_POSITION","RDB$SYSTEM_FLAG","RDB$NULL_FLAG") VALUES (?,?,?,?,?,?)`,
			interBaseFixed(name), relationName, "D_KEY", position, 0, 1)
	}
	index := func(name, relationName string, id, segmentCount int) {
		insert(`INSERT INTO "RDB$INDICES" ("RDB$INDEX_NAME","RDB$RELATION_NAME","RDB$INDEX_ID","RDB$UNIQUE_FLAG","RDB$SEGMENT_COUNT","RDB$INDEX_INACTIVE","RDB$INDEX_TYPE","RDB$SYSTEM_FLAG") VALUES (?,?,?,?,?,?,?,?)`,
			name, relationName, id, 1, segmentCount, 0, 0, 0)
	}
	segment := func(indexName, fieldName string, position int) {
		insert(`INSERT INTO "RDB$INDEX_SEGMENTS" ("RDB$INDEX_NAME","RDB$FIELD_NAME","RDB$FIELD_POSITION") VALUES (?,?,?)`,
			indexName, interBaseFixed(fieldName), position)
	}
	constraint := func(name, relationName, indexName string) {
		insert(`INSERT INTO "RDB$RELATION_CONSTRAINTS" ("RDB$CONSTRAINT_NAME","RDB$CONSTRAINT_TYPE","RDB$RELATION_NAME","RDB$INDEX_NAME") VALUES (?,?,?,?)`,
			name, "PRIMARY KEY", relationName, indexName)
	}

	relation("Foo", 500)
	field("Foo", "B", 0)
	field("Foo", "A", 1)
	index("IDX_PK_Foo", "Foo", 5000, 2)
	segment("IDX_PK_Foo", "B", 0)
	segment("IDX_PK_Foo", "A", 1)
	constraint("PK_Foo", "Foo", "IDX_PK_Foo")

	relation("FOO", 501)
	field("FOO", "ID", 0)
	index("IDX_PK_FOO", "FOO", 5001, 1)
	segment("IDX_PK_FOO", "ID", 0)
	constraint("PK_FOO", "FOO", "IDX_PK_FOO")
}

// TestInterBaseMetadataViewsFromColumnsKeyDeterministicRegardlessOfAdmissionOrder
// is the required regression for the view-column Key determinism bug: since
// readMetadataViewsFromColumns (the actual MetadataPlan() Views Run function
// since the views-reuse-columns fix) reads each view's columns from the
// ColumnsByID cache snapshot captured at the moment the Views job is
// admitted, and that snapshot's Key field depends on whether
// MetadataPrimaryKeys has already merged, the published Key must not be
// allowed to vary with that race. It forces BOTH admission orders through
// the real Loader/MetadataPlan and the fixture's gate/entered mechanism
// (not a direct mergeMetadata unit call) so a regression at the actual
// scheduling/admission level, not just in the pure merge function, is
// caught.
func TestInterBaseMetadataViewsFromColumnsKeyDeterministicRegardlessOfAdmissionOrder(t *testing.T) {
	const columnsQueryMatch = "WHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0\nORDER BY rf.RDB$RELATION_NAME"
	const primaryKeysQueryMatch = "pk.RDB$CONSTRAINT_TYPE = 'PRIMARY KEY'"

	t.Run("primary keys merge before views admitted", func(t *testing.T) {
		release := make(chan struct{})
		entered := make(chan struct{}, 1)
		fault := &interBaseFixtureFault{match: columnsQueryMatch, stage: "gate", gate: release, entered: entered}
		db := openInterBaseScalableFixtureWithFault(t, 1, fault)
		insertMetadataViewFixtures(t, db, 2)

		loader := metadata.NewLoader(nil)
		t.Cleanup(loader.Stop)
		loader.Reset(1)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		load, err := loader.Start(ctx, 1, metadataPlanFor(&InterBaseDBRepository{Conn: db}))
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-entered:
		case <-ctx.Done():
			close(release)
			t.Fatalf("columns query did not gate: %v", ctx.Err())
		}
		// PrimaryKeys has no dependency on Columns; with Columns held open,
		// it is free to complete and merge first.
		waitForInterBaseMetadataStatus(t, ctx, loader, MetadataPrimaryKeys, MetadataReady)
		close(release)
		waitLoad(t, load)
		assertInterBaseViewColumnsKeyIsNO(t, loader.Snapshot())
	})

	t.Run("views run before primary keys merge", func(t *testing.T) {
		release := make(chan struct{})
		entered := make(chan struct{}, 1)
		fault := &interBaseFixtureFault{match: primaryKeysQueryMatch, stage: "gate", gate: release, entered: entered}
		db := openInterBaseScalableFixtureWithFault(t, 1, fault)
		insertMetadataViewFixtures(t, db, 2)

		loader := metadata.NewLoader(nil)
		t.Cleanup(loader.Stop)
		loader.Reset(1)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		load, err := loader.Start(ctx, 1, metadataPlanFor(&InterBaseDBRepository{Conn: db}))
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-entered:
		case <-ctx.Done():
			close(release)
			t.Fatalf("primary keys query did not gate: %v", ctx.Err())
		}
		// Views depends only on ColumnsCurrent, not PrimaryKeys, so with
		// PrimaryKeys held open, Views is free to be admitted and complete
		// while the shared cache still has no PK-enriched Key data at all.
		waitForInterBaseMetadataStatus(t, ctx, loader, MetadataViews, MetadataReady)
		close(release)
		waitLoad(t, load)
		assertInterBaseViewColumnsKeyIsNO(t, loader.Snapshot())
	})
}

// waitForInterBaseMetadataStatus blocks until kind reaches want in loader's
// current snapshot, using the loader's own change callback rather than
// polling, or until ctx is done.
func waitForInterBaseMetadataStatus(t *testing.T, ctx context.Context, loader *metadata.Loader, kind MetadataKind, want MetadataState) {
	t.Helper()
	changed := make(chan struct{}, 1)
	loader.SetChangedCallback(func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	defer loader.SetChangedCallback(nil)
	for {
		if loader.Snapshot().Status[kind].State == want {
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("%s did not reach %s: %v (status=%+v)", kind, want, ctx.Err(), loader.Snapshot().Status[kind])
		}
	}
}

// assertInterBaseViewColumnsKeyIsNO asserts every published view column's
// Key is the deterministic constant "NO" (matching the old standalone
// viewDescriptions path), regardless of whether MetadataPrimaryKeys had
// already merged by the time MetadataViews was admitted.
func assertInterBaseViewColumnsKeyIsNO(t *testing.T, snapshot *MetadataSnapshot) {
	t.Helper()
	if snapshot.Status[MetadataViews].State != MetadataReady {
		t.Fatalf("views state = %s, want ready", snapshot.Status[MetadataViews].State)
	}
	found := false
	for _, view := range snapshot.Cache.Catalog.Views {
		for _, column := range view.Columns {
			found = true
			if column.Key != "NO" {
				t.Errorf("view %s column %s Key = %q, want \"NO\"", view.Name, column.Name, column.Key)
			}
		}
	}
	if !found {
		t.Fatal("no view columns were published to check")
	}
}

func TestInterBaseMetadataCoreReaderFailureIsIsolated(t *testing.T) {
	for _, test := range []struct {
		name, match string
		failed      MetadataKind
	}{
		{"primary keys", "pk.RDB$CONSTRAINT_TYPE = 'PRIMARY KEY'", MetadataPrimaryKeys},
		{"foreign keys", "RDB$REF_CONSTRAINTS", MetadataForeignKeys},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openInterBaseScalableFixture(t, 2)
			ctx := context.Background()
			failure := errors.New("injected " + test.name + " query failure")
			queryer := failInterBaseMetadataQuery{Queryer: db, match: test.match, err: failure}
			var reader interBaseMetadataRead
			if test.failed == MetadataPrimaryKeys {
				reader = (&InterBaseDBRepository{}).readMetadataPrimaryKeys
			} else {
				reader = (&InterBaseDBRepository{}).readMetadataForeignKeys
			}
			patch, err := reader(ctx, queryer, 127)
			if !errors.Is(err, failure) || patch.Cache != nil || patch.Count != 0 {
				t.Fatalf("failed reader patch=%+v err=%v; want discarded patch and injected error", patch, err)
			}

			repo := &interBaseMetadataFailureRepository{InterBaseDBRepository: &InterBaseDBRepository{Conn: db}, queryMatch: test.match, queryErr: failure, failedKind: test.failed}
			loader := metadata.NewLoader(nil)
			loader.Reset(1)
			if _, err := loader.Start(ctx, 1, metadataPlanFor(repo)); err != nil {
				t.Fatal(err)
			}
			if err := loader.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			snapshot := loader.Snapshot()
			if !snapshot.Cache.MetadataReady(MetadataRelations, MetadataColumnsCurrent) {
				t.Fatal(test.name + " error suppressed usable core metadata")
			}
			if snapshot.Status[test.failed].State != MetadataFailed {
				t.Fatalf("%s status = %s, want failed", test.failed, snapshot.Status[test.failed].State)
			}
		})
	}
}

type failInterBaseMetadataQuery struct {
	schema.Queryer
	match string
	err   error
}

func (q failInterBaseMetadataQuery) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, q.match) {
		return nil, q.err
	}
	return q.Queryer.QueryContext(ctx, query, args...)
}

type interBaseMetadataFailureRepository struct {
	*InterBaseDBRepository
	queryMatch string
	queryErr   error
	failedKind MetadataKind
}

func (r *interBaseMetadataFailureRepository) MetadataPlan() MetadataPlan {
	return MetadataPlan{Parallelism: 1, Jobs: []MetadataJob{
		{Kind: MetadataRelations, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return r.runMetadataRead(ctx, r.readMetadataRelations)
		}},
		{Kind: MetadataColumnsCurrent, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return r.runMetadataRead(ctx, r.readMetadataColumns)
		}},
		{Kind: MetadataPrimaryKeys, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			if r.failedKind != MetadataPrimaryKeys {
				return r.runMetadataRead(ctx, r.readMetadataPrimaryKeys)
			}
			return r.runMetadataRead(ctx, func(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
				return r.readMetadataPrimaryKeys(ctx, failInterBaseMetadataQuery{Queryer: q, match: r.queryMatch, err: r.queryErr}, width)
			})
		}},
		{Kind: MetadataForeignKeys, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			if r.failedKind != MetadataForeignKeys {
				return r.runMetadataRead(ctx, r.readMetadataForeignKeys)
			}
			return r.runMetadataRead(ctx, func(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
				return r.readMetadataForeignKeys(ctx, failInterBaseMetadataQuery{Queryer: q, match: r.queryMatch, err: r.queryErr}, width)
			})
		}},
	}}
}
