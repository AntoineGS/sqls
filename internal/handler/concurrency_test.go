package handler

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
)

const stubDriverName = "stub"

// A minimal database/sql driver so the stub repository can hand the handler a
// real *sql.Rows. Every query returns one row with a single column named "n".
type stubSQLDriver struct{}

func (stubSQLDriver) Open(string) (driver.Conn, error) { return stubSQLConn{}, nil }

type stubSQLConn struct{}

func (stubSQLConn) Prepare(query string) (driver.Stmt, error) { return stubSQLStmt{query: query}, nil }
func (stubSQLConn) Close() error                              { return nil }
func (stubSQLConn) Begin() (driver.Tx, error)                 { return nil, errors.New("stub: no transactions") }

type stubSQLStmt struct{ query string }

func (stubSQLStmt) Close() error  { return nil }
func (stubSQLStmt) NumInput() int { return 0 }
func (stubSQLStmt) Exec([]driver.Value) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

// Query serves one of three fixed result sets, chosen by a marker in the
// statement text. The markers are ordinary identifiers so the statement still
// parses and still reaches the repository unchanged.
func (s stubSQLStmt) Query([]driver.Value) (driver.Rows, error) {
	switch {
	case strings.Contains(s.query, failBlobFetchMarker):
		return &stubSQLRows{names: []string{"n", "b"}, types: []string{"INTEGER", "BLOB"}, rows: 2, failAfter: true}, nil
	case strings.Contains(s.query, failFetchMarker):
		return &stubSQLRows{names: []string{"n"}, types: []string{"INTEGER"}, rows: 2, failAfter: true}, nil
	}
	return &stubSQLRows{names: []string{"n"}, types: []string{"INTEGER"}, rows: 1}, nil
}

const (
	failFetchMarker     = "fail_fetch"
	failBlobFetchMarker = "fail_blob_fetch"
)

var errStubFetch = errors.New("interbase: BLOB result exceeds the materialization limit")

type stubSQLRows struct {
	names     []string
	types     []string
	rows      int
	failAfter bool
	sent      int
}

func (r *stubSQLRows) Columns() []string { return r.names }
func (r *stubSQLRows) Close() error      { return nil }

func (r *stubSQLRows) ColumnTypeDatabaseTypeName(index int) string { return r.types[index] }

func (r *stubSQLRows) Next(dest []driver.Value) error {
	if r.sent >= r.rows {
		if r.failAfter {
			return errStubFetch
		}
		return io.EOF
	}
	for i := range dest {
		if r.types[i] == "BLOB" {
			dest[i] = []byte("blob")
			continue
		}
		dest[i] = int64(42)
	}
	r.sent++
	return nil
}

// stubGate parks one repository method so a test can hold a command in flight.
type stubGate struct {
	entered      chan struct{}
	released     chan struct{}
	ignoreCancel bool
	// releaseOnce makes release idempotent: a test releases its own gate and
	// the backend cleanup releases every gate, so most gates are released
	// twice.
	releaseOnce   sync.Once
	mu            sync.Mutex
	sawCancelled  bool
	enteredClosed bool
}

func (g *stubGate) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("gated repository method was never entered")
	}
}

func (g *stubGate) release() {
	g.releaseOnce.Do(func() { close(g.released) })
}

func (g *stubGate) contextWasCancelled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sawCancelled
}

func (g *stubGate) enter(ctx context.Context) error {
	g.mu.Lock()
	if !g.enteredClosed {
		g.enteredClosed = true
		close(g.entered)
	}
	g.mu.Unlock()

	select {
	case <-g.released:
		return nil
	case <-ctx.Done():
		g.mu.Lock()
		g.sawCancelled = true
		g.mu.Unlock()
		if g.ignoreCancel {
			return nil
		}
		return ctx.Err()
	}
}

type stubQuery struct {
	db   *sql.DB
	text string
}

type stubBackend struct {
	mu             sync.Mutex
	gates          map[string]*stubGate
	served         []stubQuery
	execServed     []string
	openedDB       []*sql.DB
	readOnly       bool
	readOnlyServed []string
	procedures     []*database.ProcedureDesc
}

func (b *stubBackend) newGate(method string, ignoreCancel bool) *stubGate {
	g := &stubGate{
		entered:      make(chan struct{}),
		released:     make(chan struct{}),
		ignoreCancel: ignoreCancel,
	}
	b.mu.Lock()
	b.gates[method] = g
	b.mu.Unlock()
	return g
}

func (b *stubBackend) gate(method string) *stubGate {
	return b.newGate(method, false)
}

func (b *stubBackend) gateIgnoringCancel(method string) *stubGate {
	return b.newGate(method, true)
}

func (b *stubBackend) enter(ctx context.Context, method string) error {
	b.mu.Lock()
	g := b.gates[method]
	b.mu.Unlock()
	if g == nil {
		return nil
	}
	return g.enter(ctx)
}

func (b *stubBackend) recordQuery(db *sql.DB, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.served = append(b.served, stubQuery{db: db, text: text})
}

func (b *stubBackend) queries() []stubQuery {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]stubQuery(nil), b.served...)
}

