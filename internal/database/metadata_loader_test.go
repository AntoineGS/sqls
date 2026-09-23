package database

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type metadataPlanTestRepo struct {
	*MockDBRepository
	plan MetadataPlan
}

func (r metadataPlanTestRepo) MetadataPlan() MetadataPlan { return r.plan }

func metadataRepo(plan MetadataPlan) metadataPlanTestRepo {
	return metadataPlanTestRepo{MockDBRepository: &MockDBRepository{}, plan: plan}
}

func waitLoad(t *testing.T, load *MetadataLoad) {
	t.Helper()
	select {
	case <-load.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("stranded load")
	}
}

func TestMetadataLoaderSiblingSurvivesFailure(t *testing.T) {
	loader := NewMetadataLoader()
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	repo := metadataRepo(MetadataPlan{Parallelism: 2, Jobs: []MetadataJob{
		{Kind: MetadataViews, Run: func(context.Context, *DBCache) (MetadataPatch, error) { return MetadataPatch{}, errors.New("injected") }},
		{Kind: MetadataProcedures, Run: func(context.Context, *DBCache) (MetadataPatch, error) {
			return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Procedures: map[string]*ProcedureDesc{"P": {Name: "P"}}}}, Count: 1}, nil
		}},
	}})
	load, err := loader.Start(context.Background(), 1, repo)
	if err != nil {
		t.Fatal(err)
	}
	waitLoad(t, load)
	if _, ok := loader.Cache().Procedure("P"); !ok {
		t.Fatal("sibling lost")
	}
	if got := loader.Snapshot().Status[MetadataViews].State; got != MetadataFailed {
		t.Fatalf("views state = %s", got)
	}
	if loader.Snapshot().Cache.Metadata[MetadataProcedures] != MetadataReady {
		t.Fatal("ready cache metadata not published")
	}
}

func TestMetadataLoaderContainsPanicsAndBlocksDependents(t *testing.T) {
	loader := NewMetadataLoader()
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	repo := metadataRepo(MetadataPlan{Parallelism: 3, Jobs: []MetadataJob{
		{Kind: MetadataViews, Run: func(context.Context, *DBCache) (MetadataPatch, error) { panic("boom") }},
		{Kind: MetadataProcedures, DependsOn: []MetadataKind{MetadataViews}, Run: func(context.Context, *DBCache) (MetadataPatch, error) {
			t.Error("dependent ran")
			return MetadataPatch{}, nil
		}},
		{Kind: MetadataDomains, Run: func(context.Context, *DBCache) (MetadataPatch, error) { return MetadataPatch{}, errors.New("failure") }},
	}})
	load, err := loader.Start(context.Background(), 1, repo)
	if err != nil {
		t.Fatal(err)
	}
	waitLoad(t, load)
	s := loader.Snapshot()
	if s.Status[MetadataViews].State != MetadataFailed || s.Status[MetadataProcedures].State != MetadataBlocked {
		t.Fatalf("statuses: %+v", s.Status)
	}
	if s.Status[MetadataDomains].State != MetadataFailed {
		t.Fatal("independent failure hidden")
	}
}

