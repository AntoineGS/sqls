package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"reflect"
	"testing"
)

func TestInterBaseExecParamsForwardsPositionalArguments(t *testing.T) {
	db, recorder := openTxRecorderDB(t, false)
	repository := NewInterBaseDBRepository(db)

	parameterized, ok := repository.(ParameterizedRepository)
	if !ok {
		t.Fatal("*InterBaseDBRepository does not implement ParameterizedRepository")
	}

	result, err := parameterized.ExecParams(context.Background(), "UPDATE CUSTOMER SET CODE = ? WHERE ID = ?", []any{"new-code", int64(7)})
	if err != nil {
		t.Fatalf("ExecParams() error = %v", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		t.Fatalf("RowsAffected() error = %v", err)
	}
	if affected != 1 {
		t.Errorf("RowsAffected() = %d, want 1", affected)
	}

	queries, args, _ := recorder.querySnapshot()
	if len(queries) != 1 || queries[0] != "UPDATE CUSTOMER SET CODE = ? WHERE ID = ?" {
		t.Fatalf("queries = %#v, want exactly one matching the rewritten SQL", queries)
	}
	want := []driver.Value{"new-code", int64(7)}
	got := namedValuesToDriverValues(args[0])
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("driver received args = %#v, want %#v — arguments must reach the driver, not just the SQL text", got, want)
	}
}

func TestInterBaseExecForwardsNilArgumentsToExecParams(t *testing.T) {
	db, recorder := openTxRecorderDB(t, false)
	repository := NewInterBaseDBRepository(db)

	if _, err := repository.Exec(context.Background(), "DELETE FROM CUSTOMER"); err != nil {
		t.Fatalf("Exec() error = %v", err)
	}

	queries, args, _ := recorder.querySnapshot()
	if len(queries) != 1 {
		t.Fatalf("queries = %#v, want exactly one", queries)
	}
	if len(args[0]) != 0 {
		t.Errorf("driver received args = %#v, want none for legacy Exec", args[0])
	}
}

func TestInterBaseQueryParamsForwardsPositionalArguments(t *testing.T) {
	db, recorder := openTxRecorderDB(t, false)
	repository := NewInterBaseDBRepository(db)

	parameterized, ok := repository.(ParameterizedRepository)
	if !ok {
		t.Fatal("*InterBaseDBRepository does not implement ParameterizedRepository")
	}

	rows, err := parameterized.QueryParams(context.Background(), "SELECT CODE FROM CUSTOMER WHERE ID = ?", []any{int64(42)})
	if err != nil {
		t.Fatalf("QueryParams() error = %v", err)
	}
	defer func() { _ = rows.Close() }()

	queries, args, _ := recorder.querySnapshot()
	if len(queries) != 1 || queries[0] != "SELECT CODE FROM CUSTOMER WHERE ID = ?" {
		t.Fatalf("queries = %#v, want exactly one matching the rewritten SQL", queries)
	}
	want := []driver.Value{int64(42)}
	got := namedValuesToDriverValues(args[0])
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("driver received args = %#v, want %#v", got, want)
	}
}

func TestInterBaseQueryForwardsNilArgumentsToQueryParams(t *testing.T) {
	db, recorder := openTxRecorderDB(t, false)
	repository := NewInterBaseDBRepository(db)

	rows, err := repository.Query(context.Background(), "SELECT CODE FROM CUSTOMER")
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	defer func() { _ = rows.Close() }()

	queries, args, _ := recorder.querySnapshot()
	if len(queries) != 1 {
		t.Fatalf("queries = %#v, want exactly one", queries)
	}
	if len(args[0]) != 0 {
		t.Errorf("driver received args = %#v, want none for legacy Query", args[0])
	}
}

func TestInterBaseQueryReadOnlyParamsForwardsArgumentsAndUsesReadOnlyReadCommittedTransaction(t *testing.T) {
	db, recorder := openTxRecorderDB(t, false)
	repository := NewInterBaseDBRepository(db)

	readOnly, ok := repository.(ParameterizedReadOnlyQuerier)
	if !ok {
		t.Fatal("*InterBaseDBRepository does not implement ParameterizedReadOnlyQuerier")
	}

	result, err := readOnly.QueryReadOnlyParams(context.Background(), "SELECT CODE FROM CUSTOMER WHERE ID = ?", []any{int64(9)})
	if err != nil {
		t.Fatalf("QueryReadOnlyParams() error = %v", err)
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

	queries, args, rowsClosed := recorder.querySnapshot()
	if len(queries) != 1 || queries[0] != "SELECT CODE FROM CUSTOMER WHERE ID = ?" {
		t.Fatalf("queries = %#v, want exactly one matching the rewritten SQL", queries)
	}
	want := []driver.Value{int64(9)}
	got := namedValuesToDriverValues(args[0])
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("driver received args = %#v, want %#v", got, want)
	}
	if rowsClosed != 1 {
		t.Errorf("closed rows %d times, want exactly 1", rowsClosed)
	}
}

func TestInterBaseQueryReadOnlyForwardsNilArgumentsToQueryReadOnlyParams(t *testing.T) {
	db, recorder := openTxRecorderDB(t, false)
	readOnly := NewInterBaseDBRepository(db).(ReadOnlyQuerier)

	if _, err := readOnly.QueryReadOnly(context.Background(), "SELECT CODE FROM CUSTOMER"); err != nil {
		t.Fatalf("QueryReadOnly() error = %v", err)
	}

	_, args, _ := recorder.querySnapshot()
	if len(args) != 1 || len(args[0]) != 0 {
		t.Errorf("driver received args = %#v, want none for legacy QueryReadOnly", args)
	}
}

func TestInterBaseExecParamsRejectsNilConnection(t *testing.T) {
	repository := &InterBaseDBRepository{}
	if _, err := repository.ExecParams(context.Background(), "DELETE FROM T", nil); err == nil {
		t.Fatal("ExecParams() error = nil, want a nil-connection error")
	}
}

func TestInterBaseQueryParamsRejectsNilConnection(t *testing.T) {
	repository := &InterBaseDBRepository{}
	if _, err := repository.QueryParams(context.Background(), "SELECT 1", nil); err == nil {
		t.Fatal("QueryParams() error = nil, want a nil-connection error")
	}
}

func TestInterBaseQueryReadOnlyParamsRejectsNilConnection(t *testing.T) {
	repository := &InterBaseDBRepository{}
	if _, err := repository.QueryReadOnlyParams(context.Background(), "SELECT 1", nil); err == nil {
		t.Fatal("QueryReadOnlyParams() error = nil, want a nil-connection error")
	}
}

func TestMockDBRepositoryHasNoParameterizedCapabilities(t *testing.T) {
	repository := NewMockDBRepository(nil)
	if _, ok := repository.(ParameterizedRepository); ok {
		t.Error("MockDBRepository implements ParameterizedRepository, want the plain mock left untouched")
	}
	if _, ok := repository.(ParameterizedReadOnlyQuerier); ok {
		t.Error("MockDBRepository implements ParameterizedReadOnlyQuerier, want the plain mock left untouched")
	}
	if _, ok := repository.(InputDescriber); ok {
		t.Error("MockDBRepository implements InputDescriber, want the plain mock left untouched")
	}
}

// namedValuesToDriverValues strips positional ordinals so tests can compare
// against a plain []driver.Value in call order.
func namedValuesToDriverValues(values []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(values))
	for i, v := range values {
		out[i] = v.Value
	}
	return out
}
