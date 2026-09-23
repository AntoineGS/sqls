# Metadata Loader Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Introduce explicit category readiness and a bounded, failure-isolated metadata loader with immutable, generation-fenced results.

**Architecture:** Build a replacement loader alongside Worker, without activating it in Server yet. Jobs produce complete category fragments; a single merge path publishes them independently. Prepare consumers for partial catalogs before Plan 3 activates progressive loading.

**Tech Stack:** Go 1.25.7, context, sync, database/sql, existing mock repositories, Go race detector.

**Spec:** `docs/superpowers/specs/2026-09-23-progressive-metadata-readiness-design.md`, §§3–5, 7, 9.

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

1. A successfully empty category must differ from a failed/unstarted category (Task 1).
2. A late current-schema result must not overwrite a successful all-schema column result (Task 2).
3. Cancelled native work must continue counting against the global concurrency limit (Task 3).
4. One panic or invalid job graph must not kill the server or strand Done (Task 3).
5. Partial views/indexes must not produce false table identity or unique-key conclusions (Task 5).

## Prerequisites and file ownership

Read the spec and the package index. Before changing code, record `git rev-parse
HEAD`, `go version`, and `git -C ../interbase-go rev-parse HEAD`. Preserve a
baseline binary outside the working tree for Plan 4:

```sh
CGO_ENABLED=1 go build -tags interbase -o /tmp/opencode/sqls-performance-baseline .
go test ./...
make test-race
```

If native prerequisites are unavailable, preserve an untagged baseline with
`go build -o /tmp/opencode/sqls-performance-baseline .` and record that live
InterBase baseline measurement still needs a tagged build of the baseline commit.
Do not overwrite a pre-existing baseline artifact without checking its provenance.

| File | Responsibility |
| --- | --- |
| `internal/database/metadata.go` (new) | public internal contracts and readiness helpers |
| `internal/database/metadata_merge.go` (new) | category validation/immutable publication |
| `internal/database/metadata_loader.go` (new) | lifecycle, queue, limits, cancellation |
| `internal/database/metadata_jobs.go` (new) | generic repository adapter |
| `internal/database/cache.go` | cache state fields, successful legacy generator readiness |
| matching `*_test.go` files | focused behavioral tests |
| consumer files named in Task 5 | readiness-aware negative conclusions |

Do not modify Worker scheduling or Server startup in this plan. Plan 3 owns the
cutover, after the InterBase plan is ready. New file APIs below are the handoff
contract: later agents must not independently rename them.

---

### Task 1: Define metadata state and loader contracts

**Files:** Create `internal/database/metadata.go`, `internal/database/metadata_test.go`; modify `internal/database/cache.go`, `internal/database/cache_test.go`, and `internal/database/worker.go` only to mark categories ready during its existing copy-on-write publication.

**Interfaces:** Consumes existing DBCache/DBRepository. Produces the following definitions (imports context and time):

```go
type MetadataKind string
const (
    MetadataSchemas MetadataKind = "schemas"
    MetadataRelations MetadataKind = "relations"
    MetadataColumnsCurrent MetadataKind = "columns-current"
    MetadataColumnsAll MetadataKind = "columns-all"
    MetadataPrimaryKeys MetadataKind = "primary-keys"
    MetadataForeignKeys MetadataKind = "foreign-keys"
    MetadataViews MetadataKind = "views"
    MetadataProcedures MetadataKind = "procedures"
    MetadataGenerators MetadataKind = "generators"
    MetadataDomains MetadataKind = "domains"
    MetadataFunctions MetadataKind = "functions"
    MetadataIndexes MetadataKind = "indexes"
    MetadataTriggers MetadataKind = "triggers"
)
type MetadataState string
const (
    MetadataPending MetadataState = "pending"
    MetadataLoading MetadataState = "loading"
    MetadataReady MetadataState = "ready"
    MetadataFailed MetadataState = "failed"
    MetadataBlocked MetadataState = "blocked"
    MetadataCancelled MetadataState = "cancelled"
    MetadataUnsupported MetadataState = "unsupported"
)
type MetadataStatus struct {
    State MetadataState
    QueuedAt, StartedAt, FinishedAt time.Time
    Count int
    Queries int
    QueriesKnown bool
    Err error // internal only
}
type MetadataSnapshot struct {
    Generation, Revision uint64
    Cache *DBCache
    Status map[MetadataKind]MetadataStatus
}
type MetadataPatch struct {
    Cache *DBCache
    Count int
    Queries int
    QueriesKnown bool
}
type MetadataJob struct {
    Kind MetadataKind
    DependsOn []MetadataKind
    Run func(context.Context, *DBCache) (MetadataPatch, error)
}
type MetadataPlan struct { Parallelism int; Jobs []MetadataJob }
type MetadataPlanRepository interface { MetadataPlan() MetadataPlan }
```

