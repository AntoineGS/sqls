package database

import (
	"context"
	"database/sql"

	"github.com/sqls-server/sqls/metadata"
	"interbase-go/schema"
)

// metadataQueryCounter belongs to one metadata job goroutine. It counts attempted
// QueryContext calls (including failed calls), without counting transaction work.
type metadataQueryCounter struct {
	queryer schema.Queryer
	queries int
}

func (q *metadataQueryCounter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.queries++
	return q.queryer.QueryContext(ctx, query, args...)
}

// metadataJobMetrics and metadataGenerationMetrics are thin delegates to the
// shared metadata package's metrics derivation, kept under their original
// unexported names so existing call sites and tests in this package need no
// edit.
func metadataJobMetrics(status MetadataStatus) metadata.JobMetricValues {
	return metadata.JobMetrics(status)
}

func metadataGenerationMetrics(snapshot *MetadataSnapshot) metadata.GenerationMetricValues {
	return metadata.GenerationMetrics(snapshot)
}
