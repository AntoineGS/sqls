package metadata

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
)

// metadataRepo is a no-op identity: the moved loader.Start already takes a
// catalog.MetadataPlan directly, so tests ported verbatim from the
// repository-based signature keep compiling without editing every call site.
func metadataRepo(plan catalog.MetadataPlan) catalog.MetadataPlan { return plan }

type drainResourceCounts struct {
	mu                     sync.Mutex
	txBegins, txRollbacks  int
	rowsOpened, rowsClosed int
}

type drainDriver struct{ counts *drainResourceCounts }

type drainConnector struct{ driver drainDriver }

type drainConn struct{ counts *drainResourceCounts }

type drainTx struct{ counts *drainResourceCounts }

type drainRows struct{ counts *drainResourceCounts }

func (d drainDriver) Open(string) (driver.Conn, error) { return &drainConn{counts: d.counts}, nil }

func (c drainConnector) Connect(context.Context) (driver.Conn, error) {
	return &drainConn{counts: c.driver.counts}, nil
}

func (c drainConnector) Driver() driver.Driver { return c.driver }

func (c *drainConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected Prepare")
}

func (c *drainConn) Close() error { return nil }

func (c *drainConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *drainConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.counts.mu.Lock()
	c.counts.txBegins++
	c.counts.mu.Unlock()
	return &drainTx{counts: c.counts}, nil
}

func (c *drainConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	c.counts.mu.Lock()
	c.counts.rowsOpened++
	c.counts.mu.Unlock()
	return &drainRows{counts: c.counts}, nil
}

func (tx *drainTx) Commit() error { return nil }

func (tx *drainTx) Rollback() error {
	tx.counts.mu.Lock()
	tx.counts.txRollbacks++
	tx.counts.mu.Unlock()
	return nil
}

func (rows *drainRows) Columns() []string { return []string{"metadata"} }

func (rows *drainRows) Close() error {
	rows.counts.mu.Lock()
	rows.counts.rowsClosed++
	rows.counts.mu.Unlock()
	return nil
}

func (*drainRows) Next([]driver.Value) error { return io.EOF }

func waitLoad(t *testing.T, load *Load) {
	t.Helper()
	select {
	case <-load.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("stranded load")
	}
}

func TestMetadataLoaderSiblingSurvivesFailure(t *testing.T) {
	loader := NewLoader()
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	repo := metadataRepo(catalog.MetadataPlan{Parallelism: 2, Jobs: []catalog.MetadataJob{
		{Kind: catalog.MetadataViews, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{}, errors.New("injected")
		}},
		{Kind: catalog.MetadataProcedures, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Procedures: map[string]*catalog.ProcedureDesc{"P": {Name: "P"}}}}, Count: 1}, nil
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
	if got := loader.Snapshot().Status[catalog.MetadataViews].State; got != catalog.MetadataFailed {
		t.Fatalf("views state = %s", got)
	}
	if loader.Snapshot().Cache.Metadata[catalog.MetadataProcedures] != catalog.MetadataReady {
		t.Fatal("ready cache metadata not published")
	}
}

func TestMetadataLoaderMarkStartFailedIsTerminalAndGenerationFenced(t *testing.T) {
	loader := NewLoader()
	t.Cleanup(loader.Stop)
	loader.Reset(3)
	before := loader.Snapshot()
	if err := loader.MarkStartFailed(2); !errors.Is(err, ErrMetadataStaleGeneration) {
		t.Fatalf("stale MarkStartFailed error = %v, want stale generation", err)
	}
	if err := loader.MarkStartFailed(3); err != nil {
		t.Fatal(err)
	}
	failed := loader.Snapshot()
	if failed.Generation != 3 || failed.Started || !failed.StartFailed || !failed.Settled() || !failed.Degraded() {
		t.Fatalf("start-failed snapshot = %+v (settled=%v degraded=%v)", failed, failed.Settled(), failed.Degraded())
	}
	if failed.Cache != before.Cache {
		t.Fatal("start failure replaced the generation cache")
	}
	for kind, status := range before.Status {
		if failed.Status[kind] != status {
			t.Fatalf("start failure changed status %s: before=%+v after=%+v", kind, status, failed.Status[kind])
		}
	}
	if _, err := loader.Start(context.Background(), 3, metadataRepo(catalog.MetadataPlan{Parallelism: 1})); !errors.Is(err, ErrMetadataAlreadyStarted) {
		t.Fatalf("Start after terminal start failure = %v, want already-started rejection", err)
	}
	loader.Reset(4)
	if next := loader.Snapshot(); next.StartFailed || next.Settled() || next.Degraded() {
		t.Fatalf("new generation inherited start failure: %+v", next)
	}
}