Add these DBCache fields; outer PK key is `columnDatabaseKey(schema, table)`,
inner key is the catalog column spelling (use existing matching conventions):

```go
Metadata map[MetadataKind]MetadataState
PrimaryKeyColumns map[string]map[string]struct{}
```

Define `(*DBCache).MetadataReady(kinds ...MetadataKind) bool` and
`(*DBCache).ColumnsReady() bool`. Nil cache or absent state is unknown/false;
ColumnsReady accepts either current or all columns ready. Define
`(*MetadataSnapshot).Settled() bool` and `.Degraded() bool`; nil/empty snapshot
is not settled, failed/blocked is degraded, and cancelled is terminal but not a
current-generation failure warning. Unsupported is terminal, not failed.

QueriesKnown=false represents unavailable query accounting, not zero queries.
The loader copies query metrics from a returned patch even when Run returns an
error, but publishes its Cache only on success. Plan 4 populates these metrics.

- [ ] **Step 1: Add the empty-versus-unknown regression.**

```go
func TestMetadataReadyDistinguishesEmptyFromUnknown(t *testing.T) {
    cache := &DBCache{Catalog: &CatalogCache{Views: map[string]*ViewDesc{}},
        Metadata: map[MetadataKind]MetadataState{MetadataViews: MetadataLoading}}
    if cache.MetadataReady(MetadataViews) { t.Fatal("loading is not complete") }
    cache.Metadata[MetadataViews] = MetadataReady
    if !cache.MetadataReady(MetadataViews) { t.Fatal("empty success is complete") }
    if cache.MetadataReady(MetadataIndexes) { t.Fatal("absent state is unknown") }
}
```

- [ ] **Step 2: Run** `go test ./internal/database -run TestMetadataReady -count=1`; expect missing contract failure.
- [ ] **Step 3: Implement the definitions/helpers.** Preserve HasCatalog's data-presence meaning. In successful legacy primary generation mark schemas/relations/current columns/FKs ready; setColumnCache marks all columns ready; setCatalogCache marks all seven extended kinds ready. Copy the Metadata map in these legacy setters along with the existing DBCache copy; never infer readiness from non-nil maps. Do not change Worker scheduling. Update direct fixture constructors only where tests intentionally model fully loaded metadata.
- [ ] **Step 4: Run** `go test ./internal/database -count=1`; expect all tests pass, including nil and failed/blocked status cases added to metadata_test.go.
- [ ] **Step 5: Commit** `feat(metadata): define category readiness and load contracts` with only this task's files.

### Task 2: Implement immutable category merging

**Files:** Create `internal/database/metadata_merge.go`, `internal/database/metadata_merge_test.go`; modify `internal/database/cache.go` only for helper placement if necessary.

**Interfaces:** Produces `mergeMetadata(base *DBCache, kind MetadataKind, patch MetadataPatch) (*DBCache, error)` and `newMetadataCache() *DBCache`. These are package-private and consumed by the loader. A job returns only its kind's fields; merge ignores unrelated fragment fields and rejects nil payloads/negative counts/unknown kinds.

Merge ownership: schemas owns defaultSchema/Schemas; relations owns SchemaTables;
columns own ColumnsWithParent; PK owns PrimaryKeyColumns; FK owns ForeignKeys;
each extended kind owns its own Catalog map, plus IndexesByTable or
TriggersByTable where applicable. Rebuild grouped indexes from the incoming map
rather than accepting inconsistent secondary indexes. Never mutate a source map.

- [ ] **Step 1: Add independent-publication and retained-snapshot tests.**

