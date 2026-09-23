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

- `connMu` guards *connection lifetime*. `executeQuery`, `showDatabases`,
  `showSchemas` and `showTables` take `connMu.RLock()` for the whole of their
  database work including rendering. `switchDatabase`, `switchConnections`,
  `handleInitialize` and the reconnect branch of
  `handleWorkspaceDidChangeConfiguration` take `connMu.Lock()` across
  `reconnectionDB`. `showConnections` also takes `connMu.RLock()`, but it does
  no database work at all: it is in the read set solely because
  `newDBConnection` writes `connCfg.DBName` in place on a `*database.DBConfig`
  that `showConnections` reads field-by-field.
- `stateMu` guards *mutable `Server` fields*.

Because the read-set commands take `connMu.RLock()` (shared) and the
reconnecting commands take `connMu.Lock()` (exclusive), a command that switches
database or connection blocks until every in-flight query has finished, and no
query can start against a connection that is mid-swap: **a switch can never
interleave with a running query.** See §6.1bis of the design spec for why the
code serialises this way rather than reference-counting the old `*sql.DB`.

**Lock ordering: `connMu` before `stateMu`, never the reverse**, and `stateMu`
is never held across any I/O. `reconnectionDB` therefore performs `Close`,
`Open` and `ReCache` while holding only `connMu.Lock()`, taking `stateMu.Lock()`
only for the pointer assignments.

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

**Shutdown takes no `connMu`, but it does take `stateMu`.** `Server.Stop`,
`handleShutdown` and `handleExit` must not block on a runaway query, and
`sql.DB.Close` is documented as safe to call while queries are in flight — so
none of them takes `connMu`. All three still read `s.dbConn`, and `handleExit`
runs on the read loop while an async `switchDatabase` may be inside
`reconnectionDB` reassigning that pointer, so each snapshots it under
`stateMu.RLock()` and closes the local. "No `connMu`" is not "no lock".

**The field audit.** Every field of `Server` is classified. Any new field must
be added here and classified, or it ships a race.

| Field | Written by | Read by | Treatment |
| --- | --- | --- | --- |
| `files` | `openFile`/`updateFile`/`closeFile` (inline) | every handler; `executeQuery` (async) | `stateMu` on every access, plus the copy rule below |
| `dbConn` | `reconnectionDB` (async-reachable) | `newDBRepository`, `parserDriver`, `Server.Stop`, `handleShutdown`, `handleExit` | `stateMu` on every access, shutdown paths included — they take no `connMu` but still snapshot the pointer under `stateMu.RLock()` |
| `curDBCfg` | `newDBConnection` | `newDBRepository` | `stateMu` |
| `curDBName` | `switchDatabase` (async) | `newDBConnection` | `stateMu` |
| `curConnectionIndex` | `switchConnections` (async) | `newDBConnection` | `stateMu` |
| `WSCfg` | `handleWorkspaceDidChangeConfiguration` (inline) | `getConfig` ← `topConnection`/`showConnections`/`switchConnections` (async) | `stateMu` — a genuine inline-writer/async-reader race |
| `initOptionDBConfig` | `handleInitialize` (inline, once) | `topConnection` (async-reachable) | `stateMu` — write-once, but read from the async path |
| `SpecificFileCfg`, `DefaultFileCfg` | `main.go` before `jsonrpc2.NewConn` | `getConfig` | write-once-before-serving; an invariant, not a lock. Any future writer after serving begins must take `stateMu` |
| `connGeneration` | `reconnectionDB` | `connectionGeneration`, `memoisedObjectDDL` (inline, hover) | `stateMu` |
| `ddlMemo` | `reconnectionDB`, `memoisedObjectDDL` | `memoisedObjectDDL` (inline, hover) | `stateMu`, **never held across the `ObjectDDL` round trip** |
| `worker` | `NewServer` | everywhere | pointer never reassigned; the contents are guarded by the worker's own lock |
| `cancels` | `NewServer` | `handleWorkspaceExecuteCommand`, `handleCancelRequest` | pointer never reassigned; the registry has its own mutex |
| `connGeneration` | `reconnectionDB` | `snapshotContext` (inline, definition) | `stateMu` |
| `snapshots` | `NewServer` only | `interBaseDefinition` (inline), `Stop` | pointer never reassigned after construction; the store guards its own state with its own mutex, which **is** held across filesystem I/O — it is a leaf lock, unlike `stateMu` |

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
`handleWorkspaceDidChangeConfiguration` takes `connMu.Lock()` inline (across
`reconnectionDB`'s `Close`/`Open`/`ReCache`) whenever no connection exists yet,
and that acquisition can queue behind an async `switchConnections` or
`switchDatabase` already holding the write lock, or behind an in-flight query
holding the read lock; `handleInitialize` takes the same inline `connMu.Lock()`,
though in practice no query can be in flight that early. `workspace/executeCommand`
is the only method the dispatcher runs off the read loop, so it is the only
place a blocking wait is currently safe — a downstream implementer who adds a
`connMu` (or any other blocking) acquisition to another inline handler
reintroduces this stall.

**The cancel registry has no ordering relationship with `connMu`/`stateMu` —
and that is deliberate, so do not invent one.** `Server.cancels`
(`*cancelRegistry`) carries its own `sync.Mutex`, and it is never held
concurrently with either server lock by the same goroutine: `handleCancelRequest`
touches only `s.cancels`, and `handleWorkspaceExecuteCommand` touches the
registry only before and after `dispatchCommand`'s `connMu`/`stateMu` critical
sections, never during.

**The worker.** `Worker.dbRepo` is read by the worker goroutine and written by
`ReCache` on the handler goroutine. Both go through `repo()`/`setRepo()` under
`w.lock`; no `Server` lock can cover that pair.

**Writing a test that actually demonstrates a race.** The race tests in
`internal/handler/concurrency_race_test.go` and
`internal/database/worker_test.go` park an async call on a gate and then perform
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
