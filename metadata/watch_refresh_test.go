package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sqls-server/sqls/catalog"
)

type testFingerprint struct {
	value  int
	bytes  int64
	format string
}

func (f testFingerprint) Format() string {
	if f.format == "" {
		return "test/v1"
	}
	return f.format
}
func (f testFingerprint) RetainedBytes() int64 { return f.bytes }
func (f testFingerprint) Equal(other Fingerprint) bool {
	v, ok := other.(testFingerprint)
	return ok && f == v
}

// Like metadatatest.Provider, this fixture has deterministic plans, gates and
// counters. It lives here to permit whitebox baseline/lifecycle assertions.
type controlledWatchProvider struct {
	mu                                                      sync.Mutex
	fingerprint                                             testFingerprint
	scanErr, readErr                                        error
	fullStatus                                              map[catalog.MetadataKind]catalog.MetadataStatus
	fullReason                                              string
	gate                                                    <-chan struct{}
	scanGate                                                <-chan struct{}
	coverageGate                                            <-chan struct{}
	coverageEntered                                         chan struct{}
	planGate, detailGate                                    <-chan struct{}
	planEntered, detailEntered                              chan struct{}
	entered                                                 chan string
	changeDuringRead                                        bool
	deltaOverride                                           *ObjectDelta
	scans, fulls, partials, plans, details, running, closes atomic.Int64
	closedWhileRunning                                      atomic.Bool
	ignoreScanLimit                                         bool
}

