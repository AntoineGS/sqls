package metadata_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

// TestIdentityAndEventNeverCarrySecrets guards the rule that Key, Scope and
// Event never carry a password, session ID or any other secret field.
func TestIdentityAndEventNeverCarrySecrets(t *testing.T) {
	banned := []string{"password", "pwd", "secret", "session", "credential", "token"}
	for name, v := range map[string]any{
		"Key":   metadata.Key{},
		"Scope": metadata.Scope{},
		"Event": metadata.Event{},
	} {
		typ := reflect.TypeOf(v)
		for i := 0; i < typ.NumField(); i++ {
			field := strings.ToLower(typ.Field(i).Name)
			for _, word := range banned {
				if strings.Contains(field, word) {
					t.Fatalf("%s.%s looks like a secret field", name, typ.Field(i).Name)
				}
			}
		}
	}
}

func waitReadyOrFatal(t *testing.T, lease *metadata.Lease) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := metadatatest.WaitReady(ctx, lease); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSameURLConnectionsShareStore(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3, MaxDetailBytes: 64 << 20})
	defer r.Close(ctx)
	p := metadatatest.New()
	key := metadata.Key{Engine: "interbase", URL: `ib://server/3050:D:\Data\APP.GBB`}
	scope := metadata.Scope{Principal: "USER", TextPolicy: "NONE/WIN1252"}
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
	if a.Incarnation() != b.Incarnation() {
		t.Fatal("two stores for one URL")
	}
	waitReadyOrFatal(t, a)
	waitReadyOrFatal(t, b)
	if p.Calls.Load() != 1 {
		t.Fatalf("duplicate catalog fetch: Calls=%d", p.Calls.Load())
	}
}

// TestAcquireSimultaneousSharesOneCreation launches two Acquire calls from a
// shared start barrier (not a sleep) so they enter Acquire together, then
// proves via an atomic counter that exactly one factory call served both.
func TestAcquireSimultaneousSharesOneCreation(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3})
	defer r.Close(ctx)

	var factoryCalls atomic.Int64
	entered := make(chan struct{})
	var enteredOnce sync.Once
	gate := make(chan struct{})
	factory := func(fctx context.Context) (metadata.Provider, error) {
		factoryCalls.Add(1)
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-gate:
		case <-fctx.Done():
			return nil, fctx.Err()
		}
		return metadatatest.New(), nil
	}

	key := metadata.Key{Engine: "interbase", URL: "ib://host/db"}
	scope := metadata.Scope{Principal: "U"}

	type result struct {
		lease *metadata.Lease
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
	launched.Wait() // both goroutines are scheduled and blocked on start
	close(start)    // release both to call Acquire together

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("factory never entered")
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

// TestAcquireCancelDuringWaitDoesNotAffectSurvivor proves the invariant
// directly instead of racing on timing: the survivor's Acquire call is
// confirmed (via the factory's Entered signal, which can only fire after
// Acquire's own ref-increment already ran) to hold a live reference before a
// second, cancelable joiner is even started, so the joiner's cancellation —
// whenever it lands relative to the joiner's own ref bookkeeping — can never
// remove the survivor's reference.
func TestAcquireCancelDuringWaitDoesNotAffectSurvivor(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3})
	defer r.Close(ctx)

	entered := make(chan struct{})
	var enteredOnce sync.Once
	gate := make(chan struct{})
	factory := func(fctx context.Context) (metadata.Provider, error) {
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-gate:
		case <-fctx.Done():
			return nil, fctx.Err()
		}
		return metadatatest.New(), nil
	}

	key := metadata.Key{Engine: "interbase", URL: "ib://host/cancel"}
	scope := metadata.Scope{Principal: "U"}

	type result struct {
		lease *metadata.Lease
		err   error
	}
	survivorDone := make(chan result, 1)
	go func() {
		l, err := r.Acquire(context.Background(), key, scope, factory)
		survivorDone <- result{l, err}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("factory never entered")
	}
	// The survivor's Acquire call has already incremented its reference:
	// refs++ always happens before the factory call that signals Entered.

	joinerCtx, cancelJoiner := context.WithCancel(context.Background())
	joinerDone := make(chan error, 1)
	go func() {
		_, err := r.Acquire(joinerCtx, key, scope, factory)
		joinerDone <- err
	}()
	cancelJoiner()

	if joinerErr := <-joinerDone; !errors.Is(joinerErr, context.Canceled) {
		t.Fatalf("joiner error = %v, want context.Canceled", joinerErr)
	}

	close(gate)
	survivor := <-survivorDone
	if survivor.err != nil {
		t.Fatalf("survivor Acquire failed after joiner canceled: %v", survivor.err)
	}
	defer survivor.lease.Release()
	waitReadyOrFatal(t, survivor.lease)
}

