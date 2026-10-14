package metadata

import (
	"context"
	"errors"
	"time"

	"github.com/sqls-server/sqls/catalog"
)

const watchFingerprintLimit int64 = 64 << 20

// A baseline belongs to exactly one published store generation and uncertainty
// epoch. Fingerprints are immutable engine-owned compact values, not copied maps.
type watchBaseline struct {
	fingerprint                            Fingerprint
	scope                                  scopeKey
	incarnation, generation, invalidations uint64
}

// Maintain performs an opted-in catalog maintenance attempt. Scanning precedes
// all replacement reads; failures preserve the published cache and baseline.
// The attempt budget starts after serialized admission. Cancellation requests
// cooperative provider cancellation but never skips actual native drain.
func (l *Lease) Maintain(ctx context.Context, req MaintenanceRequest) (answer MaintenanceResult, failure error) {
	origin := MaintenanceFailureAdmission
	defer func() {
		if failure != nil {
			answer.FailureOrigin = origin
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	if req.Mode != MaintenanceCheck && req.Mode != MaintenanceFull {
		return MaintenanceResult{}, ErrUnsupported
	}
	if req.Mode == MaintenanceCheck && !req.Fingerprint {
		return MaintenanceResult{}, ErrUnsupported
	}
	t := l.track
	if t.closed.Load() {
		return MaintenanceResult{}, ErrClosed
	}
	// Queue wait owns neither an I/O permit nor an attempt deadline. This lock
	// remains held until synchronous provider calls have actually drained.
	t.refreshMu.Lock()
	defer t.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return MaintenanceResult{}, err
	}
	t.mu.Lock()
	if t.closed.Load() || t.ctx.Err() != nil {
		t.mu.Unlock()
		return MaintenanceResult{}, ErrClosed
	}
	reader, ok := t.provider.(ChangeReader)
	current := t.loader
	if !ok || current == nil {
		t.mu.Unlock()
		return MaintenanceResult{}, ErrUnsupported
	}
	attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
	stop := context.AfterFunc(t.ctx, cancel)
	done := make(chan struct{})
	t.maintenanceDone = done
	epoch, baseline := t.invalidations, t.baseline
	t.mu.Unlock()
	defer func() {
		stop()
		cancel()
		t.mu.Lock()
		close(done)
		if t.maintenanceDone == done {
			t.maintenanceDone = nil
		}
		t.mu.Unlock()
	}()
	if err := current.Wait(attempt); err != nil {
		return MaintenanceResult{}, err
	}
	previous := current.Snapshot()
	result := MaintenanceResult{Generation: previous.Generation}
	valid := baseline != nil && baseline.fingerprint != nil && baseline.scope == newScopeKey(t.scope) && baseline.incarnation == l.incarnation && baseline.generation == previous.Generation && baseline.invalidations == epoch && !previous.Stale
	result.BaselineValid = valid
	// Automatic work must not acknowledge an uncertain DDL interval.
	if previous.Stale && !(req.Mode == MaintenanceFull && req.Explicit) {
		origin = MaintenanceFailureUncertainty
		return result, ErrStale
	}
	io := func(read func() error) error {
		release, err := t.storeBudget.acquire(attempt)
		if err != nil {
			return err
		}
		defer release()
		if err := attempt.Err(); err != nil {
			return err
		}
		err = read()
		// A concurrent store shutdown may cancel the child before its timer
		// propagates an already-expired caller deadline. Preserve both causes
		// after actual drain, rather than losing the caller's deadline evidence.
		return errors.Join(err, attempt.Err(), ctx.Err())
	}
	var candidate Fingerprint
	if req.Fingerprint {
		origin = MaintenanceFailureScan
		err := io(func() (err error) {
			candidate, result.Scan, err = reader.ReadFingerprint(attempt, watchFingerprintLimit)
			return err
		})
		if err == nil {
			if candidate == nil || candidate.Format() == "" || candidate.RetainedBytes() < 0 {
				err = ErrWatchIncomplete
			} else if candidate.RetainedBytes() > watchFingerprintLimit {
				err = ErrWatchLimit
			}
		}
		if err != nil {
			// A failed check NEVER initiates an expensive catalog load. Only an
			// already-requested full can continue after permanent limitations.
			if req.Mode != MaintenanceFull || attempt.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return result, err
			}
			switch {
			case errors.Is(err, ErrWatchLimit):
				result.WatchError = "limit"
			case errors.Is(err, ErrWatchCapability):
				result.WatchError = "capability"
			default:
				return result, err
			}
			candidate = nil
		}
		result.ScanComplete = err == nil
	}
	origin = MaintenanceFailureReplacement
	if candidate != nil && valid && req.Mode == MaintenanceCheck && baseline.fingerprint.Format() == candidate.Format() && baseline.fingerprint.Equal(candidate) {
		t.mu.Lock()
		err := l.maintenanceFence(current, previous.Generation, epoch, attempt)
		t.mu.Unlock()
		if err != nil {
			if errors.Is(err, ErrStale) {
				origin = MaintenanceFailureUncertainty
			}
			return result, err
		}
		result.Outcome = "unchanged"
		return result, nil
	}
	full := req.Mode == MaintenanceFull || !valid || candidate == nil || baseline.fingerprint.Format() != candidate.Format()
	var cache *catalog.Cache
	var status map[catalog.MetadataKind]catalog.MetadataStatus
	// Only actual provider invocations contribute components. The first is
	// copied (an unattempted initial component is the zero identity); subsequent
	// components qualify a dimension only if every attempted component knows it.
	readStarted := false
	recordRead := func(metrics WatchMetrics) {
		if !readStarted {
			result.Read = metrics
			readStarted = true
		} else {
			result.Read = addWatchMetrics(result.Read, metrics)
		}
	}
	if !full {
		var delta ObjectDelta
		readErr := io(func() (err error) {
			delta, err = reader.ReadChanges(attempt, previous.Cache, baseline.fingerprint, candidate)
			recordRead(delta.Metrics)
			return err
		})
		if readErr != nil {
			return result, readErr
		}
		result.Affected, result.FullReason = delta.Affected, delta.FullReason
		if delta.FullReason != "" {
			full = true
		} else {
			var err error
			cache, err = MergeObjects(attempt, previous.Cache, delta)
			if err != nil {
				return result, err
			}
			status = cloneMap(previous.Status)
			for kind, patch := range delta.Patches {
				s := status[kind]
				if s.State != catalog.MetadataReady {
					return result, ErrWatchIncomplete
				}
				s.Queries, s.QueriesKnown, s.FinishedAt = patch.Queries, patch.QueriesKnown, time.Now()
				status[kind] = s
			}
			result.Outcome = "incremental"
		}
	}
	if full {
		var metrics WatchMetrics
		readErr := io(func() (err error) {
			cache, status, metrics, err = reader.ReadFull(attempt)
			// Preserve measured work even on failure, but don't add an unknown
			// component if I/O admission never actually entered ReadFull.
			recordRead(metrics)
			return err
		})
		if readErr != nil {
			return result, readErr
		}
		result.Outcome = "full"
		if candidate == nil {
			result.Outcome = "full-unwatched"
		}
	}
	if cache == nil {
		return result, ErrWatchIncomplete
	}
	nextCache := *cache
	nextCache.Metadata = cloneMap(cache.Metadata)
	status = cloneMap(status)
	for kind, s := range status {
		if s.State == catalog.MetadataReady {
			s.Count = completeCategoryCount(&nextCache, kind)
			status[kind] = s
		}
	}
	next := &catalog.MetadataSnapshot{Generation: previous.Generation + 1, Revision: 1, Started: true, Cache: &nextCache, Status: status}
	if err := backgroundRefreshFailure(previous, next); err != nil {
		return result, err
	}
	if candidate != nil && full {
		// Every supported patch category must have loaded before F can be
		// trusted, including categories which previously failed at startup.
		for _, kind := range metadataKinds {
			old, oldOK := previous.Status[kind]
			got, gotOK := status[kind]
			if (oldOK && old.State != catalog.MetadataUnsupported) || (gotOK && got.State != catalog.MetadataUnsupported) {
				if !gotOK || got.State != catalog.MetadataReady || nextCache.Metadata[kind] != catalog.MetadataReady {
					return result, ErrWatchIncomplete
				}
			}
		}
	}
	settled := NewLoader(t.storeBudget)
	if err := settled.installSettled(t.ctx, next); err != nil {
		return result, err
	}
	t.mu.Lock()
	// Acquire the last shared publication mutex BEFORE the final fence. A
	// blocked detail acquisition must not consume the attempt's deadline and
	// then mutate old details on a failed publication.
	t.details.mu.Lock()
	if err := l.maintenanceFence(current, previous.Generation, epoch, attempt); err != nil {
		if errors.Is(err, ErrStale) {
			origin = MaintenanceFailureUncertainty
		}
		t.details.mu.Unlock()
		t.mu.Unlock()
		settled.Stop()
		return result, err
	}
	// Fence details before exposing the new loader: old-generation requests
	// can fail stale, but can never acquire an old entry under the new label.
	t.details.beginGenerationLocked(next.Generation)
	settled.SetChangedCallback(func() { l.registry.publishTrackEvent(t) })
	t.loader = settled
	t.baseline = nil
	if candidate != nil {
		t.baseline = &watchBaseline{fingerprint: candidate, scope: newScopeKey(t.scope), incarnation: l.incarnation, generation: next.Generation, invalidations: epoch}
	}
	t.details.mu.Unlock()
	t.mu.Unlock()
	current.SetChangedCallback(nil)
	current.Stop()
	l.registry.publishTrackEvent(t)
	result.Generation, result.BaselineValid = next.Generation, candidate != nil
	return result, nil
}

