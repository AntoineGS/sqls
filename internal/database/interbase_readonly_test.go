package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
)

// The InterBase repository builds its read-only transaction with plain
// database/sql, so the assertion that matters is which driver.TxOptions reach
// the driver. This fixture records them, which no real database could report
// back and which asserting against sqlite3 would only obscure.
type txRecorder struct {
	mu        sync.Mutex
	options   []driver.TxOptions
	commits   int
	rollbacks int
	fail      bool
}

func (r *txRecorder) record(options driver.TxOptions) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.options = append(r.options, options)
}

func (r *txRecorder) snapshot() ([]driver.TxOptions, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]driver.TxOptions(nil), r.options...), r.commits, r.rollbacks
}

var (
	registerTxRecorderDriverOnce sync.Once
	txRecorderMu                 sync.Mutex
	txRecorders                  = map[string]*txRecorder{}
	txRecorderSeq                int
)

func openTxRecorderDB(t *testing.T, fail bool) (*sql.DB, *txRecorder) {
	t.Helper()
	registerTxRecorderDriverOnce.Do(func() {
		sql.Register("interbase_readonly_test", txRecorderDriver{})
	})

	recorder := &txRecorder{fail: fail}
	txRecorderMu.Lock()
	txRecorderSeq++
	key := "recorder-" + string(rune('a'+txRecorderSeq%26)) + "-" + itoa(txRecorderSeq)
	txRecorders[key] = recorder
	txRecorderMu.Unlock()

	db, err := sql.Open("interbase_readonly_test", key)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		txRecorderMu.Lock()
		delete(txRecorders, key)
		txRecorderMu.Unlock()
	})
	return db, recorder
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

type txRecorderDriver struct{}

func (txRecorderDriver) Open(name string) (driver.Conn, error) {
	txRecorderMu.Lock()
	recorder := txRecorders[name]
	txRecorderMu.Unlock()
	if recorder == nil {
		return nil, errors.New("no recorder registered for this connection")
	}
	return &txRecorderConn{recorder: recorder}, nil
}

type txRecorderConn struct{ recorder *txRecorder }

func (c *txRecorderConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not implemented")
}

func (c *txRecorderConn) Close() error { return nil }

func (c *txRecorderConn) Begin() (driver.Tx, error) {
	return nil, errors.New("not implemented")
}

func (c *txRecorderConn) BeginTx(_ context.Context, options driver.TxOptions) (driver.Tx, error) {
	c.recorder.record(options)
	return &txRecorderTx{recorder: c.recorder}, nil
}

func (c *txRecorderConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &txRecorderRows{fail: c.recorder.fail}, nil
}

type txRecorderTx struct{ recorder *txRecorder }

func (t *txRecorderTx) Commit() error {
	t.recorder.mu.Lock()
	t.recorder.commits++
	t.recorder.mu.Unlock()
	return nil
}

func (t *txRecorderTx) Rollback() error {
	t.recorder.mu.Lock()
	t.recorder.rollbacks++
	t.recorder.mu.Unlock()
	return nil
}

var errReadOnlyFetchTest = errors.New("interbase: BLOB result exceeds the materialization limit")

type txRecorderRows struct {
	fail bool
	sent int
}

func (r *txRecorderRows) Columns() []string                     { return []string{"CODE"} }
func (r *txRecorderRows) Close() error                          { return nil }
func (r *txRecorderRows) ColumnTypeDatabaseTypeName(int) string { return "VARCHAR" }
func (r *txRecorderRows) ColumnTypeScanType(int) reflect.Type   { return reflect.TypeOf("") }
func (r *txRecorderRows) ColumnTypeNullable(int) (bool, bool)   { return true, true }

func (r *txRecorderRows) Next(dest []driver.Value) error {
	switch r.sent {
	case 0:
		dest[0] = ""
		r.sent++
		return nil
	case 1:
		dest[0] = nil
		r.sent++
		return nil
	}
	if r.fail {
		return errReadOnlyFetchTest
	}
	return io.EOF
}

func TestInterBaseQueryReadOnlyUsesReadOnlyReadCommittedTransaction(t *testing.T) {
	db, recorder := openTxRecorderDB(t, false)
	repository := NewInterBaseDBRepository(db)

	readOnly, ok := repository.(ReadOnlyQuerier)
	if !ok {
		t.Fatal("*InterBaseDBRepository does not implement ReadOnlyQuerier")
	}

	result, err := readOnly.QueryReadOnly(context.Background(), "SELECT CODE FROM CUSTOMER")
	if err != nil {
		t.Fatalf("QueryReadOnly() error = %v", err)
	}
	if !result.Complete {
		t.Error("Complete = false, want true for a result that scanned to EOF")
	}

	options, commits, rollbacks := recorder.snapshot()
	if len(options) != 1 {
		t.Fatalf("began %d transactions, want exactly 1", len(options))
	}
	if !options[0].ReadOnly {
		t.Error("BeginTx ReadOnly = false, want true")
	}
	if got, want := options[0].Isolation, driver.IsolationLevel(sql.LevelReadCommitted); got != want {
		t.Errorf("BeginTx Isolation = %v, want %v", got, want)
	}
	if commits != 0 {
		t.Errorf("committed %d times, want 0 — a read-only transaction is released by rollback", commits)
	}
	if rollbacks != 1 {
		t.Errorf("rolled back %d times, want exactly 1", rollbacks)
	}
}

func TestInterBaseQueryReadOnlyDistinguishesNull(t *testing.T) {
	db, _ := openTxRecorderDB(t, false)
	readOnly := NewInterBaseDBRepository(db).(ReadOnlyQuerier)

	result, err := readOnly.QueryReadOnly(context.Background(), "SELECT CODE FROM CUSTOMER")
	if err != nil {
		t.Fatalf("QueryReadOnly() error = %v", err)
	}
	if want := [][]string{{""}, {"NULL"}}; !reflect.DeepEqual(result.Rows, want) {
		t.Errorf("Rows = %#v, want %#v — InterBase must keep NULL distinct from the empty string", result.Rows, want)
	}
}

func TestInterBaseQueryReadOnlyReturnsPartialRowsOnFetchFailure(t *testing.T) {
	db, recorder := openTxRecorderDB(t, true)
	readOnly := NewInterBaseDBRepository(db).(ReadOnlyQuerier)

	result, err := readOnly.QueryReadOnly(context.Background(), "SELECT CODE FROM CUSTOMER")
	if !errors.Is(err, errReadOnlyFetchTest) {
		t.Fatalf("error = %v, want %v", err, errReadOnlyFetchTest)
	}
	if result == nil {
		t.Fatal("result = nil alongside a fetch error, want the partial contract preserved through the repository")
	}
	if got := len(result.Rows); got != 2 {
		t.Errorf("len(Rows) = %d, want 2", got)
	}
	if result.Complete {
		t.Error("Complete = true after a fetch failure, want false")
	}

	// The transaction is still released on the failure path.
	if _, _, rollbacks := recorder.snapshot(); rollbacks != 1 {
		t.Errorf("rolled back %d times after a failed fetch, want exactly 1", rollbacks)
	}
}

func TestInterBaseQueryReadOnlyRejectsNilConnection(t *testing.T) {
	repository := &InterBaseDBRepository{}
	if _, err := repository.QueryReadOnly(context.Background(), "SELECT 1"); err == nil {
		t.Fatal("QueryReadOnly() error = nil, want a nil-connection error")
	}
}