func TestReleaseOneRetainsOther(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3})
	defer r.Close(ctx)
	p := metadatatest.New()
	key := metadata.Key{Engine: "interbase", URL: "ib://host/retain"}
	scope := metadata.Scope{Principal: "U"}

	a, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	waitReadyOrFatal(t, a)
	a.Release()
	waitReadyOrFatal(t, b)
	if p.Closes.Load() != 0 {
		t.Fatalf("releasing one lease closed the shared provider: Closes=%d", p.Closes.Load())
	}
	b.Release()
	waitFor(t, time.Second, func() bool { return p.Closes.Load() == 1 })
}

func TestFinalReleaseDuringInFlightFactoryCleansUp(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3})
	defer r.Close(ctx)

	p := metadatatest.New()
	release := make(chan struct{})
	factory := func(fctx context.Context) (metadata.Provider, error) {
		<-release // ignores cancellation deliberately, like a slow real connect
		return p, nil
	}

	key := metadata.Key{Engine: "interbase", URL: "ib://host/inflight"}
	scope := metadata.Scope{Principal: "U"}

	cancelCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Acquire(cancelCtx, key, scope, factory)
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire error = %v, want context.Canceled", err)
	}

	close(release)
	waitFor(t, time.Second, func() bool { return p.Closes.Load() == 1 })
}

func TestDifferentURLsGetSeparateStores(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3})
	defer r.Close(ctx)
	p := metadatatest.New()
	scope := metadata.Scope{Principal: "U"}

	a, err := r.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "ib://host/one"}, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := r.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "ib://host/two"}, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release()
	if a.Incarnation() == b.Incarnation() {
		t.Fatal("different URLs shared one store")
	}
	waitReadyOrFatal(t, a)
	waitReadyOrFatal(t, b)
	if p.Calls.Load() != 2 {
		t.Fatalf("Calls = %d, want 2", p.Calls.Load())
	}
}

func TestDifferentScopeUnderOneKeyGetsSeparateTrack(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3})
	defer r.Close(ctx)
	p := metadatatest.New()
	key := metadata.Key{Engine: "interbase", URL: "ib://host/shared"}

	a, err := r.Acquire(ctx, key, metadata.Scope{Principal: "ALICE"}, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := r.Acquire(ctx, key, metadata.Scope{Principal: "BOB"}, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release()
	waitReadyOrFatal(t, a)
	waitReadyOrFatal(t, b)
	if p.Calls.Load() != 2 {
		t.Fatalf("Calls = %d, want 2 (scopes must not share a load)", p.Calls.Load())
	}
}

func TestDifferentRoleUnderOneKeyGetsSeparateTrack(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3})
	defer r.Close(ctx)
	p := metadatatest.New()
	key := metadata.Key{Engine: "interbase", URL: "ib://host/roles"}

	a, err := r.Acquire(ctx, key, metadata.Scope{Principal: "USER", Role: "READER"}, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := r.Acquire(ctx, key, metadata.Scope{Principal: "USER", Role: "ADMIN"}, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release()
	waitReadyOrFatal(t, a)
	waitReadyOrFatal(t, b)
	if p.Calls.Load() != 2 {
		t.Fatalf("Calls = %d, want 2 (different roles under one Key must not share a load)", p.Calls.Load())
	}
}

func TestAcquireEmptyScopeRejected(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)
	p := metadatatest.New()
	_, err := r.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "ib://host/empty"}, metadata.Scope{}, p.Factory)
	if !errors.Is(err, metadata.ErrScopeRequired) {
		t.Fatalf("err = %v, want ErrScopeRequired", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)
	p := metadatatest.New()
	a, err := r.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "ib://host/idempotent"}, metadata.Scope{Principal: "U"}, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	a.Release()
	a.Release()
	waitFor(t, time.Second, func() bool { return p.Closes.Load() == 1 })
}

func TestFactoryFailureAllowsRetry(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)

	attempt := 0
	key := metadata.Key{Engine: "interbase", URL: "ib://host/retry"}
	scope := metadata.Scope{Principal: "U"}
	failing := func(context.Context) (metadata.Provider, error) {
		attempt++
		return nil, errors.New("boom")
	}
	if _, err := r.Acquire(ctx, key, scope, failing); err == nil {
		t.Fatal("expected failure")
	}
	p := metadatatest.New()
	lease, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)
	if attempt != 1 {
		t.Fatalf("failing factory called %d times", attempt)
	}
}

