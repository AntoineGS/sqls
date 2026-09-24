package metadata_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
)

// budgetTracker records concurrent-active and peak-active counts, and
// signals reached once active first hits target, without any sleep.
type budgetTracker struct {
	mu      sync.Mutex
	active  int
	peak    int
	target  int
	reached chan struct{}
	once    sync.Once
}

func newBudgetTracker(target int) *budgetTracker {
	return &budgetTracker{target: target, reached: make(chan struct{})}
}

func (bt *budgetTracker) enter() {
	bt.mu.Lock()
	bt.active++
	if bt.active > bt.peak {
		bt.peak = bt.active
	}
	hit := bt.active >= bt.target
	bt.mu.Unlock()
	if hit {
		bt.once.Do(func() { close(bt.reached) })
	}
}

func (bt *budgetTracker) leave() {
	bt.mu.Lock()
	bt.active--
	bt.mu.Unlock()
}

func (bt *budgetTracker) snapshotPeak() int {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	return bt.peak
}

// budgetProvider is a minimal metadata.Provider whose DetailPlan.Load is
// supplied per object name, for tests that need fine control over individual
// detail loads that metadatatest.Provider's single canned value cannot give.
type budgetProvider struct {
	cache *catalog.Cache
	loads map[string]func(context.Context) (metadata.Detail, error)
}

func (p *budgetProvider) Plan() catalog.MetadataPlan {
	return catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{
		Kind: catalog.MetadataRelations,
		Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{Cache: p.cache, Count: len(p.cache.SchemaTables[""])}, nil
		},
	}}}
}

func (p *budgetProvider) DetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	load, ok := p.loads[req.Object.Name]
	if !ok {
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
	return metadata.DetailPlan{
		Requires: []catalog.MetadataKind{catalog.MetadataRelations},
		Coverage: req.Object.Name,
		Load:     func(ctx context.Context, _ *catalog.Cache) (metadata.Detail, error) { return load(ctx) },
	}, nil
}

func (p *budgetProvider) Close() error { return nil }

func (p *budgetProvider) Factory(context.Context) (metadata.Provider, error) { return p, nil }

func detailReq(name string) metadata.Request {
	return metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: name}, Facet: "source", Consistency: metadata.Committed}
}

// TestBudgetHostCapLimitsConcurrencyAcrossStores proves the host-wide cap
// bounds concurrent native detail loads even when they belong to different
// stores (Review Focus: cross-store native call budget).
func TestBudgetHostCapLimitsConcurrencyAcrossStores(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 2, MaxStoreJobs: 5})
	defer r.Close(ctx)

	tracker := newBudgetTracker(2)
	gate := make(chan struct{})
	makeLoad := func(name string) func(context.Context) (metadata.Detail, error) {
		return func(context.Context) (metadata.Detail, error) {
			tracker.enter()
			defer tracker.leave()
			<-gate
			return metadata.Detail{Text: name}, nil
		}
	}

	pa := &budgetProvider{cache: &catalog.Cache{SchemaTables: map[string][]string{"": {"A1", "A2"}}}, loads: map[string]func(context.Context) (metadata.Detail, error){
		"A1": makeLoad("A1"), "A2": makeLoad("A2"),
	}}
	pb := &budgetProvider{cache: &catalog.Cache{SchemaTables: map[string][]string{"": {"B1", "B2"}}}, loads: map[string]func(context.Context) (metadata.Detail, error){
		"B1": makeLoad("B1"), "B2": makeLoad("B2"),
	}}

	scope := metadata.Scope{Principal: "U"}
	la, err := r.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "ib://host/budget-host-a"}, scope, pa.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer la.Release()
	lb, err := r.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "ib://host/budget-host-b"}, scope, pb.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Release()
	waitReadyOrFatal(t, la)
	waitReadyOrFatal(t, lb)

	names := []struct {
		lease *metadata.Lease
		name  string
	}{{la, "A1"}, {la, "A2"}, {lb, "B1"}, {lb, "B2"}}
	done := make(chan error, len(names))
	for _, n := range names {
		n := n
		go func() {
			_, err := n.lease.Detail(context.Background(), detailReq(n.name))
			done <- err
		}()
	}

	select {
	case <-tracker.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrency never reached the host cap across stores")
	}

	// Give any (incorrect) over-admission a bounded window to show up before
	// releasing the gate.
	time.Sleep(100 * time.Millisecond)
	if peak := tracker.snapshotPeak(); peak > 2 {
		t.Fatalf("peak concurrent host jobs = %d, want <= 2", peak)
	}

	close(gate)
	for range names {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if peak := tracker.snapshotPeak(); peak != 2 {
		t.Fatalf("peak concurrent host jobs = %d, want exactly 2 (genuine contention)", peak)
	}
}

