package handler

import (
	"context"
	"database/sql"
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
// ReadOnlyQuerier. This task only proves that discovery dispatches correctly
// through JSON-RPC and never reaches this fixture; later parameter-submission
// and stale-routing tests reuse it.
const stubQueryParametersDriverName = "stub-query-parameters"

// queryParametersCall records one bound-argument call the fixture served.
// args is copied at record time so a caller mutating its slice afterward
// cannot corrupt what a test observes.
type queryParametersCall struct {
	query string
	args  []any
}

// queryParametersRecorder is the stub-query-parameters repository's shared,
// concurrency-safe state: every parameterized call it served, and an
// optional gate a later stale-submission test can use to hold a call open
// until it is ready to let it complete.
type queryParametersRecorder struct {
	mu    sync.Mutex
	calls []queryParametersCall
	gate  chan struct{}
}

func (r *queryParametersRecorder) record(query string, args []any) {
	r.mu.Lock()
	r.calls = append(r.calls, queryParametersCall{query: query, args: append([]any(nil), args...)})
	gate := r.gate
	r.mu.Unlock()
	if gate != nil {
		<-gate
	}
}

func (r *queryParametersRecorder) recordedCalls() []queryParametersCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]queryParametersCall(nil), r.calls...)
}

// setGate installs a channel a later call to record blocks on until it is
// closed, letting a test hold a submission in flight.
func (r *queryParametersRecorder) setGate(gate chan struct{}) {
	r.mu.Lock()
	r.gate = gate
	r.mu.Unlock()
}

var currentQueryParametersRecorder struct {
	sync.Mutex
	recorder *queryParametersRecorder
}

func activeQueryParametersRecorder() *queryParametersRecorder {
	currentQueryParametersRecorder.Lock()
	defer currentQueryParametersRecorder.Unlock()
	return currentQueryParametersRecorder.recorder
}

// installQueryParametersRecorder makes the stub-query-parameters factory's
// next repository record through the returned recorder. Call it before
// tx.addWorkspaceConfig, which is what triggers the connection and the
// factory.
func installQueryParametersRecorder(t *testing.T) *queryParametersRecorder {
	t.Helper()
	r := &queryParametersRecorder{}
	currentQueryParametersRecorder.Lock()
	currentQueryParametersRecorder.recorder = r
	currentQueryParametersRecorder.Unlock()
	t.Cleanup(func() {
		currentQueryParametersRecorder.Lock()
		currentQueryParametersRecorder.recorder = nil
		currentQueryParametersRecorder.Unlock()
	})
	return r
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

// queryParametersFixtureRepository is the stub-query-parameters factory's
// repository: a MockDBRepository whose parameterized methods record every
// call through a queryParametersRecorder instead of reaching a real driver.
type queryParametersFixtureRepository struct {
	*database.MockDBRepository
	db       *sql.DB
	recorder *queryParametersRecorder
}

var (
	_ database.ParameterizedRepository      = (*queryParametersFixtureRepository)(nil)
	_ database.ParameterizedReadOnlyQuerier = (*queryParametersFixtureRepository)(nil)
)

func (r *queryParametersFixtureRepository) ExecParams(ctx context.Context, query string, args []any) (sql.Result, error) {
	if r.recorder != nil {
		r.recorder.record(query, args)
	}
	return r.MockDBRepository.Exec(ctx, query)
}

func (r *queryParametersFixtureRepository) QueryParams(ctx context.Context, query string, args []any) (*sql.Rows, error) {
	if r.recorder != nil {
		r.recorder.record(query, args)
	}
	return r.db.QueryContext(ctx, query)
}

func (r *queryParametersFixtureRepository) QueryReadOnlyParams(ctx context.Context, query string, args []any) (*database.QueryResult, error) {
	if r.recorder != nil {
		r.recorder.record(query, args)
	}
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return database.ScanRowsWithTypes(rows, database.RenderOptions{})
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
		return &queryParametersFixtureRepository{
			MockDBRepository: database.NewMockDBRepository(db).(*database.MockDBRepository),
			db:               db,
			recorder:         activeQueryParametersRecorder(),
		}
	})
}
