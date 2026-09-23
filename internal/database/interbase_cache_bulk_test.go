package database

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// openInterBaseScalableFixture builds an in-memory RDB$ catalog whose relation
// and constraint counts are a parameter, so a test can measure how the number
// of catalog round trips responds to schema size rather than asserting SQL
// text. Every relation carries a composite primary key and every relation but
// the first a composite foreign key to its predecessor: a primary key, a
// foreign key and their indexes are exactly the objects the per-object loader
// walked one query at a time.
//
// The same padding rules as openInterBaseSchemaFixture apply: join keys are
// stored UNPADDED because SQLite compares TEXT byte for byte, while names that
// are only ever displayed keep their catalog padding so trimming stays pinned.
func openInterBaseScalableFixture(t *testing.T, relationCount int) *sql.DB {
	t.Helper()

	name := fmt.Sprintf("file:interbase_bulk_%d?mode=memory&cache=shared", interBaseFixtureSequence.Add(1))
	db, err := sql.Open("sqlite3_interbase_catalog", name)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3_interbase_catalog) error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxIdleConns(4)

	for _, statement := range interBaseFixtureTables {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create catalog fixture table: %v: %s", err, statement)
		}
	}

	insert := func(statement string, args ...any) {
		t.Helper()
		if _, err := db.Exec(statement, args...); err != nil {
			t.Fatalf("insert catalog fixture row: %v: %s", err, statement)
		}
	}
	addInterBaseCatalogIdentifierMetadata(t, db)

	insert(`INSERT INTO "RDB$FIELDS" ("RDB$FIELD_NAME","RDB$FIELD_TYPE","RDB$FIELD_LENGTH","RDB$FIELD_SCALE","RDB$FIELD_SUB_TYPE") VALUES (?,?,?,?,?)`,
		"D_KEY", 8, 4, 0, 0)
	insert(`INSERT INTO "RDB$FIELDS" ("RDB$FIELD_NAME","RDB$FIELD_TYPE","RDB$FIELD_LENGTH","RDB$FIELD_SCALE","RDB$FIELD_SUB_TYPE","RDB$CHARACTER_LENGTH","RDB$DEFAULT_SOURCE") VALUES (?,?,?,?,?,?,?)`,
		"D_LABEL", 37, 20, 0, 0, 20, " DEFAULT 'domain' ")
	// A text BLOB with a character set and a collation. Its type renders as
	// BLOB SUB_TYPE TEXT only while the character set name resolves; without
	// it the renderer falls back and reports a bare BLOB, so this column is
	// what makes the character set and collation joins observable.
	insert(`INSERT INTO "RDB$FIELDS" ("RDB$FIELD_NAME","RDB$FIELD_TYPE","RDB$FIELD_LENGTH","RDB$FIELD_SCALE","RDB$FIELD_SUB_TYPE","RDB$SEGMENT_LENGTH","RDB$CHARACTER_SET_ID","RDB$COLLATION_ID") VALUES (?,?,?,?,?,?,?,?)`,
		"D_NOTES", 261, 8, 0, 1, 80, 4, 2)
	insert(`INSERT INTO "RDB$CHARACTER_SETS" VALUES (?,?)`, 4, interBaseFixed("UTF8"))
	insert(`INSERT INTO "RDB$COLLATIONS" VALUES (?,?,?)`, 4, 2, interBaseFixed("UNICODE"))

	relation := func(name string, id, systemFlag int) {
		insert(`INSERT INTO "RDB$RELATIONS" ("RDB$RELATION_NAME","RDB$RELATION_ID","RDB$OWNER_NAME","RDB$SYSTEM_FLAG") VALUES (?,?,?,?)`,
			name, id, interBaseFixed("SYSDBA"), systemFlag)
	}
	field := func(relationName, name, source string, position int, nullFlag any) {
		insert(`INSERT INTO "RDB$RELATION_FIELDS" ("RDB$FIELD_NAME","RDB$RELATION_NAME","RDB$FIELD_SOURCE","RDB$FIELD_POSITION","RDB$SYSTEM_FLAG","RDB$NULL_FLAG") VALUES (?,?,?,?,?,?)`,
			interBaseFixed(name), relationName, source, position, 0, nullFlag)
	}
	constraint := func(name, kind, relationName, indexName string) {
		insert(`INSERT INTO "RDB$RELATION_CONSTRAINTS" ("RDB$CONSTRAINT_NAME","RDB$CONSTRAINT_TYPE","RDB$RELATION_NAME","RDB$INDEX_NAME") VALUES (?,?,?,?)`,
			name, kind, relationName, indexName)
	}
	index := func(name, relationName string, id, uniqueFlag, segmentCount int, foreignKey any) {
		insert(`INSERT INTO "RDB$INDICES" ("RDB$INDEX_NAME","RDB$RELATION_NAME","RDB$INDEX_ID","RDB$UNIQUE_FLAG","RDB$SEGMENT_COUNT","RDB$INDEX_INACTIVE","RDB$INDEX_TYPE","RDB$FOREIGN_KEY","RDB$SYSTEM_FLAG") VALUES (?,?,?,?,?,?,?,?,?)`,
			name, relationName, id, uniqueFlag, segmentCount, 0, 0, foreignKey, 0)
	}
	segment := func(indexName, fieldName string, position int) {
		insert(`INSERT INTO "RDB$INDEX_SEGMENTS" ("RDB$INDEX_NAME","RDB$FIELD_NAME","RDB$FIELD_POSITION") VALUES (?,?,?)`,
			indexName, interBaseFixed(fieldName), position)
	}

	for i := 1; i <= relationCount; i++ {
		table := interBaseScalableRelationName(i)
		relation(table, i, 0)
		field(table, "KEY_A", "D_KEY", 0, 1)
		field(table, "KEY_B", "D_KEY", 1, 1)
		field(table, "LABEL", "D_LABEL", 2, nil)
		field(table, "NOTES", "D_NOTES", 5, nil)

		primaryIndex := "IDX_PK_" + table
		// Segments are inserted out of position order so that a loader which
		// trusts insertion order instead of RDB$FIELD_POSITION fails here.
		index(primaryIndex, table, i*10, 1, 2, nil)
		segment(primaryIndex, "KEY_B", 1)
		segment(primaryIndex, "KEY_A", 0)
		constraint("PK_"+table, "PRIMARY KEY", table, primaryIndex)

		if i == 1 {
			continue
		}
		parent := interBaseScalableRelationName(i - 1)
		field(table, "REF_A", "D_KEY", 3, nil)
		field(table, "REF_B", "D_KEY", 4, nil)
		foreignIndex := "IDX_FK_" + table
		index(foreignIndex, table, i*10+1, 0, 2, "IDX_PK_"+parent)
		segment(foreignIndex, "REF_B", 1)
		segment(foreignIndex, "REF_A", 0)
		constraint("FK_"+table, "FOREIGN KEY", table, foreignIndex)
		insert(`INSERT INTO "RDB$REF_CONSTRAINTS" ("RDB$CONSTRAINT_NAME","RDB$CONST_NAME_UQ","RDB$UPDATE_RULE","RDB$DELETE_RULE") VALUES (?,?,?,?)`,
			"FK_"+table, "PK_"+parent, "RESTRICT", "RESTRICT")
	}

	// A system relation with a column and a constraint of its own. Neither may
	// ever reach the cache, whichever loader produced it.
	relation("RDB$SYSTEM_FIXTURE", 9000, 1)
	field("RDB$SYSTEM_FIXTURE", "SYSTEM_COLUMN", "D_KEY", 0, nil)
	index("IDX_PK_RDB$SYSTEM_FIXTURE", "RDB$SYSTEM_FIXTURE", 9001, 1, 1, nil)
	segment("IDX_PK_RDB$SYSTEM_FIXTURE", "SYSTEM_COLUMN", 0)
	constraint("PK_RDB$SYSTEM_FIXTURE", "PRIMARY KEY", "RDB$SYSTEM_FIXTURE", "IDX_PK_RDB$SYSTEM_FIXTURE")

	return db
}