func TestMetadataLoaderRejectsInvalidPlansBeforeRunning(t *testing.T) {
	tests := []struct {
		name string
		plan MetadataPlan
	}{
		{"parallelism zero", MetadataPlan{Jobs: []MetadataJob{{Kind: MetadataViews, Run: func(context.Context, *DBCache) (MetadataPatch, error) { return MetadataPatch{}, nil }}}}},
		{"parallelism high", MetadataPlan{Parallelism: 4}},
		{"duplicate", MetadataPlan{Parallelism: 1, Jobs: []MetadataJob{{Kind: MetadataViews, Run: func(context.Context, *DBCache) (MetadataPatch, error) { return MetadataPatch{}, nil }}, {Kind: MetadataViews, Run: func(context.Context, *DBCache) (MetadataPatch, error) { return MetadataPatch{}, nil }}}}},
		{"unknown", MetadataPlan{Parallelism: 1, Jobs: []MetadataJob{{Kind: "unknown", Run: func(context.Context, *DBCache) (MetadataPatch, error) { return MetadataPatch{}, nil }}}}},
		{"missing dependency", MetadataPlan{Parallelism: 1, Jobs: []MetadataJob{{Kind: MetadataViews, DependsOn: []MetadataKind{MetadataProcedures}, Run: func(context.Context, *DBCache) (MetadataPatch, error) { return MetadataPatch{}, nil }}}}},
		{"cycle", MetadataPlan{Parallelism: 1, Jobs: []MetadataJob{{Kind: MetadataViews, DependsOn: []MetadataKind{MetadataProcedures}, Run: func(context.Context, *DBCache) (MetadataPatch, error) { return MetadataPatch{}, nil }}, {Kind: MetadataProcedures, DependsOn: []MetadataKind{MetadataViews}, Run: func(context.Context, *DBCache) (MetadataPatch, error) { return MetadataPatch{}, nil }}}}},
		{"nil Run", MetadataPlan{Parallelism: 1, Jobs: []MetadataJob{{Kind: MetadataViews}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loader := NewMetadataLoader()
			t.Cleanup(loader.Stop)
			loader.Reset(1)
			var runs atomic.Int32
			for i := range tt.plan.Jobs {
				if tt.plan.Jobs[i].Run != nil {
					original := tt.plan.Jobs[i].Run
					tt.plan.Jobs[i].Run = func(ctx context.Context, c *DBCache) (MetadataPatch, error) { runs.Add(1); return original(ctx, c) }
				}
			}
			_, err := loader.Start(context.Background(), 1, metadataRepo(tt.plan))
			if !errors.Is(err, ErrInvalidMetadataPlan) {
				t.Fatalf("Start error = %v", err)
			}
			if runs.Load() != 0 {
				t.Fatal("invalid plan ran a job")
			}
		})
	}
}

func TestMetadataLoaderResetKeepsGlobalNativeLimit(t *testing.T) {
	loader := NewMetadataLoader()
	t.Cleanup(loader.Stop)
	gate := make(chan struct{})
	var release sync.Once
	var active, peak atomic.Int32
	plan := func(kinds ...MetadataKind) MetadataPlan {
		jobs := make([]MetadataJob, 0, len(kinds))
		for _, kind := range kinds {
			kind := kind
			jobs = append(jobs, MetadataJob{Kind: kind, Run: func(context.Context, *DBCache) (MetadataPatch, error) {
				n := active.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				<-gate
				active.Add(-1)
				return MetadataPatch{}, nil
			}})
		}
		return MetadataPlan{Parallelism: 3, Jobs: jobs}
	}
	loader.Reset(1)
	old, err := loader.Start(context.Background(), 1, metadataRepo(plan(MetadataViews, MetadataProcedures, MetadataDomains)))
	if err != nil {
		t.Fatal(err)
	}
	waitStarted := func(want int32) {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for active.Load() < want {
			select {
			case <-deadline:
				t.Fatalf("only %d jobs started", active.Load())
			default:
				runtime.Gosched()
			}
		}
	}
	waitStarted(3)
	loader.Reset(2)
	waitLoad(t, old)
	newLoad, err := loader.Start(context.Background(), 2, metadataRepo(plan(MetadataViews)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release.Do(func() { close(gate) }) })
	if active.Load() != 3 {
		t.Fatalf("replacement started while old native calls remained: active=%d", active.Load())
	}
	release.Do(func() { close(gate) })
	waitLoad(t, newLoad)
	if err := loader.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got > 3 {
		t.Fatalf("global active peak = %d", got)
	}
	if _, ok := loader.Cache().Procedure("P"); ok {
		t.Fatal("unexpected stale publication")
	}
}

func TestMetadataLoaderWaitTracksDrainAndContext(t *testing.T) {
	loader := NewMetadataLoader()
	gate := make(chan struct{})
	started := make(chan struct{})
	loader.Reset(1)
	load, err := loader.Start(context.Background(), 1, metadataRepo(MetadataPlan{Parallelism: 1, Jobs: []MetadataJob{{Kind: MetadataViews, Run: func(context.Context, *DBCache) (MetadataPatch, error) {
		close(started)
		<-gate
		return MetadataPatch{}, nil
	}}}}))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	loader.Stop()
	waitLoad(t, load)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := loader.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v", err)
	}
	close(gate)
	if err := loader.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataLoaderCancellationSettlesBeforeRunDrains(t *testing.T) {
	loader := NewMetadataLoader()
	gate := make(chan struct{})
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	loader.Reset(1)
	load, err := loader.Start(ctx, 1, metadataRepo(MetadataPlan{Parallelism: 1, Jobs: []MetadataJob{{Kind: MetadataViews, Run: func(context.Context, *DBCache) (MetadataPatch, error) {
		close(started)
		<-gate
		return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Views: map[string]*ViewDesc{"late": {Name: "late"}}}}}, nil
	}}}}))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	waitLoad(t, load)
	if got := loader.Snapshot().Status[MetadataViews].State; got != MetadataCancelled {
		t.Fatalf("state = %s", got)
	}
	waitCtx, stop := context.WithCancel(context.Background())
	stop()
	if err := loader.Wait(waitCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v", err)
	}
	close(gate)
	if err := loader.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := loader.Cache().View("late"); ok {
		t.Fatal("cancelled result published")
	}
	loader.Stop()
}

