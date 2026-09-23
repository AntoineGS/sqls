package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

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
			loader := NewMetadataLoader()
			loader.Reset(1)
			if _, err := loader.Start(ctx, 1, repo); err != nil {
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
