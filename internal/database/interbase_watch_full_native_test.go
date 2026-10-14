//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sqls-server/sqls/metadata"
)

// Only the disposable test wrapper sets this gate. Never use live catalog
// configuration for full-cache acceptance.
func TestWatchedFullDisposableNativeParity(t *testing.T) {
	if os.Getenv("SQLS_WATCH_DISPOSABLE") != "1" {
		t.Skip("disposable fixture wrapper required")
	}
	for _, dialect := range []int{1, 3} {
		t.Run(fmt.Sprintf("dialect-%d", dialect), func(t *testing.T) {
			target := os.Getenv("SQLS_WATCH_DISPOSABLE_DATABASE")
			if dialect == 1 {
				target = os.Getenv("SQLS_WATCH_DISPOSABLE_DATABASE_D1")
			}
			if target == "" {
				t.Fatal("missing disposable fixture target")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			conn, err := AttachInterBaseForMetadata(ctx, InterBaseMetadataAttachConfig{Database: target, User: os.Getenv("SQLS_WATCH_DISPOSABLE_USER"), Password: os.Getenv("SQLS_WATCH_DISPOSABLE_PASSWORD"), Charset: "NONE", CatalogTextCharset: "WIN1252"}, dialect)
			if err != nil {
				t.Fatal("disposable attach failed")
			}
			t.Cleanup(func() { conn.Close() })
			conn.SetMaxOpenConns(1)
			repo := &InterBaseDBRepository{Conn: conn, SQLDialect: dialect, SourceSQLDialect: dialect}
			loader := metadata.NewLoader(nil)
			t.Cleanup(loader.Stop)
			loader.Reset(1)
			load, err := loader.Start(ctx, 1, repo.MetadataPlan())
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-load.Done:
			case <-ctx.Done():
				t.Fatal("independent load timed out")
			}
			want := loader.Snapshot()
			if want.Degraded() {
				t.Fatal("independent fixture load degraded")
			}
			got, status, metrics, err := repo.ReadWatchedFull(ctx)
			if err != nil {
				t.Fatal("watched fixture load failed")
			}
			assertWatchedCacheParity(t, got, want.Cache)
			for kind, s := range want.Status {
				g, ok := status[kind]
				if !ok || g.State != s.State || g.Count != s.Count || g.QueriesKnown != s.QueriesKnown {
					t.Fatalf("category=%s state/count/query-known parity failed", kind)
				}
			}
			if conn.Stats().InUse != 0 || metrics.Queries <= 0 {
				t.Fatal("native groups did not release pool/account queries")
			}
			if dialect == 3 {
				domain, ok := got.DomainByID("WATCH_TEXT")
				if !ok || !domain.DefaultSource.Valid || !strings.Contains(domain.DefaultSource.String, "café") || strings.Contains(domain.DefaultSource.String, "�") {
					t.Fatal("NONE/CP1252 source fidelity failed")
				}
				for _, name := range []string{"WatchFold", "WATCHFOLD"} {
					id := interBaseByIDKey(ObjectKindTable, name)
					if len(got.ColumnsByID[id]) == 0 {
						t.Fatal("exact relation identity missing")
					}
				}
				if _, ok := got.ProcedureByID("MDE_PROC_OUT"); !ok {
					t.Fatal("full replacement omitted procedures")
				}
			} else {
				columns := got.ColumnsWithParent[columnDatabaseKey("", "MDE_D1_COMPAT")]
				found := false
				for _, c := range columns {
					if c.Name == "DATE_VALUE" {
						found = true
						if c.Type != "DATE" {
							t.Fatal("dialect-1 date rendering lost source dialect")
						}
					}
				}
				if !found {
					t.Fatal("dialect-1 fixture columns missing")
				}
			}
			t.Logf("dialect=%d complete-cache parity; categories=%d queries=%d pool-in-use=%d", dialect, len(status), metrics.Queries, conn.Stats().InUse)
		})
	}
}
