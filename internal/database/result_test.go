package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
)

// resultTestColumn describes one column of the fixture result set. The fixture
// implements every optional database/sql/driver metadata interface, because
// ScanRowsWithTypes is built on exactly that metadata and a fixture that did
// not supply it would exercise only the untyped fallback path.
type resultTestColumn struct {
	name         string
	databaseType string
	scanType     reflect.Type
	nullable     sql.NullBool
	length       sql.NullInt64
	precision    sql.NullInt64
	scale        sql.NullInt64
}

type resultTestRows struct {
	columns  []resultTestColumn
	values   [][]driver.Value
	position int
	// failAt is the index of the Next call that fails instead of returning a
	// row. A negative value never fails.
	failAt  int
	failErr error
}

func newResultTestRows(columns []resultTestColumn, values [][]driver.Value) *resultTestRows {
	return &resultTestRows{columns: columns, values: values, failAt: -1}
}

func (r *resultTestRows) Columns() []string {
	names := make([]string, len(r.columns))
	for i, column := range r.columns {
		names[i] = column.name
	}
	return names
}

func (r *resultTestRows) Close() error { return nil }

func (r *resultTestRows) Next(dest []driver.Value) error {
	if r.failAt >= 0 && r.position == r.failAt {
		return r.failErr
	}
	if r.position >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.position])
	r.position++
	return nil
}

func (r *resultTestRows) ColumnTypeDatabaseTypeName(index int) string {
	return r.columns[index].databaseType
}

func (r *resultTestRows) ColumnTypeScanType(index int) reflect.Type {
	if r.columns[index].scanType == nil {
		return reflect.TypeOf((*any)(nil)).Elem()
	}
	return r.columns[index].scanType
}

func (r *resultTestRows) ColumnTypeNullable(index int) (nullable, ok bool) {
	column := r.columns[index].nullable
	return column.Bool, column.Valid
}

func (r *resultTestRows) ColumnTypeLength(index int) (length int64, ok bool) {
	column := r.columns[index].length
	return column.Int64, column.Valid
}

func (r *resultTestRows) ColumnTypePrecisionScale(index int) (precision, scale int64, ok bool) {
	column := r.columns[index]
	if !column.precision.Valid {
		return 0, 0, false
	}
	return column.precision.Int64, column.scale.Int64, true
}

var (
	registerResultTestDriverOnce sync.Once
	resultTestMu                 sync.Mutex
	resultTestFixtures           = map[string]func() *resultTestRows{}
	resultTestSeq                int
)

// openResultTestDB registers newRows under a unique data source name, so each
// test owns its own fixture and no package-level value is shared between them.
func openResultTestDB(t *testing.T, newRows func() *resultTestRows) *sql.DB {
	t.Helper()
	registerResultTestDriverOnce.Do(func() {
		sql.Register("scan_rows_typed_test", resultTestDriver{})
	})

	resultTestMu.Lock()
	resultTestSeq++
	key := fmt.Sprintf("fixture-%d", resultTestSeq)
	resultTestFixtures[key] = newRows
	resultTestMu.Unlock()

	db, err := sql.Open("scan_rows_typed_test", key)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		resultTestMu.Lock()
		delete(resultTestFixtures, key)
		resultTestMu.Unlock()
	})
	return db
}

type resultTestDriver struct{}

func (resultTestDriver) Open(name string) (driver.Conn, error) {
	return resultTestConn{key: name}, nil
}

type resultTestConn struct{ key string }

func (resultTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not implemented")
}

func (resultTestConn) Close() error { return nil }

func (resultTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("not implemented")
}

func (c resultTestConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	resultTestMu.Lock()
	newRows := resultTestFixtures[c.key]
	resultTestMu.Unlock()
	if newRows == nil {
		return nil, errors.New("no fixture registered for this connection")
	}
	return newRows(), nil
}

func scanFixture(t *testing.T, newRows func() *resultTestRows, opts RenderOptions) (*QueryResult, error) {
	t.Helper()
	db := openResultTestDB(t, newRows)
	rows, err := db.QueryContext(context.Background(), "SELECT")
	if err != nil {
		t.Fatalf("QueryContext() error = %v", err)
	}
	defer func() { _ = rows.Close() }()
	return ScanRowsWithTypes(rows, opts)
}

