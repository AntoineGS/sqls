## Start test databases

```sh
docker-compose up -d
```

## MySQL setup

```sh
wget https://downloads.mysql.com/docs/world.sql.gz
gzip world.sql.gz

# MySQL 5.6
mysql -u root -proot -h 127.0.0.1 -P 13305 < world.sql
# MySQL 5.7
mysql -u root -proot -h 127.0.0.1 -P 13306 < world.sql
# MySQL 8
mysql -u root -proot -h 127.0.0.1 -P 13307 < world.sql
rm world.sql
```

## Export keyword & function list

```sh
mysql -u root -proot -h 127.0.0.1 -P 13305 -D mysql < help_categories.sql > ./export/help_categories_mysql56.txt
mysql -u root -proot -h 127.0.0.1 -P 13306 -D mysql < help_categories.sql > ./export/help_categories_mysql57.txt
mysql -u root -proot -h 127.0.0.1 -P 13307 -D mysql < help_categories.sql > ./export/help_categories_mysql8.txt
# Export keyword list
mysql -u root -proot -h 127.0.0.1 -P 13305 -D mysql < help_keywords_mysql56.sql > ./export/help_keywords_mysql56.txt
mysql -u root -proot -h 127.0.0.1 -P 13306 -D mysql < help_keywords_mysql57.sql > ./export/help_keywords_mysql57.txt
mysql -u root -proot -h 127.0.0.1 -P 13307 -D mysql < help_keywords_mysql8.sql  > ./export/help_keywords_mysql8.txt
# Export function list
mysql -u root -proot -h 127.0.0.1 -P 13305 -D mysql < help_functions_mysql56.sql > ./export/help_functions_mysql56.txt
mysql -u root -proot -h 127.0.0.1 -P 13306 -D mysql < help_functions_mysql57.sql > ./export/help_functions_mysql57.txt
mysql -u root -proot -h 127.0.0.1 -P 13307 -D mysql < help_functions_mysql8.sql  > ./export/help_functions_mysql8.txt
```

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

- `connMu` serializes explicit connection switches against database commands.
  `executeQuery`, `showDatabases`, `showSchemas` and `showTables` take
  `connMu.RLock()` for the whole of their database work including rendering.
  The connection coordinator takes `connMu.Lock()` while committing a staged
  connection replacement. `handleInitialize` and
  `handleWorkspaceDidChangeConfiguration` only capture/enqueue desired state;
  neither waits on `connMu` or performs attachment I/O on the LSP read loop.
  `showConnections` does not acquire `connMu`; it renders a deep configuration
  snapshot captured under `stateMu`.
- `stateMu` guards *mutable `Server` fields*.

Because database commands take `connMu.RLock()` (shared) and explicit connection
switches take `connMu.Lock()` (exclusive), a switch waits until every in-flight
query has finished, and no query starts against a connection that is mid-swap.
The coordinator owns attachment attempts; superseded attempts cannot install a
connection or publish metadata into the current generation.

**Lock ordering: `connMu` before `stateMu`, never the reverse**, and `stateMu`
is never held across any I/O. The coordinator's `attachIntent` holds
`connMu.Lock()` while it advances the generation, opens the candidate
attachment, commits it and starts the metadata generation; it takes
`stateMu.Lock()` only for short pointer/state assignments. The replaced
attachment is transferred to the lifecycle cleanup queue and closed later on
the cleanup goroutine, not synchronously under `connMu`. Bootstrap and workspace
configuration are not inline lock holders.

**The accessor trap.** `getConfig`, `topConnection`, `getConnection`,
`parserDriver`, `newDBRepository` and `fileText` take `stateMu` *internally*.
A call site that calls one of these looks lock-free, but it is not, and the
hazard is invisible at the call site: never call any of them while already
holding `stateMu`. The concrete case that forced this rule: `topConnection`
takes `stateMu.RLock()` to read `initOptionDBConfig`, releases it, and only
then calls `getConfig`, which takes `stateMu.RLock()` again. Go's
`sync.RWMutex` explicitly disallows recursive read-locking on the same
goroutine — if a writer is queued between the two calls, the second `RLock`
blocks behind it while the first is still held, which deadlocks. A downstream
implementer who does not know these six functions take the lock will write an
inverted acquisition without realising it.

