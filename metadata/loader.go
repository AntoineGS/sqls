// Package metadata holds the engine-independent metadata generation
// scheduler, built on the neutral models in package catalog.
package metadata

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sqls-server/sqls/catalog"
)

var (
	ErrMetadataStaleGeneration = errors.New("stale metadata generation")
	ErrMetadataAlreadyStarted  = errors.New("metadata generation already started")
	ErrMetadataStopped         = errors.New("metadata loader stopped")
	ErrInvalidMetadataPlan     = errors.New("invalid metadata plan")
)

// Load represents logical settlement of one generation. Native job calls may
// still be draining after Done closes; use Loader.Wait for it.
type Load struct{ Done <-chan struct{} }

// Loader schedules one metadata generation's category jobs against a
// supplied catalog.MetadataPlan and publishes immutable snapshots as they
// settle.
type Loader struct {
	mu          sync.Mutex
	snapshot    *catalog.MetadataSnapshot
	generation  uint64
	started     bool
	startFailed bool
	stopped     bool
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	doneClosed  bool
	callback    func()

	// semaphore and hostSemaphore belong to the supplied StoreBudget, not a
	// generation or this Loader alone: they are the SAME channels a sibling
	// loader for another scope of this Key (or a retiring/replacement
	// incarnation) uses, so store- and host-wide concurrency are genuinely
	// shared. A cancelled native call keeps its slots until the Run function
	// actually returns.
	//
	// storeWake/hostWake are the SAME broadcast wakers the owning StoreBudget
	// hands to every other loader and detail load sharing these semaphores
	// (see budget.go's waker). schedule's poll-and-park loop below selects on
	// both, so a release from ANY of them — a sibling scope's loader, a
	// retiring incarnation's loader, a detail load, or a loader for a
	// completely different Key releasing the shared host semaphore — wakes
	// this loader, not just its own releases.
	semaphore     chan struct{}
	hostSemaphore chan struct{}
	storeWake     *waker
	hostWake      *waker
	active        int
	drained       chan struct{}

	// acquireAttempts counts every tryAcquireSlot call regardless of
	// outcome. It exists purely so a whitebox test can distinguish a
	// genuinely parked scheduler loop from a busy-spinning one (a parked
	// loop attempts acquisition a handful of times at most per wake; a
	// spinning one attempts thousands of times per second); it is never
	// read by production code.
	acquireAttempts atomic.Int64
}

// NewLoader creates a Loader whose job admission draws from budget's shared
// store- and host-level permits. A nil budget gets a private, unshared
// default budget (matching this package's original, registry-independent
// behavior) rather than failing.
func NewLoader(budget *StoreBudget) *Loader {
	if budget == nil {
		budget = NewBudget(0, 0).ForStore(Key{})
	}
	drained := make(chan struct{})
	close(drained)
	return &Loader{
		semaphore: budget.sem, hostSemaphore: budget.host,
		storeWake: budget.storeWake, hostWake: budget.hostWake,
		drained: drained,
	}
}

var metadataKinds = []catalog.MetadataKind{
	catalog.MetadataSchemas, catalog.MetadataRelations, catalog.MetadataColumnsCurrent, catalog.MetadataColumnsAll,
	catalog.MetadataPrimaryKeys, catalog.MetadataForeignKeys, catalog.MetadataViews, catalog.MetadataProcedures,
	catalog.MetadataGenerators, catalog.MetadataSystemRelations, catalog.MetadataExceptions,
	catalog.MetadataDomains, catalog.MetadataFunctions, catalog.MetadataIndexes, catalog.MetadataTriggers,
	catalog.MetadataDependencies,
}

