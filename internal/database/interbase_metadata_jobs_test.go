package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"interbase-go/schema"
)

type metadataTxState struct {
	mu                          sync.Mutex
	options                     driver.TxOptions
	begins, rollbacks, queries  int
	opens, closes               int
	queryInTx                   bool
	rollbackErr                 error
	cancelOnRollback            context.CancelFunc
	width                       driver.Value
	queryErr, nextErr, closeErr error
}

type metadataTxDriver struct{ state *metadataTxState }
type metadataTxConn struct {
	state *metadataTxState
	inTx  bool
}
type metadataTx struct{ conn *metadataTxConn }
type metadataRows struct {
	values []driver.Value
	sent   bool
	state  *metadataTxState
}

func (d metadataTxDriver) Open(string) (driver.Conn, error) {
	d.state.mu.Lock()
	d.state.opens++
	d.state.mu.Unlock()
	return &metadataTxConn{state: d.state}, nil
}
func (c *metadataTxConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected Prepare")
}
func (c *metadataTxConn) Close() error {
	c.state.mu.Lock()
	c.state.closes++
	c.state.mu.Unlock()
	return nil
}
func (c *metadataTxConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *metadataTxConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.begins++
	c.state.options = opts
	c.inTx = true
	return &metadataTx{conn: c}, nil
}
func (c *metadataTxConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.queries++
	if c.inTx {
		c.state.queryInTx = true
	}
	if query == interBaseMetadataIdentifierWidthQuery {
		return &metadataRows{values: []driver.Value{c.state.width}, state: c.state}, nil
	}
	if c.state.queryErr != nil {
		return nil, c.state.queryErr
	}
	return &metadataRows{state: c.state}, nil
}
func (tx *metadataTx) Commit() error { tx.conn.inTx = false; return nil }
func (tx *metadataTx) Rollback() error {
	tx.conn.state.mu.Lock()
	defer tx.conn.state.mu.Unlock()
	tx.conn.state.rollbacks++
	tx.conn.inTx = false
	if tx.conn.state.cancelOnRollback != nil {
		tx.conn.state.cancelOnRollback()
	}
	return tx.conn.state.rollbackErr
}
func (r *metadataRows) Columns() []string { return []string{"width"} }
func (r *metadataRows) Close() error      { return r.state.closeErr }
func (r *metadataRows) Next(dest []driver.Value) error {
	if r.state.nextErr != nil {
		return r.state.nextErr
	}
	if r.sent {
		return io.EOF
	}
	r.sent = true
	if len(r.values) == 0 {
		return io.EOF
	}
	dest[0] = r.values[0]
	return nil
}

