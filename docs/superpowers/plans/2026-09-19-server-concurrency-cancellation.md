# Server Concurrency and Cancellation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn sqls from a single-threaded language server into one that keeps answering hover, completion and formatting while a query runs, that stops a runaway query when the editor sends `$/cancelRequest`, and that reports a cancelled statement honestly in the results pane — with every mutable server field guarded and the race detector wired into CI.

**Architecture:** A `jsonrpc2.Handler` wrapper dispatches `workspace/executeCommand` onto its own goroutine and everything else inline, exactly as today. A cancel registry maps `jsonrpc2.ID` to `context.CancelFunc` so the newly readable `$/cancelRequest` notification can cancel the context that reaches the database driver. Two mutexes on `Server` guard the state that becomes shared: `connMu` serialises connection-mutating commands against in-flight database work, and `stateMu` guards short field accesses. A pre-existing unlocked write to `Worker.dbRepo` is fixed with accessors under the worker's own lock.

**Tech Stack:** Go 1.25.7, `github.com/sourcegraph/jsonrpc2@v0.2.1`, `database/sql`, `sync`, `context`, the Go race detector, GitHub Actions, `interbase-go` (only behind the `interbase` build tag).

**Spec:** `docs/superpowers/specs/2026-09-19-interbase-editor-features-design.md` — this plan implements **Plan 1 of 4** from that spec's "Plan decomposition" section: §6.1a, §6.1b, §6.1bis, §6.1c, §6.1d, §6.1e and §6.5, plus the `doc/develop.md` concurrency invariants. Features 1–5 and §6.2/§6.3/§6.4 belong to later plans and must **not** be built here.

## Global Constraints

- **Lock ordering: `connMu` before `stateMu`, never the reverse**, and `stateMu` is never held across any I/O. Copy this sentence into `doc/develop.md` verbatim (Task 11). Every task's locking decisions are subordinate to it.
- `connMu` guards *connection lifetime*. `executeQuery`, `showDatabases`, `showSchemas`, `showTables` and `showConnections` take `connMu.RLock()` for the whole of their database work including rendering; `switchDatabase`, `switchConnections`, `handleInitialize` and the reconnect branch of `handleWorkspaceDidChangeConfiguration` take `connMu.Lock()` across `reconnectionDB`.
- `stateMu` guards *mutable `Server` fields*. It is taken for short, non-blocking field accesses only. `reconnectionDB` performs `Close`, `Open` and `ReCache` holding only `connMu.Lock()`, taking `stateMu.Lock()` only for the pointer assignments.
- `getConfig()`, `topConnection()`, `getConnection()`, `parserDriver()`, `newDBRepository()` and `fileText()` take `stateMu` **internally**. Never call any of them while already holding `stateMu` — `sync.RWMutex` does not guarantee recursive `RLock` when a writer is queued.
- `Server.Stop`, `handleShutdown` and `handleExit` do **not** take `connMu`. Shutdown must never block on a runaway query, and `sql.DB.Close` is documented (`$GOROOT/src/database/sql/sql.go:925-927`) as safe to call while queries are in flight.
- Every reader of `Server.files` copies `File.Text` into a local `string` **while still holding the lock** and never retains the `*File`. `updateFile` mutates `f.Text` through the stored pointer (`internal/handler/handler.go:301`).
- `SpecificFileCfg` and `DefaultFileCfg` are written once in `main.go:133`/`:140`, **before** `jsonrpc2.NewConn` at `main.go:151`, and are never written again. This is a stated invariant, not a lock. Any future writer after serving begins must take `stateMu`.
- Any new `Server` field added by a later plan must be added to the audit table in `doc/develop.md` and classified.
- Hover, completion, signature help, definition, formatting and rename stay **inline**. Only `workspace/executeCommand` is dispatched asynchronously.
- No timeout is added anywhere. The driver's cancellation is best effort; a sqls-side timeout would imply a guarantee the stack cannot make.
- Driver imports stay behind `//go:build interbase && cgo && linux && amd64`. Exactly one new tagged/untagged file pair is added, for `ClassifyFailure`.
- No error string is ever matched. `ClassifyFailure` uses `errors.As` against the driver's exported error types.
- Verification commands: `go test ./...`, `make test-race` (added in Task 1), and for tagged code `CGO_ENABLED=1 go build -tags interbase ./...`.

**Baseline fact, verified at HEAD before this plan was written:** `go test -race ./...` is **already green** on `master`. The `Worker.dbRepo` race described in spec §6.1d is real but *latent* — no existing test calls `ReCache` twice against a busy worker, so the detector never sees it. This is why Task 1 (turn on `-race`) can safely come first and Task 2 (fix the worker) is the task that surfaces and fixes the race. Do not assume the first `-race` run fails; if it does, stop and report, because something changed since this plan was written.

**Racy window, stated on purpose:** Task 5 makes `workspace/executeCommand` concurrent before Tasks 6–8 add the locks. The tests written in Task 5 deliberately use only *read-only* inline requests, so `go test -race ./...` stays green at every commit boundary. Tasks 5–8 must land as one contiguous run; do not stop after Task 5 and ship.

## File Structure

| File | Status | Responsibility |
| --- | --- | --- |
| `Makefile` | Modify (`:35-37`) | add a `test-race` target |
| `.github/workflows/test.yaml` | Modify (`:20-21`) | add a race-detector step |
| `internal/database/worker.go` | Modify | `repo()`/`setRepo()` accessors under `w.lock`; idempotent `Stop` |
| `internal/database/worker_test.go` | Create | worker race and idempotent-stop tests |
| `internal/database/driver.go` | Modify (`:25-43`) | nil-guard `DBConnection.Conn` in `Close` |
| `internal/database/driver_test.go` | Create | `Close` nil-guard test |
| `internal/database/failure.go` | Create | `FailureKind` and its constants — untagged, one definition |
| `internal/database/interbase_failure_native.go` | Create (tagged) | `ClassifyFailure` via `errors.As` on driver error types |
| `internal/database/interbase_failure_stub.go` | Create (inverse tag) | `ClassifyFailure` returning `(FailureNone, "")` |
| `internal/database/interbase_failure_native_test.go` | Create (tagged) | classification of real driver errors |
| `internal/database/interbase_failure_stub_test.go` | Create (inverse tag) | stub returns `FailureNone` |
| `internal/database/interbase_failure_live_test.go` | Create (tagged) | gated live cancellation test |
| `internal/handler/dispatch.go` | Create | `NewDispatcher`, `cancelRegistry`, `cancelParams` |
| `internal/handler/dispatch_test.go` | Create | async dispatch, cancel registry, late-cancellation note |
| `internal/handler/concurrency_test.go` | Create | test fixture: stub `database/sql` driver, gated stub repository, install helper |
| `internal/handler/concurrency_race_test.go` | Create | the `-race`-only regression tests for `files`, `WSCfg` and `connMu` |
| `internal/handler/failure.go` | Create | `cancellationNotice` and the results-pane message constants |
| `internal/handler/failure_test.go` | Create | message rendering tests |
| `internal/handler/handler.go` | Modify | `stateMu`, `connMu`, `fileText`, `$/cancelRequest` case, `Stop` restructure |
| `internal/handler/execute_command.go` | Modify | `connMu` per command, copy rule, cancellation rendering |
| `internal/handler/completion.go`, `hover.go`, `definition.go`, `rename.go`, `signature_help.go`, `format.go` | Modify | read document text through `fileText` |
| `internal/handler/handler_test.go` | Modify | wrap the handler with the dispatcher; read files through `fileText` |
| `main.go` | Modify (`:125`) | wrap the handler with the dispatcher |
| `doc/develop.md` | Modify | concurrency invariants section |
| `README.md` | Modify | cancellation is best effort; the uncertain-outcome warning |

---

### Task 1: Race detection in CI and in the Makefile

Spec §6.1e. This lands first so every later task has one command that proves it did not introduce a race.

**Files:**
- Modify: `Makefile:35-37`
- Modify: `.github/workflows/test.yaml:20-21`

**Interfaces:**
- Consumes: nothing.
- Produces: `make test-race`, a shell target running `go test -race ./...`. Every later task's verification step uses it.

- [ ] **Step 1: Confirm the baseline is green before changing anything**

Run: `go test -race ./...`
Expected: every package `ok`, exit status 0. If a race is reported, stop and report it before continuing — this plan assumes a green baseline.

- [ ] **Step 2: Add the `test-race` target**

In `Makefile`, immediately after the existing `test` target (which ends at line 37), add:

```make
.PHONY: test-race
test-race:
	go test -race ./...
```

Do not add a `build` prerequisite: the race run does not need the binary, and CI would otherwise build twice.

- [ ] **Step 3: Run the new target**

Run: `make test-race`
Expected: every package `ok`, exit status 0.

- [ ] **Step 4: Add the CI step**

In `.github/workflows/test.yaml`, leave the existing coverage step untouched and append a second step after it, so the file ends:

```yaml
    - name: Test
      run: go test -coverprofile coverage.out -covermode atomic ./...
    - name: Test with race detector
      run: make test-race
```

- [ ] **Step 5: Verify the workflow file parses as YAML**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/test.yaml'))" && echo ok`
Expected: `ok`

- [ ] **Step 6: Commit**

```bash
git add Makefile .github/workflows/test.yaml
git commit -m "ci: run the race detector in CI and via make test-race"
```

---

### Task 2: Guard `Worker.dbRepo` and make `Worker.Stop` idempotent

Spec §6.1d. `Worker.ReCache` writes `w.dbRepo` with no lock at `internal/database/worker.go:76`, and the worker goroutine reads it at `:58` and `:85`. This is pre-existing: `ReCache` already runs on the handler goroutine while the worker goroutine may still be servicing the previous `updateAdditionalCache` send. No `Server` mutex can fix it, because the reader is a goroutine inside `Worker`.

`Worker.Stop` is made idempotent in the same task because `handleExit` (`handler.go:203`) calls `Server.Stop`, and `serve`'s deferred `Stop` (`main.go:121`) calls it again, so `close(w.done)` runs twice on the exit path. Task 3 relies on `Stop` being safe to call twice.

**Files:**
- Modify: `internal/database/worker.go`
- Test: `internal/database/worker_test.go` (create)

