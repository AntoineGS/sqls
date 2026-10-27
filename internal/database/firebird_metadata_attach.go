package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/nakagami/firebirdsql"
)

const firebirdDefaultConnectTimeout = 10 * time.Second

// AttachFirebirdForMetadata opens a pooled, store-owned Firebird attachment
// for the shared metadata provider and pings it. dsn must come from
// fbmeta.DriverDSN, which forces NONE, explicit transactions and SQL type
// names.
func AttachFirebirdForMetadata(ctx context.Context, dsn string, connectTimeout time.Duration) (*sql.DB, error) {
	conn, err := sql.Open("firebirdsql", dsn)
	if err != nil {
		return nil, fmt.Errorf("firebird: open: %w", err)
	}
	conn.SetMaxIdleConns(DefaultMaxIdleConns)
	conn.SetMaxOpenConns(DefaultMaxOpenConns)
	if connectTimeout <= 0 {
		connectTimeout = firebirdDefaultConnectTimeout
	}
	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := conn.PingContext(pingCtx); err != nil {
		_ = conn.Close()
		if parentErr := ctx.Err(); parentErr != nil {
			return nil, parentErr
		}
		return nil, fmt.Errorf("firebird: ping failed: %w", err)
	}
	return conn, nil
}