func openMetadataTxDB(t *testing.T, state *metadataTxState) *sql.DB {
	t.Helper()
	name := "metadata-tx-" + t.Name()
	db := sql.OpenDB(metadataTxConnector{drv: metadataTxDriver{state: state}, name: name})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type metadataTxConnector struct {
	drv  driver.Driver
	name string
}

func (c metadataTxConnector) Connect(context.Context) (driver.Conn, error) { return c.drv.Open(c.name) }
func (c metadataTxConnector) Driver() driver.Driver                        { return c.drv }

func TestInterBaseMetadataTransactionOwnsReadOnlySnapshot(t *testing.T) {
	state := &metadataTxState{width: int64(127)}
	db := &InterBaseDBRepository{Conn: openMetadataTxDB(t, state)}
	called := false
	_, err := db.runMetadataRead(context.Background(), func(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error) {
		called = true
		if state.opens != 1 {
			t.Fatalf("open connections while reading = %d, want one", state.opens)
		}
		if width != 127 {
			t.Fatalf("width = %d, want 127", width)
		}
		rows, err := q.QueryContext(ctx, "read-in-job")
		if err != nil {
			return MetadataPatch{}, err
		}
		return MetadataPatch{}, rows.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("read callback was not called")
	}
	if state.options.ReadOnly != true || state.options.Isolation != driver.IsolationLevel(sql.LevelSnapshot) {
		t.Fatalf("BeginTx options = %+v", state.options)
	}
	if state.opens != 1 || state.begins != 1 || state.rollbacks != 1 || !state.queryInTx || state.queries != 2 {
		t.Fatalf("tx state = %+v", state)
	}
}

func TestInterBaseMetadataTransactionRollbackFailureDiscardsPatch(t *testing.T) {
	state := &metadataTxState{width: int64(127), rollbackErr: errors.New("rollback failed")}
	db := &InterBaseDBRepository{Conn: openMetadataTxDB(t, state)}
	patch, err := db.runMetadataRead(context.Background(), func(context.Context, schema.Queryer, int) (MetadataPatch, error) { return MetadataPatch{Count: 1}, nil })
	if err == nil || patch.Count != 0 {
		t.Fatalf("patch=%+v err=%v", patch, err)
	}
}

func TestInterBaseMetadataTransactionCancellationDuringRollbackDiscardsPatch(t *testing.T) {
	state := &metadataTxState{width: int64(127), rollbackErr: sql.ErrTxDone}
	db := &InterBaseDBRepository{Conn: openMetadataTxDB(t, state)}
	ctx, cancel := context.WithCancel(context.Background())
	state.cancelOnRollback = cancel
	patch, err := db.runMetadataRead(ctx, func(context.Context, schema.Queryer, int) (MetadataPatch, error) {
		return MetadataPatch{Count: 9}, nil
	})
	if !errors.Is(err, context.Canceled) || patch.Count != 0 {
		t.Fatalf("patch=%+v err=%v, want empty patch and context.Canceled", patch, err)
	}
	if state.rollbacks != 1 {
		t.Fatalf("rollbacks = %d, want 1", state.rollbacks)
	}
}

func TestInterBaseMetadataTransactionPanicRollsBack(t *testing.T) {
	state := &metadataTxState{width: int64(127)}
	db := &InterBaseDBRepository{Conn: openMetadataTxDB(t, state)}
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		_, _ = db.runMetadataRead(context.Background(), func(context.Context, schema.Queryer, int) (MetadataPatch, error) {
			panic("injected read panic")
		})
	}()
	if !panicked {
		t.Fatal("read callback panic did not propagate")
	}
	if state.rollbacks != 1 {
		t.Fatalf("rollbacks = %d, want 1 after panic", state.rollbacks)
	}
}

func TestInterBaseMetadataTransactionFaultsDiscardPatch(t *testing.T) {
	failure := errors.New("injected metadata read fault")
	tests := []struct {
		name  string
		state *metadataTxState
		read  interBaseMetadataRead
	}{
		{name: "query", state: &metadataTxState{width: int64(127), queryErr: failure}, read: func(ctx context.Context, q schema.Queryer, _ int) (MetadataPatch, error) {
			rows, err := q.QueryContext(ctx, "catalog query")
			if err != nil {
				return MetadataPatch{}, err
			}
			return MetadataPatch{Count: 1}, rows.Close()
		}},
		{name: "scan", state: &metadataTxState{width: "not-an-integer"}, read: func(context.Context, schema.Queryer, int) (MetadataPatch, error) { return MetadataPatch{Count: 1}, nil }},
		{name: "iteration", state: &metadataTxState{width: int64(127), nextErr: failure}, read: func(context.Context, schema.Queryer, int) (MetadataPatch, error) { return MetadataPatch{Count: 1}, nil }},
		{name: "close", state: &metadataTxState{width: int64(127), closeErr: failure}, read: func(context.Context, schema.Queryer, int) (MetadataPatch, error) { return MetadataPatch{Count: 1}, nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := &InterBaseDBRepository{Conn: openMetadataTxDB(t, test.state)}
			patch, err := db.runMetadataRead(context.Background(), test.read)
			if err == nil || patch.Count != 0 || patch.Cache != nil {
				t.Fatalf("patch=%+v err=%v; want error and discarded patch", patch, err)
			}
		})
	}
}

// A serialized plan sharing a one-connection pool must never acquire a second
// connection from inside a job while its read-only transaction owns the first.
func TestInterBaseMetadataPlanCompletesWithOneConnection(t *testing.T) {
	db := openInterBaseScalableFixture(t, 1)
	db.SetMaxOpenConns(1)
	repo := interBaseMetadataPlanWithParallelism{InterBaseDBRepository: &InterBaseDBRepository{Conn: db}, parallelism: 1}
	loader := NewMetadataLoader()
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	load, err := loader.Start(ctx, 1, repo)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-load.Done:
	case <-ctx.Done():
		t.Fatal("metadata load deadlocked acquiring a nested connection:", ctx.Err())
	}
	if got := loader.Snapshot(); got.Degraded() {
		t.Fatalf("healthy fixture degraded: %+v", got.Status)
	}
}

type interBaseMetadataPlanWithParallelism struct {
	*InterBaseDBRepository
	parallelism int
}

