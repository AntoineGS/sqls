package fbmeta

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	firebirdsql "github.com/nakagami/firebirdsql"
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
)

var seed = []string{
	"RECREATE TABLE FB_CUSTOMER (ID INTEGER NOT NULL PRIMARY KEY, NAME VARCHAR(40))",
	"RECREATE GLOBAL TEMPORARY TABLE FB_GTT (ID INTEGER) ON COMMIT PRESERVE ROWS",
	"CREATE DOMAIN FB_DOMAIN AS VARCHAR(40)",
	"CREATE EXCEPTION FB_EXCEPTION 'Firebird metadata test'",
	"CREATE OR ALTER VIEW FB_CUSTOMER_VIEW AS SELECT ID, NAME FROM FB_CUSTOMER",
	"CREATE INDEX FB_CUSTOMER_NAME_IX ON FB_CUSTOMER (NAME)",
	"CREATE OR ALTER PROCEDURE FB_PROC (P_ID TYPE OF COLUMN FB_CUSTOMER.ID) RETURNS (N VARCHAR(40)) AS BEGIN N = 'caf\u00e9'; SUSPEND; END",
	"CREATE OR ALTER TRIGGER FB_BIU FOR FB_CUSTOMER ACTIVE BEFORE INSERT OR UPDATE POSITION 0 AS BEGIN END",
	"CREATE OR ALTER TRIGGER FB_ON_CONNECT ACTIVE ON CONNECT POSITION 0 AS BEGIN END",
	"CREATE SEQUENCE FB_SEQ",
}

