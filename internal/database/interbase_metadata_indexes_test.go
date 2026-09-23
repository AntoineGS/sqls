package database

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestInterBaseMetadataViewsMatchLegacyAndUseConstantQueries(t *testing.T) {
	for _, size := range []int{1, 100} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			db := openInterBaseScalableFixture(t, size)
			insertMetadataViewFixtures(t, db, size)
			repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
			ctx := context.Background()
			want, err := repo.DescribeViews(ctx)
			if err != nil {
				t.Fatal(err)
			}
			count := interBaseFixtureCountPrepares(t)
			patch, err := repo.runMetadataRead(ctx, repo.readMetadataViews)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]*ViewDesc, 0, len(patch.Cache.Catalog.Views))
			for _, view := range patch.Cache.Catalog.Views {
				got = append(got, view)
			}
			sort.Slice(got, func(i, j int) bool { return got[i].Name < got[j].Name })
			sort.Slice(want, func(i, j int) bool { return want[i].Name < want[j].Name })
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("view descriptors differ\n got: %#v\nwant: %#v", got, want)
			}
			if patch.Count != size {
				t.Fatalf("Count = %d, want %d", patch.Count, size)
			}
			if queries := count(); queries != 3 {
				t.Fatalf("queries = %d, want 2 data + 1 width", queries)
			}
		})
	}
}

func TestInterBaseBulkViewColumnsQueryExplicitlyRestrictsToViews(t *testing.T) {
	query := interBaseBulkViewColumnsQueryForWidth(127)
	if !strings.Contains(query, "WHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0\n  AND r.RDB$VIEW_BLR IS NOT NULL") {
		t.Fatalf("view-column query is missing its view-only predicate:\n%s", query)
	}
	if strings.Count(query, "r.RDB$VIEW_BLR IS NOT NULL") != 1 {
		t.Fatalf("view-only predicate occurs %d times, want exactly once", strings.Count(query, "r.RDB$VIEW_BLR IS NOT NULL"))
	}
	if strings.Contains(interBaseBulkColumnsQueryForWidth(127), "r.RDB$VIEW_BLR IS NOT NULL") {
		t.Fatal("general column query unexpectedly excludes non-view relations")
	}
}

func TestInterBaseMetadataIndexesMatchLegacyAndUseConstantQueries(t *testing.T) {
	for _, size := range []int{1, 100} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			db := openInterBaseScalableFixture(t, size)
			insertMetadataIndexFixtures(t, db, size)
			repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
			ctx := context.Background()
			want, err := repo.DescribeIndexes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			count := interBaseFixtureCountPrepares(t)
			patch, err := repo.runMetadataRead(ctx, repo.readMetadataIndexes)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]*IndexDesc, 0, len(patch.Cache.Catalog.Indexes))
			for _, index := range patch.Cache.Catalog.Indexes {
				got = append(got, index)
			}
			sort.Slice(got, func(i, j int) bool { return got[i].Name < got[j].Name })
			sort.Slice(want, func(i, j int) bool { return want[i].Name < want[j].Name })
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("index descriptors differ\n got: %#v\nwant: %#v", got, want)
			}
			if patch.Count != len(want) {
				t.Fatalf("Count = %d, want %d", patch.Count, len(want))
			}
			if queries := count(); queries != 3 {
				t.Fatalf("queries = %d, want 2 data + 1 width", queries)
			}
		})
	}
}

func TestInterBaseMetadataIndexInvalidSegmentPositionsDiscardCategory(t *testing.T) {
	for _, test := range []struct {
		name      string
		positions []int
	}{
		{name: "missing position", positions: []int{0, 2}},
		{name: "duplicate position", positions: []int{0, 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openInterBaseScalableFixture(t, 1)
			if _, err := db.Exec(`DELETE FROM "RDB$INDEX_SEGMENTS" WHERE "RDB$INDEX_NAME" = 'IDX_PK_T001'`); err != nil {
				t.Fatal(err)
			}
			for i, position := range test.positions {
				field := "KEY_A"
				if i == 1 {
					field = "KEY_B"
				}
				if _, err := db.Exec(`INSERT INTO "RDB$INDEX_SEGMENTS" ("RDB$INDEX_NAME","RDB$FIELD_NAME","RDB$FIELD_POSITION") VALUES (?,?,?)`, "IDX_PK_T001", interBaseFixed(field), position); err != nil {
					t.Fatal(err)
				}
			}
			patch, err := (&InterBaseDBRepository{}).readMetadataIndexes(context.Background(), db, 127)
			if err == nil {
				t.Fatal("readMetadataIndexes succeeded for invalid segment positions")
			}
			if patch.Cache != nil || patch.Count != 0 {
				t.Fatalf("failed category published patch=%+v", patch)
			}
		})
	}
}

