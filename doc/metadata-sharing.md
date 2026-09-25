# Shared metadata registry in the language server

The LSP `Server` no longer owns a private `*database.MetadataLoader`. It
acquires metadata from its own `*metadata.Registry` (`s.registry`, one per
`Server` instance, constructed once in `NewServer`) the same way MDExplorer's
workbench services do, so two connections attached to the same database
target from the same server share one loader instance, one catalog cache,
and one on-demand detail store instead of each paying its own bulk-load and
DDL-fetch cost.

## Ownership: Registry, Lease, Subscription

- `s.registry *metadata.Registry` is created once per `Server` and closed
  (in the background, see "Stop" below) when the server shuts down.
- `s.metaLease *metadata.Lease` is the server's reference-counted handle on
  the track for its current connection target. Acquiring a lease for a
  target that is already tracked (another consumer, or this same server's
  prior connection to the identical target) reuses that track's loader and
  cache instead of starting a new one; `Lease.Incarnation()` identifies which
  underlying track a lease is bound to.
- `s.metaSub *metadata.Subscription` is a single subscription per host
  context, fed into the existing `s.diagnosticsWake`/`runDiagnosticSignals`
  scheduler so metadata change events reuse the diagnostics dispatch this
  server already had, rather than adding a second event loop.
- `s.metaKey`/`s.metaScope` are the `metadata.Key`/`metadata.Scope` the
  active lease was acquired with (see "Key/Scope and Factory dispatch"
  below); they are recomputed on every `attachIntent`/`bindMetadata` pass and
  compared to decide whether a reconnect targets the same track.

`attachIntent`'s `bindMetadata` step acquires the new lease **before**
releasing the old one, so a same-target reconnect (same `Key`/`Scope`) never
drops the track's reference count to zero and never re-triggers the
provider — it reuses the existing lease's incarnation. An **explicit**
reselect of the identical target (the user re-choosing the same connection
from the client) is treated differently: it calls `Lease.Refresh` on the
already-bound lease instead of a fresh `Registry.Acquire`, because refreshing
is not a reconnect and must still leave `metaBoundGeneration` bound to this
connection either way (Ruling R10, see below). Acquire failure for a new
target routes through `recordMetadataAcquireFailure`, which releases any
previously active lease. A `Refresh` failure leaves the existing
lease/incarnation bound (a transient refresh error does not orphan a working
connection) but `Lease.Refresh` marks that generation's snapshot
`StartFailed`, and `bindMetadata` still binds `metaBoundGeneration` to it, so
`metadataStatus()` reports it as `Degraded` (`metadataErrorCode:
"metadata_load_failed"`) rather than looking identical to metadata that was
never attempted for this connection.

Query execution (`executeQuery`/`explainQuery`) and `ExplainRepository`
always use `s.dbConn`, the server's own query connection — never the
metadata lease's connection or transaction. This is intentionally kept
separate (proven by `TestExecuteExplainDoNotUseMetadataReadTransaction`):
the shared metadata store is a read-only catalog cache and on-demand detail
source, not a transaction the query path can borrow.

## `Server.Stop()`

`Stop()` releases the lease and closes the subscription without blocking
LSP shutdown or the read loop on drainage, then closes the whole registry in
a background goroutine. A lingering, slow provider drain (for example, an
InterBase attachment still finishing a bulk load) must never delay the
client-visible shutdown response.

## Key/Scope and Factory dispatch (Ruling R8)

`metadataTargetFor` (`internal/handler/metadata_context.go`) derives the
`metadata.Key`/`metadata.Scope`/`metadata.Factory` triple for a connection
config, and dispatches on driver:

- **InterBase** reuses the real `ibmeta.Factory` from `metadata/interbase`
  (via `database.InterBaseMetadataTargetFor` and `ibmeta.KeyFor`/
  `ibmeta.ScopeFor`): a store-owned attachment independent of the query
  connection, with coordinated bulk category loading and on-demand detail
  (`DetailPlan`) for DDL text.
