//go:build interbase && cgo && (linux || windows) && amd64

package database

import (
	"context"
	"database/sql"
	"errors"

	interbase "interbase-go"
)

// PrepareCheckInterBase prepares query on a pooled connection inside a
// read-only transaction that is always rolled back. It never executes query.
func PrepareCheckInterBase(ctx context.Context, db *sql.DB, query string) (serverRejected bool, sqlCode int, message string, err error) {
	if db == nil {
		return false, 0, "", errors.New("interbase: nil database")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, 0, "", err
	}
	defer func() { _ = conn.Close() }()

	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, 0, "", err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, query)
	if err == nil {
		_ = stmt.Close()
		return false, 0, "", nil
	}
	var ibErr *interbase.Error
	if ctx.Err() == nil && errors.As(err, &ibErr) && interBaseSQLRejection(ibErr.SQLCode) {
		return true, ibErr.SQLCode, ibErr.Message, nil
	}
	return false, 0, "", err
}
