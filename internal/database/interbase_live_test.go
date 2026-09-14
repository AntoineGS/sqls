//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
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
