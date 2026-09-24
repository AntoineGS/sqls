# Progressive metadata loading and LSP readiness

## 1. Intent and scope

The user wants sqls to become useful sooner after launch, load metadata faster
(InterBase first), and continue loading independent objects when a loading job
fails. They approved progressive readiness and requested implementation plans
for a later handoff to smaller agents. This document records the design decisions
behind that handoff; implementation starts after review of the package.

Success means that database latency does not delay the LSP handshake, successful
metadata becomes usable without waiting for unrelated categories, and parallel
loading improves elapsed time without starving interactive database work.

Scope includes connection/bootstrap scheduling, metadata jobs, InterBase bulk
reads, immutable publication, consumer readiness, progress reporting, and
measurement. Persistent disk metadata, a general-purpose task framework, changing
query execution semantics, and making every LSP handler asynchronous are outside
this iteration. Existing on-demand DDL operations remain available; their latency
is measured separately from cache-only requests.

## 2. Evidence at planning baseline

Baseline: sqls commit `e8bf840`, Go version declared by `go.mod`: `1.25.7`.
`interbase-go` is a local replacement at `../interbase-go`; record its commit when
executing benchmarks. No new live performance measurement was made to write this
design.

- `internal/handler/handler.go`, `handleInitialize`: connection and primary cache
  construction precede the initialize response.
- `internal/database/worker.go`, `ReCache`: primary failure skips scheduling the
  remaining passes. The background loop uses uncancellable Background contexts.
- `internal/database/cache.go`, `generateCatalogCache`: seven categories execute
  serially, returning on the first error and losing earlier successes.
- Every cache pass invokes `CatalogSnapshot`; InterBase eagerly reads relations,
  columns, PKs, and FKs on every invocation. Current/all-schema columns are the
  same data for this driver.
- `../interbase-go/schema`: relation/view columns, procedure parameters, index
  segments, and UDF arguments are fetched per parent object. Concurrency alone
  does not remove those round trips.
- `diagnosticUniqueKeys` currently interprets `HasCatalog()` as completeness.
  Hover classification can otherwise mistake a not-yet-loaded view for a table.
- Existing immutable cache replacement and generation checks are worth keeping.
  The existing pool maximum is five connections.

## 3. Readiness contract

Three milestones are distinct:

1. **Protocol ready:** initialize replies with capabilities after local parameter
   validation/config capture, without database I/O. Bootstrap starts from the
   `initialized` notification. Local parsing, formatting, and keyword completion
   work while a connection is unavailable.
2. **Basic metadata ready:** relation names and usable current/all-schema columns
   have loaded. Tables may complete before column loading finishes.
3. **Metadata settled:** all supported jobs have reached ready, failed, blocked,
   or cancelled. Report degraded completion when any required job failed; do not
   label this state “fully loaded.” An empty successful category is ready.

Connection attachment and automatic InterBase dialect detection run in the
background. Until resolved, use the explicitly configured dialect when present;
otherwise use the existing InterBase default, suppress variant-sensitive static
diagnostics, then reanalyze open documents after resolution. The connection's
resolved attachment dialect and source dialect retain their distinct meanings.

## 4. Metadata data model and ownership

Introduce a new loader alongside the current Worker, test it independently, then
replace the server's Worker in Plan 3. Do not maintain two active loading paths.

`MetadataKind` identifies schemas, relations, current columns, all columns,
primary keys, foreign keys, views, procedures, generators, domains, functions,
indexes, and triggers. Primary keys are an independent InterBase job; other
drivers may continue returning key flags with their columns.

`MetadataState` values are pending, loading, ready, failed, blocked, cancelled,
and unsupported. A `MetadataStatus` records state, start/end timestamps, object
count, and an internal error. Errors are not serialized verbatim to clients.
There is no automatic retry in this iteration. Reconnecting/reselecting a
connection retries the load; a failed job is not silently replaced with an
expensive fallback.

`MetadataSnapshot` contains a connection generation, revision, immutable
`*DBCache`, and per-kind statuses. A snapshot is replaced under the loader mutex;
readers can retain it safely. Cache maps, descriptor slices, and descriptors must
not be mutated after publication. Status-only changes advance snapshot revision
but do not replace the cache pointer or trigger document analysis.

Every active connection transition increments the server generation, clears the visible
cache immediately, and cancels the previous metadata generation. This includes
refresh by reselecting the same connection: this version favors explicit fresh
state over stale-while-revalidate. Never retain database A's metadata while
database B connects. A failing new connection leaves an empty cache with failure
status, not a closed old connection masquerading as usable.

A queued switch is intent, not an active transition: wait for connMu off the read
loop before changing the active generation/config. A separate intent id makes
superseded attachment attempts obsolete without changing an executing query's
connection identity.

