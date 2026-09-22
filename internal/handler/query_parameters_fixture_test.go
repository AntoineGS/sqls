package handler

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
)

// stubQueryParametersDriverName names a connection whose repository
// implements the optional parameterized capabilities
// (database.ParameterizedRepository, database.ParameterizedReadOnlyQuerier)
// on a type distinct from stubRepository, so those capabilities never leak
// into the stub-interbase-based tests elsewhere in this package —
// concurrency_test.go's readOnlyStubRepository documents the same concern for
// ReadOnlyQuerier.
const stubQueryParametersDriverName = "stub-query-parameters"

// parameterCall records one statement the fixture repository served, on
// whichever route the handler chose for it. Args is copied at record time so
// a caller mutating its slice afterwards cannot corrupt what a test observes,
// and is nil for the unparameterized repository methods: "took the legacy
// method" and "was bound with zero arguments" are different outcomes.
type parameterCall struct {
	SQL   string
	Args  []any
	Route string
}

// The three routes a statement can take. They name the repository method
// family, not the statement kind: a procedure with output takes Route query
// precisely because it must not use a read-only transaction.
const (
	parameterRouteQuery    = "query"
	parameterRouteReadOnly = "readonly"
	parameterRouteExec     = "exec"
)

// parameterBackend is the stub-query-parameters repository's shared,
// concurrency-safe state: every statement it served, every plan it was asked
// to prepare, and optional gates a test uses to hold one call open.
type parameterBackend struct {
	mu           sync.Mutex
	recorded     []parameterCall
	explained    []string
	gates        map[string]*stubGate
	procedures   []*database.ProcedureDesc
	capabilities repositoryCapabilities
}

// repositoryCapabilities selects which optional capabilities the next
// repository the factory builds implements. Each level is a distinct Go type,
// so a repository that is not meant to offer a capability genuinely fails the
// interface assertion instead of pretending to.
type repositoryCapabilities int

const (
	capabilitiesFull         repositoryCapabilities = iota // read-only transaction + both bound capabilities
	capabilitiesReadOnlyOnly                               // read-only transaction, no bound capability
	capabilitiesNone                                       // nothing optional at all
)

func (b *parameterBackend) record(route, sql string, args []any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	call := parameterCall{SQL: sql, Route: route}
	if args != nil {
		call.Args = append([]any(nil), args...)
	}
	b.recorded = append(b.recorded, call)
}

func (b *parameterBackend) calls() []parameterCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]parameterCall(nil), b.recorded...)
}

func (b *parameterBackend) recordExplain(sql string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.explained = append(b.explained, sql)
}

func (b *parameterBackend) explains() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.explained...)
}

// gate parks the next repository call whose statement text contains query,
// until the returned gate is released or the call's context is cancelled.
// Matching on a substring rather than the whole statement lets a test gate a
// statement whose final text the handler rewrote.
func (b *parameterBackend) gate(query string) *stubGate {
	g := &stubGate{
		entered:  make(chan struct{}),
		released: make(chan struct{}),
	}
	b.mu.Lock()
	b.gates[query] = g
	b.mu.Unlock()
	return g
}

// enterGate parks the call when a gate's key matches its statement. It takes
// the first match from an unordered map range, which is deterministic only
// because a test installs at most one gate; installing two gates whose keys
// both match one statement would park an arbitrary one of them.
func (b *parameterBackend) enterGate(ctx context.Context, sql string) error {
	b.mu.Lock()
	var gate *stubGate
	for key, g := range b.gates {
		if strings.Contains(sql, key) {
			gate = g
			break
		}
	}
	b.mu.Unlock()
	if gate == nil {
		return nil
	}
	return gate.enter(ctx)
}

// setProcedures makes the fixture repository a database.CatalogRepository, so
// the worker's catalog pass builds DBCache.Catalog from it and procedure
// routing has descriptors to consult. Call it before the connection is
// established — before tx.addWorkspaceConfig — because the factory runs at
// that point and a method set cannot be changed afterwards.
func (b *parameterBackend) setProcedures(procs []*database.ProcedureDesc) {
	b.mu.Lock()
	b.procedures = procs
	b.mu.Unlock()
}

func (b *parameterBackend) describedProcedures() []*database.ProcedureDesc {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*database.ProcedureDesc(nil), b.procedures...)
}

// withoutBoundCapabilities makes the next repository the factory builds one
// that still offers an unparameterized read-only transaction but neither
// bound capability — the repository shape that must be refused rather than
// downgraded. Same timing rule as setProcedures.
func (b *parameterBackend) withoutBoundCapabilities() {
	b.mu.Lock()
	b.capabilities = capabilitiesReadOnlyOnly
	b.mu.Unlock()
}

