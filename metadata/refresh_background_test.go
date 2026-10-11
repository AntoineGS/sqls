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

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

// phasedProvider answers each relations load with table T<call>. A gate or
// failure registered for a call number holds or fails that load.
type phasedProvider struct {
	mu                 sync.Mutex
	calls              int
	gates              map[int]chan struct{}
	fail               map[int]error
	entered            chan int
	running            atomic.Int64
	closes             atomic.Int64
	closedWhileRunning atomic.Bool
}

func newPhasedProvider() *phasedProvider {
	return &phasedProvider{gates: map[int]chan struct{}{}, fail: map[int]error{}, entered: make(chan int, 16)}
}

func (p *phasedProvider) Plan() catalog.MetadataPlan {
	return catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{Kind: catalog.MetadataRelations, Run: p.run}}}
}

func (p *phasedProvider) run(ctx context.Context, _ *catalog.Cache) (catalog.MetadataPatch, error) {
	p.mu.Lock()
	p.calls++
	call, gate, failure := p.calls, p.gates[p.calls], p.fail[p.calls]
	p.mu.Unlock()
	p.running.Add(1)
	defer p.running.Add(-1)
	p.entered <- call
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return catalog.MetadataPatch{}, ctx.Err()
		}
	}
	if failure != nil {
		return catalog.MetadataPatch{}, failure
	}
	return catalog.MetadataPatch{Cache: &catalog.Cache{SchemaTables: map[string][]string{"": {fmt.Sprintf("T%d", call)}}}, Count: 1}, nil
}

func (p *phasedProvider) DetailPlan(metadata.Request) (metadata.DetailPlan, error) {
	return metadata.DetailPlan{}, metadata.ErrUnsupported
}

func (p *phasedProvider) Close() error {
	if p.running.Load() > 0 {
		p.closedWhileRunning.Store(true)
	}
	p.closes.Add(1)
	return nil
}

func acquirePhased(t *testing.T, p *phasedProvider) (*metadata.Registry, *metadata.Lease) {
	t.Helper()
	reg := metadata.NewRegistry(context.Background(), metadata.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, err := reg.Acquire(ctx, metadata.Key{Engine: "test", URL: "phased"}, metadata.Scope{Principal: "P"},
		func(context.Context) (metadata.Provider, error) { return p, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := metadatatest.WaitReady(ctx, lease); err != nil {
		t.Fatal(err)
	}
	<-p.entered
	return reg, lease
}

func phasedTables(s *catalog.MetadataSnapshot) []string { return s.Cache.SchemaTables[""] }

func TestRefreshInBackgroundKeepsPublishedSnapshotUntilSwap(t *testing.T) {
	p := newPhasedProvider()
	gate := make(chan struct{})
	p.gates[2] = gate
	reg, lease := acquirePhased(t, p)
	defer func() { lease.Release(); _ = reg.Close(context.Background()) }()
	done := make(chan error, 1)
	go func() { done <- lease.RefreshInBackground(context.Background()) }()
	if call := <-p.entered; call != 2 {
		t.Fatalf("entered call %d, want 2", call)
	}
	during := lease.Snapshot()
	if during.Generation != 1 || !during.Settled() || !reflect.DeepEqual(phasedTables(during), []string{"T1"}) {
		t.Fatalf("snapshot during refresh = gen %d settled %v tables %v", during.Generation, during.Settled(), phasedTables(during))
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after := lease.Snapshot()
	if after.Generation != 2 || after.Stale || !reflect.DeepEqual(phasedTables(after), []string{"T2"}) {
		t.Fatalf("snapshot after refresh = gen %d stale %v tables %v", after.Generation, after.Stale, phasedTables(after))
	}
}

func TestRefreshInBackgroundFailureKeepsPublishedSnapshot(t *testing.T) {
	p := newPhasedProvider()
	p.fail[2] = errors.New("database unreachable")
	reg, lease := acquirePhased(t, p)
	defer func() { lease.Release(); _ = reg.Close(context.Background()) }()
	err := lease.RefreshInBackground(context.Background())
	if !errors.Is(err, metadata.ErrCategoryFailed) || !strings.Contains(err.Error(), "database unreachable") {
		t.Fatalf("err = %v", err)
	}
	after := lease.Snapshot()
	if after.Generation != 1 || after.Stale || !reflect.DeepEqual(phasedTables(after), []string{"T1"}) {
		t.Fatalf("snapshot after failed refresh = gen %d stale %v tables %v", after.Generation, after.Stale, phasedTables(after))
	}
}

func TestRegistryCloseDrainsBackgroundRefreshBeforeClosingProvider(t *testing.T) {
	p := newPhasedProvider()
	p.gates[2] = make(chan struct{}) // never released
	reg, lease := acquirePhased(t, p)
	done := make(chan error, 1)
	go func() { done <- lease.RefreshInBackground(context.Background()) }()
	<-p.entered
	if err := reg.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("refresh succeeded on a closed registry")
	}
	if p.closes.Load() != 1 || p.closedWhileRunning.Load() {
		t.Fatalf("closes = %d, closed while a load ran = %v", p.closes.Load(), p.closedWhileRunning.Load())
	}
}
