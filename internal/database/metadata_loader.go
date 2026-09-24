package database

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"
)

var (
	ErrMetadataStaleGeneration = errors.New("stale metadata generation")
	ErrMetadataAlreadyStarted  = errors.New("metadata generation already started")
	ErrMetadataStopped         = errors.New("metadata loader stopped")
	ErrInvalidMetadataPlan     = errors.New("invalid metadata plan")
)

// MetadataLoad represents logical settlement of one generation. Native job
// calls may still be draining after Done closes; use MetadataLoader.Wait for it.
type MetadataLoad struct{ Done <-chan struct{} }

type MetadataLoader struct {
	mu          sync.Mutex
	snapshot    *MetadataSnapshot
	generation  uint64
	started     bool
	startFailed bool
	stopped     bool
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	doneClosed  bool
	callback    func()

	// The semaphore belongs to the loader, not a generation. A cancelled native
	// call keeps its slot until the Run function actually returns.
	semaphore chan struct{}
	slotWake  chan struct{}
	active    int
	drained   chan struct{}
}

func NewMetadataLoader() *MetadataLoader {
	drained := make(chan struct{})
	close(drained)
	return &MetadataLoader{semaphore: make(chan struct{}, 3), slotWake: make(chan struct{}), drained: drained}
}

var metadataKinds = []MetadataKind{
	MetadataSchemas, MetadataRelations, MetadataColumnsCurrent, MetadataColumnsAll,
	MetadataPrimaryKeys, MetadataForeignKeys, MetadataViews, MetadataProcedures,
	MetadataGenerators, MetadataDomains, MetadataFunctions, MetadataIndexes, MetadataTriggers,
}

func emptyMetadataStatus() map[MetadataKind]MetadataStatus {
	status := make(map[MetadataKind]MetadataStatus, len(metadataKinds))
	for _, kind := range metadataKinds {
		status[kind] = MetadataStatus{State: MetadataUnsupported}
	}
	return status
}

// Reset installs an empty generation. Generations are strictly increasing;
// stale and duplicate reset requests are ignored.
func (l *MetadataLoader) Reset(generation uint64) {
	l.mu.Lock()
	if l.stopped || generation <= l.generation {
		l.mu.Unlock()
		return
	}
	if l.cancel != nil {
		l.cancel()
	}
	if l.snapshot != nil {
		status := cloneMap(l.snapshot.Status)
		for kind, value := range status {
			if value.State == MetadataPending || value.State == MetadataLoading {
				value.State, value.FinishedAt = MetadataCancelled, time.Now()
				status[kind] = value
			}
		}
		l.snapshot = &MetadataSnapshot{Generation: l.generation, Revision: l.snapshot.Revision + 1, Started: l.snapshot.Started, StartFailed: l.snapshot.StartFailed, Cache: l.snapshot.Cache, Status: status}
		l.closeDoneLocked()
	}
	l.generation, l.started, l.startFailed = generation, false, false
	l.ctx, l.cancel = nil, nil
	l.done, l.doneClosed = nil, false
	l.snapshot = &MetadataSnapshot{Generation: generation, Revision: 1, Cache: newMetadataCache(), Status: emptyMetadataStatus()}
	callback := l.callback
	l.mu.Unlock()
	callMetadataCallback(callback)
}

// MarkStartFailed records that repository planning or metadata scheduling could
// not begin for this generation. It preserves the empty cache and unsupported
// category statuses because no job outcome is known, while making the snapshot
// terminal and degraded. The generation check prevents stale attachment work
// from overwriting a newer generation.
func (l *MetadataLoader) MarkStartFailed(generation uint64) error {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return ErrMetadataStopped
	}
	if generation != l.generation || l.snapshot == nil {
		l.mu.Unlock()
		return ErrMetadataStaleGeneration
	}
	if l.started || l.startFailed {
		l.mu.Unlock()
		return ErrMetadataAlreadyStarted
	}
	l.startFailed = true
	current := l.snapshot
	l.publishLocked(&MetadataSnapshot{
		Generation:  generation,
		Revision:    current.Revision + 1,
		Started:     false,
		StartFailed: true,
		Cache:       current.Cache,
		Status:      cloneMap(current.Status),
	})
	callback := l.callback
	l.mu.Unlock()
	callMetadataCallback(callback)
	return nil
}

