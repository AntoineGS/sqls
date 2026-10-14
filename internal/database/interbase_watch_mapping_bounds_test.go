package database

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
)

// Err checks, rather than sleeps, cancel during CPU-only mapping. The old
// exported path checked only at entry and at the subsequent scoped read.
type mappingCancelContext struct {
	context.Context
	checks int
}

func (c *mappingCancelContext) Err() error {
	c.checks++
	if c.checks >= 3 {
		return context.Canceled
	}
	return nil
}

func TestWatchedDeltaCancellationDuringMapping(t *testing.T) {
	rows := make([]watchedMappingRow, 10000)
	for i := range rows {
		rows[i] = watchedMappingRow{Family: "RDB$FIELDS", Values: map[string]any{"RDB$FIELD_NAME": fmt.Sprintf("D%d", i)}}
	}
	f := watchedMappingFingerprint(t, rows...)
	ctx := &mappingCancelContext{Context: context.Background()}
	delta, err := (&InterBaseDBRepository{}).ReadWatchedDelta(ctx, metadata.NewCache(), f, f)
	if !errors.Is(err, context.Canceled) || delta.FullReason != "" || len(delta.Owners) != 0 || ctx.checks != 3 {
		t.Fatalf("mapping did not stop at cancellation: checks=%d reason=%q err=%v", ctx.checks, delta.FullReason, err)
	}
}

type mappingSpanContext struct {
	context.Context
	first, last time.Time
}

func (c *mappingSpanContext) Err() error {
	c.last = time.Now()
	if c.first.IsZero() {
		c.first = c.last
	}
	return nil
}

func TestWatchedDeltaDurationIncludesMappingFallback(t *testing.T) {
	f := watchedMappingFingerprint(t, watchedMappingRow{Family: "RDB$FIELDS", Values: map[string]any{"RDB$FIELD_NAME": "D"}})
	ctx := &mappingSpanContext{Context: context.Background()}
	delta, err := (&InterBaseDBRepository{}).ReadWatchedDelta(ctx, metadata.NewCache(), f, f)
	if err != nil || delta.FullReason != "mapping" || !delta.Metrics.DurationKnown || delta.Metrics.Duration <= 0 || delta.Metrics.Duration < ctx.last.Sub(ctx.first) {
		t.Fatalf("mapping span omitted: duration=%v known=%t measured=%v err=%v", delta.Metrics.Duration, delta.Metrics.DurationKnown, ctx.last.Sub(ctx.first), err)
	}
}

func TestWatchedDeltaDurationIncludesDependencyOnlyMapping(t *testing.T) {
	base := metadata.NewCache()
	base.Catalog = &catalog.CatalogCache{}
	base.Dependencies = catalog.NewDependencyGraph(nil, nil)
	base.PrimaryKeysByID = map[string][]string{}
	base.Metadata[catalog.MetadataDependencies] = catalog.MetadataReady
	previous := watchedMappingFingerprint(t)
	current := watchedMappingFingerprint(t, watchedMappingRow{Family: "RDB$DEPENDENCIES", Values: map[string]any{"RDB$DEPENDENT_NAME": "P", "RDB$DEPENDENT_TYPE": int64(5), "RDB$DEPENDED_ON_NAME": "T", "RDB$DEPENDED_ON_TYPE": int64(0)}})
	ctx := &mappingSpanContext{Context: context.Background()}
	delta, err := (&InterBaseDBRepository{}).ReadWatchedDelta(ctx, base, previous, current)
	if err != nil || delta.FullReason != "" || !delta.Metrics.DurationKnown || !delta.Metrics.QueriesKnown || delta.Metrics.Queries != 0 || delta.Metrics.Duration <= 0 || delta.Metrics.Duration < ctx.last.Sub(ctx.first) || len(delta.DependencyOwners) != 1 || len(delta.DependencyEdges) != 1 {
		t.Fatalf("dependency-only mapping span omitted: delta=%+v measured=%v err=%v", delta, ctx.last.Sub(ctx.first), err)
	}
}

