package metadata_test

import (
	"context"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

// TestLostWakeupAcrossStoresSharedHostSemaphore reproduces the reviewer's
// scenario: with MaxHostJobs=1, store A holds the only host permit via a
// gated category job. Store B's relations job must be genuinely blocked
// while A holds it, and must wake and proceed once A releases — without the
// fix, B's loader parks on its own private wake signal that A's release
// never fires, and hangs forever.
func TestLostWakeupAcrossStoresSharedHostSemaphore(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 1, MaxStoreJobs: 3})
	defer r.Close(ctx)

	pa := metadatatest.New()
	gateA := make(chan struct{})
	pa.Gate = gateA
	pb := metadatatest.New()

	scope := metadata.Scope{Principal: "U"}
	keyA := metadata.Key{Engine: "interbase", URL: "ib://host/wake-a"}
	keyB := metadata.Key{Engine: "interbase", URL: "ib://host/wake-b"}

	leaseA, err := r.Acquire(ctx, keyA, scope, pa.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseA.Release()
	select {
	case <-pa.Entered:
	case <-time.After(5 * time.Second):
		t.Fatal("store A's relations job never entered")
	}

	leaseB, err := r.Acquire(ctx, keyB, scope, pb.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseB.Release()

	// B must be genuinely blocked while A holds the only host permit: give
	// it a bounded window during which it must NOT have entered.
	select {
	case <-pb.Entered:
		t.Fatal("store B's relations job entered while the host semaphore was fully held by store A")
	case <-time.After(200 * time.Millisecond):
	}

	close(gateA)
	waitReadyOrFatal(t, leaseA)

	// This is exactly the lost-wakeup scenario: without the fix, B hangs
	// here forever because nothing ever signals its private wake channel.
	select {
	case <-pb.Entered:
	case <-time.After(5 * time.Second):
		t.Fatal("store B never woke up after store A released the shared host permit (lost wakeup)")
	}
	waitReadyOrFatal(t, leaseB)
}

// TestLostWakeupAcrossScopesSameKeySharedStoreSemaphore covers the "sibling
// scope under the same Key" case: two scopes under one Key share one
// StoreBudget (and therefore one per-store semaphore), each with its own
// Loader. A release by one scope's loader must wake the other's.
func TestLostWakeupAcrossScopesSameKeySharedStoreSemaphore(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 5, MaxStoreJobs: 1})
	defer r.Close(ctx)

	key := metadata.Key{Engine: "interbase", URL: "ib://host/wake-scope"}
	pa := metadatatest.New()
	gateA := make(chan struct{})
	pa.Gate = gateA
	pb := metadatatest.New()

	leaseA, err := r.Acquire(ctx, key, metadata.Scope{Principal: "ALICE"}, pa.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseA.Release()
	select {
	case <-pa.Entered:
	case <-time.After(5 * time.Second):
		t.Fatal("scope ALICE's relations job never entered")
	}

	leaseB, err := r.Acquire(ctx, key, metadata.Scope{Principal: "BOB"}, pb.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseB.Release()

	select {
	case <-pb.Entered:
		t.Fatal("scope BOB's relations job entered while the per-store semaphore was fully held by scope ALICE")
	case <-time.After(200 * time.Millisecond):
	}

	close(gateA)
	waitReadyOrFatal(t, leaseA)

	select {
	case <-pb.Entered:
	case <-time.After(5 * time.Second):
		t.Fatal("scope BOB never woke up after scope ALICE released the shared store permit (lost wakeup)")
	}
	waitReadyOrFatal(t, leaseB)
}

// TestLostWakeupDetailLoadReleaseWakesWaitingCategoryJob covers the "a
// detail load finishing and releasing its permit" case: store A holds the
// host's only permit via a gated DETAIL load (not a category job); store
// B's category job must still wake once that detail load's permit is
// released.
func TestLostWakeupDetailLoadReleaseWakesWaitingCategoryJob(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 1, MaxStoreJobs: 3})
	defer r.Close(ctx)

	pa := metadatatest.New()
	detailGate := make(chan struct{})
	pa.DetailGate = detailGate
	keyA := metadata.Key{Engine: "interbase", URL: "ib://host/wake-detail-a"}
	scope := metadata.Scope{Principal: "U"}
	leaseA, err := r.Acquire(ctx, keyA, scope, pa.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseA.Release()
	waitReadyOrFatal(t, leaseA)

	detailReq := metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Facet: "source", Consistency: metadata.Committed}
	detailDone := make(chan error, 1)
	go func() {
		_, err := leaseA.Detail(context.Background(), detailReq)
		detailDone <- err
	}()
	select {
	case <-pa.DetailEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("store A's detail load never entered")
	}

	pb := metadatatest.New()
	keyB := metadata.Key{Engine: "interbase", URL: "ib://host/wake-detail-b"}
	leaseB, err := r.Acquire(ctx, keyB, scope, pb.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseB.Release()

	select {
	case <-pb.Entered:
		t.Fatal("store B's relations job entered while the host semaphore was held by store A's detail load")
	case <-time.After(200 * time.Millisecond):
	}

	close(detailGate)
	if err := <-detailDone; err != nil {
		t.Fatal(err)
	}

	select {
	case <-pb.Entered:
	case <-time.After(5 * time.Second):
		t.Fatal("store B never woke up after store A's detail load released the shared host permit (lost wakeup)")
	}
	waitReadyOrFatal(t, leaseB)
}
