# InterBase Metadata Jobs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Load InterBase categories independently with at most three concurrent read-only jobs and remove repeated core reads and per-parent extended-catalog queries.

**Architecture:** InterBase implements Plan 1's optional MetadataPlanRepository. Each job owns one read-only transaction; narrow bulk readers return existing schema types/descriptors. The generic loader publishes complete categories and owns failure handling, generation checks, and concurrency limits.

**Tech Stack:** Go 1.25.7, database/sql, interbase-go/schema, existing SQLite-backed catalog fixtures, tagged native integration tests.

**Spec:** `docs/superpowers/specs/2026-09-23-progressive-metadata-readiness-design.md`, §§5–6, 9.

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

1. PK/FK failure must not prevent relation/column loading, or invent NO key flags (Task 2).
2. Wider-than-67-byte and common-prefix identifiers must remain distinct (Tasks 1–5).
3. Failed rows iteration/close/rollback must not publish a seemingly complete category (Tasks 1, 6).
4. Zero-child parents, shuffled child rows, and missing optional domains must retain valid identity/order (Tasks 3–5).
5. New query counts must stay constant as object counts grow and must count metadata discovery too (Tasks 2–6).

## Prerequisites and ownership

Requires Plan 1 complete. Read `metadata.go`, `metadata_merge.go`, and
`metadata_jobs.go` before editing. All production changes in this plan live in
sqls; driver source is a reference and existing oracle, not a second worktree to
modify. Keep standalone Describe*/ObjectDDL APIs and legacy CatalogSnapshot
behavior for existing callers. The new MetadataPlan path bypasses the eager
snapshot entirely.

New files: `interbase_metadata_jobs.go` (plan/transaction wrapper),
`interbase_metadata_identifiers.go` (bounded width discovery),
`interbase_metadata_relations.go` (independent core readers/views),
`interbase_metadata_procedures.go`, `interbase_metadata_indexes.go`,
`interbase_metadata_functions.go`, and matching tests. Existing
`interbase_cache_bulk.go` retains common query/row helpers;
`interbase_catalog.go` retains descriptor rendering extracted into pure helpers.

---

### Task 1: Provide job-scoped transactions and identifier projections

**Files:** Create `internal/database/interbase_metadata_jobs.go`, `interbase_metadata_identifiers.go`, and their matching tests; modify query builders in `interbase_cache_bulk.go` with corresponding existing tests.

**Interfaces:** Introduce:

```go
type interBaseMetadataRead func(context.Context, schema.Queryer, int) (MetadataPatch, error)
func (db *InterBaseDBRepository) runMetadataRead(ctx context.Context, read interBaseMetadataRead) (MetadataPatch, error)
func interBaseMetadataIdentifierWidth(ctx context.Context, q schema.Queryer) (int, error)
func interBaseMetadataIdentifier(ref string, width int) string
```

runMetadataRead begins one `sql.TxOptions{ReadOnly:true, Isolation:sql.LevelSnapshot}` transaction, discovers
identifier width within it, calls read with that transaction, and rolls back
before returning its result. Defer rollback immediately to cover panic unwinding;
the scheduler catches the panic after rollback. No read may use db.Conn while
holding the transaction. Join non-cancellation rollback errors with the read
error. sql.ErrTxDone after context cancellation is expected; a successful read
with unexpected cleanup failure becomes a failed job.

For new builders, use a single bounded width query over string catalog fields:

```sql
SELECT MAX(f.RDB$FIELD_LENGTH)
FROM RDB$RELATION_FIELDS rf
JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE
WHERE rf.RDB$RELATION_NAME LIKE 'RDB$%'
  AND f.RDB$FIELD_TYPE IN (14, 37)
```

This reads byte capacity, not current name length. Reject NULL/nonpositive width
instead of guessing 67; format only the scanned integer, never user text. This
conservative maximum covers every identifier projection in that job, with one
metadata-discovery statement irrespective of object count. Confirm on live
InterBase that the resulting VARCHAR width is accepted. Existing driver-backed
simple-category reads keep their own bounded width discovery; do not pay this
extra query for those jobs.

- [ ] **Step 1: Add transaction ownership and width tests.** Use a test database/sql driver recording BeginTx options, open connections, rollback count, and cancellation. Add catalog fixture names wider than 67 bytes and two names sharing their first 67 bytes. Assert the new projection uses the discovered capacity.