func TestInterBaseMetadataViewAndIndexQueryFailuresDiscardCategory(t *testing.T) {
	for _, test := range []struct {
		name  string
		read  interBaseMetadataRead
		match string
	}{
		{name: "view columns", read: (&InterBaseDBRepository{}).readMetadataViews, match: "FROM RDB$RELATION_FIELDS rf"},
		{name: "index segments", read: (&InterBaseDBRepository{}).readMetadataIndexes, match: "FROM RDB$INDEX_SEGMENTS s"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openInterBaseScalableFixture(t, 1)
			failure := fmt.Errorf("injected %s error", test.name)
			queryer := failInterBaseMetadataQuery{Queryer: db, match: test.match, err: failure}
			patch, err := test.read(context.Background(), queryer, 127)
			if err == nil || !strings.Contains(err.Error(), failure.Error()) {
				t.Fatalf("error = %v, want injected error", err)
			}
			if patch.Cache != nil || patch.Count != 0 {
				t.Fatalf("failed category published patch=%+v", patch)
			}
		})
	}
}

func insertMetadataViewFixtures(t *testing.T, db *sql.DB, size int) {
	t.Helper()
	for i := 1; i <= size; i++ {
		name := fmt.Sprintf("BULK_VIEW_%03d", i)
		if _, err := db.Exec(`INSERT INTO "RDB$RELATIONS" ("RDB$RELATION_NAME","RDB$RELATION_ID","RDB$VIEW_SOURCE","RDB$DESCRIPTION","RDB$OWNER_NAME","RDB$RELATION_TYPE","RDB$SYSTEM_FLAG","RDB$VIEW_BLR") VALUES (?,?,?,?,?,?,?,?)`, name, 10000+i, "SELECT LABEL FROM T001", " description ", "OWNER", "VIEW", 0, "BLR"); err != nil {
			t.Fatal(err)
		}
		if i == 1 { // A valid view without columns must remain in the category.
			continue
		}
		if _, err := db.Exec(`INSERT INTO "RDB$RELATION_FIELDS" ("RDB$FIELD_NAME","RDB$RELATION_NAME","RDB$FIELD_SOURCE","RDB$FIELD_POSITION","RDB$SYSTEM_FLAG") VALUES (?,?,?,?,?)`, interBaseFixed("LABEL"), name, "D_LABEL", 0, 0); err != nil {
			t.Fatal(err)
		}
	}
}

func insertMetadataIndexFixtures(t *testing.T, db *sql.DB, size int) {
	t.Helper()
	for i := 1; i <= size; i++ {
		name := fmt.Sprintf("EXPR_INDEX_%03d", i)
		if _, err := db.Exec(`INSERT INTO "RDB$INDICES" ("RDB$INDEX_NAME","RDB$RELATION_NAME","RDB$INDEX_ID","RDB$UNIQUE_FLAG","RDB$DESCRIPTION","RDB$SEGMENT_COUNT","RDB$INDEX_INACTIVE","RDB$INDEX_TYPE","RDB$SYSTEM_FLAG","RDB$EXPRESSION_SOURCE") VALUES (?,?,?,?,?,?,?,?,?,?)`, name, interBaseScalableRelationName(i), 30000+i, 1, " index description ", 0, 1, 0, 0, "UPPER(LABEL)"); err != nil {
			t.Fatal(err)
		}
	}
	// Database-level index names may share a long prefix. Width-aware casts and
	// exact identity grouping must not collapse them to the old 67-byte limit.
	prefix := strings.Repeat("L", 70)
	for i, suffix := range []string{"A", "B"} {
		name := prefix + suffix
		if _, err := db.Exec(`INSERT INTO "RDB$INDICES" ("RDB$INDEX_NAME","RDB$RELATION_NAME","RDB$INDEX_ID","RDB$UNIQUE_FLAG","RDB$SEGMENT_COUNT","RDB$INDEX_INACTIVE","RDB$INDEX_TYPE","RDB$SYSTEM_FLAG") VALUES (?,?,?,?,?,?,?,?)`, name, interBaseScalableRelationName(1), 40000+i, 0, 0, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
}