`HasCatalog` means catalog data is present, not that all categories succeeded.
Add `DBCache.MetadataReady(kinds ...MetadataKind) bool`; it is false for any
required category that is not ready. Production snapshots always carry explicit
states. Older fully-built cache generators and test fixtures are upgraded to
mark their successful categories ready before the loader is wired in.

## 5. Job execution and failure isolation

The exact shared Go contracts are in Plan 1. A job has a kind, explicit
dependencies, and a Run function returning a fragment of DBCache. A driver may
provide an optional MetadataPlan; generic repositories get jobs built from their
existing methods. Do not expand DBRepository's mandatory interface.

The scheduler is a bounded ready queue, not one goroutine per object and not a
fail-fast errgroup. It selects runnable jobs in stable priority order. A job
error or recovered ordinary Go panic records that job's failure; it never
cancels siblings. Failed prerequisites block only their declared dependents.
Unsupported categories are recorded without being scheduled.

Each job publishes a complete category atomically. Query/scan/iteration/close
failure discards that job's fragment, never earlier successful jobs. A malformed
object may fail its category; this version does not promise per-row recovery
from an unusable result stream. Optional unrenderable fields keep the existing
empty-field behavior. Native process crashes cannot be isolated by Go recovery.

Core InterBase categories do not depend on FK/PK success. Primary-key failure
leaves ColumnDesc.Key unknown (`""`), not falsely `"NO"`. If PKs arrive later,
copy column descriptors when enriching flags. If columns arrive later, merge
already-loaded PKs. This must work in either order without mutating old snapshots.

Default generic parallelism is one until driver support is verified. InterBase
parallelism is three; the loader's process-lifetime semaphore also caps all
superseded generations together at three active database jobs. InterBase jobs
hold at most one connection each. With the existing pool cap of five, metadata
alone cannot occupy the whole pool. This is headroom, not a reservation against
other interactive callers. Do not enlarge the pool to hide slow queries.

Reset/Stop cancels contexts and prevents all late publication. A slot stays owned
until the native call actually returns. Cancellation is best effort; a deadline
or goroutine wrapper cannot forcibly interrupt a C call. A cancelled generation
may become logically settled before its native calls drain. Tests distinguish
logical completion from resource drainage. No unbounded replacement workers.

## 6. InterBase query strategy

Use one read-only snapshot-isolation transaction per InterBase job, never share a
sql.Tx across parallel jobs. The driver maps default isolation to read committed;
explicit snapshot isolation is required for multi-query category consistency.
No job opens another connection while holding its transaction. Rollback closes
read-only jobs; propagate unexpected cleanup errors before publication. A
generation is a publication fence, not a database-wide point-in-time snapshot.
Each multi-query category is transaction-consistent; separate categories can
observe concurrent DDL at different moments. Consumers treat unresolved joins
conservatively and refresh resolves such differences.

Replace the cache's eager all-or-nothing CatalogSnapshot use with these jobs:

| Kind | Data queries | Notes |
| --- | --- | --- |
| schemas | 0 | one empty schema, current schema empty |
| relations | 1 | names only; tables and views |
| current columns | 1 | all user relation columns; independent of relation list |
| primary keys | 1 | ordered identity membership; separate enrichment |
| foreign keys | 1 | existing bulk ordered field pairing |
| views | 2 | view header/source and bulk view columns in the same transaction |
| procedures | 2 | headers/source and all user procedure parameters |
| indexes | 2 | headers and ordered segments |
| functions | 2 | headers and ordered arguments |
| generators/domains/triggers | existing set-based reads | bounded statement count |

All-columns is unsupported for this single-schema plan because current-columns
already covers the attachment. Generic drivers retain their all-schema job.
Do not rerun the core four data queries before every extended-category job.

Identifier-width discovery is counted separately from data queries, is bounded
per job, and preserves the driver's catalog-derived VARCHAR projection behavior.
New bulk loaders must not introduce the old 67-byte limit to categories whose
driver accessors already support wider identifiers. Retain exact identity,
catalog order, nullability, charset/collation, and source text; reuse the existing
schema types and descriptor rendering helpers. No DDL execution or source-text
reconstruction is part of these bulk reads.

## 7. Server lifecycle and consumer integration

Bootstrap/configuration handlers only capture intent and enqueue work. One
background connection coordinator owns pending connection attempts; repeated
configuration updates coalesce to the latest desired state. Explicit switch
commands await their own attachment result, not all metadata. Superseded attempts
cannot install a connection, emit a current error, or start a current load.

