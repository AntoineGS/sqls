# Results-Pane Semantics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the results pane tell the truth — read statements run in an explicit read-only transaction, cells are rendered from `Rows.ColumnTypes` so `NULL` is distinguishable from the empty string and scaled decimals stay exact, a fetch that dies partway shows the rows that preceded it, and `EXECUTE PROCEDURE` is routed by the procedure's cached output arity instead of unconditionally to `Exec`.

**Architecture:** A new driver-neutral `internal/database/result.go` owns `QueryResult`, `ColumnMeta`, `RenderOptions` and `ScanRowsWithTypes`, which scans through NULL-tolerant destinations chosen from each column's `ScanType()` and returns a **non-nil result alongside an error** when the fetch fails partway. A new optional `ReadOnlyQuerier` capability, implemented for InterBase with plain `database/sql` in the untagged `interbase_common.go`, materialises a whole result inside a `BeginTx(ReadOnly, LevelReadCommitted)` transaction so the transaction never escapes the repository. `s.query` is restructured into "obtain a `*QueryResult`" plus "render a `*QueryResult`", which is what makes partial rendering, the incomplete footer and per-result notes possible. Finally, `EXECUTE PROCEDURE` routing reads `DBCache.Procedure(name).OutputParameters` and chooses `Query` or `Exec` once, never both.

**Tech Stack:** Go 1.25.7, `database/sql` and `database/sql/driver`, `github.com/olekukonko/tablewriter`, `github.com/sourcegraph/jsonrpc2@v0.2.1`, `interbase-go` (only behind the `interbase` build tag; nothing in this plan imports it).

**Spec:** `docs/superpowers/specs/2026-09-19-interbase-editor-features-design.md` — this plan implements **Plan 2 of 4** from that spec's "Plan decomposition" section: §6.2 (read-only transaction), §6.3 (`ScanRowsWithTypes` and typed rendering) and §6.4 (`EXECUTE PROCEDURE` routing), plus the results-pane paragraphs of the README. Features 1–5 and §6.1/§6.1bis/§6.5 belong to other plans and must **not** be built here.

**Builds on:** `docs/superpowers/plans/2026-09-19-server-concurrency-cancellation.md` (Plan 1), which must be complete before this plan starts. This plan consumes Plan 1's `make test-race`, its `runStatement`/`cancellationNotice`/`cancelledError` shapes in `internal/handler/execute_command.go` and `internal/handler/failure.go`, and its test fixture in `internal/handler/concurrency_test.go`. Nothing here contradicts Plan 1: `s.query` keeps returning a bare error whenever the failure is a cancellation, so Plan 1's notice still fires and the late-cancellation note still cannot co-occur with it.

## Global Constraints

- `DefaultMaxCellRunes = 512` — "a display cap applied when `RenderOptions.MaxCellRunes` is zero", "a named constant reachable through `RenderOptions.MaxCellRunes` rather than a literal". Never write `512` at a use site.
- `RenderOptions` has exactly two fields, named `DistinguishNull bool` and `MaxCellRunes int`. `DistinguishNull` "defaults to false and is set to true only for InterBase, so no other driver's rendering or test output changes".
- **The partial-result contract.** `ScanRowsWithTypes` returns a **non-nil `*QueryResult` together with a non-nil error** when `rows.Next`, `rows.Scan` or `rows.Err` fails partway; it returns `(nil, err)` **only** when `rows.ColumnTypes()` itself fails. `s.query` "must render a non-nil result **before** reporting the error, emit the `N rows in set (incomplete)` footer when `Complete` is false, and return the rendered string as the command result rather than propagating a bare error."
- Exceeding the driver's 64 MiB BLOB limit "is a hard error, not a truncation… The fetch fails; there is no partial value."
- `EXECUTE PROCEDURE` **never** uses the read-only path: "per the driver README, an implicit procedure query commits its write transaction, so a procedure call is a write even when it returns a row."
- `EXECUTE PROCEDURE` routing never tries one path then the other. An unknown procedure uses `Exec` and surfaces the driver's rejection, because "'try one path, then the other' is a shape that can execute a mutating procedure twice if that reasoning is ever wrong, and a stale cache is not worth that risk."
- "Capability, not driver check, wherever possible." Handlers type-assert `database.ReadOnlyQuerier`; "a non-InterBase driver that later implements one gets the feature for free."
- "Driver imports only under the build tag." `interbase_common.go` has no tag and therefore **cannot** reference driver types or error types. Nothing in this plan adds a tagged file.
- No error string is ever matched. The BLOB hint is gated on `ColumnTypes` reporting a BLOB column, and the driver's error text "is passed through verbatim rather than matched on".
- "The header row and `%d rows in set` footer stay exactly as they are; the vertical writer is unchanged."
- "Any new configuration keys" are out of scope. "Every feature here is automatic when the driver is InterBase and the capability is present."
- Catalog accessors normalise the name they are given (`…-interbase-dialect-and-catalog-design.md` §4.5: "Every singular accessor **normalises the name it is given**; callers pass the identifier text as the user typed it and never upper-case at the call site"). Task 8 must not add `strings.ToUpper` at its call site.
- Inherited from Plan 1, unchanged by this plan: **lock ordering is `connMu` before `stateMu`, never the reverse**, and `stateMu` is never held across any I/O. `executeQuery` already holds `connMu.RLock()` for the whole of its database work including rendering; every new rendering path added here runs inside that same hold and takes no additional lock.
- Verification commands: `go test ./...`, `make test-race`, and for tagged code `CGO_ENABLED=1 go build -tags interbase ./...`.

## File Structure