**Shutdown takes no `connMu`, but it does take `stateMu`.** `handleShutdown` and
`handleExit` delegate to `Server.Stop`. Stop cancels lifecycle work, snapshots
and clears `dbConn` under `stateMu`, then transfers the attachment to the
lifecycle cleanup goroutine. It does not wait for `connMu` or close a native
attachment inline on the request loop. "No `connMu`" is not "no lock"; database
pointer/state access still uses `stateMu`.

**The field audit.** Every field of `Server` is classified. Any new field must
be added here and classified, or it ships a race.

| Field(s) | Written/owned by | Read/used by | Treatment |
| --- | --- | --- | --- |
| `connMu`, `stateMu` | `NewServer` | handlers, coordinator, shutdown | Server lock order is `connMu` before `stateMu`; `stateMu` never spans I/O. See the lock rules above. |
| `diagnosticsPublishMu` | `NewServer` | diagnostic publisher and coordinator generation transition | Serializes diagnostic validation/send with generation changes; separate from `stateMu`. |
| `diagnosticCatalogMu`; `diagnosticCache`, `derivedCatalog` | `NewServer`; diagnostics consumer | diagnostic analysis | `diagnosticCatalogMu` guards the cache identity and its immutable derived catalog. |
| `diagnosticWorkMu`; `diagnosticDocuments`, `diagnosticAllOpen` | `NewServer`; queue/take helpers | diagnostics signal consumer | `diagnosticWorkMu` guards pending work; `diagnosticsWake` is only a coalesced wake, sent nonblocking. |
| `diagnosticAnalyzer` | test setup only | diagnostics analysis | Test injection is set before the consumer uses it; no production writer after construction. |
| `dbConn` | `attachIntent` and `Stop` | `newDBRepository`, `parserDriver`, `Stop` | `stateMu`; Stop detaches it and cleanup goroutine closes it asynchronously. No `reconnectionDB` writer remains. |
| `curDBCfg`, `curDBName`, `curConnectionIndex`, `activeConfigKey` | connection coordinator (`attachIntent`) | connection/config accessors and commands | `stateMu`; connection configs are deep-cloned on capture and commit. |
| `connectionState`, `metadataStartErr`, `connGeneration`, `ddlMemo` | connection coordinator; `Stop` for stopped state | readiness checks, status, editor snapshots, hover | `stateMu`; DDL memo is invalidated per generation and never locked across `ObjectDDL` I/O. |
| `WSCfg` | `handleWorkspaceDidChangeConfiguration` (inline) | `getConfig`, `topConnection`, `showConnections`, connection requests | `stateMu`; configuration is snapshotted before enqueueing connection work. |
| `initOptionDBConfig`, `initialized` | initialize handler (once) | configuration selection and lifecycle handlers | `stateMu`; write-once/transition state, read by coordinator-reachable paths. |
| `SpecificFileCfg`, `DefaultFileCfg` | `main.go` before `jsonrpc2.NewConn` | `getConfig` | Write-once-before-serving invariant; any later writer must take `stateMu`. |
| `lifecycleCtx`, `lifecycleCancel`, `stopOnce` | `NewServer`, `Stop` | coordinator, metadata jobs, diagnostics consumer | Context is concurrency-safe; cancellation is one-shot via `stopOnce`; no server lock is held during cancellation. |
| `coordinator` | `NewServer` | initialize/configuration/switch handlers, `Stop` | Pointer is immutable; the coordinator's own mutex protects desired/pending/active intents. |
| `metadata` | `NewServer` | coordinator, status command, editor snapshots | Pointer is immutable; `MetadataLoader` owns synchronization for generation snapshots/jobs. |
| `openConnection` | `NewServer`; test setup before use | connection coordinator | Production function is immutable after construction; tests replace it before starting requests. |
| `cleanupDone`, `cleanupQueue`, `cleanupFinal` | `NewServer`; Stop signals/queues final connection | cleanup goroutine; tests await `cleanupDone` | Queue transfers connection-close ownership to one cleanup consumer; coordinator completion triggers queue close, then final connection/snapshot cleanup. |
| `cleanupOnce` | none | none | Currently unused; it provides no synchronization guarantee. |
| `diagnosticsWake`, `diagnosticsDone` | `NewServer`; signal helper sends wake | diagnostics consumer; tests await completion | Capacity-one nonblocking wake; single consumer closes `diagnosticsDone` on exit. |
| `fileRevision`, `files` | document handlers (inline) | editor snapshots and handlers; query/diagnostic paths | `stateMu` on mutable file state; read text through the copy rule below. |
| `notificationConn` | `Server.Handle` | diagnostics signal consumer | `stateMu` for replacement and snapshot before diagnostic notification work. |
| `snapshots` | `NewServer` | definition handler, cleanup goroutine | Pointer is immutable; the store uses its own leaf mutex, including around filesystem operations. |
| `cancels` | `NewServer` | execute/cancel handlers | Pointer is immutable; `cancelRegistry` owns its mutex and is not nested with server locks. |