func interBaseScalableRelationName(index int) string {
	return fmt.Sprintf("T%03d", index)
}

// interBaseBoundedCacheQueries is how many statements one cache pass may
// issue: a relation list, all column metadata, all primary-key fields and all
// foreign-key field mappings. It is spelled out here rather than imported from
// the implementation so the test pins the budget instead of restating it.
const interBaseBoundedCacheQueries = 4

// interBaseCountCacheBuildPrepares runs the two cache passes a running server
// performs and returns how many statements each one prepared. The fixture
// driver funnels every statement through PrepareContext, so these are exact
// round-trip counts rather than an argument about the code's shape.
func interBaseCountCacheBuildPrepares(t *testing.T, db *sql.DB) (primary, secondary int64) {
	t.Helper()
	ctx := context.Background()
	generator := NewDBCacheUpdater(&InterBaseDBRepository{Conn: db, SQLDialect: 3})

	count := interBaseFixtureCountPrepares(t)
	if _, err := generator.GenerateDBCachePrimary(ctx); err != nil {
		t.Fatalf("GenerateDBCachePrimary() error = %v", err)
	}
	primary = count()

	count = interBaseFixtureCountPrepares(t)
	if _, err := generator.GenerateDBCacheSecondary(ctx); err != nil {
		t.Fatalf("GenerateDBCacheSecondary() error = %v", err)
	}
	secondary = count()
	return primary, secondary
}

