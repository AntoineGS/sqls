package metadata

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
)

// TestScheduleDoesNotBusySpinWhenHostPermitSetIsFull is a regression test
// for a busy-spin introduced by round 1's lost-wakeup fix: tryAcquireSlot's
// rollback (store permit taken, then rolled back because the host permit
// set was full) broadcasts storeWake to correctly wake OTHER waiters for a
// store-level free-up, but schedule's own parking select still listened on
// the very storeWakeCh epoch that rollback had just closed, so it woke
// itself immediately, retried, rolled back again, and repeated — spinning
// at full CPU for as long as the host stayed full, instead of genuinely
// parking until a host-level release actually happened.
//
// This directly measures loop behavior via Loader.acquireAttempts (an
// internal diagnostic counter, see loader.go), not just "did it eventually
// succeed" (which TestLostWakeup* in wakeup_test.go already establishes and
// would not catch a spin: a spinning loop also eventually succeeds once the
// host frees up, it just burns CPU the whole time it's blocked).
func TestScheduleDoesNotBusySpinWhenHostPermitSetIsFull(t *testing.T) {
	budget := NewBudget(1, 5) // host=1 (the scarce resource), store=5 (never the constraint here)
	holder := budget.ForStore(Key{Engine: "e", URL: "holder"})
	underTest := budget.ForStore(Key{Engine: "e", URL: "under-test"})

	// Occupy the only host permit directly, simulating another store's job
	// (or detail load) holding it, without needing a second Loader.
	holder.host <- struct{}{}

	loader := NewLoader(underTest)
	t.Cleanup(loader.Stop)
	loader.Reset(1)

	entered := make(chan struct{})
	var enteredOnce sync.Once
	load, err := loader.Start(context.Background(), 1, catalog.MetadataPlan{
		Parallelism: 1,
		Jobs: []catalog.MetadataJob{{
			Kind: catalog.MetadataRelations,
			Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
				enteredOnce.Do(func() { close(entered) })
				return catalog.MetadataPatch{Cache: &catalog.Cache{SchemaTables: map[string][]string{}}}, nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The job must be genuinely blocked on the full host permit set: give
	// it a bounded window during which it must not have entered.
	select {
	case <-entered:
		t.Fatal("job entered while the host semaphore was fully held by another store")
	case <-time.After(200 * time.Millisecond):
	}

	// A genuinely parked loader attempts acquisition once (maybe a couple
	// more times from unrelated legitimate wakes), never thousands of times,
	// during a 200ms window with nothing available to wake it. The busy-spin
	// this guards against attempted many tens of thousands of times in the
	// same window when reproduced against the pre-fix code.
	const maxReasonableAttempts = 50
	if attempts := loader.acquireAttempts.Load(); attempts > maxReasonableAttempts {
		t.Fatalf("tryAcquireSlot called %d times while genuinely blocked on a full host permit set (want <= %d): busy-spinning", attempts, maxReasonableAttempts)
	}

	// Release the held host permit exactly like StoreBudget.acquire's
	// release closure would, so the loader's hostWake correctly fires.
	<-holder.host
	holder.hostWake.broadcast()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("job never woke up after the host permit was released (regressed back to the original lost-wakeup bug)")
	}
	waitLoad(t, load)
}
