package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"interbase-go/schema"
)

type interBaseMetadataRead func(context.Context, schema.Queryer, int) (MetadataPatch, error)

// runMetadataRead gives one metadata job ownership of a read-only snapshot
// transaction. Every query, including width discovery, runs through that tx.
func (db *InterBaseDBRepository) runMetadataRead(ctx context.Context, read interBaseMetadataRead) (result MetadataPatch, err error) {
	if db == nil || db.Conn == nil {
		return MetadataPatch{}, errors.New("interbase: metadata database is nil")
	}
	if read == nil {
		return MetadataPatch{}, errors.New("interbase: metadata read is nil")
	}
	tx, err := db.Conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelSnapshot})
	if err != nil {
		return MetadataPatch{}, fmt.Errorf("interbase: begin metadata snapshot: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !(errors.Is(rollbackErr, sql.ErrTxDone) && ctx.Err() != nil) {
			err = errors.Join(err, fmt.Errorf("interbase: rollback metadata snapshot: %w", rollbackErr))
			result = MetadataPatch{}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			result = MetadataPatch{}
			if !errors.Is(err, ctxErr) {
				err = errors.Join(err, ctxErr)
			}
		}
	}()

	width, err := interBaseMetadataIdentifierWidth(ctx, tx)
	if err != nil {
		return MetadataPatch{}, err
	}
	result, err = read(ctx, tx, width)
	if err != nil {
		result = MetadataPatch{}
		return result, err
	}
	if err := ctx.Err(); err != nil {
		result = MetadataPatch{}
		return result, err
	}
	return result, nil
}
