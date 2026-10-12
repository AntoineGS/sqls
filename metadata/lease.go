package metadata

import (
	"context"
	"fmt"
	"sync"

	"github.com/sqls-server/sqls/catalog"
)

// Lease is one consumer's handle onto a shared metadata store. Release must
// be called exactly once per successful Acquire; calling it more than once
// is safe and only the first call has effect.
type Lease struct {
	registry *Registry
	track    *track
	key      Key

	incarnation uint64

	releaseOnce sync.Once
}

// Key returns the (engine, normalized URL) this lease was acquired for.
func (l *Lease) Key() Key { return l.key }

// Incarnation identifies the store instance backing this lease. Two leases
// for the same Key observe the same Incarnation while the store they share
// is alive; a store recreated after every prior lease released gets a new,
// strictly greater Incarnation.
func (l *Lease) Incarnation() uint64 { return l.incarnation }

// Snapshot returns the store's current immutable metadata snapshot. It never
// blocks; before the underlying provider finishes creation it reports an
// unstarted snapshot.
func (l *Lease) Snapshot() *catalog.MetadataSnapshot {
	l.track.mu.Lock()
	loader := l.track.loader
	l.track.mu.Unlock()
	if loader == nil {
		return &catalog.MetadataSnapshot{Cache: NewCache(), Status: map[catalog.MetadataKind]catalog.MetadataStatus{}}
	}
	return loader.Snapshot()
}

// Detail loads and renders one object facet. Concurrent requests for the
// same provider-normalized Coverage (see DetailPlan) share exactly one Load;
// only the pure, I/O-free Render step (if any) runs per request, so a
// request that only differs by rendering options (for example Dialect) never
// triggers a second Load. A stale store (see Invalidate) returns ErrStale
// until Refresh.
func (l *Lease) Detail(ctx context.Context, req Request) (Detail, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if l.track.isClosed() {
		return Detail{}, ErrClosed
	}
	l.track.mu.Lock()
	provider, loader := l.track.provider, l.track.loader
	l.track.mu.Unlock()
	if provider == nil || loader == nil {
		return Detail{}, ErrUnsupported
	}

	plan, err := provider.DetailPlan(req)
	if err != nil {
		return Detail{}, err
	}
	if plan.Load == nil {
		return Detail{}, ErrUnsupported
	}

	// waitForCategories returns the EXACT snapshot whose Status it confirmed
	// terminal for plan.Requires: its Generation and Cache are captured from
	// that SAME read, not a later, separate Snapshot() call. A concurrent
	// Refresh between "categories confirmed terminal" and "generation/cache
	// captured" would otherwise let a Load run against a newer, incomplete
	// generation's cache while still being fenced/cached under that newer
	// generation's number -- silently caching a wrong-generation result
	// instead of failing safe. Passing snapshot.Cache explicitly into the
	// Load closure (rather than letting it call loader.Cache() lazily at
	// run time) closes that window: whatever cache the categories were
	// checked against is exactly the cache Load receives.
	snapshot, err := l.waitForCategories(ctx, loader, plan.Requires)
	if err != nil {
		return Detail{}, err
	}
	if err := requireCategoriesReady(plan.Requires, snapshot); err != nil {
		return Detail{}, err
	}
	coverage := plan.Coverage
	if plan.CoverageForCache != nil {
		coverage, err = plan.CoverageForCache(snapshot.Cache)
		if err != nil {
			return Detail{}, err
		}
	}

	acquired, err := l.track.details.acquire(ctx, snapshot.Generation, coverage, plan.Volatile, func(jobCtx context.Context) (Detail, error) {
		return plan.Load(jobCtx, snapshot.Cache)
	})
	if err != nil {
		return Detail{}, err
	}
	if plan.Render == nil {
		return acquired, nil
	}
	return plan.Render(snapshot.Cache, acquired, req)
}