Preserve connMu's serialization of explicit switches against executing queries.
The coordinator may acquire connMu; the LSP read loop must not acquire it during
bootstrap/configuration. stateMu is never held across I/O. Snapshot configuration
deeply, including Params and InterBase TLS settings, rather than modifying shared
DBConfig.DBName in place. Stage a new connection locally; commit only if its
generation is still current, otherwise close it. Stop cancels before cleanup and
does not wait on connMu or a metadata job. Connection/snapshot cleanup is requested
once on a lifecycle-owned cleanup goroutine; potentially blocking native Close
must not run inline in a shutdown/exit handler. Log cleanup errors separately.

Use a single coherent helper to capture driver variant, generation and cache for
an editor operation. Every consumer of worker.Cache is migrated. Request work
that outlives its generation cannot populate DDL memo/snapshot files for the new
connection. Preserve existing document revision and diagnostics publication
fences.

Cache-only completion/hover/signatures never wait for jobs. Completion returns
the standard CompletionList and sets isIncomplete while metadata can still
change. Database commands invoked during attachment return a clear connecting
error rather than queueing behind bootstrap indefinitely; once attached, normal
query execution may proceed during metadata loading. ShowConnections and the
new metadata-status command do not acquire connMu.

Negative catalog conclusions require complete inputs. In particular:

- unique-key diagnostics require usable current/all-schema columns, views, and
  indexes ready;
- a known view may be used immediately, but table fallback in hover/navigation
  requires the views category to have succeeded;
- cached procedure lookup success is usable immediately; an absent procedure
  during loading/failure must not become proof it is an executable-only procedure;
  preserve server-prepare based validation and existing unknown-cache messaging.

Coalesce diagnostic refreshes through one capacity-one signal channel and one
consumer. Snapshot-derived diagnostic column/key maps are built once per cache
revision, shared immutably across documents, and reused on text changes. No new
goroutine-per-cache-update fan-out or permanent pointer-keyed map of old caches.

## 8. Status and measurements

Expose loading and degraded state through the pull-based metadata-status command
only in this iteration. The pinned JSON-RPC transport ignores context deadlines
while writing; even optional work-done progress or window/logMessage can block
all replies when the client stops reading. Do not emit metadata push notifications
or create progress tokens until transport writes can be bounded safely. Ordinary
server-local logs may report generation summaries without client payloads.

Add `sqls.showMetadataStatus` to workspace/executeCommand; return a JSON-compatible
object with generation, connection state, revision, settled/degraded flags, and
per-kind state/count/duration/error-code. Use stable error codes and a local log
correlation id rather than raw SQL, credentials, or arbitrary driver errors.

Measure protocol-ready, attach-ready, relation-ready, basic-ready, settled,
per-job queue/run duration and row/object counts, data/query counts, pool wait
statistics, and allocations. Use monotonic elapsed durations. Never label an
all-failed load as a fast successful load.

## 9. Verification and acceptance

- Deterministic gated tests prove initialize and a cache-only request finish
  while attachment/catalog I/O is blocked. A two-second watchdog is a deadlock
  guard, not the latency assertion.
- Inject failure and ordinary Go panic into every job kind; independent jobs
  still publish, true dependents are blocked, and old snapshots are unchanged.
- Run worker-limit, generation-switch, cancellation, and stale callback tests
  with the race detector. Exercise max-active counts across cancelled generations.
- Assert constant InterBase query counts on 1 and 100 parent objects for bulk
  views/procedures/indexes/functions, including identifier-discovery overhead.
- Keep all existing dialect 1/3, long-identifier, DDL and navigation regressions.
- Test no-config, late workspace config, failed connect, repeated initialized,
  clients without progress, progress-create failure, and shutdown mid-attach.
- Benchmark cold process startup and metadata refresh separately on a small and
  a large database. NRF01 is a candidate large workload, not a required name.
  Record database counts, network placement, versions, warm/cold interpretation,
  and p50/p95 over at least ten runs.
- Lab targets: protocol-ready p95 < 250 ms on the documented machine; cache-only
  completion p95 < 100 ms during loading; >= 30% lower large-database basic-ready
  and settled medians than the baseline. These are targets to validate, not
  measured claims or flaky CI wall-clock assertions. Report a miss explicitly.

## 10. Global constraints (copied into every plan)

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

## 11. Handoff decomposition

1. Metadata state, immutable merge, generic bounded loader, and readiness-aware
   consumers. The loader is tested in isolation until Plan 3 integrates it.
2. InterBase job plan and set-based loading, with query-count and equivalence
   tests. Depends on Plan 1 contracts.
3. Background connection lifecycle, progressive server integration, status,
   diagnostic coalescing, and removal of the old worker. Depends on Plans 1–2.
4. End-to-end regression/benchmark harness, acceptance measurements, and release
   documentation. Depends on Plan 3.

The package index identifies task-sized handoff units and shared-file ownership.
These plans change connected internals; execute in dependency order rather than
dispatching all plans simultaneously.
