package metadata

import (
	"context"
	"sync"
)

// defaultMaxDetailBytes is the per-store retained detail/source byte budget
// used whenever a caller supplies a non-positive value.
const defaultMaxDetailBytes = 64 << 20

// detailEntry is one coverage's acquired (Load-only, never Render-ed) detail.
// It is inserted into detailStore.entries before its Load even starts, so
// that a second concurrent request for the same coverage finds it and joins
// rather than starting a duplicate load: ready, value and err are write-once,
// set exactly once by settle before ready closes.
type detailEntry struct {
	coverage string
	ready    chan struct{}
	value    Detail
	err      error
	size     int64
	settled  bool // true once value/size hold a real, budget-admitted result

	prev, next *detailEntry
}

// detailStore coordinates one (Key, Scope) store's detail acquisitions: it
// deduplicates concurrent Load calls by Coverage, fences results by
// generation so a superseded load can never publish into a newer one, and
// bounds total retained bytes with weighted (by size, not count) LRU
// eviction. It never runs a Provider's Render function: that stays a pure,
// per-request step performed by the caller (Lease.Detail) against whatever
// Detail this store hands back.
type detailStore struct {
	budget   *StoreBudget
	maxBytes int64
	parent   context.Context

	mu         sync.Mutex
	generation uint64
	stale      bool
	genCtx     context.Context
	genCancel  context.CancelFunc

	entries    map[string]*detailEntry
	head, tail *detailEntry // head = most recently used, tail = least
	totalBytes int64

	active  int
	drained chan struct{}
}

func newDetailStore(parent context.Context, budget *StoreBudget, maxBytes int64) *detailStore {
	if maxBytes <= 0 {
		maxBytes = defaultMaxDetailBytes
	}
	drained := make(chan struct{})
	close(drained)
	d := &detailStore{budget: budget, maxBytes: maxBytes, parent: parent, drained: drained}
	d.beginGenerationLocked(1)
	return d
}

// beginGeneration cancels any in-flight load from the previous generation,
// clears every cached entry (they belong to a superseded generation) and
// starts accepting new acquisitions under generation with Stale false.
func (d *detailStore) beginGeneration(generation uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.beginGenerationLocked(generation)
}

func (d *detailStore) beginGenerationLocked(generation uint64) {
	if d.genCancel != nil {
		d.genCancel()
	}
	d.generation = generation
	d.stale = false
	d.genCtx, d.genCancel = context.WithCancel(d.parent)
	d.clearLocked()
}

// markStale cancels in-flight work without starting anything new and empties
// the cache; acquire returns ErrStale for this generation until beginGeneration
// runs again.
func (d *detailStore) markStale() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.genCancel != nil {
		d.genCancel()
	}
	d.stale = true
	d.clearLocked()
}

func (d *detailStore) clearLocked() {
	d.entries = make(map[string]*detailEntry)
	d.head, d.tail = nil, nil
	d.totalBytes = 0
}

// acquire returns the cached or in-flight-then-settled Detail for coverage
// under generation, starting exactly one Load per coverage even under
// concurrent callers. ctx only bounds this specific caller's wait: canceling
// it never cancels a load another caller (or a later caller of the same
// coverage) still needs.
func (d *detailStore) acquire(ctx context.Context, generation uint64, coverage string, load func(context.Context) (Detail, error)) (Detail, error) {
	d.mu.Lock()
	if d.stale {
		d.mu.Unlock()
		return Detail{}, ErrStale
	}
	if generation != d.generation {
		d.mu.Unlock()
		return Detail{}, ErrStale
	}
	entry, existed := d.entries[coverage]
	if existed {
		d.touchLocked(entry)
	} else {
		entry = &detailEntry{coverage: coverage, ready: make(chan struct{})}
		d.entries[coverage] = entry
		d.pushFrontLocked(entry)
		genCtx := d.genCtx
		if d.active == 0 {
			d.drained = make(chan struct{})
		}
		d.active++
		go d.run(entry, generation, genCtx, load)
	}
	d.mu.Unlock()

	select {
	case <-entry.ready:
		if entry.err != nil {
			return Detail{}, entry.err
		}
		return entry.value, nil
	case <-ctx.Done():
		return Detail{}, ctx.Err()
	}
}

