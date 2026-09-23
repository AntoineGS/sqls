# LSP Progressive Readiness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the LSP handshake independent of database work, integrate progressive metadata, and report loading/degraded state without blocking editing.

**Architecture:** A single connection coordinator processes captured connection intent off the JSON-RPC read loop. Server uses MetadataLoader snapshots instead of Worker. Coherent editor snapshots and coalesced diagnostics preserve correctness while categories arrive independently; a separate status reporter consumes bounded signals.

**Tech Stack:** Go 1.25.7, sourcegraph/jsonrpc2 v0.2.1, existing LSP types, context/sync, MetadataLoader from Plan 1 and InterBase MetadataPlan from Plan 2.

**Spec:** `docs/superpowers/specs/2026-09-23-progressive-metadata-readiness-design.md`, §§3–5, 7–9.

## Global Constraints

- Go 1.25.7; use the standard library for scheduling and synchronization.
- Keep DBRepository unchanged; add optional capabilities for specialized loading.
- Lock ordering: connMu before stateMu, never the reverse; stateMu never spans I/O.
- Published metadata and descriptors are immutable; every result is generation-fenced.
- Native InterBase code remains behind interbase && cgo && linux && amd64.
- Metadata uses read-only transactions; do not run DDL or write production data.
- Cancellation is best effort; never release a concurrency slot before its call returns.
- Preserve configured/resolved/source dialect distinctions and full catalog identifiers.
- No credentials, source SQL, or raw driver errors in client progress/status payloads.
- Run go test ./... and make test-race; tagged changes also require CGO_ENABLED=1 go test -tags interbase ./... and CGO_ENABLED=1 go build -tags interbase ./....

## Review Focus

1. Missing/late configuration and repeated initialized must not duplicate attachment (Task 2).
2. A superseded or late post-shutdown attach must close locally instead of installing (Task 2).
3. A switch queued behind a running query must not change that query's connection/config (Task 2).
4. Partial cache/variant transitions must not leak old names, DDL, or false diagnostics (Tasks 3–4).
5. Clients without progress support, stalled progress creation, or notification failure must still load (Task 5).

## Dependencies and file map

Plans 1 and 2 must be merged before integration begins. The connection and
consumer work touches shared files; execute tasks in order.

| Files | Responsibility |
| --- | --- |
| `internal/database/driver.go`, `interbase_native.go`, `interbase_stub.go`, `interbase_common.go` | optional context-aware open |
| `internal/handler/connection_lifecycle.go` (new) | one connection coordinator |
| `internal/handler/handler.go`, `execute_command.go` | bootstrap, lifecycle ownership, command integration |
| `internal/handler/editor_snapshot.go` (new) | coherent document/connection/cache capture |
| `internal/handler/diagnostics_scheduler.go` (new), `diagnostics.go` | coalesced analysis and derived catalog reuse |
| `internal/handler/metadata_status.go` (new), `internal/lsp/metadata_status.go` (new), `internal/lsp/lsp.go` | progress/status contract |
| `internal/database/worker.go`, `worker_test.go` | remove after migration; retain equivalent regressions in loader tests |
| `doc/develop.md`, `README.md` | lifecycle invariants and user behavior |

The previously written 2026-09-19 concurrency plan describes an older architecture.
Its connMu/stateMu ordering still applies; its requirement to connect inside
initialize and configuration handlers is superseded by this plan.

---

### Task 1: Thread lifecycle cancellation through InterBase attachment

**Files:** Modify `internal/database/driver.go`, `interbase_common.go`, `interbase_native.go`, `interbase_stub.go`, and matching driver/native/stub tests.

**Interfaces:** Add:

```go
type ContextOpener func(context.Context, *DBConfig) (*DBConnection, error)
func RegisterOpenContext(name dialect.DatabaseDriver, opener ContextOpener)
func OpenContext(ctx context.Context, cfg *DBConfig) (*DBConnection, error)
```

RegisterOpenContext follows RegisterOpen's duplicate-registration rule in a
separate registry initialized before serving. OpenContext checks cancellation,
prefers the registered context opener, otherwise calls the legacy Open
synchronously. If cancellation wins while a legacy open completes, close its
returned connection and return ctx.Err. Do not wrap uncancellable Open in an
unbounded goroutine. Keep Open's signature for existing callers.

