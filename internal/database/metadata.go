package database

import (
	"context"
	"time"
)

// MetadataKind identifies an independently loadable metadata category.
type MetadataKind string

const (
	MetadataSchemas        MetadataKind = "schemas"
	MetadataRelations      MetadataKind = "relations"
	MetadataColumnsCurrent MetadataKind = "columns-current"
	MetadataColumnsAll     MetadataKind = "columns-all"
	MetadataPrimaryKeys    MetadataKind = "primary-keys"
	MetadataForeignKeys    MetadataKind = "foreign-keys"
	MetadataViews          MetadataKind = "views"
	MetadataProcedures     MetadataKind = "procedures"
	MetadataGenerators     MetadataKind = "generators"
	MetadataDomains        MetadataKind = "domains"
	MetadataFunctions      MetadataKind = "functions"
	MetadataIndexes        MetadataKind = "indexes"
	MetadataTriggers       MetadataKind = "triggers"
)

// MetadataState describes a category's progress in one metadata generation.
type MetadataState string

const (
	MetadataPending     MetadataState = "pending"
	MetadataLoading     MetadataState = "loading"
	MetadataReady       MetadataState = "ready"
	MetadataFailed      MetadataState = "failed"
	MetadataBlocked     MetadataState = "blocked"
	MetadataCancelled   MetadataState = "cancelled"
	MetadataUnsupported MetadataState = "unsupported"
)

// MetadataStatus records the state and outcome of one category load.
type MetadataStatus struct {
	State        MetadataState
	QueuedAt     time.Time
	StartedAt    time.Time
	FinishedAt   time.Time
	Count        int
	Queries      int
	QueriesKnown bool
	Err          error // internal only
}

// MetadataSnapshot is an immutable view of one metadata generation.
type MetadataSnapshot struct {
	Generation uint64
	Revision   uint64
	Cache      *DBCache
	Status     map[MetadataKind]MetadataStatus
}

// MetadataPatch is one job's cache fragment and accounting information.
type MetadataPatch struct {
	Cache        *DBCache
	Count        int
	Queries      int
	QueriesKnown bool
}

// MetadataJob describes one independently executable metadata category.
type MetadataJob struct {
	Kind      MetadataKind
	DependsOn []MetadataKind
	Run       func(context.Context, *DBCache) (MetadataPatch, error)
}

// MetadataPlan is the bounded set of category loads a repository supports.
type MetadataPlan struct {
	Parallelism int
	Jobs        []MetadataJob
}

// MetadataPlanRepository optionally provides a driver-specific metadata plan.
type MetadataPlanRepository interface {
	MetadataPlan() MetadataPlan
}

// Settled reports whether every category in a non-empty snapshot is terminal.
func (s *MetadataSnapshot) Settled() bool {
	if s == nil || len(s.Status) == 0 {
		return false
	}
	for _, status := range s.Status {
		switch status.State {
		case MetadataReady, MetadataFailed, MetadataBlocked, MetadataCancelled, MetadataUnsupported:
		default:
			return false
		}
	}
	return true
}

// Degraded reports whether a category in a non-empty snapshot failed or was blocked.
func (s *MetadataSnapshot) Degraded() bool {
	if s == nil || len(s.Status) == 0 {
		return false
	}
	for _, status := range s.Status {
		if status.State == MetadataFailed || status.State == MetadataBlocked {
			return true
		}
	}
	return false
}
