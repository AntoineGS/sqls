package metadata

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
)

// whiteboxProvider is a minimal metadata.Provider stub for this file's
// internal (package metadata) test. It cannot import metadatatest: that
// package imports metadata, and this file is part of metadata itself.
type whiteboxProvider struct{}

func (whiteboxProvider) Plan() catalog.MetadataPlan {
	return catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{
		Kind: catalog.MetadataRelations,
		Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{Cache: &catalog.Cache{SchemaTables: map[string][]string{"": {"T"}}}}, nil
		},
	}}}
}
func (whiteboxProvider) DetailPlan(Request) (DetailPlan, error) { return DetailPlan{}, ErrUnsupported }
func (whiteboxProvider) Close() error                           { return nil }

// waitForTrackRefs polls the unexported track ref count for (key, sk) under
// r.mu — the same lock attach/releaseTrackRef use — until it reaches at
// least want, or fails the test after timeout. This is a bounded wait on an
// internal invariant, not a guess about scheduling: it proves a specific
// number of Acquire calls have genuinely attached before the caller does
// anything time-dependent (like canceling one of them).
func waitForTrackRefs(t *testing.T, r *Registry, key Key, sk scopeKey, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		entry := r.entries[key]
		var refs int
		if entry != nil {
			if tr := entry.tracks[sk]; tr != nil {
				refs = tr.refs
			}
		}
		r.mu.Unlock()
		if refs >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d Acquire call(s) had attached (want %d) before the deadline", refs, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestAcquireSimultaneousAttachBothBeforeCreate is a whitebox strengthening
// of the external TestAcquireSimultaneousSharesOneCreation. That test's gate
// opens as soon as the creator's factory call signals Entered, which does
// not by itself prove the second Acquire call had genuinely attached to the
// track before the factory was allowed to return — a second call arriving
// late would just join the still-held track and pass the same way. This
// test proves genuine concurrent attachment directly, using the unexported
// track ref count reachable from this internal (package metadata) test: it
// waits for the track's ref count to reach 2 before releasing the gate.
func TestAcquireSimultaneousAttachBothBeforeCreate(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(ctx, Options{MaxHostJobs: 6, MaxStoreJobs: 3})
	defer r.Close(ctx)

	var factoryCalls atomic.Int64
	entered := make(chan struct{})
	var enteredOnce sync.Once
	gate := make(chan struct{})
	factory := func(fctx context.Context) (Provider, error) {
		factoryCalls.Add(1)
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-gate:
		case <-fctx.Done():
			return nil, fctx.Err()
		}
		return whiteboxProvider{}, nil
	}

	key := Key{Engine: "interbase", URL: "ib://host/whitebox"}
	scope := Scope{Principal: "U"}
	sk := newScopeKey(scope)

	type result struct {
		lease *Lease
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var launched sync.WaitGroup
	launched.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			launched.Done()
			<-start
			l, err := r.Acquire(ctx, key, scope, factory)
			results <- result{l, err}
		}()
	}
	launched.Wait()
	close(start)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("factory never entered")
	}

	waitForTrackRefs(t, r, key, sk, 2, 5*time.Second)
	close(gate)

	r1 := <-results
	r2 := <-results
	if r1.err != nil {
		t.Fatal(r1.err)
	}
	if r2.err != nil {
		t.Fatal(r2.err)
	}
	defer r1.lease.Release()
	defer r2.lease.Release()
	if r1.lease.Incarnation() != r2.lease.Incarnation() {
		t.Fatal("simultaneous acquires produced separate stores")
	}
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("factory called %d times, want exactly 1", got)
	}
}

// TestAcquireCreatorCanceledJoinerSurvives covers the plan's Review Focus #2
// scenario directly: "A canceled first acquirer must not close a provider
// another acquirer still needs." A's own Acquire call is the creator (it
// triggers the gated factory call); B, on an independent, uncancelled
// context, must join the SAME live track and still receive a working lease
// even though A's own context is canceled while A is still waiting.
//
// Proving this deterministically requires knowing B has genuinely attached
// (refs == 2) BEFORE A is canceled — a WaitGroup only proves B's goroutine
// has started, not that it has reached attach() inside Acquire, so it can
// race: A could be canceled (dropping refs to 0 and retiring the track)
// before B's Acquire call ever reaches attach(), in which case B would
// legitimately create a fresh track and call the factory a second time even
// though production behavior is correct. This is why the test lives here,
// using the same whiteboxed track-ref polling as
// TestAcquireSimultaneousAttachBothBeforeCreate, rather than in the external
// metadata_test package.
func TestAcquireCreatorCanceledJoinerSurvives(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(ctx, Options{MaxHostJobs: 6, MaxStoreJobs: 3})
	defer r.Close(ctx)

	var factoryCalls atomic.Int64
	entered := make(chan struct{})
	var enteredOnce sync.Once
	gate := make(chan struct{})
	factory := func(fctx context.Context) (Provider, error) {
		factoryCalls.Add(1)
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-gate:
		case <-fctx.Done():
			return nil, fctx.Err()
		}
		return whiteboxProvider{}, nil
	}

	key := Key{Engine: "interbase", URL: "ib://host/whitebox-creator-cancel"}
	scope := Scope{Principal: "U"}
	sk := newScopeKey(scope)

	// A is the creator: the first Acquire call for this (Key, Scope), so its
	// factory call is the one that ends up gated.
	creatorCtx, cancelCreator := context.WithCancel(context.Background())
	creatorDone := make(chan error, 1)
	go func() {
		_, err := r.Acquire(creatorCtx, key, scope, factory)
		creatorDone <- err
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("factory never entered")
	}

	// B joins the same still-live track.
	type result struct {
		lease *Lease
		err   error
	}
	joinerDone := make(chan result, 1)
	go func() {
		l, err := r.Acquire(context.Background(), key, scope, factory)
		joinerDone <- result{l, err}
	}()

	// Whitebox: do not cancel A until the track's own ref count proves B has
	// genuinely attached (refs == 2: A's own reference plus B's). Only then
	// is "a canceled first acquirer must not close a provider another
	// acquirer still needs" actually being exercised.
	waitForTrackRefs(t, r, key, sk, 2, 5*time.Second)

	cancelCreator()

	// A's own context died mid-wait (gate is still closed at this point, so
	// A cannot have raced ahead to <-t.ready): its Acquire call must fail
	// with context.Canceled. The point of the test is that B is unaffected.
	if creatorErr := <-creatorDone; !errors.Is(creatorErr, context.Canceled) {
		t.Fatalf("creator error = %v, want context.Canceled", creatorErr)
	}

	close(gate)
	joiner := <-joinerDone
	if joiner.err != nil {
		t.Fatalf("joiner Acquire failed after creator (first acquirer) canceled: %v", joiner.err)
	}
	defer joiner.lease.Release()

	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("factory called %d times, want exactly 1 (a canceled first acquirer must not close a provider another acquirer still needs)", got)
	}
}