func newControlledWatch() *controlledWatchProvider {
	return &controlledWatchProvider{fingerprint: testFingerprint{value: 1}, entered: make(chan string, 32)}
}
func watchCache(v int) *catalog.Cache {
	c := NewCache()
	c.Catalog = &catalog.CatalogCache{ProceduresByID: map[string]*catalog.ProcedureDesc{}, Procedures: map[string]*catalog.ProcedureDesc{}}
	for _, name := range []string{"P", "Q"} {
		desc := &catalog.ProcedureDesc{Name: name, Source: sql.NullString{String: fmt.Sprint(v), Valid: true}}
		c.Catalog.ProceduresByID[watchID(catalog.ObjectKindProcedure, name).Key()] = desc
		c.Catalog.Procedures[name] = desc
	}
	c.Catalog.RebuildObjects(nil)
	c.Metadata[catalog.MetadataProcedures] = catalog.MetadataReady
	return c
}
func (p *controlledWatchProvider) Plan() catalog.MetadataPlan {
	p.plans.Add(1)
	p.mu.Lock()
	gate, entered := p.planGate, p.planEntered
	p.mu.Unlock()
	return catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{Kind: catalog.MetadataProcedures, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
		p.running.Add(1)
		defer p.running.Add(-1)
		if entered != nil {
			close(entered)
		}
		if gate != nil {
			<-gate
		}
		return catalog.MetadataPatch{Cache: watchCache(0), Count: 2}, nil
	}}}}
}
func (p *controlledWatchProvider) DetailPlan(Request) (DetailPlan, error) {
	p.mu.Lock()
	gate, entered, detailGate, detailEntered := p.coverageGate, p.coverageEntered, p.detailGate, p.detailEntered
	p.mu.Unlock()
	return DetailPlan{Requires: []catalog.MetadataKind{catalog.MetadataProcedures}, Coverage: "P", CoverageForCache: func(*catalog.Cache) (string, error) {
		if entered != nil {
			close(entered)
		}
		if gate != nil {
			<-gate
		}
		return "P", nil
	}, Load: func(_ context.Context, c *catalog.Cache) (Detail, error) {
		p.details.Add(1)
		p.running.Add(1)
		defer p.running.Add(-1)
		if detailEntered != nil {
			close(detailEntered)
		}
		if detailGate != nil {
			<-detailGate
		}
		return Detail{Text: c.Catalog.ProceduresByID[watchID(catalog.ObjectKindProcedure, "P").Key()].Source.String}, nil
	}}, nil
}
func (p *controlledWatchProvider) Close() error {
	if p.running.Load() != 0 {
		p.closedWhileRunning.Store(true)
	}
	p.closes.Add(1)
	return nil
}
func (p *controlledWatchProvider) ReadFingerprint(ctx context.Context, limit int64) (Fingerprint, WatchMetrics, error) {
	p.scans.Add(1)
	p.running.Add(1)
	defer p.running.Add(-1)
	p.mu.Lock()
	f, err, gate, ignoreLimit := p.fingerprint, p.scanErr, p.scanGate, p.ignoreScanLimit
	p.mu.Unlock()
	p.entered <- "scan"
	if gate != nil {
		<-gate
	}
	if f.bytes > limit && !ignoreLimit {
		return nil, WatchMetrics{}, ErrWatchLimit
	}
	return f, WatchMetrics{Queries: 1}, err
}
func (p *controlledWatchProvider) read(kind string) (int, error, string, map[catalog.MetadataKind]catalog.MetadataStatus) {
	p.running.Add(1)
	defer p.running.Add(-1)
	p.mu.Lock()
	v, err, reason, gate, status := p.fingerprint.value, p.readErr, p.fullReason, p.gate, p.fullStatus
	if p.changeDuringRead {
		p.fingerprint.value++
		p.changeDuringRead = false
	}
	p.mu.Unlock()
	p.entered <- kind
	if gate != nil {
		<-gate
	} // Deliberately ignores cancellation until native drain.
	return v, err, reason, status
}
func (p *controlledWatchProvider) ReadFull(context.Context) (*catalog.Cache, map[catalog.MetadataKind]catalog.MetadataStatus, WatchMetrics, error) {
	p.fulls.Add(1)
	v, err, _, status := p.read("full")
	if status == nil {
		status = emptyMetadataStatus()
		status[catalog.MetadataProcedures] = catalog.MetadataStatus{State: catalog.MetadataReady, Count: 2}
	}
	return watchCache(v), status, WatchMetrics{Queries: 2}, err
}
func (p *controlledWatchProvider) ReadChanges(context.Context, *catalog.Cache, Fingerprint, Fingerprint) (ObjectDelta, error) {
	p.partials.Add(1)
	v, err, reason, _ := p.read("partial")
	p.mu.Lock()
	override := p.deltaOverride
	p.mu.Unlock()
	if override != nil {
		return *override, err
	}
	id := watchID(catalog.ObjectKindProcedure, "P")
	c := watchCache(v)
	delete(c.Catalog.ProceduresByID, watchID(catalog.ObjectKindProcedure, "Q").Key())
	delete(c.Catalog.Procedures, "Q")
	return ObjectDelta{Owners: map[catalog.MetadataKind][]catalog.ObjectID{catalog.MetadataProcedures: {id}}, Patches: map[catalog.MetadataKind]catalog.MetadataPatch{catalog.MetadataProcedures: {Cache: c, Count: 1, Queries: 3, QueriesKnown: true}}, Affected: 1, FullReason: reason}, err
}
func acquireWatch(t *testing.T, p *controlledWatchProvider) (*Registry, *Lease) {
	t.Helper()
	r := NewRegistry(context.Background(), Options{})
	l, err := r.Acquire(context.Background(), Key{Engine: "test", URL: t.Name()}, Scope{Principal: "P", TextPolicy: "NONE/CP1252"}, func(context.Context) (Provider, error) { return p, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = l.track.loader.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.Snapshot().Status[catalog.MetadataProcedures].State != catalog.MetadataReady {
		t.Fatal("fixture initial plan did not become ready", l.Snapshot().Status)
	}
	t.Cleanup(func() { l.Release(); _ = r.Close(context.Background()) })
	return r, l
}
func maintainWatch(t *testing.T, l *Lease, req MaintenanceRequest) MaintenanceResult {
	t.Helper()
	v, err := l.Maintain(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var watchedCheck = MaintenanceRequest{Mode: MaintenanceCheck, Fingerprint: true}
var watchedFull = MaintenanceRequest{Mode: MaintenanceFull, Fingerprint: true}

func TestMaintainPreloadBaselineAndNoop(t *testing.T) {
	p := newControlledWatch()
	_, l := acquireWatch(t, p)
	sub := l.Subscribe()
	defer sub.Close()
	first := maintainWatch(t, l, watchedCheck)
	if first.Outcome != "full" || !first.BaselineValid || p.scans.Load() != 1 || p.fulls.Load() != 1 {
		t.Fatalf("first: %+v", first)
	}
	before := l.Snapshot()
	detail, err := l.Detail(context.Background(), Request{})
	if err != nil || detail.Text != "1" {
		t.Fatal(detail, err)
	}
	select {
	case <-sub.C:
	default:
		t.Fatal("missing publication event")
	}
	same := maintainWatch(t, l, watchedCheck)
	if same.Outcome != "unchanged" || l.Snapshot() != before || p.details.Load() != 1 || p.fulls.Load() != 1 || p.partials.Load() != 0 {
		t.Fatal("unchanged mutated generation or read cache")
	}
	_, _ = l.Detail(context.Background(), Request{})
	if p.details.Load() != 1 {
		t.Fatal("unchanged cleared details")
	}
	select {
	case <-sub.C:
		t.Fatal("unchanged emitted event")
	default:
	}
	p.mu.Lock()
	p.fingerprint.value = 2
	p.changeDuringRead = true
	p.mu.Unlock()
	changed := maintainWatch(t, l, watchedCheck)
	if changed.Outcome != "incremental" || changed.Generation != before.Generation+1 || l.Snapshot().Status[catalog.MetadataProcedures].Count != 2 {
		t.Fatalf("changed: %+v %+v", changed, l.Snapshot())
	}
	if before.Cache.Catalog.ProceduresByID[watchID(catalog.ObjectKindProcedure, "P").Key()].Source.String != "1" {
		t.Fatal("mutated published cache")
	}
	next := maintainWatch(t, l, watchedCheck)
	if next.Outcome != "incremental" || p.partials.Load() != 2 || p.scans.Load() != 4 || p.plans.Load() != 1 {
		t.Fatalf("missed concurrent change or extra I/O: %+v", next)
	}
}

func TestMaintainScanErrorRouting(t *testing.T) {
	for _, scanErr := range []error{ErrWatchLimit, ErrWatchCapability, errors.New("native"), context.Canceled, context.DeadlineExceeded} {
		for _, req := range []MaintenanceRequest{watchedCheck, watchedFull} {
			t.Run(fmt.Sprintf("%v/%v", req.Mode, scanErr), func(t *testing.T) {
				p := newControlledWatch()
				_, l := acquireWatch(t, p)
				maintainWatch(t, l, watchedCheck)
				before := l.Snapshot()
				p.mu.Lock()
				p.scanErr = scanErr
				p.mu.Unlock()
				got, err := l.Maintain(context.Background(), req)
				fallback := req.Mode == MaintenanceFull && (errors.Is(scanErr, ErrWatchLimit) || errors.Is(scanErr, ErrWatchCapability))
				if fallback {
					if err != nil || got.Outcome != "full-unwatched" || got.BaselineValid || got.WatchError == "" || p.fulls.Load() != 2 {
						t.Fatalf("fallback: %+v %v", got, err)
					}
				} else {
					if !errors.Is(err, scanErr) || l.Snapshot() != before || p.fulls.Load() != 1 {
						t.Fatalf("failed scan caused read/publication: %+v %v", got, err)
					}
					p.mu.Lock()
					p.scanErr = nil
					p.mu.Unlock()
					if maintainWatch(t, l, watchedCheck).Outcome != "unchanged" {
						t.Fatal("failure changed baseline")
					}
				}
			})
		}
	}
}

func TestMaintainFailureAndUncertaintyFence(t *testing.T) {
	p := newControlledWatch()
	_, l := acquireWatch(t, p)
	maintainWatch(t, l, watchedCheck)
	before := l.Snapshot()
	baseline := l.track.baseline
	p.mu.Lock()
	p.fingerprint.value++
	p.readErr = errors.New("read failure")
	p.mu.Unlock()
	if _, err := l.Maintain(context.Background(), watchedCheck); err == nil || l.Snapshot() != before || l.track.baseline != baseline {
		t.Fatal("failed read published")
	}
	p.mu.Lock()
	p.readErr = nil
	gate := make(chan struct{})
	p.gate = gate
	p.mu.Unlock()
	for len(p.entered) > 0 {
		<-p.entered
	}
	done := make(chan error, 1)
	go func() { _, err := l.Maintain(context.Background(), watchedCheck); done <- err }()
	waitWatchEntered(t, p, "partial")
	_ = l.Invalidate()
	close(gate)
	if err := <-done; !errors.Is(err, ErrStale) || l.Snapshot().Generation != before.Generation || !l.Snapshot().Stale {
		t.Fatalf("invalidation: %v", err)
	}
	p.mu.Lock()
	p.gate = nil
	p.mu.Unlock()
	if _, err := l.Maintain(context.Background(), watchedFull); !errors.Is(err, ErrStale) {
		t.Fatal("automatic cleared uncertainty", err)
	}
	req := watchedFull
	req.Explicit = true
	maintainWatch(t, l, req)
	if l.Snapshot().Stale {
		t.Fatal("explicit full did not resolve captured epoch")
	}
	if err := l.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = l.track.loader.Wait(context.Background())
	if maintainWatch(t, l, watchedCheck).Outcome != "full" {
		t.Fatal("ordinary refresh kept baseline")
	}
}

func TestMaintainInvalidationEpochIsAtomicWithStaleness(t *testing.T) {
	p := newControlledWatch()
	_, l := acquireWatch(t, p)
	loader := l.track.loader
	loader.mu.Lock()
	done := make(chan struct{})
	go func() { _ = l.Invalidate(); close(done) }()
	// While MarkStale is blocked, admission must not see the incremented epoch
	// alongside a non-stale loader. That would let automatic work clear it.
	deadline := time.NewTimer(40 * time.Millisecond)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	leaked := false
loop:
	for {
		select {
		case <-tick.C:
			if l.track.mu.TryLock() {
				leaked = l.track.invalidations != 0
				l.track.mu.Unlock()
				if leaked {
					break loop
				}
			}
		case <-deadline.C:
			break loop
		}
	}
	loader.mu.Unlock()
	<-done
	if leaked {
		t.Fatal("admission observed new epoch before stale/detail fence")
	}
}

func TestMaintainExplicitFullRejectsNewerInvalidation(t *testing.T) {
	p := newControlledWatch()
	_, l := acquireWatch(t, p)
	maintainWatch(t, l, watchedCheck)
	_ = l.Invalidate()
	before := l.Snapshot()
	gate := make(chan struct{})
	p.gate = gate
	for len(p.entered) > 0 {
		<-p.entered
	}
	req := watchedFull
	req.Explicit = true
	done := make(chan error, 1)
	go func() { _, err := l.Maintain(context.Background(), req); done <- err }()
	waitWatchEntered(t, p, "full")
	_ = l.Invalidate()
	close(gate)
	if err := <-done; !errors.Is(err, ErrStale) || l.Snapshot().Generation != before.Generation || !l.Snapshot().Stale {
		t.Fatal("new uncertainty cleared", err)
	}
}

func TestMaintainScopeIncarnationAndFormatValidity(t *testing.T) {
	for _, field := range []string{"scope", "text-policy", "incarnation", "generation", "format"} {
		t.Run(field, func(t *testing.T) {
			p := newControlledWatch()
			_, l := acquireWatch(t, p)
			maintainWatch(t, l, watchedCheck)
			l.track.mu.Lock()
			b := l.track.baseline
			switch field {
			case "scope":
				b.scope.principal = "other"
			case "text-policy":
				b.scope.textPolicy = "UTF8"
			case "incarnation":
				b.incarnation++
			case "generation":
				b.generation++
			case "format":
				p.mu.Lock()
				p.fingerprint.format = "test/v2"
				p.mu.Unlock()
			}
			l.track.mu.Unlock()
			if maintainWatch(t, l, watchedCheck).Outcome != "full" || p.fulls.Load() != 2 || p.partials.Load() != 0 {
				t.Fatal("invalid baseline reused")
			}
		})
	}
}

func TestMaintainFailedSupportedCategoryCannotEstablishBaseline(t *testing.T) {
	p := newControlledWatch()
	_, l := acquireWatch(t, p)
	before := l.Snapshot()
	status := emptyMetadataStatus()
	status[catalog.MetadataProcedures] = catalog.MetadataStatus{State: catalog.MetadataReady}
	status[catalog.MetadataFunctions] = catalog.MetadataStatus{State: catalog.MetadataFailed, Count: 9, Queries: 4, QueriesKnown: true, Err: errors.New("functions failed")}
	p.fullStatus = status
	if _, err := l.Maintain(context.Background(), watchedCheck); !errors.Is(err, ErrWatchIncomplete) || l.Snapshot() != before {
		t.Fatal("incomplete full established baseline", err)
	}
	got := maintainWatch(t, l, MaintenanceRequest{Mode: MaintenanceFull})
	if got.BaselineValid || l.Snapshot().Status[catalog.MetadataFunctions].Count != 9 || l.Snapshot().Status[catalog.MetadataFunctions].Queries != 4 {
		t.Fatal("failed accounting lost")
	}
}

func TestMaintainDependencyOnlyPublishesAndPreservesFailedAccounting(t *testing.T) {
	p := newControlledWatch()
	_, l := acquireWatch(t, p)
	maintainWatch(t, l, watchedCheck)
	previous := l.Snapshot()
	// Install a synthetic immutable base to exercise failed accounting without
	// mutating any previously published snapshot.
	cache := *previous.Cache
	cache.Dependencies = catalog.NewDependencyGraph(nil, nil)
	cache.Metadata = cloneMap(previous.Cache.Metadata)
	cache.Metadata[catalog.MetadataDependencies] = catalog.MetadataReady
	status := cloneMap(previous.Status)
	status[catalog.MetadataDependencies] = catalog.MetadataStatus{State: catalog.MetadataReady}
	status[catalog.MetadataFunctions] = catalog.MetadataStatus{State: catalog.MetadataFailed, Count: 7, Queries: 8, Err: errors.New("failed")}
	base := *previous
	base.Cache, base.Status = &cache, status
	loader := NewLoader(l.track.storeBudget)
	if err := loader.installSettled(context.Background(), &base); err != nil {
		t.Fatal(err)
	}
	l.track.mu.Lock()
	l.track.loader = loader
	l.track.mu.Unlock()
	before := l.Snapshot()
	edge := catalog.DependencyEdge{From: catalog.DependencyRef{Kind: catalog.ObjectKindProcedure, Name: "P"}, To: catalog.DependencyRef{Kind: catalog.ObjectKindProcedure, Name: "Q"}}
	p.deltaOverride = &ObjectDelta{DependencyOwners: []catalog.DependencyRef{edge.From}, DependencyEdges: []catalog.DependencyEdge{edge}, Affected: 1}
	p.fingerprint.value++
	got := maintainWatch(t, l, watchedCheck)
	if got.Outcome != "incremental" || got.Generation != before.Generation+1 || l.Snapshot().Cache.Dependencies.Len() != 1 || l.Snapshot().Status[catalog.MetadataDependencies].Count != 1 || l.Snapshot().Status[catalog.MetadataFunctions].Count != 7 || l.Snapshot().Status[catalog.MetadataFunctions].Queries != 8 {
		t.Fatal(got)
	}
	if before.Cache.Dependencies.Len() != 0 {
		t.Fatal("mutated prior dependency graph")
	}
}

func TestMaintainProviderLimitDefenseAndDisabledFull(t *testing.T) {
	p := newControlledWatch()
	_, l := acquireWatch(t, p)
	before := l.Snapshot()
	p.fingerprint.bytes = watchFingerprintLimit + 1
	p.ignoreScanLimit = true // Verify the lease cap even if a reader ignores it.
	if _, err := l.Maintain(context.Background(), watchedCheck); !errors.Is(err, ErrWatchLimit) || l.Snapshot() != before || p.fulls.Load() != 0 {
		t.Fatal("over-limit check loaded catalog", err)
	}
	got := maintainWatch(t, l, watchedFull)
	if got.BaselineValid || got.WatchError != "limit" || got.Outcome != "full-unwatched" {
		t.Fatal(got)
	}
	got = maintainWatch(t, l, MaintenanceRequest{Mode: MaintenanceFull})
	if got.WatchError != "" || p.scans.Load() != 2 || p.fulls.Load() != 2 {
		t.Fatal("disabled full scanned", got)
	}
}

func TestMaintainRetireReacquireDoesNotShareBaselineOrCloseActiveProvider(t *testing.T) {
	p := newControlledWatch()
	r, l := acquireWatch(t, p)
	maintainWatch(t, l, watchedCheck)
	for len(p.entered) > 0 {
		<-p.entered
	}
	gate := make(chan struct{})
	p.gate = gate
	p.fingerprint.value++
	done := make(chan error, 1)
	go func() { _, err := l.Maintain(context.Background(), watchedCheck); done <- err }()
	waitWatchEntered(t, p, "partial")
	l.Release()
	fresh := newControlledWatch()
	other, err := r.Acquire(context.Background(), l.Key(), l.track.scope, func(context.Context) (Provider, error) { return fresh, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	_ = other.track.loader.Wait(context.Background())
	if other.Incarnation() == l.Incarnation() || p.closes.Load() != 0 {
		t.Fatal("retired track reused or active provider closed")
	}
	if maintainWatch(t, other, watchedCheck).Outcome != "full" || fresh.fulls.Load() != 1 {
		t.Fatal("baseline shared across incarnation")
	}
	close(gate)
	if err = <-done; err == nil {
		t.Fatal("retired work published")
	}
	deadline := time.After(3 * time.Second)
	for p.closes.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("retired provider not closed")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if p.closedWhileRunning.Load() || p.running.Load() != 0 {
		t.Fatal("retirement failed native drain")
	}
}

func TestInstallSettledValidatesWithoutIO(t *testing.T) {
	for _, mutation := range []string{"nil", "zero-generation", "loading", "not-started", "cache-not-ready", "missing-status"} {
		t.Run(mutation, func(t *testing.T) {
			s := &catalog.MetadataSnapshot{Generation: 2, Started: true, Cache: watchCache(2), Status: map[catalog.MetadataKind]catalog.MetadataStatus{catalog.MetadataProcedures: {State: catalog.MetadataReady}}}
			switch mutation {
			case "nil":
				s = nil
			case "zero-generation":
				s.Generation = 0
			case "loading":
				s.Status[catalog.MetadataProcedures] = catalog.MetadataStatus{State: catalog.MetadataLoading}
			case "not-started":
				s.Started = false
			case "cache-not-ready":
				delete(s.Cache.Metadata, catalog.MetadataProcedures)
			case "missing-status":
				s.Status = map[catalog.MetadataKind]catalog.MetadataStatus{catalog.MetadataSchemas: {State: catalog.MetadataUnsupported}}
			}
			if err := NewLoader(nil).installSettled(context.Background(), s); !errors.Is(err, ErrWatchIncomplete) {
				t.Fatal("accepted invalid staged snapshot", err)
			}
		})
	}
	l := NewLoader(nil)
	status := map[catalog.MetadataKind]catalog.MetadataStatus{catalog.MetadataProcedures: {State: catalog.MetadataReady}}
	if err := l.installSettled(context.Background(), &catalog.MetadataSnapshot{Generation: 2, Started: true, Cache: watchCache(2), Status: status}); err != nil {
		t.Fatal(err)
	}
	delete(status, catalog.MetadataProcedures)
	if l.active != 0 || len(l.semaphore) != 0 || len(l.hostSemaphore) != 0 || !l.Snapshot().Settled() {
		t.Fatal("settled install created work or shared mutable status")
	}
	select {
	case <-l.done:
	default:
		t.Fatal("not logically settled")
	}
	select {
	case <-l.drained:
	default:
		t.Fatal("not drained")
	}
	l.Stop()
}

func TestMaintainDetailTransitionNeverRelabelsOldCachedDetail(t *testing.T) {
	p := newControlledWatch()
	_, l := acquireWatch(t, p)
	maintainWatch(t, l, watchedCheck)
	if v, err := l.Detail(context.Background(), Request{}); err != nil || v.Text != "1" {
		t.Fatal(v, err)
	}
	gate, entered := make(chan struct{}), make(chan struct{})
	p.mu.Lock()
	p.coverageGate, p.coverageEntered = gate, entered
	p.mu.Unlock()
	old := make(chan error, 1)
	go func() { _, err := l.Detail(context.Background(), Request{}); old <- err }()
	<-entered // The detail captured the OLD snapshot, before acquisition.
	p.mu.Lock()
	p.coverageGate, p.coverageEntered = nil, nil
	p.fingerprint.value++
	p.mu.Unlock()
	maintainWatch(t, l, watchedCheck)
	newDetail, err := l.Detail(context.Background(), Request{})
	if err != nil || newDetail.Text != "2" {
		t.Fatal("new snapshot used old cached detail", newDetail, err)
	}
	close(gate)
	if err = <-old; !errors.Is(err, ErrStale) {
		t.Fatal("old acquisition crossed fence", err)
	}
	if p.details.Load() != 2 {
		t.Fatal("unexpected detail I/O", p.details.Load())
	}
}

func TestMaintainDeadlineKeepsNativePermitsThroughDrain(t *testing.T) {
	for _, phase := range []string{"scan", "full"} {
		t.Run(phase, func(t *testing.T) {
			p := newControlledWatch()
			r, l := acquireWatch(t, p)
			before := l.Snapshot()
			gate := make(chan struct{})
			if phase == "scan" {
				p.scanGate = gate
			} else {
				p.gate = gate
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := l.Maintain(ctx, watchedCheck); done <- err }()
			waitWatchEntered(t, p, phase)
			<-ctx.Done()
			if p.running.Load() != 1 || len(l.track.storeBudget.sem) != 1 || len(l.track.storeBudget.host) != 1 {
				t.Fatal("native operation lost permits")
			}
			select {
			case <-done:
				t.Fatal("deadline escaped native drain")
			default:
			}
			closed := make(chan error, 1)
			go func() { closed <- r.Close(context.Background()) }()
			select {
			case <-closed:
				t.Fatal("shutdown escaped native drain")
			case <-time.After(10 * time.Millisecond):
			}
			if p.closes.Load() != 0 {
				t.Fatal("provider closed before scan/read drained")
			}
			close(gate)
			if err := <-done; !errors.Is(err, context.DeadlineExceeded) || l.Snapshot() != before {
				t.Fatal("timed-out native success published", err)
			}
			if err := <-closed; err != nil {
				t.Fatal(err)
			}
			if p.running.Load() != 0 || p.closes.Load() != 1 || p.closedWhileRunning.Load() || len(l.track.storeBudget.sem) != 0 || len(l.track.storeBudget.host) != 0 {
				t.Fatal("native resource leak")
			}
			if phase == "scan" && (p.fulls.Load() != 0 || p.partials.Load() != 0) {
				t.Fatal("timed-out scan initiated catalog read")
			}
		})
	}
}

func TestMaintainScopesHaveIndependentBaselines(t *testing.T) {
	p := newControlledWatch()
	r, l := acquireWatch(t, p)
	maintainWatch(t, l, watchedCheck)
	for _, scope := range []Scope{{Principal: "Q", TextPolicy: "NONE/CP1252"}, {Principal: "P", TextPolicy: "UTF8"}} {
		otherProvider := newControlledWatch()
		other, err := r.Acquire(context.Background(), l.Key(), scope, func(context.Context) (Provider, error) { return otherProvider, nil })
		if err != nil {
			t.Fatal(err)
		}
		_ = other.track.loader.Wait(context.Background())
		if maintainWatch(t, other, watchedCheck).Outcome != "full" || otherProvider.fulls.Load() != 1 {
			t.Fatal("scope reused another baseline")
		}
		other.Release()
	}
	if maintainWatch(t, l, watchedCheck).Outcome != "unchanged" || p.fulls.Load() != 1 {
		t.Fatal("sibling scope invalidated baseline")
	}
}

func TestMaintainFullAlsoUsesPreloadBaselineAndForcesRead(t *testing.T) {
	p := newControlledWatch()
	_, l := acquireWatch(t, p)
	p.changeDuringRead = true
	first := maintainWatch(t, l, watchedFull)
	if first.Outcome != "full" {
		t.Fatal(first)
	}
	if phase := <-p.entered; phase != "scan" {
		t.Fatal("full read preceded scan", phase)
	}
	if phase := <-p.entered; phase != "full" {
		t.Fatal(phase)
	}
	if next := maintainWatch(t, l, watchedCheck); next.Outcome != "incremental" || p.scans.Load() != 2 || p.partials.Load() != 1 {
		t.Fatal("full acknowledged post-load change", next)
	}
	before := l.Snapshot()
	got := maintainWatch(t, l, watchedFull)
	if got.Generation != before.Generation+1 || got.Outcome != "full" || p.fulls.Load() != 2 {
		t.Fatal("equal fingerprint suppressed forced full", got)
	}
	if err := l.RefreshInBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	if maintainWatch(t, l, watchedCheck).Outcome != "full" {
		t.Fatal("background refresh retained baseline")
	}
}

func TestMaintainCopiesReadyStatusAndCacheStateMaps(t *testing.T) {
	p := newControlledWatch()
	_, l := acquireWatch(t, p)
	status := emptyMetadataStatus()
	status[catalog.MetadataProcedures] = catalog.MetadataStatus{State: catalog.MetadataReady, Count: 999, Queries: 42, QueriesKnown: true}
	p.fullStatus = status
	maintainWatch(t, l, watchedFull)
	if status[catalog.MetadataProcedures].Count != 999 || l.Snapshot().Status[catalog.MetadataProcedures].Count != 2 || l.Snapshot().Status[catalog.MetadataProcedures].Queries != 42 {
		t.Fatal("ready status not copied or count not complete")
	}
	before := l.Snapshot()
	p.fingerprint.value++
	maintainWatch(t, l, watchedCheck)
	if before.Status[catalog.MetadataProcedures].Queries != 42 || l.Snapshot().Status[catalog.MetadataProcedures].Queries != 3 || before.Status[catalog.MetadataProcedures].Count != 2 || l.Snapshot().Status[catalog.MetadataProcedures].Count != 2 {
		t.Fatal("incremental mutated prior accounting")
	}
	// A map identity assertion without mutating published data.
	if fmt.Sprintf("%p", before.Cache.Metadata) == fmt.Sprintf("%p", l.Snapshot().Cache.Metadata) {
		t.Fatal("ready state map shared across generations")
	}
}

func TestMaintainCancellationAtDetailPublicationLockPreservesEverything(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			p := newControlledWatch()
			_, l := acquireWatch(t, p)
			maintainWatch(t, l, watchedCheck)
			if v, err := l.Detail(context.Background(), Request{}); err != nil || v.Text != "1" {
				t.Fatal(v, err)
			}
			before, baseline := l.Snapshot(), l.track.baseline
			sub := l.Subscribe()
			defer sub.Close()
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
			}
			defer cancel()
			d := l.track.details
			d.mu.Lock()
			entry, genCtx, totalBytes := d.entries["P"], d.genCtx, d.totalBytes
			done := make(chan error, 1)
			go func() { _, err := l.Maintain(ctx, watchedFull); done <- err }()
			waitWatchEntered(t, p, "full")
			// A drained full plus track.mu held proves publication is waiting at
			// the detail lock, not still inside provider I/O or queued admission.
			until := time.Now().Add(time.Second)
			blocked := false
			for time.Now().Before(until) {
				locked := l.track.mu.TryLock()
				if locked {
					l.track.mu.Unlock()
				} else if p.running.Load() == 0 {
					blocked = true
					break
				}
				time.Sleep(time.Millisecond)
			}
			if !blocked {
				d.mu.Unlock()
				t.Fatal("publication did not reach detail lock")
			}
			if deadline {
				<-ctx.Done()
			} else {
				cancel()
			}
			d.mu.Unlock()
			err := <-done
			want := context.Canceled
			if deadline {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) {
				t.Fatalf("publication missed expired context: %v", err)
			}
			if l.Snapshot() != before || l.track.baseline != baseline {
				t.Fatal("failed publication replaced snapshot/baseline")
			}
			d.mu.Lock()
			preserved := d.generation == before.Generation && d.entries["P"] == entry && d.genCtx == genCtx && d.totalBytes == totalBytes && !d.stale
			d.mu.Unlock()
			if !preserved || genCtx.Err() != nil {
				t.Fatal("failed publication mutated detail generation/cache/context")
			}
			select {
			case <-sub.C:
				t.Fatal("failed publication emitted event")
			default:
			}
			if v, err := l.Detail(context.Background(), Request{}); err != nil || v.Text != "1" || p.details.Load() != 1 {
				t.Fatal("cached old detail was lost or relabelled", v, err)
			}
			if p.scans.Load() != 2 || p.fulls.Load() != 2 || p.partials.Load() != 0 || len(d.budget.sem) != 0 || len(d.budget.host) != 0 {
				t.Fatal("unexpected I/O or permit leak")
			}
		})
	}
}

func TestRegistryCloseTimeoutOwnsMaintenanceDrainAndRetries(t *testing.T) {
	p := newControlledWatch()
	r, l := acquireWatch(t, p)
	gate := make(chan struct{})
	p.gate = gate
	done := make(chan error, 1)
	go func() { _, err := l.Maintain(context.Background(), watchedFull); done <- err }()
	waitWatchEntered(t, p, "full")
	sub := l.Subscribe()
	defer sub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	first := r.Close(ctx)
	cancel()
	if !errors.Is(first, context.DeadlineExceeded) {
		close(gate)
		<-done
		t.Fatal("first close did not expire", first)
	}
	if p.closes.Load() != 0 || p.running.Load() != 1 || len(l.track.storeBudget.sem) != 1 || len(l.track.storeBudget.host) != 1 {
		close(gate)
		<-done
		t.Fatal("close timeout escaped actual drain")
	}
	late := make(chan error, 1)
	go func() { _, err := l.Maintain(context.Background(), watchedCheck); late <- err }()
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	retry := r.Close(ctx)
	cancel()
	if !errors.Is(retry, context.DeadlineExceeded) {
		close(gate)
		<-done
		t.Fatal("retry falsely completed gated cleanup", retry)
	}
	callers := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { callers <- r.Close(context.Background()) }()
	}
	select {
	case err := <-callers:
		close(gate)
		<-done
		t.Fatal("concurrent close falsely completed", err)
	case <-time.After(15 * time.Millisecond):
	}
	close(gate)
	if err := <-done; err == nil {
		t.Fatal("shutdown admitted publication")
	}
	if err := <-late; !errors.Is(err, ErrClosed) {
		t.Fatal("late admission accepted", err)
	}
	// No Release has occurred: successful retry must own provider cleanup.
	for i := 0; i < 2; i++ {
		select {
		case err := <-callers:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("close retry stranded cleanup")
		}
	}
	assertWatchClosed(t, r, l, p, sub)
	l.Release()
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.closes.Load() != 1 {
		t.Fatal("last release repeated provider close")
	}
}

func assertWatchClosed(t *testing.T, r *Registry, l *Lease, p *controlledWatchProvider, sub *Subscription) {
	t.Helper()
	if p.closes.Load() != 1 || p.running.Load() != 0 || p.closedWhileRunning.Load() || len(l.track.storeBudget.sem) != 0 || len(l.track.storeBudget.host) != 0 {
		t.Fatal("provider/native/permit cleanup incomplete")
	}
	r.budget.mu.Lock()
	_, retained := r.budget.stores[l.Key()]
	r.budget.mu.Unlock()
	if retained {
		t.Fatal("store budget reference retained")
	}
	r.mu.Lock()
	owned := len(r.liveTracks)
	r.mu.Unlock()
	if owned != 0 {
		t.Fatal("registry retained completed cleanup ownership")
	}
	select {
	case <-l.track.finalized:
	default:
		t.Fatal("physical finalization was not complete")
	}
	if err := l.track.loader.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.track.details.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case _, ok := <-sub.C:
			if !ok {
				return
			}
		default:
			t.Fatal("event hub was not closed")
		}
	}
}

