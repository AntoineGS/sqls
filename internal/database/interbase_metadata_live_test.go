//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	sqldialect "github.com/sqls-server/sqls/dialect"
)

// This is intentionally opt-in and read-only. Run once against maintenance-
// stable Dialect 1 and Dialect 3 databases; concurrent DDL can invalidate parity.
func TestInterBaseMetadataLive(t *testing.T) {
	databases := map[int]string{}
	for _, dialect := range []int{1, 3} {
		t.Run("dialect-"+string(rune('0'+dialect)), func(t *testing.T) {
			config := interBaseMetadataDialectConfig(t, dialect)
			databases[dialect] = config.DataSourceName
			connection, err := Open(config)
			if err != nil {
				t.Fatalf("open InterBase: %v", err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			repo := NewInterBaseDBRepositoryFromConnection(connection).(*InterBaseDBRepository)
			if repo.SQLDialect != dialect || repo.SourceSQLDialect != dialect {
				t.Fatalf("resolved/server dialects = %d/%d; requested separate dialect-%d database", repo.SQLDialect, repo.SourceSQLDialect, dialect)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			before := connection.Conn.Stats()
			loader := NewMetadataLoader()
			t.Cleanup(loader.Stop)
			loader.Reset(1)
			load, err := loader.Start(ctx, 1, repo)
			if err != nil {
				t.Fatalf("start metadata plan: %v", err)
			}
			select {
			case <-load.Done:
			case <-ctx.Done():
				t.Fatalf("metadata plan did not settle: %v", ctx.Err())
			}
			snapshot := loader.Snapshot()
			if snapshot.Degraded() {
				t.Fatalf("metadata plan degraded: %+v", snapshot.Status)
			}
			for kind, status := range snapshot.Status {
				if kind == MetadataColumnsAll { // InterBase intentionally does not offer this category.
					continue
				}
				if status.State != MetadataReady {
					t.Fatalf("category %s state=%s, wanted complete ready snapshot", kind, status.State)
				}
				if count := interBaseMetadataFragmentCount(snapshot.Cache, kind); count != status.Count {
					t.Errorf("category %s map entries=%d, status count=%d", kind, count, status.Count)
				}
			}

			// Compare complete descriptors, not only names or category totals.
			wantViews, err := repo.DescribeViews(ctx)
			if err != nil {
				t.Fatalf("legacy views: %v", err)
			}
			gotViews := make([]*ViewDesc, 0, len(snapshot.Cache.Catalog.Views))
			for _, item := range snapshot.Cache.Catalog.Views {
				gotViews = append(gotViews, item)
			}
			sort.Slice(wantViews, func(i, j int) bool { return wantViews[i].Name < wantViews[j].Name })
			sort.Slice(gotViews, func(i, j int) bool { return gotViews[i].Name < gotViews[j].Name })
			if !reflect.DeepEqual(gotViews, wantViews) {
				t.Errorf("view descriptors differ: got %d, legacy %d", len(gotViews), len(wantViews))
			}

			wantIndexes, err := repo.DescribeIndexes(ctx)
			if err != nil {
				t.Fatalf("legacy indexes: %v", err)
			}
			gotIndexes := make([]*IndexDesc, 0, len(snapshot.Cache.Catalog.Indexes))
			for _, item := range snapshot.Cache.Catalog.Indexes {
				gotIndexes = append(gotIndexes, item)
			}
			sort.Slice(wantIndexes, func(i, j int) bool { return wantIndexes[i].Name < wantIndexes[j].Name })
			sort.Slice(gotIndexes, func(i, j int) bool { return gotIndexes[i].Name < gotIndexes[j].Name })
			if !reflect.DeepEqual(gotIndexes, wantIndexes) {
				t.Errorf("index descriptors differ: got %d, legacy %d", len(gotIndexes), len(wantIndexes))
			}

			wantProcedures, err := repo.DescribeProcedures(ctx)
			if err != nil {
				t.Fatalf("legacy procedures: %v", err)
			}
			gotProcedures := make([]*ProcedureDesc, 0, len(snapshot.Cache.Catalog.Procedures))
			for _, item := range snapshot.Cache.Catalog.Procedures {
				gotProcedures = append(gotProcedures, item)
			}
			sort.Slice(wantProcedures, func(i, j int) bool { return wantProcedures[i].Name < wantProcedures[j].Name })
			sort.Slice(gotProcedures, func(i, j int) bool { return gotProcedures[i].Name < gotProcedures[j].Name })
			if !reflect.DeepEqual(gotProcedures, wantProcedures) {
				t.Errorf("procedure descriptors differ: got %d, legacy %d", len(gotProcedures), len(wantProcedures))
			}

			wantFunctions, err := repo.DescribeFunctions(ctx)
			if err != nil {
				t.Fatalf("legacy functions: %v", err)
			}
			gotFunctions := functionMapValues(snapshot.Cache.Catalog.Functions)
			sort.Slice(wantFunctions, func(i, j int) bool { return wantFunctions[i].Name < wantFunctions[j].Name })
			sort.Slice(gotFunctions, func(i, j int) bool { return gotFunctions[i].Name < gotFunctions[j].Name })
			if !reflect.DeepEqual(gotFunctions, wantFunctions) {
				t.Errorf("function descriptors differ: got %d, legacy %d", len(gotFunctions), len(wantFunctions))
			}
			wantGenerators, err := repo.DescribeGenerators(ctx)
			if err != nil {
				t.Fatalf("legacy generators: %v", err)
			}
			gotGenerators := make([]*GeneratorDesc, 0, len(snapshot.Cache.Catalog.Generators))
			for _, item := range snapshot.Cache.Catalog.Generators {
				gotGenerators = append(gotGenerators, item)
			}
			sort.Slice(wantGenerators, func(i, j int) bool { return wantGenerators[i].Name < wantGenerators[j].Name })
			sort.Slice(gotGenerators, func(i, j int) bool { return gotGenerators[i].Name < gotGenerators[j].Name })
			if !reflect.DeepEqual(gotGenerators, wantGenerators) {
				t.Errorf("generator descriptors differ: got %d, legacy %d", len(gotGenerators), len(wantGenerators))
			}
			wantDomains, err := repo.DescribeDomains(ctx)
			if err != nil {
				t.Fatalf("legacy domains: %v", err)
			}
			gotDomains := make([]*DomainDesc, 0, len(snapshot.Cache.Catalog.Domains))
			for _, item := range snapshot.Cache.Catalog.Domains {
				gotDomains = append(gotDomains, item)
			}
			sort.Slice(wantDomains, func(i, j int) bool { return wantDomains[i].Name < wantDomains[j].Name })
			sort.Slice(gotDomains, func(i, j int) bool { return gotDomains[i].Name < gotDomains[j].Name })
			if !reflect.DeepEqual(gotDomains, wantDomains) {
				t.Errorf("domain descriptors differ: got %d, legacy %d", len(gotDomains), len(wantDomains))
			}
			wantTriggers, err := repo.DescribeTriggers(ctx)
			if err != nil {
				t.Fatalf("legacy triggers: %v", err)
			}
			gotTriggers := make([]*TriggerDesc, 0, len(snapshot.Cache.Catalog.Triggers))
			for _, item := range snapshot.Cache.Catalog.Triggers {
				gotTriggers = append(gotTriggers, item)
			}
			sort.Slice(wantTriggers, func(i, j int) bool { return wantTriggers[i].Name < wantTriggers[j].Name })
			sort.Slice(gotTriggers, func(i, j int) bool { return gotTriggers[i].Name < gotTriggers[j].Name })
			if !reflect.DeepEqual(gotTriggers, wantTriggers) {
				t.Errorf("trigger descriptors differ: got %d, legacy %d", len(gotTriggers), len(wantTriggers))
			}

			wantRelations, err := repo.SchemaTables(ctx)
			if err != nil {
				t.Fatalf("legacy relations: %v", err)
			}
			if !reflect.DeepEqual(snapshot.Cache.SchemaTables, wantRelations) {
				t.Errorf("relation maps differ: loader=%d legacy=%d", len(snapshot.Cache.SchemaTables[""]), len(wantRelations[""]))
			}
			wantColumns, err := repo.DescribeDatabaseTable(ctx)
			if err != nil {
				t.Fatalf("legacy columns: %v", err)
			}
			gotColumns := make([]*ColumnDesc, 0)
			for _, columns := range snapshot.Cache.ColumnsWithParent {
				gotColumns = append(gotColumns, columns...)
			}
			sort.Slice(wantColumns, func(i, j int) bool {
				if wantColumns[i].Table != wantColumns[j].Table {
					return wantColumns[i].Table < wantColumns[j].Table
				}
				return wantColumns[i].Name < wantColumns[j].Name
			})
			sort.Slice(gotColumns, func(i, j int) bool {
				if gotColumns[i].Table != gotColumns[j].Table {
					return gotColumns[i].Table < gotColumns[j].Table
				}
				return gotColumns[i].Name < gotColumns[j].Name
			})
			if !reflect.DeepEqual(gotColumns, wantColumns) {
				t.Errorf("column descriptors differ: got %d, legacy %d", len(gotColumns), len(wantColumns))
			}
			wantForeignKeys, err := repo.DescribeForeignKeysBySchema(ctx, "")
			if err != nil {
				t.Fatalf("legacy foreign keys: %v", err)
			}
			gotForeignKeys := uniqueInterBaseForeignKeys(snapshot.Cache.ForeignKeys)
			sort.Slice(wantForeignKeys, func(i, j int) bool {
				return interBaseForeignKeyIdentity(wantForeignKeys[i]) < interBaseForeignKeyIdentity(wantForeignKeys[j])
			})
			if !reflect.DeepEqual(gotForeignKeys, wantForeignKeys) {
				t.Errorf("foreign-key descriptors differ: got %d, legacy %d", len(gotForeignKeys), len(wantForeignKeys))
			}

			after := connection.Conn.Stats()
			t.Logf("dialect=%d categories=%d db.Stats pool-wide WaitCount delta=%d pool-wide WaitDuration delta=%s", dialect, len(snapshot.Status), after.WaitCount-before.WaitCount, after.WaitDuration-before.WaitDuration)
			for kind, status := range snapshot.Status {
				t.Logf("category=%s state=%s count=%d queries=%d queries-known=%t queue=%s run=%s", kind, status.State, status.Count, status.Queries, status.QueriesKnown, metadataJobMetrics(status).Queue, metadataJobMetrics(status).Run)
			}
		})
	}
	if databases[1] != "" && databases[3] != "" && databases[1] == databases[3] {
		t.Fatal("Dialect 1 and Dialect 3 must use separately identified database configurations")
	}
}

func interBaseMetadataDialectConfig(t *testing.T, dialect int) *DBConfig {
	t.Helper()
	prefix := fmt.Sprintf("INTERBASE_DIALECT%d_", dialect)
	database, databaseSet := os.LookupEnv(prefix + "DATABASE")
	user, userSet := os.LookupEnv(prefix + "USER")
	password, passwordSet := os.LookupEnv(prefix + "PASSWORD")
	if !databaseSet || database == "" || !userSet || user == "" || !passwordSet {
		t.Skipf("set %sDATABASE, %sUSER, and %sPASSWORD for a separate read-only Dialect %d database", prefix, prefix, prefix, dialect)
	}
	return &DBConfig{Alias: fmt.Sprintf("live-dialect-%d", dialect), Driver: sqldialect.DatabaseDriverInterBase, DataSourceName: database, User: user, Passwd: password, Dialect: dialect}
}
