package handler

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
)

const stubDriverName = "stub"

// A minimal database/sql driver so the stub repository can hand the handler a
// real *sql.Rows. Every query returns one row with a single column named "n".
type stubSQLDriver struct{}

func (stubSQLDriver) Open(string) (driver.Conn, error) { return stubSQLConn{}, nil }

type stubSQLConn struct{}

func (stubSQLConn) Prepare(string) (driver.Stmt, error) { return stubSQLStmt{}, nil }
func (stubSQLConn) Close() error                        { return nil }
func (stubSQLConn) Begin() (driver.Tx, error)           { return nil, errors.New("stub: no transactions") }

type stubSQLStmt struct{}

func (stubSQLStmt) Close() error  { return nil }
func (stubSQLStmt) NumInput() int { return 0 }
func (stubSQLStmt) Exec([]driver.Value) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}
func (stubSQLStmt) Query([]driver.Value) (driver.Rows, error) { return &stubSQLRows{}, nil }

type stubSQLRows struct{ sent bool }

func (r *stubSQLRows) Columns() []string { return []string{"n"} }
func (r *stubSQLRows) Close() error      { return nil }
func (r *stubSQLRows) Next(dest []driver.Value) error {
	if r.sent {
		return io.EOF
	}
	r.sent = true
	dest[0] = int64(42)
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
	mu       sync.Mutex
	gates    map[string]*stubGate
	served   []stubQuery
	openedDB []*sql.DB
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
		return &stubRepository{
			MockDBRepository: database.NewMockDBRepository(db).(*database.MockDBRepository),
			backend:          b,
			db:               db,
		}
	})
}