**The copy rule for `files`.** `updateFile` mutates `File.Text` through the
stored pointer, so holding `stateMu` only while looking the pointer up is not
enough. Read document text through `Server.fileText`, which copies the string
under the lock; never retain the `*File`.

**Cancellation.** `handleWorkspaceExecuteCommand` derives a cancellable context,
registers its `context.CancelFunc` under the request id, and deregisters on
return, via `defer cancel()` and `defer s.cancels.unregister(req.ID)` placed
immediately after registration — so both run on every exit path, including a
panic unwind, not just the normal return. `$/cancelRequest` looks the id up and
cancels; an unknown id is a no-op, because a cancellation that races the
response is normal. Cancellation is best effort: a statement that completed
before the native cancellation took effect is rendered normally with a note
saying so.

**`$/cancelRequest` delivery depends on the read loop being free.**
`internal/handler/dispatch.go`'s dispatcher special-cases exactly one method,
`workspace/executeCommand`, running it in its own goroutine; every other
request — `$/cancelRequest` included — is handled inline by the same call that
reads the next message off the wire (the vendored `jsonrpc2.Conn.readMessages`
calls `c.h.Handle` synchronously in a loop and does not read the next message
until `Handle` returns). An inline handler that blocks — for example on
`connMu.Lock()` — therefore stalls the read loop itself, not just its own
response: no further message, including a `$/cancelRequest` aimed at the very
query occupying `connMu`, can even be read until it returns.
Connection bootstrap and workspace configuration only enqueue intent, so they
do not block this read loop on attachment or metadata I/O. Explicit switch
commands are dispatched asynchronously and may wait for `connMu`; the explicit
`sqls.showMetadataStatus` command reads a snapshot without acquiring it. Keep
blocking connection work out of inline handlers, or cancellation delivery can
stall behind it.

**The cancel registry has no ordering relationship with `connMu`/`stateMu` —
and that is deliberate, so do not invent one.** `Server.cancels`
(`*cancelRegistry`) carries its own `sync.Mutex`, and it is never held
concurrently with either server lock by the same goroutine: `handleCancelRequest`
touches only `s.cancels`, and `handleWorkspaceExecuteCommand` touches the
registry only before and after `dispatchCommand`'s `connMu`/`stateMu` critical
sections, never during.

**The coordinator and loader are lifecycle owners, not request-local workers.**
The coordinator serializes desired connection intent; the single
`MetadataLoader` owns the current generation, category jobs and immutable cache
snapshots. No `Worker`/`ReCache` path exists in the server. Its callback only
queues a coalesced diagnostics signal and does so without holding loader or
server locks.

