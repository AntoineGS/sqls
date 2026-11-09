//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestInterBaseLivePrepareCheckIsReadOnlyAndReportsServerSyntax(t *testing.T) {
	connection, err := Open(interBaseLiveConfig(t, 1))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO RDB$DATABASE (RDB$DB_KEY) VALUES (",
		"CREATE PROCEDURE SQLS_L596_BROKEN AS\nBEGIN\n  _SELECT 1 FROM RDB$DATABASE;\nEND",
	} {
		rejected, code, message, err := PrepareCheckInterBase(ctx, connection.Conn, statement)
		if err != nil {
			t.Fatalf("PrepareCheckInterBase(%q) error = %v", statement, err)
		}
		if !rejected || code != -104 || !strings.Contains(message, "Token unknown") {
			t.Errorf("PrepareCheckInterBase(%q) = (%v, %d, %q), want SQLCODE -104 token rejection", statement, rejected, code, message)
		}
	}

	procedureName := fmt.Sprintf("SQLS_L596_%d", time.Now().UnixNano())
	statement := fmt.Sprintf("CREATE PROCEDURE %s AS BEGIN SUSPEND; END", procedureName)
	rejected, code, message, err := PrepareCheckInterBase(ctx, connection.Conn, statement)
	if err != nil {
		t.Fatalf("PrepareCheckInterBase(valid procedure) error = %v", err)
	}
	if rejected {
		t.Fatalf("valid procedure was rejected with SQLCODE %d: %s", code, message)
	}
	var count int
	if err := connection.Conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM RDB$PROCEDURES WHERE RDB$PROCEDURE_NAME = ?", procedureName).Scan(&count); err != nil {
		t.Fatalf("query RDB$PROCEDURES after prepare = %v", err)
	}
	if count != 0 {
		t.Fatalf("read-only prepare created %d procedure rows for %s, want none", count, procedureName)
	}
}