```go
func TestMetadataMergeKeepsIndependentResults(t *testing.T) {
    before := newMetadataCache()
    first, err := mergeMetadata(before, MetadataProcedures, MetadataPatch{Cache: &DBCache{
        Catalog: &CatalogCache{Procedures: map[string]*ProcedureDesc{
            "P": {Name: "P"},
        }},
    }, Count: 1})
    if err != nil { t.Fatal(err) }
    second, err := mergeMetadata(first, MetadataViews, MetadataPatch{Cache: &DBCache{
        Catalog: &CatalogCache{Views: map[string]*ViewDesc{}},
    }})
    if err != nil { t.Fatal(err) }
    if _, ok := second.Procedure("P"); !ok { t.Fatal("lost sibling result") }
    if before.HasCatalog() { t.Fatal("mutated prior cache") }
    if first.MetadataReady(MetadataViews) { t.Fatal("mutated prior states") }
}
```

Add order permutations for PK-before-columns and columns-before-PK, checking
retained ColumnDesc pointers still contain their original Key values. Add both
current/all-column completion orders and a zero-column relation.

- [ ] **Step 2: Run** `go test ./internal/database -run 'TestMetadataMerge' -count=1`; expect missing merge implementation.
- [ ] **Step 3: Implement copy-on-write merge.** Copy DBCache and its Metadata map; copy CatalogCache before replacing a category. Initial cache has allocated base maps and nil Catalog. Set the merged kind ready. All-columns wins over a late current-columns result. For independent PK enrichment, clone every modified ColumnDesc; mark keys YES/NO only when PK status is ready, otherwise preserve generic-driver flags and InterBase's unknown empty flags.

```go
next := *base
next.Metadata = maps.Clone(base.Metadata)
next.Metadata[kind] = MetadataReady
// Replacing an extended map first copies CatalogCache, never mutates base.Catalog.
```

- [ ] **Step 4: Run** `go test -race ./internal/database -run TestMetadataMerge -count=1`; expect all order and immutability assertions pass.
- [ ] **Step 5: Commit** `feat(metadata): merge successful categories immutably`.

### Task 3: Build the bounded, generation-aware scheduler

**Files:** Create `internal/database/metadata_loader.go`, `internal/database/metadata_loader_test.go`.

**Interfaces:** Produces:

```go
type MetadataLoader struct { /* private synchronization and state */ }
type MetadataLoad struct { Done <-chan struct{} }
func NewMetadataLoader() *MetadataLoader
func (l *MetadataLoader) Reset(generation uint64)
func (l *MetadataLoader) Start(ctx context.Context, generation uint64, repo DBRepository) (*MetadataLoad, error)
func (l *MetadataLoader) Snapshot() *MetadataSnapshot
func (l *MetadataLoader) Cache() *DBCache
func (l *MetadataLoader) SetChangedCallback(callback func())
func (l *MetadataLoader) Stop()
func (l *MetadataLoader) Wait(ctx context.Context) error
```

Define sentinels `ErrMetadataStaleGeneration`, `ErrMetadataAlreadyStarted`,
`ErrMetadataStopped`, `ErrInvalidMetadataPlan`. Reset accepts strictly increasing
generations starting at one, ignores older/equal resets, and invalidates the
previous generation immediately. Start accepts only the current, unstarted
generation. Snapshot returns an immutable pointer. Callback executes outside
locks and must be short/nonblocking; callers enqueue work. Cache returns
Snapshot().Cache or a safe empty cache. Stop is idempotent and nonblocking;
Wait waits for actual runner drainage and respects its own context. Done means
logical settlement, including cancellation, not guaranteed native drainage.

- [ ] **Step 1: Add a test-only plan repository.**