// waitForCategories blocks until every one of kinds reaches a terminal
// status in loader's current generation, or ctx/the store ends first, then
// returns that exact snapshot (see the comment in Detail for why the caller
// must use THIS snapshot's Generation/Cache rather than reading a fresh one
// afterward). It reuses the same coalesced-event subscription Lease.Subscribe
// exposes, so it never polls.
func (l *Lease) waitForCategories(ctx context.Context, loader *Loader, kinds []catalog.MetadataKind) (*catalog.MetadataSnapshot, error) {
	if len(kinds) == 0 {
		return loader.Snapshot(), nil
	}
	sub := l.Subscribe()
	defer sub.Close()
	for {
		snapshot := loader.Snapshot()
		if snapshot.Stale {
			return nil, ErrStale
		}
		allTerminal := true
		for _, kind := range kinds {
			status, ok := snapshot.Status[kind]
			if !ok || !terminalMetadataState(status.State) {
				allTerminal = false
				break
			}
		}
		if allTerminal {
			return snapshot, nil
		}
		select {
		case _, ok := <-sub.C:
			if !ok {
				return nil, ErrClosed
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// requireCategoriesReady rejects a snapshot whose required categories
// reached a terminal state other than Ready (Failed, Blocked or Cancelled):
// proceeding to Load against such a category's cache would silently compute
// a Detail against incomplete/wrong data instead of failing.
func requireCategoriesReady(kinds []catalog.MetadataKind, snapshot *catalog.MetadataSnapshot) error {
	for _, kind := range kinds {
		status := snapshot.Status[kind]
		if status.State == catalog.MetadataReady {
			continue
		}
		if status.Err != nil {
			return fmt.Errorf("%w: %s: %v", ErrCategoryFailed, kind, status.Err)
		}
		return fmt.Errorf("%w: %s is %s", ErrCategoryFailed, kind, status.State)
	}
	return nil
}

// Invalidate marks this store's current data stale: detail reads return
// ErrStale, and any detail load still in flight is canceled and, once it
// does return, discarded rather than cached or handed to a waiter. It starts
// no new I/O; only an explicit Refresh does that. The published catalog
// snapshot itself keeps its last known Cache/Status (just flagged Stale) so
// readers of Snapshot still see the last good data, but the detail cache is
// emptied immediately.
func (l *Lease) Invalidate() error {
	if l.track.isClosed() {
		return ErrClosed
	}
	l.track.mu.Lock()
	loader := l.track.loader
	l.track.invalidations++
	l.track.mu.Unlock()
	if loader == nil {
		return ErrUnsupported
	}
	loader.MarkStale()
	l.track.details.markStale()
	return nil
}

// Refresh re-runs the store's catalog plan against the same provider,
// publishing a new metadata generation with Stale false, and advances the
// detail store to that same generation: any detail load still in flight for
// the superseded generation is canceled and its eventual result discarded.
// It does not create a new provider or a new Incarnation. Reset unconditionally
// replaces the published snapshot with an empty one for the new generation
// before Start ever runs, so a failed Start leaves no prior data to fall
// back to; on failure Refresh marks that empty generation StartFailed (the
// same signal a failed initial creation publishes) so callers reading
// Snapshot() can distinguish "refresh failed" from "never attempted".
func (l *Lease) Refresh(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	l.track.refreshMu.Lock()
	defer l.track.refreshMu.Unlock()
	if l.track.isClosed() {
		return ErrClosed
	}
	l.track.mu.Lock()
	loader, provider := l.track.loader, l.track.provider
	l.track.mu.Unlock()
	if loader == nil || provider == nil {
		return ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	generation := loader.Snapshot().Generation + 1
	l.track.details.beginGeneration(generation)
	loader.Reset(generation)
	if _, err := loader.Start(l.track.ctx, generation, provider.Plan()); err != nil {
		_ = loader.MarkStartFailed(generation)
		return err
	}
	return nil
}

// RefreshInBackground loads a new generation on a separate loader while the
// current snapshot stays published, then publishes it in one step. A
// generation that loses a category the current snapshot had ready is
// discarded and its error returned, leaving the current snapshot untouched.
// ctx bounds only this caller's wait; the load runs under the store's
// context. It never overlaps Refresh.
func (l *Lease) RefreshInBackground(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	t := l.track
	t.refreshMu.Lock()
	defer t.refreshMu.Unlock()
	if t.isClosed() {
		return ErrClosed
	}
	t.mu.Lock()
	current, provider := t.loader, t.provider
	t.mu.Unlock()
	if current == nil || provider == nil {
		return ErrUnsupported
	}
	// No job of the current generation may still run once it is replaced.
	if err := current.Wait(ctx); err != nil {
		return err
	}
	previous := current.Snapshot()
	generation := previous.Generation + 1
	staging := NewLoader(t.storeBudget)
	staging.Reset(generation)
	t.mu.Lock()
	if t.closed || t.ctx.Err() != nil {
		t.mu.Unlock()
		return ErrClosed
	}
	t.staging = staging
	invalidations := t.invalidations
	t.mu.Unlock()
	discard := func() {
		staging.Stop()
		_ = staging.Wait(context.Background())
		t.mu.Lock()
		if t.staging == staging {
			t.staging = nil
		}
		t.mu.Unlock()
	}
	load, err := staging.Start(t.ctx, generation, provider.Plan())
	if err != nil {
		discard()
		return err
	}
	select {
	case <-load.Done:
	case <-ctx.Done():
		discard()
		return ctx.Err()
	}
	if t.ctx.Err() != nil {
		discard()
		return ErrClosed
	}
	if err := backgroundRefreshFailure(previous, staging.Snapshot()); err != nil {
		discard()
		return err
	}
	t.mu.Lock()
	if t.closed || t.ctx.Err() != nil {
		t.mu.Unlock()
		discard()
		return ErrClosed
	}
	staging.SetChangedCallback(func() { l.registry.publishTrackEvent(t) })
	t.loader, t.staging = staging, nil
	invalidated := t.invalidations != invalidations
	t.mu.Unlock()
	t.details.beginGeneration(generation)
	current.Stop()
	if invalidated {
		// An Invalidate during the load may postdate the staged queries.
		staging.MarkStale()
		t.details.markStale()
	}
	l.registry.publishTrackEvent(t)
	return nil
}

// backgroundRefreshFailure rejects a new generation that start-failed or
// lost a category the previous one had ready. Categories that were already
// failed may stay failed: a partial store can still refresh.
func backgroundRefreshFailure(previous, next *catalog.MetadataSnapshot) error {
	if next.StartFailed {
		return fmt.Errorf("%w: metadata load did not start", ErrCategoryFailed)
	}
	for kind, status := range previous.Status {
		if status.State != catalog.MetadataReady {
			continue
		}
		got := next.Status[kind]
		if got.State == catalog.MetadataReady {
			continue
		}
		if got.Err != nil {
			return fmt.Errorf("%w: %s: %v", ErrCategoryFailed, kind, got.Err)
		}
		return fmt.Errorf("%w: %s is %s", ErrCategoryFailed, kind, got.State)
	}
	return nil
}

// Subscribe returns a Subscription delivering coalesced change events for
// this lease's store. The caller must Close it when done.
func (l *Lease) Subscribe() *Subscription {
	sub, ch := l.track.hub.subscribe()
	subscription := &Subscription{C: ch}
	subscription.close = func() { l.track.hub.unsubscribe(sub) }
	return subscription
}

// Release drops this lease's reference to its store. The last release for a
// (Key, Scope) cancels and closes the underlying provider.
func (l *Lease) Release() {
	l.releaseOnce.Do(func() {
		l.registry.releaseTrackRef(l.track)
	})
}