// installSettled installs a fully built immutable generation without category
// jobs, permits or I/O. Only a fresh loader can accept this staged snapshot.
func (l *Loader) installSettled(ctx context.Context, snapshot *catalog.MetadataSnapshot) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if snapshot == nil || snapshot.Generation == 0 || snapshot.Cache == nil || !snapshot.Started || snapshot.StartFailed || !snapshot.Settled() {
		return ErrWatchIncomplete
	}
	for kind, s := range snapshot.Status {
		if !knownMetadataKind(kind) || s.Count < 0 || s.Queries < 0 {
			return ErrWatchIncomplete
		}
		if s.State == catalog.MetadataReady && snapshot.Cache.Metadata[kind] != catalog.MetadataReady {
			return ErrWatchIncomplete
		}
	}
	for kind, state := range snapshot.Cache.Metadata {
		if state == catalog.MetadataReady && snapshot.Status[kind].State != catalog.MetadataReady {
			return ErrWatchIncomplete
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		return ErrMetadataStopped
	}
	if l.started || l.snapshot != nil {
		return ErrMetadataAlreadyStarted
	}
	copy := *snapshot
	copy.Status = cloneMap(snapshot.Status)
	l.ctx, l.cancel = context.WithCancel(ctx)
	l.generation, l.started, l.snapshot = snapshot.Generation, true, &copy
	l.done = make(chan struct{})
	close(l.done)
	l.doneClosed = true
	return nil
}

func emptyMetadataStatus() map[catalog.MetadataKind]catalog.MetadataStatus {
	status := make(map[catalog.MetadataKind]catalog.MetadataStatus, len(metadataKinds))
	for _, kind := range metadataKinds {
		status[kind] = catalog.MetadataStatus{State: catalog.MetadataUnsupported}
	}
	return status
}

// Reset installs an empty generation. Generations are strictly increasing;
// stale and duplicate reset requests are ignored.
func (l *Loader) Reset(generation uint64) {
	l.mu.Lock()
	if l.stopped || generation <= l.generation {
		l.mu.Unlock()
		return
	}
	if l.cancel != nil {
		l.cancel()
	}
	var retired *catalog.MetadataSnapshot
	if l.snapshot != nil {
		status := cloneMap(l.snapshot.Status)
		changed := false
		for kind, value := range status {
			if value.State == catalog.MetadataPending || value.State == catalog.MetadataLoading {
				value.State, value.FinishedAt = catalog.MetadataCancelled, time.Now()
				status[kind] = value
				changed = true
			}
		}
		l.snapshot = &catalog.MetadataSnapshot{Generation: l.generation, Revision: l.snapshot.Revision + 1, Started: l.snapshot.Started, StartFailed: l.snapshot.StartFailed, Stale: l.snapshot.Stale, Cache: l.snapshot.Cache, Status: status}
		if changed {
			retired = l.snapshot
		}
		l.closeDoneLocked()
	}
	l.generation, l.started, l.startFailed = generation, false, false
	l.ctx, l.cancel = nil, nil
	l.done, l.doneClosed = nil, false
	// Stale is explicitly reset to false here: this is the one path
	// (Refresh, via Lease.Refresh -> beginGeneration then Reset) that
	// intentionally clears it for the new generation. Every other publish
	// site in this file must instead carry the current value forward.
	l.snapshot = &catalog.MetadataSnapshot{Generation: generation, Revision: 1, Stale: false, Cache: newMetadataCache(), Status: emptyMetadataStatus()}
	callback := l.callback
	l.mu.Unlock()
	if retired != nil {
		// Emit after unlocking so logging cannot block lifecycle transitions. A
		// concurrent new generation may therefore log before this retired one.
		logCancelledMetadataStatuses(retired)
		logMetadataGeneration(retired)
	}
	callMetadataCallback(callback)
}

