package metadata_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

// TestDetailLoadedOnceAndSharedAcrossLeases is the decisive test: two
// separate leases onto the same store must share exactly one Load for the
// same object/facet, and must observe the same rendered value/descriptors.
func TestDetailLoadedOnceAndSharedAcrossLeases(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3, MaxDetailBytes: 64 << 20})
	defer r.Close(ctx)
	p := metadatatest.New()
	p.DetailValue = metadata.Detail{
		Text:       "CREATE TABLE T (ID INTEGER)",
		Provenance: "ddl",
		Columns:    []*catalog.ColumnDesc{{ColumnBase: catalog.ColumnBase{Table: "T", Name: "ID"}}},
	}
	key := metadata.Key{Engine: "interbase", URL: "ib://host/detail-shared"}
	scope := metadata.Scope{Principal: "U"}

	a, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release()
	waitReadyOrFatal(t, a)
	waitReadyOrFatal(t, b)

	request := metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Facet: "source", Consistency: metadata.Committed}

	first, err := a.Detail(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Detail(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Text != second.Text || p.DetailCalls.Load() != 1 {
		t.Fatal("detail fetched twice")
	}
	if len(first.Columns) > 0 && first.Columns[0] != second.Columns[0] {
		t.Fatal("detail copied")
	}
}

// TestDetailManyConcurrentRequestsShareOneLoad stress-tests the same
// invariant under heavy concurrency, the class of bug a single-pair test can
// miss: a TOCTOU window in the reservation check would show up here as
// DetailCalls > 1 under -race -count=N.
func TestDetailManyConcurrentRequestsShareOneLoad(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)
	p := metadatatest.New()
	p.DetailValue = metadata.Detail{Text: "X"}
	key := metadata.Key{Engine: "interbase", URL: "ib://host/detail-stress"}
	scope := metadata.Scope{Principal: "U"}
	lease, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)

	request := metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Facet: "source", Consistency: metadata.Committed}
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := lease.Detail(ctx, request)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := p.DetailCalls.Load(); got != 1 {
		t.Fatalf("DetailCalls = %d, want 1 under %d concurrent requesters", got, n)
	}
}

// TestDetailCanceledWaiterDoesNotCancelSharedLoad proves canceling one
// caller's own context only withdraws that caller's interest: the shared
// load (and any other waiter on it) is unaffected. No sleeps: B either joins
// the still in-flight load or finds it already cached; either way exactly
// one Load must have run.
func TestDetailCanceledWaiterDoesNotCancelSharedLoad(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)
	p := metadatatest.New()
	gate := make(chan struct{})
	p.DetailGate = gate
	p.DetailValue = metadata.Detail{Text: "X"}
	key := metadata.Key{Engine: "interbase", URL: "ib://host/detail-cancel"}
	scope := metadata.Scope{Principal: "U"}
	lease, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)

	request := metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Facet: "source", Consistency: metadata.Committed}

	canceledCtx, cancel := context.WithCancel(context.Background())
	aDone := make(chan error, 1)
	go func() {
		_, err := lease.Detail(canceledCtx, request)
		aDone <- err
	}()
	select {
	case <-p.DetailEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("detail load never entered")
	}
	cancel()
	if err := <-aDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v, want context.Canceled", err)
	}

	bDone := make(chan struct{})
	var bResult metadata.Detail
	var bErr error
	go func() {
		bResult, bErr = lease.Detail(context.Background(), request)
		close(bDone)
	}()
	close(gate)
	select {
	case <-bDone:
	case <-time.After(5 * time.Second):
		t.Fatal("surviving waiter never completed")
	}
	if bErr != nil {
		t.Fatal(bErr)
	}
	if bResult.Text != "X" {
		t.Fatalf("bResult = %+v", bResult)
	}
	if p.DetailCalls.Load() != 1 {
		t.Fatalf("DetailCalls = %d, want 1", p.DetailCalls.Load())
	}
}

