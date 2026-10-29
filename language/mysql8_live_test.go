package language_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
	mymeta "github.com/sqls-server/sqls/metadata/mysql"
)

func TestMySQL8LiveLanguageConsumesProviderSnapshot(t *testing.T) {
	cfg := mysql8LanguageLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	key, err := mymeta.KeyFor(cfg)
	if err != nil {
		t.Fatalf("KeyFor() error = %v", err)
	}
	scope, err := mymeta.ScopeFor(cfg)
	if err != nil {
		t.Fatalf("ScopeFor() error = %v", err)
	}
	registry := metadata.NewRegistry(ctx, metadata.Options{})
	defer func() {
		if err := registry.Close(context.Background()); err != nil {
			t.Errorf("Registry.Close() error = %v", err)
		}
	}()
	lease, err := registry.Acquire(ctx, key, scope, mymeta.Factory(cfg))
	if err != nil {
		t.Fatalf("Registry.Acquire() error = %v", err)
	}
	defer lease.Release()
	if err := metadatatest.WaitReady(ctx, lease); err != nil {
		t.Fatalf("metadata did not settle: %v", err)
	}
	snapshot := lease.Snapshot()
	if snapshot == nil || snapshot.Cache == nil || snapshot.StartFailed {
		t.Fatalf("metadata snapshot = %#v, want a started snapshot with cache", snapshot)
	}

	variant := dialect.DriverVariant{Driver: dialect.DatabaseDriverMySQL8}
	request := language.Request{
		Document: language.Document{URI: "memory:mysql8-live", Text: "SELECT * FROM ", Version: 1},
		Context:  language.Context{Snapshot: snapshot, Variant: variant, DialectResolved: true, Details: lease},
	}
	result, err := language.NewService().Complete(ctx, request, language.Position{UTF16Column: 14})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	found := false
	for _, table := range snapshot.Cache.SortedTables() {
		for _, item := range result.Value.Items {
			if item.Label == table {
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatalf("completion returned no table from live snapshot %q: %+v", snapshot.Cache.SortedTables(), result.Value.Items)
	}

	diagnosticsRequest := language.Request{
		Document: language.Document{URI: "memory:mysql8-live/diagnostics", Text: "SHOW TABLES", Version: 1},
		Context:  language.Context{Snapshot: snapshot, Variant: variant, DialectResolved: true, Details: lease},
	}
	diagnostics, err := language.NewService().Diagnostics(ctx, diagnosticsRequest)
	if err != nil {
		t.Fatalf("Diagnostics(SHOW TABLES) error = %v", err)
	}
	for _, diagnostic := range diagnostics.Value {
		if diagnostic.Severity == 1 {
			t.Errorf("SHOW TABLES produced error-severity diagnostic: %+v", diagnostic)
		}
	}
}

func mysql8LanguageLiveConfig(t *testing.T) mymeta.Config {
	t.Helper()
	if os.Getenv("MYSQL8_TEST_DSN") == "" {
		t.Skip("MYSQL8_TEST_DSN is not set")
	}
	addr := os.Getenv("MYSQL8_TEST_ADDR")
	user := os.Getenv("MYSQL8_TEST_USER")
	password := os.Getenv("MYSQL8_TEST_PASSWORD")
	schema := os.Getenv("MYSQL8_TEST_SCHEMA")
	if addr == "" || user == "" || schema == "" {
		t.Fatal("MYSQL8_TEST_ADDR, MYSQL8_TEST_USER, and MYSQL8_TEST_SCHEMA must be set when MYSQL8_TEST_DSN is set")
	}
	return mymeta.Config{Target: fmt.Sprintf("%s@%s/%s", user, addr, schema), User: user, Password: password}
}