func TestInterBaseSnapshotCacheReadsDoNotGrowWithTheSchema(t *testing.T) {
	// The whole point of the bulk loader is that startup cost stops tracking
	// the number of relations and constraints. Measure the same cache build
	// against a small and a large schema and require the count to be equal:
	// an equality is a claim about growth that no per-object loader can pass.
	smallPrimary, smallSecondary := interBaseCountCacheBuildPrepares(t, openInterBaseScalableFixture(t, 2))
	largePrimary, largeSecondary := interBaseCountCacheBuildPrepares(t, openInterBaseScalableFixture(t, 40))

	t.Logf("primary: %d statements for 2 relations, %d for 40", smallPrimary, largePrimary)
	t.Logf("secondary: %d statements for 2 relations, %d for 40", smallSecondary, largeSecondary)

	if largePrimary != smallPrimary {
		t.Errorf("the primary cache pass prepared %d statements for 40 relations and %d for 2; it must not grow with the schema",
			largePrimary, smallPrimary)
	}
	// The secondary pass takes its own snapshot, so it must be bounded too.
	if largeSecondary != smallSecondary {
		t.Errorf("the secondary cache pass prepared %d statements for 40 relations and %d for 2; it must not grow with the schema",
			largeSecondary, smallSecondary)
	}
	if largePrimary > interBaseBoundedCacheQueries {
		t.Errorf("the primary cache pass prepared %d statements, want at most %d bulk queries",
			largePrimary, interBaseBoundedCacheQueries)
	}
	if largeSecondary > interBaseBoundedCacheQueries {
		t.Errorf("the secondary cache pass prepared %d statements, want at most %d bulk queries",
			largeSecondary, interBaseBoundedCacheQueries)
	}
}

// interBaseCompareRepositoryMetadata asserts that two repositories return the
// same cache-visible metadata. It is the equivalence oracle for the bulk
// loader: want is always produced by the direct path, which still reads through
// the driver's schema.Relations and schema.Constraints.
func interBaseCompareRepositoryMetadata(t *testing.T, got, want DBRepository) {
	t.Helper()
	ctx := context.Background()

	gotTables, err := got.SchemaTables(ctx)
	if err != nil {
		t.Fatalf("SchemaTables() error = %v", err)
	}
	wantTables, err := want.SchemaTables(ctx)
	if err != nil {
		t.Fatalf("direct SchemaTables() error = %v", err)
	}
	if !reflect.DeepEqual(gotTables, wantTables) {
		t.Errorf("SchemaTables() = %#v, want the direct result %#v", gotTables, wantTables)
	}

	for _, describe := range []struct {
		name string
		call func(DBRepository) ([]*ColumnDesc, error)
	}{
		{name: "DescribeDatabaseTable", call: func(repo DBRepository) ([]*ColumnDesc, error) {
			return repo.DescribeDatabaseTable(ctx)
		}},
		{name: "DescribeDatabaseTableBySchema", call: func(repo DBRepository) ([]*ColumnDesc, error) {
			return repo.DescribeDatabaseTableBySchema(ctx, "")
		}},
	} {
		gotColumns, err := describe.call(got)
		if err != nil {
			t.Fatalf("%s() error = %v", describe.name, err)
		}
		wantColumns, err := describe.call(want)
		if err != nil {
			t.Fatalf("direct %s() error = %v", describe.name, err)
		}
		if len(gotColumns) != len(wantColumns) {
			t.Fatalf("%s() returned %d columns, want the direct result's %d",
				describe.name, len(gotColumns), len(wantColumns))
		}
		for i := range wantColumns {
			if !reflect.DeepEqual(*gotColumns[i], *wantColumns[i]) {
				t.Errorf("%s() column %d = %#v, want %#v", describe.name, i, *gotColumns[i], *wantColumns[i])
			}
		}
	}

	gotKeys, err := got.DescribeForeignKeysBySchema(ctx, "")
	if err != nil {
		t.Fatalf("DescribeForeignKeysBySchema() error = %v", err)
	}
	wantKeys, err := want.DescribeForeignKeysBySchema(ctx, "")
	if err != nil {
		t.Fatalf("direct DescribeForeignKeysBySchema() error = %v", err)
	}
	if got, want := interBaseRenderForeignKeys(gotKeys), interBaseRenderForeignKeys(wantKeys); !reflect.DeepEqual(got, want) {
		t.Errorf("DescribeForeignKeysBySchema() = %v, want the direct result %v", got, want)
	}
}