// TestDetailCacheSurvivesStatusOnlyRevisions proves that repeated Detail
// reads for an already-settled coverage are pure cache hits: they trigger
// neither a new Load nor a new bulk snapshot revision.
func TestDetailCacheSurvivesStatusOnlyRevisions(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)
	p := metadatatest.New()
	p.DetailValue = metadata.Detail{Text: "X"}
	key := metadata.Key{Engine: "interbase", URL: "ib://host/detail-status-only"}
	scope := metadata.Scope{Principal: "U"}
	lease, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)

	request := metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Facet: "source", Consistency: metadata.Committed}
	if _, err := lease.Detail(ctx, request); err != nil {
		t.Fatal(err)
	}
	if p.DetailCalls.Load() != 1 {
		t.Fatal("expected exactly one initial detail load")
	}
	revisionAfterFirstLoad := lease.Snapshot().Revision

	for i := 0; i < 5; i++ {
		if _, err := lease.Detail(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	if got := p.DetailCalls.Load(); got != 1 {
		t.Fatalf("repeated Detail calls triggered %d loads, want 1", got)
	}
	if lease.Snapshot().Revision != revisionAfterFirstLoad {
		t.Fatal("cached detail reads must not publish new bulk snapshot revisions")
	}
}

func TestVolatileDetailCoalescesInFlightAndReloadsAfterSettlement(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)

	gate := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int64
	p := &renderingProvider{
		cache:    &catalog.Cache{SchemaTables: map[string][]string{"": {"T"}}},
		volatile: true,
		load: func(ctx context.Context) (metadata.Detail, error) {
			call := calls.Add(1)
			if call == 1 {
				close(entered)
				select {
				case <-gate:
				case <-ctx.Done():
					return metadata.Detail{}, ctx.Err()
				}
			}
			return metadata.Detail{Text: fmt.Sprintf("value-%d", call)}, nil
		},
	}
	lease, err := r.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "ib://host/detail-volatile"}, metadata.Scope{Principal: "U"}, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)

	request := metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindGenerator, Name: "G"}, Facet: "ddl", Consistency: metadata.Committed}
	type result struct {
		detail metadata.Detail
		err    error
	}
	firstDone := make(chan result, 1)
	go func() {
		detail, err := lease.Detail(ctx, request)
		firstDone <- result{detail: detail, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("volatile detail load did not enter")
	}

	secondStarted := make(chan struct{})
	secondDone := make(chan result, 1)
	go func() {
		close(secondStarted)
		detail, err := lease.Detail(ctx, request)
		secondDone <- result{detail: detail, err: err}
	}()
	<-secondStarted
	select {
	case got := <-secondDone:
		close(gate)
		t.Fatalf("concurrent volatile detail returned before shared load settled: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
	if got := calls.Load(); got != 1 {
		close(gate)
		t.Fatalf("in-flight load calls = %d, want 1", got)
	}
	close(gate)
	first, second := <-firstDone, <-secondDone
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent Detail errors = %v, %v", first.err, second.err)
	}
	if first.detail.Text != "value-1" || second.detail.Text != "value-1" {
		t.Fatalf("coalesced values = %q, %q; want value-1 for both", first.detail.Text, second.detail.Text)
	}

	third, err := lease.Detail(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if third.Text != "value-2" || calls.Load() != 2 {
		t.Fatalf("post-settlement detail = %q after %d loads, want value-2 after 2 loads", third.Text, calls.Load())
	}
}

func TestVolatileDetailGenerationChangeMakesOldWaitersStale(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)

	gate := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int64
	p := &renderingProvider{
		cache:    &catalog.Cache{SchemaTables: map[string][]string{"": {"T"}}},
		volatile: true,
		load: func(context.Context) (metadata.Detail, error) {
			call := calls.Add(1)
			if call == 1 {
				close(entered)
				<-gate
			}
			return metadata.Detail{Text: fmt.Sprintf("value-%d", call)}, nil
		},
	}
	lease, err := r.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "ib://host/detail-volatile-generation"}, metadata.Scope{Principal: "U"}, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)

	request := metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindGenerator, Name: "G"}, Facet: "ddl", Consistency: metadata.Committed}
	oldDone := make(chan error, 1)
	go func() {
		_, err := lease.Detail(ctx, request)
		oldDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old-generation volatile load did not enter")
	}
	if err := lease.Refresh(ctx); err != nil {
		close(gate)
		t.Fatalf("Refresh() error = %v", err)
	}
	if err := metadatatest.WaitReady(ctx, lease); err != nil {
		close(gate)
		t.Fatalf("WaitReady after Refresh() error = %v", err)
	}
	close(gate)
	select {
	case err := <-oldDone:
		if !errors.Is(err, metadata.ErrStale) {
			t.Fatalf("old-generation volatile detail error = %v, want ErrStale", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old-generation volatile detail did not settle after release")
	}

	current, err := lease.Detail(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if current.Text != "value-2" || calls.Load() != 2 {
		t.Fatalf("current-generation detail = %q after %d loads, want value-2 after 2 loads", current.Text, calls.Load())
	}
}

// TestDetailDialectOnlyDifferenceDoesNotReload proves Render (when present)
// runs per request against the one acquired Detail, so two requests that
// differ only in Dialect never trigger a second Load.
func TestDetailDialectOnlyDifferenceDoesNotReload(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)

	renderCalls := 0
	var renderMu sync.Mutex
	p := &renderingProvider{
		cache: &catalog.Cache{SchemaTables: map[string][]string{"": {"T"}}},
		load: func(context.Context) (metadata.Detail, error) {
			return metadata.Detail{Text: "RAW"}, nil
		},
		render: func(_ *catalog.Cache, acquired metadata.Detail, req metadata.Request) (metadata.Detail, error) {
			renderMu.Lock()
			renderCalls++
			renderMu.Unlock()
			acquired.Text = acquired.Text + "#" + strconv.Itoa(req.Dialect)
			return acquired, nil
		},
	}
	key := metadata.Key{Engine: "interbase", URL: "ib://host/detail-render"}
	scope := metadata.Scope{Principal: "U"}
	lease, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)

	req1 := metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Facet: "source", Dialect: 1, Consistency: metadata.Committed}
	req2 := req1
	req2.Dialect = 2

	d1, err := lease.Detail(ctx, req1)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := lease.Detail(ctx, req2)
	if err != nil {
		t.Fatal(err)
	}
	if d1.Text == d2.Text {
		t.Fatalf("expected dialect-specific rendering, got identical text %q", d1.Text)
	}
	if p.loadCalls != 1 {
		t.Fatalf("Load called %d times, want 1 (only Render should differ by Dialect)", p.loadCalls)
	}
	if renderCalls != 2 {
		t.Fatalf("Render called %d times, want 2", renderCalls)
	}
}