func TestLiveProviderLoadsFirebird25Catalog(t *testing.T) {
	attach := os.Getenv("FIREBIRD25_TEST_ATTACH")
	if attach == "" {
		t.Skip("FIREBIRD25_TEST_ATTACH is not set")
	}
	ctx := context.Background()
	dsn, err := DriverDSN(attach, "SYSDBA", "masterkey", "", 3, url.Values{"none_text_fallback": {"WIN1252"}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("firebirdsql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range seed {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			_ = tx.Rollback()
			t.Fatalf("%s: %v", statement, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	registry := metadata.NewRegistry(ctx, metadata.Options{})
	defer registry.Close(ctx)
	cfg := Config{Target: "fb://" + attach, User: "SYSDBA", Password: "masterkey", Dialect: 3}
	key, err := KeyFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := ScopeFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := registry.Acquire(ctx, key, scope, Factory(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	snapshot := waitForReadyCache(t, ctx, lease)
	for kind, status := range snapshot.Status {
		if status.Err != nil && (errors.Is(status.Err, firebirdsql.ErrImplicitTransaction) || strings.Contains(status.Err.Error(), firebirdsql.ErrImplicitTransaction.Error())) {
			t.Errorf("%s surfaced ErrImplicitTransaction: %v", kind, status.Err)
		}
	}
	if snapshot.StartFailed {
		t.Fatal("metadata load failed to start")
	}
	for _, kind := range []catalog.MetadataKind{
		catalog.MetadataSchemas,
		catalog.MetadataRelations,
		catalog.MetadataColumnsCurrent,
		catalog.MetadataProcedures,
		catalog.MetadataPrimaryKeys,
		catalog.MetadataViews,
		catalog.MetadataDependencies,
		catalog.MetadataIndexes,
		catalog.MetadataForeignKeys,
		catalog.MetadataFunctions,
		catalog.MetadataGenerators,
		catalog.MetadataSystemRelations,
		catalog.MetadataExceptions,
		catalog.MetadataDomains,
		catalog.MetadataTriggers,
	} {
		status, ok := snapshot.Status[kind]
		if !ok || status.State != catalog.MetadataReady {
			t.Errorf("%s state = %s (error: %v), want ready", kind, status.State, status.Err)
		}
	}
	cache := snapshot.Cache
	for _, object := range []struct {
		kind catalog.ObjectKind
		name string
	}{
		{catalog.ObjectKindTable, "FB_CUSTOMER"},
		{catalog.ObjectKindTable, "FB_GTT"},
		{catalog.ObjectKindView, "FB_CUSTOMER_VIEW"},
		{catalog.ObjectKindProcedure, "FB_PROC"},
		{catalog.ObjectKindTrigger, "FB_BIU"},
		{catalog.ObjectKindTrigger, "FB_ON_CONNECT"},
		{catalog.ObjectKindGenerator, "FB_SEQ"},
		{catalog.ObjectKindException, "FB_EXCEPTION"},
		{catalog.ObjectKindDomain, "FB_DOMAIN"},
		{catalog.ObjectKindIndex, "FB_CUSTOMER_NAME_IX"},
	} {
		if !cacheHasObject(cache, object.name) || !cacheHasObjectOfKind(cache, object.kind, object.name) {
			t.Errorf("catalog lacks %s %s", object.kind, object.name)
		}
	}
	ddl := detailText(t, ctx, lease, catalog.ObjectKindProcedure, "FB_PROC", "ddl")
	if !strings.Contains(ddl, "café") {
		t.Errorf("procedure DDL lost CP1252 text: %q", ddl)
	}
	trigger := detailText(t, ctx, lease, catalog.ObjectKindTrigger, "FB_BIU", "ddl")
	if !strings.Contains(trigger, "BEFORE INSERT OR UPDATE") {
		t.Errorf("trigger DDL = %q", trigger)
	}
	databaseTrigger := detailText(t, ctx, lease, catalog.ObjectKindTrigger, "FB_ON_CONNECT", "ddl")
	if !strings.Contains(databaseTrigger, "ON CONNECT") {
		t.Errorf("database trigger DDL = %q", databaseTrigger)
	}
}

func waitForReadyCache(t *testing.T, ctx context.Context, lease *metadata.Lease) *catalog.MetadataSnapshot {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	subscription := lease.Subscribe()
	defer subscription.Close()
	for {
		snapshot := lease.Snapshot()
		if snapshot.Settled() {
			return snapshot
		}
		select {
		case _, ok := <-subscription.C:
			if !ok {
				t.Fatal("metadata event stream closed before the catalog settled")
			}
		case <-waitCtx.Done():
			t.Fatalf("metadata catalog did not settle: %v", waitCtx.Err())
		}
	}
}

func cacheHasObject(cache *catalog.Cache, name string) bool {
	if cache == nil {
		return false
	}
	for _, table := range cache.SortedTables() {
		if strings.EqualFold(table, name) {
			return true
		}
	}
	for _, kind := range []catalog.ObjectKind{
		catalog.ObjectKindView,
		catalog.ObjectKindProcedure,
		catalog.ObjectKindTrigger,
		catalog.ObjectKindGenerator,
		catalog.ObjectKindException,
		catalog.ObjectKindDomain,
		catalog.ObjectKindFunction,
		catalog.ObjectKindIndex,
	} {
		if cacheHasObjectOfKind(cache, kind, name) {
			return true
		}
	}
	return false
}

func cacheHasObjectOfKind(cache *catalog.Cache, kind catalog.ObjectKind, name string) bool {
	if cache == nil {
		return false
	}
	if kind == catalog.ObjectKindTable {
		for _, table := range cache.SortedTables() {
			if strings.EqualFold(table, name) {
				return true
			}
		}
		return false
	}
	_, found, _ := cache.ResolveObjectName(kind, name)
	return found
}

func detailText(t *testing.T, ctx context.Context, lease *metadata.Lease, kind catalog.ObjectKind, name, facet string) string {
	t.Helper()
	detail, err := lease.Detail(ctx, metadata.Request{
		Object:      catalog.ObjectID{Kind: kind, Name: name},
		Facet:       facet,
		Dialect:     3,
		Consistency: metadata.Committed,
	})
	if err != nil {
		t.Fatalf("Detail(%s %s %s): %v", kind, name, facet, err)
	}
	return detail.Text
}