**Writing a test that actually demonstrates a race.** The race tests in
`internal/handler/concurrency_race_test.go` park an async call on a gate and then perform
the conflicting access. Do **not** wait for the gate by receiving on a channel
the async goroutine sent on: in every one of these cases the racy read happens
*before* the gated call, so the receive is a happens-before edge that orders the
read ahead of the write and the detector sees nothing. Such a test passes
identically with and without the lock it is supposed to be testing. Wait with a
short `time.Sleep` instead. Channel handshakes are correct in the tests that
assert liveness — that a second request is served while a command is in flight —
where there is no racing access to order.

## InterBase capability interfaces

InterBase-specific behaviour is added to sqls through **optional interfaces
asserted at the call site**, never through a driver check, wherever a driver
check can be avoided. A repository that later implements one of these gets the
feature for free, and shared code stays upstreamable.

The three rules:

1. **Capability, not driver name.** Handlers type-assert
   `database.DDLRepository` and `database.ExplainRepository`, and read catalog
   data through `DBCache.HasCatalog()` rather than by asserting
   `database.CatalogRepository`. `HasCatalog()` means catalog data is present;
   it does not mean every catalog category finished loading. Use
   `MetadataReady(kinds...)` or `ColumnsReady()` before treating an absent
   descriptor/key as conclusive. A descriptor that is present is usable even
   while unrelated categories remain pending; negative conclusions require
   readiness for the categories that could disprove them. `HasCatalog()` is
   false on every other driver and before any catalog data arrives.
2. **Driver identity only for parser- and lexer-shaped behaviour**, matching the
   existing `c.Driver == dialect.DatabaseDriverInterBase` checks in
   `internal/completer/candidates.go`. Completion candidate *generators* use it
   because the surrounding completer already does; hover and explain do not.
3. **Driver imports only under the `interbase` build tag**, following the
   `interbase_native.go` / `interbase_stub.go` pair. Nothing in the editor
   features imports `interbase-go`.

The bound-parameter work adds two more optional capabilities, in
`internal/database/parameter.go`, and follows the same rule — the handler
asserts the capability, never the driver name:

```go
type ParameterizedRepository interface {
	ExecParams(ctx context.Context, query string, args []any) (sql.Result, error)
	QueryParams(ctx context.Context, query string, args []any) (*sql.Rows, error)
}

type ParameterizedReadOnlyQuerier interface {
	QueryReadOnlyParams(ctx context.Context, query string, args []any) (*QueryResult, error)
}
```

`ParameterizedReadOnlyQuerier` is `ReadOnlyQuerier`'s bound counterpart, and
the pair has one rule worth stating: a repository that offers
`ReadOnlyQuerier` but **not** its parameterized counterpart is refused
(`errParameterizedReadOnlyUnsupported`) rather than silently downgraded to an
ordinary transaction — a read that was promised a read-only transaction does
not lose it because it acquired arguments. `boundReadFor`/`boundExecFor` in
`internal/handler/execute_command.go` are the single owners of these two
ladders: the batch preflight and the statement execution both call them, so
"preflight passed but statement two failed on a missing capability" — a
half-executed batch — cannot happen.

| File | Tag | Contents |
| --- | --- | --- |
| `internal/database/capability.go` | none | the capability interfaces, `ObjectKind`, the sentinels |
| `internal/database/parameter.go` | none | the two parameterized capabilities above |
| `internal/queryparams/` | none | the named-marker compiler and the wire value converter; imports no handler, database or lsp package |
| `internal/lsp/query_parameters.go` | none | the version-1 discovery/submission envelopes |
| `internal/handler/query_parameters.go` | none | `getQueryParameters`, identity hashing, submission preflight |
| `internal/database/catalog_doc.go` | none | markdown rendered from catalog descriptors, shared by the completer and the handler |
| `internal/database/capability_mock.go` | none | `MockCapabilityRepository`, deliberately distinct from `MockDBRepository`; seven `Describe*` hooks, `ObjectDDL`/`ExplainPlan` call recording, and `NewUnsupportedDDLError` |
| `internal/handler/explain.go` | none | the `explainQuery` command |
| `internal/handler/interbase_hover.go` | none | hover target resolution, the DDL appendix and its memo |
| `internal/completer/interbase_candidates.go` | none | procedure, view, generator and UDF candidates |