// TestDetailCacheMatchesCategoryCheckUnderConcurrentRefresh races Detail
// calls against concurrent Refreshes and proves the cache handed to Load
// always corresponds to the exact snapshot whose Status was confirmed
// terminal for the required categories -- never a newer, incomplete
// generation's cache paired with an older check. Render also checks the
// generation-specific relation name against its cache, catching accidental
// use of a newer loader snapshot after acquisition. Before the fix, Detail
// read the category-terminal snapshot once (for the check) and then read
// loader.Snapshot()/loader.Cache() again, separately, to capture the
// generation and the cache handed to Load; a Refresh landing in that gap
// could pair a "relations is Ready" check with a brand new, empty cache.
func TestDetailCacheMatchesCategoryCheckUnderConcurrentRefresh(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)

	var violations atomic.Int64
	p := &consistencyProvider{}
	key := metadata.Key{Engine: "interbase", URL: "ib://host/detail-consistency"}
	scope := metadata.Scope{Principal: "U"}
	lease, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)
	// Force one successful Render before racing refreshes so a zero-render
	// schedule cannot make the assertion vacuous. Subsequent calls exercise
	// the snapshot-bound renderer under concurrent generation replacement.
	if _, err := lease.Detail(ctx, metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "BASE"}, Facet: "source", Consistency: metadata.Committed}); err != nil {
		t.Fatalf("preflight Detail before refresh race: %v", err)
	}

	// Both loops are independently bounded (rather than one spinning
	// unbounded until the other finishes): an unthrottled tight Refresh
	// loop can otherwise pile up schedule() goroutines faster than they
	// drain (each Refresh cancels the previous generation but does not wait
	// for it), which is a self-inflicted resource-exhaustion problem in the
	// test's own stress pattern, not something the production fix controls
	// or needs to.
	const refreshIterations, detailIterations = 60, 60
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < refreshIterations; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = lease.Refresh(context.Background())
		}
	}()
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < detailIterations; i++ {
			req := metadata.Request{
				Object:      catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: fmt.Sprintf("T%d", i)},
				Facet:       "source",
				Consistency: metadata.Committed,
			}
			_, err := lease.Detail(context.Background(), req)
			if err != nil && !errors.Is(err, metadata.ErrStale) && !errors.Is(err, metadata.ErrCategoryFailed) {
				violations.Add(1)
			}
		}
	}()
	wg.Wait()

	if v := violations.Load(); v != 0 {
		t.Fatalf("%d Detail Load calls observed a cache inconsistent with the category check that gated them (or an unexpected error)", v)
	}
	if v := p.violations.Load(); v != 0 {
		t.Fatalf("%d Load calls ran against a cache where the checked-terminal category was not actually merged in", v)
	}
	if got := p.renders.Load(); got == 0 {
		t.Fatal("refresh race test never reached Render; generation-consistency path was not exercised")
	}
}