func TestMetadataLoaderContainsPanicsAndBlocksDependents(t *testing.T) {
	loader := NewLoader()
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	repo := metadataRepo(catalog.MetadataPlan{Parallelism: 3, Jobs: []catalog.MetadataJob{
		{Kind: catalog.MetadataViews, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) { panic("boom") }},
		{Kind: catalog.MetadataProcedures, DependsOn: []catalog.MetadataKind{catalog.MetadataViews}, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			t.Error("dependent ran")
			return catalog.MetadataPatch{}, nil
		}},
		{Kind: catalog.MetadataDomains, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{}, errors.New("failure")
		}},
	}})
	load, err := loader.Start(context.Background(), 1, repo)
	if err != nil {
		t.Fatal(err)
	}
	waitLoad(t, load)
	s := loader.Snapshot()
	if s.Status[catalog.MetadataViews].State != catalog.MetadataFailed || s.Status[catalog.MetadataProcedures].State != catalog.MetadataBlocked {
		t.Fatalf("statuses: %+v", s.Status)
	}
	if s.Status[catalog.MetadataDomains].State != catalog.MetadataFailed {
		t.Fatal("independent failure hidden")
	}
}

func TestMetadataLoaderRejectsInvalidPlansBeforeRunning(t *testing.T) {
	tests := []struct {
		name string
		plan catalog.MetadataPlan
	}{
		{"parallelism zero", catalog.MetadataPlan{Jobs: []catalog.MetadataJob{{Kind: catalog.MetadataViews, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{}, nil
		}}}}},
		{"parallelism high", catalog.MetadataPlan{Parallelism: 4}},
		{"duplicate", catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{Kind: catalog.MetadataViews, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{}, nil
		}}, {Kind: catalog.MetadataViews, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{}, nil
		}}}}},
		{"unknown", catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{Kind: "unknown", Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{}, nil
		}}}}},
		{"missing dependency", catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{Kind: catalog.MetadataViews, DependsOn: []catalog.MetadataKind{catalog.MetadataProcedures}, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{}, nil
		}}}}},
		{"cycle", catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{Kind: catalog.MetadataViews, DependsOn: []catalog.MetadataKind{catalog.MetadataProcedures}, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{}, nil
		}}, {Kind: catalog.MetadataProcedures, DependsOn: []catalog.MetadataKind{catalog.MetadataViews}, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{}, nil
		}}}}},
		{"nil Run", catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{Kind: catalog.MetadataViews}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loader := NewLoader()
			t.Cleanup(loader.Stop)
			loader.Reset(1)
			var runs atomic.Int32
			for i := range tt.plan.Jobs {
				if tt.plan.Jobs[i].Run != nil {
					original := tt.plan.Jobs[i].Run
					tt.plan.Jobs[i].Run = func(ctx context.Context, c *catalog.Cache) (catalog.MetadataPatch, error) {
						runs.Add(1)
						return original(ctx, c)
					}
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
	loader := NewLoader()
	t.Cleanup(loader.Stop)
	gate := make(chan struct{})
	var release sync.Once
	var active, peak atomic.Int32
	plan := func(kinds ...catalog.MetadataKind) catalog.MetadataPlan {
		jobs := make([]catalog.MetadataJob, 0, len(kinds))
		for _, kind := range kinds {
			kind := kind
			jobs = append(jobs, catalog.MetadataJob{Kind: kind, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
				n := active.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				<-gate
				active.Add(-1)
				return catalog.MetadataPatch{}, nil
			}})
		}
		return catalog.MetadataPlan{Parallelism: 3, Jobs: jobs}
	}
	loader.Reset(1)
	old, err := loader.Start(context.Background(), 1, metadataRepo(plan(catalog.MetadataViews, catalog.MetadataProcedures, catalog.MetadataDomains)))
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
	newLoad, err := loader.Start(context.Background(), 2, metadataRepo(plan(catalog.MetadataViews)))
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
	loader := NewLoader()
	gate := make(chan struct{})
	started := make(chan struct{})
	loader.Reset(1)
	load, err := loader.Start(context.Background(), 1, metadataRepo(catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{Kind: catalog.MetadataViews, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
		close(started)
		<-gate
		return catalog.MetadataPatch{}, nil
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
	loader := NewLoader()
	gate := make(chan struct{})
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	loader.Reset(1)
	load, err := loader.Start(ctx, 1, metadataRepo(catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{Kind: catalog.MetadataViews, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
		close(started)
		<-gate
		return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Views: map[string]*catalog.ViewDesc{"late": {Name: "late"}}}}}, nil
	}}}}))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	waitLoad(t, load)
	if got := loader.Snapshot().Status[catalog.MetadataViews].State; got != catalog.MetadataCancelled {
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

func TestMetadataLoaderFailureIsIsolatedForEveryJobKind(t *testing.T) {
	kinds := []catalog.MetadataKind{catalog.MetadataSchemas, catalog.MetadataRelations, catalog.MetadataColumnsCurrent, catalog.MetadataColumnsAll, catalog.MetadataPrimaryKeys, catalog.MetadataForeignKeys, catalog.MetadataViews, catalog.MetadataProcedures, catalog.MetadataGenerators, catalog.MetadataDomains, catalog.MetadataFunctions, catalog.MetadataIndexes, catalog.MetadataTriggers}
	for _, failedKind := range kinds {
		t.Run(string(failedKind), func(t *testing.T) {
			loader := NewLoader()
			t.Cleanup(loader.Stop)
			loader.Reset(1)
			jobs := make([]catalog.MetadataJob, 0, len(kinds))
			for _, kind := range kinds {
				kind := kind
				jobs = append(jobs, catalog.MetadataJob{Kind: kind, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
					if kind == failedKind {
						return catalog.MetadataPatch{}, errors.New("injected job failure")
					}
					return catalog.MetadataPatch{Cache: emptyMetadataFragment(kind)}, nil
				}})
			}
			load, err := loader.Start(context.Background(), 1, metadataRepo(catalog.MetadataPlan{Parallelism: 3, Jobs: jobs}))
			if err != nil {
				t.Fatal(err)
			}
			waitLoad(t, load)
			status := loader.Snapshot().Status
			if status[failedKind].State != catalog.MetadataFailed {
				t.Fatalf("failed job status = %+v", status[failedKind])
			}
			for _, kind := range kinds {
				if kind != failedKind && status[kind].State != catalog.MetadataReady {
					t.Fatalf("independent %s status = %+v", kind, status[kind])
				}
			}
		})
	}
}