func TestRegistryCloseRejectsFurtherAcquire(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	p := metadatatest.New()
	_, err := r.Acquire(ctx, metadata.Key{Engine: "interbase", URL: "ib://host/closed"}, metadata.Scope{Principal: "U"}, p.Factory)
	if !errors.Is(err, metadata.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

// TestAcquireReleaseChurnNeverHoldsTwoIncarnations is a regression test for a
// bug where an Acquire call could observe a registry entry after it was
// emptied and removed by a concurrent last Release, but before that removal
// completed, and attach a new track to the now-orphaned entry — leaving a
// third Acquire to create a second, independent store for the same
// (Key, Scope). It reproduces the reviewer's finding: several goroutines
// churning acquire -> record incarnation -> release, asserting that at any
// instant at most one distinct Incarnation is held for the (Key, Scope).
func TestAcquireReleaseChurnNeverHoldsTwoIncarnations(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{})
	defer r.Close(ctx)

	key := metadata.Key{Engine: "interbase", URL: "ib://host/churn"}
	scope := metadata.Scope{Principal: "U"}
	factory := func(context.Context) (metadata.Provider, error) { return metadatatest.New(), nil }

	const goroutines = 8
	const iterations = 3000

	var mu sync.Mutex
	held := make(map[uint64]int)
	var violation atomic.Value // string

	report := func(msg string) {
		violation.CompareAndSwap(nil, msg)
	}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if violation.Load() != nil {
					return
				}
				lease, err := r.Acquire(ctx, key, scope, factory)
				if err != nil {
					report(fmt.Sprintf("acquire error: %v", err))
					return
				}
				inc := lease.Incarnation()
				mu.Lock()
				held[inc]++
				distinct := len(held)
				mu.Unlock()
				if distinct > 1 {
					report(fmt.Sprintf("observed %d distinct incarnations held concurrently for one (Key, Scope)", distinct))
				}
				// Drop this incarnation from the held set before actually
				// releasing: Release() drops the registry's real reference
				// synchronously before it returns, so decrementing after
				// would leave a window where a since-retired incarnation
				// still looks "held" while a legitimately new one already
				// exists — a false positive in this checker, not a registry
				// bug. Decrementing first never hides a real double-hold: the
				// registry's own reference is still live until Release()
				// actually runs, so any concurrent Acquire during that
				// window still correctly joins this same live track.
				mu.Lock()
				held[inc]--
				if held[inc] == 0 {
					delete(held, inc)
				}
				mu.Unlock()
				lease.Release()
			}
		}()
	}
	wg.Wait()

	if msg := violation.Load(); msg != nil {
		t.Fatal(msg)
	}
}

// TestAcquireDuringCloseNeverHangsOrLeaks is a regression test for a bug
// where a concurrent Acquire racing Registry.Close could return a
// successful-looking Lease whose underlying loader never started (because
// the registry was closed after the provider was created but before Start
// ran), leaving WaitReady to hang forever. Every Acquire call here must
// either fail with ErrClosed or return a lease that settles quickly.
func TestAcquireDuringCloseNeverHangsOrLeaks(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		r := metadata.NewRegistry(context.Background(), metadata.Options{})
		key := metadata.Key{Engine: "interbase", URL: fmt.Sprintf("ib://host/closerace-%d", iter)}
		scope := metadata.Scope{Principal: "U"}
		// A factory that ignores cancellation entirely, like a real cgo
		// connect that cannot be interrupted mid-flight.
		factory := func(context.Context) (metadata.Provider, error) {
			return metadatatest.New(), nil
		}

		type result struct {
			lease *metadata.Lease
			err   error
		}
		results := make(chan result, 4)
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l, err := r.Acquire(context.Background(), key, scope, factory)
				results <- result{l, err}
			}()
		}
		go func() { r.Close(context.Background()) }()
		wg.Wait()
		close(results)

		for res := range results {
			if res.err != nil {
				if !errors.Is(res.err, metadata.ErrClosed) {
					t.Fatalf("iter %d: unexpected Acquire error: %v", iter, res.err)
				}
				continue
			}
			waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := metadatatest.WaitReady(waitCtx, res.lease)
			cancel()
			res.lease.Release()
			if err != nil {
				t.Fatalf("iter %d: successful lease never settled: %v", iter, err)
			}
		}
	}
}