- **Every other driver** gets a minimal generic `metadata.Factory` whose
  `genericMetadataProvider.Plan()` builds a query repository via the existing
  `database.CreateRepositoryFromConnection`/`database.MetadataPlanFor` pair —
  the same repository and queries used before this change, no second
  connection pool. Its `DetailPlan()` always returns `metadata.ErrUnsupported`;
  generic drivers have no on-demand detail source, by design, not as a gap
  to close later.
  - `Key.URL` is a structured `proto|host|port|path|dbname` projection of the
    connection config when it has no raw `DataSourceName`; otherwise it falls
    back to the `DataSourceName` verbatim. Some non-InterBase DSNs embed
    credentials in that string — a pre-existing exposure this path inherits
    from `intentKey`/`activeConfigKey`, not a new one.
  - `Scope.Principal` is `cfg.User` when set, otherwise the constant
    placeholder `"sqls-generic-unauthenticated"` (`Registry.Acquire` rejects
    a zero `Scope`, and most generic drivers have no real separate-principal
    concept). `Scope.TextPolicy` is `string(cfg.Driver)`.
  - `Plan()` resolves the connection it builds that repository from
    **dynamically**, via `s.liveConnection()` read at Plan()-call time, never
    a connection captured once when the Factory ran. This is load-bearing,
    not cosmetic: `Lease.Refresh` (the explicit-reselect-of-the-same-target
    path above) re-runs `Plan()` on the SAME `genericMetadataProvider`
    instance a connection swap later. `attachIntent` always opens a new
    connection and enqueues the old one for closing (via
    `enqueueDetachedConnection`) *before* `bindMetadata` — and therefore
    `Refresh` — ever runs, so a `Plan()` that captured its connection once at
    Factory-creation time would run its catalog query against a connection
    the cleanup worker may already be closing. `TestGenericExplicitReselectUsesLiveConnectionNotClosedOne`
    (`internal/handler/metadata_lifecycle_test.go`) proves this by driving an
    explicit same-target reselect through a fake generic driver and asserting
    the refreshed catalog query ran against the second (fresh) connection,
    not the first. InterBase is unaffected: `ibmeta.Factory` owns an
    independent, store-owned attachment per A5, never tied to `s.dbConn`.

## Connection identity: `connGeneration`, `metaBoundGeneration`, lease generation

Three different counters track "has anything changed" at different scopes,
and code that fences on connection identity must use the right one:

- `s.connGeneration` (existing, `int`) advances on every reconnect. It ties
  per-connection artifacts (the hover DDL memo, the go-to-definition
  snapshot directory) to the connection that produced them.
- `s.metaBoundGeneration` (new, Ruling R9, generation-binding rule revised by
  Ruling R10) records the `connGeneration` value that was active at the
  moment `bindMetadata` last completed a bind for `s.metaLease` — set on
  BOTH `bindMetadata`'s Acquire branch AND its explicit-Refresh branch
  (regardless of whether the Refresh itself succeeded; see above), never by
  `Lease.Refresh` itself, which has no access to `Server` state. R9's
  original text said Refresh should leave this field untouched; R10
  supersedes that once wording: `attachIntent` unconditionally increments
  `connGeneration` on every intent it processes, including an explicit
  same-target reselect, so a Refresh that left `metaBoundGeneration` at its
  prior value would immediately fail the fence below until the next
  unrelated reconnect. Every fenced call site that used to compare against
  the old fused loader generation now compares `s.connGeneration ==
  s.metaBoundGeneration` instead, which stays true across a same-target
  refresh (successful or failed) and only goes false on an actual reconnect.
- The Lease's own `Snapshot().Generation` is a *different* counter, owned by
  the shared track: it starts at 1 and only advances on an explicit
  `Lease.Refresh`, and is identical across every consumer sharing that
  track (it has nothing to do with this server's own reconnects).
  `lsp.MetadataStatusResult.Generation` is repurposed to report this value.
  The additive `lsp.MetadataStatusResult.ConnectionGeneration` field carries
  `s.connGeneration` alongside it, so a client that needs the
  per-connection counter (not the shared track's) does not have to
  reconstruct it from other status fields.

## Hover/definition Detail migration scoping

Hover and go-to-definition's DDL lookups for InterBase route through the
active lease's `Detail` (facet `"ddl"`) instead of a private catalog round
trip, via the `leaseDDLRepository` decorator
(`internal/handler/interbase_metadata_lease_repository.go`) wired in at
`hover.go`'s `handleTextDocumentHover` and `definition.go`'s two call sites.
`wrapRepositoryWithLease` returns the repository unchanged unless the driver
is InterBase and both the repository and lease are non-nil — non-InterBase
drivers always keep the raw-repository path, matching their `DetailPlan()`
always returning `metadata.ErrUnsupported` (R8, above).

Within InterBase, only a fixed subset of object kinds are covered by the
lease's `"ddl"` facet (`leaseDDLObjectKinds`): procedure, domain, trigger,
index, generator, and view. Table and function DDL fall straight through to
the wrapped repository's own `ObjectDDL`, unchanged: table DDL needs
constraint/index/trigger facts no bulk category retains on its own, and
function DDL is permanently refused by the driver. Any `metadata.ErrUnsupported`
answer from the lease for a covered kind also falls back to the raw
repository, so a partially-capable provider degrades to the pre-existing
behavior instead of failing hover/definition outright.

`TableDescription` is forwarded to the wrapped repository as-is (it is not
part of `database.DBRepository`, so it is not otherwise promoted through the
decorator's embedding) to keep the table-description fallback used by
relation-definition unaffected by this decorator's presence.
