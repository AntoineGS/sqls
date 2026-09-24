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

// Detail loads and renders one object facet. This increment of the registry
// does not implement detail coordination; every request reports
// ErrUnsupported. Task A4 replaces this with real coverage-aware loading.
func (l *Lease) Detail(ctx context.Context, req Request) (Detail, error) {
	if l.track.isClosed() {
		return Detail{}, ErrClosed
	}
	l.track.mu.Lock()
	provider := l.track.provider
	l.track.mu.Unlock()
	if provider == nil {
		return Detail{}, ErrUnsupported
	}
	if _, err := provider.DetailPlan(req); err != nil {
		return Detail{}, err
	}
	return Detail{}, ErrUnsupported
}

// Invalidate marks this store's current data as stale. This increment of the
// registry does not implement detail/generation coordination beyond the
// underlying catalog loader; task A4 gives Invalidate its full semantics.
func (l *Lease) Invalidate() error {
	if l.track.isClosed() {
		return ErrClosed
	}
	return nil
}

// Refresh re-runs the store's catalog plan against the same provider,
// publishing a new metadata generation. It does not create a new provider or
// a new Incarnation.
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