// interBaseRenderForeignKeys renders foreign keys as readable text so a failed
// comparison names the offending pair instead of printing pointers. It keeps
// pair order, which is the composite-key ordering guarantee.
func interBaseRenderForeignKeys(keys []*ForeignKey) []string {
	rendered := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs := make([]string, 0, len(*key))
		for _, pair := range *key {
			pairs = append(pairs, fmt.Sprintf("%s.%s.%s->%s.%s.%s",
				pair[0].Schema, pair[0].Table, pair[0].Name,
				pair[1].Schema, pair[1].Table, pair[1].Name))
		}
		rendered = append(rendered, strings.Join(pairs, ","))
	}
	return rendered
}

func TestInterBaseSnapshotMetadataMatchesTheDirectCatalogReads(t *testing.T) {
	// Full metadata equivalence, not a spot check: every ColumnDesc field and
	// every foreign-key pair must match what the driver's own catalog APIs
	// produce, in both SQL dialects.
	fixtures := []struct {
		name string
		open func(*testing.T) *sql.DB
	}{
		{name: "hand written catalog", open: openInterBaseSchemaFixture},
		{name: "composite keys at scale", open: func(t *testing.T) *sql.DB {
			return openInterBaseScalableFixture(t, 6)
		}},
		{name: "empty schema", open: func(t *testing.T) *sql.DB {
			return openInterBaseScalableFixture(t, 0)
		}},
	}
	for _, fixture := range fixtures {
		for _, sqlDialect := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s/dialect %d", fixture.name, sqlDialect), func(t *testing.T) {
				db := fixture.open(t)
				source := &InterBaseDBRepository{Conn: db, SQLDialect: sqlDialect}

				snapshot, closeSnapshot, err := source.CatalogSnapshot(context.Background())
				if err != nil {
					t.Fatalf("CatalogSnapshot() error = %v", err)
				}
				defer func() { _ = closeSnapshot() }()

				interBaseCompareRepositoryMetadata(t, snapshot, source)
			})
		}
	}
}

func TestInterBaseSnapshotKeepsCompositeKeyOrderAndKeyFlags(t *testing.T) {
	// Composite order is the one property a set-shaped bulk read can lose
	// silently, so assert it against literal expectations rather than only
	// against the direct path.
	db := openInterBaseScalableFixture(t, 3)
	source := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	snapshot, closeSnapshot, err := source.CatalogSnapshot(ctx)
	if err != nil {
		t.Fatalf("CatalogSnapshot() error = %v", err)
	}
	defer func() { _ = closeSnapshot() }()

	foreignKeys, err := snapshot.DescribeForeignKeysBySchema(ctx, "")
	if err != nil {
		t.Fatalf("DescribeForeignKeysBySchema() error = %v", err)
	}
	want := []string{
		".T002.REF_A->.T001.KEY_A,.T002.REF_B->.T001.KEY_B",
		".T003.REF_A->.T002.KEY_A,.T003.REF_B->.T002.KEY_B",
	}
	if got := interBaseRenderForeignKeys(foreignKeys); !reflect.DeepEqual(got, want) {
		t.Errorf("foreign keys = %v, want %v (segment position pairs the fields)", got, want)
	}

	columns, err := snapshot.DescribeDatabaseTableBySchema(ctx, "")
	if err != nil {
		t.Fatalf("DescribeDatabaseTableBySchema() error = %v", err)
	}
	wantTypes := map[string]string{
		"T001.KEY_A": "INTEGER",
		"T001.LABEL": "VARCHAR(20)",
		// The subtype, the segment size and the character set all come from
		// the domain projection; if the character set name does not resolve
		// the renderer falls back and reports a bare "BLOB".
		"T001.NOTES": "BLOB SUB_TYPE TEXT SEGMENT SIZE 80",
	}
	for _, column := range columns {
		want, ok := wantTypes[column.Table+"."+column.Name]
		if !ok {
			continue
		}
		if column.Type != want {
			t.Errorf("%s.%s type = %q, want %q", column.Table, column.Name, column.Type, want)
		}
	}

	wantKeys := map[string]string{
		"T001.KEY_A": "YES", "T001.KEY_B": "YES", "T001.LABEL": "NO",
		"T002.REF_A": "NO", "T002.REF_B": "NO",
	}
	seen := map[string]string{}
	for _, column := range columns {
		name := column.Table + "." + column.Name
		if _, ok := wantKeys[name]; ok {
			seen[name] = column.Key
		}
		if strings.HasPrefix(column.Table, "RDB$") || column.Name == "SYSTEM_COLUMN" {
			t.Errorf("a system relation's column reached the cache: %#v", column)
		}
	}
	if !reflect.DeepEqual(seen, wantKeys) {
		t.Errorf("primary-key flags = %v, want %v", seen, wantKeys)
	}

	// The relation list keeps catalog order and drops the system relation.
	tables, err := snapshot.SchemaTables(ctx)
	if err != nil {
		t.Fatalf("SchemaTables() error = %v", err)
	}
	if !reflect.DeepEqual(tables, map[string][]string{"": {"T001", "T002", "T003"}}) {
		t.Errorf("SchemaTables() = %#v, want T001..T003 in catalog order", tables)
	}
}