func TestScanRowsWithTypesDistinguishesNullFromEmptyString(t *testing.T) {
	fixture := func() *resultTestRows {
		return newResultTestRows(
			[]resultTestColumn{{
				name:         "CODE",
				databaseType: "VARCHAR",
				scanType:     reflect.TypeOf(""),
				nullable:     sql.NullBool{Bool: true, Valid: true},
				length:       sql.NullInt64{Int64: 10, Valid: true},
			}},
			[][]driver.Value{{""}, {nil}},
		)
	}

	distinguished, err := scanFixture(t, fixture, RenderOptions{DistinguishNull: true})
	if err != nil {
		t.Fatalf("ScanRowsWithTypes() error = %v", err)
	}
	if want := [][]string{{""}, {"NULL"}}; !reflect.DeepEqual(distinguished.Rows, want) {
		t.Errorf("Rows = %#v, want %#v", distinguished.Rows, want)
	}
	if !distinguished.Complete {
		t.Error("Complete = false, want true for a result that scanned to EOF")
	}
	if got := distinguished.Columns[0].Nullable; !got.Valid || !got.Bool {
		t.Errorf("Columns[0].Nullable = %#v, want a valid true", got)
	}
	if got := distinguished.Columns[0].Length; !got.Valid || got.Int64 != 10 {
		t.Errorf("Columns[0].Length = %#v, want a valid 10", got)
	}

	// The default must not change any other driver's output: without the
	// option a NULL still renders as an empty cell, exactly as ScanRows does.
	plain, err := scanFixture(t, fixture, RenderOptions{})
	if err != nil {
		t.Fatalf("ScanRowsWithTypes() error = %v", err)
	}
	if want := [][]string{{""}, {""}}; !reflect.DeepEqual(plain.Rows, want) {
		t.Errorf("Rows with zero RenderOptions = %#v, want %#v", plain.Rows, want)
	}
}

func TestScanRowsWithTypesRendersScaledNumericExactly(t *testing.T) {
	// InterBase reports a scaled integer with ScanType string and hands over
	// exact decimal text. Anything that routed it through float64 would lose
	// the last digits, which is the whole point of the driver returning a
	// string, so this asserts a byte-for-byte pass-through.
	const exact = "12345678901234.56"
	result, err := scanFixture(t, func() *resultTestRows {
		return newResultTestRows(
			[]resultTestColumn{{
				name:         "AMOUNT",
				databaseType: "NUMERIC",
				scanType:     reflect.TypeOf(""),
				precision:    sql.NullInt64{Int64: 18, Valid: true},
				scale:        sql.NullInt64{Int64: 2, Valid: true},
			}},
			[][]driver.Value{{exact}},
		)
	}, RenderOptions{DistinguishNull: true})
	if err != nil {
		t.Fatalf("ScanRowsWithTypes() error = %v", err)
	}
	if got := result.Rows[0][0]; got != exact {
		t.Errorf("cell = %q, want %q", got, exact)
	}
	if got := result.Columns[0].Precision; !got.Valid || got.Int64 != 18 {
		t.Errorf("Columns[0].Precision = %#v, want a valid 18", got)
	}
	if got := result.Columns[0].Scale; !got.Valid || got.Int64 != 2 {
		t.Errorf("Columns[0].Scale = %#v, want a valid 2", got)
	}
}

func TestScanRowsWithTypesRendersTypedValues(t *testing.T) {
	moment := time.Date(2026, 9, 19, 10, 4, 11, 500000000, time.UTC)
	result, err := scanFixture(t, func() *resultTestRows {
		return newResultTestRows(
			[]resultTestColumn{
				{name: "N", databaseType: "INTEGER", scanType: reflect.TypeOf(int64(0))},
				{name: "R", databaseType: "DOUBLE PRECISION", scanType: reflect.TypeOf(float64(0))},
				{name: "B", databaseType: "BOOLEAN", scanType: reflect.TypeOf(false)},
				{name: "T", databaseType: "TIMESTAMP", scanType: reflect.TypeOf(time.Time{})},
			},
			[][]driver.Value{
				{int64(42), 1.5, true, moment},
				{nil, nil, nil, nil},
			},
		)
	}, RenderOptions{DistinguishNull: true})
	if err != nil {
		t.Fatalf("ScanRowsWithTypes() error = %v", err)
	}
	want := [][]string{
		{"42", "1.5", "true", moment.Format(time.RFC3339Nano)},
		{"NULL", "NULL", "NULL", "NULL"},
	}
	if !reflect.DeepEqual(result.Rows, want) {
		t.Errorf("Rows = %#v, want %#v", result.Rows, want)
	}
}

