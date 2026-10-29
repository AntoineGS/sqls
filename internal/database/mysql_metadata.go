package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sqls-server/sqls/catalog"
)

// MySQLMetadataRepository publishes metadata for the one schema selected by
// the MySQL connection target. Each category owns its short read transaction.
type MySQLMetadataRepository struct {
	Conn   *sql.DB
	Schema string
}

type mysqlMetadataRead func(context.Context, *sql.Tx) (catalog.MetadataPatch, error)
type mysqlMetadataSchemaRead func(context.Context, *sql.Tx, string) (catalog.MetadataPatch, error)

func (db *MySQLMetadataRepository) withSchemaMetadataRead(ctx context.Context, cache *catalog.Cache, read mysqlMetadataSchemaRead) (catalog.MetadataPatch, error) {
	if cache == nil || cache.DefaultSchema == "" {
		return catalog.MetadataPatch{}, errors.New("mysql: canonical metadata schema is unavailable")
	}
	if read == nil {
		return catalog.MetadataPatch{}, errors.New("mysql: metadata schema read is nil")
	}
	schema := cache.DefaultSchema
	return db.withMetadataRead(ctx, func(ctx context.Context, tx *sql.Tx) (catalog.MetadataPatch, error) {
		return read(ctx, tx, schema)
	})
}

func (db *MySQLMetadataRepository) withMetadataRead(ctx context.Context, read mysqlMetadataRead) (patch catalog.MetadataPatch, err error) {
	if db == nil || db.Conn == nil {
		return catalog.MetadataPatch{}, errors.New("mysql: metadata database is nil")
	}
	if db.Schema == "" {
		return catalog.MetadataPatch{}, errors.New("mysql: metadata schema is empty")
	}
	if ctx == nil {
		return catalog.MetadataPatch{}, errors.New("mysql: metadata context is nil")
	}
	if read == nil {
		return catalog.MetadataPatch{}, errors.New("mysql: metadata read is nil")
	}
	tx, err := db.Conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelReadCommitted})
	if err != nil {
		return catalog.MetadataPatch{}, fmt.Errorf("mysql: begin metadata read: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				err = errors.Join(err, fmt.Errorf("mysql: rollback metadata read: %w", rollbackErr))
			}
		}
		if err != nil {
			patch.Cache = nil
		}
	}()

	patch, err = read(ctx, tx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = errors.Join(err, ctxErr)
	}
	if err != nil {
		patch.Cache = nil
		return patch, err
	}
	if err = tx.Commit(); err != nil {
		patch.Cache = nil
		return patch, fmt.Errorf("mysql: commit metadata read: %w", err)
	}
	committed = true
	return patch, nil
}

// mysqlMetadataRows runs one schema-bound information_schema query through the
// job's transaction and treats scan, iteration, close and context errors as
// category failures. Callers build local fragments and publish only on success.
func mysqlMetadataRows(ctx context.Context, tx *sql.Tx, query, schema string, scan func(*sql.Rows) error) (err error) {
	if scan == nil {
		return errors.New("mysql: metadata row scanner is nil")
	}
	rows, err := tx.QueryContext(ctx, query, schema)
	if err != nil {
		return fmt.Errorf("mysql: query metadata: %w", err)
	}
	if rows == nil {
		return errors.New("mysql: metadata query returned nil rows")
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("mysql: close metadata rows: %w", closeErr))
		}
	}()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return fmt.Errorf("mysql: scan metadata row: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("mysql: iterate metadata rows: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
