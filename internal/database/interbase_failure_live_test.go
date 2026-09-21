//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
)

// TestInterBaseLiveQueryCancellationReturnsTypedError cancels a deliberately
// slow catalog cross join and asserts the driver reports a typed cancellation
// outcome rather than an opaque error. The connection setup mirrors
// TestInterBaseLiveReadOnlyCatalog in interbase_live_test.go.
func TestInterBaseLiveQueryCancellationReturnsTypedError(t *testing.T) {
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

	const slowQuery = `SELECT COUNT(*) FROM RDB$RELATION_FIELDS a, RDB$RELATION_FIELDS b, RDB$RELATION_FIELDS c`

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(250 * time.Millisecond)
		cancel()
	}()

	rows, err := connection.Conn.QueryContext(ctx, slowQuery)
	if err == nil {
		_ = rows.Close()
		t.Skip("the cross join completed before the cancellation took effect")
	}

	// Cancellation is best effort and a stalled call can outlive its context,
	// but when the statement does fail after a cancellation the failure must be
	// typed, never opaque.
	if kind, _ := ClassifyFailure(err); kind == FailureNone {
		t.Fatalf("ClassifyFailure(%v) = FailureNone, want FailureCanceled or FailureUncertain", err)
	}
}