**Interfaces:**
- Consumes: `make test-race` (Task 1).
- Produces:
  - `func (w *Worker) repo() DBRepository` — unexported, returns the current repository under `w.lock`.
  - `func (w *Worker) setRepo(repo DBRepository)` — unexported, sets it under `w.lock`.
  - `func (w *Worker) Stop()` — unchanged signature, now safe to call any number of times.

- [ ] **Step 1: Write the failing tests**

Create `internal/database/worker_test.go`:

```go
package database

import (
	"context"
	"testing"
)

// newWorkerTestRepo builds a repository whose secondary cache pass (which the
// worker goroutine runs) can be parked, so the test can write w.dbRepo from the
// caller goroutine while the worker goroutine is reading it.
func newWorkerTestRepo(describeAll func(context.Context) ([]*ColumnDesc, error)) *MockDBRepository {
	return &MockDBRepository{
		MockDatabase:  func(context.Context) (string, error) { return "", nil },
		MockDatabases: func(context.Context) ([]string, error) { return []string{""}, nil },
		MockDatabaseTables: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"": {"t"}}, nil
		},
		MockDescribeDatabaseTableBySchema: func(context.Context, string) ([]*ColumnDesc, error) {
			return nil, nil
		},
		MockDescribeForeignKeysBySchema: func(context.Context, string) ([]*ForeignKey, error) {
			return nil, nil
		},
		MockDescribeDatabaseTable: describeAll,
	}
}

func TestWorkerReCacheIsRaceFreeUnderConcurrentUpdates(t *testing.T) {
	ctx := context.Background()

	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	parked := newWorkerTestRepo(func(context.Context) ([]*ColumnDesc, error) {
		entered <- struct{}{}
		<-release
		return nil, nil
	})
	fast := newWorkerTestRepo(func(context.Context) ([]*ColumnDesc, error) {
		return nil, nil
	})

	w := NewWorker()
	w.Start()
	t.Cleanup(w.Stop)
	t.Cleanup(func() { close(release) })

	if err := w.ReCache(ctx, parked); err != nil {
		t.Fatal("first ReCache:", err)
	}

	// The worker goroutine has now read w.dbRepo and is inside the secondary
	// pass. The write below is concurrent with that read and has no
	// happens-before edge to it, which is exactly the reported race.
	<-entered

	if err := w.ReCache(ctx, fast); err != nil {
		t.Fatal("second ReCache:", err)
	}

	if w.Cache() == nil {
		t.Fatal("worker cache is nil after ReCache")
	}
}

func TestWorkerStopIsIdempotent(t *testing.T) {
	w := NewWorker()
	w.Start()
	w.Stop()
	w.Stop()
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race -run 'TestWorker' ./internal/database/ -v`
Expected: `TestWorkerReCacheIsRaceFreeUnderConcurrentUpdates` FAILS with `WARNING: DATA RACE`, naming a write at `worker.go:76` and a read at `worker.go:58`. `TestWorkerStopIsIdempotent` FAILS with `panic: close of closed channel`.

- [ ] **Step 3: Write the minimal implementation**

In `internal/database/worker.go`, add `stopOnce` to the struct:

```go
type Worker struct {
	dbRepo  DBRepository
	dbCache *DBCache

	done     chan struct{}
	update   chan struct{}
	lock     sync.Mutex
	stopOnce sync.Once
}
```

Add the accessors next to `setCache`:

```go
// repo returns the repository the worker goroutine should use. ReCache runs on
// the handler goroutine and may replace it while the worker goroutine is
// servicing an update, so both sides go through w.lock.
func (w *Worker) repo() DBRepository {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.dbRepo
}

func (w *Worker) setRepo(repo DBRepository) {
	w.lock.Lock()
	defer w.lock.Unlock()
	w.dbRepo = repo
}
```

Replace the three raw accesses:

- `worker.go:58`: `generator := NewDBCacheUpdater(w.dbRepo)` → `generator := NewDBCacheUpdater(w.repo())`
- `worker.go:76` in `ReCache`: `w.dbRepo = repo` → `w.setRepo(repo)`
- `worker.go:85` in `updateAllCache`: `generator := NewDBCacheUpdater(w.dbRepo)` → `generator := NewDBCacheUpdater(w.repo())`

Replace `Stop`:

```go
// Stop is safe to call more than once: handleExit and the deferred Stop in
// main both reach it on an ordinary shutdown.
func (w *Worker) Stop() {
	w.stopOnce.Do(func() { close(w.done) })
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race -run 'TestWorker' ./internal/database/ -v`
Expected: both PASS, no race warnings.

- [ ] **Step 5: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/database/worker.go internal/database/worker_test.go
git commit -m "fix: guard Worker.dbRepo with the worker lock and make Stop idempotent"
```

---

### Task 3: Restructure `Server.Stop` and nil-guard `DBConnection.Close`

Spec §5's "Shutdown cleanup must not depend on `Server.Stop` reaching its end". `Stop` (`handler.go:72-78`) returns early when `dbConn.Close()` fails, so `worker.Stop()` is skipped — a goroutine leak on exactly the path a half-dead InterBase attachment takes. Plan 4 appends snapshot removal here and would inherit the same bug.

`DBConnection.Close` nil-guards its receiver but not `db.Conn` (`driver.go:29`), so `(&DBConnection{Driver: "x"}).Close()` panics. `configureInterBaseTestServer` (`internal/handler/interbase_test.go:171`) builds exactly that value, so the restructured `Stop` needs the guard to be callable from tests.

**Files:**
- Modify: `internal/database/driver.go:25-43`
- Modify: `internal/handler/handler.go:72-78`
- Test: `internal/database/driver_test.go` (create)
- Test: `internal/handler/dispatch_test.go` (create — first test lands here)

**Interfaces:**
- Consumes: `Worker.Stop` being idempotent (Task 2).
- Produces:
  - `func (db *DBConnection) Close() error` — unchanged signature; now returns `nil` for a nil `Conn` and still closes `SSHConn`/`Tunnel`.
  - `func (s *Server) Stop() error` — unchanged signature; `s.worker.Stop()` now always runs.

- [ ] **Step 1: Write the failing tests**

Create `internal/database/driver_test.go`:

```go
package database

import (
	"errors"
	"testing"
)

type errCloser struct{ err error }

func (c errCloser) Close() error { return c.err }

func TestDBConnectionCloseWithoutSQLConn(t *testing.T) {
	conn := &DBConnection{Driver: "stub"}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
}

func TestDBConnectionCloseReportsTunnelFailure(t *testing.T) {
	want := errors.New("tunnel is wedged")
	conn := &DBConnection{Driver: "stub", Tunnel: errCloser{err: want}}
	if err := conn.Close(); !errors.Is(err, want) {
		t.Fatalf("Close() = %v, want %v", err, want)
	}
}
```

Create `internal/handler/dispatch_test.go`:

```go
package handler

import (
	"errors"
	"testing"

	"github.com/sqls-server/sqls/internal/database"
)

type failingCloser struct{ err error }

func (c failingCloser) Close() error { return c.err }

func TestStopStopsWorkerEvenWhenConnectionCloseFails(t *testing.T) {
	closeErr := errors.New("attachment is half dead")
	server := NewServer()
	server.dbConn = &database.DBConnection{
		Driver: "stub",
		Tunnel: failingCloser{err: closeErr},
	}

	if err := server.Stop(); !errors.Is(err, closeErr) {
		t.Fatalf("Stop() = %v, want %v", err, closeErr)
	}

	// A stopped worker must not accept another update. Stop is idempotent, so
	// calling it again is the cheap way to assert it already ran: a worker that
	// was never stopped would still be running here and the select below would
	// not see a closed channel.
	select {
	case <-server.worker.Done():
	default:
		t.Fatal("Stop returned without stopping the worker")
	}
}
```

This needs one new accessor on `Worker`. Add it in the same task (Step 3).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestDBConnectionClose|TestStopStopsWorker' ./internal/database/ ./internal/handler/ -v`
Expected: `TestDBConnectionCloseWithoutSQLConn` and `TestDBConnectionCloseReportsTunnelFailure` FAIL with a nil-pointer panic in `(*sql.DB).Close`. `TestStopStopsWorkerEvenWhenConnectionCloseFails` FAILS to compile with `server.worker.Done undefined`.

- [ ] **Step 3: Write the minimal implementation**

In `internal/database/driver.go`, replace the body of `Close`:

```go
func (db *DBConnection) Close() error {
	if db == nil {
		return nil
	}
	if db.Conn != nil {
		if err := db.Conn.Close(); err != nil {
			return err
		}
	}
	if db.SSHConn != nil {
		if err := db.SSHConn.Close(); err != nil {
			return err
		}
	}
	if db.Tunnel != nil {
		if err := db.Tunnel.Close(); err != nil {
			return err
		}
	}
	return nil
}
```

In `internal/database/worker.go`, add next to `Stop`:

```go
// Done reports the worker's shutdown channel. It exists so callers can assert
// that Stop has run.
func (w *Worker) Done() <-chan struct{} {
	return w.done
}
```

In `internal/handler/handler.go`, replace `Stop` (`:72-78`):

```go
// Stop closes the database connection and always stops the worker, including
// when closing the connection fails. Shutdown deliberately does not take
// connMu: a runaway query must not be able to hold the process open.
func (s *Server) Stop() error {
	defer s.worker.Stop()
	return s.dbConn.Close()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestDBConnectionClose|TestStopStopsWorker' ./internal/database/ ./internal/handler/ -v`
Expected: all three PASS.