func (l *MetadataLoader) Start(ctx context.Context, generation uint64, repo DBRepository) (*MetadataLoad, error) {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return nil, ErrMetadataStopped
	}
	if generation != l.generation || l.snapshot == nil {
		l.mu.Unlock()
		return nil, ErrMetadataStaleGeneration
	}
	if l.started || l.startFailed {
		l.mu.Unlock()
		return nil, ErrMetadataAlreadyStarted
	}
	l.mu.Unlock()

	plan := metadataPlanFor(repo)
	if err := validateMetadataPlan(plan); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMetadataPlan, err)
	}

	// Planning and validation are repository-controlled work. Recheck the
	// lifecycle state after that work before committing this generation's start.
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return nil, ErrMetadataStopped
	}
	if generation != l.generation || l.snapshot == nil {
		l.mu.Unlock()
		return nil, ErrMetadataStaleGeneration
	}
	if l.started || l.startFailed {
		l.mu.Unlock()
		return nil, ErrMetadataAlreadyStarted
	}
	if ctx == nil {
		ctx = context.Background()
	}
	jobCtx, cancel := context.WithCancel(ctx)
	l.ctx, l.cancel, l.started = jobCtx, cancel, true
	l.done, l.doneClosed = make(chan struct{}), false
	status := emptyMetadataStatus()
	now := time.Now()
	for _, job := range plan.Jobs {
		status[job.Kind] = MetadataStatus{State: MetadataPending, QueuedAt: now}
	}
	l.publishLocked(&MetadataSnapshot{Generation: generation, Revision: l.snapshot.Revision + 1, Started: true, Cache: l.snapshot.Cache, Status: status})
	done := l.done
	callback := l.callback
	if l.active == 0 {
		l.drained = make(chan struct{})
	}
	l.active++
	l.mu.Unlock()
	callMetadataCallback(callback)
	go func() { defer l.runnerFinished(); l.schedule(jobCtx, generation, plan, done) }()
	return &MetadataLoad{Done: done}, nil
}

func validateMetadataPlan(plan MetadataPlan) error {
	if plan.Parallelism < 1 || plan.Parallelism > 3 {
		return fmt.Errorf("parallelism must be in 1..3")
	}
	indices := make(map[MetadataKind]int, len(plan.Jobs))
	for i, job := range plan.Jobs {
		if !knownMetadataKind(job.Kind) {
			return fmt.Errorf("unknown kind %q", job.Kind)
		}
		if _, exists := indices[job.Kind]; exists {
			return fmt.Errorf("duplicate kind %q", job.Kind)
		}
		if job.Run == nil {
			return fmt.Errorf("nil Run for %q", job.Kind)
		}
		indices[job.Kind] = i
	}
	for _, job := range plan.Jobs {
		seen := make(map[MetadataKind]bool, len(job.DependsOn))
		for _, dependency := range job.DependsOn {
			if _, exists := indices[dependency]; !exists {
				return fmt.Errorf("missing dependency %q", dependency)
			}
			if dependency == job.Kind || seen[dependency] {
				if dependency == job.Kind {
					return fmt.Errorf("cycle at %q", job.Kind)
				}
				return fmt.Errorf("duplicate dependency %q", dependency)
			}
			seen[dependency] = true
		}
	}
	// Kahn's algorithm keeps validation explicit and handles arbitrary cycles.
	remaining := make(map[MetadataKind]int, len(plan.Jobs))
	for _, job := range plan.Jobs {
		remaining[job.Kind] = len(job.DependsOn)
	}
	for done := 0; done < len(plan.Jobs); {
		progress := false
		for _, job := range plan.Jobs {
			if remaining[job.Kind] != 0 {
				continue
			}
			remaining[job.Kind] = -1
			done++
			progress = true
			for _, dependent := range plan.Jobs {
				for _, dep := range dependent.DependsOn {
					if dep == job.Kind && remaining[dependent.Kind] > 0 {
						remaining[dependent.Kind]--
					}
				}
			}
		}
		if !progress {
			return fmt.Errorf("dependency cycle")
		}
	}
	return nil
}

type metadataResult struct {
	kind  MetadataKind
	patch MetadataPatch
	err   error
}

