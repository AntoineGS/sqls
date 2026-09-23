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

// runMetadataRepositoryRead binds a normal repository accessor to one
// transaction without doing the bulk reader's separate identifier-width
// discovery. The driver accessors perform their own projection discovery.
func (db *InterBaseDBRepository) runMetadataRepositoryRead(ctx context.Context, read func(context.Context, *InterBaseDBRepository) (MetadataPatch, error)) (result MetadataPatch, err error) {
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
	bound := &InterBaseDBRepository{
		Conn: db.Conn, SQLDialect: db.SQLDialect, SourceSQLDialect: db.SourceSQLDialect,
		DatabaseName: db.DatabaseName,
		snapshot:     &interBaseCatalogSnapshot{catalog: schema.New(tx)},
	}
	result, err = read(ctx, bound)
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

// MetadataPlan exposes independent category reads, each with its own
// transaction/connection. Three concurrent jobs are the verified pool budget.
func (db *InterBaseDBRepository) MetadataPlan() MetadataPlan {
	return MetadataPlan{Parallelism: 3, Jobs: []MetadataJob{
		{Kind: MetadataSchemas, Run: func(context.Context, *DBCache) (MetadataPatch, error) {
			return MetadataPatch{Cache: &DBCache{Schemas: map[string]string{"": ""}}, Count: 1}, nil
		}},
		{Kind: MetadataRelations, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return db.runMetadataRead(ctx, db.readMetadataRelations)
		}},
		{Kind: MetadataColumnsCurrent, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return db.runMetadataRead(ctx, db.readMetadataColumns)
		}},
		{Kind: MetadataProcedures, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return db.runMetadataRead(ctx, db.readMetadataProcedures)
		}},
		{Kind: MetadataPrimaryKeys, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return db.runMetadataRead(ctx, db.readMetadataPrimaryKeys)
		}},
		{Kind: MetadataViews, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return db.runMetadataRead(ctx, db.readMetadataViews)
		}},
		{Kind: MetadataIndexes, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return db.runMetadataRead(ctx, db.readMetadataIndexes)
		}},
		{Kind: MetadataForeignKeys, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return db.runMetadataRead(ctx, db.readMetadataForeignKeys)
		}},
		{Kind: MetadataFunctions, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return db.runMetadataRead(ctx, db.readMetadataFunctions)
		}},
		{Kind: MetadataGenerators, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return db.runMetadataRepositoryRead(ctx, func(ctx context.Context, bound *InterBaseDBRepository) (MetadataPatch, error) {
				values, err := bound.DescribeGenerators(ctx)
				if err != nil {
					return MetadataPatch{}, err
				}
				items := make(map[string]*GeneratorDesc, len(values))
				for _, value := range values {
					copy := *value
					items[catalogCacheKey(value.Name)] = &copy
				}
				return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Generators: items}}, Count: len(items)}, nil
			})
		}},
		{Kind: MetadataDomains, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return db.runMetadataRepositoryRead(ctx, func(ctx context.Context, bound *InterBaseDBRepository) (MetadataPatch, error) {
				values, err := bound.DescribeDomains(ctx)
				if err != nil {
					return MetadataPatch{}, err
				}
				items := make(map[string]*DomainDesc, len(values))
				for _, value := range values {
					copy := *value
					items[catalogCacheKey(value.Name)] = &copy
				}
				return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Domains: items}}, Count: len(items)}, nil
			})
		}},
		{Kind: MetadataTriggers, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return db.runMetadataRepositoryRead(ctx, func(ctx context.Context, bound *InterBaseDBRepository) (MetadataPatch, error) {
				values, err := bound.DescribeTriggers(ctx)
				if err != nil {
					return MetadataPatch{}, err
				}
				items := make(map[string]*TriggerDesc, len(values))
				for _, value := range values {
					copy := *value
					items[catalogCacheKey(value.Name)] = &copy
				}
				return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Triggers: items}}, Count: len(items)}, nil
			})
		}},
	}}
}
