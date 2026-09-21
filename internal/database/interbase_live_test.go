//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
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

func TestInterBaseCharsetAllowlistMatchesDriverNormalizer(t *testing.T) {
	// NewConnector validates and returns without dialing (interbase.go:114-139),
	// so this test needs no server. It fails the moment the driver's allowlist and
	// interBaseCharsets disagree in either direction.
	for _, charset := range interBaseCharsets {
		if _, err := interbase.NewConnector(interbase.Config{
			Database: "/tmp/sqls-allowlist.ib",
			User:     "sqls",
			Charset:  charset,
		}); err != nil {
			t.Errorf("driver rejected charset %q that sqls accepts: %v", charset, err)
		}
	}
	if _, err := interbase.NewConnector(interbase.Config{
		Database: "/tmp/sqls-allowlist.ib",
		User:     "sqls",
		Charset:  "LATIN1",
	}); err == nil {
		t.Error("driver accepted charset LATIN1 that sqls rejects; the allowlists have drifted")
	}
}