// withoutAnyOptionalCapabilities makes the next repository the factory builds
// implement nothing beyond database.DBRepository (plus Explain), so a bound
// read has no read-only transaction to be refused for and falls to the
// plain-parameterized tier instead.
func (b *parameterBackend) withoutAnyOptionalCapabilities() {
	b.mu.Lock()
	b.capabilities = capabilitiesNone
	b.mu.Unlock()
}

func (b *parameterBackend) capabilityLevel() repositoryCapabilities {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.capabilities
}

var currentParameterBackend struct {
	sync.Mutex
	backend *parameterBackend
}

func activeParameterBackend() *parameterBackend {
	currentParameterBackend.Lock()
	defer currentParameterBackend.Unlock()
	return currentParameterBackend.backend
}

// installParameterBackend makes the stub-query-parameters factory's next
// repository record through the returned backend. Call it before
// tx.addWorkspaceConfig, which is what triggers the connection and the
// factory.
func installParameterBackend(t *testing.T) *parameterBackend {
	t.Helper()
	b := &parameterBackend{gates: map[string]*stubGate{}}
	currentParameterBackend.Lock()
	currentParameterBackend.backend = b
	currentParameterBackend.Unlock()
	t.Cleanup(func() {
		currentParameterBackend.Lock()
		currentParameterBackend.backend = nil
		currentParameterBackend.Unlock()
		// Snapshot under the mutex: a gated call may still be reading b.gates
		// through enterGate, and ranging the map unlocked races with gate() —
		// a concurrent map iteration and write is a fatal runtime error, in
		// the very suite whose green -race status gates every task.
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

// stubQueryParametersConnections builds connections resolved by the
// stub-query-parameters factory. Fields are chosen so
// database.NewInterBaseConnectionIdentity succeeds on them.
func stubQueryParametersConnections(aliases ...string) *config.Config {
	conns := make([]*database.DBConfig, 0, len(aliases))
	for _, alias := range aliases {
		conns = append(conns, &database.DBConfig{
			Alias:  alias,
			Driver: stubQueryParametersDriverName,
			User:   "tester",
			Path:   "db.ib",
		})
	}
	return &config.Config{Connections: conns}
}

// parameterFixtureBase serves the unparameterized repository methods and the
// explain capability, and nothing else. Every optional capability lives on a
// wrapper around it, because a wrapper's methods cannot be un-promoted: only
// a type that never had the method can fail the interface assertion the
// handler makes.
type parameterFixtureBase struct {
	*database.MockDBRepository
	db      *sql.DB
	backend *parameterBackend
}

var _ database.ExplainRepository = (*parameterFixtureBase)(nil)

func (r *parameterFixtureBase) Query(ctx context.Context, query string) (*sql.Rows, error) {
	r.backend.record(parameterRouteQuery, query, nil)
	if err := r.backend.enterGate(ctx, query); err != nil {
		return nil, err
	}
	return r.rows(query)
}

func (r *parameterFixtureBase) Exec(ctx context.Context, query string) (sql.Result, error) {
	r.backend.record(parameterRouteExec, query, nil)
	return r.execute(ctx, query)
}

func (r *parameterFixtureBase) ExplainPlan(ctx context.Context, query string) (string, error) {
	r.backend.recordExplain(query)
	return "PLAN (T NATURAL)", nil
}

// rows runs the statement against the stub database/sql driver, which ignores
// arguments and chooses its result set from a marker in the statement text.
// The fetch deliberately uses a background context: the gate, not the
// statement, models cancellation here, so a late cancellation can still be
// observed as a completed statement.
func (r *parameterFixtureBase) rows(query string) (*sql.Rows, error) {
	return r.db.QueryContext(context.Background(), query)
}

func (r *parameterFixtureBase) scan(ctx context.Context, query string) (*database.QueryResult, error) {
	if err := r.backend.enterGate(ctx, query); err != nil {
		return nil, err
	}
	rows, err := r.rows(query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return database.ScanRowsWithTypes(rows, database.RenderOptions{})
}

func (r *parameterFixtureBase) execute(ctx context.Context, query string) (sql.Result, error) {
	if err := r.backend.enterGate(ctx, query); err != nil {
		return nil, err
	}
	// Same rejection stubRepository.Exec models: the driver refuses
	// ExecContext for a procedure it does not know as an Exec-able one, so
	// the unknown-procedure hint has a real failure to explain.
	if name := interBaseProcedureName(query); name != "" {
		procs := r.backend.describedProcedures()
		if len(procs) > 0 && !procedureNamed(procs, name) {
			return nil, errStubExecRejected
		}
	}
	return r.MockDBRepository.Exec(ctx, query)
}

// readOnlyParameterFixture adds the unparameterized read-only transaction.
type readOnlyParameterFixture struct {
	*parameterFixtureBase
}

var _ database.ReadOnlyQuerier = (*readOnlyParameterFixture)(nil)

func (r *readOnlyParameterFixture) QueryReadOnly(ctx context.Context, query string) (*database.QueryResult, error) {
	r.backend.record(parameterRouteReadOnly, query, nil)
	return r.scan(ctx, query)
}

// parameterFixtureRepository adds the two optional bound capabilities, on top
// of the read-only transaction a real InterBase repository also offers.
type parameterFixtureRepository struct {
	*readOnlyParameterFixture
}

var (
	_ database.ParameterizedRepository      = (*parameterFixtureRepository)(nil)
	_ database.ParameterizedReadOnlyQuerier = (*parameterFixtureRepository)(nil)
)

func (r *parameterFixtureRepository) QueryParams(ctx context.Context, query string, args []any) (*sql.Rows, error) {
	r.backend.record(parameterRouteQuery, query, argsOrEmpty(args))
	if err := r.backend.enterGate(ctx, query); err != nil {
		return nil, err
	}
	return r.rows(query)
}

func (r *parameterFixtureRepository) QueryReadOnlyParams(ctx context.Context, query string, args []any) (*database.QueryResult, error) {
	r.backend.record(parameterRouteReadOnly, query, argsOrEmpty(args))
	return r.scan(ctx, query)
}

func (r *parameterFixtureRepository) ExecParams(ctx context.Context, query string, args []any) (sql.Result, error) {
	r.backend.record(parameterRouteExec, query, argsOrEmpty(args))
	return r.execute(ctx, query)
}

// argsOrEmpty keeps a bound call's Args non-nil even when the handler passed
// no arguments, so a recorded call always says which family served it.
func argsOrEmpty(args []any) []any {
	if args == nil {
		return []any{}
	}
	return args
}

// catalogParameterRepository is a distinct type so that a repository built
// without setProcedures never satisfies database.CatalogRepository, which is
// what the no-catalog routing case needs.
type catalogParameterRepository struct {
	*parameterFixtureRepository
}

func (r *catalogParameterRepository) DescribeProcedures(context.Context) ([]*database.ProcedureDesc, error) {
	return r.backend.describedProcedures(), nil
}

func (r *catalogParameterRepository) DescribeViews(context.Context) ([]*database.ViewDesc, error) {
	return nil, nil
}

func (r *catalogParameterRepository) DescribeGenerators(context.Context) ([]*database.GeneratorDesc, error) {
	return nil, nil
}

func (r *catalogParameterRepository) DescribeTriggers(context.Context) ([]*database.TriggerDesc, error) {
	return nil, nil
}

func (r *catalogParameterRepository) DescribeDomains(context.Context) ([]*database.DomainDesc, error) {
	return nil, nil
}

func (r *catalogParameterRepository) DescribeIndexes(context.Context) ([]*database.IndexDesc, error) {
	return nil, nil
}

func (r *catalogParameterRepository) DescribeFunctions(context.Context) ([]*database.FunctionDesc, error) {
	return nil, nil
}

func init() {
	// sql.Register("sqls-handler-stub", ...) happens in concurrency_test.go's
	// init; ordering between the two doesn't matter because sql.Open is only
	// called later, when a test actually connects.
	database.RegisterOpen(stubQueryParametersDriverName, func(*database.DBConfig) (*database.DBConnection, error) {
		db, err := sql.Open("sqls-handler-stub", "")
		if err != nil {
			return nil, err
		}
		return &database.DBConnection{Conn: db, Driver: dialect.DatabaseDriverInterBase}, nil
	})

	database.RegisterFactory(stubQueryParametersDriverName, func(db *sql.DB) database.DBRepository {
		b := activeParameterBackend()
		if b == nil {
			return database.NewMockDBRepository(db)
		}
		base := &parameterFixtureBase{
			MockDBRepository: database.NewMockDBRepository(db).(*database.MockDBRepository),
			db:               db,
			backend:          b,
		}
		level := b.capabilityLevel()
		if level == capabilitiesNone {
			return base
		}
		readOnly := &readOnlyParameterFixture{parameterFixtureBase: base}
		if level == capabilitiesReadOnlyOnly {
			return readOnly
		}
		repository := &parameterFixtureRepository{readOnlyParameterFixture: readOnly}
		if len(b.describedProcedures()) > 0 {
			return &catalogParameterRepository{parameterFixtureRepository: repository}
		}
		return repository
	})
}