func TestWatchedMappingTransientBudgetAtReferenceScale(t *testing.T) {
	// A larger, header-heavy catalog must still hit the shared transient budget.
	// One description change must not reconstruct unbounded dual indexes.
	const ordinary = 230000
	rows := make([]watchedMappingRow, 0, ordinary+140000)
	for i := 0; i < ordinary; i++ {
		rows = append(rows, watchedMappingRow{Family: "RDB$FIELDS", Values: map[string]any{"RDB$FIELD_NAME": fmt.Sprintf("D%06d", i)}})
	}
	for i := 0; i < 140000; i++ {
		rows = append(rows, watchedMappingRow{Family: "RDB$DEPENDENCIES", Values: map[string]any{"RDB$DEPENDENT_NAME": fmt.Sprintf("P%06d", i), "RDB$DEPENDENT_TYPE": int64(5), "RDB$DEPENDED_ON_NAME": "D000000", "RDB$DEPENDED_ON_TYPE": int64(9)}})
	}
	f := watchedMappingFingerprint(t, rows...)
	rows[0].Values["RDB$DESCRIPTION"] = "changed"
	candidate := watchedMappingFingerprint(t, rows...)
	work := &watchedMappingWork{ctx: context.Background()}
	first := watchedRoutingFor(f, work)
	routing := watchedRoutingFor(candidate, work)
	if !work.exhausted || work.err != nil || work.bytes > watchedMappingBudget || len(first.headers) != ordinary || len(routing.headers) >= ordinary {
		t.Fatalf("index construction exceeded budget: bytes=%d headers=%d exhausted=%t err=%v", work.bytes, len(routing.headers), work.exhausted, work.err)
	}
	t.Logf("budget fallback: charged=%d limit=%d retained first headers=%d second headers=%d of %d", work.bytes, watchedMappingBudget, len(first.headers), len(routing.headers), ordinary)
	got, err := mapWatchedChanges(context.Background(), metadata.NewCache(), f, candidate)
	if err != nil || got.FullReason != "mapping" || len(got.Owners) != 0 {
		t.Fatalf("unbounded routing indexes: reason=%q owners=%d err=%v", got.FullReason, len(got.Owners), err)
	}
}