**Two rules the renderers must keep.** They are the reason the markdown lives in
one place instead of at each call site:

- A value the catalog does not have renders as **nothing**. Unknown parameter
  nullability renders neither "nullable" nor "unknown"; an empty type, return
  type or trigger event omits its line. No placeholder is ever emitted, and no
  object is hidden because one of its fields could not be rendered.
- No `CREATE` statement is ever synthesized. Executable DDL comes from
  `DDLRepository.ObjectDDL` or it is not shown, and its two sentinel errors are
  not interchangeable: `ErrUnsupportedDDL` means the catalog cannot reproduce a
  faithful definition, and the caller appends a one-line note carrying the
  `(object, name, feature)` triple from `UnsupportedDDLDetail`; `ErrObjectNotFound`
  means the cache named an object the catalog no longer has, and
  `interbase_hover.go` appends nothing at all rather than a stale-cache footnote
  the user cannot act on. What the catalog cannot reproduce as DDL is otherwise
  displayed as its own fields and its verbatim source text.

### InterBase metadata jobs

InterBase's `MetadataPlan` executes independent category jobs with a plan
parallelism of three; each catalog job owns one read-only snapshot transaction
and therefore at most one connection. Identifier-width discovery is part of
that job's budget, not hidden setup. The fixture-backed set-based readers have
these measured fixture-reader statement budgets (including discovery): views, indexes,
procedures, and functions each use **3 statements** (two data reads plus one
identifier-width read), independent of whether the fixture has 1 or 100 parent
objects. No Plan-level statement-count budgets are published for simple
categories or repository-accessor jobs: end-to-end category query accounting
waits for Plan 4 instrumentation. The fixture's reader tests can assert local
statement behavior, but do not establish a complete live per-category budget.
These counts are statements prepared/executed by the fixture catalog path, not
a claim about wire-level packets. `TestInterBaseMetadataLive` is opt-in and
read-only; missing per-dialect configuration is explicitly not native
equivalence verification.

### Progressive metadata lifecycle

The LSP server owns one connection coordinator and one `MetadataLoader`. The
coordinator stages attachment work off the request loop; after an attachment is
committed, the loader publishes independent metadata categories into immutable,
generation-tagged snapshots. There is no production `Worker` or `ReCache` path.
The synchronous `DBCacheGenerator` APIs remain available for standalone
tests/tools, not for server loading.

The useful readiness milestones are protocol-ready (the LSP can answer),
attach-ready (a database connection is committed), relation-ready (the schema
and relation inventory is available), basic-ready (the common relation/column
and key metadata needed by core editor features is available), and settled (all
supported category jobs reached a terminal outcome). These milestones are
observed, not a promise that every category succeeds. A category can be failed,
blocked by a failed prerequisite, unsupported, or cancelled while independent
categories still become ready. A failure never erases a successful sibling or
replaces the cache with a partially mutated object.

**Partial metadata is safe only with readiness-aware negative answers.** A
positive cached descriptor can be used as soon as it is published. Absence is
conclusive only after every category that could contain the descriptor is ready;
for example, key diagnostics require the relevant columns, views and indexes to
be ready. `HasCatalog()` means some catalog object exists, not that every
catalog category completed. Cache-only completion/hover/signature requests do
not wait for the loader, and completion may indicate that results are
incomplete while metadata can still change.

