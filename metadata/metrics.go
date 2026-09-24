package metadata

import (
	"log/slog"
	"os"
	"time"

	"github.com/sqls-server/sqls/catalog"
)

var metadataTraceLogger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

// JobMetricValues is the derived timing/count summary for one category's
// terminal or in-flight status.
type JobMetricValues struct {
	Queue           time.Duration
	Run             time.Duration
	Count           int
	SuccessfulCount int
	FailedCount     int
	Queries         int
	QueriesKnown    bool
}

// JobMetrics derives timing and count metrics from one category's status.
func JobMetrics(status catalog.MetadataStatus) JobMetricValues { return metadataJobMetrics(status) }

func metadataJobMetrics(status catalog.MetadataStatus) JobMetricValues {
	metrics := JobMetricValues{
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
	if status.State == catalog.MetadataReady {
		metrics.SuccessfulCount = status.Count
	}
	if status.State == catalog.MetadataFailed {
		metrics.FailedCount = status.Count
	}
	return metrics
}

// GenerationMetricValues is the derived timing/count summary across every
// category in one metadata generation.
type GenerationMetricValues struct {
	Generation      uint64
	Count           int
	SuccessfulCount int
	FailedCount     int
	Queries         int
	QueriesKnown    bool
	Queue           time.Duration
	Run             time.Duration
}

// GenerationMetrics derives timing and count metrics across every category in
// snapshot.
func GenerationMetrics(snapshot *catalog.MetadataSnapshot) GenerationMetricValues {
	return metadataGenerationMetrics(snapshot)
}

func metadataGenerationMetrics(snapshot *catalog.MetadataSnapshot) GenerationMetricValues {
	result := GenerationMetricValues{QueriesKnown: true}
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
		} else if status.State != catalog.MetadataUnsupported {
			result.QueriesKnown = false
		}
	}
	return result
}

func metadataTraceEnabled() bool { return os.Getenv("SQLS_METADATA_TRACE") == "1" }

func logMetadataJob(generation uint64, kind catalog.MetadataKind, status catalog.MetadataStatus) {
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

func logMetadataGeneration(snapshot *catalog.MetadataSnapshot) {
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
			if status.State == catalog.MetadataCancelled {
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

func logCancelledMetadataStatuses(snapshot *catalog.MetadataSnapshot) {
	if snapshot == nil {
		return
	}
	for kind, status := range snapshot.Status {
		if status.State == catalog.MetadataCancelled {
			logMetadataJob(snapshot.Generation, kind, status)
		}
	}
}
