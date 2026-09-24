package metadata_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

// TestOversizedDetailReturnsErrDetailLimitWithoutCaching proves an
// individually oversized Detail is never retained, not even partially: it
// returns ErrDetailLimit every time, and every call is a real reload.
func TestOversizedDetailReturnsErrDetailLimitWithoutCaching(t *testing.T) {
	ctx := context.Background()
	r := metadata.NewRegistry(ctx, metadata.Options{MaxDetailBytes: 8})
	defer r.Close(ctx)
	p := metadatatest.New()
	p.DetailValue = metadata.Detail{Text: "this text is way bigger than eight bytes"}
	key := metadata.Key{Engine: "interbase", URL: "ib://host/detail-oversize"}
	scope := metadata.Scope{Principal: "U"}
	lease, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)

	request := metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: "T"}, Facet: "source", Consistency: metadata.Committed}
	if _, err := lease.Detail(ctx, request); !errors.Is(err, metadata.ErrDetailLimit) {
		t.Fatalf("err = %v, want ErrDetailLimit", err)
	}
	if p.DetailCalls.Load() != 1 {
		t.Fatal("expected exactly one Load attempt")
	}

	if _, err := lease.Detail(ctx, request); !errors.Is(err, metadata.ErrDetailLimit) {
		t.Fatalf("second err = %v, want ErrDetailLimit", err)
	}
	if p.DetailCalls.Load() != 2 {
		t.Fatalf("DetailCalls = %d, want 2 (an oversized detail must never be cached, so a retry reloads)", p.DetailCalls.Load())
	}
}

// TestOldestUnpinnedDetailEvictedByBytesNotCount proves eviction is
// weighted-LRU by retained bytes, not by entry count: the least-recently-
// used entry is evicted once a new one would push total bytes over budget,
// and a still-cached entry survives untouched.
func TestOldestUnpinnedDetailEvictedByBytesNotCount(t *testing.T) {
	ctx := context.Background()
	text := strings.Repeat("x", 10) // exactly 10 bytes per detail
	r := metadata.NewRegistry(ctx, metadata.Options{MaxDetailBytes: 25})
	defer r.Close(ctx)
	p := metadatatest.New()
	p.DetailValue = metadata.Detail{Text: text}
	key := metadata.Key{Engine: "interbase", URL: "ib://host/detail-evict"}
	scope := metadata.Scope{Principal: "U"}
	lease, err := r.Acquire(ctx, key, scope, p.Factory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	waitReadyOrFatal(t, lease)

	reqFor := func(name string) metadata.Request {
		return metadata.Request{Object: catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: name}, Facet: "source", Consistency: metadata.Committed}
	}

	if _, err := lease.Detail(ctx, reqFor("A")); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Detail(ctx, reqFor("B")); err != nil {
		t.Fatal(err)
	}
	if p.DetailCalls.Load() != 2 {
		t.Fatal("expected two initial loads (A then B; both fit: 20 <= 25)")
	}

	// C (10 more bytes) would push total to 30 > 25: the least-recently-used
	// entry (A) must be evicted, not simply "the first one ever inserted" if
	// that were no longer also the least-recently-used one.
	if _, err := lease.Detail(ctx, reqFor("C")); err != nil {
		t.Fatal(err)
	}
	if p.DetailCalls.Load() != 3 {
		t.Fatal("expected a third load for C")
	}

	// B must still be cached: it was touched more recently than A.
	if _, err := lease.Detail(ctx, reqFor("B")); err != nil {
		t.Fatal(err)
	}
	if p.DetailCalls.Load() != 3 {
		t.Fatalf("DetailCalls = %d, want 3 (B must still be cached)", p.DetailCalls.Load())
	}

	// A must have been evicted and therefore reload.
	if _, err := lease.Detail(ctx, reqFor("A")); err != nil {
		t.Fatal(err)
	}
	if p.DetailCalls.Load() != 4 {
		t.Fatalf("DetailCalls = %d, want 4 (A must have been evicted and reloaded)", p.DetailCalls.Load())
	}
}

// buildLargeCache generates a 100-table/2,000-column fixture for the
// allocation benchmarks below.
func buildLargeCache() *catalog.Cache {
	const tables, columnsPerTable = 100, 20
	schemaTables := map[string][]string{"": make([]string, 0, tables)}
	columns := make(map[string][]*catalog.ColumnDesc, tables)
	for ti := 0; ti < tables; ti++ {
		table := fmt.Sprintf("T%d", ti)
		schemaTables[""] = append(schemaTables[""], table)
		cols := make([]*catalog.ColumnDesc, 0, columnsPerTable)
		for ci := 0; ci < columnsPerTable; ci++ {
			cols = append(cols, &catalog.ColumnDesc{ColumnBase: catalog.ColumnBase{Table: table, Name: fmt.Sprintf("C%d", ci)}, Type: "INTEGER"})
		}
		columns[catalog.ColumnDatabaseKey("", table)] = cols
	}
	return &catalog.Cache{SchemaTables: schemaTables, ColumnsWithParent: columns}
}

// BenchmarkAcquireOneConsumerLargeCatalog and
// BenchmarkAcquireTwoConsumersLargeCatalog report allocations for one versus
// two consumers of the same 100-table/2,000-column store settling. Since
// descriptors are loaded once and shared by pointer across every lease of
// one store, the second consumer's marginal cost is expected to be
// dominated by its own lease/subscription bookkeeping, not a second copy of
// the descriptor set; compare the two -benchmem outputs to see that split.
func BenchmarkAcquireOneConsumerLargeCatalog(b *testing.B) {
	benchmarkAcquireConsumers(b, 1)
}

func BenchmarkAcquireTwoConsumersLargeCatalog(b *testing.B) {
	benchmarkAcquireConsumers(b, 2)
}

func benchmarkAcquireConsumers(b *testing.B, consumers int) {
	b.ReportAllocs()
	cache := buildLargeCache()
	for i := 0; i < b.N; i++ {
		ctx := context.Background()
		r := metadata.NewRegistry(ctx, metadata.Options{})
		p := metadatatest.New()
		p.Cache = cache
		key := metadata.Key{Engine: "interbase", URL: fmt.Sprintf("ib://host/bench-%d", i)}
		scope := metadata.Scope{Principal: "U"}
		leases := make([]*metadata.Lease, 0, consumers)
		for c := 0; c < consumers; c++ {
			l, err := r.Acquire(ctx, key, scope, p.Factory)
			if err != nil {
				b.Fatal(err)
			}
			leases = append(leases, l)
		}
		for _, l := range leases {
			if err := metadatatest.WaitReady(ctx, l); err != nil {
				b.Fatal(err)
			}
		}
		for _, l := range leases {
			l.Release()
		}
		if err := r.Close(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
