package metadata_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

// TestRefreshDiscardsInFlightOldGenerationDetail (Review Focus: "object load
// overlapping a bulk load must share coverage in both arrival orders" and
// the plan's stale-callback-fencing requirement): a Refresh that arrives
// while an old-generation detail load is still in flight must cancel that
// load and never let its eventual result reach a waiter or the cache; a
// fresh request after Refresh must reflect the new generation.
func TestRefreshDiscardsInFlightOldGenerationDetail(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)
	p := metadatatest.New()
	gate := make(chan struct{})
	p.DetailGate = gate
	// The same provider instance serves both generations (Refresh does not
	// create a new provider), so DetailValue must not be mutated once a load
	// may be reading it concurrently; keep it constant and rely on
	// DetailCalls/ErrStale, not differing content, to prove non-leakage.
	p.DetailValue = metadata.Detail{Text: "V"}
	key := metadata.Key{Engine: "interbase", URL: "ib://host/detail-refresh"}
	scope := metadata.Scope{Principal: "U"}
	lease, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)

	request := metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Facet: "source", Consistency: metadata.Committed}

	oldDone := make(chan error, 1)
	go func() {
		_, err := lease.Detail(context.Background(), request)
		oldDone <- err
	}()
	select {
	case <-p.DetailEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("old detail load never entered")
	}

	if err := lease.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	waitReadyOrFatal(t, lease)

	close(gate) // release the now-superseded old load; it must not win

	if err := <-oldDone; !errors.Is(err, metadata.ErrStale) {
		t.Fatalf("stale in-flight detail error = %v, want ErrStale", err)
	}

	fresh, err := lease.Detail(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Text != "V" {
		t.Fatalf("fresh detail = %+v, want V", fresh)
	}
	if p.DetailCalls.Load() != 2 {
		t.Fatalf("DetailCalls = %d, want 2 (old + fresh reload; the old in-flight load's result must not have been published/cached into the new generation)", p.DetailCalls.Load())
	}
}

// TestInvalidateBlocksDetailUntilRefresh covers Invalidate/Refresh's
// documented Stale semantics end to end.
func TestInvalidateBlocksDetailUntilRefresh(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)
	p := metadatatest.New()
	p.DetailValue = metadata.Detail{Text: "X"}
	key := metadata.Key{Engine: "interbase", URL: "ib://host/detail-invalidate"}
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

	if err := lease.Invalidate(); err != nil {
		t.Fatal(err)
	}
	if !lease.Snapshot().Stale {
		t.Fatal("Invalidate did not mark the snapshot stale")
	}
	// The bulk cache/status themselves must survive Invalidate untouched
	// (only flagged stale): a reader of Snapshot still sees last-known data.
	if lease.Snapshot().Status[catalog.MetadataRelations].State != catalog.MetadataReady {
		t.Fatal("Invalidate must not clear last-known category status")
	}
	if _, err := lease.Detail(ctx, request); !errors.Is(err, metadata.ErrStale) {
		t.Fatalf("Detail after Invalidate = %v, want ErrStale", err)
	}
	if p.DetailCalls.Load() != 1 {
		t.Fatal("Invalidate must not start any new I/O")
	}

	if err := lease.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	waitReadyOrFatal(t, lease)
	if lease.Snapshot().Stale {
		t.Fatal("Refresh did not clear Stale")
	}
	if _, err := lease.Detail(ctx, request); err != nil {
		t.Fatal(err)
	}
	if p.DetailCalls.Load() != 2 {
		t.Fatalf("DetailCalls = %d, want 2 (initial load + reload after refresh)", p.DetailCalls.Load())
	}
}

// TestInvalidateStaleSurvivesInFlightJobSettling covers two parts of the
// same requirement: Invalidate must actually cancel in-flight category jobs
// (not just document that it does), and Stale must survive every later
// snapshot publish -- including the canceled job's own Run finishing and
// recordMetadataJobMetrics publishing its metrics -- until an explicit
// Refresh clears it.
func TestInvalidateStaleSurvivesInFlightJobSettling(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)
	p := metadatatest.New()
	gate := make(chan struct{})
	p.Gate = gate
	key := metadata.Key{Engine: "interbase", URL: "ib://host/invalidate-inflight"}
	scope := metadata.Scope{Principal: "U"}
	lease, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	select {
	case <-p.Entered:
	case <-time.After(5 * time.Second):
		t.Fatal("relations job never entered")
	}

	if err := lease.Invalidate(); err != nil {
		t.Fatal(err)
	}
	if !lease.Snapshot().Stale {
		t.Fatal("Invalidate did not mark the snapshot stale")
	}
	if status := lease.Snapshot().Status[catalog.MetadataRelations]; status.State != catalog.MetadataCancelled {
		t.Fatalf("relations state after Invalidate = %s, want cancelled (Invalidate must cancel in-flight category jobs)", status.State)
	}

	close(gate) // let the already-cancelled job's Run actually return

	// The (now-cancelled) job's Run still finishes and publishes metrics via
	// recordMetadataJobMetrics (Count becomes non-zero once that runs) --
	// this is the exact scenario that must not silently clear Stale. Wait
	// for that publish to have genuinely happened, then check Stale;
	// checking immediately after Invalidate would not exercise this path,
	// since recordMetadataJobMetrics races the test goroutine for the lock
	// and may already have run by then.
	waitFor(t, 2*time.Second, func() bool {
		return lease.Snapshot().Status[catalog.MetadataRelations].Count != 0
	})
	if !lease.Snapshot().Stale {
		t.Fatal("Stale flipped back to false when the in-flight category job's Run settled after Invalidate")
	}
}

