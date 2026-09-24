package metadata

import (
	"context"
	"sync"
)

// scopeKey is the comparable projection of Scope used to key per-scope
// tracks. It never leaves this package.
type scopeKey struct {
	principal  string
	role       string
	textPolicy string
}

func newScopeKey(s Scope) scopeKey {
	return scopeKey{principal: s.Principal, role: s.Role, textPolicy: s.TextPolicy}
}

// registryEntry is one Key's shared state: a monotonically allocated
// incarnation and the independently tracked stores for each distinct Scope
// currently in use under that Key. All of an entry's fields below are
// protected by its owning Registry's mu, never by a separate per-entry lock,
// so that "does this entry still exist and accept new tracks" and "add or
// remove a track" are always one atomic operation. Once every track drains,
// the entry is retired and removed from Registry.entries; a later Acquire
// for the same Key allocates a fresh entry and a new incarnation rather than
// resurrecting torn-down state.
type registryEntry struct {
	key         Key
	incarnation uint64
	retired     bool
	tracks      map[scopeKey]*track
}

// track is one (Key, Scope) store: its provider, its metadata loader and its
// reference count. A track is created by the first Acquire for its
// (Key, Scope) and torn down by the Acquire/Release that drops the last
// reference. refs is protected by the owning Registry's mu (the same lock
// that protects entry membership); provider/err/loader/closed are protected
// by t.mu, since they are set by the asynchronous factory/loader lifecycle
// rather than by registry bookkeeping.
type track struct {
	entry *registryEntry
	scope Scope

	ctx    context.Context
	cancel context.CancelFunc

	ready chan struct{} // closed once the factory call has returned (success or failure)

	refs int

	mu       sync.Mutex
	provider Provider
	err      error
	loader   *Loader
	closed   bool // set once the provider has been finalized (Closed)

	closeOnce sync.Once
	hub       *eventHub
}

func (t *track) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// Registry shares metadata stores across consumers keyed by (engine,
// normalized database URL). Acquire returns a Lease onto the shared store for
// a given authenticated Scope; the store is created on first use and torn
// down once its last lease releases.
//
// A single mutex protects the registry's entries map together with every
// entry's tracks map, retired flag and each track's ref count. This is
// deliberate: "look up (or create) the entry for a Key" and "look up (or
// create) the track for a Scope under it" must be one atomic operation with
// respect to a concurrent last-Release tearing that same entry down, or an
// Acquire can attach a new track to an entry that is simultaneously being
// removed, producing two live stores for one (Key, Scope).
type Registry struct {
	opts Options

	ctx    context.Context
	cancel context.CancelFunc

	mu             sync.Mutex
	entries        map[Key]*registryEntry
	incarnationSeq uint64
	closed         bool
}

// NewRegistry creates a registry. ctx bounds the lifetime of every factory
// call and background metadata load the registry ever starts; canceling it
// is equivalent to calling Close.
func NewRegistry(ctx context.Context, opts Options) *Registry {
	if ctx == nil {
		ctx = context.Background()
	}
	rootCtx, cancel := context.WithCancel(ctx)
	return &Registry{
		opts:    opts,
		ctx:     rootCtx,
		cancel:  cancel,
		entries: make(map[Key]*registryEntry),
	}
}

// Acquire returns a Lease onto the shared store for (key, scope), creating it
// via factory if no live store exists yet. Concurrent Acquire calls for the
// same (Key, Scope) share one factory call and one provider. Canceling ctx
// while waiting only withdraws this caller's interest; it never cancels
// creation still needed by another consumer.
func (r *Registry) Acquire(ctx context.Context, key Key, scope Scope, factory Factory) (*Lease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if scope == (Scope{}) {
		return nil, ErrScopeRequired
	}
	sk := newScopeKey(scope)

	entry, t, creator, err := r.attach(key, sk, scope)
	if err != nil {
		return nil, err
	}

	if creator {
		go r.createTrack(t, factory)
	}

	select {
	case <-t.ready:
		t.mu.Lock()
		trackErr := t.err
		t.mu.Unlock()
		if trackErr != nil {
			r.releaseTrackRef(t)
			return nil, trackErr
		}
		if t.ctx.Err() != nil {
			// The registry was closed (or this track otherwise force-
			// canceled) while the provider was being created, so its loader
			// never started. Never hand out a lease that will never settle.
			r.releaseTrackRef(t)
			return nil, ErrClosed
		}
		return &Lease{registry: r, track: t, key: key, incarnation: entry.incarnation}, nil
	case <-ctx.Done():
		r.releaseTrackRef(t)
		return nil, ctx.Err()
	}
}

