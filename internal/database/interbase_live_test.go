//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
	interbase "interbase-go"
)

func TestInterBaseNativeOpenValidatesConfigBeforeDial(t *testing.T) {
	_, err := Open(&DBConfig{Driver: dialect.DatabaseDriverInterBase, Path: "/tmp/example.ib"})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "user") {
		t.Fatalf("Open() error = %v, want validation to reject the missing user", err)
	}
}

func TestInterBaseLiveReadOnlyCatalog(t *testing.T) {
	databaseName := os.Getenv("INTERBASE_DATABASE")
	user := os.Getenv("INTERBASE_USER")
	password, passwordSet := os.LookupEnv("INTERBASE_PASSWORD")
	if databaseName == "" || user == "" || !passwordSet {
		t.Skip("set INTERBASE_DATABASE, INTERBASE_USER, and INTERBASE_PASSWORD to run the live InterBase test")
	}

	connection, err := Open(&DBConfig{
		Driver:         dialect.DatabaseDriverInterBase,
		DataSourceName: databaseName,
		User:           user,
		Passwd:         password,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	repository := NewInterBaseDBRepository(connection.Conn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := repository.SchemaTables(ctx); err != nil {
		t.Fatalf("SchemaTables() error = %v", err)
	}
	if _, err := repository.DescribeDatabaseTable(ctx); err != nil {
		t.Fatalf("DescribeDatabaseTable() error = %v", err)
	}
	if _, err := repository.DescribeForeignKeysBySchema(ctx, ""); err != nil {
		t.Fatalf("DescribeForeignKeysBySchema() error = %v", err)
	}
}

func interBaseLiveConfig(t *testing.T, sqlDialect int) *DBConfig {
	t.Helper()
	databaseName := os.Getenv("INTERBASE_DATABASE")
	user := os.Getenv("INTERBASE_USER")
	password, passwordSet := os.LookupEnv("INTERBASE_PASSWORD")
	if databaseName == "" || user == "" || !passwordSet {
		t.Skip("set INTERBASE_DATABASE, INTERBASE_USER, and INTERBASE_PASSWORD to run the live InterBase test")
	}
	return &DBConfig{
		Alias:          "live",
		Driver:         dialect.DatabaseDriverInterBase,
		DataSourceName: databaseName,
		User:           user,
		Passwd:         password,
		Dialect:        sqlDialect,
	}
}

// interBaseLiveReportedDialect reads the dialect the server reports for a
// connection, so the auto-detect test can self-check without a fixture.
func interBaseLiveReportedDialect(t *testing.T, connection *DBConnection) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := connection.Conn.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn() error = %v", err)
	}
	defer func() { _ = conn.Close() }()

	diagnostics, err := interbase.Diagnostics(ctx, conn)
	if err != nil {
		t.Fatalf("interbase.Diagnostics() error = %v", err)
	}
	return diagnostics.SQLDialect
}

// TestInterBaseLiveDialectAutoDetect must be run against both a Dialect 1 and a
// Dialect 3 database to be meaningful. Set INTERBASE_EXPECT_DIALECT to pin the
// expectation; otherwise the test self-checks against the server's own answer.
func TestInterBaseLiveDialectAutoDetect(t *testing.T) {
	cfg := interBaseLiveConfig(t, 0)

	connection, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	reported := interBaseLiveReportedDialect(t, connection)
	want := int(reported)
	if expected := os.Getenv("INTERBASE_EXPECT_DIALECT"); expected != "" {
		parsed, err := strconv.Atoi(expected)
		if err != nil {
			t.Fatalf("INTERBASE_EXPECT_DIALECT = %q is not a number", expected)
		}
		if parsed != want {
			t.Fatalf("INTERBASE_EXPECT_DIALECT = %d but the server reports %d", parsed, want)
		}
	}

	if got := connection.Variant.InterBaseSQLDialect(); got != want {
		t.Fatalf("auto-detected variant = %q (dialect %d), want dialect %d", connection.Variant, got, want)
	}
	if len(connection.Warnings) != 0 {
		t.Errorf("auto-detect against a %d-dialect database warned: %v", want, connection.Warnings)
	}
	if connection.DatabaseName != cfg.DataSourceName {
		t.Errorf("DatabaseName = %q, want %q", connection.DatabaseName, cfg.DataSourceName)
	}
	t.Logf("server reports SQL dialect %d; resolved variant %q", reported, connection.Variant)
}

func TestInterBaseLiveExplicitDialectMismatchWarnsAndConnects(t *testing.T) {
	probe, err := Open(interBaseLiveConfig(t, 0))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	reported := interBaseLiveReportedDialect(t, probe)
	_ = probe.Close()

	opposite := 1
	if reported == 1 {
		opposite = 3
	}

	cfg := interBaseLiveConfig(t, opposite)
	if cfg.Alias == "" {
		t.Fatal("interBaseLiveConfig must set a non-empty Alias; the alias assertion below is vacuous without one")
	}
	connection, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() with a mismatched dialect must connect, got error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	if got := connection.Variant.InterBaseSQLDialect(); got != opposite {
		t.Errorf("resolved dialect = %d, want the configured %d", got, opposite)
	}
	if len(connection.Warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(connection.Warnings), connection.Warnings)
	}
	warning := connection.Warnings[0]
	for _, mention := range []string{
		fmt.Sprintf("dialect %d", opposite),
		fmt.Sprintf("dialect %d", reported),
	} {
		if !strings.Contains(warning, mention) {
			t.Errorf("warning %q does not mention %q", warning, mention)
		}
	}
	// A user with several connections needs to know which one warned.
	if !strings.Contains(warning, cfg.Alias) {
		t.Errorf("warning %q does not name the connection alias %q", warning, cfg.Alias)
	}
}