- [ ] **Step 5: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/database/driver.go internal/database/driver_test.go internal/database/worker.go internal/handler/handler.go internal/handler/dispatch_test.go
git commit -m "fix: always stop the worker on shutdown and guard a nil sql.DB in Close"
```

---

### Task 4: A controllable repository fixture for command tests

`Test_executeQuery` (`internal/handler/execute_command_test.go:53-59`) has its assertions commented out because the registered `"mock"` driver returns `&sql.Rows{}`, a zero value that `database.Columns` cannot read. Every remaining task in this plan needs a command that can be parked mid-flight and then produce a real result, so the fixture is built once here and its correctness is proved by finishing the abandoned test.

**Files:**
- Test: `internal/handler/concurrency_test.go` (create)
- Modify: `internal/handler/execute_command_test.go:15-60`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces (all test-only, package `handler`):
  - `func installStubBackend(t *testing.T) *stubBackend` — registers the backend for the `"stub"` sqls driver and clears it on cleanup.
  - `func stubConnections(aliases ...string) *config.Config` — a `*config.Config` with one `"stub"` connection per alias.
  - `func (b *stubBackend) gate(method string) *stubGate` — arms a one-shot gate on a repository method.
  - `func (b *stubBackend) gateIgnoringCancel(method string) *stubGate` — a gate that also releases when the context is cancelled and then lets the method succeed anyway.
  - `func (g *stubGate) waitEntered(t *testing.T)` — blocks until the gated method is entered.
  - `func (g *stubGate) release()` — lets the gated method return.
  - `func (g *stubGate) contextWasCancelled() bool` — whether the gated call observed a cancelled context.
  - `func (b *stubBackend) queries() []stubQuery` — a snapshot of `{db *sql.DB, text string}` for every `Query` the backend served.
  - `func (b *stubBackend) opened() []*sql.DB` — every `*sql.DB` handed out by the opener, in order.

- [ ] **Step 1: Write the fixture and the failing test**

Create `internal/handler/concurrency_test.go`:

```go
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
	entered       chan struct{}
	released      chan struct{}
	ignoreCancel  bool
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
	select {
	case <-g.released:
	default:
		close(g.released)
	}
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
		for _, g := range b.gates {
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
```

Now replace the commented-out block in `internal/handler/execute_command_test.go`. Replace the whole of `Test_executeQuery` (lines 15-60) with:

```go
func Test_executeQuery(t *testing.T) {
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

	if !strings.Contains(got, "42") {
		t.Errorf("query result = %q, want it to contain the row value 42", got)
	}
	if !strings.Contains(got, "1 rows in set") {
		t.Errorf("query result = %q, want the row-count footer", got)
	}
	if queries := backend.queries(); len(queries) != 1 {
		t.Fatalf("repository served %d queries, want 1", len(queries))
	}
}
```

Note: `tx.textDocumentDidOpen` always opens `testFileURI` regardless of the `uri` argument it is passed (`handler_test.go:92`), which is why both the open and the command argument use `testFileURI` directly. `strings` and `lsp` are already imported by this file; `config` and `database` become unused once the old inline `DidChangeConfigurationParams` literal is gone, so remove those two imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run Test_executeQuery ./internal/handler/ -v`
Expected: FAIL — before the fixture exists the file does not compile; once `concurrency_test.go` is in place the test must PASS. If it fails with `cannot get query columns`, the stub driver is not wired into `sql.Open`; fix that before moving on.

- [ ] **Step 3: Run it again and read the rendered table**

Run: `go test -run Test_executeQuery ./internal/handler/ -v`
Expected: PASS, and the verbose log shows a table containing `42`.

- [ ] **Step 4: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/handler/concurrency_test.go internal/handler/execute_command_test.go
git commit -m "test: add a controllable repository fixture and finish Test_executeQuery"
```

---

### Task 5: Selective async dispatch for `workspace/executeCommand`

Spec §6.1a. `main.go:125` wraps the handler with `jsonrpc2.HandlerWithError` and no `AsyncHandler`, and `Conn.readMessages` calls `Handle` inline, so a running query blocks the server from reading the next message. Dispatch is made selective rather than using `jsonrpc2.AsyncHandler`, because making every request concurrent turns every unsynchronised `Server` field into a race on day one.

The test in this task deliberately uses `textDocument/formatting` as the concurrent second request. Formatting only *reads* `Server.files` and `Server.dbConn`, so it introduces no data race at this commit, where the locks do not exist yet. Do not substitute `didChange` or `didOpen` here — those write, and this commit would go red under `-race`. Task 6 adds exactly that test, after the lock exists.

**Files:**
- Create: `internal/handler/dispatch.go`
- Modify: `main.go:125`
- Modify: `internal/handler/handler_test.go:29`
- Test: `internal/handler/dispatch_test.go`

**Interfaces:**
- Consumes: `installStubBackend`, `stubConnections`, `(*stubBackend).gate`, `(*stubGate).waitEntered`, `(*stubGate).release` (Task 4).
- Produces: `func NewDispatcher(inner jsonrpc2.Handler) jsonrpc2.Handler` — runs `workspace/executeCommand` in its own goroutine, everything else inline.

- [ ] **Step 1: Write the failing test**

Append to `internal/handler/dispatch_test.go`:

```go
func TestExecuteCommandDispatchesAsynchronously(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	gate := backend.gate("Query")
	commandDone := make(chan error, 1)
	go func() {
		var got string
		commandDone <- tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
			Command:   CommandExecuteQuery,
			Arguments: []interface{}{testFileURI},
		}, &got)
	}()
	gate.waitEntered(t)

	// The command is parked inside the repository. A read-only request must
	// still be served; with inline dispatch this call times out.
	formatCtx, cancel := context.WithTimeout(tx.ctx, 5*time.Second)
	defer cancel()
	var edits []lsp.TextEdit
	if err := tx.conn.Call(formatCtx, "textDocument/formatting", lsp.DocumentFormattingParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
	}, &edits); err != nil {
		t.Fatal("second request was not served while a command was in flight:", err)
	}

	gate.release()
	select {
	case err := <-commandDone:
		if err != nil {
			t.Fatal("conn.Call workspace/executeCommand:", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight command never completed")
	}
}
```

Add `"context"`, `"time"` and `"github.com/sqls-server/sqls/internal/lsp"` to the file's imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestExecuteCommandDispatchesAsynchronously ./internal/handler/ -v`
Expected: FAIL with `second request was not served while a command was in flight: context deadline exceeded`.

- [ ] **Step 3: Write the minimal implementation**

Create `internal/handler/dispatch.go`:

```go
package handler

import (
	"context"

	"github.com/sourcegraph/jsonrpc2"
)

// NewDispatcher wraps a handler so that workspace/executeCommand runs in its
// own goroutine and every other request is handled inline on the connection's
// read loop, exactly as before. Dispatch is selective rather than
// jsonrpc2.AsyncHandler so that exactly one method is concurrent and exactly
// the state it touches needs guarding.
func NewDispatcher(inner jsonrpc2.Handler) jsonrpc2.Handler {
	return &dispatcher{inner: inner}
}

type dispatcher struct {
	inner jsonrpc2.Handler
}

func (d *dispatcher) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	if req.Method == "workspace/executeCommand" {
		go d.inner.Handle(ctx, conn, req)
		return
	}
	d.inner.Handle(ctx, conn, req)
}
```

In `main.go`, replace line 125:

```go
	h := handler.NewDispatcher(jsonrpc2.HandlerWithError(server.Handle))
```

In `internal/handler/handler_test.go`, replace line 29:

```go
	handler := NewDispatcher(jsonrpc2.HandlerWithError(server.Handle))
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -run TestExecuteCommandDispatchesAsynchronously ./internal/handler/ -v`
Expected: PASS.

- [ ] **Step 5: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`, no race warnings. If a race is reported here, a test other than this one is exercising a writer concurrently — find it and move that assertion into Task 6, do not add a lock early.

- [ ] **Step 6: Commit**

```bash
git add internal/handler/dispatch.go internal/handler/dispatch_test.go internal/handler/handler_test.go main.go
git commit -m "feat: dispatch workspace/executeCommand on its own goroutine"
```

---

### Task 6: `stateMu` over `Server.files` and the copy rule

Spec §6.1c. `updateFile` (`handler.go:296-303`) mutates `f.Text` through the stored `*File` pointer, so holding a lock only while looking the pointer up is not enough. `executeQuery` (`execute_command.go:125-141`) is today's concrete offender: it retains `f` at line 125 and reads `f.Text` sixteen lines later at line 141.

**Files:**
- Modify: `internal/handler/handler.go` (struct at `:23-42`, `openFile:282`, `closeFile:291`, `updateFile:296`)
- Modify: `internal/handler/completion.go:23`, `hover.go:32`, `definition.go:29`, `rename.go:28`, `signature_help.go:27`, `format.go:23`, `format.go:48`
- Modify: `internal/handler/execute_command.go:125-141`
- Modify: `internal/handler/handler_test.go:206`, `:213`
- Test: `internal/handler/concurrency_race_test.go` (create)

**Interfaces:**
- Consumes: `NewDispatcher` (Task 5), the Task 4 fixture.
- Produces:
  - `Server.stateMu sync.RWMutex` — guards mutable `Server` fields.
  - `func (s *Server) fileText(uri string) (string, bool)` — returns a copy of the document text; the only supported way to read `Server.files`.

- [ ] **Step 1: Write the failing test**

Create `internal/handler/concurrency_race_test.go`:

```go
package handler

import (
	"fmt"
	"testing"
	"time"

	"github.com/sqls-server/sqls/internal/lsp"
)

// This test is meaningful under -race. It parks executeQuery after it has read
// the document text and then rewrites that text from the inline read loop,
// which is the unsynchronised write/read pair the copy rule closes.
func TestDidChangeDuringAsyncQueryDoesNotRaceOnFileText(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	gate := backend.gate("Query")
	commandDone := make(chan error, 1)
	go func() {
		var got string
		commandDone <- tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
			Command:   CommandExecuteQuery,
			Arguments: []interface{}{testFileURI},
		}, &got)
	}()
	gate.waitEntered(t)

	for i := 0; i < 50; i++ {
		params := lsp.DidChangeTextDocumentParams{
			TextDocument: lsp.VersionedTextDocumentIdentifier{URI: testFileURI, Version: i + 1},
			ContentChanges: []lsp.TextDocumentContentChangeEvent{
				{Text: fmt.Sprintf("SELECT %d;", i)},
			},
		}
		if err := tx.conn.Call(tx.ctx, "textDocument/didChange", params, nil); err != nil {
			t.Fatal("conn.Call textDocument/didChange:", err)
		}
	}

	gate.release()
	select {
	case err := <-commandDone:
		if err != nil {
			t.Fatal("conn.Call workspace/executeCommand:", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight command never completed")
	}

	if got, ok := tx.server.fileText(testFileURI); !ok || got != "SELECT 49;" {
		t.Fatalf("fileText = (%q, %v), want (\"SELECT 49;\", true)", got, ok)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -race -run TestDidChangeDuringAsyncQueryDoesNotRaceOnFileText ./internal/handler/ -v`
Expected: FAIL. It will not compile first (`tx.server.fileText undefined`); add only the accessor's signature if you want to see the race itself, otherwise the compile failure is the failing state. After the accessor exists but before the locks are added, the run reports `WARNING: DATA RACE` naming a write in `updateFile` at `handler.go:301` and a read in `executeQuery`.

- [ ] **Step 3: Write the minimal implementation**

In `internal/handler/handler.go`, add the mutex to the struct (`:23-42`), directly above `dbConn`:

```go
type Server struct {
	SpecificFileCfg *config.Config
	DefaultFileCfg  *config.Config
	WSCfg           *config.Config

	// stateMu guards every mutable field below. It is taken for short,
	// non-blocking accesses only: connMu, not stateMu, is what a command holds
	// across database I/O.
	stateMu sync.RWMutex

	dbConn *database.DBConnection
	// ... unchanged fields ...
}
```

Add `"sync"` to the imports.

Replace the three file mutators (`:282-303`):

```go
func (s *Server) openFile(uri string, languageID string) error {
	f := &File{
		Text:       "",
		LanguageID: languageID,
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.files[uri] = f
	return nil
}

func (s *Server) closeFile(uri string) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	delete(s.files, uri)
	return nil
}

func (s *Server) updateFile(uri string, text string) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	f, ok := s.files[uri]
	if !ok {
		return fmt.Errorf("document not found: %v", uri)
	}
	f.Text = text
	return nil
}

// fileText returns a copy of the document text for uri. Callers must never
// retain the *File: updateFile mutates Text through the stored pointer, so a
// reader that keeps the pointer races with a concurrent didChange.
func (s *Server) fileText(uri string) (string, bool) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	f, ok := s.files[uri]
	if !ok {
		return "", false
	}
	return f.Text, true
}
```

Convert every reader. In `internal/handler/completion.go:23`:

```go
	text, ok := s.fileText(params.TextDocument.URI)
	if !ok {
		return nil, fmt.Errorf("document not found: %s", params.TextDocument.URI)
	}
```

and use `text` where `f.Text` was used. Apply the identical shape — preserving each site's existing error message and wording — at:

- `internal/handler/hover.go:32` (then `hoverWithDriver(text, params, s.worker.Cache(), s.parserDriver())`)
- `internal/handler/definition.go:29` (then `definitionWithDriver(params.TextDocument.URI, text, params, s.worker.Cache(), s.parserDriver())`)
- `internal/handler/rename.go:28`
- `internal/handler/signature_help.go:27` (then `SignatureHelpWithDriver(text, params, s.worker.Cache(), s.parserDriver())`)
- `internal/handler/format.go:23` (then `formatter.FormatWithDriver(text, params, s.getConfig(), s.parserDriver())`)
- `internal/handler/format.go:48`, which discards the value: `if _, ok := s.fileText(params.TextDocument.URI); !ok {`

In `internal/handler/execute_command.go`, replace lines 125-128 and the read at line 141:

```go
	text, ok := s.fileText(uri)
	if !ok {
		return nil, fmt.Errorf("document not found, %q", uri)
	}
```

and delete `text := f.Text` at line 141, leaving `if params.Range != nil {` to operate on the `text` local declared above.

In `internal/handler/handler_test.go`, replace the two direct map accesses:

```go
	// line 206
	_, ok := tx.server.fileText(didCloseParams.TextDocument.URI)
	if ok {
		t.Errorf("found opened file. URI:%s", didCloseParams.TextDocument.URI)
	}
```

```go
// line 212
func (tx *TestContext) testFile(t *testing.T, uri, text string) {
	got, ok := tx.server.fileText(uri)
	if !ok {
		t.Errorf("not found opened file. URI:%s", uri)
	}
	if got != text {
		t.Errorf("not match %s. got: %s", text, got)
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -race -run TestDidChangeDuringAsyncQueryDoesNotRaceOnFileText ./internal/handler/ -v`
Expected: PASS, no race warnings.

- [ ] **Step 5: Verify no direct `s.files` access remains outside the accessors**

Run: `grep -rn 's\.files\|server\.files\|\.Text' internal/handler --include='*.go' | grep -v '_test.go' | grep -v 'handler.go:'`
Expected: no lines referencing `s.files`; remaining `.Text` hits are `File.Text` in `handler.go` and unrelated LSP params.

- [ ] **Step 6: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/handler/handler.go internal/handler/completion.go internal/handler/hover.go internal/handler/definition.go internal/handler/rename.go internal/handler/signature_help.go internal/handler/format.go internal/handler/execute_command.go internal/handler/handler_test.go internal/handler/concurrency_race_test.go
git commit -m "fix: guard Server.files with stateMu and copy document text under the lock"
```

---

### Task 7: `stateMu` over the connection and configuration fields

Spec §6.1c, the rest of the audit table. `WSCfg` is the genuine inline-writer/async-reader race: `handleWorkspaceDidChangeConfiguration:315` writes it on the read loop while an async command reads it through `getConfig:423` ← `topConnection`. `dbConn`, `curDBCfg`, `curDBName`, `curConnectionIndex` and `initOptionDBConfig` are all reachable from the async path.

The restructuring below exists to honour the invariant that `stateMu` is never held across I/O: `reconnectionDB` and `newDBConnection` read fields into locals, do their I/O unlocked, and take the write lock only for the assignments.

**Files:**
- Modify: `internal/handler/handler.go:171`, `:192-205`, `:309-359`, `:361-438`
- Modify: `internal/handler/execute_command.go:115`, `:391`, `:459`
- Test: `internal/handler/concurrency_race_test.go`

**Interfaces:**
- Consumes: `Server.stateMu` (Task 6), the Task 4 fixture.
- Produces: no new exported names. `getConfig`, `topConnection`, `getConnection`, `parserDriver` and `newDBRepository` keep their current signatures and now take `stateMu` internally.

- [ ] **Step 1: Write the failing test**

Append to `internal/handler/concurrency_race_test.go`:

```go
// switchDatabase reads WSCfg through getConfig and then parks inside the cache
// rebuild, so an inline didChangeConfiguration writes WSCfg while the async
// command's read is still unordered against it. Meaningful under -race.
func TestWorkspaceConfigurationChangeDuringAsyncCommandDoesNotRace(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))

	gate := backend.gate("CurrentSchema")
	commandDone := make(chan error, 1)
	go func() {
		commandDone <- tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
			Command:   CommandSwitchDatabase,
			Arguments: []interface{}{"other"},
		}, nil)
	}()
	gate.waitEntered(t)

	tx.addWorkspaceConfig(t, stubConnections("primary", "secondary"))

	gate.release()
	select {
	case <-commandDone:
	case <-time.After(10 * time.Second):
		t.Fatal("switchDatabase never completed")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -race -run TestWorkspaceConfigurationChangeDuringAsyncCommandDoesNotRace ./internal/handler/ -v`
Expected: FAIL with `WARNING: DATA RACE`, naming a write at `handler.go:315` (`s.WSCfg = ...`) and a read at `handler.go:423` (`validConfig(s.WSCfg)`).

- [ ] **Step 3: Write the minimal implementation**

In `internal/handler/handler.go`:

`handleInitialize:171`:

```go
	s.stateMu.Lock()
	s.initOptionDBConfig = params.InitializationOptions.ConnectionConfig
	s.stateMu.Unlock()
```

`handleShutdown` and `handleExit` (`:192-205`):

```go
func (s *Server) handleShutdown(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	s.stateMu.RLock()
	dbConn := s.dbConn
	s.stateMu.RUnlock()
	if dbConn != nil {
		dbConn.Close()
	}
	return nil, nil
}

func (s *Server) handleExit(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	s.stateMu.RLock()
	dbConn := s.dbConn
	s.stateMu.RUnlock()
	if dbConn != nil {
		dbConn.Close()
	}
	err = s.Stop()
	return nil, err
}
```

`handleWorkspaceDidChangeConfiguration` (`:309-339`), replacing lines 315-320:

```go
	s.stateMu.Lock()
	s.WSCfg = params.Settings.SQLS
	s.stateMu.Unlock()

	// Skip database connection
	s.stateMu.RLock()
	connected := s.dbConn != nil
	s.stateMu.RUnlock()
	if connected {
		return nil, nil
	}
```

`reconnectionDB` (`:341-359`):

```go
func (s *Server) reconnectionDB(ctx context.Context) error {
	s.stateMu.RLock()
	oldConn := s.dbConn
	s.stateMu.RUnlock()
	if err := oldConn.Close(); err != nil {
		return err
	}

	dbConn, err := s.newDBConnection(ctx)
	if err != nil {
		return err
	}
	s.stateMu.Lock()
	s.dbConn = dbConn
	s.stateMu.Unlock()

	dbRepo, err := s.newDBRepository(ctx)
	if err != nil {
		return err
	}
	if err := s.worker.ReCache(ctx, dbRepo); err != nil {
		return err
	}
	return nil
}
```

`newDBConnection` (`:361-384`):

```go
func (s *Server) newDBConnection(ctx context.Context) (*database.DBConnection, error) {
	// Get the most preferred DB connection settings
	connCfg := s.topConnection()
	if connCfg == nil {
		return nil, ErrNoConnection
	}
	s.stateMu.RLock()
	index := s.curConnectionIndex
	dbName := s.curDBName
	s.stateMu.RUnlock()

	if index != 0 {
		connCfg = s.getConnection(index)
	}
	if connCfg == nil {
		return nil, fmt.Errorf("not found database connection config, index %d", index+1)
	}
	if dbName != "" {
		connCfg.DBName = dbName
	}
	s.stateMu.Lock()
	s.curDBCfg = connCfg
	s.stateMu.Unlock()

	// Connect database
	conn, err := database.Open(connCfg)
	if err != nil {
		return nil, err
	}
	return conn, nil
}
```

`newDBRepository` (`:386-395`):

```go
func (s *Server) newDBRepository(ctx context.Context) (database.DBRepository, error) {
	s.stateMu.RLock()
	curDBCfg := s.curDBCfg
	dbConn := s.dbConn
	s.stateMu.RUnlock()
	if curDBCfg == nil || dbConn == nil {
		return nil, ErrNoConnection
	}
	repo, err := database.CreateRepository(curDBCfg.Driver, dbConn.Conn)
	if err != nil {
		return nil, err
	}
	return repo, nil
}
```

`topConnection` (`:397-408`) — release `stateMu` before calling `getConfig`, which takes it again:

```go
func (s *Server) topConnection() *database.DBConfig {
	// if the init config is set, ignore all other connection configs
	s.stateMu.RLock()
	initCfg := s.initOptionDBConfig
	s.stateMu.RUnlock()
	if initCfg != nil {
		return initCfg
	}

	cfg := s.getConfig()
	if cfg == nil || len(cfg.Connections) == 0 {
		return nil
	}
	return cfg.Connections[0]
}
```

`getConfig` (`:418-431`):

```go
func (s *Server) getConfig() *config.Config {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	switch {
	case validConfig(s.SpecificFileCfg):
		return s.SpecificFileCfg
	case validConfig(s.WSCfg):
		return s.WSCfg
	case validConfig(s.DefaultFileCfg):
		return s.DefaultFileCfg
	default:
		return config.NewConfig()
	}
}
```

`parserDriver` (`:433-438`):

```go
func (s *Server) parserDriver() dialect.DatabaseDriver {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if s.dbConn == nil {
		return ""
	}
	return s.dbConn.Driver
}
```

In `internal/handler/execute_command.go`, replace line 115:

```go
	s.stateMu.RLock()
	connected := s.dbConn != nil
	s.stateMu.RUnlock()
	if !connected {
		return nil, errors.New("database connection is not open")
	}
```

replace line 391 in `switchDatabase`:

```go
	// Change current database
	s.stateMu.Lock()
	s.curDBName = dbName
	s.stateMu.Unlock()
```

and line 459 in `switchConnections`:

```go
	// Reconnect database
	s.stateMu.Lock()
	s.curConnectionIndex = index
	s.stateMu.Unlock()
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -race -run TestWorkspaceConfigurationChangeDuringAsyncCommandDoesNotRace ./internal/handler/ -v`
Expected: PASS, no race warnings.

- [ ] **Step 5: Verify no unguarded field access remains**

Run: `grep -rn 's\.dbConn\|s\.curDB\|s\.curConnectionIndex\|s\.WSCfg\|s\.initOptionDBConfig\|s\.SpecificFileCfg\|s\.DefaultFileCfg' internal/handler --include='*.go' | grep -v '_test.go'`
Expected: every hit is inside a `stateMu.RLock()`/`stateMu.Lock()` region in `handler.go`, and nothing in `execute_command.go` outside the three blocks above.

- [ ] **Step 6: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/handler/handler.go internal/handler/execute_command.go internal/handler/concurrency_race_test.go
git commit -m "fix: guard the connection and configuration fields with stateMu"
```

---

### Task 8: `connMu` and the command-versus-command policy

Spec §6.1bis. With all of `workspace/executeCommand` async, `executeQuery` and `switchConnections` can run at the same time, and `reconnectionDB` closes and reassigns `s.dbConn`. The hazard is **not** a use-after-close: `DB.Close` "waits for all queries that have started processing on the server to finish" (`$GOROOT/src/database/sql/sql.go:925-927`). The two real hazards are a query that snapshotted the repository but *starts* after `Close` completes and fails with `sql: database is closed`, and `reconnectionDB` blocking for the drain — which, held under `stateMu`, would freeze every inline request for up to the ~10 s an InterBase row-lock wait can take.

**Files:**
- Modify: `internal/handler/handler.go` (struct, `handleInitialize`, `handleWorkspaceDidChangeConfiguration`)
- Modify: `internal/handler/execute_command.go` (`executeQuery`, `showDatabases`, `showSchemas`, `showConnections`, `switchDatabase`, `switchConnections`, `showTables`)
- Test: `internal/handler/concurrency_race_test.go`

**Interfaces:**
- Consumes: `Server.stateMu` (Tasks 6–7), the Task 4 fixture including `(*stubBackend).opened` and `(*stubBackend).queries`.
- Produces: `Server.connMu sync.RWMutex` — guards connection lifetime. Later plans' database-touching commands must take `connMu.RLock()`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/handler/concurrency_race_test.go`:

```go
func TestSwitchConnectionWaitsForInFlightQuery(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary", "secondary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	gate := backend.gate("Query")

	queryDone := make(chan string, 1)
	queryErr := make(chan error, 1)
	go func() {
		var got string
		if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
			Command:   CommandExecuteQuery,
			Arguments: []interface{}{testFileURI},
		}, &got); err != nil {
			queryErr <- err
			return
		}
		queryDone <- got
	}()
	gate.waitEntered(t)

	switchDone := make(chan error, 1)
	go func() {
		switchDone <- tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
			Command:   CommandSwitchConnection,
			Arguments: []interface{}{"2"},
		}, nil)
	}()

	select {
	case err := <-switchDone:
		t.Fatalf("switchConnections completed while a query was in flight (err=%v)", err)
	case err := <-queryErr:
		t.Fatal("conn.Call workspace/executeCommand:", err)
	case <-time.After(500 * time.Millisecond):
		// Expected: the switch is blocked behind the in-flight query.
	}

	gate.release()

	select {
	case got := <-queryDone:
		if !strings.Contains(got, "42") {
			t.Errorf("query result = %q, want the intact row value 42", got)
		}
	case err := <-queryErr:
		t.Fatal("in-flight query failed:", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight query never completed")
	}

	select {
	case err := <-switchDone:
		if err != nil {
			t.Fatal("conn.Call switchConnections:", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("switchConnections never completed")
	}
}

func TestQueryAfterSwitchUsesNewConnection(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary", "secondary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandSwitchConnection,
		Arguments: []interface{}{"2"},
	}, nil); err != nil {
		t.Fatal("conn.Call switchConnections:", err)
	}

	var got string
	if err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
		Command:   CommandExecuteQuery,
		Arguments: []interface{}{testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call executeQuery:", err)
	}

	opened := backend.opened()
	if len(opened) < 2 {
		t.Fatalf("opened %d connections, want at least 2", len(opened))
	}
	newest := opened[len(opened)-1]

	queries := backend.queries()
	if len(queries) != 1 {
		t.Fatalf("repository served %d queries, want 1", len(queries))
	}
	if queries[0].db != newest {
		t.Error("the query ran against a stale connection, want the newest one")
	}
}
```

Add `"strings"` to the file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race -run 'TestSwitchConnectionWaitsForInFlightQuery|TestQueryAfterSwitchUsesNewConnection' ./internal/handler/ -v`
Expected: `TestSwitchConnectionWaitsForInFlightQuery` FAILS with `switchConnections completed while a query was in flight`. `TestQueryAfterSwitchUsesNewConnection` may already pass; keep it, it is the regression test that `connMu` does not leave the window open in the other direction.

- [ ] **Step 3: Write the minimal implementation**

In `internal/handler/handler.go`, add the second mutex above `stateMu` in the struct:

```go
	// connMu guards connection lifetime. Commands that touch the database take
	// it for reading; commands that replace the connection take it for writing.
	// Lock ordering: connMu before stateMu, never the reverse.
	connMu sync.RWMutex

	// stateMu guards every mutable field below. It is taken for short,
	// non-blocking accesses only.
	stateMu sync.RWMutex
```

In `handleInitialize`, replace the reconnect block at `:176-188` in full. Only the first line of the `if` changes: the reconnect moves out of the `if` initialiser so the lock can be released before the branch, and it assigns the function's named return `err` instead of shadowing it. The two inner `if err := messenger.Show*` blocks keep their own shadowed `err` exactly as today.

```go
	messenger := lsp.NewMessenger(conn)
	s.connMu.Lock()
	err = s.reconnectionDB(ctx)
	s.connMu.Unlock()
	if err != nil {
		if errors.Is(err, ErrNoConnection) {
			if err := messenger.ShowInfo(ctx, err.Error()); err != nil {
				log.Println("send info", err.Error())
				return nil, err
			}
		} else {
			log.Println("send err", err.Error())
			if err := messenger.ShowError(ctx, err.Error()); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
```

In `handleWorkspaceDidChangeConfiguration`, apply the identical transformation to its reconnect block at `:323-336`, which has the same shape and the same two inner blocks. It sits **after** the early return added in Task 7, so the `WSCfg` write itself never waits on a running query — only the reconnect does.

```go
	messenger := lsp.NewMessenger(conn)
	s.connMu.Lock()
	err = s.reconnectionDB(ctx)
	s.connMu.Unlock()
	if err != nil {
		if errors.Is(err, ErrNoConnection) {
			if err := messenger.ShowInfo(ctx, err.Error()); err != nil {
				log.Println("send info", err.Error())
				return nil, err
			}
		} else {
			log.Println("send err", err.Error())
			if err := messenger.ShowError(ctx, err.Error()); err != nil {
				return nil, err
			}
		}
	}

	return nil, nil
```

In `internal/handler/execute_command.go`, add the read lock as the first statement of `executeQuery`, `showDatabases`, `showSchemas`, `showConnections` and `showTables`:

```go
	s.connMu.RLock()
	defer s.connMu.RUnlock()
```

`showConnections` is in this list although the spec's §6.1bis prose omits it: `newDBConnection` writes `connCfg.DBName` in place at `handler.go:374` on a `*database.DBConfig` that `showConnections` reads field-by-field at `execute_command.go:406-418`. Without the read lock that in-place write is a data race on config *contents*, which no `Server`-field lock can cover.

Add the write lock as the first statement of `switchDatabase` and `switchConnections`:

```go
	s.connMu.Lock()
	defer s.connMu.Unlock()
```

In `switchConnections` this must come before the `s.getConfig()` call at line 437 so the alias lookup and the reconnect are one atomic step.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race -run 'TestSwitchConnectionWaitsForInFlightQuery|TestQueryAfterSwitchUsesNewConnection' ./internal/handler/ -v`
Expected: both PASS.

- [ ] **Step 5: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`. A hang here means a lock-order violation — check that nothing takes `connMu` while holding `stateMu`, and that `Stop`/`handleShutdown`/`handleExit` take no `connMu` at all.

- [ ] **Step 6: Commit**

```bash
git add internal/handler/handler.go internal/handler/execute_command.go internal/handler/concurrency_race_test.go
git commit -m "feat: serialise connection-mutating commands against in-flight queries with connMu"
```

---

### Task 9: `$/cancelRequest` and the cancel registry

Spec §6.1b. `sourcegraph/jsonrpc2@v0.2.1` has no `$/cancelRequest` support — zero occurrences in the module — so the registry lives in sqls. Now that dispatch is non-blocking for `workspace/executeCommand`, the cancel notification can actually be read while the query runs.

This task also implements the "cancellation that arrived too late" note from the spec's User-Visible Behavior section: when the statement completed anyway, the result is rendered normally with a note above it, because the driver README's "the executing native result remains authoritative" makes that the honest report.

**Files:**
- Modify: `internal/handler/dispatch.go`
- Modify: `internal/handler/handler.go` (`NewServer`, the `handle` switch)
- Modify: `internal/handler/execute_command.go:84-111`
- Test: `internal/handler/dispatch_test.go`

**Interfaces:**
- Consumes: `NewDispatcher` (Task 5), `(*stubBackend).gate`, `(*stubBackend).gateIgnoringCancel`, `(*stubGate).contextWasCancelled` (Task 4).
- Produces:
  - `type cancelParams struct { ID jsonrpc2.ID `json:"id"` }`
  - `func newCancelRegistry() *cancelRegistry`
  - `func (r *cancelRegistry) register(id jsonrpc2.ID, cancel context.CancelFunc) *cancelEntry`
  - `func (r *cancelRegistry) unregister(id jsonrpc2.ID)`
  - `func (r *cancelRegistry) cancel(id jsonrpc2.ID)`
  - `func (e *cancelEntry) cancelRequested() bool`
  - `Server.cancels *cancelRegistry`
  - `const lateCancellationNote string`

- [ ] **Step 1: Write the failing tests**

Append to `internal/handler/dispatch_test.go`:

```go
func TestExecuteQueryHonoursCancelRequest(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	gate := backend.gate("Query")
	requestID := jsonrpc2.ID{Str: "cancel-me", IsString: true}

	done := make(chan error, 1)
	go func() {
		var got string
		done <- tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
			Command:   CommandExecuteQuery,
			Arguments: []interface{}{testFileURI},
		}, &got, jsonrpc2.PickID(requestID))
	}()
	gate.waitEntered(t)

	if err := tx.conn.Notify(tx.ctx, "$/cancelRequest", cancelParams{ID: requestID}); err != nil {
		t.Fatal("conn.Notify $/cancelRequest:", err)
	}

	select {
	case <-done:
		// Any outcome is acceptable here; the assertion is that the call
		// returned promptly and the repository saw a cancelled context.
	case <-time.After(5 * time.Second):
		t.Fatal("$/cancelRequest did not unblock the in-flight query")
	}

	if !gate.contextWasCancelled() {
		t.Error("the repository never observed a cancelled context")
	}
}

func TestCancelRequestForUnknownIDIsIgnored(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	unknown := jsonrpc2.ID{Str: "no-such-request", IsString: true}
	if err := tx.conn.Notify(tx.ctx, "$/cancelRequest", cancelParams{ID: unknown}); err != nil {
		t.Fatal("conn.Notify $/cancelRequest:", err)
	}

	// The server must still be serving.
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")
}

func TestLateCancellationRendersTheRealResultWithANote(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	// This gate lets the statement succeed once the context is cancelled, which
	// is the driver's "the cancellation arrived too late" case.
	gate := backend.gateIgnoringCancel("Query")
	requestID := jsonrpc2.ID{Str: "late-cancel", IsString: true}

	type callResult struct {
		out string
		err error
	}
	done := make(chan callResult, 1)
	go func() {
		var got string
		err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
			Command:   CommandExecuteQuery,
			Arguments: []interface{}{testFileURI},
		}, &got, jsonrpc2.PickID(requestID))
		done <- callResult{out: got, err: err}
	}()
	gate.waitEntered(t)

	if err := tx.conn.Notify(tx.ctx, "$/cancelRequest", cancelParams{ID: requestID}); err != nil {
		t.Fatal("conn.Notify $/cancelRequest:", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatal("conn.Call workspace/executeCommand:", res.err)
		}
		if !strings.Contains(res.out, "42") {
			t.Errorf("result = %q, want the real row value 42", res.out)
		}
		if !strings.Contains(res.out, "the cancellation request arrived after the statement completed") {
			t.Errorf("result = %q, want the late-cancellation note", res.out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command never completed")
	}
}
```

Add `"strings"` and `"github.com/sourcegraph/jsonrpc2"` to the file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race -run 'CancelRequest|LateCancellation' ./internal/handler/ -v`
Expected: compile failure on `cancelParams`. After adding only the type, `TestExecuteQueryHonoursCancelRequest` FAILS with `$/cancelRequest did not unblock the in-flight query`, and `TestLateCancellationRendersTheRealResultWithANote` FAILS on the missing note.

- [ ] **Step 3: Write the minimal implementation**

Append to `internal/handler/dispatch.go`:

```go
import (
	"context"
	"sync"

	"github.com/sourcegraph/jsonrpc2"
)

// cancelParams is the payload of the LSP $/cancelRequest notification.
// jsonrpc2.ID unmarshals both the numeric and the string form.
type cancelParams struct {
	ID jsonrpc2.ID `json:"id"`
}

// cancelEntry is one in-flight cancellable request. requested stays true after
// a cancellation even when the statement completed anyway, so the caller can
// tell the user that the result it is about to read is the real result.
type cancelEntry struct {
	cancel context.CancelFunc

	mu        sync.Mutex
	requested bool
}

func (e *cancelEntry) cancelRequested() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.requested
}

func (e *cancelEntry) request() {
	e.mu.Lock()
	e.requested = true
	e.mu.Unlock()
	e.cancel()
}

type cancelRegistry struct {
	mu      sync.Mutex
	entries map[jsonrpc2.ID]*cancelEntry
}

func newCancelRegistry() *cancelRegistry {
	return &cancelRegistry{entries: map[jsonrpc2.ID]*cancelEntry{}}
}

func (r *cancelRegistry) register(id jsonrpc2.ID, cancel context.CancelFunc) *cancelEntry {
	entry := &cancelEntry{cancel: cancel}
	r.mu.Lock()
	r.entries[id] = entry
	r.mu.Unlock()
	return entry
}

func (r *cancelRegistry) unregister(id jsonrpc2.ID) {
	r.mu.Lock()
	delete(r.entries, id)
	r.mu.Unlock()
}

// cancel is a no-op for an id that is not in flight: a cancellation that races
// the response is normal and must not error.
func (r *cancelRegistry) cancel(id jsonrpc2.ID) {
	r.mu.Lock()
	entry := r.entries[id]
	r.mu.Unlock()
	if entry != nil {
		entry.request()
	}
}
```

In `internal/handler/handler.go`, add the field to `Server`:

```go
	worker  *database.Worker
	files   map[string]*File
	cancels *cancelRegistry
```

and initialise it in `NewServer`:

```go
	return &Server{
		files:   make(map[string]*File),
		worker:  worker,
		cancels: newCancelRegistry(),
	}
```

Add the case to the `handle` switch, next to `workspace/executeCommand`:

```go
	case "$/cancelRequest":
		return s.handleCancelRequest(ctx, conn, req)
```

and add the handler below `handleWorkspaceDidChangeConfiguration`:

```go
func (s *Server) handleCancelRequest(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	if req.Params == nil {
		return nil, nil
	}
	var params cancelParams
	if err := json.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}
	s.cancels.cancel(params.ID)
	return nil, nil
}
```

In `internal/handler/execute_command.go`, add the note constant near the command constants:

```go
const lateCancellationNote = "Note: the cancellation request arrived after the statement completed; the result\nbelow is the real result.\n\n"
```

and replace the body of `handleWorkspaceExecuteCommand` after the unmarshal (`:94-111`):

```go
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	entry := s.cancels.register(req.ID, cancel)
	defer s.cancels.unregister(req.ID)

	result, err = s.dispatchCommand(ctx, params)
	if err != nil || !entry.cancelRequested() {
		return result, err
	}
	// The statement finished before the native cancellation took effect. The
	// executing result stays authoritative, so it is rendered with a note.
	if text, ok := result.(string); ok {
		return lateCancellationNote + text, nil
	}
	return result, nil
}

func (s *Server) dispatchCommand(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	switch params.Command {
	case CommandExecuteQuery:
		return s.executeQuery(ctx, params)
	case CommandShowDatabases:
		return s.showDatabases(ctx, params)
	case CommandShowSchemas:
		return s.showSchemas(ctx, params)
	case CommandShowConnections:
		return s.showConnections(ctx, params)
	case CommandSwitchDatabase:
		return s.switchDatabase(ctx, params)
	case CommandSwitchConnection:
		return s.switchConnections(ctx, params)
	case CommandShowTables:
		return s.showTables(ctx, params)
	}
	return nil, fmt.Errorf("unsupported command: %v", params.Command)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race -run 'CancelRequest|LateCancellation' ./internal/handler/ -v`
Expected: all three PASS.

- [ ] **Step 5: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/handler/dispatch.go internal/handler/dispatch_test.go internal/handler/handler.go internal/handler/execute_command.go
git commit -m "feat: honour \$/cancelRequest for workspace/executeCommand"
```

---

### Task 10: `ClassifyFailure` and the results-pane cancellation messages

Spec §6.5 and the "Query cancelled" entries of the User-Visible Behavior section. `UncertainOutcomeError` is checked **before** `CancellationError` because `(*UncertainOutcomeError).Unwrap` returns its `Cause`, which is normally a `*CancellationError` (`../interbase-go/cancellation.go:507-512`), so `errors.As` for a cancellation also matches an uncertain outcome. No error string is ever matched.

`FailureKind` and its constants live in an untagged file so there is exactly one definition of the enum; only `ClassifyFailure` is in the tagged/untagged pair. This is a small deviation from the spec's §0 file table, taken so the two builds cannot drift apart.

The rendering also honours a bare `context.Canceled` so the cancelled message is reachable on an ordinary, untagged build — otherwise the whole path would be unreachable outside an InterBase build and untestable in CI. The classifier stays authoritative whenever it recognises the error.

**Files:**
- Create: `internal/database/failure.go`
- Create: `internal/database/interbase_failure_native.go`
- Create: `internal/database/interbase_failure_stub.go`
- Create: `internal/database/interbase_failure_native_test.go`
- Create: `internal/database/interbase_failure_stub_test.go`
- Create: `internal/database/interbase_failure_live_test.go`
- Create: `internal/handler/failure.go`
- Create: `internal/handler/failure_test.go`
- Modify: `internal/handler/execute_command.go:156-179`

**Interfaces:**
- Consumes: `Server.connMu` (Task 8), the cancel registry (Task 9).
- Produces:
  - `type FailureKind int` with `FailureNone`, `FailureCanceled`, `FailureUncertain`.
  - `func ClassifyFailure(err error) (FailureKind, string)` — kind and operation name.
  - `func cancellationNotice(ctx context.Context, err error) string` (package `handler`) — the results-pane text, `""` when the failure is not a cancellation.
  - `func (s *Server) runStatement(ctx context.Context, query string, vertical bool) (string, error)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/database/interbase_failure_stub_test.go`:

```go
//go:build !interbase || !cgo || !linux || !amd64

package database

import (
	"errors"
	"testing"
)

func TestClassifyFailureWithoutNativeBuildReportsNone(t *testing.T) {
	kind, operation := ClassifyFailure(errors.New("boom"))
	if kind != FailureNone {
		t.Errorf("kind = %v, want FailureNone", kind)
	}
	if operation != "" {
		t.Errorf("operation = %q, want \"\"", operation)
	}
	if kind, _ := ClassifyFailure(nil); kind != FailureNone {
		t.Errorf("ClassifyFailure(nil) = %v, want FailureNone", kind)
	}
}
```

Create `internal/database/interbase_failure_native_test.go`:

```go
//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
	"fmt"
	"testing"

	interbase "interbase-go"
)

func TestInterBaseClassifyFailureUncertainOutcome(t *testing.T) {
	canceled := &interbase.CancellationError{
		Operation: "execute statement",
		Mutating:  true,
		Context:   context.Canceled,
	}
	uncertain := &interbase.UncertainOutcomeError{
		Operation: "execute statement",
		Mutating:  true,
		Cause:     canceled,
		Cleanup:   fmt.Errorf("rollback failed"),
	}

	// An uncertain outcome wraps a cancellation, so the uncertain check must
	// come first or this reports FailureCanceled.
	if kind, operation := ClassifyFailure(uncertain); kind != FailureUncertain || operation != "execute statement" {
		t.Errorf("ClassifyFailure(uncertain) = (%v, %q), want (FailureUncertain, \"execute statement\")", kind, operation)
	}
	if kind, operation := ClassifyFailure(canceled); kind != FailureCanceled || operation != "execute statement" {
		t.Errorf("ClassifyFailure(canceled) = (%v, %q), want (FailureCanceled, \"execute statement\")", kind, operation)
	}
	if kind, _ := ClassifyFailure(fmt.Errorf("wrapped: %w", uncertain)); kind != FailureUncertain {
		t.Errorf("ClassifyFailure(wrapped uncertain) = %v, want FailureUncertain", kind)
	}
	if kind, _ := ClassifyFailure(fmt.Errorf("ordinary")); kind != FailureNone {
		t.Errorf("ClassifyFailure(ordinary) = %v, want FailureNone", kind)
	}
}
```

Create `internal/handler/failure_test.go`:

```go
package handler

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCancellationNoticeForContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := cancellationNotice(ctx, context.Canceled)
	if got != canceledMessage {
		t.Errorf("cancellationNotice = %q, want %q", got, canceledMessage)
	}
	if strings.Contains(got, "UNCERTAIN") {
		t.Error("the cancelled message must not mention an uncertain outcome")
	}
	if strings.Contains(got, "Do not re-run") {
		t.Error("the cancelled message must not carry the do-not-retry warning")
	}
}

func TestCancellationNoticeIsEmptyForOrdinaryFailure(t *testing.T) {
	if got := cancellationNotice(context.Background(), errors.New("syntax error")); got != "" {
		t.Errorf("cancellationNotice = %q, want \"\"", got)
	}
}

func TestQueryFailureMessagesMatchClassification(t *testing.T) {
	if !strings.Contains(uncertainOutcomeMessage, "UNCERTAIN") {
		t.Error("the uncertain message must contain UNCERTAIN")
	}
	if !strings.Contains(uncertainOutcomeMessage, "Do not re-run") {
		t.Error("the uncertain message must tell the user not to re-run the statement")
	}
	if strings.Contains(canceledMessage, "UNCERTAIN") {
		t.Error("the canceled message must not claim the outcome is uncertain")
	}
	if strings.Contains(canceledMessage, "Do not re-run") {
		t.Error("the canceled message must not carry the do-not-retry warning")
	}
	// The canceled message says the statement was stopped, never that it was
	// guaranteed stopped: the driver's cancellation is best effort.
	if strings.Contains(canceledMessage, "guaranteed") {
		t.Error("the canceled message must not promise a guarantee the driver cannot make")
	}
}
```

Create `internal/database/interbase_failure_live_test.go`, following the gating of the existing `internal/database/interbase_live_test.go`:

```go
//go:build interbase && cgo && linux && amd64

package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
)

// TestInterBaseLiveQueryCancellationReturnsTypedError cancels a deliberately
// slow catalog cross join and asserts the driver reports a typed cancellation
// outcome rather than an opaque error. The connection setup mirrors
// TestInterBaseLiveReadOnlyCatalog in interbase_live_test.go.
func TestInterBaseLiveQueryCancellationReturnsTypedError(t *testing.T) {
	databaseName := os.Getenv("INTERBASE_DATABASE")
	user := os.Getenv("INTERBASE_USER")
	password, passwordSet := os.LookupEnv("INTERBASE_PASSWORD")
	if databaseName == "" || user == "" || !passwordSet {
		t.Skip("set INTERBASE_DATABASE, INTERBASE_USER, and INTERBASE_PASSWORD to run the live InterBase test")
	}

	connection, err := Open(&DBConfig{
		Driver:         dialect.DatabaseDriverInterBase,
		DataSourceName: databaseName,
		User:           user,
		Passwd:         password,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	const slowQuery = `SELECT COUNT(*) FROM RDB$RELATION_FIELDS a, RDB$RELATION_FIELDS b, RDB$RELATION_FIELDS c`

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(250 * time.Millisecond)
		cancel()
	}()

	rows, err := connection.Conn.QueryContext(ctx, slowQuery)
	if err == nil {
		_ = rows.Close()
		t.Skip("the cross join completed before the cancellation took effect")
	}

	// Cancellation is best effort and a stalled call can outlive its context,
	// but when the statement does fail after a cancellation the failure must be
	// typed, never opaque.
	if kind, _ := ClassifyFailure(err); kind == FailureNone {
		t.Fatalf("ClassifyFailure(%v) = FailureNone, want FailureCanceled or FailureUncertain", err)
	}
}
```

Append to `internal/handler/concurrency_race_test.go`:

```go
func TestCancelledQueryRendersTheCancelledMessage(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	backend := installStubBackend(t)
	tx.addWorkspaceConfig(t, stubConnections("primary"))
	tx.textDocumentDidOpen(t, testFileURI, "SELECT 1;")

	gate := backend.gate("Query")
	requestID := jsonrpc2.ID{Str: "render-cancel", IsString: true}

	type callResult struct {
		out string
		err error
	}
	done := make(chan callResult, 1)
	go func() {
		var got string
		err := tx.conn.Call(tx.ctx, "workspace/executeCommand", lsp.ExecuteCommandParams{
			Command:   CommandExecuteQuery,
			Arguments: []interface{}{testFileURI},
		}, &got, jsonrpc2.PickID(requestID))
		done <- callResult{out: got, err: err}
	}()
	gate.waitEntered(t)

	if err := tx.conn.Notify(tx.ctx, "$/cancelRequest", cancelParams{ID: requestID}); err != nil {
		t.Fatal("conn.Notify $/cancelRequest:", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatal("conn.Call workspace/executeCommand:", res.err)
		}
		if !strings.Contains(res.out, "Cancelled. The statement was stopped before it finished.") {
			t.Errorf("result = %q, want the cancelled message", res.out)
		}
		if strings.Contains(res.out, "42") {
			t.Errorf("result = %q, want no rows for a cancelled statement", res.out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled command never completed")
	}
}
```

Add `"github.com/sourcegraph/jsonrpc2"` to that file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race -run 'ClassifyFailure|CancellationNotice|QueryFailureMessages|CancelledQueryRenders' ./internal/database/ ./internal/handler/ -v`
Expected: compile failure — `ClassifyFailure`, `FailureNone`, `cancellationNotice`, `canceledMessage` and `uncertainOutcomeMessage` are undefined.

- [ ] **Step 3: Write the minimal implementation**

Create `internal/database/failure.go`:

```go
package database

// FailureKind classifies a statement failure that a cancellation can explain.
// The enum is declared here, untagged, so the native and stub implementations
// of ClassifyFailure cannot drift apart.
type FailureKind int

const (
	// FailureNone means the error is not a cancellation outcome.
	FailureNone FailureKind = iota
	// FailureCanceled means the statement was stopped and its outcome is known.
	FailureCanceled
	// FailureUncertain means a write was canceled and its effect could not be
	// established. Such a statement must not be blindly retried.
	FailureUncertain
)
```

Create `internal/database/interbase_failure_native.go`:

```go
//go:build interbase && cgo && linux && amd64

package database

import (
	"errors"

	interbase "interbase-go"
)

// ClassifyFailure reports whether err is a canceled statement and, if so,
// whether its outcome is known. The uncertain case is checked first because an
// UncertainOutcomeError unwraps to the CancellationError it wraps, so the
// reverse order would report every uncertain write as merely canceled.
func ClassifyFailure(err error) (FailureKind, string) {
	if err == nil {
		return FailureNone, ""
	}
	var uncertain *interbase.UncertainOutcomeError
	if errors.As(err, &uncertain) && uncertain != nil {
		return FailureUncertain, uncertain.Operation
	}
	var canceled *interbase.CancellationError
	if errors.As(err, &canceled) && canceled != nil {
		return FailureCanceled, canceled.Operation
	}
	return FailureNone, ""
}
```

Create `internal/database/interbase_failure_stub.go`:

```go
//go:build !interbase || !cgo || !linux || !amd64

package database

// ClassifyFailure never recognises a driver cancellation outcome without the
// native build: the driver's error types are not linked in.
func ClassifyFailure(error) (FailureKind, string) {
	return FailureNone, ""
}
```

Create `internal/handler/failure.go`:

```go
package handler

import (
	"context"
	"errors"
	"log"

	"github.com/sqls-server/sqls/internal/database"
)

const canceledMessage = "Cancelled. The statement was stopped before it finished."

const uncertainOutcomeMessage = `Cancelled, but the outcome is UNCERTAIN.

InterBase could not confirm whether this statement took effect. Do not re-run it
until you have checked the database state — reconcile by operation id or by
querying the affected rows.`

// cancellationNotice renders the results-pane text for a statement that failed
// after its request was cancelled, or "" when the failure is something else.
// The driver's classifier is authoritative when it recognises the error; a bare
// context cancellation is reported as cancelled so the message is reachable on
// every driver and on an ordinary, untagged build.
func cancellationNotice(ctx context.Context, err error) string {
	switch kind, operation := database.ClassifyFailure(err); kind {
	case database.FailureUncertain:
		log.Printf("interbase: %s outcome is uncertain: %v", operation, err)
		return uncertainOutcomeMessage
	case database.FailureCanceled:
		log.Printf("interbase: %s canceled: %v", operation, err)
		return canceledMessage
	}
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		return canceledMessage
	}
	return ""
}
```

In `internal/handler/execute_command.go`, replace the statement loop of `executeQuery` (`:156-177`):

```go
	// execute statements
	buf := new(bytes.Buffer)
	for _, stmt := range stmts {
		query := strings.TrimSpace(stmt.String())
		if query == "" {
			continue
		}

		res, err := s.runStatement(ctx, query, showVertical)
		if err != nil {
			if notice := cancellationNotice(ctx, err); notice != "" {
				fmt.Fprintln(buf, notice)
				return buf.String(), nil
			}
			return nil, err
		}
		fmt.Fprintln(buf, res)
	}
	return buf.String(), nil
}

func (s *Server) runStatement(ctx context.Context, query string, vertical bool) (string, error) {
	if _, isQuery := database.QueryExecType(query, ""); isQuery {
		return s.query(ctx, query, vertical)
	}
	return s.exec(ctx, query, vertical)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race -run 'ClassifyFailure|CancellationNotice|QueryFailureMessages|CancelledQueryRenders' ./internal/database/ ./internal/handler/ -v`
Expected: all PASS.

- [ ] **Step 5: Verify the tagged build compiles and its tests run**

Run: `CGO_ENABLED=1 go build -tags interbase ./... && CGO_ENABLED=1 go test -tags interbase -run 'TestInterBaseClassifyFailureUncertainOutcome|TestInterBaseLiveQueryCancellationReturnsTypedError' ./internal/database/ -v`
Expected: the build succeeds, `TestInterBaseClassifyFailureUncertainOutcome` PASSES, and the live test either PASSES or SKIPS with "set INTERBASE_DATABASE, …" when the environment variables are unset. If the InterBase SDK is unavailable in this environment, record that the tagged build could not be verified and report it — do not weaken the code to make an untagged build pass.

- [ ] **Step 6: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/database/failure.go internal/database/interbase_failure_native.go internal/database/interbase_failure_stub.go internal/database/interbase_failure_native_test.go internal/database/interbase_failure_stub_test.go internal/database/interbase_failure_live_test.go internal/handler/failure.go internal/handler/failure_test.go internal/handler/execute_command.go internal/handler/concurrency_race_test.go
git commit -m "feat: classify and render cancelled and uncertain statement outcomes"
```

---

### Task 11: Document the concurrency invariants

Spec's Documentation section: "`doc/develop.md`: … the **server concurrency invariants**, because they are the kind of rule a future contributor breaks silently". The README gains the cancellation paragraph, which is the only user-visible behaviour this plan ships; the rest of the InterBase README section belongs to Plans 2–4.

**Files:**
- Modify: `doc/develop.md`
- Modify: `README.md` (the `### InterBase Build` section ends at line 90)

**Interfaces:**
- Consumes: every name introduced by Tasks 2–10.
- Produces: no code.

- [ ] **Step 1: Add the invariants section to `doc/develop.md`**

Append to `doc/develop.md`:

````markdown
## Server concurrency invariants

sqls served every request inline until the concurrency work landed. It is now a
two-threaded server, and these rules are what keep it correct. Breaking one of
them is silent until a user hits it, so `make test-race` is the check that
catches violations — it runs `go test -race ./...` and CI runs the same target.

**What runs concurrently.** `internal/handler/dispatch.go` wraps the handler so
that `workspace/executeCommand` runs in its own goroutine and every other
request is handled inline on the connection's read loop. Hover, completion,
signature help, definition, formatting and rename stay inline and take only
`stateMu`, so a long-running query never makes the editor feel dead. Do not
switch to `jsonrpc2.AsyncHandler`: making every request concurrent turns every
unsynchronised field into a race.

**Two locks, one order.** `Server` has two mutexes:

- `connMu` guards *connection lifetime*. `executeQuery`, `showDatabases`,
  `showSchemas`, `showTables` and `showConnections` take `connMu.RLock()` for
  the whole of their database work including rendering. `switchDatabase`,
  `switchConnections`, `handleInitialize` and the reconnect branch of
  `handleWorkspaceDidChangeConfiguration` take `connMu.Lock()` across
  `reconnectionDB`. `showConnections` is in the read set because
  `newDBConnection` writes `connCfg.DBName` in place on a `*database.DBConfig`
  that `showConnections` reads field-by-field.
- `stateMu` guards *mutable `Server` fields*.

**Lock ordering: `connMu` before `stateMu`, never the reverse**, and `stateMu`
is never held across any I/O. `reconnectionDB` therefore performs `Close`,
`Open` and `ReCache` while holding only `connMu.Lock()`, taking `stateMu.Lock()`
only for the pointer assignments. `getConfig`, `topConnection`, `getConnection`,
`parserDriver`, `newDBRepository` and `fileText` take `stateMu` internally, so
never call them while already holding it.

`Server.Stop`, `handleShutdown` and `handleExit` take no `connMu`: shutdown must
not block on a runaway query, and `sql.DB.Close` is documented as safe to call
while queries are in flight.

**The field audit.** Every field of `Server` is classified. Any new field must
be added here and classified, or it ships a race.

| Field | Written by | Read by | Treatment |
| --- | --- | --- | --- |
| `files` | `openFile`/`updateFile`/`closeFile` (inline) | every handler; `executeQuery` (async) | `stateMu` on every access, plus the copy rule below |
| `dbConn` | `reconnectionDB` (async-reachable) | `newDBRepository`, `parserDriver` | `stateMu` |
| `curDBCfg` | `newDBConnection` | `newDBRepository` | `stateMu` |
| `curDBName` | `switchDatabase` (async) | `newDBConnection` | `stateMu` |
| `curConnectionIndex` | `switchConnections` (async) | `newDBConnection` | `stateMu` |
| `WSCfg` | `handleWorkspaceDidChangeConfiguration` (inline) | `getConfig` ← `topConnection`/`showConnections`/`switchConnections` (async) | `stateMu` — a genuine inline-writer/async-reader race |
| `initOptionDBConfig` | `handleInitialize` (inline, once) | `topConnection` (async-reachable) | `stateMu` — write-once, but read from the async path |
| `SpecificFileCfg`, `DefaultFileCfg` | `main.go` before `jsonrpc2.NewConn` | `getConfig` | write-once-before-serving; an invariant, not a lock. Any future writer after serving begins must take `stateMu` |
| `worker` | `NewServer` | everywhere | pointer never reassigned; the contents are guarded by the worker's own lock |
| `cancels` | `NewServer` | `handleWorkspaceExecuteCommand`, `handleCancelRequest` | pointer never reassigned; the registry has its own mutex |

**The copy rule for `files`.** `updateFile` mutates `File.Text` through the
stored pointer, so holding `stateMu` only while looking the pointer up is not
enough. Read document text through `Server.fileText`, which copies the string
under the lock; never retain the `*File`.

**Cancellation.** `handleWorkspaceExecuteCommand` derives a cancellable context,
registers its `context.CancelFunc` under the request id, and deregisters on
return. `$/cancelRequest` looks the id up and cancels; an unknown id is a no-op,
because a cancellation that races the response is normal. Cancellation is best
effort: a statement that completed before the native cancellation took effect
is rendered normally with a note saying so.

**The worker.** `Worker.dbRepo` is read by the worker goroutine and written by
`ReCache` on the handler goroutine. Both go through `repo()`/`setRepo()` under
`w.lock`; no `Server` lock can cover that pair.
````

- [ ] **Step 2: Add the cancellation paragraph to `README.md`**

Insert after line 90 (the end of the `### InterBase Build` section, before `## Editor Plugins`):

```markdown
### Cancelling a running query

`workspace/executeCommand` runs off the request-handling loop, so sqls keeps
answering completion, hover and formatting while a query runs, and an editor
that sends `$/cancelRequest` stops it.

Cancellation is **best effort, not a hard deadline**. A statement that finished
before the cancellation took effect returns its real result, with a note saying
the cancellation arrived too late. For InterBase, a cancelled statement whose
effect could not be established is reported loudly:

> Cancelled, but the outcome is UNCERTAIN.
>
> InterBase could not confirm whether this statement took effect. Do not re-run
> it until you have checked the database state — reconcile by operation id or by
> querying the affected rows.

Do not blindly re-run a cancelled write.
```

- [ ] **Step 3: Verify the documents render and the claims match the code**

Run: `grep -n 'connMu before stateMu' doc/develop.md && grep -rn 'connMu.RLock' internal/handler/execute_command.go`
Expected: the invariant sentence appears in `doc/develop.md`, and the `connMu.RLock()` call sites in `execute_command.go` are exactly the five commands the table names.

- [ ] **Step 4: Run the whole suite one last time**

Run: `make test-race && go test ./...`
Expected: every package `ok` in both runs.

- [ ] **Step 5: Commit**

```bash
git add doc/develop.md README.md
git commit -m "docs: record the server concurrency invariants and cancellation behaviour"
```

---

## Out of scope for this plan

Named here so no task drifts into them. Each belongs to a later plan in the same spec:

- §6.2 `ReadOnlyQuerier` and the read-only transaction — Plan 2.
- §6.3 `ScanRowsWithTypes`, `QueryResult`, `ColumnMeta`, `RenderOptions`, `DefaultMaxCellRunes` and the partial-result contract, including the BLOB-limit rendering — Plan 2.
- §6.4 `EXECUTE PROCEDURE` routing by output arity — Plan 2.
- Features 1–4 (Explain, completion, signature help, hover DDL) — Plan 3. The hover DDL memo will live under `stateMu`; this plan provides the mutex, not the memo.
- Feature 5 (go-to-definition snapshots) — Plan 4. This plan restructures `Server.Stop` so Plan 4 can add `defer s.snapshots.RemoveAll()` as the first deferred statement, but adds no snapshot store.
- The InterBase feature sections of `README.md` — Plans 2–4.