Loads do not retry automatically. A failure is visible in metadata status and
remains in that generation. Re-selecting the same connection with
`switchConnections` is the explicit refresh path: it performs a fresh
connection/generation transition and starts metadata again. InterBase's
`switchDatabase` remains a no-op for its single attachment and does not refresh
metadata. A new generation fences late results from the prior connection.
Cancellation is best effort: cancellation settles the logical generation, but
an in-flight native call may continue until the driver returns; its concurrency
permit remains occupied meanwhile and its late result cannot publish.

Use the `sqls.showMetadataStatus` execute command to pull a JSON-compatible
snapshot on demand. The outer object contains `generation`, `connectionState`,
`revision`, `settled`, and `degraded`, with optional `connectionErrorCode` and
`metadataErrorCode`. Each `categories` entry contains `kind`, `state`, `count`,
`durationMs`, and optional `errorCode`; codes are stable summaries, not raw
driver errors or SQL. No optional
metadata push notification or work-done progress is emitted: the pinned
JSON-RPC transport can block writes when the client stops reading, which could
otherwise block unrelated replies. Metadata changes coalesce into one internal
diagnostics signal instead.

**Lock audit.** `connMu` protects query-vs-explicit-switch serialization and is
acquired only by async command/coordinator paths; bootstrap/configuration and
the status command do not take it. `stateMu` protects published server fields
and never spans I/O. The coordinator mutex protects desired connection intent,
the loader mutex protects generation snapshots, and the diagnostics wake
channel has one consumer with nonblocking capacity-one sends. No callback does
database work while holding a loader or server lock. When changing this
lifecycle, update the field audit above and preserve the `connMu`-before-
`stateMu` ordering.

**The hover DDL round trip is the one place this plan puts inline I/O on the
request-handling loop.** `interbase_hover.go` calls `ObjectDDL` synchronously
from the hover handler, which is not among the async-dispatched methods (see
"Server concurrency invariants" above), bounded by `hoverDDLTimeout` (three
seconds); for that whole window the read loop cannot take the next request off
the wire. `memoisedObjectDDL` makes every later hover of the same object free,
but `Server.ddlMemo` is never trimmed — it keeps one entry per distinct
`(kind, name)` pair hovered for as long as the connection generation lives, and
is only replaced wholesale on the next successful reconnect.

**Testing against the driver registry.** `internal/database/interbase_common.go`
registers the InterBase factory in its `init`, and `RegisterFactory` panics on a
duplicate, so a test cannot install a capability-bearing repository under the
InterBase driver name. Features that need a repository therefore take it as a
parameter — `explainStatements(ctx, explainer, queries)` and
`(*Server).interBaseHover(ctx, repo, cache, …)` — and the tests call them
directly. Features that read only the `*DBCache`, which is completion and
signature help, need no server at all.

## Query parameter protocol

Named query parameters need a round trip the editor drives: the client asks
what the selection needs, prompts the user, and sends the answers back with the
execution request. The command is `getQueryParameters`, advertised next to the
existing commands in `ServerCapabilities.ExecuteCommandProvider.Commands`.
There is no experimental capability field: a client detects support by looking
for the command name, which is what the shipped Neovim adapter does.

**Version 1 is the only version.** `version` is checked on both legs — a
discovery result announces `1`, and a submission carrying anything else is
refused rather than interpreted under a contract its client never agreed to.
Add a field, bump the number.

```go
// internal/lsp/query_parameters.go
type QueryParameterContext struct {
	Version              int    `json:"version"`              // exactly 1
	ConnectionKey        string `json:"connectionKey"`
	ConnectionGeneration int    `json:"connectionGeneration"`
	QueryKey             string `json:"queryKey"`
	DocumentKey          string `json:"documentKey"`
}

type QueryParameterDiscovery struct {
	QueryParameterContext
	Supported  bool                    `json:"supported"`
	Parameters []queryparams.Parameter `json:"parameters"`
}

type QueryParameterSubmission struct {
	QueryParameterContext
	Values []queryparams.Value `json:"values"`
}
```