func (b *stubBackend) recordExec(query string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.execServed = append(b.execServed, query)
}

func (b *stubBackend) execs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.execServed...)
}

// enableReadOnlyQuerier makes the next repository the factory builds implement
// database.ReadOnlyQuerier. Call it before the connection is established —
// before tx.addWorkspaceConfig — because the factory runs at that point and a
// method set cannot be changed afterwards.
func (b *stubBackend) enableReadOnlyQuerier() {
	b.mu.Lock()
	b.readOnly = true
	b.mu.Unlock()
}

func (b *stubBackend) readOnlyEnabled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.readOnly
}

func (b *stubBackend) recordReadOnlyQuery(text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.readOnlyServed = append(b.readOnlyServed, text)
}

func (b *stubBackend) readOnlyQueries() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.readOnlyServed...)
}

func (b *stubBackend) recordOpen(db *sql.DB) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.openedDB = append(b.openedDB, db)
}

func (b *stubBackend) opened() []*sql.DB {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*sql.DB(nil), b.openedDB...)
}

// setProcedures makes the stub repository a database.CatalogRepository, so the
// worker's catalog pass builds DBCache.Catalog from it. Call it before
// tx.addWorkspaceConfig, which is what triggers the connection and the cache
// build.
func (b *stubBackend) setProcedures(procs []*database.ProcedureDesc) {
	b.mu.Lock()
	b.procedures = procs
	b.mu.Unlock()
}

func (b *stubBackend) describedProcedures() []*database.ProcedureDesc {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*database.ProcedureDesc(nil), b.procedures...)
}

// stubRepository is a MockDBRepository whose Query, CurrentSchema and Databases
// can be parked by the backend's gates.
type stubRepository struct {
	*database.MockDBRepository
	backend *stubBackend
	db      *sql.DB
}

func (r *stubRepository) Query(ctx context.Context, query string) (*sql.Rows, error) {
	if err := r.backend.enter(ctx, "Query"); err != nil {
		return nil, err
	}
	r.backend.recordQuery(r.db, query)
	// The gate, not the statement, models cancellation here: once the gate lets
	// the call through, the result is produced unconditionally so a late
	// cancellation can be observed as a completed statement.
	return r.db.QueryContext(context.Background(), query)
}

// errStubExecRejected simulates the driver rejecting ExecContext for a
// procedure that needs to run through Query instead. MockDBRepository.Exec
// otherwise always succeeds regardless of statement text, so without this the
// unknown-procedure routing test could never observe the failure its hint is
// designed to explain.
var errStubExecRejected = errors.New("interbase: statement requires execution as a query")

func (r *stubRepository) Exec(ctx context.Context, query string) (sql.Result, error) {
	r.backend.recordExec(query)
	if name := interBaseProcedureName(query); name != "" {
		procs := r.backend.describedProcedures()
		if len(procs) > 0 && !procedureNamed(procs, name) {
			return nil, errStubExecRejected
		}
	}
	return r.MockDBRepository.Exec(ctx, query)
}

func procedureNamed(procs []*database.ProcedureDesc, name string) bool {
	for _, p := range procs {
		if strings.EqualFold(p.Name, name) {
			return true
		}
	}
	return false
}

func (r *stubRepository) CurrentSchema(ctx context.Context) (string, error) {
	if err := r.backend.enter(ctx, "CurrentSchema"); err != nil {
		return "", err
	}
	return r.MockDBRepository.CurrentSchema(ctx)
}

func (r *stubRepository) Databases(ctx context.Context) ([]string, error) {
	if err := r.backend.enter(ctx, "Databases"); err != nil {
		return nil, err
	}
	return r.MockDBRepository.Databases(ctx)
}

// readOnlyStubRepository is a distinct type rather than a method on
// stubRepository: if stubRepository itself satisfied ReadOnlyQuerier, every
// existing command test would silently change path.
type readOnlyStubRepository struct {
	*stubRepository
}

func (r *readOnlyStubRepository) QueryReadOnly(ctx context.Context, query string) (*database.QueryResult, error) {
	rows, err := r.stubRepository.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	r.backend.recordReadOnlyQuery(query)
	return database.ScanRowsWithTypes(rows, database.RenderOptions{})
}

// catalogStubRepository is a distinct type so that a repository built without
// setProcedures never satisfies database.CatalogRepository — which is what the
// HasCatalog()-false degradation test needs.
type catalogStubRepository struct {
	*stubRepository
}

func (r *catalogStubRepository) DescribeProcedures(context.Context) ([]*database.ProcedureDesc, error) {
	return r.backend.describedProcedures(), nil
}

func (r *catalogStubRepository) DescribeViews(context.Context) ([]*database.ViewDesc, error) {
	return nil, nil
}

func (r *catalogStubRepository) DescribeGenerators(context.Context) ([]*database.GeneratorDesc, error) {
	return nil, nil
}

func (r *catalogStubRepository) DescribeTriggers(context.Context) ([]*database.TriggerDesc, error) {
	return nil, nil
}

func (r *catalogStubRepository) DescribeDomains(context.Context) ([]*database.DomainDesc, error) {
	return nil, nil
}