func (r interBaseMetadataPlanWithParallelism) MetadataPlan() MetadataPlan {
	plan := r.InterBaseDBRepository.MetadataPlan()
	plan.Parallelism = r.parallelism
	return plan
}

func TestInterBaseMetadataJobsLeaveInteractiveConnectionCapacity(t *testing.T) {
	db := openInterBaseScalableFixture(t, 1)
	db.SetMaxOpenConns(5)
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	jobs := make([]MetadataJob, 3)
	for i, kind := range []MetadataKind{MetadataRelations, MetadataColumnsCurrent, MetadataProcedures} {
		jobs[i] = MetadataJob{Kind: kind, Run: func(ctx context.Context, _ *DBCache) (MetadataPatch, error) {
			conn, err := db.Conn(ctx)
			if err != nil {
				return MetadataPatch{}, err
			}
			defer conn.Close()
			entered <- struct{}{}
			select {
			case <-release:
				return MetadataPatch{}, nil
			case <-ctx.Done():
				return MetadataPatch{}, ctx.Err()
			}
		}}
	}
	loader := NewMetadataLoader()
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	load, err := loader.Start(context.Background(), 1, metadataRepo(MetadataPlan{Parallelism: 3, Jobs: jobs}))
	if err != nil {
		t.Fatal(err)
	}
	for range jobs {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("three metadata jobs did not acquire their connections")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	interactive, err := db.Conn(ctx)
	if err != nil {
		close(release)
		t.Fatalf("fourth interactive connection blocked with max-open=5: %v", err)
	}
	_ = interactive.Close()
	close(release)
	select {
	case <-load.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("gated jobs did not finish after release")
	}
}

func TestInterBaseMetadataActualPlanLeavesInteractiveSelectCapacity(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 3)
	fault := &interBaseFixtureFault{
		match: "SELECT MAX(f.RDB$FIELD_LENGTH)", stage: "gate", gate: release, entered: entered,
	}
	db := openInterBaseScalableFixtureWithFault(t, 1, fault)
	db.SetMaxOpenConns(5)
	loader := NewMetadataLoader()
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	load, err := loader.Start(ctx, 1, &InterBaseDBRepository{Conn: db})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			close(release)
			t.Fatalf("actual metadata plan did not gate three query jobs: %v", ctx.Err())
		}
	}
	var value int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&value); err != nil || value != 1 {
		close(release)
		t.Fatalf("interactive SELECT 1 with three actual jobs gated: value=%d err=%v", value, err)
	}
	close(release)
	select {
	case <-load.Done:
	case <-ctx.Done():
		t.Fatalf("actual plan did not drain after releasing gates: %v; statuses=%+v", ctx.Err(), loader.Snapshot().Status)
	}
}

func TestInterBaseMetadataActualPlanReaderQueryFaultsAreIsolated(t *testing.T) {
	// Schemas is deliberately a local synthetic constant in MetadataPlan and
	// has no query to fault-inject. Every DB-backed planned category is matched
	// against its reader's real SQL in the fixture driver's Prepare/Query path.
	cases := []struct {
		kind  MetadataKind
		match string
	}{
		{MetadataRelations, "FROM RDB$RELATIONS r\nWHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0\nORDER BY"},
		{MetadataColumnsCurrent, "WHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0\nORDER BY rf.RDB$RELATION_NAME"},
		{MetadataPrimaryKeys, "pk.RDB$CONSTRAINT_TYPE = 'PRIMARY KEY'"},
		{MetadataForeignKeys, "fk.RDB$CONSTRAINT_TYPE = 'FOREIGN KEY'"},
		{MetadataViews, "r.RDB$VIEW_BLR IS NOT NULL"},
		{MetadataIndexes, "FROM RDB$INDICES i\nLEFT JOIN RDB$RELATION_CONSTRAINTS"},
		{MetadataProcedures, "FROM RDB$PROCEDURES p"},
		{MetadataFunctions, "FROM RDB$FUNCTIONS f"},
		{MetadataGenerators, "RDB$GENERATORS"},
		{MetadataDomains, "FROM RDB$FIELDS f\n"},
		{MetadataTriggers, "RDB$TRIGGERS"},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			failure := errors.New("injected actual-plan reader query failure")
			db := openInterBaseScalableFixtureWithFault(t, 1, &interBaseFixtureFault{match: tc.match, stage: "query", err: failure})
			loader := NewMetadataLoader()
			t.Cleanup(loader.Stop)
			loader.Reset(1)
			load, err := loader.Start(context.Background(), 1, &InterBaseDBRepository{Conn: db})
			if err != nil {
				t.Fatal(err)
			}
			waitLoad(t, load)
			snapshot := loader.Snapshot()
			if got := snapshot.Status[tc.kind].State; got != MetadataFailed {
				t.Fatalf("%s state = %s; match %q failed to reach real reader SQL", tc.kind, got, tc.match)
			}
			if snapshot.Cache.MetadataReady(tc.kind) {
				t.Fatalf("failed %s category was published ready", tc.kind)
			}
			if count := interBaseMetadataFragmentCount(snapshot.Cache, tc.kind); count != 0 {
				t.Fatalf("failed %s category published %d fragment entries", tc.kind, count)
			}
			for _, job := range (&InterBaseDBRepository{}).MetadataPlan().Jobs {
				if job.Kind != tc.kind && snapshot.Status[job.Kind].State != MetadataReady {
					t.Errorf("independent sibling %s state = %s, want ready", job.Kind, snapshot.Status[job.Kind].State)
				}
			}
		})
	}
}