// TestCategoryFailureRequiresExplicitRefreshToRetry: a failed bulk category
// must not silently auto-retry; only an explicit Refresh retries it.
func TestCategoryFailureRequiresExplicitRefreshToRetry(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)
	p := metadatatest.New()
	p.Failure = errors.New("boom")
	key := metadata.Key{Engine: "interbase", URL: "ib://host/category-retry"}
	scope := metadata.Scope{Principal: "U"}
	lease, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)

	status := lease.Snapshot().Status[catalog.MetadataRelations]
	if status.State != catalog.MetadataFailed {
		t.Fatalf("relations state = %s, want failed", status.State)
	}
	if p.Calls.Load() != 1 {
		t.Fatalf("Calls = %d, want 1 (no auto-retry)", p.Calls.Load())
	}

	for i := 0; i < 3; i++ {
		_ = lease.Snapshot()
	}
	if p.Calls.Load() != 1 {
		t.Fatalf("Calls = %d, want 1 (repeated reads must not retry)", p.Calls.Load())
	}

	p.Failure = nil
	if err := lease.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	waitReadyOrFatal(t, lease)
	if p.Calls.Load() != 2 {
		t.Fatalf("Calls = %d, want 2 (explicit refresh retried)", p.Calls.Load())
	}
	status = lease.Snapshot().Status[catalog.MetadataRelations]
	if status.State != catalog.MetadataReady {
		t.Fatalf("relations state after refresh = %s, want ready", status.State)
	}
}

// TestDetailFailedRequiredCategoryBlocksLoad: when a required category
// failed rather than merely not-yet-settling, Detail must return an error
// referencing the underlying cause, not proceed to Load against an
// incomplete/wrong cache.
func TestDetailFailedRequiredCategoryBlocksLoad(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)
	p := metadatatest.New()
	p.Failure = errors.New("relations boom")
	key := metadata.Key{Engine: "interbase", URL: "ib://host/detail-category-failed"}
	scope := metadata.Scope{Principal: "U"}
	lease, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)

	if status := lease.Snapshot().Status[catalog.MetadataRelations]; status.State != catalog.MetadataFailed {
		t.Fatalf("relations state = %s, want failed", status.State)
	}

	request := metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Facet: "source", Consistency: metadata.Committed}
	_, err = lease.Detail(ctx, request)
	if err == nil {
		t.Fatal("Detail succeeded despite a failed required category")
	}
	if !errors.Is(err, metadata.ErrCategoryFailed) {
		t.Fatalf("err = %v, want it to wrap ErrCategoryFailed", err)
	}
	if !strings.Contains(err.Error(), "relations boom") {
		t.Fatalf("err = %v, want it to reference the underlying cause", err)
	}
	if p.DetailCalls.Load() != 0 {
		t.Fatal("Load must not run against a failed required category's cache")
	}
}

// TestLastReleaseThenReacquireReloadsDetail exercises the registry's
// incarnation lifecycle together with detail coordination: once every lease
// releases and the store retires, a fresh Acquire must genuinely reload
// detail rather than reuse anything from the torn-down store.
func TestLastReleaseThenReacquireReloadsDetail(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)
	p := metadatatest.New()
	p.DetailValue = metadata.Detail{Text: "X"}
	key := metadata.Key{Engine: "interbase", URL: "ib://host/detail-reacquire"}
	scope := metadata.Scope{Principal: "U"}
	request := metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Facet: "source", Consistency: metadata.Committed}

	lease1, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	waitReadyOrFatal(t, lease1)
	if _, err := lease1.Detail(ctx, request); err != nil {
		t.Fatal(err)
	}
	if p.DetailCalls.Load() != 1 {
		t.Fatal("expected exactly one initial detail load")
	}
	incarnation1 := lease1.Incarnation()
	lease1.Release()

	lease2, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease2.Release()
	waitReadyOrFatal(t, lease2)
	if lease2.Incarnation() == incarnation1 {
		t.Fatal("reacquire reused the torn-down store instead of a fresh incarnation")
	}
	if _, err := lease2.Detail(ctx, request); err != nil {
		t.Fatal(err)
	}
	if p.DetailCalls.Load() != 2 {
		t.Fatalf("DetailCalls = %d, want 2 (fresh incarnation must reload, not reuse a torn-down cache)", p.DetailCalls.Load())
	}
}