func TestScanRowsWithTypesDefaultsPreserveExistingRendering(t *testing.T) {
	moment := time.Date(2026, 9, 19, 10, 4, 11, 0, time.UTC)
	fixture := func() *resultTestRows {
		return newResultTestRows(
			[]resultTestColumn{
				{name: "S", databaseType: "VARCHAR", scanType: reflect.TypeOf("")},
				{name: "N", databaseType: "INTEGER", scanType: reflect.TypeOf(int64(0))},
				{name: "T", databaseType: "TIMESTAMP", scanType: reflect.TypeOf(time.Time{})},
			},
			[][]driver.Value{
				{"alice", int64(7), moment},
				{nil, nil, nil},
			},
		)
	}

	db := openResultTestDB(t, fixture)
	legacyRows, err := db.QueryContext(context.Background(), "SELECT")
	if err != nil {
		t.Fatalf("QueryContext() error = %v", err)
	}
	columns, err := Columns(legacyRows)
	if err != nil {
		t.Fatalf("Columns() error = %v", err)
	}
	legacy, err := ScanRows(legacyRows, len(columns))
	if err != nil {
		t.Fatalf("ScanRows() error = %v", err)
	}
	_ = legacyRows.Close()

	typed, err := scanFixture(t, fixture, RenderOptions{})
	if err != nil {
		t.Fatalf("ScanRowsWithTypes() error = %v", err)
	}

	// Row 0 has no NULLs and must render byte-for-byte as ScanRows renders it.
	// That is the actual regression guard: VARCHAR, INTEGER and TIMESTAMP
	// formatting — in particular time.RFC3339Nano — must not drift.
	if !reflect.DeepEqual(typed.Rows[0], legacy[0]) {
		t.Errorf("zero RenderOptions changed non-NULL rendering: got %#v, ScanRows gives %#v",
			typed.Rows[0], legacy[0])
	}

	// Row 1 is all NULLs, and here the two DO differ, deliberately.
	//
	// ScanRows scans into *interface{} (scan_row.go:34-36). A driver NULL
	// leaves the pointee an UNTYPED nil, whose reflect.Kind is Invalid rather
	// than Pointer, so the IsNil branch at scan_row.go:69-72 never fires, no
	// case in the type switch matches, and the default arm renders
	// fmt.Sprintf("%v", nil) == "<nil>" (scan_row.go:97-98). The existing
	// Test_sqlValToString_nilTypedPointer covers a *typed* nil pointer, which
	// is a different value and does render "".
	//
	// So today a real NULL reaches the results pane as the literal text
	// "<nil>". ScanRowsWithTypes renders it as an empty cell instead, and with
	// DistinguishNull it renders "NULL". Both are better than "<nil>", which
	// is indistinguishable from a string column literally containing "<nil>".
	// Nothing in the repository asserts "<nil>" (grep confirms), so this is a
	// safe improvement — but it is a cross-cutting change for every driver and
	// is recorded as such in Risk 3 and in the README.
	for column, cell := range legacy[1] {
		if cell != "<nil>" {
			t.Fatalf("this test's premise is wrong: ScanRows rendered NULL in column %d as %q, not \"<nil>\"; "+
				"re-check scan_row.go before changing anything else", column, cell)
		}
	}
	wantNull := []string{"", "", ""}
	if !reflect.DeepEqual(typed.Rows[1], wantNull) {
		t.Errorf("NULL row = %#v, want %#v", typed.Rows[1], wantNull)
	}
	names := make([]string, len(typed.Columns))
	for i, column := range typed.Columns {
		names[i] = column.Name
	}
	if !reflect.DeepEqual(names, columns) {
		t.Errorf("column names = %#v, Columns() gives %#v", names, columns)
	}
}

func TestScanRowsWithTypesNamesBlankColumns(t *testing.T) {
	// database.Columns replaces a blank column name with col<N>, and the
	// results-pane header now comes from QueryResult.Columns rather than
	// from Columns(), so the same substitution has to happen here or an
	// expression column loses its header.
	result, err := scanFixture(t, func() *resultTestRows {
		return newResultTestRows(
			[]resultTestColumn{
				{name: "  ", databaseType: "INTEGER", scanType: reflect.TypeOf(int64(0))},
				{name: "NAMED", databaseType: "INTEGER", scanType: reflect.TypeOf(int64(0))},
			},
			[][]driver.Value{{int64(1), int64(2)}},
		)
	}, RenderOptions{})
	if err != nil {
		t.Fatalf("ScanRowsWithTypes() error = %v", err)
	}
	if got := result.Columns[0].Name; got != "col0" {
		t.Errorf("Columns[0].Name = %q, want %q", got, "col0")
	}
	if got := result.Columns[1].Name; got != "NAMED" {
		t.Errorf("Columns[1].Name = %q, want %q", got, "NAMED")
	}
}