func emptyMetadataFragment(kind catalog.MetadataKind) *catalog.Cache {
	fragment := &catalog.Cache{}
	switch kind {
	case catalog.MetadataSchemas:
		fragment.Schemas = map[string]string{}
	case catalog.MetadataRelations:
		fragment.SchemaTables = map[string][]string{}
	case catalog.MetadataColumnsCurrent, catalog.MetadataColumnsAll:
		fragment.ColumnsWithParent = map[string][]*catalog.ColumnDesc{}
	case catalog.MetadataPrimaryKeys:
		fragment.PrimaryKeyColumns = map[string]map[string]struct{}{}
	case catalog.MetadataForeignKeys:
		fragment.ForeignKeys = map[string]map[string][]*catalog.ForeignKey{}
	case catalog.MetadataViews:
		fragment.Catalog = &catalog.CatalogCache{Views: map[string]*catalog.ViewDesc{}}
	case catalog.MetadataProcedures:
		fragment.Catalog = &catalog.CatalogCache{Procedures: map[string]*catalog.ProcedureDesc{}}
	case catalog.MetadataGenerators:
		fragment.Catalog = &catalog.CatalogCache{Generators: map[string]*catalog.GeneratorDesc{}}
	case catalog.MetadataDomains:
		fragment.Catalog = &catalog.CatalogCache{Domains: map[string]*catalog.DomainDesc{}}
	case catalog.MetadataFunctions:
		fragment.Catalog = &catalog.CatalogCache{Functions: map[string]*catalog.FunctionDesc{}}
	case catalog.MetadataIndexes:
		fragment.Catalog = &catalog.CatalogCache{Indexes: map[string]*catalog.IndexDesc{}}
	case catalog.MetadataTriggers:
		fragment.Catalog = &catalog.CatalogCache{Triggers: map[string]*catalog.TriggerDesc{}}
	}
	return fragment
}

type observedWaitContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestMetadataLoaderDrainsRepeatedRefreshesAndStopWithPendingWork(t *testing.T) {
	loader := NewLoader()
	resources := &drainResourceCounts{}
	db := sql.OpenDB(drainConnector{driver: drainDriver{counts: resources}})
	db.SetMaxOpenConns(3)
	t.Cleanup(func() { _ = db.Close() })
	var releaseLatestGate func()
	t.Cleanup(func() {
		if releaseLatestGate != nil {
			releaseLatestGate()
		}
		loader.Stop()
		_ = loader.Wait(context.Background())
	})
	var active, peak atomic.Int32
	var last *Load
	var releasePreviousGate func()
	kinds := []catalog.MetadataKind{catalog.MetadataViews, catalog.MetadataProcedures, catalog.MetadataDomains}
	for generation := uint64(1); generation <= 100; generation++ {
		loader.Reset(generation)
		if releasePreviousGate != nil {
			releasePreviousGate() // drain the superseded generation only after its work entered
			releasePreviousGate = nil
			if err := loader.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		gate := make(chan struct{})
		var releaseOnce sync.Once
		releaseGate := func() { releaseOnce.Do(func() { close(gate) }) }
		releaseLatestGate = releaseGate
		entered := make(chan struct{}, len(kinds))
		gen := generation
		plan := catalog.MetadataPlan{Parallelism: len(kinds)}
		for _, kind := range kinds {
			kind := kind
			plan.Jobs = append(plan.Jobs, catalog.MetadataJob{Kind: kind, Run: func(ctx context.Context, _ *catalog.Cache) (catalog.MetadataPatch, error) {
				tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
				if err != nil {
					return catalog.MetadataPatch{}, err
				}
				rows, err := tx.QueryContext(ctx, "select metadata")
				if err != nil {
					_ = tx.Rollback()
					return catalog.MetadataPatch{}, err
				}
				n := active.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				defer active.Add(-1)
				entered <- struct{}{}
				<-gate
				if err := rows.Close(); err != nil {
					_ = tx.Rollback()
					return catalog.MetadataPatch{}, err
				}
				if err := tx.Rollback(); err != nil {
					return catalog.MetadataPatch{}, err
				}
				name := fmt.Sprintf("GEN_%d", gen)
				fragment := &catalog.Cache{}
				switch kind {
				case catalog.MetadataViews:
					fragment.Catalog = &catalog.CatalogCache{Views: map[string]*catalog.ViewDesc{name: {Name: name}}}
				case catalog.MetadataProcedures:
					fragment.Catalog = &catalog.CatalogCache{Procedures: map[string]*catalog.ProcedureDesc{name: {Name: name}}}
				case catalog.MetadataDomains:
					fragment.Catalog = &catalog.CatalogCache{Domains: map[string]*catalog.DomainDesc{name: {Name: name}}}
				}
				return catalog.MetadataPatch{Cache: fragment, Count: int(gen)}, nil
			}})
		}
		load, err := loader.Start(context.Background(), generation, metadataRepo(plan))
		if err != nil {
			t.Fatalf("generation %d: %v", generation, err)
		}
		last = load
		for range kinds {
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatalf("generation %d did not enter all gated queries (active=%d)", generation, active.Load())
			}
		}
		if got := active.Load(); got != int32(len(kinds)) {
			t.Fatalf("generation %d active queries = %d, want %d", generation, got, len(kinds))
		}
		releasePreviousGate = releaseGate
	}
	loader.Stop() // generation 100's three real transactions/rows remain gated
	waitLoad(t, last)
	if got := active.Load(); got != int32(len(kinds)) {
		t.Fatalf("active calls during Stop = %d, want %d", got, len(kinds))
	}
	if cache := loader.Cache(); cache.HasCatalog() && (len(cache.Catalog.Views) != 0 || len(cache.Catalog.Procedures) != 0 || len(cache.Catalog.Domains) != 0) {
		t.Fatalf("stopped current generation retained stale catalog: %+v", cache.Catalog)
	}
	waitDone := make(chan error, 1)
	waitObserving := make(chan struct{})
	waitContext, cancelWait := context.WithCancel(context.Background())
	waitCtx := &observedWaitContext{Context: waitContext, observed: waitObserving}
	go func() {
		waitDone <- loader.Wait(waitCtx)
	}()
	select {
	case <-waitObserving: // Wait has entered the context's Done method.
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not enter its context's Done method")
	}
	// Cancellation proves this particular Wait is still in flight even if it
	// has not yet been scheduled into its select after calling Done.
	cancelWait()
	select {
	case err := <-waitDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight Wait error = %v, want context.Canceled while generation remains gated", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight Wait did not return after its context was canceled")
	}
	releaseLatestGate()
	releaseLatestGate = nil
	if err := loader.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snapshot := loader.Snapshot(); snapshot.Generation != 100 {
		t.Fatalf("current generation = %d, want 100", snapshot.Generation)
	} else if snapshot.Status[catalog.MetadataViews].State != catalog.MetadataCancelled || snapshot.Status[catalog.MetadataProcedures].State != catalog.MetadataCancelled || snapshot.Status[catalog.MetadataDomains].State != catalog.MetadataCancelled {
		t.Fatalf("stopped generation statuses = %+v", snapshot.Status)
	}
	if active.Load() != 0 {
		t.Fatalf("active calls after Wait = %d", active.Load())
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	resources.mu.Lock()
	if resources.txBegins != resources.txRollbacks || resources.rowsOpened != resources.rowsClosed {
		t.Fatalf("drained driver resources: tx begin/rollback=%d/%d rows open/close=%d/%d", resources.txBegins, resources.txRollbacks, resources.rowsOpened, resources.rowsClosed)
	}
	if resources.txBegins != 3*100 {
		t.Fatalf("driver transactions = %d, want 300 (three jobs in each of 100 generations)", resources.txBegins)
	}
	resources.mu.Unlock()
	if got := peak.Load(); got != 3 {
		t.Fatalf("peak active calls = %d, want worker bound 3", got)
	}
}