| File | Status | Responsibility |
| --- | --- | --- |
| `internal/database/result.go` | Create | `ColumnMeta`, `QueryResult`, `RenderOptions`, `DefaultMaxCellRunes`, `ScanRowsWithTypes`, `RenderOptionsFor` — the whole typed-rendering surface, driver-neutral |
| `internal/database/result_test.go` | Create | the metadata-complete `database/sql/driver` fixture and every scanner test |
| `internal/database/scan_row.go` | Modify (`:30`) | one doc comment on `ScanRows` pointing at the replacement and naming the contract difference |
| `internal/database/scan_row_test.go` | Modify | characterisation test pinning that `ScanRows` discards partial rows |
| `internal/database/database.go` | Modify (`:24-36`) | the `ReadOnlyQuerier` optional capability interface |
| `internal/database/interbase_common.go` | Modify (after `:506`) | `(*InterBaseDBRepository).QueryReadOnly` |
| `internal/database/interbase_readonly_test.go` | Create | a recording `database/sql` driver that captures `driver.TxOptions` |
| `internal/handler/execute_command.go` | Modify (`:279-334`, and `runStatement` from Plan 1 Task 9) | split `s.query` into obtain/render; read-only preference; `EXECUTE PROCEDURE` routing |
| `internal/handler/concurrency_test.go` | Modify (Plan 1's fixture) | failing-fetch and BLOB stub rows; the read-only stub repository; the InterBase-identity stub connection |
| `internal/handler/execute_command_test.go` | Modify | partial-render, BLOB-hint and read-only handler tests |
| `internal/handler/interbase_procedure_test.go` | Create | §6.4 routing tests and the name-parsing unit test |
| `README.md` | Modify | results-pane behaviour: `NULL`, the 512-character cap, partial results, `EXECUTE PROCEDURE` |

**Sequencing and the one external dependency.** Tasks 1–7 depend on nothing outside this repository and can be executed immediately after Plan 1. **Task 8 (§6.4) depends on sub-project 2's catalog work** — `DBCache.HasCatalog()`, `DBCache.Procedure(name) (*ProcedureDesc, bool)`, `ProcedureDesc.OutputParameters`, and the worker populating `DBCache.Catalog` from a `CatalogRepository`. It is deliberately last so that if sub-project 2 slips, Tasks 1–7 and 9 still ship a complete, working improvement and Task 8 moves to Plan 3 exactly as the spec's "Plan decomposition" anticipates. Task 8 opens with a precondition step that verifies those symbols exist and stops if they do not.

---

### Task 1: `QueryResult`, `RenderOptions` and the core typed scanner

Spec §6.3. `ScanRows` (`internal/database/scan_row.go:30`) scans into `[]interface{}` and stringifies with `reflect`, so a NULL and an empty string both render as `""`, and `rows.ColumnTypes()` is never called anywhere in the repository. This task adds the replacement scanner and the metadata-complete fixture every later task tests against.

`ScanRows` is **kept**. It is upstream code, deleting it is a separate API decision, and Task 3 uses it as the pinned contrast the partial-result contract is defined against.

**Files:**
- Create: `internal/database/result.go`
- Create: `internal/database/result_test.go`
- Modify: `internal/database/scan_row.go:30`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type ColumnMeta struct { Name, DatabaseTypeName string; Nullable sql.NullBool; Length, Precision, Scale sql.NullInt64 }`
  - `type QueryResult struct { Columns []ColumnMeta; Rows [][]string; Notes []string; Complete bool }`
  - `type RenderOptions struct { DistinguishNull bool; MaxCellRunes int }`
  - `const DefaultMaxCellRunes = 512`
  - `func ScanRowsWithTypes(rows *sql.Rows, opts RenderOptions) (*QueryResult, error)`
  - `func RenderOptionsFor(driver dialect.DatabaseDriver) RenderOptions`
  - Test-only, package `database`: `type resultTestColumn`, `type resultTestRows`, `func newResultTestRows(columns []resultTestColumn, values [][]driver.Value) *resultTestRows`, `func openResultTestDB(t *testing.T, newRows func() *resultTestRows) *sql.DB`, `func scanFixture(t *testing.T, newRows func() *resultTestRows, opts RenderOptions) (*QueryResult, error)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/database/result_test.go`:

```go
package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
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
	// results-pane header now comes from QueryResult.Columns rather than from
	// Columns(), so the same substitution has to happen here or an expression
	// column loses its header.
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
		dialect.DatabaseDriverMySql,
		dialect.DatabaseDriverPostgreSQL,
		dialect.DatabaseDriverSQLite3,
	} {
		if got := RenderOptionsFor(name); got.DistinguishNull {
			t.Errorf("RenderOptionsFor(%s).DistinguishNull = true, want false", name)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestScanRowsWithTypes|TestRenderOptionsFor' ./internal/database/ -v`
Expected: FAIL to compile — `undefined: ScanRowsWithTypes`, `undefined: RenderOptions`, `undefined: RenderOptionsFor`.

If `dialect.DatabaseDriverMySql`, `DatabaseDriverPostgreSQL` or `DatabaseDriverSQLite3` does not resolve, check the constant names in `dialect/dialect.go` and use the ones that exist; the test only needs three non-InterBase drivers.

- [ ] **Step 3: Write the minimal implementation**

Create `internal/database/result.go`:

```go
package database

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sqls-server/sqls/dialect"
)

// ColumnMeta is the immutable execution snapshot of one result column. Fields
// the driver cannot establish stay invalid rather than defaulting to a value
// the catalog never reported.
type ColumnMeta struct {
	Name             string
	DatabaseTypeName string
	Nullable         sql.NullBool
	Length           sql.NullInt64
	Precision        sql.NullInt64
	Scale            sql.NullInt64
}

// QueryResult is a fully materialised result set, rendered to strings.
type QueryResult struct {
	Columns []ColumnMeta
	Rows    [][]string
	Notes   []string
	// Complete is false when scanning stopped early because of an error.
	Complete bool
}

// RenderOptions controls driver-sensitive cell formatting.
//
// The zero value reproduces the existing ScanRows output for every non-NULL
// cell within the display cap. It diverges in exactly two places, both of them
// cross-cutting rather than InterBase-only, and both deliberate:
//
//   - a cell longer than DefaultMaxCellRunes is truncated, which ScanRows does
//     not do; and
//   - a SQL NULL renders as an empty cell, where ScanRows renders the literal
//     text "<nil>". ScanRows scans into *interface{} (scan_row.go:34-36), so a
//     driver NULL leaves an UNTYPED nil whose reflect.Kind is Invalid, not
//     Pointer; the IsNil branch at scan_row.go:69-72 never fires and the
//     default arm formats it as "<nil>" (scan_row.go:97-98). The existing
//     Test_sqlValToString_nilTypedPointer covers a *typed* nil pointer, which
//     is a different value and does render "". Nothing in the repository
//     asserts "<nil>".
type RenderOptions struct {
	// DistinguishNull renders SQL NULL as the literal NULL instead of an
	// empty cell. Set only for InterBase. Note that the zero value already
	// differs from ScanRows here: it renders an empty cell, not "<nil>".
	DistinguishNull bool
	// MaxCellRunes caps rendered cell width. Zero means DefaultMaxCellRunes.
	MaxCellRunes int
}

// DefaultMaxCellRunes is the display cap applied when RenderOptions.MaxCellRunes
// is zero. It is a display limit only, unrelated to the InterBase driver's
// 64 MiB materialisation limit: it exists so a large text BLOB the driver did
// materialise is not pushed whole through JSON-RPC into the editor.
const DefaultMaxCellRunes = 512

const nullCellText = "NULL"

// BlobTypeName is the DatabaseTypeName InterBase reports for a BLOB column.
//
// It is exported because internal/handler gates the 64 MiB materialisation
// hint on the same value (hasBlobColumn, Task 4). Two copies of the literal
// "BLOB" in two packages would be free to drift, and the drift would be
// silent: the hint would simply stop appearing.
const BlobTypeName = "BLOB"

var (
	stringScanType  = reflect.TypeOf("")
	int64ScanType   = reflect.TypeOf(int64(0))
	float64ScanType = reflect.TypeOf(float64(0))
	boolScanType    = reflect.TypeOf(false)
	timeScanType    = reflect.TypeOf(time.Time{})
	bytesScanType   = reflect.TypeOf([]byte(nil))
)

// RenderOptionsFor returns the cell-rendering options for a driver. Only
// InterBase distinguishes NULL from the empty string, because only its driver
// guarantees that empty values stay distinct from NULL through the fetch.
func RenderOptionsFor(driver dialect.DatabaseDriver) RenderOptions {
	if driver == dialect.DatabaseDriverInterBase {
		return RenderOptions{DistinguishNull: true}
	}
	return RenderOptions{}
}

// ScanRowsWithTypes renders rows using the column metadata the driver reports,
// which ScanRows never asks for.
//
// It returns a non-nil *QueryResult together with a non-nil error when the
// fetch fails partway: Rows holds everything scanned before the failure,
// Columns is populated, and Complete is false. It returns (nil, err) only when
// rows.ColumnTypes() itself fails, i.e. when there is nothing to report. The
// caller is expected to render the partial result and then report the error —
// the InterBase driver's 64 MiB BLOB limit is a hard fetch error rather than a
// truncation, so the rows that preceded it are the only clue to which value
// broke the query.
func ScanRowsWithTypes(rows *sql.Rows, opts RenderOptions) (*QueryResult, error) {
	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return nil, fmt.Errorf("cannot get query column types, %w", err)
	}

	result := &QueryResult{
		Columns: make([]ColumnMeta, len(columnTypes)),
		Rows:    [][]string{},
	}
	for i, columnType := range columnTypes {
		meta := ColumnMeta{
			Name:             columnType.Name(),
			DatabaseTypeName: strings.ToUpper(columnType.DatabaseTypeName()),
		}
		if strings.TrimSpace(meta.Name) == "" {
			meta.Name = fmt.Sprintf("col%d", i)
		}
		if nullable, ok := columnType.Nullable(); ok {
			meta.Nullable = sql.NullBool{Bool: nullable, Valid: true}
		}
		if length, ok := columnType.Length(); ok {
			meta.Length = sql.NullInt64{Int64: length, Valid: true}
		}
		if precision, scale, ok := columnType.DecimalSize(); ok {
			meta.Precision = sql.NullInt64{Int64: precision, Valid: true}
			meta.Scale = sql.NullInt64{Int64: scale, Valid: true}
		}
		result.Columns[i] = meta
	}

	maxCellRunes := opts.MaxCellRunes
	if maxCellRunes <= 0 {
		maxCellRunes = DefaultMaxCellRunes
	}

	for rows.Next() {
		buffer := make([]interface{}, len(columnTypes))
		for i, columnType := range columnTypes {
			buffer[i] = newScanDest(columnType.ScanType())
		}
		if err := rows.Scan(buffer...); err != nil {
			return result, err
		}

		row := make([]string, len(columnTypes))
		for i, dest := range buffer {
			cell, err := renderCell(dest, result.Columns[i], opts, maxCellRunes)
			if err != nil {
				return result, err
			}
			row[i] = cell
		}
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}

	result.Complete = true
	return result, nil
}

// newScanDest allocates a NULL-tolerant destination for one column. A plain
// *string or *int64 fails on a NULL value ("converting NULL to string is
// unsupported"), which would destroy exactly the distinction this renderer
// exists to keep, so every concrete type gets a nullable holder. An unknown
// scan type falls back to *interface{}, which is what ScanRows uses for every
// column — so a driver that reports no scan types renders identically to today.
func newScanDest(scanType reflect.Type) interface{} {
	switch scanType {
	case stringScanType:
		return new(sql.NullString)
	case int64ScanType:
		return new(sql.NullInt64)
	case float64ScanType:
		return new(sql.NullFloat64)
	case boolScanType:
		return new(sql.NullBool)
	case timeScanType:
		return new(sql.NullTime)
	case bytesScanType:
		return new([]byte)
	}
	return new(interface{})
}

func renderCell(dest interface{}, meta ColumnMeta, opts RenderOptions, maxCellRunes int) (string, error) {
	switch value := dest.(type) {
	case *sql.NullString:
		if !value.Valid {
			return nullCell(opts), nil
		}
		return capCell(value.String, meta, maxCellRunes), nil
	case *sql.NullInt64:
		if !value.Valid {
			return nullCell(opts), nil
		}
		return fmt.Sprintf("%v", value.Int64), nil
	case *sql.NullFloat64:
		if !value.Valid {
			return nullCell(opts), nil
		}
		return fmt.Sprintf("%v", value.Float64), nil
	case *sql.NullBool:
		if !value.Valid {
			return nullCell(opts), nil
		}
		return fmt.Sprintf("%v", value.Bool), nil
	case *sql.NullTime:
		if !value.Valid {
			return nullCell(opts), nil
		}
		return value.Time.Format(time.RFC3339Nano), nil
	case *[]byte:
		if *value == nil {
			return nullCell(opts), nil
		}
		if meta.DatabaseTypeName == BlobTypeName {
			return fmt.Sprintf("<BLOB %d bytes>", len(*value)), nil
		}
		return capCell(string(*value), meta, maxCellRunes), nil
	case *interface{}:
		if *value == nil {
			return nullCell(opts), nil
		}
		// A driver with no scan-type metadata gets exactly today's rendering.
		text, err := sqlValToString(value)
		if err != nil {
			return "", err
		}
		return capCell(text, meta, maxCellRunes), nil
	}
	return "", fmt.Errorf("unsupported scan destination %T", dest)
}

func nullCell(opts RenderOptions) string {
	if opts.DistinguishNull {
		return nullCellText
	}
	return ""
}

// capCell truncates an over-long rendered cell and says by how much, so the cap
// is never mistaken for missing data.
func capCell(text string, meta ColumnMeta, maxCellRunes int) string {
	total := utf8.RuneCountInString(text)
	if total <= maxCellRunes {
		return text
	}
	return string([]rune(text)[:maxCellRunes]) + fmt.Sprintf("…(truncated, %d characters)", total)
}
```

Note: `meta` is accepted by `capCell` but unused today; Task 2 keeps it that way. If the Go vet/lint configuration in this repository rejects an unused parameter, drop `meta` from `capCell` and from its two call sites in `renderCell` — nothing else depends on it.

In `internal/database/scan_row.go`, add a doc comment above `ScanRows` at line 30:

```go
// ScanRows stringifies every column through reflection, which renders a NULL
// and an empty string identically and discards every row it scanned when the
// fetch fails partway. ScanRowsWithTypes is the replacement: it renders from
// the driver's column metadata and returns the rows it did scan alongside the
// error. ScanRows is retained because it is upstream code and because
// TestScanRowsDiscardsPartialRows pins the contrast.
func ScanRows(rows *sql.Rows, columnLength int) ([][]string, error) {
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestScanRowsWithTypes|TestRenderOptionsFor' ./internal/database/ -v`
Expected: all six PASS.

- [ ] **Step 5: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/database/result.go internal/database/result_test.go internal/database/scan_row.go
git commit -m "feat: render query results from driver column metadata"
```

---

### Task 2: BLOB placeholders and the display cap

Spec §6.3's rendering table and the "A BLOB that fits but is large" entry of User-Visible Behavior. A binary BLOB is never rendered as bytes in a terminal table; a text BLOB and any other over-long cell are capped at `RenderOptions.MaxCellRunes`.

**Files:**
- Modify: `internal/database/result.go` (`renderCell`, `capCell`)
- Test: `internal/database/result_test.go`

**Interfaces:**
- Consumes: `ScanRowsWithTypes`, `RenderOptions`, `DefaultMaxCellRunes`, `resultTestColumn`, `newResultTestRows`, `scanFixture` (Task 1).
- Produces: no new exported names. `RenderOptions.MaxCellRunes` becomes observable; `<BLOB n bytes>` and `…(truncated, n characters)` become fixed output strings that Task 5's README text describes.

- [ ] **Step 1: Write the failing tests**

Append to `internal/database/result_test.go`:

```go
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
```

Add `"strings"` to the file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestScanRowsWithTypesRendersBlob|TestScanRowsWithTypesNullBlob|TestScanRowsWithTypesCaps|TestScanRowsWithTypesLeavesShort|TestDefaultMaxCellRunes' ./internal/database/ -v`
Expected: `TestDefaultMaxCellRunesIsFiveHundredTwelve` and `TestScanRowsWithTypesLeavesShortCellsAlone` PASS because Task 1 already implements them. The three BLOB/cap tests also PASS — Task 1's `renderCell` and `capCell` already contain this behaviour.

If they all pass, that is the expected outcome and not a reason to stop: this task's deliverable is the **coverage**, and Task 1's implementation was written whole because splitting `renderCell` across two commits would have left it non-compiling. Confirm by breaking it deliberately in Step 3 before restoring it.

- [ ] **Step 3: Prove the tests are not vacuous**

Temporarily change `capCell` to `return text` unconditionally and change the `BlobTypeName` branch in `renderCell` to `return string(*value), nil`. **Also remove the `unicode/utf8` import**, which `capCell` was its only user — leaving it makes the run fail with `vet: result.go:9:2: "unicode/utf8" imported and not used` instead of showing the two failures you are looking for. Then run:

Run: `go test -run 'TestScanRowsWithTypesRendersBlob|TestScanRowsWithTypesCaps' ./internal/database/ -v`
Expected: `TestScanRowsWithTypesRendersBlobPlaceholder` FAILS with `binary BLOB cell = "hello", want "<BLOB 5 bytes>"` and `TestScanRowsWithTypesCapsLongCellsByRunes` FAILS with the uncapped 12-rune string.

Restore both edits with `git checkout -- internal/database/result.go` before continuing. Do not commit the broken form.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestScanRowsWithTypes|TestDefaultMaxCellRunes|TestRenderOptionsFor' ./internal/database/ -v`
Expected: all PASS.

- [ ] **Step 5: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/database/result_test.go
git commit -m "test: cover BLOB placeholders and the rendered cell display cap"
```

---

### Task 3: The partial-result contract

Spec §6.3's "Partial-result contract". This is the load-bearing departure from `ScanRows`, which "returns `nil, err` and discards every scanned row". Without it, the BLOB-limit entry of User-Visible Behavior — "The pane shows the rows fetched before the failure" — is unimplementable.

**Where the failure actually surfaces, so the implementer does not chase the wrong branch.** `sql.Rows.Next()` calls the driver's `Next`; on a non-`io.EOF` error it records the error and returns `false`, so the loop exits normally and the error appears from `rows.Err()`. A `rows.Scan` failure is a different branch, reached when a value cannot convert into its destination. Both are return points that must carry the partial result, and both are tested below.

**Files:**
- Test: `internal/database/result_test.go`
- Test: `internal/database/scan_row_test.go`

**Interfaces:**
- Consumes: `ScanRowsWithTypes`, `resultTestRows` (with its `failAt`/`failErr` fields), `openResultTestDB`, `scanFixture` (Task 1).
- Produces: no new names. It pins the contract every later task and Plan 3 rely on.

- [ ] **Step 1: Write the failing tests**

Append to `internal/database/result_test.go`:

```go
var errFetchTest = errors.New("interbase: BLOB result exceeds the materialization limit")

func newFailingFetchFixture() *resultTestRows {
	rows := newResultTestRows(
		[]resultTestColumn{
			{name: "ID", databaseType: "INTEGER", scanType: reflect.TypeOf(int64(0))},
			{name: "BODY", databaseType: "BLOB", scanType: reflect.TypeOf([]byte(nil))},
		},
		[][]driver.Value{
			{int64(1), []byte("one")},
			{int64(2), []byte("two")},
			{int64(3), []byte("three")},
		},
	)
	// The third Next fails instead of yielding a row, which is what an
	// oversized BLOB does: a hard fetch error, not a truncated value.
	rows.failAt = 2
	rows.failErr = errFetchTest
	return rows
}

func TestScanRowsWithTypesReturnsPartialRowsOnFetchFailure(t *testing.T) {
	result, err := scanFixture(t, newFailingFetchFixture, RenderOptions{DistinguishNull: true})

	if !errors.Is(err, errFetchTest) {
		t.Fatalf("error = %v, want %v", err, errFetchTest)
	}
	if result == nil {
		t.Fatal("result = nil alongside a fetch error, want the rows scanned before it")
	}
	if got := len(result.Rows); got != 2 {
		t.Fatalf("len(Rows) = %d, want the 2 rows that preceded the failure", got)
	}
	want := [][]string{
		{"1", "<BLOB 3 bytes>"},
		{"2", "<BLOB 3 bytes>"},
	}
	if !reflect.DeepEqual(result.Rows, want) {
		t.Errorf("Rows = %#v, want %#v", result.Rows, want)
	}
	if result.Complete {
		t.Error("Complete = true after a fetch failure, want false")
	}
	// Column metadata is available before the first row, so a caller can still
	// render a header and still tell that a BLOB column was involved.
	if got := len(result.Columns); got != 2 {
		t.Fatalf("len(Columns) = %d, want 2", got)
	}
	if got := result.Columns[1].DatabaseTypeName; got != "BLOB" {
		t.Errorf("Columns[1].DatabaseTypeName = %q, want %q", got, "BLOB")
	}
}

func TestScanRowsWithTypesReturnsPartialRowsOnScanFailure(t *testing.T) {
	// A value that cannot convert into its column's destination fails inside
	// rows.Scan rather than rows.Err, which is the other return point that has
	// to carry the partial result.
	result, err := scanFixture(t, func() *resultTestRows {
		return newResultTestRows(
			[]resultTestColumn{{name: "N", databaseType: "INTEGER", scanType: reflect.TypeOf(int64(0))}},
			[][]driver.Value{{int64(1)}, {"not a number"}},
		)
	}, RenderOptions{})

	if err == nil {
		t.Fatal("error = nil, want a scan conversion failure")
	}
	if result == nil {
		t.Fatal("result = nil alongside a scan error, want the row scanned before it")
	}
	if want := [][]string{{"1"}}; !reflect.DeepEqual(result.Rows, want) {
		t.Errorf("Rows = %#v, want %#v", result.Rows, want)
	}
	if result.Complete {
		t.Error("Complete = true after a scan failure, want false")
	}
}

func TestScanRowsWithTypesReturnsNilOnlyWhenColumnTypesFails(t *testing.T) {
	// The single case with nothing to report: the metadata call itself fails,
	// so there are no columns and no rows to hand back.
	//
	// This test pins the (nil, err) BOUNDARY rather than a change, so unlike
	// its two siblings it stays green under the "return nil, err" mutation in
	// Step 4 — do not read its passing as evidence for the partial-result
	// contract.
	//
	// It also depends on (*sql.Rows).ColumnTypes returning an error after
	// Close, which is database/sql behaviour rather than a documented
	// contract (verified on Go 1.27.1). If a future toolchain makes
	// ColumnTypes succeed on closed rows, this test fails at "error = nil"
	// and the fix is a fixture whose ColumnTypeScanType path errors, not
	// deleting the assertion.
	db := openResultTestDB(t, func() *resultTestRows {
		return newResultTestRows(
			[]resultTestColumn{{name: "N", databaseType: "INTEGER", scanType: reflect.TypeOf(int64(0))}},
			[][]driver.Value{{int64(1)}},
		)
	})
	rows, err := db.QueryContext(context.Background(), "SELECT")
	if err != nil {
		t.Fatalf("QueryContext() error = %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("rows.Close() error = %v", err)
	}

	result, err := ScanRowsWithTypes(rows, RenderOptions{})
	if err == nil {
		t.Fatal("error = nil for closed rows, want a ColumnTypes failure")
	}
	if result != nil {
		t.Errorf("result = %#v, want nil when ColumnTypes fails", result)
	}
}
```

Append to `internal/database/scan_row_test.go`:

```go
func TestScanRowsDiscardsPartialRows(t *testing.T) {
	// Characterisation test, deliberately green. It pins the behaviour
	// ScanRowsWithTypes was introduced to replace, so that
	// TestScanRowsWithTypesReturnsPartialRowsOnFetchFailure is demonstrably a
	// change rather than a restatement: on the same fixture ScanRows throws
	// away both rows it scanned and returns a nil slice.
	stringRows, err := scanLegacyFailingFetch(t)
	if !errors.Is(err, errFetchTest) {
		t.Fatalf("ScanRows() error = %v, want %v", err, errFetchTest)
	}
	if stringRows != nil {
		t.Errorf("ScanRows() rows = %#v, want nil — it discards what it scanned", stringRows)
	}
}

func scanLegacyFailingFetch(t *testing.T) ([][]string, error) {
	t.Helper()
	db := openResultTestDB(t, newFailingFetchFixture)
	rows, err := db.QueryContext(context.Background(), "SELECT")
	if err != nil {
		t.Fatalf("QueryContext() error = %v", err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := Columns(rows)
	if err != nil {
		t.Fatalf("Columns() error = %v", err)
	}
	return ScanRows(rows, len(columns))
}
```

`errors` and `context` are already imported by `scan_row_test.go`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'PartialRows|ColumnTypesFails|DiscardsPartialRows' ./internal/database/ -count=1 -v`
Expected:

- `TestScanRowsWithTypesReturnsPartialRowsOnFetchFailure` FAILS at `result = nil alongside a fetch error` **if** Task 1's implementation was written with `return nil, err`. With Task 1 as specified it PASSES.
- `TestScanRowsWithTypesReturnsPartialRowsOnScanFailure` and `TestScanRowsWithTypesReturnsNilOnlyWhenColumnTypesFails` likewise PASS against Task 1 as specified.
- `TestScanRowsDiscardsPartialRows` PASSES, which is the point: it records that today's `ScanRows` returns `(nil, err)` for the identical fixture.

**These tests are not vacuous even when green on first run, and here is how to confirm it rather than take it on trust.** In `internal/database/result.go`, temporarily change both `return result, err` statements inside the scan loop and the `rows.Err()` branch to `return nil, err`, then run the same command.

Expected after that edit: `TestScanRowsWithTypesReturnsPartialRowsOnFetchFailure` FAILS with `result = nil alongside a fetch error, want the rows scanned before it`, and `TestScanRowsWithTypesReturnsPartialRowsOnScanFailure` FAILS with `result = nil alongside a scan error`. `TestScanRowsDiscardsPartialRows` still passes, because it tests the old function. That is the demonstration: with the old contract the two new tests fail, and they fail on exactly the assertion the contract exists for.

Restore with `git checkout -- internal/database/result.go`. If the tests keep passing with `return nil, err` in place, something is wrong with the fixture — most likely `failAt` is never reached — and you must fix it before continuing.

- [ ] **Step 3: Run the tests to verify they pass**

Run: `go test -run 'PartialRows|ColumnTypesFails|DiscardsPartialRows' ./internal/database/ -count=1 -v`
Expected: all four PASS with `result.go` restored.

- [ ] **Step 4: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/database/result_test.go internal/database/scan_row_test.go
git commit -m "test: pin the partial-result contract against the discarding ScanRows"
```

---

### Task 4: Render the results pane from `*QueryResult`

Spec §6.3's "`s.query` must render a non-nil result **before** reporting the error" and the "BLOB exceeds the 64 MiB materialisation limit" entry of User-Visible Behavior. `s.query` (`internal/handler/execute_command.go:279`) today calls `database.Columns` and `database.ScanRows` and returns `"", err` on any failure, which is the second half of the discarding behaviour.

`s.query` keeps returning a bare error whenever the failure is a cancellation, so Plan 1's `cancellationNotice` still fires and the cancelled/late-cancelled messages are unaffected.

**Files:**
- Modify: `internal/handler/execute_command.go:279-334`
- Modify: `internal/handler/concurrency_test.go` (Plan 1's fixture: failing-fetch rows and BLOB column types)
- Test: `internal/handler/execute_command_test.go`

**Interfaces:**
- Consumes: `database.ScanRowsWithTypes`, `database.QueryResult`, `database.RenderOptionsFor` (Task 1); `installStubBackend`, `stubConnections`, `testFileURI`, `tx.textDocumentDidOpen`, `tx.addWorkspaceConfig` (Plan 1 Task 4); `cancellationNotice` (Plan 1 Task 9).
- Produces:
  - `func (s *Server) queryResult(ctx context.Context, query string) (*database.QueryResult, error)`
  - `func renderQueryResult(result *database.QueryResult, vertical bool, scanErr error) (string, error)`
  - `func hasBlobColumn(result *database.QueryResult) bool`
  - `const blobLimitHint string`
  - Test-only: `const failFetchMarker = "fail_fetch"`, `const failBlobFetchMarker = "fail_blob_fetch"` in `concurrency_test.go`.

- [ ] **Step 1: Extend Plan 1's stub driver so a fetch can fail**

Plan 1's `stubSQLRows` (`internal/handler/concurrency_test.go`) always yields exactly one row and reports no column types. Three additive changes, none of which alter the existing behaviour for a query containing neither marker.

Replace `stubSQLConn.Prepare` and `stubSQLStmt` with versions that keep the statement text:

```go
func (stubSQLConn) Prepare(query string) (driver.Stmt, error) { return stubSQLStmt{query: query}, nil }

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
```

Replace `stubSQLRows` with:

```go
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
```

Add `"strings"` to the file's imports; `errors` and `io` are already there.

`Test_executeQuery` still expects one row containing `42` and a `1 rows in set` footer, which the no-marker branch still produces. To be precise about which version of that test: **Plan 1's Task 4 rewrites it**, replacing today's body whose `workspace/executeCommand` call is commented out (`execute_command_test.go:53-59`) and which asserts nothing about rendering. Plan 1 is a stated prerequisite of this plan, so the assertions are in place by the time you get here — but do not go looking for them in the current file first and conclude they are missing.

- [ ] **Step 2: Write the failing tests**

Append to `internal/handler/execute_command_test.go`:

```go
func TestQueryRendersPartialResultBeforeReportingError(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT fail_fetch FROM t;")

	var got string
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExecuteQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call workspace/executeCommand:", err)
	}

	if !strings.Contains(got, "42") {
		t.Errorf("result = %q, want the rows that preceded the failure", got)
	}
	if !strings.Contains(got, "2 rows in set (incomplete)") {
		t.Errorf("result = %q, want the incomplete row-count footer", got)
	}
	if !strings.Contains(got, "Fetch failed: interbase: BLOB result exceeds the materialization limit") {
		t.Errorf("result = %q, want the driver's error text passed through verbatim", got)
	}
}

func TestBlobLimitHintOnlyWithBlobColumn(t *testing.T) {
	const hintFragment = "larger than the driver's 64 MiB"

	for _, tt := range []struct {
		name     string
		text     string
		wantHint bool
	}{
		{name: "blob column", text: "SELECT fail_blob_fetch FROM t;", wantHint: true},
		{name: "no blob column", text: "SELECT fail_fetch FROM t;", wantHint: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tx := newTestContext()
			tx.setup(t)
			defer tx.tearDown()
			defer tx.server.worker.Stop()

			installStubBackend(t)
			tx.addWorkspaceConfig(t, stubConnections("primary"))
			tx.textDocumentDidOpen(t, testFileURI, tt.text)

			var got string
			if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
				Command:   CommandExecuteQuery,
				Arguments: []interface{}{testFileURI},
			}, &got); err != nil {
				t.Fatal("conn.Call workspace/executeCommand:", err)
			}

			if strings.Contains(got, hintFragment) != tt.wantHint {
				t.Errorf("result = %q, want BLOB hint present = %v", got, tt.wantHint)
			}
			// The failure itself is reported either way; only the advice is
			// conditional, so the advice is never wrong.
			if !strings.Contains(got, "Fetch failed:") {
				t.Errorf("result = %q, want the fetch failure reported", got)
			}
		})
	}
}

func TestQuerySucceedsWithCompleteFooter(t *testing.T) {
	// Regression guard for the footer wording: a complete result must keep the
	// exact "%d rows in set" text the existing pane and tests rely on.
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	var got string
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExecuteQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call workspace/executeCommand:", err)
	}
	if !strings.Contains(got, "1 rows in set") {
		t.Errorf("result = %q, want the complete row-count footer", got)
	}
	if strings.Contains(got, "(incomplete)") {
		t.Errorf("result = %q, want no incomplete marker on a complete result", got)
	}
	if strings.Contains(got, "Fetch failed:") {
		t.Errorf("result = %q, want no fetch failure on a complete result", got)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test -run 'TestQueryRendersPartialResult|TestBlobLimitHint|TestQuerySucceedsWithCompleteFooter' ./internal/handler/ -count=1 -v`
Expected: `TestQueryRendersPartialResultBeforeReportingError` and both `TestBlobLimitHintOnlyWithBlobColumn` subtests FAIL with `conn.Call workspace/executeCommand: interbase: BLOB result exceeds the materialization limit` — today `s.query` returns `"", err` on a scan failure and `executeQuery` propagates it, so the command produces a JSON-RPC error and no pane text at all. `TestQuerySucceedsWithCompleteFooter` PASSES already and is the regression guard.

- [ ] **Step 4: Write the minimal implementation**

In `internal/handler/execute_command.go`, replace `s.query` (`:279-334`) with:

```go
func (s *Server) query(ctx context.Context, query string, vertical bool) (string, error) {
	result, scanErr := s.queryResult(ctx, query)
	if result == nil {
		return "", scanErr
	}
	// A cancelled fetch is reported by the caller, which renders the
	// cancellation notice; partial rows under that notice would suggest the
	// statement produced a result when it was stopped.
	if scanErr != nil && cancellationNotice(ctx, scanErr) != "" {
		return "", scanErr
	}
	return renderQueryResult(result, vertical, scanErr)
}

func (s *Server) queryResult(ctx context.Context, query string) (*database.QueryResult, error) {
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := repo.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return database.ScanRowsWithTypes(rows, database.RenderOptionsFor(repo.Driver()))
}

// blobLimitHint is emitted only when the result actually has a BLOB column, so
// the advice is never wrong. The driver's own error text is passed through
// verbatim beside it and is never matched on.
const blobLimitHint = `One or more BLOB columns in this result are larger than the driver's 64 MiB
materialisation limit. Re-run the query without the BLOB column, or select a
substring of it.`

// renderQueryResult writes the header, the rows that were scanned, the
// row-count footer and any notes. scanErr is non-nil when the fetch died
// partway: the rows that preceded it are still rendered, because knowing which
// row broke is the fastest route to the value that broke it.
func renderQueryResult(result *database.QueryResult, vertical bool, scanErr error) (string, error) {
	columns := make([]string, len(result.Columns))
	for i, column := range result.Columns {
		columns[i] = column.Name
	}

	buf := new(bytes.Buffer)
	if vertical {
		table := newVerticalTableWriter(buf)
		table.setHeaders(columns)
		for _, stringRow := range result.Rows {
			table.appendRow(stringRow)
		}
		table.render()
	} else {
		table := tablewriter.NewTable(buf, tablewriter.WithHeaderConfig(tw.CellConfig{
			Formatting: tw.CellFormatting{AutoFormat: tw.Off},
		}))
		headers := make([]any, len(columns))
		for i, v := range columns {
			headers[i] = v
		}
		table.Header(headers...)
		for _, stringRow := range result.Rows {
			row := make([]any, len(stringRow))
			for i, v := range stringRow {
				row[i] = v
			}
			if err := table.Append(row...); err != nil {
				return "", err
			}
		}
		if err := table.Render(); err != nil {
			return "", err
		}
	}

	if result.Complete {
		fmt.Fprintf(buf, "%d rows in set", len(result.Rows))
	} else {
		fmt.Fprintf(buf, "%d rows in set (incomplete)", len(result.Rows))
	}
	fmt.Fprintln(buf, "")
	fmt.Fprintln(buf, "")

	notes := append([]string(nil), result.Notes...)
	if scanErr != nil {
		notes = append(notes, fmt.Sprintf("Fetch failed: %v", scanErr))
		if hasBlobColumn(result) {
			notes = append(notes, blobLimitHint)
		}
	}
	for _, note := range notes {
		fmt.Fprintln(buf, note)
		fmt.Fprintln(buf, "")
	}
	return buf.String(), nil
}

func hasBlobColumn(result *database.QueryResult) bool {
	for _, column := range result.Columns {
		// database.BlobTypeName, never a second "BLOB" literal: the same
		// value gates renderCell's placeholder, and two copies in two
		// packages would drift silently.
		if column.DatabaseTypeName == database.BlobTypeName {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -run 'TestQueryRendersPartialResult|TestBlobLimitHint|TestQuerySucceedsWithCompleteFooter|Test_executeQuery' ./internal/handler/ -count=1 -v`
Expected: all PASS, including the unchanged `Test_executeQuery`.

- [ ] **Step 6: Confirm no caller still uses the discarding path**

Run: `grep -rn 'database.ScanRows(\|database.Columns(' internal/handler --include='*.go'`
Expected: no hits. Both helpers now have callers only inside `internal/database` and its tests.

- [ ] **Step 7: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 8: Commit**

```bash
git add internal/handler/execute_command.go internal/handler/execute_command_test.go internal/handler/concurrency_test.go
git commit -m "feat: render partial results and the BLOB-limit hint in the results pane"
```

---

### Task 5: `ReadOnlyQuerier` and the InterBase read-only transaction

Spec §6.2. The capability interface is driver-neutral and lives in `internal/database/database.go`; the implementation lives in the **untagged** `interbase_common.go` because it needs nothing beyond `database/sql`. The driver accepts these options: `validateTxOptions` (`../interbase-go/interbase.go:671-675`) admits `LevelReadCommitted`, and the TPB builder sets the read-only flag from `options.ReadOnly` (`interbase.go:332`) and read-committed isolation (`interbase.go:341`).

The result is fully materialised before the method returns, so the transaction's lifetime never escapes the repository and an early return in the handler cannot leak it.

**Files:**
- Modify: `internal/database/database.go:24-36`
- Modify: `internal/database/interbase_common.go` (append after `Query`, `:506`)
- Test: `internal/database/interbase_readonly_test.go` (create)

**Interfaces:**
- Consumes: `QueryResult`, `RenderOptions`, `RenderOptionsFor`, `ScanRowsWithTypes` (Task 1); the partial contract (Task 3).
- Produces:
  - `type ReadOnlyQuerier interface { QueryReadOnly(ctx context.Context, query string) (*QueryResult, error) }`
  - `func (db *InterBaseDBRepository) QueryReadOnly(ctx context.Context, query string) (*QueryResult, error)`

- [ ] **Step 1: Write the failing tests**

Create `internal/database/interbase_readonly_test.go`:

```go
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

func (r *txRecorderRows) Columns() []string                            { return []string{"CODE"} }
func (r *txRecorderRows) Close() error                                 { return nil }
func (r *txRecorderRows) ColumnTypeDatabaseTypeName(int) string        { return "VARCHAR" }
func (r *txRecorderRows) ColumnTypeScanType(int) reflect.Type          { return reflect.TypeOf("") }
func (r *txRecorderRows) ColumnTypeNullable(int) (bool, bool)          { return true, true }

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
```

Run `gofmt -w internal/database/interbase_readonly_test.go` after pasting. The
`txRecorderRows` one-line method block above is not column-aligned as written
here, and `gofmt` realigns it silently — doing it now rather than discovering it
in CI.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestInterBaseQueryReadOnly' ./internal/database/ -count=1 -v`
Expected: FAIL to compile — `undefined: ReadOnlyQuerier` and `repository.QueryReadOnly undefined`.

- [ ] **Step 3: Write the minimal implementation**

In `internal/database/database.go`, add below the `DBRepository` interface (after `:36`):

```go
// ReadOnlyQuerier is an optional repository capability: a repository that can
// run a read statement inside an explicit read-only transaction and materialise
// the whole result itself. Handlers type-assert it rather than checking the
// driver name, so any driver that later implements it gets the behaviour for
// free.
type ReadOnlyQuerier interface {
	QueryReadOnly(ctx context.Context, query string) (*QueryResult, error)
}
```

In `internal/database/interbase_common.go`, append after `Query` (`:506`):

```go
// QueryReadOnly runs a read statement inside an explicit read-only,
// read-committed transaction and materialises the whole result before
// returning, so the transaction's lifetime never escapes this method and an
// early return in the handler cannot leak it.
//
// EXECUTE PROCEDURE deliberately never reaches this path: per the driver's
// documented boundary an implicit procedure query commits its write
// transaction, so a procedure call is a write even when it returns a row.
func (db *InterBaseDBRepository) QueryReadOnly(ctx context.Context, query string) (*QueryResult, error) {
	if db == nil || db.Conn == nil {
		return nil, errors.New("interbase: database connection is nil")
	}

	tx, err := db.Conn.BeginTx(ctx, &sql.TxOptions{
		ReadOnly:  true,
		Isolation: sql.LevelReadCommitted,
	})
	if err != nil {
		return nil, err
	}
	// A read-only transaction is released by rolling it back; there is nothing
	// to commit, and the rollback must run on every path including a partial
	// fetch.
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return ScanRowsWithTypes(rows, RenderOptionsFor(dialect.DatabaseDriverInterBase))
}

var _ ReadOnlyQuerier = (*InterBaseDBRepository)(nil)
```

`context`, `database/sql`, `errors` and `dialect` are already imported by that file.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestInterBaseQueryReadOnly' ./internal/database/ -count=1 -v`
Expected: all four PASS.

- [ ] **Step 5: Confirm the untagged build is unaffected and the tagged build still compiles**

Run: `go build ./... && CGO_ENABLED=1 go build -tags interbase ./...`
Expected: both succeed. If the InterBase SDK is unavailable in this environment, record that the tagged build could not be verified and report it — do not weaken the code to make the untagged build pass.

- [ ] **Step 6: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/database/database.go internal/database/interbase_common.go internal/database/interbase_readonly_test.go
git commit -m "feat: run InterBase read statements in an explicit read-only transaction"
```

---

### Task 6: Prefer `ReadOnlyQuerier` in the results pane

Spec §6.2's "`s.query` uses it when the assertion succeeds and falls back to the current `repo.Query` path otherwise". The fallback keeps `ScanRowsWithTypes` — one renderer serves every driver, so the partial-result contract is uniform rather than InterBase-only.

**Files:**
- Modify: `internal/handler/execute_command.go` (`queryResult`)
- Modify: `internal/handler/concurrency_test.go` (a stub repository that implements `ReadOnlyQuerier`)
- Test: `internal/handler/execute_command_test.go`

**Interfaces:**
- Consumes: `database.ReadOnlyQuerier`, `(*InterBaseDBRepository).QueryReadOnly` as the shape to mirror (Task 5); `s.queryResult`, `renderQueryResult` (Task 4); Plan 1's `stubBackend`/`stubRepository`.
- Produces:
  - Test-only: `func (b *stubBackend) enableReadOnlyQuerier()`, `func (b *stubBackend) readOnlyQueries() []string`, `type readOnlyStubRepository`.

- [ ] **Step 1: Extend Plan 1's fixture with a read-only repository**

In `internal/handler/concurrency_test.go`, add to `stubBackend`:

```go
	readOnly        bool
	readOnlyServed  []string
```

and the accessors:

```go
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
```

Add the wrapper type below `stubRepository`:

```go
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
```

In the `RegisterFactory(stubDriverName, ...)` body, wrap when enabled:

```go
		repository := &stubRepository{
			MockDBRepository: database.NewMockDBRepository(db).(*database.MockDBRepository),
			backend:          b,
			db:               db,
		}
		if b.readOnlyEnabled() {
			return &readOnlyStubRepository{stubRepository: repository}
		}
		return repository
```

- [ ] **Step 2: Write the failing tests**

Append to `internal/handler/execute_command_test.go`:

```go
func TestExecuteQueryUsesReadOnlyTransactionForSelect(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	backend.enableReadOnlyQuerier()
	tx.addWorkspaceConfig(t, stubConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	var got string
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExecuteQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call workspace/executeCommand:", err)
	}

	readOnly := backend.readOnlyQueries()
	if len(readOnly) != 1 {
		t.Fatalf("QueryReadOnly served %d statements, want 1", len(readOnly))
	}
	if !strings.Contains(readOnly[0], "SELECT 1") {
		t.Errorf("QueryReadOnly received %q, want the SELECT statement", readOnly[0])
	}
	if !strings.Contains(got, "42") || !strings.Contains(got, "1 rows in set") {
		t.Errorf("result = %q, want the rendered table and footer", got)
	}
}

func TestExecuteQueryFallsBackWhenReadOnlyQuerierAbsent(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	var got string
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExecuteQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call workspace/executeCommand:", err)
	}

	if served := backend.readOnlyQueries(); len(served) != 0 {
		t.Errorf("QueryReadOnly served %d statements on a repository without the capability, want 0", len(served))
	}
	if queries := backend.queries(); len(queries) != 1 {
		t.Fatalf("Query served %d statements, want 1", len(queries))
	}
	if !strings.Contains(got, "42") || !strings.Contains(got, "1 rows in set") {
		t.Errorf("result = %q, want the rendered table and footer", got)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test -run 'TestExecuteQueryUsesReadOnlyTransaction|TestExecuteQueryFallsBackWhenReadOnly' ./internal/handler/ -count=1 -v`
Expected: `TestExecuteQueryUsesReadOnlyTransactionForSelect` FAILS with `QueryReadOnly served 0 statements, want 1` — `queryResult` calls `repo.Query` unconditionally. `TestExecuteQueryFallsBackWhenReadOnlyQuerierAbsent` PASSES already and is the regression guard for the fallback.

- [ ] **Step 4: Write the minimal implementation**

In `internal/handler/execute_command.go`, replace `queryResult`:

```go
// queryResult materialises a read statement's result. It prefers an explicit
// read-only transaction when the repository offers one; that transaction's
// lifetime stays inside the repository, so an early return here cannot leak it.
// Every path renders through ScanRowsWithTypes, so the partial-result contract
// is the same on every driver.
func (s *Server) queryResult(ctx context.Context, query string) (*database.QueryResult, error) {
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return nil, err
	}
	if readOnly, ok := repo.(database.ReadOnlyQuerier); ok {
		return readOnly.QueryReadOnly(ctx, query)
	}
	rows, err := repo.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return database.ScanRowsWithTypes(rows, database.RenderOptionsFor(repo.Driver()))
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -run 'TestExecuteQuery' ./internal/handler/ -count=1 -v`
Expected: both new tests and `Test_executeQuery` PASS.

- [ ] **Step 6: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/handler/execute_command.go internal/handler/execute_command_test.go internal/handler/concurrency_test.go
git commit -m "feat: prefer a read-only transaction for read statements in the results pane"
```

---

### Task 7: Parse the procedure name out of an `EXECUTE PROCEDURE` statement

Spec §6.4 needs "the identifier following `EXECUTE PROCEDURE`". The spec obtains it "from the already-parsed statement", which in the spec's design means the `"EXECUTE": {"PROCEDURE"}` entry in `parser/parser.go`'s `multiKeywordMap` — **and that parser change belongs to Plan 3**, not here (spec: "Plan 3 — Contents: features 1–4 — … completion candidates and the `EXECUTE PROCEDURE` parser change").

So this plan reads the statement text instead. That keeps §6.4 independent of Plan 3 and, more importantly, keeps it out of shared parser state. Splitting the name extraction into its own task is what makes it unit-testable without a catalog, which is the whole reason Task 8 can be a thin, blocked task.

**Files:**
- Modify: `internal/handler/execute_command.go`
- Test: `internal/handler/interbase_procedure_test.go` (create)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func interBaseProcedureName(query string) string` — the procedure name for an `EXECUTE PROCEDURE` statement, `""` for anything else.

- [ ] **Step 1: Write the failing test**

Create `internal/handler/interbase_procedure_test.go`:

```go
package handler

import "testing"

func TestInterBaseProcedureName(t *testing.T) {
	for _, tt := range []struct {
		name  string
		query string
		want  string
	}{
		{name: "call with parenthesised arguments", query: "EXECUTE PROCEDURE MYPROC(1, 2)", want: "MYPROC"},
		{name: "call with no parenthesis", query: "EXECUTE PROCEDURE MYPROC 1 2", want: "MYPROC"},
		{name: "no arguments", query: "EXECUTE PROCEDURE MYPROC", want: "MYPROC"},
		{name: "trailing semicolon", query: "EXECUTE PROCEDURE MYPROC;", want: "MYPROC"},
		{name: "lower case keywords and name", query: "execute procedure myproc(1)", want: "myproc"},
		{name: "quoted name", query: `EXECUTE PROCEDURE "MyProc"(1)`, want: "MyProc"},
		{name: "dollar in name", query: "EXECUTE PROCEDURE MY$PROC", want: "MY$PROC"},
		{name: "extra whitespace", query: "EXECUTE\tPROCEDURE\n  MYPROC", want: "MYPROC"},
		{name: "not a procedure call", query: "SELECT * FROM MYPROC", want: ""},
		{name: "execute without procedure", query: "EXECUTE STMT", want: ""},
		{name: "execute procedure with no name", query: "EXECUTE PROCEDURE", want: ""},
		{name: "empty", query: "", want: ""},
		// Known limitation, recorded rather than worked around: a Dialect-3
		// quoted name containing whitespace does not survive field splitting.
		// It degrades to "", which routes to Exec — today's behaviour — rather
		// than to a wrong decision.
		{name: "quoted name with a space", query: `EXECUTE PROCEDURE "My Proc"(1)`, want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := interBaseProcedureName(tt.query); got != tt.want {
				t.Errorf("interBaseProcedureName(%q) = %q, want %q", tt.query, got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestInterBaseProcedureName ./internal/handler/ -v`
Expected: FAIL to compile — `undefined: interBaseProcedureName`.

- [ ] **Step 3: Write the minimal implementation**

In `internal/handler/execute_command.go`, add near `getStatementsWithDriver`:

```go
// interBaseProcedureName returns the procedure named by an EXECUTE PROCEDURE
// statement, or "" when the statement is not one.
//
// It reads the statement text rather than the parse tree because grouping
// EXECUTE PROCEDURE into a single ast.MultiKeyword is a change to shared,
// dialect-independent parser state that a later plan owns. Returning "" is
// always safe: it routes to Exec, which is today's unconditional behaviour.
func interBaseProcedureName(query string) string {
	fields := strings.Fields(query)
	if len(fields) < 3 {
		return ""
	}
	if !strings.EqualFold(fields[0], "EXECUTE") || !strings.EqualFold(fields[1], "PROCEDURE") {
		return ""
	}

	name := fields[2]
	if index := strings.IndexAny(name, "(;"); index >= 0 {
		name = name[:index]
	}
	if strings.HasPrefix(name, `"`) {
		// A quoted name is only recoverable here when it contains no
		// whitespace; otherwise strings.Fields has already split it and the
		// caller falls back to Exec.
		if !strings.HasSuffix(name, `"`) || len(name) < 2 {
			return ""
		}
		return name[1 : len(name)-1]
	}
	return name
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -run TestInterBaseProcedureName ./internal/handler/ -v`
Expected: every subtest PASSES.

- [ ] **Step 5: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/handler/execute_command.go internal/handler/interbase_procedure_test.go
git commit -m "feat: extract the procedure name from an EXECUTE PROCEDURE statement"
```

---

### Task 8: `EXECUTE PROCEDURE` routing by cached output arity

Spec §6.4. `"EXECUTE"` is in `execMap` (`internal/database/query_type.go:177`) and `QueryExecType` takes the first whitespace token (`:246-262`), so today **every** `EXECUTE PROCEDURE` takes the `Exec` path. The driver requires `ExecContext` for a procedure with no output and rejects `ExecContext` for one with output, so half of those statements fail today.

**The routing decides once and never retries across paths.** The spec refuses a try-then-fall-back because "a mutating procedure could execute twice if that reasoning is ever wrong, and a stale cache is not worth that risk". An unknown procedure takes `Exec` and reports the driver's rejection with a cache-refresh hint. The cost is a stale-cache papercut; the benefit is a guarantee. Do not weaken it.

> **BLOCKED until sub-project 2 lands.** This is the only task in this plan with an external dependency. Step 1 is a precondition check; if it fails, stop, report, and ship Tasks 1–7 and 9 — the spec anticipates §6.4 moving to Plan 3.

**Files:**
- Modify: `internal/handler/execute_command.go` (`runStatement`, `query`, `queryResult`)
- Modify: `internal/handler/concurrency_test.go` (a stub connection whose `DBConnection.Driver` is InterBase; a catalog-capable stub repository)
- Test: `internal/handler/interbase_procedure_test.go`

**Interfaces:**
- Consumes: `interBaseProcedureName` (Task 7); `s.queryResult`, `renderQueryResult` (Tasks 4 and 6); Plan 1's `runStatement` and `cancellationNotice`; from sub-project 2, `database.DBCache.HasCatalog() bool`, `database.DBCache.Procedure(name string) (*database.ProcedureDesc, bool)`, `database.ProcedureDesc.OutputParameters []*database.ProcedureParameterDesc`, `database.CatalogRepository`.
- Produces:
  - `func (s *Server) queryProcedure(ctx context.Context, query string, vertical bool) (string, error)`
  - `func (s *Server) renderQuery(ctx context.Context, query string, vertical, allowReadOnly bool, notes []string) (string, error)`
  - `func (s *Server) interBaseProcedureRouting(query string) procedureRouting`
  - `type procedureRouting struct { name string; returnsRows, unknown bool }`
  - `const executeProcedureOneRowNote string`, `const unknownProcedureHint string`
  - Test-only: `func stubInterBaseConnections(aliases ...string) *config.Config`, `func (b *stubBackend) setProcedures(procs []*database.ProcedureDesc)`.

- [ ] **Step 1: Verify the precondition**

Run:

```bash
go doc github.com/sqls-server/sqls/internal/database.DBCache | grep -E 'HasCatalog|Procedure\(' && \
go doc github.com/sqls-server/sqls/internal/database.ProcedureDesc | grep OutputParameters && \
go doc github.com/sqls-server/sqls/internal/database.CatalogRepository | grep DescribeProcedures
```

Expected: `func (dc *DBCache) HasCatalog() bool`, `func (dc *DBCache) Procedure(name string) (*ProcedureDesc, bool)`, `OutputParameters []*ProcedureParameterDesc`, and `DescribeProcedures(ctx context.Context) ([]*ProcedureDesc, error)`.

If any is missing, **stop**. Report that §6.4 is blocked on sub-project 2 and that Tasks 1–7 and 9 are complete. Do not stub the accessors locally: a local definition would collide with sub-project 2's when it lands.

Also confirm the accessor normalises its argument, which is what makes a user's lowercase `myproc` find the catalog's `MYPROC` (`…-interbase-dialect-and-catalog-design.md` §4.5: "Every singular accessor **normalises the name it is given**"):

Run: `grep -n 'func (dc \*DBCache) Procedure' -A 8 internal/database/cache.go`
Expected: the body upper-cases or `EqualFold`-matches the argument. If it is an exact match, that is a one-line fix in `cache.go` — make it and note it; do **not** add `strings.ToUpper` at this task's call site.

- [ ] **Step 2: Extend the fixture with an InterBase-identity connection and a catalog**

In `internal/handler/concurrency_test.go`, add:

```go
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
```

Add to `stubBackend`:

```go
	procedures []*database.ProcedureDesc
```

with:

```go
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
```

Add a catalog-capable wrapper below `readOnlyStubRepository`:

```go
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
```

Register the second sqls driver in the same `init` as the first:

```go
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
```

Add `"github.com/sqls-server/sqls/dialect"` to the file's imports.

Then apply the **same** `catalogStubRepository` wrap to the existing
`stubDriverName` factory that Plan 1 registered — the identical three lines:

```go
		if len(b.describedProcedures()) > 0 {
			return &catalogStubRepository{stubRepository: repository}
		}
		return repository
```

This is not symmetry for its own sake. `TestExecuteProcedureRoutingIgnoresNonInterBaseDrivers`
has to prove that the **driver check** stops the Query path; if the non-InterBase
connection had no catalog, routing would return `unknown` for want of a cache and
the test would pass even with the driver check deleted. Wrapping both factories
makes the driver the only difference between that test and
`TestExecuteProcedureWithOutputUsesQueryPath`.

It changes nothing for Plan 1's tests: the wrap is gated on
`len(b.describedProcedures()) > 0`, and only this plan's tests call
`setProcedures`.

- [ ] **Step 3: Write the failing tests**

Append to `internal/handler/interbase_procedure_test.go`:

```go
import (
	"database/sql"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

func testProcedures() []*database.ProcedureDesc {
	return []*database.ProcedureDesc{
		{
			Name: "MYPROC",
			InputParameters: []*database.ProcedureParameterDesc{
				{Name: "IN_CODE", Position: 0, Direction: database.ParameterInput, Type: "VARCHAR(3)"},
			},
			OutputParameters: []*database.ProcedureParameterDesc{
				{Name: "OUT_TOTAL", Position: 0, Direction: database.ParameterOutput, Type: "INTEGER", Nullable: sql.NullBool{}},
			},
		},
		{
			Name: "DOWORK",
			InputParameters: []*database.ProcedureParameterDesc{
				{Name: "IN_ID", Position: 0, Direction: database.ParameterInput, Type: "INTEGER"},
			},
		},
	}
}

func runProcedureCommand(t *testing.T, text string, procs []*database.ProcedureDesc) (string, *stubBackend) {
	t.Helper()
	tx := newTestContext()
	tx.setup(t)
	t.Cleanup(tx.tearDown)
	t.Cleanup(tx.server.worker.Stop)

	backend := installStubBackend(t)
	if procs != nil {
		backend.setProcedures(procs)
	}
	tx.addWorkspaceConfig(t, stubInterBaseConnections("interbase"))

	// The catalog lands on the worker's SECONDARY, asynchronous pass:
	// addWorkspaceConfig reaches ReCache, which only signals the worker
	// goroutine (worker.go:95-97). Issuing the command straight afterwards
	// races that goroutine, HasCatalog() is still false, and routing falls to
	// the unknown-procedure branch — so the two tests that assert the Query
	// path would fail or, worse, flake. Wait for the catalog first.
	//
	// waitForCatalog is the polling helper the catalog-migration plan adds
	// alongside GenerateCatalogCache. If that plan has not landed, add it
	// there rather than duplicating it here.
	if procs != nil {
		waitForCatalog(t, tx.server.worker)
		if !tx.server.worker.Cache().HasCatalog() {
			t.Fatal("the catalog never arrived; every routing assertion below would be vacuous")
		}
	}

	tx.textDocumentDidOpen(t, testFileURI, text)

	var got string
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExecuteQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call workspace/executeCommand:", err)
	}
	return got, backend
}

func TestExecuteProcedureWithOutputUsesQueryPath(t *testing.T) {
	got, backend := runProcedureCommand(t, "EXECUTE PROCEDURE MYPROC(1);", testProcedures())

	if queries := backend.queries(); len(queries) != 1 {
		t.Fatalf("Query served %d statements, want 1 — a procedure with output must take the Query path", len(queries))
	}
	if served := backend.readOnlyQueries(); len(served) != 0 {
		t.Errorf("QueryReadOnly served %d statements, want 0 — EXECUTE PROCEDURE is a write even when it returns a row", len(served))
	}
	if !strings.Contains(got, "EXECUTE PROCEDURE returns at most one row.") {
		t.Errorf("result = %q, want the one-row note", got)
	}
	if !strings.Contains(got, "42") {
		t.Errorf("result = %q, want the rendered output row", got)
	}
}

func TestExecuteProcedureWithoutOutputUsesExecPath(t *testing.T) {
	got, backend := runProcedureCommand(t, "EXECUTE PROCEDURE DOWORK(1);", testProcedures())

	if queries := backend.queries(); len(queries) != 0 {
		t.Fatalf("Query served %d statements, want 0 — a procedure with no output must take the Exec path", len(queries))
	}
	if !strings.Contains(got, "Query OK") {
		t.Errorf("result = %q, want the Exec result line", got)
	}
	if strings.Contains(got, "EXECUTE PROCEDURE returns at most one row.") {
		t.Errorf("result = %q, want no one-row note on the Exec path", got)
	}
}

func TestExecuteProcedureRoutingIsCaseInsensitive(t *testing.T) {
	// InterBase stores catalog names upper-cased; users type them lower-cased.
	_, backend := runProcedureCommand(t, "execute procedure myproc(1);", testProcedures())

	if queries := backend.queries(); len(queries) != 1 {
		t.Fatalf("Query served %d statements, want 1 — a lower-case name must still resolve to MYPROC", len(queries))
	}
}

func TestExecuteProcedureUnknownProcedureUsesExecAndExplainsCacheRefresh(t *testing.T) {
	got, backend := runProcedureCommand(t, "EXECUTE PROCEDURE NOSUCHPROC(1);", testProcedures())

	if queries := backend.queries(); len(queries) != 0 {
		t.Fatalf("Query served %d statements, want 0 — an unknown procedure must never be tried on both paths", len(queries))
	}
	if !strings.Contains(got, "NOSUCHPROC is not in the catalog cache") {
		t.Errorf("result = %q, want the cache-refresh hint naming the procedure", got)
	}
	if !strings.Contains(got, "switch to this connection again to refresh the cache") {
		t.Errorf("result = %q, want the refresh instruction", got)
	}
}

func TestExecuteProcedureWithoutCatalogUsesExecPath(t *testing.T) {
	// The window before the worker's catalog pass lands: HasCatalog() is false
	// and routing falls back to today's unconditional Exec, which is not a
	// regression.
	got, backend := runProcedureCommand(t, "EXECUTE PROCEDURE MYPROC(1);", nil)

	if queries := backend.queries(); len(queries) != 0 {
		t.Fatalf("Query served %d statements, want 0 without a catalog", len(queries))
	}
	if !strings.Contains(got, "Query OK") && !strings.Contains(got, "is not in the catalog cache") {
		t.Errorf("result = %q, want either the Exec result or the refresh hint", got)
	}
}

func TestExecuteProcedureRoutingIgnoresNonInterBaseDrivers(t *testing.T) {
	// This test is only meaningful if the non-InterBase connection HAS a
	// catalog. If it does not, routing returns unknown for want of a cache,
	// takes the Exec path, and the assertion below passes for the wrong
	// reason — deleting the parserDriver() check from
	// interBaseProcedureRouting would leave it green. Step 2 therefore
	// teaches the plain stubDriverName factory the same catalogStubRepository
	// wrap the InterBase one gets, so the ONLY difference between this test
	// and TestExecuteProcedureWithOutputUsesQueryPath is the driver.
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	backend.setProcedures(testProcedures())
	tx.addWorkspaceConfig(t, stubConnections("primary"))

	waitForCatalog(t, tx.server.worker)
	if !tx.server.worker.Cache().HasCatalog() {
		t.Fatal("the non-InterBase connection has no catalog; this test would pass for the wrong reason")
	}
	if _, ok := tx.server.worker.Cache().Procedure("MYPROC"); !ok {
		t.Fatal("MYPROC is not in the cache; routing would return unknown regardless of the driver")
	}

	tx.textDocumentDidOpen(t, testFileURI, "EXECUTE PROCEDURE MYPROC(1);")

	var got string
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExecuteQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call workspace/executeCommand:", err)
	}

	if queries := backend.queries(); len(queries) != 0 {
		t.Fatalf("Query served %d statements on a non-InterBase driver, want 0 — routing must be InterBase-only", len(queries))
	}
	// And no cache-refresh hint either: an unknown procedure is an InterBase
	// concept, so a non-InterBase driver must produce today's plain output.
	if strings.Contains(got, "is not in the catalog cache") {
		t.Errorf("result = %q, want no InterBase routing hint on a non-InterBase driver", got)
	}
}
```

Remove the now-duplicated bare `import "testing"` at the top of the file, merging it into the block above.

- [ ] **Step 4: Run the tests to verify they fail**

Run: `go test -run 'TestExecuteProcedure' ./internal/handler/ -count=1 -v`
Expected:

- `TestExecuteProcedureWithOutputUsesQueryPath` FAILS with `Query served 0 statements, want 1` — `"EXECUTE"` is in `execMap`, so today the statement goes to `Exec` unconditionally.
- `TestExecuteProcedureRoutingIsCaseInsensitive` FAILS the same way.
- `TestExecuteProcedureUnknownProcedureUsesExecAndExplainsCacheRefresh` FAILS on the missing hint.
- `TestExecuteProcedureWithoutOutputUsesExecPath`, `TestExecuteProcedureWithoutCatalogUsesExecPath` and `TestExecuteProcedureRoutingIgnoresNonInterBaseDrivers` PASS already: they assert today's behaviour survives, which is exactly their job.

Because those three are green before and after, they carry no evidence on their own. After Step 5 lands, prove the last one discriminates: delete the

```go
	if s.parserDriver() != dialect.DatabaseDriverInterBase {
		return procedureRouting{}
	}
```

guard from `interBaseProcedureRouting` and re-run. `TestExecuteProcedureRoutingIgnoresNonInterBaseDrivers` must then FAIL with `Query served 1 statements on a non-InterBase driver, want 0`. If it still passes, the non-InterBase connection is not catalog-bearing and Step 2's second wrap was not applied. Restore the guard with `git checkout -- internal/handler/execute_command.go` before continuing.

- [ ] **Step 5: Write the minimal implementation**

In `internal/handler/execute_command.go`, add the constants near the command constants:

```go
const executeProcedureOneRowNote = "EXECUTE PROCEDURE returns at most one row."

const unknownProcedureHint = `%s is not in the catalog cache. If it was created after this connection
opened, switch to this connection again to refresh the cache.`
```

Replace Plan 1's `runStatement` with:

```go
func (s *Server) runStatement(ctx context.Context, query string, vertical bool) (string, error) {
	if _, isQuery := database.QueryExecType(query, ""); isQuery {
		return s.query(ctx, query, vertical)
	}

	routing := s.interBaseProcedureRouting(query)
	if routing.returnsRows {
		return s.queryProcedure(ctx, query, vertical)
	}

	res, err := s.exec(ctx, query, vertical)
	// The cache did not know this procedure, so Exec was a fallback rather
	// than a decision. Say so, instead of letting the driver's rejection read
	// like a mistake in the user's statement. A cancelled statement is left
	// alone so Plan 1's cancellation notice still wins.
	if err != nil && routing.unknown && cancellationNotice(ctx, err) == "" {
		return fmt.Sprintf("Exec failed: %v\n\n"+unknownProcedureHint+"\n", err, routing.name), nil
	}
	return res, err
}

// procedureRouting is the once-and-only-once routing decision for an
// EXECUTE PROCEDURE statement.
type procedureRouting struct {
	name string
	// returnsRows is true only when the cache says the procedure has at least
	// one output parameter.
	returnsRows bool
	// unknown is true when the statement is an EXECUTE PROCEDURE call whose
	// procedure the cache could not resolve.
	unknown bool
}

// interBaseProcedureRouting decides how to run an EXECUTE PROCEDURE statement,
// once. It deliberately never tries one path and falls back to the other: the
// driver rejects the wrong path at prepare, before execution, but a
// try-then-retry shape could execute a mutating procedure twice if that
// reasoning were ever wrong, and a stale cache is not worth that risk.
func (s *Server) interBaseProcedureRouting(query string) procedureRouting {
	if s.parserDriver() != dialect.DatabaseDriverInterBase {
		return procedureRouting{}
	}
	name := interBaseProcedureName(query)
	if name == "" {
		return procedureRouting{}
	}

	cache := s.worker.Cache()
	if cache == nil || !cache.HasCatalog() {
		return procedureRouting{name: name, unknown: true}
	}
	// The accessor normalises the name it is given, so the identifier goes in
	// exactly as the user typed it.
	desc, ok := cache.Procedure(name)
	if !ok {
		return procedureRouting{name: name, unknown: true}
	}
	return procedureRouting{name: name, returnsRows: len(desc.OutputParameters) > 0}
}
```

Replace `query` and `queryResult` with the read-only-aware pair:

```go
func (s *Server) query(ctx context.Context, query string, vertical bool) (string, error) {
	return s.renderQuery(ctx, query, vertical, true, nil)
}

// queryProcedure runs an EXECUTE PROCEDURE statement that the cache says
// returns output. It never uses ReadOnlyQuerier: an implicit procedure query
// commits its write transaction, so a procedure call is a write even when it
// returns a row.
func (s *Server) queryProcedure(ctx context.Context, query string, vertical bool) (string, error) {
	return s.renderQuery(ctx, query, vertical, false, []string{executeProcedureOneRowNote})
}

func (s *Server) renderQuery(ctx context.Context, query string, vertical, allowReadOnly bool, notes []string) (string, error) {
	result, scanErr := s.queryResult(ctx, query, allowReadOnly)
	if result == nil {
		return "", scanErr
	}
	if scanErr != nil && cancellationNotice(ctx, scanErr) != "" {
		return "", scanErr
	}
	result.Notes = append(result.Notes, notes...)
	return renderQueryResult(result, vertical, scanErr)
}

func (s *Server) queryResult(ctx context.Context, query string, allowReadOnly bool) (*database.QueryResult, error) {
	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return nil, err
	}
	if readOnly, ok := repo.(database.ReadOnlyQuerier); ok && allowReadOnly {
		return readOnly.QueryReadOnly(ctx, query)
	}
	rows, err := repo.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return database.ScanRowsWithTypes(rows, database.RenderOptionsFor(repo.Driver()))
}
```

`dialect` is already imported by this file.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -run 'TestExecuteProcedure|TestInterBaseProcedureName|TestExecuteQuery' ./internal/handler/ -count=1 -v`
Expected: all PASS.

- [ ] **Step 7: Confirm the no-retry guarantee holds in the code, not only in the tests**

Run: `grep -n 'queryProcedure\|s.exec(ctx' internal/handler/execute_command.go`
Expected: `runStatement` reaches `queryProcedure` and `s.exec` from mutually exclusive branches, and no branch calls both. A statement that reached `s.exec` must never afterwards reach `s.query` or `s.queryProcedure` in the same iteration.

- [ ] **Step 8: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 9: Commit**

```bash
git add internal/handler/execute_command.go internal/handler/interbase_procedure_test.go internal/handler/concurrency_test.go
git commit -m "feat: route EXECUTE PROCEDURE by the procedure's cached output arity"
```

---

### Task 9: Document the results-pane behaviour

Spec's Documentation section: the README must cover "that query results distinguish `NULL` from the empty string; that cells are display-capped at 512 characters". The partial-result behaviour and the `EXECUTE PROCEDURE` stale-cache papercut are added for the same reason — a user who sees `(incomplete)` or a refresh hint needs to know it is designed behaviour.

This task is independent of Task 8. If §6.4 was deferred, drop the `EXECUTE PROCEDURE` paragraph and nothing else changes.

**Files:**
- Modify: `README.md` (after the `### Cancelling a running query` section Plan 1 added at the end of `### InterBase Build`)

**Interfaces:**
- Consumes: every name and message introduced by Tasks 1–8.
- Produces: no code.

- [ ] **Step 1: Add the results-pane section to `README.md`**

Insert after the `### Cancelling a running query` section:

```markdown
### Query results

Results are rendered from the column metadata the driver reports, so the pane
shows what the database actually returned.

- **`NULL` is not an empty string.** For InterBase, a SQL `NULL` renders as the
  literal `NULL` and an empty value renders as an empty cell. Other drivers are
  unchanged.
- **Exact decimals stay exact.** A scaled `NUMERIC`/`DECIMAL` column is rendered
  from the driver's exact decimal text, never through a float.
- **Large cells are display-capped at 512 characters.** A longer value is cut at
  the cap and marked `…(truncated, N characters)`, where `N` is the real length.
  This is a display limit in sqls, not data loss and not a database limit. A
  binary BLOB is shown as `<BLOB N bytes>` rather than dumped into the table.
- **A failed fetch still shows what it fetched.** If the query dies partway —
  most often because a BLOB exceeds the InterBase driver's 64 MiB
  materialisation limit, which is a hard error rather than a truncation — the
  pane shows the rows that preceded the failure, a `N rows in set (incomplete)`
  footer, and the driver's error text. When the result has a BLOB column, it
  also suggests re-running without that column or selecting a substring of it.
- **Read statements run in a read-only transaction.** For InterBase, `SELECT`
  and friends run inside an explicit read-committed, read-only transaction that
  is opened and released entirely inside sqls.

### `EXECUTE PROCEDURE`

sqls routes `EXECUTE PROCEDURE` by the procedure's output parameters as recorded
in the catalog cache: a procedure that returns output is run as a query and its
single row is shown; one that returns nothing is executed.

If the procedure is not in the cache — most often because it was created or
altered after this connection opened — sqls executes it and, if the driver
rejects that, tells you to switch to the connection again to refresh the cache.
It deliberately does **not** try the other path afterwards: retrying across paths
could run a mutating procedure twice.
```

- [ ] **Step 2: Verify the documented strings match the code**

Run:

```bash
grep -n 'truncated, %d characters\|<BLOB %d bytes>\|rows in set (incomplete)' internal/database/result.go internal/handler/execute_command.go && \
grep -n 'DefaultMaxCellRunes = 512' internal/database/result.go
```

Expected: every documented marker appears in the implementation, and the cap constant really is 512. If Task 8 was deferred, also delete the `### EXECUTE PROCEDURE` section before committing.

- [ ] **Step 3: Run the whole suite one last time**

Run: `make test-race && go test ./...`
Expected: every package `ok` in both runs.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: describe results-pane rendering and EXECUTE PROCEDURE routing"
```

---

## Out of scope for this plan

Named here so no task drifts into them:

- §6.1/§6.1bis/§6.5 — async dispatch, the cancel registry, `stateMu`/`connMu`, the `Worker.dbRepo` fix, `-race` in CI, and `ClassifyFailure`. **Plan 1, already complete.** This plan consumes them and must not re-derive them.
- The `"EXECUTE": {"PROCEDURE"}` entry in `parser/parser.go`'s `multiKeywordMap` and the `parseutil.ExecuteProcedure` syntax position — **Plan 3**. Task 7 deliberately parses the statement text instead so §6.4 does not depend on it.
- Features 1–4 — Explain, completion, signature help, hover DDL — **Plan 3**.
- Feature 5, go-to-definition snapshots — **Plan 4**.
- Live, gated InterBase tests for this plan's surfaces. `TestInterBaseLiveExecuteProcedureWithOutputIsRejectedByExec` is listed in the spec's "Live, gated" section; it needs a writable fixture database that this repository does not own, and the driver's own integration suite already covers the no-side-effect guarantee. It is deliberately not scheduled here.

## Risks

1. **The `EXECUTE PROCEDURE` multi-keyword change is not in this plan, but it is worth restating where it lands.** Plan 3 adds `"EXECUTE": {"PROCEDURE"}` to `multiKeywordMap`, which `parser.go:112` applies on **every** parse regardless of dialect. PostgreSQL's legacy `CREATE TRIGGER … FOR EACH ROW EXECUTE PROCEDURE f()` contains exactly that sequence and is still accepted by current PostgreSQL (superseded by `EXECUTE FUNCTION` in PG 11, not removed), so **PostgreSQL parsing does change**: the syntax position after those two keywords goes from `Unknown` to `ExecuteProcedure`. The spec's mitigation is to retain `CompletionTypeKeyword` in that branch so no PostgreSQL user loses a candidate they get today. It is not true that no other dialect is affected. Task 7 keeps *this* plan clear of that blast radius by reading the statement text, which is the reason it is a separate task.
2. **Routing depends on a cache that can be stale.** A procedure created or altered after connect routes on old output arity. The chosen behaviour fails safely — one rejected statement and a clear refresh instruction — rather than guessing, but it is a real papercut and the spec accepts it knowingly.
3. **There are two cross-cutting rendering changes, not one, and both affect every driver.**

   **The display cap.** A cell over 512 characters is now truncated where `ScanRows` rendered it whole. The spec's own comment ("The zero value reproduces the existing ScanRows output exactly") and its rule that the cap applies to "any cell over `MaxCellRunes` characters" cannot both hold; the cap wins, because it is the one the spec's User-Visible Behavior section describes to the user.

   **NULL rendering.** A SQL NULL now renders as an empty cell where `ScanRows` renders the literal text `<nil>`. The spec's evidence line — "`sqlValToString` returns `\"\"` for a nil pointer" — is true only for a *typed* nil pointer, which is what `Test_sqlValToString_nilTypedPointer` (`scan_row_test.go:34`) covers. A real driver NULL is an **untyped** nil: `reflect.ValueOf(nil).Kind()` is `Invalid`, not `Pointer`, so the `IsNil` branch at `scan_row.go:69-72` never fires, no case in the type switch matches, and the default arm returns `fmt.Sprintf("%v", nil)` == `"<nil>"` (`scan_row.go:97-98`). Verified by direct execution, not read off the source.

   Keep the new behaviour. `<nil>` is a worse lie than an empty cell — it is indistinguishable from a string column that literally contains `<nil>` — and nothing in the repository asserts it (`grep -rn '<nil>' --include='*_test.go' .` returns nothing). But it is a change every driver's users will see, so `TestScanRowsWithTypesDefaultsPreserveExistingRendering` asserts equivalence only for the non-NULL row, asserts the `<nil>` divergence explicitly for the NULL row, and `t.Fatal`s with a "this test's premise is wrong" message if `ScanRows` ever stops producing `<nil>`. Both divergences are documented in the README rather than hidden.
4. **Task 8 is the only externally blocked work.** If sub-project 2 slips, the first two thirds of this plan ship on their own and §6.4 moves to Plan 3 exactly as the spec's plan decomposition anticipates. Nothing in Tasks 1–7 references a catalog symbol.
5. **A quoted procedure name containing whitespace is not routed.** `EXECUTE PROCEDURE "My Proc"(1)` yields no name from `interBaseProcedureName` and falls back to `Exec`, which is today's behaviour. It is pinned by a test case rather than left to be discovered.
