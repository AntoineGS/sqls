//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	interbase "interbase-go"
	"interbase-go/schema"
)

type watchedNativeRecorder struct {
	q       schema.Queryer
	queries []watchedRecordedQuery
}

func (q *watchedNativeRecorder) QueryContext(ctx context.Context, s string, args ...any) (*sql.Rows, error) {
	q.queries = append(q.queries, watchedRecordedQuery{s, append([]any(nil), args...)})
	return q.q.QueryContext(ctx, s, args...)
}

func TestWatchedChangesDisposableNativeParity(t *testing.T) {
	if os.Getenv("SQLS_WATCH_DISPOSABLE") != "1" {
		t.Skip("disposable fixture wrapper required")
	}
	for _, dialect := range []int{1, 3} {
		t.Run(fmt.Sprintf("dialect-%d", dialect), func(t *testing.T) {
			target := os.Getenv("SQLS_WATCH_DISPOSABLE_DATABASE")
			if dialect == 1 {
				target = os.Getenv("SQLS_WATCH_DISPOSABLE_DATABASE_D1")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			conn, err := AttachInterBaseForMetadata(ctx, InterBaseMetadataAttachConfig{Database: target, User: os.Getenv("SQLS_WATCH_DISPOSABLE_USER"), Password: os.Getenv("SQLS_WATCH_DISPOSABLE_PASSWORD"), Charset: "NONE", CatalogTextCharset: "WIN1252"}, dialect)
			if err != nil {
				t.Fatal("disposable attach failed")
			}
			defer conn.Close()
			conn.SetMaxOpenConns(1)
			repo := &InterBaseDBRepository{Conn: conn, SQLDialect: dialect, SourceSQLDialect: dialect}
			ddlNumber := 0
			exec := func(t *testing.T, sql string) {
				t.Helper()
				ddlNumber++
				if _, err := conn.ExecContext(ctx, sql); err != nil {
					t.Fatalf("disposable DDL number %d failed: %v", ddlNumber, err)
				}
			}
			for _, sql := range []string{
				"CREATE DOMAIN WATCH_D AS INTEGER",
				"CREATE TABLE WATCH_A (ID INTEGER NOT NULL PRIMARY KEY, C WATCH_D)",
				"CREATE TABLE WATCH_B (ID INTEGER NOT NULL PRIMARY KEY, A_ID INTEGER, C WATCH_D, CONSTRAINT WATCH_FK FOREIGN KEY (A_ID) REFERENCES WATCH_A (ID), CONSTRAINT WATCH_CK CHECK (A_ID > 0))",
				"CREATE PROCEDURE WATCH_P1 (A INTEGER) RETURNS (B INTEGER) AS BEGIN B = A; SUSPEND; END",
				"CREATE PROCEDURE WATCH_P2 AS BEGIN /*" + strings.Repeat("untouched", 4000) + "*/ EXIT; END",
				"CREATE PROCEDURE WATCH_GRAPH RETURNS (B INTEGER) AS BEGIN SELECT ID FROM WATCH_A INTO :B; SUSPEND; END",
				"CREATE VIEW WATCH_V AS SELECT ID, C FROM WATCH_A",
			} {
				exec(t, sql)
			}
			if dialect == 3 {
				exec(t, `CREATE TABLE "WatchExact" ("id" INTEGER NOT NULL PRIMARY KEY, "C" INTEGER)`)
				exec(t, `CREATE TABLE "WATCHEXACT" ("ID" INTEGER NOT NULL PRIMARY KEY, "D" INTEGER)`)
			}
			scan := func() *schema.CatalogFingerprint {
				t.Helper()
				c, err := conn.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				f, err := interbase.ReadCatalogFingerprint(ctx, c, 64<<20)
				closeErr := c.Close()
				if err != nil || closeErr != nil {
					t.Fatal("disposable scan failed", err, closeErr)
				}
				return f
			}
			load := func() *catalog.Cache {
				t.Helper()
				cache, _, _, err := repo.ReadWatchedFull(ctx)
				if err != nil {
					t.Fatal(err)
				}
				return cache
			}
			baseline := scan()
			base := load()
			steps := []struct{ name, sql string }{
				{"body-only", "ALTER PROCEDURE WATCH_P1 (A INTEGER) RETURNS (B INTEGER) AS BEGIN B = A + 1; SUSPEND; END"},
				{"shared-domain", "ALTER DOMAIN WATCH_D SET DEFAULT 7"},
				{"generated-domain", "ALTER TABLE WATCH_A ADD GEN_VALUE VARCHAR(27)"},
				{"generated-domain-change", "ALTER TABLE WATCH_A ALTER GEN_VALUE TYPE VARCHAR(31)"},
				{"quoted-name", `ALTER TABLE "WatchExact" ADD "CaseColumn" INTEGER`},
				{"view-context", "CREATE VIEW WATCH_V (ID, C) AS SELECT ID, A_ID FROM WATCH_B"},
				{"drop-check", "ALTER TABLE WATCH_B DROP CONSTRAINT WATCH_CK"},
				{"drop-FK", "ALTER TABLE WATCH_B DROP CONSTRAINT WATCH_FK"},
				{"rename-column", "ALTER TABLE WATCH_A ALTER GEN_VALUE TO GEN_RENAMED"},
				{"drop-procedure", "DROP PROCEDURE WATCH_P1"},
				{"eligibility-excluded", "UPDATE RDB$PROCEDURES SET RDB$SYSTEM_FLAG=1 WHERE RDB$PROCEDURE_NAME='WATCH_P2'"},
				{"eligibility-restored", "UPDATE RDB$PROCEDURES SET RDB$SYSTEM_FLAG=0 WHERE RDB$PROCEDURE_NAME='WATCH_P2'"},
				{"same-eligibility-null", "UPDATE RDB$PROCEDURES SET RDB$SYSTEM_FLAG=NULL WHERE RDB$PROCEDURE_NAME='WATCH_P2'"},
				{"dependency-only", "UPDATE RDB$DEPENDENCIES SET RDB$FIELD_NAME='C' WHERE RDB$DEPENDENT_NAME='WATCH_GRAPH' AND RDB$FIELD_NAME='ID'"},
			}
			for _, step := range steps {
				if dialect == 1 && step.name == "quoted-name" {
					continue
				}
				ok := t.Run(step.name, func(t *testing.T) {
					if step.name == "view-context" {
						exec(t, "DROP VIEW WATCH_V")
					}
					if step.name == "dependency-only" {
						var fields []string
						baseline.VisitRecords(func(row schema.FingerprintRecord) bool {
							if row.Family == "RDB$DEPENDENCIES" && len(row.Owners) > 0 && row.Owners[0].Name == "WATCH_GRAPH" {
								for _, link := range row.Links {
									fields = append(fields, link.Field)
								}
							}
							return true
						})
						if len(fields) == 0 {
							t.Fatal("native dependency fixture is empty")
						}
					}
					exec(t, step.sql)
					candidate := scan()
					delta, err := repo.ReadWatchedDelta(ctx, base, baseline, candidate)
					if err != nil {
						t.Fatal("delta", err)
					}
					var got *catalog.Cache
					if delta.FullReason != "" {
						allowed := delta.FullReason == "visibility" && (step.name == "view-context" || step.name == "drop-procedure") || delta.FullReason == "mapping" && strings.HasPrefix(step.name, "eligibility-")
						if !allowed || len(delta.Patches) != 0 || len(delta.Owners) != 0 || len(delta.Deleted) != 0 || len(delta.DependencyOwners) != 0 || len(delta.DependencyEdges) != 0 {
							t.Fatalf("unexpected full fallback: %s", delta.FullReason)
						}
						got = load()
						t.Logf("required full-safe selection reason=%s, no partial fragments", delta.FullReason)
					} else {
						got, err = metadata.MergeObjects(ctx, base, delta)
						if err != nil {
							t.Fatal("merge", err)
						}
					}
					if step.name == "dependency-only" {
						if delta.Affected != 0 || delta.FullReason != "" || len(delta.Owners) != 0 || len(delta.Patches) != 0 || len(delta.DependencyOwners) == 0 || delta.Metrics.Queries != 0 {
							t.Fatalf("native dependency-only selection affected=%d reason=%s owners=%d patches=%d groups=%d queries=%d fingerprint-equal=%t", delta.Affected, delta.FullReason, len(delta.Owners), len(delta.Patches), len(delta.DependencyOwners), delta.Metrics.Queries, baseline.Equal(candidate))
						}
						t.Log("native dependency-only graph parity: affected=0, descriptor SQL queries=0")
					}
					want := load()
					assertWatchedDeltaParity(t, got, want)
					loader := metadata.NewLoader(nil)
					loader.Reset(1)
					job, err := loader.Start(ctx, 1, repo.MetadataPlan())
					if err != nil {
						loader.Stop()
						t.Fatal(err)
					}
					select {
					case <-job.Done:
					case <-ctx.Done():
						loader.Stop()
						t.Fatal(ctx.Err())
					}
					snapshot := loader.Snapshot()
					loader.Stop()
					if snapshot.Degraded() {
						t.Fatal("independent full cache degraded")
					}
					assertWatchedDeltaParity(t, got, snapshot.Cache)
					if step.name == "body-only" {
						if len(delta.Owners[catalog.MetadataProcedures]) != 1 || delta.Owners[catalog.MetadataProcedures][0].Name != "WATCH_P1" {
							t.Fatal("body changed unrelated owner")
						}
						var records []watchedRecordedQuery
						patch, err := repo.runWatchedRead(ctx, func(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
							r := &watchedNativeRecorder{q: q}
							p, e := repo.readWatchedCategoryOwners(ctx, r, width, catalog.MetadataProcedures, delta.Owners[catalog.MetadataProcedures])
							records = r.queries
							return p, e
						})
						if err != nil {
							t.Fatal(err)
						}
						for _, query := range records {
							if strings.Contains(query.sql, "RDB$PROCEDURE_SOURCE") {
								if len(query.args) != 1 || query.args[0] != "WATCH_P1" || !strings.Contains(query.sql, "p.RDB$PROCEDURE_NAME IN (?)") {
									t.Fatal("unbounded native source query")
								}
							}
						}
						p, ok := patch.Cache.ProcedureByID("WATCH_P1")
						if !ok || !strings.Contains(p.Source.String, "A + 1") {
							t.Fatal("changed source missing")
						}
						if _, ok := patch.Cache.ProcedureByID("WATCH_P2"); ok {
							t.Fatal("unchanged source fetched")
						}
						t.Logf("source selection WATCH_P1 only: fetched source bytes=%d; WATCH_P2 bytes in full cache=%d; source query exact one bound name", len(p.Source.String), len(base.Catalog.ProceduresByID[interBaseByIDKey(ObjectKindProcedure, "WATCH_P2")].Source.String))
					}
					if conn.Stats().InUse != 0 {
						t.Fatal("native work not drained")
					}
					t.Logf("incremental parity affected=%d categories=%d queries=%d", delta.Affected, len(delta.Patches), delta.Metrics.Queries)
					baseline, base = candidate, got
				})
				if !ok {
					return
				}
			}
		})
	}
}

func assertWatchedDeltaParity(t *testing.T, got, want *catalog.Cache) {
	t.Helper()
	a, b := *got, *want
	normalize := func(g *catalog.DependencyGraph) *catalog.DependencyGraph {
		edges := g.Edges()
		sort.Slice(edges, func(i, j int) bool { return fmt.Sprint(edges[i]) < fmt.Sprint(edges[j]) })
		checks := g.CheckNames()
		for k, v := range checks {
			if len(v) == 0 {
				delete(checks, k)
			}
		}
		return catalog.NewDependencyGraph(edges, checks)
	}
	a.Dependencies, b.Dependencies = normalize(a.Dependencies), normalize(b.Dependencies)
	if !reflect.DeepEqual(a.Dependencies, b.Dependencies) {
		am, bm := map[catalog.DependencyEdge]bool{}, map[catalog.DependencyEdge]bool{}
		for _, edge := range a.Dependencies.Edges() {
			am[edge] = true
		}
		for _, edge := range b.Dependencies.Edges() {
			bm[edge] = true
		}
		for edge := range am {
			if !bm[edge] {
				t.Logf("incremental-only edge=%+v", edge)
			}
		}
		for edge := range bm {
			if !am[edge] {
				t.Logf("full-only edge=%+v", edge)
			}
		}
		if !reflect.DeepEqual(a.Dependencies.CheckNames(), b.Dependencies.CheckNames()) {
			t.Log("check-name membership differs")
		}
	}
	for _, pair := range [][2]any{{a, b}, {*a.Catalog, *b.Catalog}} {
		av, bv := reflect.ValueOf(pair[0]), reflect.ValueOf(pair[1])
		for i := 0; i < av.NumField(); i++ {
			if av.Type().Field(i).Name == "Objects" || av.Type().Field(i).Name == "Catalog" {
				continue
			}
			if !reflect.DeepEqual(av.Field(i).Interface(), bv.Field(i).Interface()) {
				t.Logf("parity difference field=%s", av.Type().Field(i).Name)
			}
		}
	}
	assertWatchedCacheParity(t, &a, &b)
}
