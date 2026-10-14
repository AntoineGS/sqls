package metadata

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
)

type aggregateWatchProvider struct {
	*controlledWatchProvider
	mapping, fullMetrics WatchMetrics
	beforeMappingReturn  func()
}

func (p *aggregateWatchProvider) ReadChanges(context.Context, *catalog.Cache, Fingerprint, Fingerprint) (ObjectDelta, error) {
	p.partials.Add(1)
	if p.beforeMappingReturn != nil {
		p.beforeMappingReturn()
	}
	return ObjectDelta{FullReason: "mapping", Metrics: p.mapping}, nil
}

func (p *aggregateWatchProvider) ReadFull(ctx context.Context) (*catalog.Cache, map[catalog.MetadataKind]catalog.MetadataStatus, WatchMetrics, error) {
	c, s, _, err := p.controlledWatchProvider.ReadFull(ctx)
	return c, s, p.fullMetrics, err
}

func acquireAggregateWatch(t *testing.T, p *aggregateWatchProvider) *Lease {
	t.Helper()
	r := NewRegistry(context.Background(), Options{})
	l, err := r.Acquire(context.Background(), Key{Engine: "test", URL: t.Name()}, Scope{Principal: "P"}, func(context.Context) (Provider, error) { return p, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Release(); _ = r.Close(context.Background()) })
	if err = l.track.loader.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	maintainWatch(t, l, watchedCheck)
	p.mu.Lock()
	p.fingerprint.value++
	p.mu.Unlock()
	return l
}

func TestMaintainAggregateAvailability(t *testing.T) {
	known := WatchMetrics{Queries: 7, QueriesKnown: true, Duration: 2 * time.Second, DurationKnown: true}
	mappingKnown := WatchMetrics{Queries: 3, QueriesKnown: true, Duration: time.Second, DurationKnown: true}
	for _, tc := range []struct {
		name                        string
		mapping, full               WatchMetrics
		standalone, fail            bool
		queriesKnown, durationKnown bool
	}{
		{"unknown-mapping-known-full", WatchMetrics{}, known, false, false, false, false},
		{"standalone-known-full", WatchMetrics{}, known, true, false, true, true},
		{"both-known", mappingKnown, known, false, false, true, true},
		{"mapping-duration-unknown", WatchMetrics{Queries: 3, QueriesKnown: true}, known, false, false, true, false},
		{"mapping-queries-unknown", WatchMetrics{Duration: time.Second, DurationKnown: true}, known, false, false, false, true},
		{"full-duration-unknown", mappingKnown, WatchMetrics{Queries: 7, QueriesKnown: true}, false, false, true, false},
		{"full-queries-unknown", mappingKnown, WatchMetrics{Duration: 2 * time.Second, DurationKnown: true}, false, false, false, true},
		{"both-unknown-zero", WatchMetrics{}, WatchMetrics{}, false, false, false, false},
		{"both-measured-zero", WatchMetrics{QueriesKnown: true, DurationKnown: true}, WatchMetrics{QueriesKnown: true, DurationKnown: true}, false, false, true, true},
		{"known-mapping-failed-full", mappingKnown, known, false, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &aggregateWatchProvider{controlledWatchProvider: newControlledWatch(), mapping: tc.mapping, fullMetrics: tc.full}
			l := acquireAggregateWatch(t, p)
			before := l.Snapshot()
			req := watchedCheck
			if tc.standalone {
				req = watchedFull
			}
			if tc.fail {
				p.mu.Lock()
				p.readErr = errors.New("full failed")
				p.mu.Unlock()
			}
			got, err := l.Maintain(context.Background(), req)
			wantQueries, wantDuration := tc.full.Queries, tc.full.Duration
			if !tc.standalone {
				wantQueries += tc.mapping.Queries
				wantDuration += tc.mapping.Duration
			}
			if (err != nil) != tc.fail || got.Read.Queries != wantQueries || got.Read.Duration != wantDuration || got.Read.QueriesKnown != tc.queriesKnown || got.Read.DurationKnown != tc.durationKnown {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			if tc.fail {
				if l.Snapshot() != before || got.FailureOrigin != MaintenanceFailureReplacement {
					t.Fatal("failed full published", got)
				}
			} else if got.Outcome != "full" || got.Generation != before.Generation+1 {
				t.Fatal("successful full not published", got)
			}
		})
	}
}

func TestMaintainAggregateUnenteredFullIsIdentity(t *testing.T) {
	p := &aggregateWatchProvider{controlledWatchProvider: newControlledWatch(), mapping: WatchMetrics{Queries: 3, QueriesKnown: true, Duration: time.Second, DurationKnown: true}}
	l := acquireAggregateWatch(t, p)
	before := l.Snapshot()
	sb := l.track.storeBudget
	returned := make(chan struct{})
	p.beforeMappingReturn = func() { sb.hostWake.mu.Lock(); close(returned) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var got MaintenanceResult
	var err error
	go func() { got, err = l.Maintain(ctx, watchedCheck); close(done) }()
	<-returned
	// The read's error/context join has completed before deferred release removes
	// the host token and blocks on this existing waker mutex. Cancel precisely
	// here so the later Full I/O admission fails, not the measured mapping read.
	deadline := time.Now().Add(5 * time.Second)
	for len(sb.host) != 0 {
		if time.Now().After(deadline) {
			sb.hostWake.mu.Unlock()
			t.Fatal("mapping release did not begin")
		}
		runtime.Gosched()
	}
	cancel()
	sb.hostWake.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not drain")
	}
	if !errors.Is(err, context.Canceled) || got.FullReason != "mapping" || got.FailureOrigin != MaintenanceFailureReplacement || !got.ScanComplete || l.Snapshot() != before || p.fulls.Load() != 1 || got.Read.Queries != 3 || got.Read.Duration != time.Second || !got.Read.QueriesKnown || !got.Read.DurationKnown {
		t.Fatal("unentered Full invalidated known mapping work", got, err, p.fulls.Load())
	}
}