// Caller holds track.mu, and also details.mu for replacement publication.
// Invalidate shares track.mu; logical retirement is observed atomically.
func (l *Lease) maintenanceFence(current *Loader, generation, epoch uint64, ctx context.Context) error {
	t := l.track
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.loader != current || current.Snapshot().Generation != generation || t.invalidations != epoch {
		return ErrStale
	}
	// Snapshot takes the old loader mutex. Observe the context once more after
	// every shared publication lock has been acquired, before any mutation.
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.closed.Load() || t.ctx.Err() != nil {
		return ErrClosed
	}
	return nil
}

func addWatchMetrics(a, b WatchMetrics) WatchMetrics {
	return WatchMetrics{DurationKnown: a.DurationKnown && b.DurationKnown, QueriesKnown: a.QueriesKnown && b.QueriesKnown, Queries: a.Queries + b.Queries, Rows: a.Rows + b.Rows, BlobOpens: a.BlobOpens + b.BlobOpens, SourceBytes: a.SourceBytes + b.SourceBytes, RetainedBytes: a.RetainedBytes + b.RetainedBytes, Duration: a.Duration + b.Duration}
}

// Counts describe the complete category, not the replacement fragment. Query
// accounting remains the provider's, and failed categories retain their status.
func completeCategoryCount(c *catalog.Cache, kind catalog.MetadataKind) int {
	n := 0
	switch kind {
	case catalog.MetadataSchemas:
		return len(c.Schemas)
	case catalog.MetadataRelations:
		for _, v := range c.SchemaTables {
			n += len(v)
		}
	case catalog.MetadataColumnsCurrent, catalog.MetadataColumnsAll:
		if c.ColumnsByID != nil {
			for _, v := range c.ColumnsByID {
				n += len(v)
			}
		} else {
			for _, v := range c.ColumnsWithParent {
				n += len(v)
			}
		}
	case catalog.MetadataPrimaryKeys:
		if c.PrimaryKeysByID != nil {
			return len(c.PrimaryKeysByID)
		}
		return len(c.PrimaryKeyColumns)
	case catalog.MetadataForeignKeys:
		seen := map[*catalog.ForeignKey]bool{}
		for _, refs := range c.ForeignKeys {
			for _, keys := range refs {
				for _, key := range keys {
					seen[key] = true
				}
			}
		}
		return len(seen)
	case catalog.MetadataDependencies:
		if c.Dependencies != nil {
			return c.Dependencies.Len()
		}
	default:
		if c.Catalog == nil {
			return 0
		}
		switch kind {
		case catalog.MetadataViews:
			return len(c.Catalog.ViewsByID)
		case catalog.MetadataProcedures:
			return len(c.Catalog.ProceduresByID)
		case catalog.MetadataFunctions:
			return len(c.Catalog.FunctionsByID)
		case catalog.MetadataDomains:
			return len(c.Catalog.DomainsByID)
		case catalog.MetadataGenerators:
			return len(c.Catalog.GeneratorsByID)
		case catalog.MetadataIndexes:
			return len(c.Catalog.IndexesByID)
		case catalog.MetadataTriggers:
			return len(c.Catalog.TriggersByID)
		case catalog.MetadataExceptions:
			return len(c.Catalog.ExceptionsByID)
		case catalog.MetadataSystemRelations:
			return len(c.Catalog.SystemRelationsByID)
		}
	}
	return n
}