func TestRegistryCloseTimeoutOwnsNotReadyFactory(t *testing.T) {
	r := NewRegistry(context.Background(), Options{})
	p := newControlledWatch()
	gate, entered := make(chan struct{}), make(chan struct{})
	acquired := make(chan error, 1)
	key := Key{Engine: "test", URL: t.Name()}
	go func() {
		l, err := r.Acquire(context.Background(), key, Scope{Principal: "P"}, func(context.Context) (Provider, error) { close(entered); <-gate; return p, nil })
		if l != nil {
			l.Release()
		}
		acquired <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	first := r.Close(ctx)
	cancel()
	if !errors.Is(first, context.DeadlineExceeded) {
		close(gate)
		<-acquired
		t.Fatal(first)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	retry := r.Close(ctx)
	cancel()
	if !errors.Is(retry, context.DeadlineExceeded) {
		close(gate)
		<-acquired
		t.Fatal("retry abandoned pending factory", retry)
	}
	if p.closes.Load() != 0 || p.plans.Load() != 0 {
		t.Fatal("factory provider closed or started before return")
	}
	close(gate)
	if err := <-acquired; !errors.Is(err, ErrClosed) {
		t.Fatal("acquired a closing track", err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.closes.Load() != 1 || p.plans.Load() != 0 {
		t.Fatal("factory cleanup duplicated, leaked or started a canceled load")
	}
	r.budget.mu.Lock()
	_, retained := r.budget.stores[key]
	r.budget.mu.Unlock()
	if retained {
		t.Fatal("factory budget reference leaked")
	}
}

func TestRegistryCloseTimeoutOwnsActualLoaderAndDetailDrain(t *testing.T) {
	for _, phase := range []string{"loader", "detail"} {
		t.Run(phase, func(t *testing.T) {
			r := NewRegistry(context.Background(), Options{})
			p := newControlledWatch()
			gate, entered := make(chan struct{}), make(chan struct{})
			if phase == "loader" {
				p.planGate, p.planEntered = gate, entered
			} else {
				p.detailGate, p.detailEntered = gate, entered
			}
			l, err := r.Acquire(context.Background(), Key{Engine: "test", URL: t.Name()}, Scope{Principal: "P"}, func(context.Context) (Provider, error) { return p, nil })
			if err != nil {
				t.Fatal(err)
			}
			defer l.Release()
			defer r.Close(context.Background())
			var detail chan error
			if phase == "detail" {
				_ = l.track.loader.Wait(context.Background())
				detail = make(chan error, 1)
				go func() { _, err := l.Detail(context.Background(), Request{}); detail <- err }()
			}
			<-entered
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			err = r.Close(ctx)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				close(gate)
				t.Fatal(err)
			}
			if p.running.Load() != 1 || p.closes.Load() != 0 || len(l.track.storeBudget.sem) != 1 || len(l.track.storeBudget.host) != 1 {
				close(gate)
				t.Fatal("close timeout lost actual loader/detail drain")
			}
			ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
			err = r.Close(ctx)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				close(gate)
				t.Fatal("retry falsely completed loader/detail drain", err)
			}
			close(gate)
			if err = r.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if detail != nil {
				if err = <-detail; !errors.Is(err, ErrStale) {
					t.Fatal("closing detail returned native success", err)
				}
			}
			if p.running.Load() != 0 || p.closes.Load() != 1 || p.closedWhileRunning.Load() || len(l.track.storeBudget.sem) != 0 || len(l.track.storeBudget.host) != 0 {
				t.Fatal("loader/detail resources leaked")
			}
			select {
			case <-l.track.finalized:
			default:
				t.Fatal("provider closed without owned completion")
			}
		})
	}
}

func TestRegistryCloseOwnsRetiredTrackAlongsideReplacement(t *testing.T) {
	p := newControlledWatch()
	r, l := acquireWatch(t, p)
	gate := make(chan struct{})
	p.gate = gate
	done := make(chan error, 1)
	go func() { _, err := l.Maintain(context.Background(), watchedFull); done <- err }()
	waitWatchEntered(t, p, "full")
	l.Release()
	fresh := newControlledWatch()
	other, err := r.Acquire(context.Background(), l.Key(), l.track.scope, func(context.Context) (Provider, error) { return fresh, nil })
	if err != nil {
		close(gate)
		<-done
		t.Fatal(err)
	}
	defer other.Release()
	_ = other.track.loader.Wait(context.Background())
	if other.Incarnation() == l.Incarnation() || other.track.storeBudget != l.track.storeBudget {
		close(gate)
		<-done
		t.Fatal("replacement lost incarnation or shared drain budget")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	err = r.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		close(gate)
		<-done
		t.Fatal("close ignored owned retired native drain", err)
	}
	if p.closes.Load() != 0 || len(l.track.storeBudget.sem) != 1 {
		close(gate)
		<-done
		t.Fatal("retired native drain closed or lost its permit")
	}
	close(gate)
	if err = <-done; err == nil {
		t.Fatal("retired work published")
	}
	if err = r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.closes.Load() != 1 || fresh.closes.Load() != 1 || p.closedWhileRunning.Load() || len(l.track.storeBudget.sem) != 0 || len(l.track.storeBudget.host) != 0 {
		t.Fatal("retired/replacement cleanup not exactly once")
	}
	r.budget.mu.Lock()
	_, retained := r.budget.stores[l.Key()]
	r.budget.mu.Unlock()
	if retained {
		t.Fatal("shared budget leaked after both tracks drained")
	}
}

func TestRegistryCloseDeadlineDuringGatedPublicationAndRelease(t *testing.T) {
	p := newControlledWatch()
	r, l := acquireWatch(t, p)
	maintainWatch(t, l, watchedCheck)
	d := l.track.details
	d.mu.Lock()
	done := make(chan error, 1)
	go func() { _, err := l.Maintain(context.Background(), watchedFull); done <- err }()
	waitWatchEntered(t, p, "full")
	until := time.Now().Add(time.Second)
	blocked := false
	for time.Now().Before(until) {
		locked := l.track.mu.TryLock()
		if locked {
			l.track.mu.Unlock()
		} else if p.running.Load() == 0 {
			blocked = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !blocked {
		d.mu.Unlock()
		<-done
		t.Fatal("publication did not block")
	}
	released := make(chan struct{})
	go func() { l.Release(); close(released) }()
	// Give Release a bounded opportunity to commit retirement or expose the
	// old registry.mu -> blocked publication mutex ordering.
	until = time.Now().Add(time.Second)
	for time.Now().Before(until) {
		select {
		case <-released:
			goto ready
		default:
		}
		if !r.mu.TryLock() {
			break
		}
		r.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
ready:
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- r.Close(ctx) }()
	<-ctx.Done()
	var early error
	returned := false
	select {
	case early = <-closed:
		returned = true
	case <-time.After(20 * time.Millisecond):
	}
	d.mu.Unlock()
	<-released
	<-done
	if !returned {
		<-closed
		t.Fatal("Close deadline blocked behind Release/publication lock")
	}
	if !errors.Is(early, context.DeadlineExceeded) {
		t.Fatal("close did not report its bounded wait", early)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.closes.Load() != 1 {
		t.Fatal("publication/release/close cleanup duplicated")
	}
}

func TestMaintainLifecycleCancellationSharesPublicationLock(t *testing.T) {
	for _, closeRegistry := range []bool{false, true} {
		t.Run(fmt.Sprint(closeRegistry), func(t *testing.T) {
			p := newControlledWatch()
			r, l := acquireWatch(t, p)
			// Holding mu stands in for the final publication critical section.
			l.track.mu.Lock()
			done := make(chan struct{})
			go func() {
				if closeRegistry {
					_ = r.Close(context.Background())
				} else {
					l.Release()
				}
				close(done)
			}()
			canceled := false
			select {
			case <-l.track.ctx.Done():
				canceled = true
			case <-time.After(20 * time.Millisecond):
			}
			l.track.mu.Unlock()
			<-done
			if canceled {
				t.Fatal("lifecycle cancellation crossed publication critical section")
			}
		})
	}
}
func waitWatchEntered(t *testing.T, p *controlledWatchProvider, kind string) {
	t.Helper()
	for {
		select {
		case v := <-p.entered:
			if v == kind {
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("read not entered")
		}
	}
}

func TestMaintainNativeDrainAndSerialization(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprint(shutdown), func(t *testing.T) {
			p := newControlledWatch()
			r, l := acquireWatch(t, p)
			gate := make(chan struct{})
			p.gate = gate
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := l.Maintain(ctx, watchedCheck); done <- err }()
			waitWatchEntered(t, p, "full")
			before := l.Snapshot()
			cancel()
			var closed chan error
			if shutdown {
				closed = make(chan error, 1)
				go func() { closed <- r.Close(context.Background()) }()
			}
			select {
			case <-done:
				t.Fatal("returned before native drain")
			case <-time.After(20 * time.Millisecond):
			}
			if p.closes.Load() != 0 {
				t.Fatal("closed provider before native drain")
			}
			// After drain the queued second attempt is legitimately allowed to
			// publish before the first caller observes its return. Hold its scan
			// so the assertion below isolates the canceled first attempt.
			secondScanGate := make(chan struct{})
			releaseSecond := sync.OnceFunc(func() { close(secondScanGate) })
			defer releaseSecond()
			p.mu.Lock()
			p.scanGate = secondScanGate
			p.mu.Unlock()
			second := make(chan error, 1)
			go func() { _, err := l.Maintain(context.Background(), watchedCheck); second <- err }()
			select {
			case <-second:
				if !shutdown {
					t.Fatal("lost serialization")
				}
			case <-time.After(20 * time.Millisecond):
			}
			if p.scans.Load() != 1 {
				t.Fatal("second scan during native drain")
			}
			close(gate)
			if err := <-done; err == nil || l.Snapshot() != before {
				t.Fatal("canceled success published", err)
			}
			releaseSecond()
			if shutdown {
				if err := <-closed; err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-second:
					if !errors.Is(err, ErrClosed) {
						t.Fatal(err)
					}
				default:
				}
			} else {
				if err := <-second; err != nil {
					t.Fatal(err)
				}
			}
			if p.running.Load() != 0 || p.closedWhileRunning.Load() {
				t.Fatal("native lifecycle leak")
			}
		})
	}
}

func TestMaintainFullFallbackAndIncomplete(t *testing.T) {
	p := newControlledWatch()
	_, l := acquireWatch(t, p)
	maintainWatch(t, l, watchedCheck)
	p.mu.Lock()
	p.fingerprint.value++
	p.fullReason = "visibility"
	p.mu.Unlock()
	got := maintainWatch(t, l, watchedCheck)
	if got.Outcome != "full" || got.FullReason != "visibility" || p.scans.Load() != 2 || p.fulls.Load() != 2 {
		t.Fatal(got)
	}
	before := l.Snapshot()
	status := emptyMetadataStatus()
	status[catalog.MetadataProcedures] = catalog.MetadataStatus{State: catalog.MetadataFailed, Err: errors.New("category")}
	p.mu.Lock()
	p.fullStatus = status
	p.mu.Unlock()
	if _, err := l.Maintain(context.Background(), watchedFull); err == nil || l.Snapshot() != before {
		t.Fatal("lost ready category")
	}
	p.mu.Lock()
	p.fullStatus = nil
	p.mu.Unlock()
	disabled := maintainWatch(t, l, MaintenanceRequest{Mode: MaintenanceFull})
	if disabled.Outcome != "full-unwatched" || disabled.BaselineValid || p.scans.Load() != 3 {
		t.Fatal(disabled)
	}
	if maintainWatch(t, l, watchedCheck).Outcome != "full" {
		t.Fatal("disabled full kept baseline")
	}
}