func TestRenderOptionsForOnlyInterBaseDistinguishesNull(t *testing.T) {
	if got := RenderOptionsFor(dialect.DatabaseDriverInterBase); !got.DistinguishNull {
		t.Error("RenderOptionsFor(interbase).DistinguishNull = false, want true")
	}
	for _, name := range []dialect.DatabaseDriver{
		dialect.DatabaseDriverMySQL,
		dialect.DatabaseDriverPostgreSQL,
		dialect.DatabaseDriverSQLite3,
	} {
		if got := RenderOptionsFor(name); got.DistinguishNull {
			t.Errorf("RenderOptionsFor(%s).DistinguishNull = true, want false", name)
		}
	}
}

func TestScanRowsWithTypesRendersBlobPlaceholder(t *testing.T) {
	// Subtype 0 BLOBs scan as []byte and must never be spilled into the pane;
	// subtype 1 BLOBs scan as string and are capped like any other text.
	long := strings.Repeat("x", 600)
	result, err := scanFixture(t, func() *resultTestRows {
		return newResultTestRows(
			[]resultTestColumn{
				{name: "BINARY_BLOB", databaseType: "BLOB", scanType: reflect.TypeOf([]byte(nil))},
				{name: "TEXT_BLOB", databaseType: "BLOB", scanType: reflect.TypeOf("")},
			},
			[][]driver.Value{{[]byte("hello"), long}},
		)
	}, RenderOptions{DistinguishNull: true})
	if err != nil {
		t.Fatalf("ScanRowsWithTypes() error = %v", err)
	}

	if got, want := result.Rows[0][0], "<BLOB 5 bytes>"; got != want {
		t.Errorf("binary BLOB cell = %q, want %q", got, want)
	}
	wantText := strings.Repeat("x", DefaultMaxCellRunes) + "…(truncated, 600 characters)"
	if got := result.Rows[0][1]; got != wantText {
		t.Errorf("text BLOB cell = %q, want %q", got, wantText)
	}
	if got := result.Columns[0].DatabaseTypeName; got != "BLOB" {
		t.Errorf("Columns[0].DatabaseTypeName = %q, want %q", got, "BLOB")
	}
}

func TestScanRowsWithTypesNullBlobIsNotAPlaceholder(t *testing.T) {
	// A NULL BLOB arrives as a nil []byte. Rendering it as "<BLOB 0 bytes>"
	// would claim an empty value exists where there is none.
	result, err := scanFixture(t, func() *resultTestRows {
		return newResultTestRows(
			[]resultTestColumn{{name: "B", databaseType: "BLOB", scanType: reflect.TypeOf([]byte(nil))}},
			[][]driver.Value{{nil}},
		)
	}, RenderOptions{DistinguishNull: true})
	if err != nil {
		t.Fatalf("ScanRowsWithTypes() error = %v", err)
	}
	if got, want := result.Rows[0][0], "NULL"; got != want {
		t.Errorf("NULL BLOB cell = %q, want %q", got, want)
	}
}

func TestScanRowsWithTypesCapsLongCellsByRunes(t *testing.T) {
	// Eight multi-byte runes: a byte-based cap would slice mid-rune and emit
	// replacement characters, and would report the wrong total.
	value := strings.Repeat("é", 12)
	result, err := scanFixture(t, func() *resultTestRows {
		return newResultTestRows(
			[]resultTestColumn{{name: "S", databaseType: "VARCHAR", scanType: reflect.TypeOf("")}},
			[][]driver.Value{{value}},
		)
	}, RenderOptions{MaxCellRunes: 8})
	if err != nil {
		t.Fatalf("ScanRowsWithTypes() error = %v", err)
	}
	want := strings.Repeat("é", 8) + "…(truncated, 12 characters)"
	if got := result.Rows[0][0]; got != want {
		t.Errorf("cell = %q, want %q", got, want)
	}
}

func TestScanRowsWithTypesLeavesShortCellsAlone(t *testing.T) {
	result, err := scanFixture(t, func() *resultTestRows {
		return newResultTestRows(
			[]resultTestColumn{{name: "S", databaseType: "VARCHAR", scanType: reflect.TypeOf("")}},
			[][]driver.Value{{"short"}},
		)
	}, RenderOptions{MaxCellRunes: 8})
	if err != nil {
		t.Fatalf("ScanRowsWithTypes() error = %v", err)
	}
	if got := result.Rows[0][0]; got != "short" {
		t.Errorf("cell = %q, want it untouched", got)
	}
}

func TestDefaultMaxCellRunesIsFiveHundredTwelve(t *testing.T) {
	if DefaultMaxCellRunes != 512 {
		t.Errorf("DefaultMaxCellRunes = %d, want 512", DefaultMaxCellRunes)
	}
}