func (l *MetadataLoader) schedule(ctx context.Context, generation uint64, plan MetadataPlan, done chan struct{}) {
	results := make(chan metadataResult, len(plan.Jobs))
	states := make(map[MetadataKind]MetadataState, len(plan.Jobs))
	for _, job := range plan.Jobs {
		states[job.Kind] = MetadataPending
	}
	running, terminal := 0, 0
	for terminal < len(plan.Jobs) {
		if ctx.Err() != nil {
			l.settleCancelled(generation)
			return
		}
		// Take the wake generation before attempting acquisitions. A release
		// after this point broadcasts to every scheduler waiting on this epoch.
		wake := l.slotWakeChannel()
		// A failed prerequisite blocks only its transitive dependents.
		changed := true
		for changed {
			changed = false
			for _, job := range plan.Jobs {
				if states[job.Kind] != MetadataPending {
					continue
				}
				for _, dep := range job.DependsOn {
					if states[dep] == MetadataFailed || states[dep] == MetadataBlocked || states[dep] == MetadataCancelled {
						states[job.Kind] = MetadataBlocked
						terminal++
						l.setStatus(generation, job.Kind, MetadataStatus{State: MetadataBlocked, FinishedAt: time.Now(), Err: fmt.Errorf("dependency %s did not succeed", dep)}, nil)
						changed = true
						break
					}
				}
			}
		}
		launched := false
		for _, job := range plan.Jobs {
			if running >= plan.Parallelism {
				break
			}
			if states[job.Kind] != MetadataPending || !dependenciesReady(job, states) {
				continue
			}
			select {
			case l.semaphore <- struct{}{}:
			default:
				continue
			}
			if ctx.Err() != nil {
				l.releaseMetadataSlot()
				l.settleCancelled(generation)
				return
			}
			cache, admitted := l.admitJob(generation, ctx, job.Kind)
			if !admitted {
				l.releaseMetadataSlot()
				l.settleCancelled(generation)
				return
			}
			states[job.Kind] = MetadataLoading
			running++
			launched = true
			go func(job MetadataJob, cache *DBCache) {
				defer func() {
					l.releaseMetadataSlot()
					l.runnerFinished()
				}()
				if ctx.Err() != nil {
					return
				}
				result := metadataResult{kind: job.Kind}
				func() {
					defer func() {
						if recovered := recover(); recovered != nil {
							result.err = fmt.Errorf("metadata job %s panicked: %v", job.Kind, recovered)
							log.Printf("metadata job %s panic: %v\n%s", job.Kind, recovered, debug.Stack())
						}
					}()
					result.patch, result.err = job.Run(ctx, cache)
				}()
				results <- result
			}(job, cache)
		}
		if terminal == len(plan.Jobs) {
			break
		}
		if running == 0 && !launched {
			// Pending work is waiting for a global slot held by another generation.
			select {
			case <-ctx.Done():
				l.settleCancelled(generation)
				return
			case <-wake:
				continue
			}
		}
		select {
		case <-ctx.Done():
			l.settleCancelled(generation)
			return
		case <-wake:
			continue
		case result := <-results:
			running--
			if ctx.Err() != nil {
				l.settleCancelled(generation)
				return
			}
			states[result.kind] = MetadataFailed
			status := MetadataStatus{State: MetadataFailed, FinishedAt: time.Now(), Count: result.patch.Count, Queries: result.patch.Queries, QueriesKnown: result.patch.QueriesKnown, Err: result.err}
			if result.err == nil {
				cache := l.Cache()
				merged, err := mergeMetadata(cache, result.kind, result.patch)
				if err != nil {
					status.Err = err
				} else {
					states[result.kind] = MetadataReady
					status.State = MetadataReady
					l.setStatus(generation, result.kind, status, merged)
					terminal++
					continue
				}
			}
			l.setStatus(generation, result.kind, status, nil)
			terminal++
		}
	}
	l.finish(generation)
}

// admitJob is the launch linearization point. The generation, cancellation
// state, status transition, and cache input are checked/captured together so a
// reset cannot make an old scheduler hand a new generation's cache to a job.
func (l *MetadataLoader) admitJob(generation uint64, ctx context.Context, kind MetadataKind) (*DBCache, bool) {
	l.mu.Lock()
	if l.stopped || !l.started || l.generation != generation || l.snapshot == nil || l.ctx == nil || l.ctx.Err() != nil || ctx == nil || ctx.Err() != nil {
		l.mu.Unlock()
		return nil, false
	}
	current := l.snapshot
	statuses := cloneMap(current.Status)
	status := statuses[kind]
	if status.State != MetadataPending {
		l.mu.Unlock()
		return nil, false
	}
	status.State, status.StartedAt = MetadataLoading, time.Now()
	statuses[kind] = status
	l.publishLocked(&MetadataSnapshot{Generation: generation, Revision: current.Revision + 1, Started: current.Started, Cache: current.Cache, Status: statuses})
	if l.active == 0 {
		l.drained = make(chan struct{})
	}
	l.active++
	cache, callback := current.Cache, l.callback
	l.mu.Unlock()
	callMetadataCallback(callback)
	return cache, true
}

func (l *MetadataLoader) slotWakeChannel() <-chan struct{} {
	l.mu.Lock()
	wake := l.slotWake
	l.mu.Unlock()
	return wake
}

func (l *MetadataLoader) signalSlotAvailable() {
	l.mu.Lock()
	previous := l.slotWake
	l.slotWake = make(chan struct{})
	close(previous)
	l.mu.Unlock()
}

// releaseMetadataSlot releases one loader-wide runner permit and wakes every
// generation that may be waiting to acquire it.
func (l *MetadataLoader) releaseMetadataSlot() {
	<-l.semaphore
	l.signalSlotAvailable()
}