func TestWatchedMappingSmallChangeAtTargetScale(t *testing.T) {
	// 190k ordinary + 140k dependency records. Relations have 30 columns,
	// five 12-segment indexes and three triggers; procedures have 20 parameters.
	// Include support/child families, not a catalog made entirely of headers.
	rows := make([]watchedMappingRow, 0, 330000)
	add := func(family string, values map[string]any) {
		rows = append(rows, watchedMappingRow{Family: "RDB$" + family, Values: values})
	}
	for i := 0; i < 1000; i++ {
		add("RELATIONS", map[string]any{"RDB$RELATION_NAME": fmt.Sprintf("T%04d", i)})
	}
	for i := 0; i < 30000; i++ {
		add("FIELDS", map[string]any{"RDB$FIELD_NAME": fmt.Sprintf("D%05d", i), "RDB$FIELD_TYPE": int64(8)})
		add("RELATION_FIELDS", map[string]any{"RDB$RELATION_NAME": fmt.Sprintf("T%04d", i/30), "RDB$FIELD_NAME": fmt.Sprintf("C%02d", i%30), "RDB$FIELD_SOURCE": fmt.Sprintf("D%05d", i), "RDB$FIELD_POSITION": int64(i % 30)})
	}
	for i := 0; i < 5000; i++ {
		add("INDICES", map[string]any{"RDB$INDEX_NAME": fmt.Sprintf("I%04d", i), "RDB$RELATION_NAME": fmt.Sprintf("T%04d", i/5), "RDB$SEGMENT_COUNT": int64(12)})
		for j := 0; j < 12; j++ {
			add("INDEX_SEGMENTS", map[string]any{"RDB$INDEX_NAME": fmt.Sprintf("I%04d", i), "RDB$FIELD_NAME": fmt.Sprintf("C%02d", j), "RDB$FIELD_POSITION": int64(j)})
		}
	}
	changed := len(rows)
	for i := 0; i < 1000; i++ {
		add("PROCEDURES", map[string]any{"RDB$PROCEDURE_NAME": fmt.Sprintf("P%04d", i), "RDB$PROCEDURE_INPUTS": int64(20), "RDB$PROCEDURE_OUTPUTS": int64(0)})
		for j := 0; j < 20; j++ {
			add("PROCEDURE_PARAMETERS", map[string]any{"RDB$PROCEDURE_NAME": fmt.Sprintf("P%04d", i), "RDB$PARAMETER_NAME": fmt.Sprintf("A%02d", j), "RDB$PARAMETER_TYPE": int64(0), "RDB$PARAMETER_NUMBER": int64(j), "RDB$FIELD_SOURCE": fmt.Sprintf("D%05d", i*20+j)})
		}
	}
	for i := 0; i < 3000; i++ {
		add("TRIGGERS", map[string]any{"RDB$TRIGGER_NAME": fmt.Sprintf("G%04d", i), "RDB$RELATION_NAME": fmt.Sprintf("T%04d", i/3)})
	}
	for i := 0; i < 20000; i++ {
		add("TRIGGER_MESSAGES", map[string]any{"RDB$TRIGGER_NAME": fmt.Sprintf("G%04d", i%3000), "RDB$MESSAGE_NUMBER": int64(i / 3000), "RDB$MESSAGE": "fixture"})
		// Four dimensions for 5k array domains; routing remains domain ownership.
		add("FIELD_DIMENSIONS", map[string]any{"RDB$FIELD_NAME": fmt.Sprintf("D%05d", i/4), "RDB$DIMENSION": int64(i % 4), "RDB$LOWER_BOUND": int64(1), "RDB$UPPER_BOUND": int64(10)})
	}
	for i := 0; i < 140000; i++ {
		add("DEPENDENCIES", map[string]any{"RDB$DEPENDENT_NAME": fmt.Sprintf("P%04d", i/140), "RDB$DEPENDENT_TYPE": int64(5), "RDB$DEPENDED_ON_NAME": fmt.Sprintf("T%04d", i%140), "RDB$DEPENDED_ON_TYPE": int64(0)})
	}
	previous := watchedMappingFingerprint(t, rows...)
	rows[changed].Values["RDB$DESCRIPTION"] = "single procedure description change"
	current := watchedMappingFingerprint(t, rows...)
	if previous.RecordCount() != 330000 || current.RecordCount() != 330000 {
		t.Fatalf("scale fixture lost identities: previous=%d current=%d", previous.RecordCount(), current.RecordCount())
	}
	rows = nil
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	// Test-only starting credit lets the real routing builder finish without
	// changing its charges or the production constant. Count both indexes as
	// one attempt; selection charges can only increase this measured lower bound.
	const measurementAllowance = int64(1 << 30)
	initial := int64(watchedMappingBudget) - measurementAllowance
	work := &watchedMappingWork{ctx: context.Background(), bytes: initial}
	old := watchedRoutingFor(previous, work)
	new := watchedRoutingFor(current, work)
	if work.exhausted || work.err != nil {
		t.Fatalf("test measurement allowance exhausted: charged=%d err=%v", work.bytes-initial, work.err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(old)
	runtime.KeepAlive(new)
	charged := work.bytes - initial
	minimumCap := ((charged*3/2 + (32 << 20) - 1) / (32 << 20)) * (32 << 20)
	t.Logf("records=330000 relation-fields=30000 dependencies=140000 routing-charge=%d bytes (%.2f MiB); minimum 1.5x rounded cap=%d bytes (%.0f MiB); retained routing heap delta=%d bytes (%.2f MiB), before=%d after=%d (observational, not asserted)", charged, float64(charged)/(1<<20), minimumCap, float64(minimumCap)/(1<<20), int64(after.HeapAlloc)-int64(before.HeapAlloc), float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/(1<<20), before.HeapAlloc, after.HeapAlloc)
	got, err := mapWatchedChanges(context.Background(), metadata.NewCache(), previous, current)
	want := []catalog.ObjectID{{Kind: catalog.ObjectKindProcedure, Name: "P0000"}}
	if err != nil || got.FullReason != "" || got.Affected != 1 || len(got.Owners) != 1 || !reflect.DeepEqual(got.Owners[catalog.MetadataProcedures], want) {
		t.Fatalf("small change lost incremental routing: reason=%q affected=%d owners=%v err=%v", got.FullReason, got.Affected, got.Owners, err)
	}
}