Add `interBaseOpenContext`, `interBaseAttachContext`, and
`interBaseDiagnosticsContext`; current named functions become Background wrappers
so tests/callers stay compatible. Native/stub pairs must compile on both build
paths. Child ping/diagnostics timeout contexts derive from the supplied parent,
preserving existing connection-timeout and dialect-reattach behavior.

- [ ] **Step 1: Add a cancelled-before-open test and a delayed-opener cleanup test.** Register a unique test driver name once; assert no opener call for a cancelled context and one close when cancellation arrives before a legacy opener returns. Native tests assert a parent cancellation reaches PingContext/diagnostics and prevents a Dialect 1 reattach.

```go
ctx, cancel := context.WithCancel(context.Background())
cancel()
_, err := OpenContext(ctx, &DBConfig{Driver: dialect.DatabaseDriverInterBase})
if !errors.Is(err, context.Canceled) { t.Fatalf("error = %v", err) }
```

- [ ] **Step 2: Run** `go test ./internal/database -run 'TestOpenContext|TestInterBase.*Context' -count=1`; expect missing context API.
- [ ] **Step 3: Implement the optional registry and InterBase wrappers.** Preserve errors.Is wrapping and configuration validation. Root lifetime cancellation must reach both the initial attach and any dialect reattach; never replace it with context.Background inside the new functions.
- [ ] **Step 4: Run** `go test ./internal/database -count=1` and `CGO_ENABLED=1 go test -tags interbase ./internal/database -count=1`; existing auto-dialect tests must pass.
- [ ] **Step 5: Commit** `feat(database): propagate lifecycle context through InterBase open`.

### Task 2: Move connection bootstrap to a single coordinator

**Files:** Create `internal/handler/connection_lifecycle.go`, `connection_lifecycle_test.go`; modify `handler.go`, `execute_command.go`, `diagnostics.go`, `snapshot_store.go` (shutdown write fence), every handler file referencing `worker.Cache()`, `handler_test.go`, `concurrency_test.go`, `concurrency_race_test.go`, and handler fixtures referencing worker/ReCache. Task 3 refines these reads into a coherent operation snapshot.

**Interfaces:** Create these package-private definitions:

```go
type connectionState string
const (
    connectionIdle connectionState = "idle"
    connectionConnecting connectionState = "connecting"
    connectionReady connectionState = "ready"
    connectionFailed connectionState = "failed"
    connectionStopped connectionState = "stopped"
)
type connectionIntent struct {
    ID uint64
    Context context.Context
    Config *database.DBConfig
    ConnectionIndex int
    DatabaseName string
    Reply chan error // capacity one; exactly one terminal reply
}
type connectionCoordinator struct { /* private queue, cancellation, server */ }
func newConnectionCoordinator(s *Server) *connectionCoordinator
func (c *connectionCoordinator) Request(ctx context.Context, cfg *database.DBConfig, index int, dbName string) <-chan error
func (c *connectionCoordinator) Stop()
func cloneConnectionConfig(cfg *database.DBConfig) *database.DBConfig
```

Server gains lifecycleCtx/cancel, coordinator, metadata *database.MetadataLoader,
initialized bool, connectionState, and openConnection ContextOpener test seam.
The seam defaults to database.OpenContext and is assigned before serving. Add
all mutable fields to the developer field audit; pointer-owned objects guard
their own internal state. Construct background consumers once in NewServer.

The coordinator has one goroutine, a capacity-one wake signal, and one pending
intent slot. Replacing a pending intent returns context.Canceled to its waiter.
Request deep-copies effective Config after precedence resolution, including
Params, SSHCfg, InterBase, and TLS. Identical/overridden notifications neither
supersede an active attach nor retry an unchanged failed configuration; only a
changed effective config or explicit reselect retries. Bootstrap and
workspace configuration pass the server lifecycle context, not a notification's
short-lived handler context. Explicit switches pass their cancellable request
context. A cancelled queued intent is skipped; an active attempt has a context
cancelled on request cancellation, supersession, or Stop. Use context.AfterFunc
to bridge request cancellation into the lifecycle-derived attempt and stop that
registration on completion. Do not hold the
coordinator mutex while acquiring server locks, doing I/O, or sending a reply.