func dependenciesReady(job MetadataJob, states map[MetadataKind]MetadataState) bool {
	for _, dep := range job.DependsOn {
		if states[dep] != MetadataReady {
			return false
		}
	}
	return true
}

func (l *MetadataLoader) runnerFinished() {
	l.mu.Lock()
	l.active--
	if l.active == 0 {
		close(l.drained)
	}
	l.mu.Unlock()
}

func (l *MetadataLoader) setStatus(generation uint64, kind MetadataKind, status MetadataStatus, cache *DBCache) {
	l.mu.Lock()
	if l.generation != generation || l.snapshot == nil || l.ctx == nil || l.ctx.Err() != nil {
		l.mu.Unlock()
		return
	}
	current := l.snapshot
	statuses := cloneMap(current.Status)
	old := statuses[kind]
	if old.State == MetadataReady || terminalMetadataState(old.State) {
		l.mu.Unlock()
		return
	}
	if status.State != MetadataLoading {
		status.QueuedAt = old.QueuedAt
		if status.StartedAt.IsZero() {
			status.StartedAt = old.StartedAt
		}
	}
	if cache == nil {
		cache = current.Cache
	}
	next := &MetadataSnapshot{Generation: generation, Revision: current.Revision + 1, Started: current.Started, Cache: cache, Status: statuses}
	statuses[kind] = status
	l.publishLocked(next)
	callback := l.callback
	l.mu.Unlock()
	callMetadataCallback(callback)
}

func terminalMetadataState(state MetadataState) bool {
	return state == MetadataReady || state == MetadataFailed || state == MetadataBlocked || state == MetadataCancelled || state == MetadataUnsupported
}

func (l *MetadataLoader) settleCancelled(generation uint64) {
	l.mu.Lock()
	if l.generation != generation || l.snapshot == nil {
		l.mu.Unlock()
		return
	}
	statuses := cloneMap(l.snapshot.Status)
	changed := false
	for kind, status := range statuses {
		if status.State == MetadataPending || status.State == MetadataLoading {
			status.State, status.FinishedAt = MetadataCancelled, time.Now()
			statuses[kind] = status
			changed = true
		}
	}
	if changed {
		l.publishLocked(&MetadataSnapshot{Generation: generation, Revision: l.snapshot.Revision + 1, Started: l.snapshot.Started, Cache: l.snapshot.Cache, Status: statuses})
	}
	l.closeDoneLocked()
	callback := l.callback
	l.mu.Unlock()
	if changed {
		callMetadataCallback(callback)
	}
}

func (l *MetadataLoader) finish(generation uint64) {
	l.mu.Lock()
	if l.generation == generation {
		l.closeDoneLocked()
	}
	l.mu.Unlock()
}

func (l *MetadataLoader) closeDoneLocked() {
	if l.done != nil && !l.doneClosed {
		close(l.done)
		l.doneClosed = true
	}
}
func (l *MetadataLoader) publishLocked(snapshot *MetadataSnapshot) { l.snapshot = snapshot }

func (l *MetadataLoader) Snapshot() *MetadataSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.snapshot == nil {
		return &MetadataSnapshot{Cache: newMetadataCache(), Status: emptyMetadataStatus()}
	}
	return l.snapshot
}
func (l *MetadataLoader) Cache() *DBCache {
	snapshot := l.Snapshot()
	if snapshot.Cache != nil {
		return snapshot.Cache
	}
	return newMetadataCache()
}

// SetChangedCallback installs a notification hook. Hooks run synchronously but
// outside the loader lock, and should enqueue work rather than block or do I/O.
func (l *MetadataLoader) SetChangedCallback(callback func()) {
	l.mu.Lock()
	l.callback = callback
	l.mu.Unlock()
}

// Stop cancels current work and settles it logically without waiting for native
// calls. Wait can be used separately when actual runner drainage is required.
func (l *MetadataLoader) Stop() {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return
	}
	l.stopped = true
	if l.cancel != nil {
		l.cancel()
	}
	if l.snapshot != nil {
		statuses := cloneMap(l.snapshot.Status)
		changed := false
		for kind, status := range statuses {
			if status.State == MetadataPending || status.State == MetadataLoading {
				status.State, status.FinishedAt = MetadataCancelled, time.Now()
				statuses[kind] = status
				changed = true
			}
		}
		if changed {
			l.publishLocked(&MetadataSnapshot{Generation: l.generation, Revision: l.snapshot.Revision + 1, Started: l.snapshot.Started, Cache: l.snapshot.Cache, Status: statuses})
		}
	}
	l.closeDoneLocked()
	callback := l.callback
	l.mu.Unlock()
	callMetadataCallback(callback)
}

func (l *MetadataLoader) Wait(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	l.mu.Lock()
	drained := l.drained
	l.mu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func callMetadataCallback(callback func()) {
	if callback != nil {
		callback()
	}
}