// attach finds or creates the live entry for key and the live track for sk
// under it, incrementing the track's ref count, all as one atomic operation
// under r.mu. It never returns a retired entry or reuses a track that is no
// longer reachable from its entry.
func (r *Registry) attach(key Key, sk scopeKey, scope Scope) (entry *registryEntry, t *track, creator bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil, nil, false, ErrClosed
	}
	entry, ok := r.entries[key]
	if !ok || entry.retired {
		r.incarnationSeq++
		entry = &registryEntry{key: key, incarnation: r.incarnationSeq, tracks: make(map[scopeKey]*track)}
		r.entries[key] = entry
	}
	t, exists := entry.tracks[sk]
	if !exists {
		tctx, cancel := context.WithCancel(r.ctx)
		t = &track{entry: entry, scope: scope, ctx: tctx, cancel: cancel, ready: make(chan struct{}), hub: newEventHub()}
		entry.tracks[sk] = t
		creator = true
	}
	t.refs++
	return entry, t, creator, nil
}

// createTrack runs factory outside any registry mutex, then starts the
// track's metadata loader on success. It always closes t.ready exactly once.
func (r *Registry) createTrack(t *track, factory Factory) {
	provider, err := factory(t.ctx)
	if err != nil {
		t.mu.Lock()
		t.err = err
		t.mu.Unlock()
		close(t.ready)
		return
	}

	loader := NewLoader()
	loader.SetChangedCallback(func() { r.publishTrackEvent(t) })

	t.mu.Lock()
	t.provider = provider
	t.loader = loader
	t.mu.Unlock()

	// The decision to start (or abandon) the loader must be made and, when
	// starting, fully committed (Started published synchronously by Start)
	// BEFORE t.ready closes. Closing ready first would let a concurrent
	// Acquire observe "ready, not yet canceled" and hand back a lease, only
	// for this goroutine to observe cancellation a moment later and abandon
	// the loader entirely — stranding that lease with a snapshot that never
	// starts and therefore never settles.
	if t.ctx.Err() != nil {
		// Every interested caller already withdrew (or the registry closed)
		// while we were creating the provider; the ref-count-zero path (or
		// Close) will drain and close it. Acquire's own post-ready check of
		// t.ctx.Err() (monotonic once canceled) will observe this and never
		// hand out a lease for it.
		close(t.ready)
		return
	}
	loader.Reset(1)
	if _, err := loader.Start(t.ctx, 1, provider.Plan()); err != nil {
		_ = loader.MarkStartFailed(1)
	}
	close(t.ready)
}

// releaseTrackRef drops one reference to t. The last reference to drop
// removes t from its entry (and retires + removes the entry from the
// registry once it holds no more live tracks) and finalizes t's
// provider/loader asynchronously. All entry/track bookkeeping happens under
// the same registry mutex Acquire uses, so a dropping-to-zero release and a
// concurrent Acquire for the same (Key, Scope) can never both believe they
// own the live track.
func (r *Registry) releaseTrackRef(t *track) {
	entry := t.entry
	sk := newScopeKey(t.scope)

	r.mu.Lock()
	t.refs--
	remaining := t.refs
	if remaining <= 0 {
		if current, ok := entry.tracks[sk]; ok && current == t {
			delete(entry.tracks, sk)
		}
		if len(entry.tracks) == 0 && !entry.retired {
			entry.retired = true
			if current, ok := r.entries[entry.key]; ok && current == entry {
				delete(r.entries, entry.key)
			}
		}
	}
	r.mu.Unlock()

	if remaining <= 0 {
		r.finalizeTrack(t)
	}
}

// finalizeTrack cancels t's registry-owned context and, once its factory call
// and metadata loader have both settled, closes its provider exactly once.
// It never blocks its caller.
func (r *Registry) finalizeTrack(t *track) {
	t.cancel()
	go func() {
		<-t.ready
		t.mu.Lock()
		provider, loader := t.provider, t.loader
		t.mu.Unlock()
		if loader != nil {
			loader.Stop()
			loader.Wait(context.Background())
		}
		if provider != nil {
			t.closeOnce.Do(func() { _ = provider.Close() })
		}
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()
		t.hub.closeAll()
	}()
}

func (r *Registry) publishTrackEvent(t *track) {
	t.mu.Lock()
	loader := t.loader
	t.mu.Unlock()
	if loader == nil {
		return
	}
	snapshot := loader.Snapshot()
	t.hub.publish(Event{Key: t.entry.key, Incarnation: t.entry.incarnation, Generation: snapshot.Generation, Revision: snapshot.Revision})
}

// Close cancels every store this registry owns and waits for their
// factories, metadata loads and providers to settle before returning.
func (r *Registry) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	var tracks []*track
	for _, entry := range r.entries {
		entry.retired = true
		for _, t := range entry.tracks {
			tracks = append(tracks, t)
		}
		entry.tracks = make(map[scopeKey]*track)
	}
	r.entries = make(map[Key]*registryEntry)
	r.mu.Unlock()

	r.cancel()

	for _, t := range tracks {
		select {
		case <-t.ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		t.mu.Lock()
		provider, loader := t.provider, t.loader
		t.mu.Unlock()
		if loader != nil {
			loader.Stop()
			if err := loader.Wait(ctx); err != nil {
				return err
			}
		}
		if provider != nil {
			t.closeOnce.Do(func() { _ = provider.Close() })
		}
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()
		t.hub.closeAll()
	}
	return ctx.Err()
}
