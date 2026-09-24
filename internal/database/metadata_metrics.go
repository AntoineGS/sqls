package database

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"time"

	"interbase-go/schema"
)

// metadataQueryCounter belongs to one metadata job goroutine. It counts attempted
// QueryContext calls (including failed calls), without counting transaction work.
type metadataQueryCounter struct {
	queryer schema.Queryer
	queries int
}

var metadataTraceLogger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

func (q *metadataQueryCounter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.queries++
	return q.queryer.QueryContext(ctx, query, args...)
}

type metadataJobMetricValues struct {
	Queue           time.Duration
	Run             time.Duration
	Count           int
	SuccessfulCount int
	FailedCount     int
	Queries         int
	QueriesKnown    bool
}

func metadataJobMetrics(status MetadataStatus) metadataJobMetricValues {
	metrics := metadataJobMetricValues{
		Count: status.Count, Queries: status.Queries, QueriesKnown: status.QueriesKnown,
	}
	if !status.QueuedAt.IsZero() && !status.StartedAt.IsZero() {
		metrics.Queue = status.StartedAt.Sub(status.QueuedAt)
		if metrics.Queue < 0 {
			metrics.Queue = 0
		}
	}
	if !status.StartedAt.IsZero() {
		end := status.FinishedAt
		if end.IsZero() {
			end = time.Now()
		}
		metrics.Run = end.Sub(status.StartedAt)
		if metrics.Run < 0 {
			metrics.Run = 0
		}
	}
	if status.State == MetadataReady {
		metrics.SuccessfulCount = status.Count
	}
	if status.State == MetadataFailed {
		metrics.FailedCount = status.Count
	}
	return metrics
}

type metadataGenerationMetricValues struct {
	Generation      uint64
	Count           int
	SuccessfulCount int
	FailedCount     int
	Queries         int
	QueriesKnown    bool
	Queue           time.Duration
	Run             time.Duration
}

func metadataGenerationMetrics(snapshot *MetadataSnapshot) metadataGenerationMetricValues {
	result := metadataGenerationMetricValues{QueriesKnown: true}
	if snapshot == nil {
		return result
	}
	result.Generation = snapshot.Generation
	for _, status := range snapshot.Status {
		metrics := metadataJobMetrics(status)
		result.Queue += metrics.Queue
		result.Run += metrics.Run
		result.SuccessfulCount += metrics.SuccessfulCount
		result.FailedCount += metrics.FailedCount
		result.Count += metrics.Count
		if metrics.QueriesKnown {
			result.Queries += metrics.Queries
		} else if status.State != MetadataUnsupported {
			result.QueriesKnown = false
		}
	}
	return result
}

func metadataTraceEnabled() bool { return os.Getenv("SQLS_METADATA_TRACE") == "1" }

func logMetadataJob(generation uint64, kind MetadataKind, status MetadataStatus) {
	if !metadataTraceEnabled() {
		return
	}
	metrics := metadataJobMetrics(status)
	var queries any
	if metrics.QueriesKnown {
		queries = metrics.Queries
	}
	metadataTraceLogger.Info("metadata job terminal", "generation", generation, "kind", kind, "state", status.State,
		"queue_ms", metrics.Queue.Milliseconds(), "run_ms", metrics.Run.Milliseconds(),
		"count", status.Count, "queries", queries)
}

func logMetadataGeneration(snapshot *MetadataSnapshot) {
	if !metadataTraceEnabled() || snapshot == nil {
		return
	}
	metrics := metadataGenerationMetrics(snapshot)
	var queries any
	if metrics.QueriesKnown {
		queries = metrics.Queries
	}
	state := "ready"
	if snapshot.Degraded() {
		state = "degraded"
	} else {
		for _, status := range snapshot.Status {
			if status.State == MetadataCancelled {
				state = "cancelled"
				break
			}
		}
	}
	metadataTraceLogger.Info("metadata generation settled", "generation", metrics.Generation,
		"state", state,
		"queue_ms", metrics.Queue.Milliseconds(), "run_ms", metrics.Run.Milliseconds(),
		"count", metrics.Count, "successful_count", metrics.SuccessfulCount,
		"failed_count", metrics.FailedCount, "queries", queries)
}

func logCancelledMetadataStatuses(snapshot *MetadataSnapshot) {
	if snapshot == nil {
		return
	}
	for kind, status := range snapshot.Status {
		if status.State == MetadataCancelled {
			logMetadataJob(snapshot.Generation, kind, status)
		}
	}
}