func TestExplainRepositoryPresentWithNativeBuild(t *testing.T) {
	repository := DBRepository(&InterBaseDBRepository{})
	if _, ok := repository.(ExplainRepository); !ok {
		t.Error("*InterBaseDBRepository must implement ExplainRepository under the interbase build tag")
	}
}

// interBaseLiveRepository opens the configured live database or skips.
func interBaseLiveRepository(t *testing.T) *InterBaseDBRepository {
	t.Helper()
	databaseName := os.Getenv("INTERBASE_DATABASE")
	user := os.Getenv("INTERBASE_USER")
	password, passwordSet := os.LookupEnv("INTERBASE_PASSWORD")
	if databaseName == "" || user == "" || !passwordSet {
		t.Skip("set INTERBASE_DATABASE, INTERBASE_USER, and INTERBASE_PASSWORD to run the live InterBase test")
	}

	connection, err := Open(&DBConfig{
		Driver:         dialect.DatabaseDriverInterBase,
		DataSourceName: databaseName,
		User:           user,
		Passwd:         password,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	return &InterBaseDBRepository{Conn: connection.Conn, DatabaseName: databaseName}
}

func TestInterBaseLiveCatalogObjectsSurface(t *testing.T) {
	repository := interBaseLiveRepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	generator := NewDBCacheUpdater(repository)

	started := time.Now()
	cache, err := generator.GenerateDBCachePrimary(ctx)
	if err != nil {
		t.Fatalf("GenerateDBCachePrimary() error = %v", err)
	}
	primaryElapsed := time.Since(started)
	tables := cache.SortedTables()
	if len(tables) == 0 {
		t.Fatal("GenerateDBCachePrimary() cached no tables")
	}
	columnCount := 0
	for _, columns := range cache.ColumnsWithParent {
		columnCount += len(columns)
	}
	if columnCount == 0 {
		t.Fatal("GenerateDBCachePrimary() cached no columns")
	}

	started = time.Now()
	catalog, ok, err := generator.GenerateCatalogCache(ctx)
	if err != nil {
		t.Fatalf("GenerateCatalogCache() error = %v", err)
	}
	catalogElapsed := time.Since(started)
	if !ok || catalog == nil {
		t.Fatal("GenerateCatalogCache() reported no extended catalog for an InterBase repository")
	}

	// The measurement that feeds the escalation trigger in the spec's Risk 1:
	// a primary pass above 5s or a catalog pass above 30s means the fix is a
	// bulk projection in the driver's schema package, not hand-written SQL
	// here. Logged rather than asserted, because it is a property of the
	// database under test.
	t.Logf("primary pass: %d tables, %d columns, %d foreign-key tables in %s",
		len(tables), columnCount, len(cache.ForeignKeys), primaryElapsed)
	t.Logf("catalog pass: %d views, %d procedures, %d generators, %d domains, %d indexes, %d triggers, %d functions in %s",
		len(catalog.Views), len(catalog.Procedures), len(catalog.Generators), len(catalog.Domains),
		len(catalog.Indexes), len(catalog.Triggers), len(catalog.Functions), catalogElapsed)
}

func TestInterBaseLiveObjectDDL(t *testing.T) {
	repository := interBaseLiveRepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	// Both outcomes are expected in a real database: computed columns block
	// table DDL and unknown parameter nullability blocks procedure DDL.
	assertRenderedOrExplained := func(t *testing.T, kind ObjectKind, name, wantPrefix string) {
		t.Helper()
		ddl, err := repository.ObjectDDL(ctx, kind, name)
		switch {
		case err == nil:
			if !strings.HasPrefix(strings.TrimSpace(ddl), wantPrefix) {
				t.Errorf("ObjectDDL(%s, %q) = %q, want it to start with %q", kind, name, ddl, wantPrefix)
			}
		case errors.Is(err, ErrUnsupportedDDL):
			object, detailName, feature, ok := UnsupportedDDLDetail(err)
			t.Logf("ObjectDDL(%s, %q) is unavailable: object=%q name=%q feature=%q detailed=%v",
				kind, name, object, detailName, feature, ok)
		default:
			t.Errorf("ObjectDDL(%s, %q) error = %v, want nil or ErrUnsupportedDDL", kind, name, err)
		}
	}

	views, err := repository.DescribeViews(ctx)
	if err != nil {
		t.Fatalf("DescribeViews() error = %v", err)
	}
	tables, err := repository.SchemaTables(ctx)
	if err != nil {
		t.Fatalf("SchemaTables() error = %v", err)
	}
	viewNames := map[string]bool{}
	for _, view := range views {
		viewNames[view.Name] = true
	}
	testedTable := false
	for _, name := range tables[""] {
		if viewNames[name] {
			continue
		}
		assertRenderedOrExplained(t, ObjectKindTable, name, "CREATE TABLE")
		testedTable = true
		break
	}
	if !testedTable {
		t.Fatal("SchemaTables() returned no non-view table to exercise ObjectDDL(table, ...) against")
	}

	procedures, err := repository.DescribeProcedures(ctx)
	if err != nil {
		t.Fatalf("DescribeProcedures() error = %v", err)
	}
	if len(procedures) == 0 {
		t.Fatal("DescribeProcedures() returned no procedures to exercise ObjectDDL(procedure, ...) against")
	}
	assertRenderedOrExplained(t, ObjectKindProcedure, procedures[0].Name, "CREATE PROCEDURE")

	if _, err := repository.ObjectDDL(ctx, ObjectKindTable, "SQLS_NO_SUCH_TABLE_XYZ"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("ObjectDDL(table, unknown) error = %v, want ErrObjectNotFound", err)
	}
}

func TestInterBaseLiveExplainPlan(t *testing.T) {
	repository := interBaseLiveRepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plan, err := repository.ExplainPlan(ctx, "SELECT RDB$RELATION_ID FROM RDB$DATABASE")
	if err != nil {
		t.Fatalf("ExplainPlan() error = %v", err)
	}
	if strings.TrimSpace(plan) == "" {
		t.Fatal("ExplainPlan() returned empty plan text")
	}
	t.Logf("plan: %s", plan)
}
