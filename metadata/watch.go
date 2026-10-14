package metadata

import (
	"context"
	"errors"
	"time"

	"github.com/sqls-server/sqls/catalog"
)

// Fingerprint is an engine-neutral, immutable catalog scan retained by a watcher.
type Fingerprint interface {
	Format() string
	RetainedBytes() int64
	Equal(Fingerprint) bool
}

type WatchMetrics struct {
	// Availability is independent of numeric values and physical completeness.
	// On failure these describe measured elapsed time/attempted query work only.
	DurationKnown, QueriesKnown                          bool
	Queries, Rows, BlobOpens, SourceBytes, RetainedBytes int64
	Duration                                             time.Duration
}

// ObjectDelta replaces complete per-category owner sets, including empty sets.
// An omitted owner is unknown, not deleted. Columns and keys use table identities
// even for views. Index/trigger coverage may name individual objects or relations
// whose complete groups were read. Patches transfer ownership of their data.
type ObjectDelta struct {
	Owners           map[catalog.MetadataKind][]catalog.ObjectID
	Deleted          []catalog.ObjectID
	Patches          map[catalog.MetadataKind]catalog.MetadataPatch
	DependencyOwners []catalog.DependencyRef
	DependencyEdges  []catalog.DependencyEdge
	CheckNames       map[string][]string
	Affected         int
	FullReason       string
	Metrics          WatchMetrics
}

// ChangeReader is optional; Provider remains unchanged for non-watching engines.
type ChangeReader interface {
	ReadFingerprint(context.Context, int64) (Fingerprint, WatchMetrics, error)
	ReadChanges(context.Context, *catalog.Cache, Fingerprint, Fingerprint) (ObjectDelta, error)
	ReadFull(context.Context) (*catalog.Cache, map[catalog.MetadataKind]catalog.MetadataStatus, WatchMetrics, error)
}

type MaintenanceMode uint8

const (
	MaintenanceCheck MaintenanceMode = iota
	MaintenanceFull
)

type MaintenanceRequest struct {
	Mode        MaintenanceMode
	Fingerprint bool
	Explicit    bool // Only explicit manual full may clear captured uncertainty.
}

// MaintenanceFailureOrigin identifies the actual failing phase, independently
// of the requested mode and of partial or unavailable metrics.
type MaintenanceFailureOrigin uint8

const (
	MaintenanceFailureNone MaintenanceFailureOrigin = iota
	MaintenanceFailureAdmission
	MaintenanceFailureScan
	MaintenanceFailureReplacement
	MaintenanceFailureUncertainty
)

type MaintenanceResult struct {
	FailureOrigin MaintenanceFailureOrigin
	// ScanComplete reports a valid, within-budget fingerprint acquisition. It
	// does not imply successful replacement or a current baseline.
	ScanComplete        bool
	Outcome, FullReason string // unchanged, incremental, full, full-unwatched
	Generation          uint64
	Affected            int
	BaselineValid       bool
	Scan, Read          WatchMetrics
	WatchError          string // Safe category for a successful catalog-only fallback.
}

var (
	// ErrNativeFailure marks a typed engine/driver cause at a native adapter.
	// It never carries provider text and does not override other stable errors.
	ErrNativeFailure   = errors.New("metadata: native engine failure")
	ErrWatchLimit      = errors.New("metadata: watcher retained-byte limit")
	ErrWatchCapability = errors.New("metadata: watcher capability unavailable")
	ErrWatchIncomplete = errors.New("metadata: watcher replacement is incomplete")
)