func (r *catalogStubRepository) DescribeIndexes(context.Context) ([]*database.IndexDesc, error) {
	return nil, nil
}

func (r *catalogStubRepository) DescribeFunctions(context.Context) ([]*database.FunctionDesc, error) {
	return nil, nil
}

var currentStubBackend struct {
	sync.Mutex
	backend *stubBackend
}

func activeStubBackend() *stubBackend {
	currentStubBackend.Lock()
	defer currentStubBackend.Unlock()
	return currentStubBackend.backend
}

func installStubBackend(t *testing.T) *stubBackend {
	t.Helper()
	b := &stubBackend{gates: map[string]*stubGate{}}
	currentStubBackend.Lock()
	currentStubBackend.backend = b
	currentStubBackend.Unlock()
	t.Cleanup(func() {
		currentStubBackend.Lock()
		currentStubBackend.backend = nil
		currentStubBackend.Unlock()
		// Snapshot under the mutex: a gated call may still be reading b.gates
		// through enter, and ranging the map unlocked races with gate() — a
		// concurrent map iteration and write is a fatal runtime error, in the
		// very suite whose green -race status gates every task.
		b.mu.Lock()
		gates := make([]*stubGate, 0, len(b.gates))
		for _, g := range b.gates {
			gates = append(gates, g)
		}
		b.mu.Unlock()
		for _, g := range gates {
			g.release()
		}
	})
	return b
}

func stubConnections(aliases ...string) *config.Config {
	conns := make([]*database.DBConfig, 0, len(aliases))
	for _, alias := range aliases {
		conns = append(conns, &database.DBConfig{
			Alias:          alias,
			Driver:         stubDriverName,
			DataSourceName: "",
		})
	}
	return &config.Config{Connections: conns}
}

const stubInterBaseDriverName = "stub-interbase"

// stubInterBaseConnections builds connections whose repository is the stub but
// whose DBConnection.Driver is InterBase, so s.parserDriver() reports InterBase
// while CreateRepository still resolves to the stub factory. The real InterBase
// opener and factory are registered by interbase_common.go's init and cannot be
// replaced — RegisterOpen panics on a duplicate name.
func stubInterBaseConnections(aliases ...string) *config.Config {
	conns := make([]*database.DBConfig, 0, len(aliases))
	for _, alias := range aliases {
		conns = append(conns, &database.DBConfig{
			Alias:          alias,
			Driver:         stubInterBaseDriverName,
			DataSourceName: "",
		})
	}
	return &config.Config{Connections: conns}
}

// waitForCatalog polls until the worker's asynchronous catalog pass lands.
// addWorkspaceConfig reaches ReCache, which only signals the worker goroutine
// (worker.go:122-129); issuing a command straight afterwards races that
// goroutine, so routing tests that depend on HasCatalog() must wait first.
// database/cache_test.go defines an equivalent helper, but it is unexported in
// the database package's test binary and unreachable from here.
func waitForCatalog(t *testing.T, worker *database.Worker) *database.DBCache {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cache := worker.Cache(); cache.HasCatalog() {
			return cache
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the worker never swapped in an extended catalog")
	return nil
}

func init() {
	sql.Register("sqls-handler-stub", stubSQLDriver{})

	database.RegisterOpen(stubDriverName, func(*database.DBConfig) (*database.DBConnection, error) {
		db, err := sql.Open("sqls-handler-stub", "")
		if err != nil {
			return nil, err
		}
		if b := activeStubBackend(); b != nil {
			b.recordOpen(db)
		}
		return &database.DBConnection{Conn: db, Driver: stubDriverName}, nil
	})

	database.RegisterFactory(stubDriverName, func(db *sql.DB) database.DBRepository {
		b := activeStubBackend()
		if b == nil {
			return database.NewMockDBRepository(db)
		}
		repository := &stubRepository{
			MockDBRepository: database.NewMockDBRepository(db).(*database.MockDBRepository),
			backend:          b,
			db:               db,
		}
		if b.readOnlyEnabled() {
			return &readOnlyStubRepository{stubRepository: repository}
		}
		if len(b.describedProcedures()) > 0 {
			return &catalogStubRepository{stubRepository: repository}
		}
		return repository
	})

	database.RegisterOpen(stubInterBaseDriverName, func(*database.DBConfig) (*database.DBConnection, error) {
		db, err := sql.Open("sqls-handler-stub", "")
		if err != nil {
			return nil, err
		}
		if b := activeStubBackend(); b != nil {
			b.recordOpen(db)
		}
		return &database.DBConnection{Conn: db, Driver: dialect.DatabaseDriverInterBase}, nil
	})

	database.RegisterFactory(stubInterBaseDriverName, func(db *sql.DB) database.DBRepository {
		b := activeStubBackend()
		if b == nil {
			return database.NewMockDBRepository(db)
		}
		repository := &stubRepository{
			MockDBRepository: database.NewMockDBRepository(db).(*database.MockDBRepository),
			backend:          b,
			db:               db,
		}
		if len(b.describedProcedures()) > 0 {
			return &catalogStubRepository{stubRepository: repository}
		}
		return repository
	})
}
