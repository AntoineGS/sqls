package metadata

import (
	"context"
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

	if err := l.waitForCategories(ctx, loader, plan.Requires); err != nil {
		return Detail{}, err
	}

	snapshot := loader.Snapshot()
	if snapshot.Stale {
		return Detail{}, ErrStale
	}

	acquired, err := l.track.details.acquire(ctx, snapshot.Generation, plan.Coverage, func(jobCtx context.Context) (Detail, error) {
		return plan.Load(jobCtx, loader.Cache())
	})
	if err != nil {
		return Detail{}, err
	}
	if plan.Render == nil {
		return acquired, nil
	}
	return plan.Render(loader.Cache(), acquired, req)
}

// waitForCategories blocks until every one of kinds reaches a terminal
// status in loader's current generation, or ctx/the store ends first. It
// reuses the same coalesced-event subscription Lease.Subscribe exposes, so
// it never polls.
func (l *Lease) waitForCategories(ctx context.Context, loader *Loader, kinds []catalog.MetadataKind) error {
	if len(kinds) == 0 {
		return nil
	}
	sub := l.Subscribe()
	defer sub.Close()
	for {
		snapshot := loader.Snapshot()
		if snapshot.Stale {
			return ErrStale
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
			return nil
		}
		select {
		case _, ok := <-sub.C:
			if !ok {
				return ErrClosed
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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
// It does not create a new provider or a new Incarnation.
func (l *Lease) Refresh(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
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
	_, err := loader.Start(l.track.ctx, generation, provider.Plan())
	return err
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
