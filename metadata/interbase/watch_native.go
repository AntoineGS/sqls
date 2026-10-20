//go:build interbase && cgo && (linux || windows) && amd64

package ibmeta

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	interbase "interbase-go"
	"interbase-go/schema"
)

type watchedFingerprint struct{ value *schema.CatalogFingerprint }

func (f *watchedFingerprint) Format() string       { return f.value.Version }
func (f *watchedFingerprint) RetainedBytes() int64 { return f.value.AccountedBytes }
func (f *watchedFingerprint) Equal(other metadata.Fingerprint) bool {
	rhs, ok := other.(*watchedFingerprint)
	return ok && f.value.Equal(rhs.value)
}

func (p *nativeProvider) ReadFingerprint(ctx context.Context, limit int64) (metadata.Fingerprint, metadata.WatchMetrics, error) {
	return readWatchedFingerprint(ctx, p.conn, limit, interbase.ReadCatalogFingerprint)
}

// The scanner owns native drain and all transaction endings. Only then may the
// borrowed connection return to the provider's existing pool.
func readWatchedFingerprint(ctx context.Context, pool *sql.DB, limit int64, scan func(context.Context, *sql.Conn, int64) (*schema.CatalogFingerprint, error)) (result metadata.Fingerprint, metrics metadata.WatchMetrics, err error) {
	start := time.Now()
	defer func() {
		metrics.Duration = time.Since(start)
		metrics.DurationKnown = true
		err = classifyWatchNativeError(err)
	}()
	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, metrics, err
	}
	defer func() {
		err = errors.Join(err, conn.Close(), ctx.Err())
		if err != nil {
			result = nil
		}
	}()
	value, err := scan(ctx, conn, limit)
	if value != nil {
		metrics.Queries = value.Stats.Queries
		metrics.QueriesKnown = true
		metrics.Rows = value.Stats.Rows
		metrics.BlobOpens = value.Stats.BlobOpens
		metrics.SourceBytes = value.Stats.SourceBytes
		metrics.RetainedBytes = value.AccountedBytes
	}
	if errors.Is(err, schema.ErrFingerprintLimit) {
		err = errors.Join(metadata.ErrWatchLimit, err)
	}
	if errors.Is(err, schema.ErrFingerprintCapability) {
		err = errors.Join(metadata.ErrWatchCapability, err)
	}
	if err != nil {
		return nil, metrics, err
	}
	if value == nil {
		return nil, metrics, metadata.ErrWatchIncomplete
	}
	return &watchedFingerprint{value: value}, metrics, nil
}

func (p *nativeProvider) ReadFull(ctx context.Context) (*catalog.Cache, map[catalog.MetadataKind]catalog.MetadataStatus, metadata.WatchMetrics, error) {
	cache, status, metrics, err := p.repo.ReadWatchedFull(ctx)
	return cache, status, metrics, classifyWatchNativeError(err)
}

func (p *nativeProvider) ReadChanges(ctx context.Context, base *catalog.Cache, previous, current metadata.Fingerprint) (metadata.ObjectDelta, error) {
	old, oldOK := previous.(*watchedFingerprint)
	new, newOK := current.(*watchedFingerprint)
	if !oldOK || !newOK || old == nil || new == nil || old.value == nil || new.value == nil || p.repo == nil {
		return metadata.ObjectDelta{}, metadata.ErrWatchIncomplete
	}
	delta, err := p.repo.ReadWatchedDelta(ctx, base, old.value, new.value)
	return delta, classifyWatchNativeError(err)
}

// Only a genuine typed driver error establishes native origin. Generic provider,
// mapping and validation failures are not identified by their diagnostic text.
func classifyWatchNativeError(err error) error {
	var native *interbase.Error
	if errors.As(err, &native) {
		return errors.Join(metadata.ErrNativeFailure, err)
	}
	return err
}

var _ metadata.ChangeReader = (*nativeProvider)(nil)