```go
func TestInterBaseMetadataIdentifierUsesDiscoveredWidth(t *testing.T) {
    got := interBaseMetadataIdentifier("r.RDB$RELATION_NAME", 127)
    if got != "CAST(r.RDB$RELATION_NAME AS VARCHAR(127))" { t.Fatal(got) }
}
```

- [ ] **Step 2: Run** `go test ./internal/database -run 'TestInterBaseMetadata(Transaction|Identifier)' -count=1`; expect missing helpers.
- [ ] **Step 3: Implement helpers and parameterize existing core SQL builders.** Extract SQL-building functions taking width; legacy variables call them with the existing legacy constant, while new jobs call with discovered width. Do not change field order or joins in the current bulk SQL. Use this cleanup pattern, with errors.Join imported:

```go
defer func() {
    closeErr := tx.Rollback()
    if closeErr != nil && !(errors.Is(closeErr, sql.ErrTxDone) && ctx.Err() != nil) {
        err = errors.Join(err, closeErr)
        result = MetadataPatch{}
    }
}()
```

- [ ] **Step 4: Run** `go test -race ./internal/database -run 'TestInterBase(Metadata|Bulk)' -count=1`; verify existing legacy query tests still pass.
- [ ] **Step 5: Commit** `feat(interbase): add isolated read-only metadata job transactions`.

### Task 2: Split core metadata into independent bulk jobs

**Files:** Create `internal/database/interbase_metadata_relations.go` and matching tests; modify `interbase_metadata_jobs.go`, `interbase_cache_bulk.go` only to reuse pure scans.

**Interfaces:** Produce these readers (all return MetadataPatch, error):

```go
func (db *InterBaseDBRepository) readMetadataRelations(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error)
func (db *InterBaseDBRepository) readMetadataColumns(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error)
func (db *InterBaseDBRepository) readMetadataPrimaryKeys(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error)
func (db *InterBaseDBRepository) readMetadataForeignKeys(ctx context.Context, q schema.Queryer, width int) (MetadataPatch, error)
```

Relations returns SchemaTables with empty-schema key. Columns scans all rows into
genColumnMap without requiring the relation list; its SQL already filters user
relations. Use interBaseBulkColumnRow and columnDescription, explicitly setting
Key to empty until the PK merge. PK returns PrimaryKeyColumns; FK returns the
grouped ForeignKeys produced by Plan 1's pure grouping helper. Each reader uses
one data SELECT plus wrapper width discovery. Count columns/relations/constraints
as the job's objects, not groups accidentally produced by the map.

- [ ] **Step 1: Write PK/FK isolation tests with gated/failing queryer injection.** Add a small schema.Queryer wrapper that delegates to the fixture except for one matching SQL query, where it returns the injected error. Do not use error-string classification in production. Test the four readers independently and together through MetadataLoader.

```go
if !snapshot.Cache.MetadataReady(MetadataRelations, MetadataColumnsCurrent) {
    t.Fatal("FK error suppressed usable core metadata")
}
if snapshot.Status[MetadataForeignKeys].State != MetadataFailed { t.Fatal("FK failure hidden") }
```

Add PK-before/after-columns cases and preserve a zero-column relation. At sizes
1 and 100, assert each reader performs one data query and one width query, rather
than an all-four-queries snapshot. Use interBaseFixtureCountPrepares after fixture
setup; never t.Parallel tests sharing its global counter.

- [ ] **Step 2: Run** `go test ./internal/database -run TestInterBaseMetadataCore -count=1`; expect missing readers or coupling failure.
- [ ] **Step 3: Implement the readers.** Keep required-name validation, ordered FK segment pairing, full constraint identities, and drop-whole-incomplete-FK behavior. Extract reusable scans rather than maintaining two divergent copies. Build `MetadataPatch{Cache: &DBCache{...}, Count: n}`; maps are owned by the fragment.
- [ ] **Step 4: Run** `go test -race ./internal/database -run 'TestInterBase(MetadataCore|Bulk|Catalog)' -count=1`; compare core results to legacy snapshot on the small fixture after all core jobs settle.
- [ ] **Step 5: Commit** `perf(interbase): load core metadata once in independent jobs`.