// run performs the actual Load outside any store lock, holding a budget
// permit for its entire duration (through real completion, even if genCtx was
// canceled by a Refresh/Invalidate that arrived first), then settles entry.
func (d *detailStore) run(entry *detailEntry, generation uint64, genCtx context.Context, load func(context.Context) (Detail, error)) {
	defer d.runnerFinished()
	release, err := d.budget.acquire(genCtx)
	var value Detail
	if err == nil {
		value, err = load(genCtx)
		release()
	}
	d.settle(entry, generation, value, err)
}

// settle publishes entry's outcome. A result belonging to a generation that
// is no longer current (a Refresh/Invalidate ran while this load was still
// in flight) is never cached and never returned as if it were fresh data:
// every waiter on this specific entry gets ErrStale instead, and must ask
// again to see the new generation's answer.
func (d *detailStore) settle(entry *detailEntry, generation uint64, value Detail, loadErr error) {
	d.mu.Lock()
	current, stillMapped := d.entries[entry.coverage]
	valid := stillMapped && current == entry && !d.stale && generation == d.generation
	switch {
	case !valid:
		if stillMapped && current == entry {
			d.removeLocked(entry)
			delete(d.entries, entry.coverage)
		}
		entry.err = ErrStale
	case loadErr != nil:
		d.removeLocked(entry)
		delete(d.entries, entry.coverage)
		entry.err = loadErr
	default:
		size := detailWeight(value)
		if size > d.maxBytes {
			d.removeLocked(entry)
			delete(d.entries, entry.coverage)
			entry.err = ErrDetailLimit
		} else {
			entry.value = value
			entry.size = size
			entry.settled = true
			d.totalBytes += size
			d.evictLocked()
		}
	}
	d.mu.Unlock()
	close(entry.ready)
}

// evictLocked drops least-recently-used, already-settled entries until
// totalBytes fits maxBytes. It never removes an entry whose load has not yet
// settled: such an entry contributes nothing to totalBytes, and removing it
// would let a second concurrent request for the same coverage start a
// duplicate load.
func (d *detailStore) evictLocked() {
	node := d.tail
	for d.totalBytes > d.maxBytes && node != nil {
		prev := node.prev
		if node.settled {
			d.removeLocked(node)
			delete(d.entries, node.coverage)
			d.totalBytes -= node.size
		}
		node = prev
	}
}

func (d *detailStore) pushFrontLocked(e *detailEntry) {
	e.prev, e.next = nil, d.head
	if d.head != nil {
		d.head.prev = e
	}
	d.head = e
	if d.tail == nil {
		d.tail = e
	}
}

func (d *detailStore) removeLocked(e *detailEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else if d.head == e {
		d.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else if d.tail == e {
		d.tail = e.prev
	}
	e.prev, e.next = nil, nil
}

func (d *detailStore) touchLocked(e *detailEntry) {
	if d.head == e {
		return
	}
	d.removeLocked(e)
	d.pushFrontLocked(e)
}

func (d *detailStore) runnerFinished() {
	d.mu.Lock()
	d.active--
	if d.active == 0 {
		close(d.drained)
	}
	d.mu.Unlock()
}

// wait blocks until every in-flight Load this store has started has actually
// returned, or ctx is done first.
func (d *detailStore) wait(ctx context.Context) error {
	d.mu.Lock()
	drained := d.drained
	d.mu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// detailWeight approximates the retained bytes one Detail contributes to its
// store's budget: its owned text payloads. Columns/Description column spans
// are not counted here since ColumnDesc pointers are typically shared with
// the bulk catalog.Cache rather than owned by the detail cache itself.
func detailWeight(d Detail) int64 {
	return int64(len(d.Text) + len(d.Provenance) + len(d.Description.Body))
}
