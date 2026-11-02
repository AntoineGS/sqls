package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sqls-server/sqls/catalog"
)

// DuckDBMetadataRepository publishes metadata for every user schema in one
// catalog. Conn is supplied by the host and every category uses a short,
// explicit read-only transaction on a pinned connection.
type DuckDBMetadataRepository struct {
	Conn    func(context.Context) (*sql.Conn, error)
	Catalog string
}

type duckDBMetadataRead func(context.Context, *sql.Conn) (catalog.MetadataPatch, error)

func (db *DuckDBMetadataRepository) withMetadataRead(ctx context.Context, read duckDBMetadataRead) (patch catalog.MetadataPatch, err error) {
	if db == nil || db.Conn == nil {
		return catalog.MetadataPatch{}, errors.New("duckdb: metadata connection source is nil")
	}
	if db.Catalog == "" {
		return catalog.MetadataPatch{}, errors.New("duckdb: metadata catalog is empty")
	}
	if ctx == nil {
		return catalog.MetadataPatch{}, errors.New("duckdb: metadata context is nil")
	}
	if read == nil {
		return catalog.MetadataPatch{}, errors.New("duckdb: metadata read is nil")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return catalog.MetadataPatch{}, fmt.Errorf("duckdb: acquire metadata connection: %w", err)
	}
	if conn == nil {
		return catalog.MetadataPatch{}, errors.New("duckdb: metadata connection source returned nil connection")
	}
	transactionStarted := false
	committed := false
	defer func() {
		if transactionStarted && !committed {
			if _, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK"); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("duckdb: rollback metadata read: %w", rollbackErr))
			}
		}
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("duckdb: close metadata connection: %w", closeErr))
		}
		if err != nil {
			patch.Cache = nil
		}
	}()

	if _, err = conn.ExecContext(ctx, "BEGIN TRANSACTION READ ONLY"); err != nil {
		return catalog.MetadataPatch{}, fmt.Errorf("duckdb: begin metadata read: %w", err)
	}
	transactionStarted = true
	patch, err = read(ctx, conn)
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = errors.Join(err, ctxErr)
	}
	if err != nil {
		return catalog.MetadataPatch{}, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return catalog.MetadataPatch{}, fmt.Errorf("duckdb: commit metadata read: %w", err)
	}
	committed = true
	return patch, nil
}

// duckDBMetadataRows executes a catalog query and fails the whole category on
// query, scan, iteration, close, or context errors.
func duckDBMetadataRows(ctx context.Context, conn *sql.Conn, query string, args []any, scan func(*sql.Rows) error) (err error) {
	if conn == nil {
		return errors.New("duckdb: metadata connection is nil")
	}
	if scan == nil {
		return errors.New("duckdb: metadata row scanner is nil")
	}
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("duckdb: query metadata: %w", err)
	}
	if rows == nil {
		return errors.New("duckdb: metadata query returned nil rows")
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("duckdb: close metadata rows: %w", closeErr))
		}
	}()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return fmt.Errorf("duckdb: scan metadata row: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("duckdb: iterate metadata rows: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
