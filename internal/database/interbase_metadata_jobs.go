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
		return MetadataPatch{QueriesKnown: true}, fmt.Errorf("interbase: begin metadata snapshot: %w", err)
	}
	counter := &metadataQueryCounter{queryer: tx}
	result.QueriesKnown = true
	defer func() {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !(errors.Is(rollbackErr, sql.ErrTxDone) && ctx.Err() != nil) {
			err = errors.Join(err, fmt.Errorf("interbase: rollback metadata snapshot: %w", rollbackErr))
			result.Cache = nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			result.Cache = nil
			if !errors.Is(err, ctxErr) {
				err = errors.Join(err, ctxErr)
			}
		}
		result.Queries, result.QueriesKnown = counter.queries, true
	}()

	width, err := interBaseMetadataIdentifierWidth(ctx, counter)
	if err != nil {
		return result, err
	}
	result, err = read(ctx, counter, width)
	if err != nil {
		result.Cache = nil
		return result, err
	}
	if err := ctx.Err(); err != nil {
		result.Cache = nil
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
		return MetadataPatch{QueriesKnown: true}, fmt.Errorf("interbase: begin metadata snapshot: %w", err)
	}
	counter := &metadataQueryCounter{queryer: tx}
	defer func() {
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !(errors.Is(rollbackErr, sql.ErrTxDone) && ctx.Err() != nil) {
			err = errors.Join(err, fmt.Errorf("interbase: rollback metadata snapshot: %w", rollbackErr))
			result.Cache = nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			result.Cache = nil
			if !errors.Is(err, ctxErr) {
				err = errors.Join(err, ctxErr)
			}
		}
		result.Queries, result.QueriesKnown = counter.queries, true
	}()
	bound := &InterBaseDBRepository{
		Conn: db.Conn, SQLDialect: db.SQLDialect, SourceSQLDialect: db.SourceSQLDialect,
		DatabaseName: db.DatabaseName,
		snapshot:     &interBaseCatalogSnapshot{catalog: schema.New(counter)},
	}
	result, err = read(ctx, bound)
	if err != nil {
		result.Cache = nil
		return result, err
	}
	if err := ctx.Err(); err != nil {
		result.Cache = nil
		return result, err
	}
	return result, nil
}

// MetadataPlan exposes independent category reads, each with its own
// transaction/connection. Three concurrent jobs are the verified pool budget.
func (db *InterBaseDBRepository) MetadataPlan() MetadataPlan {
	return MetadataPlan{Parallelism: 3, Jobs: []MetadataJob{
		{Kind: MetadataSchemas, Run: func(context.Context, *DBCache) (MetadataPatch, error) {
			return MetadataPatch{Cache: &DBCache{Schemas: map[string]string{"": ""}}, Count: 1, QueriesKnown: true}, nil
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
		// Views depends on ColumnsCurrent so its Run function can reuse the
		// already-loaded column data instead of issuing its own duplicate
		// view-columns query; see readMetadataViewsFromColumns.
		{Kind: MetadataViews, DependsOn: []MetadataKind{MetadataColumnsCurrent}, Run: func(ctx context.Context, cache *DBCache) (MetadataPatch, error) {
			return db.runMetadataRead(ctx, func(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
				return db.readMetadataViewsFromColumns(ctx, q, width, cache)
			})
		}},
		// Dependencies resolves relation dependencies to tables or views,
		// so it reads the already-loaded view namespace.
		{Kind: MetadataDependencies, DependsOn: []MetadataKind{MetadataViews}, Run: func(ctx context.Context, cache *DBCache) (MetadataPatch, error) {
			return db.runMetadataRead(ctx, func(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
				return db.readMetadataDependencies(ctx, q, width, cache)
			})
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
				return bound.readMetadataAccessor(ctx, MetadataGenerators)
			})
		}},
		{Kind: MetadataSystemRelations, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return db.runMetadataRepositoryRead(ctx, func(ctx context.Context, bound *InterBaseDBRepository) (MetadataPatch, error) {
				return bound.readMetadataAccessor(ctx, MetadataSystemRelations)
			})
		}},
		{Kind: MetadataExceptions, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return db.runMetadataRepositoryRead(ctx, func(ctx context.Context, bound *InterBaseDBRepository) (MetadataPatch, error) {
				return bound.readMetadataAccessor(ctx, MetadataExceptions)
			})
		}},
		{Kind: MetadataDomains, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return db.runMetadataRepositoryRead(ctx, func(ctx context.Context, bound *InterBaseDBRepository) (MetadataPatch, error) {
				return bound.readMetadataAccessor(ctx, MetadataDomains)
			})
		}},
		{Kind: MetadataTriggers, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			return db.runMetadataRepositoryRead(ctx, func(ctx context.Context, bound *InterBaseDBRepository) (MetadataPatch, error) {
				return bound.readMetadataAccessor(ctx, MetadataTriggers)
			})
		}},
	}}
}

