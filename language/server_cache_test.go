package language

import (
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