**Intent id is not connection generation.** A queued switch does not modify the
active config or generation while an executing query owns connMu.RLock. The
coordinator obtains connMu.Lock off the read loop, rechecks intent freshness,
then begins the transition under connMu → diagnosticsPublishMu → stateMu:
increment connGeneration, set connecting, detach the
old visible connection, clear DDL memo, reset MetadataLoader with that generation,
and trigger diagnostic/status signals. Reset/Start callbacks only enqueue
nonblocking signals, never take server locks synchronously. Release state locks
before opening; old attachment closes run on a bounded lifecycle-owned cleanup
worker, not the critical path to attaching the new connection.
Keep connMu across this transition to preserve query/switch serialization.

Open the candidate locally. Check intent/lifecycle cancellation before installing
it under the generation fence. A stale candidate is closed and cannot start
metadata. A current candidate is installed with resolved dialect, config/index,
and ready state; call metadata.Start with the lifecycle-derived generation
context, not the switch request's response context. Explicit switches finish
after attach/metadata scheduling; metadata errors are reported separately. If
metadata.Start rejects a plan, keep the attachment ready but publish a terminal
degraded metadata-status error instead of leaving an unstarted snapshot. A
generation-level `MetadataSnapshot.StartFailed` marker, set via
`MetadataLoader.MarkStartFailed(generation)`, records that no jobs were started;
it preserves the cache pointer and category statuses. This is not an attachment
failure and does not expose the raw internal error.

- [ ] **Step 1: Add a gated opener test proving handshake independence.**

```go
entered, release := make(chan struct{}), make(chan struct{})
s.openConnection = func(ctx context.Context, cfg *database.DBConfig) (*database.DBConnection, error) {
    close(entered)
    select { case <-release: return database.Open(cfg); case <-ctx.Done(): return nil, ctx.Err() }
}
// Send initialize with the existing mock connection config; it must reply
// before initialized is sent and before entered can close.
// Send initialized, wait for entered, then request formatting while release
// remains blocked. Assert formatting replies before closing release.
```

Use the existing net.Pipe/jsonrpc2 test transport rather than directly calling
handleInitialize: this verifies the actual read loop. Add no-config -> late-config,
duplicate initialized, configuration-during-attach, and Stop-during-attach cases.
Add a gated query followed by a switch: its original repository/config remain
stable until the query releases connMu, then the transition starts.

- [ ] **Step 2: Run** `go test ./internal/handler -run 'TestConnectionLifecycle|TestInitializeDoesNotWait' -count=1`; the current implementation blocks or lacks the seam.
- [ ] **Step 3: Implement coordinator integration.** handleInitialize only captures config/capabilities; initialized idempotently requests bootstrap. Late workspace config enqueues only after initialized and when disconnected/failed/connecting; preserve the existing rule that an established connection is not automatically switched by a settings notification. Config arriving before initialized is retained for bootstrap. Explicit switch commands validate arguments and database-switch capability on the async command path, release any validation lock, then enqueue/wait with select on their request context. They must not hold connMu while awaiting the coordinator. Remove in-place config DBName mutation. Stop cancels root/coordinator/metadata before connection/snapshot cleanup; an active attempt returning afterward closes its local connection. Worker is no longer constructed by Server.

Keep this task buildable: replace production worker.Cache calls with metadata.Cache
and migrate handler test installation helpers from ReCache to Reset/Start plus
an explicit wait for required readiness. Add a test-only
`loadMetadataForTest(t *testing.T, s *Server, repo database.DBRepository)` helper
that advances generation under diagnosticsPublishMu/stateMu, calls Reset/Start,
and awaits Done with a watchdog; tests intentionally loading partial failures
use Start directly. Remove the obsolete diagnosticsCacheGeneration gate and use
the metadata snapshot's matching generation plus ColumnsReady in
diagnosticsSnapshot. Task 3 adds coherent full-operation capture; Task 4 adds
coalescing. Preserve existing diagnostic publication fencing throughout.

Keep attachment-attempt cancellation distinct from metadata lifetime: pass the
server lifecycle context to metadata.Start (Reset cancels a generation), not an
attach child context that is deferred-cancelled when Open returns. Route shutdown,
exit, and Stop through one idempotent stop path. Schedule connection/snapshot
cleanup once on a lifecycle-owned cleanup goroutine, logging close errors; do not
call potentially blocking native Close inline on the JSON-RPC read loop. Existing
Stop() error returns scheduling errors only; asynchronous cleanup errors are
logged. Tests can await cleanup through a private done channel after releasing
native gates; the protocol response does not await that channel.
Fence snapshot-store writes once shutdown cleanup starts so a concurrent DDL
snapshot cannot recreate files after RemoveAll.