`QueryParameterSubmission` reaches the server as
`ExecuteCommandParams.ParameterValues` (`parameterValues`, omitempty) on the
ordinary `executeQuery` request. A non-nil `ParameterValues` is what selects
the bound path; a request without one is a legacy execution and is parsed from
the live document as before.

**Discovery is stateless and touches nothing.** `getQueryParameters` takes the
existing URI and `Range` fields, reads the document text, and runs
`queryparams.Compile` over it. No catalog, no database, no server-side session:
the server keeps nothing between discovery and submission, and the entered
values are forgotten as soon as the command returns. A connection that is not
InterBase — including no connection at all — answers `supported: false` with an
empty parameter list, which is the client's signal to use the legacy flow. A
supported selection with no markers still returns the full identity so the
client can make that choice with the same information.

**The three keys.** All are the hex SHA-256 of a JSON encoding, produced by
`hashJSON`:

- `connectionKey` hashes an explicit identity tuple — driver, alias, attachment
  string, host, port, path, database name, user, role, charset, effective
  connection database name (`database.NewInterBaseConnectionIdentity`). The
  whole config is deliberately **not** serialized: a password or TLS secret must
  never enter this hash. Database path case is preserved. A nil config or
  connection is an error, never a shared empty identity that would make two
  unrelated connections compare equal.
- `queryKey` hashes `[resolvedDialect, selectedSQL]`, so the same text under a
  different dialect is a different query.
- `documentKey` hashes the complete current document. It is submission
  validation only.

**What each key is for is not the same as what each key is keyed on.** The
server compares all five context fields on submission (`validateSubmission`)
and rejects a mismatch with a message naming *what* changed — the connection,
the selected SQL, or the document — having executed nothing. The client's
prefill cache is keyed on `connectionKey` and `queryKey` only:

- `documentKey` is excluded so that identical SQL in another buffer still
  reuses prefills, while an edit anywhere in the prompting document — including
  outside the selection — still rejects the submission. Rejecting an
  outside-selection edit is deliberate: it is a document version the user did
  not review.
- `connectionGeneration` is excluded so that switching away from a connection
  and back recovers the values entered before. It is still checked on
  submission, so values entered against the pre-reconnect connection cannot be
  bound to the post-reconnect one. Because the cache lives in the client
  process and is dropped when that client stops, a restarted server's initial
  generation cannot make an old prompt valid.

**The whole batch is preflighted before the first statement runs.**
`preflightBoundBatch` validates the context, compiles the entire selection,
binds every value, and resolves each statement's route and required capability
up front. A batch that fails there has executed nothing, which is what makes
re-running it after fixing one value safe. Routing is decided once and travels
with the statement in `boundStatement`, so execution never re-asks a
possibly-replaced worker cache and gets a different answer for a statement
already under way. A statement with no arguments inside a parameterized batch
keeps the legacy repository method and is held to no optional capability.

**The two refusals that keep raw marker text away from the driver.**
`refuseLegacyNamedParameters` rejects an InterBase `executeQuery` that carries
genuine named markers but no submission, so `:NAME` never reaches the driver as
SQL. It deliberately only claims text the compiler fully accepts: a PSQL body
whose `:V` is a local variable, DDL, or a bare `?` keeps the unparameterized
path it has always had, with the driver as the authority. `explainQueries` is
the counterpart on the Explain side — a marker-bearing selection is compiled to
positional SQL and prepared, never prompted for and never bound, because a plan
needs no values.

**Locking.** `getQueryParameters` and the bound execution both hold
`connMu.RLock` for the whole call, so no reconnect can land between the
connection a batch was validated against and the one it runs on.
`parameterSelection` copies the document text and every identity scalar out
from under `stateMu` and releases it before extracting the range, hashing or
compiling: the `*File` is never retained past the lock, and no mutable config
is hashed once the lock guarding its replacement is gone. No lock and no
transaction is held while the user answers prompts — the prompts happen in the
client, between two independent requests.