func TestMetadataLoaderStatusOnlyChangesRetainCachePointer(t *testing.T) {
	loader := NewMetadataLoader()
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	changed := make(chan struct{}, 8)
	loader.SetChangedCallback(func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	gate := make(chan struct{})
	started := make(chan struct{})
	load, err := loader.Start(context.Background(), 1, metadataRepo(MetadataPlan{Parallelism: 1, Jobs: []MetadataJob{
		{Kind: MetadataViews, Run: func(context.Context, *DBCache) (MetadataPatch, error) {
			close(started)
			<-gate
			return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Views: map[string]*ViewDesc{}}}}, nil
		}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cache := loader.Cache()
	if loader.Snapshot().Cache != cache {
		t.Fatal("loading status replaced cache pointer")
	}
	close(gate)
	waitLoad(t, load)
	if loader.Snapshot().Cache == cache {
		t.Fatal("ready publication did not replace cache")
	}
	if loader.Snapshot().Cache.Metadata[MetadataViews] != MetadataReady {
		t.Fatal("cache readiness missing")
	}
}

func TestMetadataLoaderGenerationAndStartErrors(t *testing.T) {
	loader := NewMetadataLoader()
	t.Cleanup(loader.Stop)
	if _, err := loader.Start(context.Background(), 0, metadataRepo(MetadataPlan{})); !errors.Is(err, ErrMetadataStaleGeneration) {
		t.Fatalf("initial Start error = %v", err)
	}
	loader.Reset(2)
	loader.Reset(1)
	load, err := loader.Start(context.Background(), 2, metadataRepo(MetadataPlan{Parallelism: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loader.Start(context.Background(), 2, metadataRepo(MetadataPlan{Parallelism: 1})); !errors.Is(err, ErrMetadataAlreadyStarted) {
		t.Fatalf("duplicate Start error = %v", err)
	}
	waitLoad(t, load)
	loader.Stop()
	if _, err := loader.Start(context.Background(), 2, metadataRepo(MetadataPlan{Parallelism: 1})); !errors.Is(err, ErrMetadataStopped) {
		t.Fatalf("stopped Start error = %v", err)
	}
}

func TestMetadataLoaderAbsentPlan(t *testing.T) {
	loader := NewMetadataLoader()
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	if _, err := loader.Start(context.Background(), 1, NewMockDBRepository(nil)); !errors.Is(err, ErrInvalidMetadataPlan) {
		t.Fatalf("Start error = %v", err)
	}
}