For commands needing a ready attachment, add
`acquireReadyConnection() (database.DBRepository, func(), error)`: use
connMu.TryRLock, check ready under stateMu, and return the RUnlock function with
the repository. If connecting or the write lock is held, return a clear transient
error immediately. Migrate executeQuery/showDatabases/showSchemas/showTables and
explain/parameter preparation where they own connection lifetime. A command that
already acquired the read lock keeps it until its work finishes. ShowConnections
only reads copied configuration and takes no connMu.

- [ ] **Step 4: Run** `go test -race ./internal/handler -run 'TestConnection|TestInitialize|Test.*Switch|Test.*Concurrent' -count=1`; then `go test ./internal/handler -count=1`. Update helpers that previously expected initialize to imply metadata completion to explicitly send initialized and await the needed readiness signal, never sleep for guessed load duration.
- [ ] **Step 5: Commit** `feat(lsp): bootstrap connections outside the initialize read loop`.

### Task 3: Capture coherent editor and metadata snapshots

**Files:** Create `internal/handler/editor_snapshot.go`, `editor_snapshot_test.go`; modify `completion.go`, `hover.go`, `signature_help.go`, `definition.go`, `interbase_hover.go`, `interbase_definition.go`, `interbase_relation_definition.go`, `parameter_type_inference.go`, `query_parameters.go`, `execute_command.go`, `diagnostics.go`, and tests that reference worker.

**Interfaces:** Introduce:

```go
type editorSnapshot struct {
    URI, Text string
    Version int
    Revision uint64
    Generation int
    Variant dialect.DriverVariant
    DialectResolved bool
    Cache *database.DBCache
    Metadata *database.MetadataSnapshot
    Repository database.DBRepository // nil until attached
}
func (s *Server) captureEditorSnapshot(uri string) (editorSnapshot, error)
```

Capture document fields and active connection/config under stateMu. Reading the
loader snapshot while stateMu is held is allowed only because loader callbacks
never hold its lock when touching server state. Compare loader/server generation;
substitute a safe empty cache on mismatch. Construct a repository only from the
captured connection/config, never a second current-state lookup. Until attachment,
derive the parser variant from configured driver/explicit dialect with the
existing default; DialectResolved is false for auto-dialect pending detection.
Do not retain mutable File or configuration pointers.

- [ ] **Step 1: Add connection A/B mixed-snapshot and partial-completion tests.** Gate A's metadata, begin B, then release A: completions and new DDL/snapshot writes must not contain A names. With procedures ready and views failed, procedure signature/completion is available. Before connection, keyword completion/formatting succeeds. Include explicit and auto Dialect 1 transitions.

```go
snapshot, err := s.captureEditorSnapshot(uri)
if err != nil { t.Fatal(err) }
if snapshot.Metadata != nil && snapshot.Metadata.Generation != uint64(snapshot.Generation) {
    if snapshot.Cache.HasCatalog() { t.Fatal("mixed connection/catalog generation") }
}
```

- [ ] **Step 2: Run** `go test ./internal/handler ./internal/completer -run 'TestEditorSnapshot|TestProgressive' -count=1`; expect missing snapshot contract or stale-state failures.
- [ ] **Step 3: Migrate every worker.Cache read.** Use one captured snapshot for each operation; do not mix s.parserDriverVariant with a separately acquired cache. Pass snapshot generation/variant into InterBase hover/definition work and reject stale memo/file publication. Keep cache-only paths independent of connMu. DDL calls already running can fail on close; they must not publish into a new generation. Completion now returns `lsp.CompletionList{Items: items, IsIncomplete: ...}`; incomplete is true while attaching or any completion-relevant supported category is pending/loading, false once relevant categories are terminal. Update client-facing tests for the standard list response. Nil/empty metadata must not panic any cache accessor.
- [ ] **Step 4: Run** `go test -race ./internal/handler ./internal/completer -count=1`; preserve full-cache navigation, DDL accuracy, and query-parameter behavior.
- [ ] **Step 5: Commit** `feat(lsp): serve editor features from coherent progressive snapshots`.

