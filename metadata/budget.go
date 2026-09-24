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

// waker implements a broadcast-wake pattern (the same shape as sync.Cond,
// but select-compatible): broadcast unblocks every goroutine currently
// parked on the channel returned by wait, without either side needing to
// know how many waiters exist or where they live. It never queues missed
// wakeups — a wait call after a broadcast gets a fresh, unclosed channel —
// so callers must always re-check their condition after waking, exactly
// like sync.Cond. This is what lets a release on ANY loader or detail load
// sharing a Budget/StoreBudget wake up EVERY other loader/load waiting on
// that same shared permit set, not just the one that happened to release it.
type waker struct {
	mu sync.Mutex
	ch chan struct{}
}

func newWaker() *waker {
	return &waker{ch: make(chan struct{})}
}

func (w *waker) wait() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ch
}

func (w *waker) broadcast() {
	w.mu.Lock()
	previous := w.ch
	w.ch = make(chan struct{})
	w.mu.Unlock()
	close(previous)
}

// Budget owns one host-wide native-call semaphore and hands out ref-counted
// per-store permit sets keyed by Key. Every native metadata call — a bulk
// category job or a detail load — acquires a permit from both the host
// semaphore and its store's semaphore before doing I/O, and releases both
// only once that I/O actually returns, so a canceled-but-still-running native
// call keeps counting against both caps until it genuinely drains. Every
// release, from any caller sharing this Budget, broadcasts hostWake so that
// no waiter for a host-level permit is ever left with nothing to wake it.
type Budget struct {
	host     chan struct{}
	hostWake *waker
	perStore int

	mu     sync.Mutex
	stores map[Key]*storeBudgetHandle
}

type storeBudgetHandle struct {
	budget *StoreBudget
	refs   int
}

// StoreBudget is the shared permit set for one Key: a per-store semaphore
// plus the owning Budget's host-wide semaphore, and a waker for each level.
// The same *StoreBudget is returned by every ForStore(key) call while any
// reference to it is outstanding, so multiple scopes under one Key, and a
// retiring incarnation together with its replacement, genuinely share one
// native-call budget AND its wakeups.
type StoreBudget struct {
	host      chan struct{}
	hostWake  *waker
	sem       chan struct{}
	storeWake *waker
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
	return &Budget{host: make(chan struct{}, host), hostWake: newWaker(), perStore: perStore, stores: make(map[Key]*storeBudgetHandle)}
}

// ForStore returns the shared permit set for key, creating it on first use
// and incrementing its reference count. Callers that hold a long-lived
// reference (a registry track) must eventually call release(key) exactly
// once so the permit set can be reclaimed once nothing needs it any more; a
// later ForStore for the same key then allocates a fresh one (with a fresh
// storeWake — sharing is scoped to instances that are actually live
// together, matching the sem/refcounting lifetime exactly).
func (b *Budget) ForStore(key Key) *StoreBudget {
	b.mu.Lock()
	defer b.mu.Unlock()
	handle, ok := b.stores[key]
	if !ok {
		handle = &storeBudgetHandle{budget: &StoreBudget{
			host: b.host, hostWake: b.hostWake,
			sem: make(chan struct{}, b.perStore), storeWake: newWaker(),
		}}
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
// exactly once, after the native call it guards has actually returned; it
// broadcasts both wakers so any loader elsewhere polling the same shared
// semaphores (see Loader.tryAcquireSlot) is woken rather than left parked on
// a private signal nothing here would ever fire. Note that acquire's own
// waits below need no waker: a blocking channel send on a buffered channel
// is itself woken by ANY receive from that channel, by any goroutine, so
// there is no lost-wakeup risk on this side — only the non-blocking
// poll-and-park pattern in Loader.schedule needs an explicit broadcast.
func (sb *StoreBudget) acquire(ctx context.Context) (func(), error) {
	select {
	case sb.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case sb.host <- struct{}{}:
		return func() {
			<-sb.host
			sb.hostWake.broadcast()
			<-sb.sem
			sb.storeWake.broadcast()
		}, nil
	case <-ctx.Done():
		<-sb.sem
		sb.storeWake.broadcast()
		return nil, ctx.Err()
	}
}
