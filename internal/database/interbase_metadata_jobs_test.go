package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"testing"

	"interbase-go/schema"
)

type metadataTxState struct {
	mu                         sync.Mutex
	options                    driver.TxOptions
	begins, rollbacks, queries int
	opens, closes              int
	queryInTx                  bool
	rollbackErr                error
	cancelOnRollback           context.CancelFunc
	width                      driver.Value
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
		return &metadataRows{values: []driver.Value{c.state.width}}, nil
	}
	return &metadataRows{}, nil
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
func (r *metadataRows) Close() error      { return nil }
func (r *metadataRows) Next(dest []driver.Value) error {
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