// consistencyProvider's relations job always succeeds immediately; its
// DetailPlan.Load asserts the cache it receives genuinely has relations
// merged in (Metadata[MetadataRelations] == MetadataReady and SchemaTables
// populated), while Render asserts it receives that same generation's cache.
// Violations in either stage are recorded.
type consistencyProvider struct {
	violations atomic.Int64
	plans      atomic.Int64
	renders    atomic.Int64
}

func (p *consistencyProvider) Plan() catalog.MetadataPlan {
	name := fmt.Sprintf("T-gen-%d", p.plans.Add(1))
	return catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{
		Kind: catalog.MetadataRelations,
		Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{Cache: &catalog.Cache{SchemaTables: map[string][]string{"": {name}}}, Count: 1}, nil
		},
	}}}
}

func (p *consistencyProvider) DetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	return metadata.DetailPlan{
		Requires: []catalog.MetadataKind{catalog.MetadataRelations},
		Coverage: req.Object.Key() + "\x00" + req.Facet,
		Load: func(_ context.Context, cache *catalog.Cache) (metadata.Detail, error) {
			if cache == nil || cache.Metadata[catalog.MetadataRelations] != catalog.MetadataReady || len(cache.SchemaTables[""]) == 0 {
				p.violations.Add(1)
			}
			return metadata.Detail{Text: cache.SchemaTables[""][0]}, nil
		},
		Render: func(cache *catalog.Cache, acquired metadata.Detail, _ metadata.Request) (metadata.Detail, error) {
			p.renders.Add(1)
			if cache == nil || len(cache.SchemaTables[""]) != 1 || cache.SchemaTables[""][0] != acquired.Text {
				p.violations.Add(1)
			}
			return acquired, nil
		},
	}, nil
}

func (p *consistencyProvider) Close() error { return nil }

func (p *consistencyProvider) Factory(context.Context) (metadata.Provider, error) { return p, nil }

// renderingProvider is a minimal metadata.Provider with a configurable Load
// and Render, for tests needing a real (non-nil) Render function.
type renderingProvider struct {
	cache     *catalog.Cache
	volatile  bool
	gate      <-chan struct{}
	entered   chan struct{}
	enterOnce sync.Once
	load      func(context.Context) (metadata.Detail, error)
	render    func(*catalog.Cache, metadata.Detail, metadata.Request) (metadata.Detail, error)
	coverage  func(*catalog.Cache) (string, error)
	loadCalls int
	mu        sync.Mutex
}

func (p *renderingProvider) Plan() catalog.MetadataPlan {
	p.mu.Lock()
	cache := p.cache
	p.mu.Unlock()
	return catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{
		Kind: catalog.MetadataRelations,
		Run: func(ctx context.Context, _ *catalog.Cache) (catalog.MetadataPatch, error) {
			if p.entered != nil {
				p.enterOnce.Do(func() { close(p.entered) })
			}
			if p.gate != nil {
				select {
				case <-p.gate:
				case <-ctx.Done():
					return catalog.MetadataPatch{}, ctx.Err()
				}
			}
			return catalog.MetadataPatch{Cache: cache, Count: len(cache.SchemaTables[""])}, nil
		},
	}}}
}

func (p *renderingProvider) DetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	return metadata.DetailPlan{
		Requires:         []catalog.MetadataKind{catalog.MetadataRelations},
		Coverage:         req.Object.Key() + "\x00" + req.Facet,
		Volatile:         p.volatile,
		CoverageForCache: p.coverage,
		Load: func(ctx context.Context, cache *catalog.Cache) (metadata.Detail, error) {
			p.mu.Lock()
			p.loadCalls++
			p.mu.Unlock()
			return p.load(ctx)
		},
		Render: p.render,
	}, nil
}