### Task 3: Bulk-load views and indexes

**Files:** Modify `internal/database/interbase_metadata_relations.go`, `interbase_catalog.go`; create `interbase_metadata_indexes.go` and matching tests.

**Interfaces:** Produce `readMetadataViews` and `readMetadataIndexes` methods with
the same `(ctx, q, width) (MetadataPatch,error)` signature as Task 2. Extract pure
`viewDescriptions(views []schema.Relation) []*ViewDesc` and
`indexDescriptions(indexes []schema.Index) []*IndexDesc` methods from the current
DescribeViews/DescribeIndexes implementations; both old/new paths call them.

- [ ] **Step 1: Add equality and constant-query-count tests.** Extend existing scalable catalog fixtures with views and indexes of count 1 and 100. Compare every descriptor field against DescribeViews/DescribeIndexes on the original repository; perform oracle reads before resetting counters. Test shuffled segments, empty index segments, inactive/expression indexes, database-level names with common prefixes, and zero-column views. New path uses exactly two data queries plus one width query per category.

```go
if diff := cmp.Diff(want, got); diff != "" { t.Fatalf("descriptor mismatch (-want +got):\n%s", diff) }
if queries != 3 { t.Fatalf("queries = %d, want 2 data + 1 width", queries) }
```

- [ ] **Step 2: Run** `go test ./internal/database -run 'TestInterBaseMetadata(Views|Indexes)' -count=1`; expect missing readers.
- [ ] **Step 3: Implement bulk headers plus child grouping.** Views: select the fields consumed by ViewDesc from RDB$RELATIONS, filtered to user views; use the core column projection restricted to views for query two. Indexes: adapt `indexQueryTemplate` and `scanIndex` from `../interbase-go/schema/catalog_extended.go`, preserving all fields consumed by IndexDesc; query all segments joined to user indexes, ordered by index name/field position. Use dynamic casts on identifiers. Group by exact catalog identity before normalizing cache keys. Parent headers and children come from the same job transaction.

```sql
SELECT CAST(s.RDB$INDEX_NAME AS VARCHAR(127)),
       CAST(s.RDB$FIELD_NAME AS VARCHAR(127)), s.RDB$FIELD_POSITION
FROM RDB$INDEX_SEGMENTS s
JOIN RDB$INDICES i ON i.RDB$INDEX_NAME = s.RDB$INDEX_NAME
WHERE COALESCE(i.RDB$SYSTEM_FLAG, 0) = 0
ORDER BY s.RDB$INDEX_NAME, s.RDB$FIELD_POSITION
```

The 127 in this illustrative SQL is supplied by the width argument, not a
production constant. Append child records to existing parents; an orphan child
is a category error instead of inventing a parent. Preserve a header with no
children. Missing/duplicate required positions are errors; optional expression
metadata retains existing semantics.

- [ ] **Step 4: Run** `go test -race ./internal/database -run 'TestInterBase(Metadata|Catalog|Bulk)' -count=1`; ensure legacy DDL tests remain unchanged.
- [ ] **Step 5: Commit** `perf(interbase): batch view columns and index segments`.

### Task 4: Bulk-load procedures and their parameter domains

**Files:** Create `internal/database/interbase_metadata_procedures.go` and matching tests; modify `interbase_catalog.go` to extract pure mapping.

**Interfaces:** Produce `readMetadataProcedures(ctx context.Context, q schema.Queryer, width int) (MetadataPatch,error)` and `procedureDescriptions(procedures []schema.Procedure) []*ProcedureDesc` methods.

- [ ] **Step 1: Add descriptor parity and query-count tests.** Include a no-parameter procedure, input/output parameters with shuffled insertion order, a user domain, an inline RDB$ domain, missing domain, nullable/non-nullable domain, numeric Dialect 1/3 types, and verbatim source/description BLOBs. Compare the complete descriptors with legacy DescribeProcedures; assert exactly two data queries plus one width query for 1 and 100 procedures.

