package language

import (
	"fmt"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/metadata"
)

func TestServerCacheDelayedOlderGenerationCannotRewindOrPoisonNewerRequest(t *testing.T) {
	cache := &serverCache{}
	id := serverIdentity{
		key:         metadata.Key{Engine: "interbase", URL: "fixture://cache-generation"},
		incarnation: 1,
		variant:     dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
	}
	older := serverGen{generation: 1, revision: 4}
	newer := serverGen{generation: 2, revision: 1}
	olderKey := serverCacheKey{id: id, generation: older, sql: "SELECT _STALE"}
	newerKey := serverCacheKey{id: id, generation: newer, sql: "SELECT _STALE"}
	oldFinding := serverOutcome{err: &StatementError{SQLCode: -104, Message: "old generation"}}

	cache.sync(id, older)
	cache.put(olderKey, oldFinding)
	cache.sync(id, newer)

	// Model an older request that was delayed before its first cache sync. It
	// arrives only after the newer request has established generation 2.
	cache.sync(id, older)
	cache.put(olderKey, oldFinding)
	if got := cache.gens[id]; got != newer {
		t.Fatalf("active generation = %+v, want newer %+v", got, newer)
	}
	if _, ok := cache.get(newerKey); ok {
		t.Fatal("newer generation read an older request's result")
	}
	if _, ok := cache.get(olderKey); ok {
		t.Fatal("older request read a result after its generation became stale")
	}

	olderRevision := serverGen{generation: newer.generation, revision: newer.revision - 1}
	olderRevisionKey := serverCacheKey{id: id, generation: olderRevision, sql: "SELECT _STALE"}
	cache.sync(id, olderRevision)
	cache.put(olderRevisionKey, oldFinding)
	if got := cache.gens[id]; got != newer {
		t.Fatalf("active generation after older revision = %+v, want newer %+v", got, newer)
	}
	if _, ok := cache.get(olderRevisionKey); ok {
		t.Fatal("older revision read a result after its revision became stale")
	}

	cache.put(newerKey, serverOutcome{})
	if outcome, ok := cache.get(newerKey); !ok || outcome.err != nil {
		t.Fatalf("newer request's accepted result = (%+v, %t), want cached acceptance", outcome, ok)
	}
}

func testServerIdentity(n int) serverIdentity {
	return serverIdentity{
		key:         metadata.Key{Engine: "interbase", URL: "fixture://cache-identity"},
		incarnation: uint64(n),
		variant:     dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
	}
}

func TestServerCacheDropsGenerationRecordWhenLastEntryIsEvicted(t *testing.T) {
	cache := &serverCache{}
	gen := serverGen{generation: 1, revision: 1}
	idle := testServerIdentity(0)
	cache.sync(idle, gen)
	cache.put(serverCacheKey{id: idle, generation: gen, sql: "SELECT 0"}, serverOutcome{})

	busy := testServerIdentity(1)
	cache.sync(busy, gen)
	for i := 0; i < serverCacheLimit; i++ {
		cache.put(serverCacheKey{id: busy, generation: gen, sql: fmt.Sprintf("SELECT %d", i)}, serverOutcome{})
	}

	if _, ok := cache.gens[idle]; ok {
		t.Fatal("generation record kept for an identity whose last entry was evicted")
	}
	if got := cache.gens[busy]; got != gen {
		t.Fatalf("busy identity generation = %+v, want %+v", got, gen)
	}
	if len(cache.entries) != serverCacheLimit {
		t.Fatalf("entries = %d, want %d", len(cache.entries), serverCacheLimit)
	}
}

func TestServerCacheBoundsGenerationRecordsForIdentitiesWithoutEntries(t *testing.T) {
	cache := &serverCache{}
	gen := serverGen{generation: 1, revision: 1}
	kept := testServerIdentity(0)
	cache.sync(kept, gen)
	cache.put(serverCacheKey{id: kept, generation: gen, sql: "SELECT 1"}, serverOutcome{})

	// Cached-only lookups sync an identity without ever storing an entry.
	for i := 1; i <= 3*serverCacheLimit; i++ {
		cache.sync(testServerIdentity(i), gen)
	}

	if len(cache.gens) > serverCacheLimit+1 {
		t.Fatalf("generation records = %d, want at most %d", len(cache.gens), serverCacheLimit+1)
	}
	if _, ok := cache.get(serverCacheKey{id: kept, generation: gen, sql: "SELECT 1"}); !ok {
		t.Fatal("pruning dropped an identity that still has cached entries")
	}
	latest := testServerIdentity(3 * serverCacheLimit)
	if got, ok := cache.gens[latest]; !ok || got != gen {
		t.Fatalf("most recently synced identity record = (%+v, %t), want %+v", got, ok, gen)
	}
}