// MarkStartFailed records that repository planning or metadata scheduling could
// not begin for this generation. It preserves the empty cache and unsupported
// category statuses because no job outcome is known, while making the snapshot
// terminal and degraded. The generation check prevents stale attachment work
// from overwriting a newer generation.
func (l *Loader) MarkStartFailed(generation uint64) error {
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
	l.publishLocked(&catalog.MetadataSnapshot{
		Generation:  generation,
		Revision:    current.Revision + 1,
		Started:     false,
		StartFailed: true,
		Stale:       current.Stale,
		Cache:       current.Cache,
		Status:      cloneMap(current.Status),
	})
	callback := l.callback
	l.mu.Unlock()
	callMetadataCallback(callback)
	return nil
}

// Start begins scheduling plan's jobs for generation. The caller is
// responsible for producing plan (for example via an engine-specific
// adapter); the loader itself is engine-independent and never derives a plan
// on its own.
func (l *Loader) Start(ctx context.Context, generation uint64, plan catalog.MetadataPlan) (*Load, error) {
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

	if err := validateMetadataPlan(plan); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMetadataPlan, err)
	}

	// Recheck the lifecycle state after validation before committing this
	// generation's start.
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
		status[job.Kind] = catalog.MetadataStatus{State: catalog.MetadataPending, QueuedAt: now}
	}
	l.publishLocked(&catalog.MetadataSnapshot{Generation: generation, Revision: l.snapshot.Revision + 1, Started: true, Stale: l.snapshot.Stale, Cache: l.snapshot.Cache, Status: status})
	done := l.done
	callback := l.callback
	if l.active == 0 {
		l.drained = make(chan struct{})
	}
	l.active++
	l.mu.Unlock()
	callMetadataCallback(callback)
	go func() { defer l.runnerFinished(); l.schedule(jobCtx, generation, plan, done) }()
	return &Load{Done: done}, nil
}

func validateMetadataPlan(plan catalog.MetadataPlan) error {
	if plan.Parallelism < 1 || plan.Parallelism > 3 {
		return fmt.Errorf("parallelism must be in 1..3")
	}
	indices := make(map[catalog.MetadataKind]int, len(plan.Jobs))
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
		seen := make(map[catalog.MetadataKind]bool, len(job.DependsOn))
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
	remaining := make(map[catalog.MetadataKind]int, len(plan.Jobs))
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
	kind  catalog.MetadataKind
	patch catalog.MetadataPatch
	err   error
}