```go
for _, dialect := range []int{1, 3} {
    t.Run(strconv.Itoa(dialect), func(t *testing.T) {
        fixture := openInterBaseSchemaFixture(t)
        repo := &InterBaseDBRepository{Conn: fixture, SQLDialect: dialect}
        want, err := repo.DescribeProcedures(context.Background())
        if err != nil { t.Fatal(err) }
        patch, err := repo.runMetadataRead(context.Background(), repo.readMetadataProcedures)
        if err != nil { t.Fatal(err) }
        got := patch.Cache.Catalog.Procedures
        if len(got) != len(want) { t.Fatalf("count = %d, want %d", len(got), len(want)) }
        for _, procedure := range want {
            if diff := cmp.Diff(procedure, got[catalogCacheKey(procedure.Name)]); diff != "" {
                t.Fatal(diff)
            }
        }
    })
}
```

- [ ] **Step 2: Run** `go test ./internal/database -run TestInterBaseMetadataProcedures -count=1`; expect missing reader.
- [ ] **Step 3: Implement two reads.** Header query uses the current driver `procedureProjection` fields needed by ProcedureDesc. Child query starts from `procedureParametersQueryTemplate`, removes its single-procedure predicate, joins user RDB$PROCEDURES, and LEFT JOINs RDB$FIELDS/CHARACTER_SETS/COLLATIONS to include the full domain projection that `scanDomain` supplies. Do not call catalog.Domain or loadParameterDomain inside the loop. Match driver `parameterNullable`: a non-nullable domain proves false, otherwise nullable remains unknown. Keep domain.SystemFlag so interBaseUserDomainName still discriminates inline domains.

```sql
FROM RDB$PROCEDURE_PARAMETERS pp
JOIN RDB$PROCEDURES p ON p.RDB$PROCEDURE_NAME = pp.RDB$PROCEDURE_NAME
LEFT JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = pp.RDB$FIELD_SOURCE
LEFT JOIN RDB$CHARACTER_SETS cs ON cs.RDB$CHARACTER_SET_ID = f.RDB$CHARACTER_SET_ID
LEFT JOIN RDB$COLLATIONS co ON co.RDB$CHARACTER_SET_ID = f.RDB$CHARACTER_SET_ID
                         AND co.RDB$COLLATION_ID = f.RDB$COLLATION_ID
WHERE COALESCE(p.RDB$SYSTEM_FLAG, 0) = 0
ORDER BY pp.RDB$PROCEDURE_NAME, pp.RDB$PARAMETER_TYPE, pp.RDB$PARAMETER_NUMBER
```

Scan required identity/direction/position before attaching the child. Reject
unknown directions and duplicate positions within one direction. Reuse
parameterDescriptions for final rendering; source dialect behavior is unchanged.

- [ ] **Step 4: Run** `go test -race ./internal/database ./internal/handler -run 'TestInterBase|TestParameter' -count=1`; expect signature and parameter-inference regressions green.
- [ ] **Step 5: Commit** `perf(interbase): batch procedure parameters and domain metadata`.

### Task 5: Bulk-load UDF arguments and assemble the InterBase plan

**Files:** Create `internal/database/interbase_metadata_functions.go` and matching tests; modify `interbase_metadata_jobs.go`, `interbase_catalog.go`.

**Interfaces:** Produce `readMetadataFunctions(ctx context.Context, q schema.Queryer, width int) (MetadataPatch,error)`, pure `functionDescriptions(functions []schema.Function) ([]*FunctionDesc,error)`, and `(db *InterBaseDBRepository) MetadataPlan() MetadataPlan`.

- [ ] **Step 1: Add UDF parity tests and plan-shape tests.** Preserve return-argument-as-position semantics, including returning an input argument, missing character length, zero arguments, and unsupported type rendering. Query count is three for 1 and 100 UDFs. Assert plan parallelism is three, all-columns is omitted, and core/extended categories have no unrelated dependencies.

```go
plan := repo.MetadataPlan()
if plan.Parallelism != 3 { t.Fatal(plan.Parallelism) }
for _, job := range plan.Jobs {
    if job.Kind == MetadataColumnsAll { t.Fatal("duplicate InterBase columns") }
    if len(job.DependsOn) != 0 { t.Fatalf("unexpected dependency for %s", job.Kind) }
}
```

