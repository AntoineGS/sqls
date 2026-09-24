package metadata

import (
	"context"
	"sync"
)

// defaultHostJobs and defaultStoreJobs are the registry-wide concurrency caps
// used whenever a caller supplies a non-positive value. A zero or negative
// budget must never silently mean unbounded.
const (
	defaultHostJobs  = 6
	defaultStoreJobs = 3
)

// Budget owns one host-wide native-call semaphore and hands out ref-counted
// per-store permit sets keyed by Key. Every native metadata call — a bulk
// category job or a detail load — acquires a permit from both the host
// semaphore and its store's semaphore before doing I/O, and releases both
// only once that I/O actually returns, so a canceled-but-still-running native
// call keeps counting against both caps until it genuinely drains.
type Budget struct {
	host     chan struct{}
	perStore int

	mu     sync.Mutex
	stores map[Key]*storeBudgetHandle
}

type storeBudgetHandle struct {
	budget *StoreBudget
	refs   int
}

// StoreBudget is the shared permit set for one Key: a per-store semaphore
// plus the owning Budget's host-wide semaphore. The same *StoreBudget is
// returned by every ForStore(key) call while any reference to it is
// outstanding, so multiple scopes under one Key, and a retiring incarnation
// together with its replacement, genuinely share one native-call budget.
type StoreBudget struct {
	host chan struct{}
	sem  chan struct{}
}

// NewBudget creates a host-wide budget. host bounds total concurrent native
// jobs across every store this budget serves; perStore bounds each
// individual store. Non-positive values fall back to the registry defaults.
func NewBudget(host, perStore int) *Budget {
	if host <= 0 {
		host = defaultHostJobs
	}
	if perStore <= 0 {
		perStore = defaultStoreJobs
	}
	return &Budget{host: make(chan struct{}, host), perStore: perStore, stores: make(map[Key]*storeBudgetHandle)}
}

// ForStore returns the shared permit set for key, creating it on first use
// and incrementing its reference count. Callers that hold a long-lived
// reference (a registry track) must eventually call release(key) exactly
// once so the permit set can be reclaimed once nothing needs it any more; a
// later ForStore for the same key then allocates a fresh one.
func (b *Budget) ForStore(key Key) *StoreBudget {
	b.mu.Lock()
	defer b.mu.Unlock()
	handle, ok := b.stores[key]
	if !ok {
		handle = &storeBudgetHandle{budget: &StoreBudget{host: b.host, sem: make(chan struct{}, b.perStore)}}
		b.stores[key] = handle
	}
	handle.refs++
	return handle.budget
}

// release drops one reference obtained from ForStore(key). Once the last
// reference drops, the bookkeeping entry is removed; it does not affect any
// *StoreBudget a caller still holds directly, only whether a later ForStore
// for the same key shares that instance or allocates a fresh one.
func (b *Budget) release(key Key) {
	b.mu.Lock()
	defer b.mu.Unlock()
	handle, ok := b.stores[key]
	if !ok {
		return
	}
	handle.refs--
	if handle.refs <= 0 {
		delete(b.stores, key)
	}
}

// acquire blocks until both a store-level and a host-level permit are held,
// or ctx is done first. The returned release function must be called
// exactly once, after the native call it guards has actually returned.
func (sb *StoreBudget) acquire(ctx context.Context) (func(), error) {
	select {
	case sb.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case sb.host <- struct{}{}:
		return func() { <-sb.host; <-sb.sem }, nil
	case <-ctx.Done():
		<-sb.sem
		return nil, ctx.Err()
	}
}