func (p *renderingProvider) Close() error { return nil }

func (p *renderingProvider) Factory(context.Context) (metadata.Provider, error) { return p, nil }

func TestDetailCoverageCallbackUsesReadySnapshotAndFencesRefresh(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)
	oldCache := &catalog.Cache{SchemaTables: map[string][]string{"": {"OLD"}}}
	newCache := &catalog.Cache{SchemaTables: map[string][]string{"": {"NEW"}}}
	var lease *metadata.Lease
	var callbackCalls, renderCalls int
	var callbackCache *catalog.Cache
	var p *renderingProvider
	p = &renderingProvider{
		cache: oldCache,
		load: func(context.Context) (metadata.Detail, error) {
			return metadata.Detail{Text: "loaded"}, nil
		},
		coverage: func(cache *catalog.Cache) (string, error) {
			callbackCalls++
			callbackCache = cache
			if cache.Metadata[catalog.MetadataRelations] != catalog.MetadataReady || cache.SchemaTables[""][0] != "OLD" {
				return "", fmt.Errorf("coverage callback did not receive ready captured cache: %#v", cache)
			}
			p.mu.Lock()
			p.cache = newCache
			p.mu.Unlock()
			if err := lease.Refresh(ctx); err != nil {
				return "", err
			}
			return "canonical", nil
		},
		render: func(cache *catalog.Cache, acquired metadata.Detail, _ metadata.Request) (metadata.Detail, error) {
			renderCalls++
			if cache != callbackCache {
				return metadata.Detail{}, fmt.Errorf("Render cache = %p, want the same captured cache passed to coverage %p", cache, callbackCache)
			}
			return acquired, nil
		},
	}
	var err error
	lease, err = r.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "ib://host/detail-coverage-refresh"}, metadata.Scope{Principal: "U"}, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)

	_, err = lease.Detail(ctx, metadata.Request{
		Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "old"},
		Facet:  "ddl", Consistency: metadata.Committed,
	})
	if !errors.Is(err, metadata.ErrStale) {
		t.Fatalf("Detail after callback-triggered Refresh error = %v, want ErrStale", err)
	}
	if callbackCache == nil || callbackCache.SchemaTables[""][0] != "OLD" || callbackCalls != 1 {
		t.Fatalf("coverage callback cache/calls = %v/%d, want captured ready OLD cache /1", callbackCache, callbackCalls)
	}
	if p.loadCalls != 0 || renderCalls != 0 {
		t.Fatalf("stale-generation Load/Render calls = %d/%d, want 0/0", p.loadCalls, renderCalls)
	}
}

func TestDetailCoverageCallbackWaitsForReadinessAndHonorsCallerCancellation(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	var callbackCalls atomic.Int64
	p := &renderingProvider{
		cache:   &catalog.Cache{SchemaTables: map[string][]string{"": {"T"}}},
		gate:    gate,
		entered: entered,
		load: func(context.Context) (metadata.Detail, error) {
			return metadata.Detail{Text: "loaded"}, nil
		},
		coverage: func(cache *catalog.Cache) (string, error) {
			callbackCalls.Add(1)
			if cache.Metadata[catalog.MetadataRelations] != catalog.MetadataReady {
				return "", fmt.Errorf("coverage callback ran before Relations was ready")
			}
			return "canonical", nil
		},
	}
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)
	lease, err := r.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "ib://host/detail-coverage-cancel"}, metadata.Scope{Principal: "U"}, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("relations category did not enter its gate")
	}

	requestCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := lease.Detail(requestCtx, metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "t"}, Facet: "ddl", Consistency: metadata.Committed})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Detail canceled while waiting for Relations = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled Detail did not return")
	}
	if got := callbackCalls.Load(); got != 0 {
		t.Fatalf("coverage callback calls before category readiness = %d, want 0", got)
	}
	close(gate)
	waitReadyOrFatal(t, lease)
	if _, err := lease.Detail(ctx, metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "t"}, Facet: "ddl", Consistency: metadata.Committed}); err != nil {
		t.Fatal(err)
	}
	if got := callbackCalls.Load(); got != 1 {
		t.Fatalf("coverage callback calls after ready Detail = %d, want 1", got)
	}
}