// TestBudgetStoreCapLimitsConcurrencyWithinOneStore proves the per-store cap
// bounds concurrency independent of the (larger) host cap.
func TestBudgetStoreCapLimitsConcurrencyWithinOneStore(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 10, MaxStoreJobs: 2})
	defer r.Close(ctx)

	tracker := newBudgetTracker(2)
	gate := make(chan struct{})
	makeLoad := func(name string) func(context.Context) (metadata.Detail, error) {
		return func(context.Context) (metadata.Detail, error) {
			tracker.enter()
			defer tracker.leave()
			<-gate
			return metadata.Detail{Text: name}, nil
		}
	}
	names := []string{"T1", "T2", "T3", "T4"}
	loads := map[string]func(context.Context) (metadata.Detail, error){}
	for _, n := range names {
		loads[n] = makeLoad(n)
	}
	p := &budgetProvider{cache: &catalog.Cache{SchemaTables: map[string][]string{"": names}}, loads: loads}

	scope := metadata.Scope{Principal: "U"}
	lease, err := r.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "ib://host/budget-store"}, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)

	done := make(chan error, len(names))
	for _, n := range names {
		n := n
		go func() {
			_, err := lease.Detail(context.Background(), detailReq(n))
			done <- err
		}()
	}

	select {
	case <-tracker.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrency never reached the per-store cap")
	}
	time.Sleep(100 * time.Millisecond)
	if peak := tracker.snapshotPeak(); peak > 2 {
		t.Fatalf("peak concurrent store jobs = %d, want <= 2", peak)
	}

	close(gate)
	for range names {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if peak := tracker.snapshotPeak(); peak != 2 {
		t.Fatalf("peak concurrent store jobs = %d, want exactly 2 (genuine contention)", peak)
	}
}

// TestBudgetPermitHeldUntilCanceledLoadActuallyReturns is Review Focus #4:
// "Canceled native jobs surviving refresh/reacquire must still count against
// resource limits." Canceling the FIRST caller's own context must not free
// its store permit early: a second, distinct-coverage request in the same
// (one-permit) store must stay blocked until the first load actually
// returns, even though its caller already gave up.
func TestBudgetPermitHeldUntilCanceledLoadActuallyReturns(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 5, MaxStoreJobs: 1})
	defer r.Close(ctx)

	gate := make(chan struct{})
	firstEntered := make(chan struct{})
	var firstOnce sync.Once
	secondEntered := make(chan struct{})

	p := &budgetProvider{
		cache: &catalog.Cache{SchemaTables: map[string][]string{"": {"FIRST", "SECOND"}}},
		loads: map[string]func(context.Context) (metadata.Detail, error){},
	}
	p.loads["FIRST"] = func(context.Context) (metadata.Detail, error) {
		firstOnce.Do(func() { close(firstEntered) })
		<-gate // deliberately ignores ctx: a real native call cannot be interrupted mid-flight
		return metadata.Detail{Text: "first"}, nil
	}
	p.loads["SECOND"] = func(context.Context) (metadata.Detail, error) {
		close(secondEntered)
		return metadata.Detail{Text: "second"}, nil
	}

	scope := metadata.Scope{Principal: "U"}
	lease, err := r.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "ib://host/budget-hold"}, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := lease.Detail(firstCtx, detailReq("FIRST"))
		firstDone <- err
	}()
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first detail load never entered")
	}
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller error = %v, want context.Canceled", err)
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := lease.Detail(context.Background(), detailReq("SECOND"))
		secondDone <- err
	}()

	select {
	case <-secondEntered:
		t.Fatal("second load started while the store's only permit is still held by the canceled-but-still-running first load")
	case <-time.After(100 * time.Millisecond):
		// Expected: still blocked, the permit has not been released yet.
	}

	close(gate)
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondEntered:
	default:
		t.Fatal("second load should have entered once the permit was released")
	}
}