### Task 4: Coalesce diagnostics and reuse derived metadata

**Files:** Create `internal/handler/diagnostics_scheduler.go`, `diagnostics_scheduler_test.go`; modify `diagnostics.go`, `handler.go`, `diagnostics_test.go`, `singleton_diagnostics_test.go`.

**Interfaces:** Add `signalDiagnostics()` and one server-owned diagnostic worker.
Use a dirty-document set plus an all-open-documents bit protected by its own
mutex; the capacity-one signal only wakes the worker, it does not carry the
data. Define `diagnosticCatalogFor(cache *database.DBCache) sqlsymbol.Catalog`
on Server, caching only the current cache pointer and its immutable derived
catalog. The worker is the single builder; request-triggered consumers use the
same protected memo without mutating it.

- [ ] **Step 1: Add burst/coalescing and revision tests.** Hold analysis at a gate,
publish 50 metadata signals and several document changes, then release. Assert
one active analyzer, bounded pending work, latest document/version publication,
and only one catalog conversion for a given cache pointer. A status-only change
must not rebuild the catalog. A new cache pointer must rebuild once.

```go
for range 50 { s.signalDiagnostics() }
// The test's instrumented analyzer increments atomic active/peak counters.
if peak.Load() > 1 { t.Fatal("unbounded diagnostic fan-out") }
```

- [ ] **Step 2: Run** `go test ./internal/handler -run 'TestDiagnostics(Coalesces|CatalogReuse|Latest)' -count=1`; expect missing scheduler/reuse behavior.
- [ ] **Step 3: Implement signaling.** Document open/change queues that document;
cache data change/dialect resolution queues all documents. Preserve diagnosticsPublishMu's generation/revision recheck at send time and close-document clearing. Suppress dialect-sensitive analysis while auto detection is unresolved. Replace `go server.republishOpenDiagnostics(context.Background())` with a bounded signal. Mark dirty before signaling so coalescing cannot lose the last change; changes arriving during analysis cause another drain iteration. Stop via lifecycleCtx.

```go
select { case s.diagnosticsWake <- struct{}{}: default: }
```

Update diagnosticCatalog.Columns/UniqueKeys to use immutable stored data where
the sqlsymbol.Catalog contract permits it; retain defensive copies where callers
can mutate. Do not cache every historic revision in a growing map.

- [ ] **Step 4: Run** `go test -race ./internal/handler ./internal/sqlsymbol -count=1`; assert old-generation results and closed-document results remain suppressed.
- [ ] **Step 5: Commit** `perf(lsp): coalesce diagnostics and reuse catalog analysis inputs`.

### Task 5: Expose loading progress and a machine-readable status command

**Files:** Create `internal/lsp/metadata_status.go`, `internal/handler/metadata_status.go`, `metadata_status_test.go`; modify `internal/lsp/lsp.go`, `handler.go`, `execute_command.go`, and capability tests.

**Interfaces:** Extend ClientCapabilities with `Window.WorkDoneProgress bool` using
the standard `window`/`workDoneProgress` JSON keys. Add command constant
`CommandShowMetadataStatus = "sqls.showMetadataStatus"`. Define payload types:

```go
type MetadataCategoryStatus struct {
    Kind string `json:"kind"`
    State string `json:"state"`
    Count int `json:"count"`
    DurationMS int64 `json:"durationMs"`
    ErrorCode string `json:"errorCode,omitempty"`
}
type MetadataStatusResult struct {
    Generation uint64 `json:"generation"`
    Revision uint64 `json:"revision"`
    ConnectionState string `json:"connectionState"`
    ConnectionErrorCode string `json:"connectionErrorCode,omitempty"`
    MetadataErrorCode string `json:"metadataErrorCode,omitempty"`
    Settled bool `json:"settled"`
    Degraded bool `json:"degraded"`
    Categories []MetadataCategoryStatus `json:"categories"`
}
```

Status command is nonblocking and takes no connMu. Error codes are
metadata_load_failed, dependency_failed, cancelled, and connection_failed;
generation/kind form the correlation key in server logs. Sort categories in
defined job-kind order, not map iteration order. Never return raw Err strings.