```go
type metadataPlanTestRepo struct {
    *MockDBRepository
    plan MetadataPlan
}
func (r metadataPlanTestRepo) MetadataPlan() MetadataPlan { return r.plan }

func TestMetadataLoaderSiblingSurvivesFailure(t *testing.T) {
    loader := NewMetadataLoader()
    t.Cleanup(loader.Stop)
    repo := metadataPlanTestRepo{MockDBRepository: NewMockDBRepository(nil),
        plan: MetadataPlan{Parallelism: 2, Jobs: []MetadataJob{
            {Kind: MetadataViews, Run: func(context.Context, *DBCache) (MetadataPatch, error) {
                return MetadataPatch{}, errors.New("injected")
            }},
            {Kind: MetadataProcedures, Run: func(context.Context, *DBCache) (MetadataPatch, error) {
                return MetadataPatch{Cache: &DBCache{Catalog: &CatalogCache{
                    Procedures: map[string]*ProcedureDesc{"P": {Name: "P"}},
                }}, Count: 1}, nil
            }},
        }}}
    loader.Reset(1)
    load, err := loader.Start(context.Background(), 1, repo)
    if err != nil { t.Fatal(err) }
    select { case <-load.Done: case <-time.After(2*time.Second): t.Fatal("stranded load") }
    if _, ok := loader.Cache().Procedure("P"); !ok { t.Fatal("sibling lost") }
    if loader.Snapshot().Status[MetadataViews].State != MetadataFailed { t.Fatal("failure hidden") }
}
```

Add table cases replacing injected error with panic, dependency failure, and
context cancellation. Add graph validation for duplicate/unknown kinds, missing
dependencies, cycles, nil Run, and parallelism outside 1..3. Invalid plans return
an error before any Run executes. Add gates and atomic active/peak counters for
three blocked old jobs followed by Reset/Start: the replacement must not start a
fourth native job. Release gates in test cleanup so race tests can drain.

- [ ] **Step 2: Run** `go test ./internal/database -run TestMetadataLoader -count=1`; expect missing loader failure.
- [ ] **Step 3: Implement the scheduler.** Initialize all kinds unsupported, then scheduled kinds pending. Scan the small ordered job list for ready dependencies; mark failures' transitive dependents blocked. Use a capacity-three semaphore owned by the loader across all generations, plus plan-local parallelism. Acquire capacity before launching a Run goroutine. Capture one immutable cache pointer for dependencies. Wrap each Run with ordinary Go panic recovery and stack logging, and defer resource release. Collect results in a bounded channel; send/select must also observe cancellation. Under the loader mutex recheck generation and current context before publishing. Mark terminal statuses exactly once. Reset/Stop mark pending/loading jobs cancelled and settle Done; obsolete callbacks/results cannot update the current snapshot. Wait tracks real runners without racing WaitGroup.Add against a zero-count Wait (close intake on Stop, or maintain an explicit active-count/drained channel).

For this task, Start obtains its plan from MetadataPlanRepository and returns
ErrInvalidMetadataPlan when that capability is absent. Task 4 replaces that plan
selection with metadataPlanFor to support ordinary repositories; no undefined
adapter symbol is needed to compile or test this scheduler commit.
- [ ] **Step 4: Run** `go test -race ./internal/database -run 'TestMetadataLoader|TestMetadataMerge' -count=1`; expect bounded peak, no data races, no stranded Done, and no late old-generation results.
- [ ] **Step 5: Commit** `feat(metadata): isolate jobs with bounded generation-aware scheduling`.

### Task 4: Adapt generic repositories without eager snapshots

**Files:** Create `internal/database/metadata_jobs.go`, `internal/database/metadata_jobs_test.go`; modify `internal/database/metadata_loader.go` to use the adapter.

**Interfaces:** Produces `metadataPlanFor(repo DBRepository) MetadataPlan`. Prefer MetadataPlanRepository. Otherwise return parallelism one and independently owned job fragments from existing methods. No CatalogSnapshot call in this new path.

| Job | Call/dependency | Payload |
| --- | --- | --- |
| schemas | CurrentSchema, Schemas | defaultSchema and normalized Schemas |
| relations | SchemaTables; no dependency | normalized SchemaTables |
| current columns | schemas -> DescribeDatabaseTableBySchema | genColumnMap |
| all columns | DescribeDatabaseTable; independent | genColumnMap |
| foreign keys | schemas -> DescribeForeignKeysBySchema | existing FK grouping |
| each extended kind | corresponding CatalogRepository method; independent | category map/grouping |

Use deterministic sorted schema selection if CurrentSchema is empty and a
fallback is necessary. Plain MockDBRepository must not accidentally acquire the
optional plan or catalog interfaces.

