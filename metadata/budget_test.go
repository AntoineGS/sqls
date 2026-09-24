package metadata

import (
	"context"
	"testing"
)

// TestNewBudgetDefaultsNonPositiveValues proves a zero/negative budget can
// never silently mean unbounded: it falls back to the registry defaults.
func TestNewBudgetDefaultsNonPositiveValues(t *testing.T) {
	b := NewBudget(0, 0)
	sb := b.ForStore(Key{Engine: "e", URL: "u"})
	if cap(sb.host) != defaultHostJobs {
		t.Fatalf("host cap = %d, want default %d", cap(sb.host), defaultHostJobs)
	}
	if cap(sb.sem) != defaultStoreJobs {
		t.Fatalf("store cap = %d, want default %d", cap(sb.sem), defaultStoreJobs)
	}

	b2 := NewBudget(-1, -1)
	sb2 := b2.ForStore(Key{Engine: "e", URL: "u2"})
	if cap(sb2.host) != defaultHostJobs || cap(sb2.sem) != defaultStoreJobs {
		t.Fatalf("negative caps = (%d,%d), want defaults (%d,%d)", cap(sb2.host), cap(sb2.sem), defaultHostJobs, defaultStoreJobs)
	}
}

// TestNewBudgetPreservesPositiveOverrides proves an explicit positive value
// is honored rather than replaced by the default.
func TestNewBudgetPreservesPositiveOverrides(t *testing.T) {
	b := NewBudget(10, 4)
	sb := b.ForStore(Key{Engine: "e", URL: "u"})
	if cap(sb.host) != 10 || cap(sb.sem) != 4 {
		t.Fatalf("caps = (%d,%d), want (10,4)", cap(sb.host), cap(sb.sem))
	}
}

// TestForStoreSharesLiveInstanceThenAllocatesFreshOnceReleased is the crux of
// the "shared across scopes and across a retiring incarnation and its
// replacement" requirement: while any reference to a Key's permit set is
// outstanding, ForStore must return that SAME instance; only once every
// reference has been released does a later ForStore allocate a fresh one.
func TestForStoreSharesLiveInstanceThenAllocatesFreshOnceReleased(t *testing.T) {
	b := NewBudget(6, 3)
	key := Key{Engine: "e", URL: "shared"}

	a := b.ForStore(key) // simulates the retiring incarnation's reference
	c := b.ForStore(key) // simulates a second scope, or the replacement incarnation
	if a != c {
		t.Fatal("ForStore returned different instances while a reference was still live")
	}

	b.release(key) // the retiring incarnation finishes draining
	if d := b.ForStore(key); d != a {
		t.Fatal("ForStore allocated a new instance while another reference (c) was still live")
	}
	b.release(key) // undo the extra ForStore call above
	b.release(key) // c's reference

	fresh := b.ForStore(key)
	if fresh == a {
		t.Fatal("ForStore reused a fully-released permit set instead of allocating a fresh one")
	}
}

// TestStoreBudgetAcquireRespectsContext proves acquire aborts cleanly (and
// does not leak a store-level permit) when ctx is done while waiting for the
// host-level permit.
func TestStoreBudgetAcquireRespectsContext(t *testing.T) {
	b := NewBudget(1, 1)
	sb := b.ForStore(Key{Engine: "e", URL: "u"})

	ctx, cancel := context.WithCancel(context.Background())
	// Hold the only host permit directly so the second acquire blocks there.
	sb.host <- struct{}{}
	defer func() { <-sb.host }()

	cancel()
	if _, err := sb.acquire(ctx); err == nil {
		t.Fatal("acquire with a done context should have failed")
	}
	select {
	case sb.sem <- struct{}{}:
		<-sb.sem
	default:
		t.Fatal("a canceled acquire leaked its store-level permit")
	}
}
