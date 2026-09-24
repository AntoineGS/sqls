package metadata_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
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

// renderingProvider is a minimal metadata.Provider with a configurable Load
// and Render, for tests needing a real (non-nil) Render function.
type renderingProvider struct {
	cache     *catalog.Cache
	load      func(context.Context) (metadata.Detail, error)
	render    func(*catalog.Cache, metadata.Detail, metadata.Request) (metadata.Detail, error)
	loadCalls int
	mu        sync.Mutex
}

func (p *renderingProvider) Plan() catalog.MetadataPlan {
	return catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{
		Kind: catalog.MetadataRelations,
		Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{Cache: p.cache, Count: len(p.cache.SchemaTables[""])}, nil
		},
	}}}
}

func (p *renderingProvider) DetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	return metadata.DetailPlan{
		Requires: []catalog.MetadataKind{catalog.MetadataRelations},
		Coverage: req.Object.Key() + "\x00" + req.Facet,
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