func TestMetadataLoaderStatusOnlyChangesRetainCachePointer(t *testing.T) {
	loader := NewLoader()
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
	load, err := loader.Start(context.Background(), 1, metadataRepo(catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{
		{Kind: catalog.MetadataViews, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			close(started)
			<-gate
			return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Views: map[string]*catalog.ViewDesc{}}}}, nil
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
	if loader.Snapshot().Cache.Metadata[catalog.MetadataViews] != catalog.MetadataReady {
		t.Fatal("cache readiness missing")
	}
}

func TestMetadataLoaderGenerationAndStartErrors(t *testing.T) {
	loader := NewLoader()
	t.Cleanup(loader.Stop)
	if _, err := loader.Start(context.Background(), 0, metadataRepo(catalog.MetadataPlan{})); !errors.Is(err, ErrMetadataStaleGeneration) {
		t.Fatalf("initial Start error = %v", err)
	}
	loader.Reset(2)
	loader.Reset(1)
	load, err := loader.Start(context.Background(), 2, metadataRepo(catalog.MetadataPlan{Parallelism: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loader.Start(context.Background(), 2, metadataRepo(catalog.MetadataPlan{Parallelism: 1})); !errors.Is(err, ErrMetadataAlreadyStarted) {
		t.Fatalf("duplicate Start error = %v", err)
	}
	waitLoad(t, load)
	loader.Stop()
	if _, err := loader.Start(context.Background(), 2, metadataRepo(catalog.MetadataPlan{Parallelism: 1})); !errors.Is(err, ErrMetadataStopped) {
		t.Fatalf("stopped Start error = %v", err)
	}
}

func TestMetadataLoaderAdmissionRejectsResetGeneration(t *testing.T) {
	loader := NewLoader()
	loader.Reset(1)
	oldCache := loader.Cache()
	loader.mu.Lock()
	loader.started = true
	loader.ctx = context.Background()
	status := cloneMap(loader.snapshot.Status)
	status[catalog.MetadataViews] = catalog.MetadataStatus{State: catalog.MetadataPending}
	loader.snapshot = &catalog.MetadataSnapshot{Generation: 1, Revision: loader.snapshot.Revision + 1, Cache: loader.snapshot.Cache, Status: status}
	loader.mu.Unlock()
	loader.semaphore <- struct{}{}
	captured, admitted := loader.admitJob(1, context.Background(), catalog.MetadataViews)
	if !admitted || captured != oldCache {
		t.Fatalf("current generation admission = (%p, %t), want (%p, true)", captured, admitted, oldCache)
	}
	loader.runnerFinished()
	loader.Reset(2)
	cache, admitted := loader.admitJob(1, context.Background(), catalog.MetadataProcedures)
	<-loader.semaphore
	if admitted {
		t.Fatal("obsolete generation admitted a runner")
	}
	if cache != nil || loader.Cache() == oldCache {
		t.Fatal("obsolete admission captured or published the new generation cache")
	}
	loader.Stop()
}

func TestMetadataLoaderRejectedOldGenerationSlotWakesWaitingGeneration(t *testing.T) {
	loader := NewLoader()
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	for i := 0; i < cap(loader.semaphore); i++ {
		loader.semaphore <- struct{}{}
	}
	loader.Reset(2)

	// Model the new generation parked on the loader's current wake epoch while
	// all three permits remain held by superseded-generation runners.
	wake := loader.slotWakeChannel()
	waiting := make(chan struct{})
	go func() {
		close(waiting)
		<-wake
	}()
	<-waiting

	// A stale admission releases a permit after Reset. That release must wake
	// the parked generation as well as make the permit available.
	loader.releaseMetadataSlot()
	select {
	case <-wake:
	case <-time.After(2 * time.Second):
		t.Fatal("releasing rejected old-generation slot did not wake waiting generation")
	}
	select {
	case loader.semaphore <- struct{}{}:
		<-loader.semaphore
	default:
		t.Fatal("rejected admission did not release its global slot")
	}
}

func TestMetadataLoaderRejectsOldGenerationColumns(t *testing.T) {
	loader := NewLoader()
	t.Cleanup(loader.Stop)
	oldStarted, releaseOld := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseOld) }) }
	t.Cleanup(release)
	loader.Reset(1)
	oldLoad, err := loader.Start(context.Background(), 1, metadataRepo(catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{
		Kind: catalog.MetadataColumnsAll,
		Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			close(oldStarted)
			<-releaseOld
			return catalog.MetadataPatch{Cache: &catalog.Cache{ColumnsWithParent: map[string][]*catalog.ColumnDesc{"T": {{ColumnBase: catalog.ColumnBase{Table: "T", Name: "OLD"}}}}}}, nil
		},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-oldStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("old-generation column job did not start")
	}
	loader.Reset(2)
	newLoad, err := loader.Start(context.Background(), 2, metadataRepo(catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{
		Kind: catalog.MetadataColumnsAll,
		Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
			return catalog.MetadataPatch{Cache: &catalog.Cache{ColumnsWithParent: map[string][]*catalog.ColumnDesc{"T": {{ColumnBase: catalog.ColumnBase{Table: "T", Name: "NEW"}}}}}}, nil
		},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	waitLoad(t, newLoad)
	release()
	waitLoad(t, oldLoad)
	if err := loader.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	columns := loader.Cache().ColumnsWithParent["T"]
	if len(columns) != 1 || columns[0].Name != "NEW" {
		t.Fatalf("published columns = %#v, want only NEW", columns)
	}
}

func TestMetadataLoaderStopIsIdempotent(t *testing.T) {
	loader := NewLoader()
	loader.Reset(1)
	loader.Stop()
	loader.Stop()
	loader.Stop()
	if _, err := loader.Start(context.Background(), 1, metadataRepo(catalog.MetadataPlan{Parallelism: 1})); !errors.Is(err, ErrMetadataStopped) {
		t.Fatalf("Start after repeated Stop = %v, want stopped", err)
	}
}