- [ ] **Step 1: Add tests that fail each primary prerequisite while a CatalogRepository sibling still succeeds.** Reuse catalogTestRepository and its MockDescribeViews/MockDescribeProcedures hooks. Assert CurrentSchema failure blocks current columns/FK only; all columns and extended categories still settle independently. Use the test repository from Task 3 to check optional-plan precedence.

```go
plan := metadataPlanFor(catalogTestRepository())
if plan.Parallelism != 1 { t.Fatalf("generic parallelism = %d", plan.Parallelism) }
for _, job := range plan.Jobs {
    if job.Kind == MetadataProcedures && len(job.DependsOn) != 0 {
        t.Fatal("procedures depend on unrelated primary metadata")
    }
}
```

- [ ] **Step 2: Run** `go test ./internal/database -run TestMetadataGeneric -count=1`; expect missing adapter/failing independence assertions.
- [ ] **Step 3: Implement the adapter.** Factor existing FK grouping into a pure helper accepting `[]*ForeignKey`; reject malformed empty pairs with an error instead of indexing blindly. Close over the repository only; never share a mutable builder between jobs. Category accessors returning unsupported rendering fields still preserve descriptors as today.
- [ ] **Step 4: Run** `go test -race ./internal/database -count=1`; verify existing generators still work for callers not migrated yet.
- [ ] **Step 5: Commit** `feat(metadata): adapt repository reads into independent jobs`.

### Task 5: Make consumers explicitly readiness-aware

**Files:** Modify `internal/handler/diagnostics.go`, `internal/handler/interbase_hover.go`, `internal/handler/interbase_relation_definition.go`, `internal/handler/interbase_definition.go`, `internal/handler/query_parameters.go`, `internal/handler/execute_command.go`, relevant matching tests, `doc/develop.md`. Inspect `internal/completer/interbase_candidates.go` and `internal/handler/parameter_type_inference.go` for absence-based decisions too.

**Interfaces:** Consumes DBCache.MetadataReady/ColumnsReady; produces no new public API. Preserve lookup success on loaded descriptors even while unrelated categories are pending. Retain HasCatalog for presence checks only.

- [ ] **Step 1: Add partial-catalog regressions.** Use a cache with a relation named V and columns, but views loading. It must not be classified as a table DDL target. Mark views ready-empty and table fallback becomes valid. For diagnostics, indexes ready-empty plus views loading must return unknown keys, not known-empty keys. Add a failed procedure-category case to existing query-parameter/execution tests and verify the existing unknown-metadata path rather than claiming the procedure has no outputs.

```go
cache.Metadata = map[database.MetadataKind]database.MetadataState{
    database.MetadataColumnsCurrent: database.MetadataReady,
    database.MetadataViews: database.MetadataLoading,
    database.MetadataIndexes: database.MetadataReady,
}
if _, known := diagnosticUniqueKeys(cache, "T", columns); known {
    t.Fatal("incomplete view classification cannot prove table keys")
}
```

Here `cache`/`columns` use the existing diagnostics test fixture for a unique-key
table; add the mutation immediately before its current diagnosticUniqueKeys call.

- [ ] **Step 2: Run** `go test ./internal/handler ./internal/completer -count=1`; expect the newly asserted absence/completeness cases to fail before changes.
- [ ] **Step 3: Implement gates at negative conclusions.** `diagnosticUniqueKeys` requires ColumnsReady and views/indexes ready; table fallback requires views ready. Unknown procedure metadata keeps existing explanatory behavior and must not invent a complete signature. Mark known-full test fixtures explicitly ready, using local fixture helpers rather than production fallback inference. Update doc/develop.md's HasCatalog explanation.
- [ ] **Step 4: Run** `go test ./...` and `make test-race`; expect no changed results for fully loaded legacy caches.
- [ ] **Step 5: Commit** `fix(metadata): distinguish partial catalogs from complete results`.

## Handoff

Deliver commit ids, test output, the recorded baseline artifact provenance, and
the exact API definitions to Plan 2. The new loader is intentionally not yet
wired into Server. Do not report startup speed improvement at this milestone.