func TestInterBaseSnapshotSchemaTablesAreNotSharedWithTheCache(t *testing.T) {
	// DBCache.SortedTablesByDBName sorts the cached slice in place, so the
	// snapshot must hand out a copy rather than its own storage.
	db := openInterBaseScalableFixture(t, 3)
	source := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	snapshot, closeSnapshot, err := source.CatalogSnapshot(ctx)
	if err != nil {
		t.Fatalf("CatalogSnapshot() error = %v", err)
	}
	defer func() { _ = closeSnapshot() }()

	first, err := snapshot.SchemaTables(ctx)
	if err != nil {
		t.Fatalf("SchemaTables() error = %v", err)
	}
	first[""][0] = "MUTATED"

	second, err := snapshot.SchemaTables(ctx)
	if err != nil {
		t.Fatalf("second SchemaTables() error = %v", err)
	}
	if second[""][0] != "T001" {
		t.Errorf("SchemaTables() = %#v after a caller mutated an earlier result; each call must return its own copy", second)
	}
}

func TestInterBaseSnapshotReleasesItsTransactionWhenALoadFails(t *testing.T) {
	// A failed catalog read must not leave the read-only transaction holding a
	// pooled connection, and must not expose the rows it already read.
	db := openInterBaseScalableFixture(t, 3)
	if _, err := db.Exec(`DROP TABLE "RDB$INDEX_SEGMENTS"`); err != nil {
		t.Fatalf("drop the index segment table: %v", err)
	}
	source := &InterBaseDBRepository{Conn: db, SQLDialect: 3}

	snapshot, closeSnapshot, err := source.CatalogSnapshot(context.Background())
	if err == nil {
		if closeSnapshot != nil {
			_ = closeSnapshot()
		}
		t.Fatalf("CatalogSnapshot() succeeded with an unreadable catalog; got %#v", snapshot)
	}
	if snapshot != nil {
		t.Errorf("CatalogSnapshot() returned %#v with an error; a partial catalog must never be served", snapshot)
	}
	if closeSnapshot != nil {
		t.Errorf("CatalogSnapshot() returned a closer with an error; the transaction is already rolled back")
	}
	if inUse := db.Stats().InUse; inUse != 0 {
		t.Errorf("%d connections are still in use after a failed snapshot; the transaction leaked", inUse)
	}

	// The pool stays usable, which is what the failed cache build falls back to.
	if _, err := source.DescribeGenerators(context.Background()); err != nil {
		t.Errorf("the source repository must stay usable after a failed snapshot: %v", err)
	}
}

func TestInterBaseSnapshotReleasesItsTransactionOnClose(t *testing.T) {
	db := openInterBaseScalableFixture(t, 3)
	source := &InterBaseDBRepository{Conn: db, SQLDialect: 3}

	_, closeSnapshot, err := source.CatalogSnapshot(context.Background())
	if err != nil {
		t.Fatalf("CatalogSnapshot() error = %v", err)
	}
	if inUse := db.Stats().InUse; inUse != 1 {
		t.Errorf("%d connections are in use while a snapshot is open, want exactly the transaction's one", inUse)
	}
	if err := closeSnapshot(); err != nil {
		t.Fatalf("close() error = %v", err)
	}
	if inUse := db.Stats().InUse; inUse != 0 {
		t.Errorf("%d connections are still in use after the snapshot closed", inUse)
	}
}