func interBaseMetadataFragmentCount(cache *DBCache, kind MetadataKind) int {
	if cache == nil {
		return 0
	}
	switch kind {
	case MetadataSchemas:
		return len(cache.Schemas)
	case MetadataRelations:
		count := 0
		for _, tables := range cache.SchemaTables {
			count += len(tables)
		}
		return count
	case MetadataColumnsCurrent:
		count := 0
		for _, columns := range cache.ColumnsWithParent {
			count += len(columns)
		}
		return count
	case MetadataPrimaryKeys:
		return len(cache.PrimaryKeyColumns)
	case MetadataForeignKeys:
		count := 0
		for _, byReferencedTable := range cache.ForeignKeys {
			for _, keys := range byReferencedTable {
				count += len(keys)
			}
		}
		return count
	case MetadataViews:
		if cache.Catalog != nil {
			return len(cache.Catalog.Views)
		}
	case MetadataIndexes:
		if cache.Catalog != nil {
			return len(cache.Catalog.Indexes)
		}
	case MetadataProcedures:
		if cache.Catalog != nil {
			return len(cache.Catalog.Procedures)
		}
	case MetadataFunctions:
		if cache.Catalog != nil {
			return len(cache.Catalog.Functions)
		}
	case MetadataGenerators:
		if cache.Catalog != nil {
			return len(cache.Catalog.Generators)
		}
	case MetadataDomains:
		if cache.Catalog != nil {
			return len(cache.Catalog.Domains)
		}
	case MetadataTriggers:
		if cache.Catalog != nil {
			return len(cache.Catalog.Triggers)
		}
	}
	return 0
}

func TestInterBaseMetadataActualPlanDataRowsFaultStagesDiscardCategory(t *testing.T) {
	for _, tc := range []struct {
		kind  MetadataKind
		match string
		view  bool
	}{
		{MetadataRelations, "FROM RDB$RELATIONS r\nWHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0\nORDER BY", false},
		{MetadataViews, "r.RDB$VIEW_BLR IS NOT NULL", true},
	} {
		for _, stage := range []string{"scan", "iterate", "close"} {
			t.Run(string(tc.kind)+"/"+stage, func(t *testing.T) {
				failure := errors.New("injected actual reader rows " + stage + " failure")
				fault := &interBaseFixtureFault{match: tc.match, stage: stage, err: failure}
				db := openInterBaseScalableFixtureWithFault(t, 1, fault)
				if tc.view {
					insertMetadataViewFixtures(t, db, 1)
				}
				loader := NewMetadataLoader()
				t.Cleanup(loader.Stop)
				loader.Reset(1)
				load, err := loader.Start(context.Background(), 1, &InterBaseDBRepository{Conn: db})
				if err != nil {
					t.Fatal(err)
				}
				waitLoad(t, load)
				snapshot := loader.Snapshot()
				if got := snapshot.Status[tc.kind].State; got != MetadataFailed {
					t.Fatalf("actual %s reader %s injection status = %s, want failed", tc.kind, stage, got)
				}
				if snapshot.Status[MetadataProcedures].State != MetadataReady {
					t.Fatal("data-row error suppressed independent procedure reader")
				}
			})
		}
	}
}

