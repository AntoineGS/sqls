package metadata_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
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

func TestAcquireSimultaneousSharesOneCreation(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3})
	defer r.Close(ctx)

	entered := make(chan struct{})
	var enteredOnce int32
	gate := make(chan struct{})
	factory := func(fctx context.Context) (metadata.Provider, error) {
		if enteredOnce == 0 {
			enteredOnce = 1
			close(entered)
		}
		select {
		case <-gate:
		case <-fctx.Done():
			return nil, fctx.Err()
		}
		p := metadatatest.New()
		return p, nil
	}

	key := metadata.Key{Engine: "interbase", URL: "ib://host/db"}
	scope := metadata.Scope{Principal: "U"}

	type result struct {
		lease *metadata.Lease
		err   error
	}
	results := make(chan result, 2)
	go func() {
		l, err := r.Acquire(ctx, key, scope, factory)
		results <- result{l, err}
	}()
	go func() {
		l, err := r.Acquire(ctx, key, scope, factory)
		results <- result{l, err}
	}()

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
}

func TestAcquireCancelDuringWaitDoesNotAffectSurvivor(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxHostJobs: 6, MaxStoreJobs: 3})
	defer r.Close(ctx)

	gate := make(chan struct{})
	factory := func(fctx context.Context) (metadata.Provider, error) {
		select {
		case <-gate:
		case <-fctx.Done():
			return nil, fctx.Err()
		}
		return metadatatest.New(), nil
	}

	key := metadata.Key{Engine: "interbase", URL: "ib://host/cancel"}
	scope := metadata.Scope{Principal: "U"}

	cancelCtx, cancel := context.WithCancel(context.Background())
	type result struct {
		lease *metadata.Lease
		err   error
	}
	firstDone := make(chan result, 1)
	go func() {
		l, err := r.Acquire(cancelCtx, key, scope, factory)
		firstDone <- result{l, err}
	}()

	secondDone := make(chan result, 1)
	go func() {
		l, err := r.Acquire(context.Background(), key, scope, factory)
		secondDone <- result{l, err}
	}()

	// Give both Acquire calls a chance to register their interest before the
	// first one cancels.
	time.Sleep(20 * time.Millisecond)
	cancel()

	first := <-firstDone
	if !errors.Is(first.err, context.Canceled) {
		t.Fatalf("first Acquire error = %v, want context.Canceled", first.err)
	}
	if first.lease != nil {
		t.Fatal("canceled Acquire returned a lease")
	}

	close(gate)
	second := <-secondDone
	if second.err != nil {
		t.Fatalf("second Acquire failed after first canceled: %v", second.err)
	}
	defer second.lease.Release()
	waitReadyOrFatal(t, second.lease)
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
