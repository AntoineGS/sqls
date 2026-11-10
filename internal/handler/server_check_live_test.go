//go:build interbase && cgo && linux && amd64

package handler

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
)

func TestInterBaseLiveServerCheckParsesProcedureBodyPosition(t *testing.T) {
	databaseName := os.Getenv("INTERBASE_DATABASE")
	user := os.Getenv("INTERBASE_USER")
	password, passwordSet := os.LookupEnv("INTERBASE_PASSWORD")
	if databaseName == "" || user == "" || !passwordSet {
		t.Skip("set INTERBASE_DATABASE, INTERBASE_USER, and INTERBASE_PASSWORD to run the live InterBase test")
	}

	connection, err := database.Open(&database.DBConfig{
		Driver:         dialect.DatabaseDriverInterBase,
		DataSourceName: databaseName,
		User:           user,
		Passwd:         password,
		Dialect:        1,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	statement := "CREATE PROCEDURE SQLS_L596_BROKEN AS\nBEGIN\n  _SELECT 1 FROM RDB$DATABASE;\nEND"
	got, err := (interBaseChecker{db: connection.Conn}).CheckStatement(ctx, statement)
	if err != nil {
		t.Fatalf("CheckStatement() error = %v", err)
	}
	if got == nil {
		t.Fatal("CheckStatement() = nil, want server syntax finding")
	}
	if got.SQLCode != -104 || got.Line != 3 || got.Column != 3 || got.Token != "_SELECT" {
		t.Fatalf("CheckStatement() error = %+v, want SQLCODE -104 at line 3, column 3, token _SELECT", got)
	}
}