func TestInterBaseMetadataJobPanicDoesNotSuppressIndependentSibling(t *testing.T) {
	loader := NewMetadataLoader()
	t.Cleanup(loader.Stop)
	loader.Reset(1)
	repo := metadataRepo(MetadataPlan{Parallelism: 2, Jobs: []MetadataJob{
		{Kind: MetadataViews, Run: func(context.Context, *DBCache) (MetadataPatch, error) { panic("ordinary panic") }},
		{Kind: MetadataProcedures, Run: func(context.Context, *DBCache) (MetadataPatch, error) {
			return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{Procedures: map[string]*ProcedureDesc{"P": {Name: "P"}}}}, Count: 1}, nil
		}},
	}})
	load, err := loader.Start(context.Background(), 1, repo)
	if err != nil {
		t.Fatal(err)
	}
	waitLoad(t, load)
	snapshot := loader.Snapshot()
	if snapshot.Status[MetadataViews].State != MetadataFailed {
		t.Fatalf("panic status = %s, want failed", snapshot.Status[MetadataViews].State)
	}
	if snapshot.Status[MetadataProcedures].State != MetadataReady {
		t.Fatalf("independent procedure status = %s, want ready", snapshot.Status[MetadataProcedures].State)
	}
	if _, ok := snapshot.Cache.Procedure("P"); !ok {
		t.Fatal("panic suppressed independent procedure result")
	}
}

func TestInterBaseMetadataFailureIsIsolatedForEveryIndependentKind(t *testing.T) {
	kinds := []MetadataKind{MetadataSchemas, MetadataRelations, MetadataColumnsCurrent, MetadataProcedures, MetadataPrimaryKeys, MetadataViews, MetadataIndexes, MetadataForeignKeys, MetadataFunctions, MetadataGenerators, MetadataDomains, MetadataTriggers}
	for _, failed := range kinds {
		t.Run(string(failed), func(t *testing.T) {
			jobs := make([]MetadataJob, 0, len(kinds))
			for _, kind := range kinds {
				kind := kind
				jobs = append(jobs, MetadataJob{Kind: kind, Run: func(context.Context, *DBCache) (MetadataPatch, error) {
					if kind == failed {
						return MetadataPatch{}, errors.New("injected independent job failure")
					}
					return MetadataPatch{Cache: emptyMetadataPatch(kind)}, nil
				}})
			}
			loader := NewMetadataLoader()
			t.Cleanup(loader.Stop)
			loader.Reset(1)
			load, err := loader.Start(context.Background(), 1, metadataRepo(MetadataPlan{Parallelism: 3, Jobs: jobs}))
			if err != nil {
				t.Fatal(err)
			}
			waitLoad(t, load)
			snapshot := loader.Snapshot()
			for _, kind := range kinds {
				want := MetadataReady
				if kind == failed {
					want = MetadataFailed
				}
				if got := snapshot.Status[kind].State; got != want {
					t.Errorf("%s state = %s, want %s", kind, got, want)
				}
			}
		})
	}
}

func emptyMetadataPatch(kind MetadataKind) *DBCache {
	switch kind {
	case MetadataSchemas:
		return &DBCache{Schemas: map[string]string{}}
	case MetadataRelations:
		return &DBCache{SchemaTables: map[string][]string{}}
	case MetadataColumnsCurrent:
		return &DBCache{ColumnsWithParent: map[string][]*ColumnDesc{}}
	case MetadataPrimaryKeys:
		return &DBCache{PrimaryKeyColumns: map[string]map[string]struct{}{}}
	case MetadataForeignKeys:
		return &DBCache{ForeignKeys: map[string]map[string][]*ForeignKey{}}
	case MetadataViews:
		return &DBCache{Catalog: &CatalogCache{Views: map[string]*ViewDesc{}}}
	case MetadataProcedures:
		return &DBCache{Catalog: &CatalogCache{Procedures: map[string]*ProcedureDesc{}}}
	case MetadataIndexes:
		return &DBCache{Catalog: &CatalogCache{Indexes: map[string]*IndexDesc{}}}
	case MetadataFunctions:
		return &DBCache{Catalog: &CatalogCache{Functions: map[string]*FunctionDesc{}}}
	case MetadataGenerators:
		return &DBCache{Catalog: &CatalogCache{Generators: map[string]*GeneratorDesc{}}}
	case MetadataDomains:
		return &DBCache{Catalog: &CatalogCache{Domains: map[string]*DomainDesc{}}}
	case MetadataTriggers:
		return &DBCache{Catalog: &CatalogCache{Triggers: map[string]*TriggerDesc{}}}
	default:
		panic("unhandled metadata kind " + string(kind))
	}
}