- [ ] **Step 2: Run** `go test ./internal/database -run 'TestInterBaseMetadata(Functions|Plan)' -count=1`; expect missing implementation.
- [ ] **Step 3: Implement two UDF reads and the job plan.** Adapt driver functionQueryTemplate/scanFunction and functionArgumentsQueryTemplate/raw argument scan. Remove the single-function predicate, join user functions, order by function/argument position, and preserve every type/mechanism/length field used by SQLType/ReturnType. Feed schema.Function into the shared descriptor mapper, preserving interBaseOptionalRendering.

Plan order: schemas, relations, current columns, procedures, primary keys, views,
indexes, foreign keys, functions, generators, domains, triggers. The scheduler
starts up to three in this order; it does not wait for a tier to finish. Schemas
is a zero-I/O local job. Wrap bulk readers with runMetadataRead. For
generators/domains/triggers, open a transaction and construct a fresh bound
repository with `snapshot: &interBaseCatalogSnapshot{catalog: schema.New(tx)}`;
its Describe method uses that tx and does not load bulk core data. Use a wrapper
variant with the exact contract below to avoid redundant width discovery for
driver-backed categories; define it next to runMetadataRead and test its
single-connection ownership. Do not call CatalogSnapshot.

```go
func (db *InterBaseDBRepository) runMetadataRepositoryRead(
    ctx context.Context,
    read func(context.Context, *InterBaseDBRepository) (MetadataPatch, error),
) (MetadataPatch, error)
```

This wrapper preserves SQLDialect, SourceSQLDialect, and DatabaseName on the
bound repository and applies the same cleanup/error rules as runMetadataRead.

- [ ] **Step 4: Run** `go test -race ./internal/database -count=1`; verify optional-interface guard tests and existing standalone accessors.
- [ ] **Step 5: Commit** `perf(interbase): publish a bounded set-based metadata job plan`.

### Task 6: Verify failure isolation, resource bounds, and native equivalence

**Files:** Create `internal/database/interbase_metadata_jobs_test.go` tests not already present and `internal/database/interbase_metadata_live_test.go` (native build tag); update `doc/develop.md` InterBase metadata section.

**Interfaces:** Consumes the complete plan and MetadataLoader, produces test evidence and documented statement-count budgets. No new runtime API.

- [ ] **Step 1: Add fault-injection and one-connection tests.** For each kind inject query, scan, iteration, and cleanup errors using the catalog fixture driver wrapper. Assert all other independent jobs settle successfully. With pool max open one and plan parallelism overridden to one in a test-only wrapper, loading must complete: this catches nested connection acquisition. With max open five and three metadata jobs gated, a fourth interactive read must obtain a connection. Add ordinary Go panic cleanup coverage.

```go
loader.Reset(1)
load, err := loader.Start(ctx, 1, repo)
if err != nil { t.Fatal(err) }
select { case <-load.Done: case <-ctx.Done(): t.Fatal(ctx.Err()) }
if loader.Snapshot().Degraded() { t.Fatal("healthy fixture degraded") }
```

- [ ] **Step 2: Run** `go test -race ./internal/database -run TestInterBaseMetadata -count=1`; fix any resource-lifetime failures before native measurement.
- [ ] **Step 3: Add an opt-in live read-only equivalence test.** Reuse interBaseLiveConfig and INTERBASE_DATABASE/USER/PASSWORD. Read a full new-loader snapshot and compare category counts/representative full descriptors against legacy methods on a small stable database; use a controlled fixture or maintenance-stable schema to avoid interpreting concurrent DDL as deterministic parity. Require both Dialect 1 and 3 runs. Never create/drop objects in shared databases. Record full query counts, including discovery, and db.Stats WaitCount/WaitDuration deltas.
- [ ] **Step 4: Run** `go test ./...`, `make test-race`, `CGO_ENABLED=1 go test -tags interbase ./...`, and `CGO_ENABLED=1 go build -tags interbase ./...`. Run the opt-in test with `CGO_ENABLED=1 go test -tags interbase ./internal/database -run TestInterBaseMetadataLive -count=1 -v` when environment credentials are configured. A skip is not native verification.
- [ ] **Step 5: Commit** `test(interbase): verify isolated metadata jobs and bounded catalog reads`.

## Handoff

Give Plan 3 the confirmed job list, exact per-category query budgets including
discovery, test/native results, and any unmeasured live cases. Server still uses
Worker until Plan 3 switches it over. The bulk loaders must preserve existing
DDL, long-name, nullability, and dialect results before that cutover.
