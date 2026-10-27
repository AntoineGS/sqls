package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"interbase-go/schema"
	"interbase-go/txcontext"
)

// runWatchedRead has no automatic database/sql rollback goroutine: the caller
// owns synchronous ending, even when cancellation arrives during a native read.
// The optional driver bridge preserves cancellable Begin admission while query
// contexts carry the same attempt deadline. Returning means native work and
// rollback have drained, not merely that cancellation was requested.
func (db *InterBaseDBRepository) runWatchedRead(ctx context.Context, read interBaseMetadataRead) (result MetadataPatch, err error) {
	if db == nil || db.Conn == nil {
		return result, metadata.ErrWatchIncomplete
	}
	if read == nil {
		return result, metadata.ErrWatchIncomplete
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	conn, err := db.Conn.Conn(ctx)
	if err != nil {
		return result, err
	}
	defer func() {
		err = errors.Join(err, conn.Close())
		if err != nil {
			result.Cache = nil
		}
	}()
	tx, err := conn.BeginTx(txcontext.WithExplicitEnd(ctx), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelReadCommitted})
	if err != nil {
		return MetadataPatch{QueriesKnown: true}, err
	}
	counter := &metadataQueryCounter{queryer: tx}
	defer func() {
		err = errors.Join(err, tx.Rollback(), ctx.Err())
		if err != nil {
			result.Cache = nil
		}
		result.Queries, result.QueriesKnown = counter.queries, true
	}()
	if err = ctx.Err(); err != nil {
		return result, err
	}
	width, err := interBaseMetadataIdentifierWidth(ctx, counter)
	if err != nil {
		return result, err
	}
	result, err = read(ctx, counter, width)
	return result, err
}

// ReadWatchedFull builds a replacement only from completely successful short,
// serial READ COMMITTED groups. It does not change ordinary MetadataPlan.
func (db *InterBaseDBRepository) ReadWatchedFull(ctx context.Context) (cache *catalog.Cache, status map[catalog.MetadataKind]catalog.MetadataStatus, metrics metadata.WatchMetrics, err error) {
	start := time.Now()
	metrics.QueriesKnown = true // All attempted queries already use runWatchedRead accounting.
	defer func() { metrics.Duration = time.Since(start); metrics.DurationKnown = true }()
	cache = metadata.NewCache()
	status = make(map[catalog.MetadataKind]catalog.MetadataStatus)
	status[MetadataColumnsAll] = catalog.MetadataStatus{State: MetadataUnsupported}
	// Procedures are required by the independent full loader as well as views.
	kinds := []MetadataKind{MetadataSchemas, MetadataRelations, MetadataColumnsCurrent, MetadataPrimaryKeys, MetadataViews, MetadataDependencies, MetadataIndexes, MetadataForeignKeys, MetadataProcedures, MetadataFunctions, MetadataGenerators, MetadataSystemRelations, MetadataExceptions, MetadataDomains, MetadataTriggers}
	for _, kind := range kinds {
		if err = ctx.Err(); err != nil {
			return nil, status, metrics, err
		}
		var patch MetadataPatch
		if kind == MetadataSchemas {
			patch = MetadataPatch{Cache: &DBCache{Schemas: map[string]string{"": ""}}, Count: 1, QueriesKnown: true}
		} else {
			patch, err = db.runWatchedRead(ctx, func(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
				switch kind {
				case MetadataRelations:
					return db.readMetadataRelations(ctx, q, width)
				case MetadataColumnsCurrent:
					return db.readMetadataColumns(ctx, q, width)
				case MetadataPrimaryKeys:
					return db.readMetadataPrimaryKeys(ctx, q, width)
				case MetadataViews:
					return db.readMetadataViewsFromColumns(ctx, q, width, cache)
				case MetadataDependencies:
					return db.readMetadataDependencies(ctx, q, width, cache)
				case MetadataIndexes:
					return db.readMetadataIndexes(ctx, q, width)
				case MetadataForeignKeys:
					return db.readMetadataForeignKeys(ctx, q, width)
				case MetadataProcedures:
					return db.readMetadataProcedures(ctx, q, width)
				case MetadataFunctions:
					return db.readMetadataFunctions(ctx, q, width)
				default:
					bound := &InterBaseDBRepository{Conn: db.Conn, Firebird: db.Firebird, SQLDialect: db.SQLDialect, SourceSQLDialect: db.SourceSQLDialect, DatabaseName: db.DatabaseName, snapshot: &interBaseCatalogSnapshot{catalog: schema.New(q)}}
					return bound.readMetadataAccessor(ctx, kind)
				}
			})
		}
		metrics.Queries += int64(patch.Queries)
		s := catalog.MetadataStatus{State: MetadataReady, Count: patch.Count, Queries: patch.Queries, QueriesKnown: patch.QueriesKnown}
		if err == nil {
			cache, err = metadata.Merge(cache, kind, patch)
		}
		if err != nil {
			s.State = MetadataFailed
			s.Err = err
			status[kind] = s
			return nil, status, metrics, errors.Join(metadata.ErrWatchIncomplete, fmt.Errorf("interbase: watched category %s: %w", kind, err))
		}
		status[kind] = s
	}
	if err = ctx.Err(); err != nil {
		return nil, status, metrics, err
	}
	return cache, status, metrics, nil
}
