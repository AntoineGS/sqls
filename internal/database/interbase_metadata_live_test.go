//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"
)

// This is intentionally opt-in and read-only. Run once against maintenance-
// stable Dialect 1 and Dialect 3 databases; concurrent DDL can invalidate parity.
func TestInterBaseMetadataLive(t *testing.T) {
	for _, dialect := range []int{1, 3} {
		t.Run("dialect-"+string(rune('0'+dialect)), func(t *testing.T) {
			connection, err := Open(interBaseLiveConfig(t, dialect))
			if err != nil {
				t.Fatalf("open InterBase: %v", err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			repo := NewInterBaseDBRepositoryFromConnection(connection).(*InterBaseDBRepository)
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

			after := connection.Conn.Stats()
			t.Logf("dialect=%d categories=%d db.Stats WaitCount delta=%d WaitDuration delta=%s; per-job query counts are not instrumented by database/sql", dialect, len(snapshot.Status), after.WaitCount-before.WaitCount, after.WaitDuration-before.WaitDuration)
			for kind, status := range snapshot.Status {
				t.Logf("category=%s state=%s count=%d queries=%d queries-known=%t", kind, status.State, status.Count, status.Queries, status.QueriesKnown)
			}
		})
	}
}