// readMetadataAccessor shares the ordinary descriptor converters with watched
// reads. Its receiver must already be bound to the group's query source.
func (db *InterBaseDBRepository) readMetadataAccessor(ctx context.Context, kind MetadataKind) (MetadataPatch, error) {
	switch kind {
	case MetadataGenerators:
		values, err := db.DescribeGenerators(ctx)
		if err != nil {
			return MetadataPatch{}, err
		}
		items := make(map[string]*GeneratorDesc, len(values))
		byID := make(map[string]*GeneratorDesc, len(values))
		for _, value := range values {
			copy := *value
			items[catalogCacheKey(value.Name)] = &copy
			byID[interBaseByIDKey(ObjectKindGenerator, value.Name)] = &copy
		}
		return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Generators: items, GeneratorsByID: byID}}, Count: len(items)}, nil
	case MetadataSystemRelations:
		values, err := db.DescribeSystemRelations(ctx)
		if err != nil {
			return MetadataPatch{}, err
		}
		byID := make(map[string]*SystemRelationDesc, len(values))
		for _, value := range values {
			if value != nil {
				byID[interBaseByIDKey(ObjectKindTable, value.Name)] = value
			}
		}
		return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{SystemRelationsByID: byID}}, Count: len(byID)}, nil
	case MetadataExceptions:
		values, err := db.DescribeExceptions(ctx)
		if err != nil {
			return MetadataPatch{}, err
		}
		byID := make(map[string]*ExceptionDesc, len(values))
		for _, value := range values {
			if value != nil {
				byID[interBaseByIDKey(ObjectKindException, value.Name)] = value
			}
		}
		return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{ExceptionsByID: byID}}, Count: len(byID)}, nil
	case MetadataDomains:
		values, err := db.DescribeDomains(ctx)
		if err != nil {
			return MetadataPatch{}, err
		}
		items := make(map[string]*DomainDesc, len(values))
		byID := make(map[string]*DomainDesc, len(values))
		for _, value := range values {
			copy := *value
			items[catalogCacheKey(value.Name)] = &copy
			byID[interBaseByIDKey(ObjectKindDomain, value.Name)] = &copy
		}
		return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Domains: items, DomainsByID: byID}}, Count: len(items)}, nil
	case MetadataTriggers:
		values, err := db.DescribeTriggers(ctx)
		if err != nil {
			return MetadataPatch{}, err
		}
		items := make(map[string]*TriggerDesc, len(values))
		byID := make(map[string]*TriggerDesc, len(values))
		for _, value := range values {
			copy := *value
			items[catalogCacheKey(value.Name)] = &copy
			byID[interBaseByIDKey(ObjectKindTrigger, value.Name)] = &copy
		}
		return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Triggers: items, TriggersByID: byID}}, Count: len(items)}, nil
	default:
		return MetadataPatch{}, fmt.Errorf("interbase: unsupported accessor category %s", kind)
	}
}
