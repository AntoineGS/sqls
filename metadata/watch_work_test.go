package metadata

import (
	"context"
	"errors"
	"github.com/sqls-server/sqls/catalog"
	"testing"
	"time"
)

type timedWatchProvider struct {
	*controlledWatchProvider
	mapping bool
}

func (p *timedWatchProvider) ReadFingerprint(ctx context.Context, n int64) (Fingerprint, WatchMetrics, error) {
	f, m, e := p.controlledWatchProvider.ReadFingerprint(ctx, n)
	m.Duration = time.Second
	m.DurationKnown = true
	return f, m, e
}
func (p *timedWatchProvider) ReadFull(ctx context.Context) (*catalog.Cache, map[catalog.MetadataKind]catalog.MetadataStatus, WatchMetrics, error) {
	c, s, m, e := p.controlledWatchProvider.ReadFull(ctx)
	m.Queries = 5
	m.QueriesKnown = true
	m.Duration = 2 * time.Second
	m.DurationKnown = true
	return c, s, m, e
}
func (p *timedWatchProvider) ReadChanges(ctx context.Context, c *catalog.Cache, a, b Fingerprint) (ObjectDelta, error) {
	p.mu.Lock()
	mapping := p.mapping
	p.mu.Unlock()
	if mapping {
		return ObjectDelta{FullReason: "mapping", Metrics: WatchMetrics{Queries: 3, QueriesKnown: true, Duration: time.Second, DurationKnown: true}}, nil
	}
	return p.controlledWatchProvider.ReadChanges(ctx, c, a, b)
}
func TestMaintainKnownFailureWork(t *testing.T) {
	p := &timedWatchProvider{controlledWatchProvider: newControlledWatch()}
	r := NewRegistry(context.Background(), Options{})
	defer r.Close(context.Background())
	l, e := r.Acquire(context.Background(), Key{Engine: "test", URL: t.Name()}, Scope{Principal: "P"}, func(context.Context) (Provider, error) { return p, nil })
	if e != nil {
		t.Fatal(e)
	}
	defer l.Release()
	if e = l.track.loader.Wait(context.Background()); e != nil {
		t.Fatal(e)
	}
	maintainWatch(t, l, watchedCheck)
	p.mu.Lock()
	p.readErr = errors.New("read failed")
	p.mu.Unlock()
	before := l.Snapshot()
	got, e := l.Maintain(context.Background(), watchedFull)
	if e == nil || got.FailureOrigin != MaintenanceFailureReplacement || !got.ScanComplete || !got.Scan.DurationKnown || !got.Read.DurationKnown || !got.Read.QueriesKnown || got.Read.Queries != 5 || got.Read.Duration != 2*time.Second || l.Snapshot() != before {
		t.Fatal(got, e)
	}
	p.mu.Lock()
	p.fingerprint.value++
	p.mu.Unlock()
	got, e = l.Maintain(context.Background(), watchedCheck)
	if e == nil || got.Read.QueriesKnown || got.Read.DurationKnown {
		t.Fatal("failed changes falsely measured", got, e)
	}
	p.mu.Lock()
	p.mapping = true
	p.mu.Unlock()
	got, e = l.Maintain(context.Background(), watchedCheck)
	if e == nil || got.FullReason != "mapping" || got.Read.Queries != 8 || got.Read.Duration != 3*time.Second || !got.Read.QueriesKnown || !got.Read.DurationKnown || l.Snapshot() != before {
		t.Fatal("mapping+failed full accounting lost", got, e)
	}
	p.mu.Lock()
	p.scanErr = errors.New("scan failed")
	p.mu.Unlock()
	got, e = l.Maintain(context.Background(), watchedCheck)
	if e == nil || got.ScanComplete || !got.Scan.DurationKnown || got.Scan.Duration != time.Second {
		t.Fatal(got, e)
	}
}
