package metadata

import (
	"context"
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

	// Whitebox: poll the track's own ref count directly until both Acquire
	// calls have attached, rather than assuming the second one arrived in
	// time. This is a bounded wait on an internal invariant, not a guess
	// about scheduling.
	deadline := time.Now().Add(5 * time.Second)
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
		if refs >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d Acquire call(s) had attached before the deadline", refs)
		}
		time.Sleep(time.Millisecond)
	}

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