For idle/no configuration and connecting states, Settled=false. A failed current
attachment has Settled=true and Degraded=true even though no metadata jobs were
started; ConnectionErrorCode is connection_failed.
After attachment, derive these flags from MetadataSnapshot. Stopped/cancelled
generations end progress as cancelled rather than degraded success.
For a ready attachment whose metadata scheduling failed before any job started,
report Settled/Degraded true and MetadataErrorCode `metadata_load_failed`; do
not claim any individual category failed or serialize the raw error.

- [ ] **Step 1: Add capability/failure tests over JSON-RPC.** Capture notifications
with existing test clients. Cases: advertised progress, no capability, create
request refused, create never answered, error containing a fake password, reset
during reporting, all jobs failed, and cancelled old generation. Assert one
terminal summary, no misleading fully-loaded text, and no fake password in
serialized output.

```go
body, err := json.Marshal(status)
if err != nil { t.Fatal(err) }
if bytes.Contains(body, []byte("secret-from-driver")) { t.Fatal("raw error leaked") }
```

- [ ] **Step 2: Run** `go test ./internal/handler -run TestMetadataStatus -count=1`; expect command/capability handling absent.
- [ ] **Step 3: Implement one reporter goroutine with a capacity-one wake channel.**
After initialized, use `window/workDoneProgress/create` with a generation-specific
string token and a 500 ms child-context budget; failure disables progress for
that generation only. Send `$/progress` begin/report/end values using that token.
Throttle reports to 100 ms, flush end immediately, and end an obsolete token as
cancelled before starting a new one. Reporter/client I/O never executes from a
loader callback or while holding stateMu/connMu/loader locks. Bound notification
sends by reporter context; broken client transport must not stall loading.
Fallback emits one window/logMessage summary and one warning on degraded
completion. User cancellation of work-done progress is not advertised in this
iteration; omit `cancellable` or set false. Add the command to advertised
execute-command capabilities and dispatch.
- [ ] **Step 4: Run** `go test -race ./internal/handler ./internal/lsp -count=1`; verify progress failures never change load outcomes.
- [ ] **Step 5: Commit** `feat(lsp): report progressive metadata readiness and failures`.

### Task 6: Complete cutover and document the new lifecycle

**Files:** Remove `internal/database/worker.go` after migrating callers; migrate
`worker_test.go` regressions into `metadata_loader_test.go` and remove the obsolete
test file; update `cache_test.go`, all handler tests referencing worker/ReCache,
`doc/develop.md`, `README.md`.

**Interfaces:** Server has exactly one MetadataLoader and one connection
coordinator. No production ReCache or Worker path remains. Legacy synchronous
DBCacheGenerator methods may remain for standalone tests/tools; they are not on
the LSP loading path.

- [ ] **Step 1: Locate all old callers.**

```sh
rg 'worker\.|NewWorker|ReCache|GenerateDBCachePrimary|GenerateCatalogCache' internal main.go
```

Move tests for old-generation column rejection, catalog-after-column-error,
idempotent Stop, and full-update-channel nonblocking behavior to their new
equivalents. Never delete a regression merely because its implementation changed.

- [ ] **Step 2: Run migrated regressions** using `go test ./internal/database ./internal/handler -count=1` before removing the old implementation; assert equivalent behavior on the new loader.
- [ ] **Step 3: Remove dead worker code and update docs.** Document readiness
milestones, status command, no automatic retry, same-connection reselect refresh,
safe partial completion, and best-effort cancellation. Replace obsolete claims
in doc/develop.md that initialize/configuration hold connMu or that HasCatalog
means complete. Add lifecycle/coordinator/diagnostic/status field and lock audit
rows. A no-op InterBase switchDatabase retains its existing no-op semantics;
reselecting switchConnections is the explicit refresh path.
- [ ] **Step 4: Run** `go test ./...`, `make test-race`, `CGO_ENABLED=1 go test -tags interbase ./...`, and `CGO_ENABLED=1 go build -tags interbase ./...`. Re-run the search and explain any remaining generator references; no production worker/ReCache call may remain.
- [ ] **Step 5: Commit** `refactor(lsp): finish progressive loader cutover and lifecycle docs`.

## Handoff

Give Plan 4 all test results, the status payload, any native cancellation limits
observed, and the baseline binary provenance from Plan 1. Startup availability is
now behaviorally verified; numerical performance claims still require Plan 4.
