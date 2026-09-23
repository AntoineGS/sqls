package database

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

func TestInterBaseMetadataFunctionsMatchLegacyWithConstantQueries(t *testing.T) {
	for _, count := range []int{1, 100} {
		t.Run(fmt.Sprintf("%d-functions", count), func(t *testing.T) {
			db := openInterBaseSchemaFixture(t)
			for i := 0; i < count; i++ {
				name := fmt.Sprintf("UDF_%03d", i)
				if _, err := db.Exec(`INSERT INTO "RDB$FUNCTIONS" VALUES (?,?,?,?,?,?,?)`, name, 0, "description", interBaseFixed("module"), interBaseFixed("entry"), 1, 0); err != nil {
					t.Fatal(err)
				}
				// Return position 1 names an input argument. Character length is
				// absent for CHAR, while CSTRING and INTEGER exercise rendering.
				for _, arg := range []struct{ pos, mechanism, length, typ int }{{1, 1, 255, 40}, {2, 1, 10, 14}, {3, 1, 4, 8}} {
					if _, err := db.Exec(`INSERT INTO "RDB$FUNCTION_ARGUMENTS" VALUES (?,?,?,?,?,?,?,?,?,?)`, name, arg.pos, arg.mechanism, arg.length, 0, arg.typ, 0, 0, nil, nil); err != nil {
						t.Fatal(err)
					}
				}
			}
			repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
			want, err := repo.DescribeFunctions(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			countQueries := interBaseFixtureCountPrepares(t)
			patch, err := repo.runMetadataRead(context.Background(), repo.readMetadataFunctions)
			if err != nil {
				t.Fatal(err)
			}
			got := functionMapValues(patch.Cache.Catalog.Functions)
			sort.Slice(got, func(i, j int) bool { return got[i].Name < got[j].Name })
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("function descriptors differ\n got: %#v\nwant: %#v", got, want)
			}
			if queries := countQueries(); queries != 3 {
				t.Fatalf("queries = %d, want two data reads and one width discovery", queries)
			}
			if patch.Count != count+1 { // The fixture also contains F_LTRIM.
				t.Fatalf("Count = %d, want %d", patch.Count, count+1)
			}
		})
	}
}

func TestInterBaseMetadataPlanHasIndependentBoundedJobs(t *testing.T) {
	plan := (&InterBaseDBRepository{}).MetadataPlan()
	if plan.Parallelism != 3 {
		t.Fatalf("parallelism = %d, want 3", plan.Parallelism)
	}
	want := []MetadataKind{MetadataSchemas, MetadataRelations, MetadataColumnsCurrent, MetadataProcedures, MetadataPrimaryKeys, MetadataViews, MetadataIndexes, MetadataForeignKeys, MetadataFunctions, MetadataGenerators, MetadataDomains, MetadataTriggers}
	if len(plan.Jobs) != len(want) {
		t.Fatalf("job count = %d, want %d", len(plan.Jobs), len(want))
	}
	for i, job := range plan.Jobs {
		if job.Kind != want[i] {
			t.Errorf("job[%d] = %s, want %s", i, job.Kind, want[i])
		}
		if job.Kind == MetadataColumnsAll {
			t.Fatal("InterBase plan must omit all-columns")
		}
		if len(job.DependsOn) != 0 {
			t.Errorf("%s dependencies = %v, want none", job.Kind, job.DependsOn)
		}
		if job.Run == nil {
			t.Errorf("%s has nil Run", job.Kind)
		}
	}
}

func TestInterBaseMetadataFunctionsKeepEmptyAndUnsupportedRendering(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	if _, err := db.Exec(`INSERT INTO "RDB$FUNCTIONS" VALUES (?,?,?,?,?,?,?)`, "UDF_UNSUPPORTED", 0, nil, nil, nil, 9, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO "RDB$FUNCTION_ARGUMENTS" VALUES (?,?,?,?,?,?,?,?,?,?)`, "UDF_UNSUPPORTED", 9, 1, 4, 0, 999, 0, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO "RDB$FUNCTIONS" VALUES (?,?,?,?,?,?,?)`, "UDF_EMPTY", 0, nil, nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	repo := &InterBaseDBRepository{Conn: db}
	want, err := repo.DescribeFunctions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	patch, err := repo.runMetadataRead(context.Background(), repo.readMetadataFunctions)
	if err != nil {
		t.Fatal(err)
	}
	got := functionMapValues(patch.Cache.Catalog.Functions)
	sort.Slice(got, func(i, j int) bool { return got[i].Name < got[j].Name })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("function descriptors differ\n got: %#v\nwant: %#v", got, want)
	}
	if got := patch.Cache.Catalog.Functions[catalogCacheKey("UDF_EMPTY")]; got == nil || len(got.Arguments) != 0 {
		t.Fatalf("zero-argument function descriptor = %#v", got)
	}
	if got := patch.Cache.Catalog.Functions[catalogCacheKey("UDF_UNSUPPORTED")]; got == nil || got.ReturnType != "" || len(got.Arguments) != 1 || got.Arguments[0].Type != "" {
		t.Fatalf("unsupported rendering descriptor = %#v", got)
	}
}

func TestInterBaseMetadataRepositoryReadUsesBoundSingleConnection(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	db.SetMaxOpenConns(1)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 1, SourceSQLDialect: 3, DatabaseName: "attachment"}
	countQueries := interBaseFixtureCountPrepares(t)
	if _, err := repo.DescribeGenerators(context.Background()); err != nil {
		t.Fatal(err)
	}
	directQueries := countQueries()
	interBaseFixtureCountPrepares(t)
	patch, err := repo.runMetadataRepositoryRead(context.Background(), func(ctx context.Context, bound *InterBaseDBRepository) (MetadataPatch, error) {
		if bound.Conn != db || bound.SQLDialect != 1 || bound.SourceSQLDialect != 3 || bound.DatabaseName != "attachment" {
			t.Fatalf("bound repository lost connection metadata: %+v", bound)
		}
		if bound.snapshot == nil || bound.snapshot.catalog == nil {
			t.Fatal("bound repository lacks its transaction catalog")
		}
		values, err := bound.DescribeGenerators(ctx)
		if err != nil {
			return MetadataPatch{}, err
		}
		items := make(map[string]*GeneratorDesc, len(values))
		for _, value := range values {
			copy := *value
			items[catalogCacheKey(value.Name)] = &copy
		}
		return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Generators: items}}, Count: len(items)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if patch.Cache == nil || patch.Cache.Catalog == nil || patch.Cache.Catalog.Generators == nil {
		t.Fatalf("repository fragment is incomplete: %+v", patch)
	}
	if got := countQueries(); got != directQueries {
		t.Fatalf("repository queries = %d, direct accessor queries = %d; wrapper must not add width discovery", got, directQueries)
	}
}

func functionMapValues(values map[string]*FunctionDesc) []*FunctionDesc {
	result := make([]*FunctionDesc, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}