func (l *Loader) schedule(ctx context.Context, generation uint64, plan catalog.MetadataPlan, done chan struct{}) {
	results := make(chan metadataResult, len(plan.Jobs))
	states := make(map[catalog.MetadataKind]catalog.MetadataState, len(plan.Jobs))
	for _, job := range plan.Jobs {
		states[job.Kind] = catalog.MetadataPending
	}
	running, terminal := 0, 0
	for terminal < len(plan.Jobs) {
		if ctx.Err() != nil {
			l.settleCancelled(generation)
			return
		}
		// Take the wake epochs before attempting acquisitions. A release
		// after this point — from this loader, a sibling loader sharing the
		// same store permits, a detail load, or any loader anywhere sharing
		// the host permit — broadcasts to every scheduler waiting on either
		// epoch, so neither is a private signal only this loader can fire.
		storeWakeCh := l.storeWake.wait()
		hostWakeCh := l.hostWake.wait()
		// A failed prerequisite blocks only its transitive dependents.
		changed := true
		for changed {
			changed = false
			for _, job := range plan.Jobs {
				if states[job.Kind] != catalog.MetadataPending {
					continue
				}
				for _, dep := range job.DependsOn {
					if states[dep] == catalog.MetadataFailed || states[dep] == catalog.MetadataBlocked || states[dep] == catalog.MetadataCancelled {
						states[job.Kind] = catalog.MetadataBlocked
						terminal++
						l.setStatus(generation, job.Kind, catalog.MetadataStatus{State: catalog.MetadataBlocked, FinishedAt: time.Now(), Err: fmt.Errorf("dependency %s did not succeed", dep)}, nil)
						changed = true
						break
					}
				}
			}
		}
		launched := false
		sawStoreFull := false
		sawHostFull := false
		for _, job := range plan.Jobs {
			if running >= plan.Parallelism {
				break
			}
			if states[job.Kind] != catalog.MetadataPending || !dependenciesReady(job, states) {
				continue
			}
			switch l.tryAcquireSlot() {
			case slotStoreFull:
				sawStoreFull = true
				continue
			case slotHostFull:
				sawHostFull = true
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
			states[job.Kind] = catalog.MetadataLoading
			running++
			launched = true
			go func(job catalog.MetadataJob, cache *catalog.Cache) {
				defer func() {
					l.releaseMetadataSlot()
					l.runnerFinished()
				}()
				if ctx.Err() != nil {
					return
				}
				if !l.markMetadataJobStarted(generation, ctx, job.Kind) {
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
				l.recordMetadataJobMetrics(generation, job.Kind, result.patch)
				results <- result
			}(job, cache)
		}
		if terminal == len(plan.Jobs) {
			break
		}
		// If every failed attempt this iteration was a host-triggered
		// rollback (no plain store-full attempt also occurred), our own
		// rollback already closed the top-captured storeWakeCh epoch: park
		// only on hostWakeCh (a nil channel case never becomes ready, so
		// this disables the storeWakeCh case below without complicating its
		// structure) rather than immediately re-waking on a signal we just
		// fired ourselves, for a permit-set change that could not admit a
		// runner anyway while the host stays full. hostWakeCh itself is
		// never touched by this rollback, so it is always safe to keep.
		parkStoreWakeCh := storeWakeCh
		if sawHostFull && !sawStoreFull {
			parkStoreWakeCh = nil
		}
		if running == 0 && !launched {
			// Pending work is waiting for a global slot held by another generation.
			select {
			case <-ctx.Done():
				l.settleCancelled(generation)
				return
			case <-parkStoreWakeCh:
				continue
			case <-hostWakeCh:
				continue
			}
		}
		select {
		case <-ctx.Done():
			l.settleCancelled(generation)
			return
		case <-parkStoreWakeCh:
			continue
		case <-hostWakeCh:
			continue
		case result := <-results:
			running--
			if ctx.Err() != nil {
				l.settleCancelled(generation)
				return
			}
			states[result.kind] = catalog.MetadataFailed
			status := catalog.MetadataStatus{State: catalog.MetadataFailed, FinishedAt: time.Now(), Count: result.patch.Count, Queries: result.patch.Queries, QueriesKnown: result.patch.QueriesKnown, Err: result.err}
			if result.err == nil {
				cache := l.Cache()
				merged, err := mergeMetadata(cache, result.kind, result.patch)
				if err != nil {
					status.Err = err
				} else {
					states[result.kind] = catalog.MetadataReady
					status.State = catalog.MetadataReady
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
// StartedAt is recorded by the runner immediately before invoking Run.
func (l *Loader) admitJob(generation uint64, ctx context.Context, kind catalog.MetadataKind) (*catalog.Cache, bool) {
	l.mu.Lock()
	if l.stopped || !l.started || l.generation != generation || l.snapshot == nil || l.ctx == nil || l.ctx.Err() != nil || ctx == nil || ctx.Err() != nil {
		l.mu.Unlock()
		return nil, false
	}
	current := l.snapshot
	statuses := cloneMap(current.Status)
	status := statuses[kind]
	if status.State != catalog.MetadataPending {
		l.mu.Unlock()
		return nil, false
	}
	status.State = catalog.MetadataLoading
	statuses[kind] = status
	l.publishLocked(&catalog.MetadataSnapshot{Generation: generation, Revision: current.Revision + 1, Started: current.Started, Stale: current.Stale, Cache: current.Cache, Status: statuses})
	if l.active == 0 {
		l.drained = make(chan struct{})
	}
	l.active++
	cache, callback := current.Cache, l.callback
	l.mu.Unlock()
	callMetadataCallback(callback)
	return cache, true
}

func (l *Loader) markMetadataJobStarted(generation uint64, ctx context.Context, kind catalog.MetadataKind) bool {
	l.mu.Lock()
	if l.stopped || generation != l.generation || l.snapshot == nil || ctx == nil || ctx.Err() != nil {
		l.mu.Unlock()
		return false
	}
	current := l.snapshot
	statuses := cloneMap(current.Status)
	status := statuses[kind]
	if status.State != catalog.MetadataLoading || !status.StartedAt.IsZero() {
		l.mu.Unlock()
		return false
	}
	status.StartedAt = time.Now()
	statuses[kind] = status
	l.publishLocked(&catalog.MetadataSnapshot{Generation: generation, Revision: current.Revision + 1, Started: current.Started, Stale: current.Stale, Cache: current.Cache, Status: statuses})
	l.mu.Unlock()
	return true
}

// recordMetadataJobMetrics stores the runner's outcome before it hands the
// result to the scheduler. Cancellation may settle the generation first; in
// that case retain only metrics in the same generation and never publish cache.
func (l *Loader) recordMetadataJobMetrics(generation uint64, kind catalog.MetadataKind, patch catalog.MetadataPatch) {
	l.mu.Lock()
	if generation != l.generation || l.snapshot == nil {
		l.mu.Unlock()
		return
	}
	current := l.snapshot
	statuses := cloneMap(current.Status)
	status, ok := statuses[kind]
	if !ok || (status.State != catalog.MetadataLoading && status.State != catalog.MetadataCancelled) {
		l.mu.Unlock()
		return
	}
	status.Count = patch.Count
	status.Queries, status.QueriesKnown = patch.Queries, patch.QueriesKnown
	statuses[kind] = status
	l.publishLocked(&catalog.MetadataSnapshot{Generation: generation, Revision: current.Revision + 1, Started: current.Started, Stale: current.Stale, Cache: current.Cache, Status: statuses})
	callback := l.callback
	l.mu.Unlock()
	callMetadataCallback(callback)
}

// slotOutcome reports which permit set, if any, blocked one tryAcquireSlot
// attempt. schedule uses this to decide what it actually still needs to wait
// for: waiting on a signal that can never unblock it (see slotHostFull's use
// in schedule) is what caused the busy-spin this type exists to prevent.
type slotOutcome int

const (
	slotAcquired  slotOutcome = iota // both permits held; caller must release later
	slotStoreFull                    // never reached the host check; a store-level release could help
	slotHostFull                     // store permit was taken then rolled back; only a host-level release can help
)

// tryAcquireSlot makes one non-blocking attempt to admit a runner: it needs
// both a store-level and a host-level permit, acquired in that order, and
// rolls back the store permit if the host is momentarily full. A rollback
// broadcasts storeWake: releasing a permit this loader itself briefly held
// must wake any OTHER waiter for it exactly like any other release would.
// The caller (schedule) must NOT itself re-arm on that same storeWake epoch
// for a slotHostFull outcome: this rollback already consumed it, and a
// plain store-level change can never admit a runner while the host stays
// full (admission needs both), so waiting on it here would just spin.
func (l *Loader) tryAcquireSlot() slotOutcome {
	l.acquireAttempts.Add(1)
	select {
	case l.semaphore <- struct{}{}:
	default:
		return slotStoreFull
	}
	select {
	case l.hostSemaphore <- struct{}{}:
		return slotAcquired
	default:
		<-l.semaphore
		l.storeWake.broadcast()
		return slotHostFull
	}
}

// releaseMetadataSlot releases one loader-wide runner permit (host and
// store) and broadcasts both wakers so every loader or detail load anywhere
// sharing either permit — not just this one — wakes to retry.
func (l *Loader) releaseMetadataSlot() {
	<-l.hostSemaphore
	l.hostWake.broadcast()
	<-l.semaphore
	l.storeWake.broadcast()
}

func dependenciesReady(job catalog.MetadataJob, states map[catalog.MetadataKind]catalog.MetadataState) bool {
	for _, dep := range job.DependsOn {
		if states[dep] != catalog.MetadataReady {
			return false
		}
	}
	return true
}

func (l *Loader) runnerFinished() {
	l.mu.Lock()
	l.active--
	if l.active == 0 {
		close(l.drained)
	}
	l.mu.Unlock()
}

func (l *Loader) setStatus(generation uint64, kind catalog.MetadataKind, status catalog.MetadataStatus, cache *catalog.Cache) {
	l.mu.Lock()
	if l.generation != generation || l.snapshot == nil || l.ctx == nil || l.ctx.Err() != nil {
		l.mu.Unlock()
		return
	}
	current := l.snapshot
	statuses := cloneMap(current.Status)
	old := statuses[kind]
	if old.State == catalog.MetadataReady || terminalMetadataState(old.State) {
		l.mu.Unlock()
		return
	}
	if status.State != catalog.MetadataLoading {
		status.QueuedAt = old.QueuedAt
		if status.StartedAt.IsZero() {
			status.StartedAt = old.StartedAt
		}
	}
	if cache == nil {
		cache = current.Cache
	}
	next := &catalog.MetadataSnapshot{Generation: generation, Revision: current.Revision + 1, Started: current.Started, Stale: current.Stale, Cache: cache, Status: statuses}
	statuses[kind] = status
	l.publishLocked(next)
	callback := l.callback
	terminal := terminalMetadataState(status.State)
	l.mu.Unlock()
	if terminal {
		logMetadataJob(generation, kind, status)
	}
	callMetadataCallback(callback)
}

func terminalMetadataState(state catalog.MetadataState) bool {
	return state == catalog.MetadataReady || state == catalog.MetadataFailed || state == catalog.MetadataBlocked || state == catalog.MetadataCancelled || state == catalog.MetadataUnsupported
}

func (l *Loader) settleCancelled(generation uint64) {
	l.mu.Lock()
	if l.generation != generation || l.snapshot == nil {
		l.mu.Unlock()
		return
	}
	statuses := cloneMap(l.snapshot.Status)
	changed := false
	for kind, status := range statuses {
		if status.State == catalog.MetadataPending || status.State == catalog.MetadataLoading {
			status.State, status.FinishedAt = catalog.MetadataCancelled, time.Now()
			statuses[kind] = status
			changed = true
		}
	}
	if changed {
		l.publishLocked(&catalog.MetadataSnapshot{Generation: generation, Revision: l.snapshot.Revision + 1, Started: l.snapshot.Started, Stale: l.snapshot.Stale, Cache: l.snapshot.Cache, Status: statuses})
	}
	l.closeDoneLocked()
	snapshot := l.snapshot
	callback := l.callback
	l.mu.Unlock()
	if changed {
		for kind, status := range statuses {
			if status.State == catalog.MetadataCancelled {
				logMetadataJob(generation, kind, status)
			}
		}
		logMetadataGeneration(snapshot)
		callMetadataCallback(callback)
	}
}

func (l *Loader) finish(generation uint64) {
	l.mu.Lock()
	var snapshot *catalog.MetadataSnapshot
	if l.generation == generation {
		l.closeDoneLocked()
		snapshot = l.snapshot
	}
	l.mu.Unlock()
	logMetadataGeneration(snapshot)
}

func (l *Loader) closeDoneLocked() {
	if l.done != nil && !l.doneClosed {
		close(l.done)
		l.doneClosed = true
	}
}
func (l *Loader) publishLocked(snapshot *catalog.MetadataSnapshot) { l.snapshot = snapshot }

// MarkStale flags the current snapshot Stale and cancels this generation's
// in-flight category jobs (transitioning any Pending/Loading status to
// Cancelled), without starting anything new: no new generation, no new job.
// It is idempotent. It never changes Generation. Reset/Start (via Refresh)
// is the only path that clears Stale back to false, for the next generation.
//
// The Cache itself is deliberately left intact (not emptied): "make the
// authoritative cache empty" is scoped, by this task's rulings, to
// detail-read purposes — Lease.Invalidate separately empties the detail
// store (see detailStore.markStale) and detail reads already return
// ErrStale via that path. Leaving the bulk Cache/Status here lets a reader
// of Snapshot still see the last known category data (just flagged Stale)
// during the window before Refresh completes, rather than presenting a
// blank tree. See the task report for the full reasoning.
func (l *Loader) MarkStale() {
	l.markStaleDeferred()()
}

// The track uses the deferred notification to make epoch, snapshot and detail
// invalidation atomic without invoking its callback while holding track.mu.
func (l *Loader) markStaleDeferred() func() {
	l.mu.Lock()
	if l.stopped || l.snapshot == nil || l.snapshot.Stale {
		l.mu.Unlock()
		return func() {}
	}
	if l.cancel != nil {
		l.cancel()
	}
	current := l.snapshot
	statuses := cloneMap(current.Status)
	changed := false
	for kind, status := range statuses {
		if status.State == catalog.MetadataPending || status.State == catalog.MetadataLoading {
			status.State, status.FinishedAt = catalog.MetadataCancelled, time.Now()
			statuses[kind] = status
			changed = true
		}
	}
	next := &catalog.MetadataSnapshot{
		Generation: current.Generation, Revision: current.Revision + 1,
		Started: current.Started, StartFailed: current.StartFailed, Stale: true,
		Cache: current.Cache, Status: statuses,
	}
	l.snapshot = next
	l.closeDoneLocked()
	callback := l.callback
	l.mu.Unlock()
	return func() {
		if changed {
			logCancelledMetadataStatuses(next)
			logMetadataGeneration(next)
		}
		callMetadataCallback(callback)
	}
}

func (l *Loader) Snapshot() *catalog.MetadataSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.snapshot == nil {
		return &catalog.MetadataSnapshot{Cache: newMetadataCache(), Status: emptyMetadataStatus()}
	}
	return l.snapshot
}
func (l *Loader) Cache() *catalog.Cache {
	snapshot := l.Snapshot()
	if snapshot.Cache != nil {
		return snapshot.Cache
	}
	return newMetadataCache()
}

// SetChangedCallback installs a notification hook. Hooks run synchronously but
// outside the loader lock, and should enqueue work rather than block or do I/O.
func (l *Loader) SetChangedCallback(callback func()) {
	l.mu.Lock()
	l.callback = callback
	l.mu.Unlock()
}

// Stop cancels current work and settles it logically without waiting for native
// calls. Wait can be used separately when actual runner drainage is required.
func (l *Loader) Stop() {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return
	}
	l.stopped = true
	if l.cancel != nil {
		l.cancel()
	}
	var cancelled *catalog.MetadataSnapshot
	if l.snapshot != nil {
		statuses := cloneMap(l.snapshot.Status)
		changed := false
		for kind, status := range statuses {
			if status.State == catalog.MetadataPending || status.State == catalog.MetadataLoading {
				status.State, status.FinishedAt = catalog.MetadataCancelled, time.Now()
				statuses[kind] = status
				changed = true
			}
		}
		if changed {
			l.publishLocked(&catalog.MetadataSnapshot{Generation: l.generation, Revision: l.snapshot.Revision + 1, Started: l.snapshot.Started, Stale: l.snapshot.Stale, Cache: l.snapshot.Cache, Status: statuses})
			cancelled = l.snapshot
		}
	}
	l.closeDoneLocked()
	callback := l.callback
	l.mu.Unlock()
	if cancelled != nil {
		logCancelledMetadataStatuses(cancelled)
		logMetadataGeneration(cancelled)
	}
	callMetadataCallback(callback)
}

func (l *Loader) Wait(ctx context.Context) error {
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
