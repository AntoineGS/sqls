# InterBase Editor Features — Design Spec (sub-project 3)

Date: 2026-09-19
Status: design, not implemented
Scope owner: sub-project 3 of the InterBase integration (editor-facing features)
Depends on: sub-project 1 (driver `Diagnostics`/`Plan`), sub-project 2 (dialect
propagation, `schema`-backed repository, capability interfaces, extended `DBCache`)

## Purpose

sqls currently exposes InterBase as a minimally adapted relational database: a
catalog reader that produces `ColumnDesc` values for non-system relations, a
lexer/parser tweak for `$` identifiers and Dialect-1 double-quoted strings, and
the generic execute-query command. Everything an InterBase developer actually
spends the day on — stored procedures, external functions (UDFs), generators,
triggers, views, and the plan the engine picked — is invisible in the editor.

This sub-project makes those objects first-class in the six LSP surfaces sqls
already owns:

1. an **Explain SQL** code action backed by the driver's prepare-only `Plan`;
2. **completion** for procedures, procedure output parameters, external
   functions, generators, and views;
3. **signature help** for `EXECUTE PROCEDURE` and selectable-procedure calls;
4. **hover** backed by real `GenerateDDL()` output with honest degradation;
5. **go-to-definition** for procedures, views, and triggers whose source lives in
   the database rather than on disk;
6. **execution semantics** for the results pane: real LSP cancellation, a
   read-only transaction for read-only statements, typed rendering from
   `Rows.ColumnTypes`, and correct `EXECUTE PROCEDURE` routing.

Everything InterBase-specific stays behind capability interfaces and, where the
driver must be imported, behind the `interbase` build tag. Shared code is
extended additively so the safe parts remain upstreamable.

## Evidence and Constraints

Read before writing this spec; each item below is a fact from the code, not an
assumption.

### sqls, as it exists today

- `internal/handler/execute_command.go:34` advertises seven commands
  unconditionally, with no driver check. `handleWorkspaceExecuteCommand`
  (`:84`) dispatches on `params.Command` and returns
  `fmt.Errorf("unsupported command: %v")` for anything else. Results are
  rendered as a **plain string** by `s.query` (`:279`) through
  `tablewriter`, or `s.exec` (`:336`) as `"Query OK, %d row affected"`.
- `executeQuery` (`:113`) routes per statement with
  `database.QueryExecType(query, "")` — note the whole statement is passed as
  the `prefix` argument and `sqlstr` is empty; `QueryExecType`
  (`internal/database/query_type.go:246`) splits on whitespace and uses the
  first token. `"EXECUTE"` is in `execMap`
  (`query_type.go:177`), so **`EXECUTE PROCEDURE` currently takes the `Exec`
  path unconditionally**.
- `internal/database/scan_row.go:30` `ScanRows` scans into
  `[]interface{}` and stringifies with `reflect`; **a NULL and an empty string
  both render as `""`** (`sqlValToString` returns `""` for a nil pointer and for
  an empty `[]byte`/`string`). `rows.ColumnTypes()` is never called anywhere in
  the repository.
- `internal/handler/hover.go` is a pure function over
  `(text, params, *database.DBCache, driver)`. It has **no database access** —
  `hoverWithDriver` only reads the cache. `hoverType` already declares
  `hoverTypeView` and `hoverTypeFunction`, and both are set in
  `getHoverTypes`, but neither is ever consumed by `hoverContentFrom*`.
- `internal/handler/signature_help.go` serves exactly one case,
  `SignatureHelpTypeInsertValue`, and gets its active-parameter index from
  `ast.IdentifierList.GetIndex(pos)` via `parseutil.ExtractInsert`. That index
  helper is reusable for any parenthesised comma list.
- `internal/completer/completer.go:29` declares `CompletionTypeView` and sets it
  in eight context branches, but `Complete` has **no `CompletionTypeView`
  branch** — view candidates do not exist. Views only appear today because
  `interBaseRelationsQuery`
  (`internal/database/interbase_common.go:162`) selects every non-system row of
  `RDB$RELATIONS`, which includes views, and they arrive as "tables".
- `getSortTextPrefix` (`completer.go:212`) enumerates completion kinds
  explicitly and buckets everything it does not name into `"9999"` (the keyword
  bucket). Adding a new candidate kind requires adding a case here or the new
  candidates sort below keywords.
- InterBase case-insensitivity handling exists at `candidates.go:168`
  (`caseInsensitive := c.Driver == dialect.DatabaseDriverInterBase`) and
  `candidates.go:221` (`foreignKeysForTable` falls back to an `EqualFold` scan).
  `DBCache.Column` (`cache.go:173`) is already `EqualFold`; `ColumnDescs` keys on
  an uppercased `schema\ttable` string (`cache.go:186`).
- `parser/parser.go:263` `multiKeywordMap` groups `ORDER BY`, `GROUP BY`,
  `INSERT INTO`, `DELETE FROM` and the JOIN forms into `ast.MultiKeyword`.
  **`EXECUTE PROCEDURE` is not in that map**, so it parses as two separate
  keyword tokens and `parseutil.CheckSyntaxPosition`
  (`parser/parseutil/position.go:25`) classifies the position after it as
  `Unknown` — i.e. keywords only.
- `parser/parser.go:211` `parseFunctions` groups `NAME(` into an
  `ast.FunctionLiteral` **only when the parenthesis immediately follows with no
  whitespace** (`reader.PeekNodeIs(false, functionArgsMatcher)`).
- `internal/database/interbase_native.go` and `interbase_stub.go` establish the
  build-tag pattern: only the `//go:build interbase && cgo && linux && amd64`
  file may import `interbase-go`. `interbase_common.go` has no tag and
  therefore **cannot** reference driver types or error types.

### Corrections to the task description

Four things in the brief do not match the code. They change the design, so they
are called out here rather than silently worked around.

1. **`internal/handler/definition.go` does not resolve table/column
   identifiers.** It resolves *in-document aliases and subquery names only*
   (`parseutil.ExtractAliased`, then a string match on `alias.AliasedName`), and
   returns `nil, nil` when the document has no aliases at all. There is no
   existing database-backed definition path to extend; feature 5 adds the first
   one.
2. **sqls has no LSP cancellation today, and the deficiency is not in the
   database layer.** `main.go:125` builds `jsonrpc2.HandlerWithError(server.Handle)`
   and passes it to `jsonrpc2.NewConn` **without `jsonrpc2.AsyncHandler`**.
   `sourcegraph/jsonrpc2@v0.2.1` contains no `$/cancelRequest` support at all
   (zero occurrences in the module) and `Conn.readMessages` calls
   `c.h.Handle(ctx, c, m.request)` inline with the *connection* context. So:
   the context reaching `executeQuery` is never cancelled, and while a query
   runs the server cannot even read the next message. "Honour LSP cancellation
   end-to-end" therefore requires request dispatch and a cancel registry in
   sqls, not only a context handed to the driver. See §6.1.
3. **Trigger completion has no correct context.** Triggers are never named in
   DML; they are named only in `ALTER TRIGGER` / `DROP TRIGGER`, and
   `CheckSyntaxPosition` models no DDL position whatsoever (the README lists
   `CREATE TABLE` / `ALTER TABLE` completion as unimplemented). Triggers are
   therefore supported for hover and go-to-definition, and **excluded from
   completion**. See §10.
4. **Exceeding the driver's 64 MiB BLOB limit is a hard error, not a
   truncation.** `native.c:8992` returns
   `"BLOB result exceeds the materialization limit"` from
   `ib_append_blob_data` when `IB_MAX_BLOB_BUFFER` (`native.c:21`,
   `64U * 1024U * 1024U`) would be exceeded. The fetch fails; there is no
   partial value. The results pane story is about *reporting a failed fetch*,
   not about rendering a clipped BLOB.

### Driver facts this spec relies on

From `../interbase-go/README.md` and `../interbase-go/schema/README.md`:

- `Plan` only prepares. "A valid DML statement may return an empty plan, which
  never means that the DML executed." Prepared statements are dropped on every
  path.
- DSQL cancellation is **best effort, not a hard deadline**. "The executing
  native result is authoritative": a prepare/execute/fetch that completed stays
  completed. A canceled mutating operation returns `CancellationError` when the
  transaction state is confirmed, and `UncertainOutcomeError` when it is not —
  and "canceled writes must not be blindly retried". Cancellation never becomes
  `driver.ErrBadConn`, and never completes a caller-owned transaction. Observed
  latency on the tested stack ranged from ~101 ms to ~9.2–10 s; a stalled call
  can outlive its context.
- `EXECUTE PROCEDURE` goes through `QueryContext`/`QueryRowContext` when the
  procedure returns output (**exactly one row**), and through `ExecContext` when
  it does not. **`ExecContext` rejects output-producing procedures.** An
  implicit procedure query *commits its write transaction* after EOF or early
  close and rolls back on failure — i.e. `EXECUTE PROCEDURE` is a write path
  even when it returns rows.
- Ordinary SELECT cursors borrow the connection's explicit transaction or own a
  read-committed read-only implicit one.
- `Rows.ColumnTypes` metadata is an immutable execution snapshot;
  "properties unavailable from the result are reported unknown". Concretely
  (`native.go:1325` `columnMetadataFromNative`), `DatabaseTypeName` is one of
  `CHAR`, `VARCHAR`, `SMALLINT`, `INTEGER`, `BIGINT`, `NUMERIC`, `DECIMAL`,
  `FLOAT`, `DOUBLE PRECISION`, `DATE`, `TIME`, `TIMESTAMP`, `BOOLEAN`, `BLOB`,
  `ARRAY`; scaled integers have `ScanType` `string` (exact decimal text); BLOB
  subtype 1 scans as `string`, other subtypes as `[]byte`.
- `schema.GenerateDDL()` returns an error wrapping `schema.ErrUnsupportedDDL`
  for `Function`, `DatabaseFile`, `Shadow`, computed columns, and **procedure
  parameters whose declaration nullability is unknown** — which
  `schema/README.md` says is the normal case, since the reference-compatible
  `RDB$PROCEDURE_PARAMETERS` projection has no declaration nullability flag and
  only a non-nullable domain can prove non-nullability. The error is an
  `*UnsupportedDDLError{Object, Name, Feature}`.
- `Procedure.Source`, `Trigger.Source`, and `Relation.ViewSource` are preserved
  verbatim `sql.NullString` catalog text, untrimmed, and are available even when
  `GenerateDDL` refuses.
- No named parameters exist; DSQL parameters are positional `?` only.

## Dependencies on sub-project 2

This is the reconciliation section. Every cross-spec dependency is listed here
with the name this spec assumes; nothing below is assumed silently elsewhere in
the document. The coordinator should align names before planning. Where a
dependency cannot be met as stated, the stated fallback keeps sub-project 3
shippable.

All names are in package `github.com/sqls-server/sqls/internal/database`.

### D1 — object kind enum

```go
type ObjectKind string

const (
    ObjectKindTable     ObjectKind = "table"
    ObjectKindView      ObjectKind = "view"
    ObjectKindProcedure ObjectKind = "procedure"
    ObjectKindTrigger   ObjectKind = "trigger"
    ObjectKindGenerator ObjectKind = "generator"
    ObjectKindFunction  ObjectKind = "function" // external UDF
)
```

Used by `ObjectDDL` (D6), hover (§4), and definition (§5). If sub-project 2
prefers per-kind methods instead of an enum, sub-project 3 adapts with a
six-case switch; the enum is a convenience, not a requirement.

### D2 — catalog capability interfaces on the repository

Optional interfaces that `InterBaseDBRepository` implements and that callers
reach by type assertion on `database.DBRepository`:

```go
type ProcedureDescriber interface {
    Procedures(ctx context.Context) ([]*ProcedureDesc, error)
}
type ViewDescriber interface {
    Views(ctx context.Context) ([]*ViewDesc, error)
}
type TriggerDescriber interface {
    Triggers(ctx context.Context) ([]*TriggerDesc, error)
}
type GeneratorDescriber interface {
    Generators(ctx context.Context) ([]*GeneratorDesc, error)
}
type ExternalFunctionDescriber interface {
    ExternalFunctions(ctx context.Context) ([]*FunctionDesc, error)
}
```

Sub-project 3 does **not** need the domain, index, or constraint capabilities;
if they exist they are simply unused here.

### D3 — descriptor fields required

Only the fields listed are required. Extra fields are welcome and ignored.

`ProcedureDesc`:

| Field | Type | Used by |
| --- | --- | --- |
| `Name` | `string` | completion label, hover, definition, signature label |
| `Description` | `sql.NullString` | hover, completion documentation |
| `Source` | `sql.NullString` | definition fallback, hover fallback (verbatim PSQL) |
| `InputParameters` | `[]*ProcedureParameterDesc` | signature help, hover |
| `OutputParameters` | `[]*ProcedureParameterDesc` | selectable-procedure columns, exec routing |

`ProcedureParameterDesc`:

| Field | Type | Used by |
| --- | --- | --- |
| `Name` | `string` | parameter label |
| `Position` | `int` | ordering, active-parameter match |
| `Direction` | `ParameterDirection` (`"input"` / `"output"`) | direction text |
| `Type` | `string` | rendered type, e.g. `VARCHAR(40)`, `NUMERIC(18, 2)` |
| `Nullable` | `sql.NullBool` | **must preserve unknown**; see §3 |
| `Description` | `sql.NullString` | parameter documentation |

`ViewDesc`: `Name string`, `Source sql.NullString`, `Columns []*ColumnDesc`.

`TriggerDesc`: `Name string`, `RelationName sql.NullString`,
`Source sql.NullString`, `Event string` (e.g. `BEFORE INSERT`),
`Active sql.NullBool`, `Sequence sql.NullInt64`.

`GeneratorDesc`: `Name string`, `Description sql.NullString`.

`FunctionDesc` (external UDF): `Name string`, `ModuleName sql.NullString`,
`EntryPoint sql.NullString`, `ReturnPosition sql.NullInt64`,
`Arguments []*FunctionArgumentDesc` where `FunctionArgumentDesc` has
`Position int` and `Type string`.

Requirement on `Type`: it must be a **rendered declaration string** produced by
sub-project 2 from the resolved `RDB$FIELDS` attributes, not raw catalog
integers. Sub-project 3 renders no types itself.

### D4 — extended `DBCache` accessors

```go
func (dc *DBCache) SortedProcedures() []string
func (dc *DBCache) Procedure(name string) (*ProcedureDesc, bool)
func (dc *DBCache) SortedViews() []string
func (dc *DBCache) View(name string) (*ViewDesc, bool)
func (dc *DBCache) SortedTriggers() []string
func (dc *DBCache) Trigger(name string) (*TriggerDesc, bool)
func (dc *DBCache) SortedGenerators() []string
func (dc *DBCache) Generator(name string) (*GeneratorDesc, bool)
func (dc *DBCache) SortedExternalFunctions() []string
func (dc *DBCache) ExternalFunction(name string) (*FunctionDesc, bool)
```

Requirements:

- **Lookups must be case-insensitive**, keyed the way `columnDatabaseKey`
  already does it (`strings.ToUpper`). InterBase catalog names are uppercase;
  users type lowercase. Without this, every feature here silently misses.
- The singular accessors return `(nil, false)` for unknown names; they never
  return a non-nil descriptor with zero fields.
- `SortedViews()` **must be disjoint from `SortedTables()`** — i.e. the rebuilt
  repository's `SchemaTables` returns base tables only, and views arrive through
  the view cache. *Fallback if not met:* §2 de-duplicates view candidates
  against `SortedTables()` with an `EqualFold` check, at the cost of views
  showing the `table` detail string in the FROM position.

### D5 — unsupported-DDL sentinel re-exported by sqls

Because `internal/database/interbase_common.go` carries no build tag, it cannot
import `interbase-go`. Sub-project 2 must therefore re-export the sentinel and
its detail, so untagged handler code can classify the failure:

```go
var ErrUnsupportedDDL = errors.New("database: unsupported DDL")

// UnsupportedDDLDetail reports the object, name and blocking feature of an
// unsupported-DDL error, mapped from *schema.UnsupportedDDLError.
func UnsupportedDDLDetail(err error) (object, name, feature string, ok bool)
```

`ObjectDDL` must return an error satisfying
`errors.Is(err, database.ErrUnsupportedDDL)` whenever the underlying
`schema.GenerateDDL` failed with `schema.ErrUnsupportedDDL`. Hover (§4) and
definition (§5) branch on exactly this; they never string-match error text.

### D6 — `ObjectDDL`

```go
type DDLDescriber interface {
    ObjectDDL(ctx context.Context, kind ObjectKind, name string) (string, error)
}
```

Requirements: read-only; name matching is case-insensitive in the same sense as
D4; returns `("", nil)` — empty string, nil error — for an object that does not
exist, so callers can distinguish "no such object" from "cannot render DDL";
honours `ctx`.

### D7 — `ExplainPlan`

```go
type PlanExplainer interface {
    ExplainPlan(ctx context.Context, query string) (string, error)
}
```

Requirements, all load-bearing for §1 and for the user-facing safety claim:

- backed by the driver's `Plan`, which **prepares without executing**;
- acquires and releases its own `*sql.Conn` from the pool (the repository holds
  a `*sql.DB`; the driver's `Plan` takes a `*sql.Conn`), and does not leave a
  transaction open;
- propagates `ctx` so §6.1 cancellation reaches it;
- returns `("", nil)` for a statement that prepares successfully with no plan
  text, and never substitutes a placeholder string for an empty plan.

### D8 — mock repository for capability tests

Sub-project 2 is expected to add a mock that implements D2/D6/D7. Sub-project 3
assumes:

```go
type MockInterBaseDBRepository struct {
    MockDBRepository
    MockProcedures        func(context.Context) ([]*ProcedureDesc, error)
    MockViews             func(context.Context) ([]*ViewDesc, error)
    MockTriggers          func(context.Context) ([]*TriggerDesc, error)
    MockGenerators        func(context.Context) ([]*GeneratorDesc, error)
    MockExternalFunctions func(context.Context) ([]*FunctionDesc, error)
    MockObjectDDL         func(context.Context, ObjectKind, string) (string, error)
    MockExplainPlan       func(context.Context, string) (string, error)
}
```

It must be a **separate type from `MockDBRepository`**, not extra fields on it:
if `MockDBRepository` implemented the capability interfaces, every existing
handler test would start satisfying the type assertions and panic on the nil
func fields. If sub-project 2 does not provide this, sub-project 3 adds it in
`internal/database/interbase_mock.go`.

### D9 — things sub-project 3 explicitly does *not* need

Stated so the peers do not build them on our account: no dialect value on the
connection beyond the existing `s.parserDriver()` driver identity (all
dialect-sensitive rendering happens in the driver or in sub-project 2's `Type`
strings); no domain/index/constraint capabilities; no `Diagnostics` surface in
the editor (it belongs to a status command, not to these six features).

## Scope

### Included

- `explainQuery` command + "Explain SQL" code action, InterBase-only behavior,
  generic error elsewhere.
- Completion candidates for procedures, selectable procedures in `FROM`,
  procedure output parameters as columns of a selectable procedure, external
  functions, generators inside `GEN_ID(`, and views.
- Signature help for `EXECUTE PROCEDURE NAME(...)` and selectable-procedure
  calls `NAME(...)`.
- Hover: append real DDL to the existing summary for tables, views, procedures,
  triggers and generators; degrade to summary + one note line on unsupported
  DDL.
- Go-to-definition for procedures, views and triggers via a read-only source
  snapshot materialised as a real file, returned as a `file://` location.
- Execution: asynchronous dispatch of `workspace/executeCommand`,
  `$/cancelRequest` support, read-only transaction for read-only statements,
  typed rendering from `ColumnTypes`, `EXECUTE PROCEDURE` routing by output
  arity, and honest cancellation/BLOB failure reporting.
- Two shared, driver-neutral additions: `database.ScanRowsWithTypes` and
  `database.ReadOnlyQuerier` (see §6.2, §6.3).

### Excluded

- **Services Manager admin commands** (backup, restore, validate, sweep,
  statistics, logs) — ruled out project-wide; the user has MCP servers for this
  and an LSP is a poor host for long-running admin operations.
- Trigger, domain, role, index, privilege, and shadow **completion** — no SQL
  context in sqls's parser names them (correction 3).
- Procedure **input parameter name** completion — DSQL has no named parameters
  (driver "Known Limits"), so a parameter name is never valid text in a
  statement. They appear in signature help and hover only.
- `NEXT VALUE FOR <generator>` completion — Dialect-3 syntax needing another
  multi-keyword group; `GEN_ID(` covers the common Dialect-1 and Dialect-3 case.
- Column-level or line-level navigation **into** procedure/trigger bodies, and
  find-references across database-resident source. See §5 for why.
- Rewriting table hover as `CREATE TABLE` DDL — the existing markdown column
  table is more useful and existing tests assert it; DDL is *appended*, not
  substituted.
- Diagnostics/`textDocument/publishDiagnostics` from prepare failures. It is
  tempting (prepare is cheap and would underline bad SQL) and deliberately not
  in this sub-project: it changes the "sqls never touches your database while
  you type" contract and deserves its own decision.
- Any new configuration keys. Every feature here is automatic when the driver is
  InterBase and the capability is present.

## Plan decomposition

This spec is delivered as four sequenced plans. The ordering inverts an earlier
draft of this document, which proposed the concurrency work as a *fallback* to
be deferred if it proved too large. That was wrong, for three reasons that hold
up against the code:

- **§6.1 is the only part of this spec with zero dependency on sub-project 2.**
  It touches `main.go`, `internal/handler/handler.go`,
  `internal/database/worker.go` and CI, and needs no descriptor, no capability
  interface, and no cache accessor. It can therefore be built while sub-project
  2 is still in flight, which is exactly what a first plan should do.
- **Without it, §6.5 is dead code.** The context reaching the driver is never
  cancelled today (correction 2), so `CancellationError` and
  `UncertainOutcomeError` cannot arise from an editor cancellation at all, and
  the cancelled/uncertain rendering would be testable only against synthetic
  errors — a rendering path no user could reach.
- **Retrofitting locks is more expensive than starting with them.** §4's hover
  memo is specified as living under `stateMu`, and §5's snapshot store keeps a
  generation counter there. Building either before the mutex exists means
  touching both again.

Two refinements to the split as proposed by review, both dependency
corrections rather than disagreements:

1. Plan 4 depends on **D4 as well as D1, D5 and D6.** Resolving the identifier
   under the cursor to a procedure, view or trigger is a `DBCache` lookup
   (§5, "Resolution order"); without D4 there is nothing to resolve against.
2. Plan 2 is **partially** dependent on sub-project 2, not wholly independent of
   it: §6.2 (read-only transaction) and §6.3 (`ScanRowsWithTypes`) need nothing
   from D1–D8 and can start immediately after Plan 1. Only §6.4 needs D4, so if
   D4 slips, Plan 2 ships its first two thirds and §6.4 moves to Plan 3.

### Plan 1 — Server concurrency and cancellation

**Contents:** §6.1a selective async dispatch; §6.1b `$/cancelRequest` registry;
§6.1c `stateMu` with the complete field audit and the `files` copy rule;
§6.1bis `connMu` command-versus-command policy; §6.1d the `Worker.dbRepo` lock
fix; §6.1e adding `-race` to `.github/workflows/test.yaml` and a `make
test-race` target; §6.5 `ClassifyFailure` with its tagged/untagged file pair.

**Depends on:** nothing. Not sub-project 2, and only sub-project 1's already
existing driver error types for §6.5.

**Delivers working software:** a language server that keeps answering hover and
completion while a query runs, that stops a runaway query when the editor asks,
that reports a cancelled write as cancelled-or-uncertain in the results pane,
and that is verified by the race detector in CI for the first time.

**Acceptance:** `go test -race ./...` green in CI; `TestExecuteQueryHonoursCancelRequest`,
`TestSwitchConnectionWaitsForInFlightQuery` and
`TestWorkerReCacheIsRaceFreeUnderConcurrentUpdates` passing.

### Plan 2 — Results-pane semantics

**Contents:** §6.2 `ReadOnlyQuerier` and the read-only transaction; §6.3
`ScanRowsWithTypes`, `RenderOptions`, `QueryResult` and the partial-result
contract, including `s.query` rendering partial rows; §6.4 `EXECUTE PROCEDURE`
routing by output arity.

**Depends on:** Plan 1 (it renders the cancellation outcomes Plan 1 classifies);
D4 for §6.4 only.

**Delivers working software:** SELECTs run in an explicit read-only
transaction; results distinguish `NULL` from the empty string, render exact
scaled decimals, and cap huge cells; a fetch that dies on an oversized BLOB
shows the rows that preceded it instead of nothing; and `EXECUTE PROCEDURE`
stops being routed unconditionally to `Exec`.

### Plan 3 — Catalog-backed editor surfaces

**Contents:** features 1–4 — Explain code action and command, completion
candidates and the `EXECUTE PROCEDURE` parser change, signature help, hover DDL
augmentation with its memo.

**Depends on:** Plan 1 (hover memo under `stateMu`); D1–D8.

**Delivers working software:** the four surfaces a developer touches every
minute — explain, complete, signature, hover — become InterBase-aware.

### Plan 4 — Go-to-definition snapshots

**Contents:** feature 5 in full — resolution, the snapshot store, banner and
body selection, range computation, pruning, and the `Server.Stop` restructure.

**Depends on:** Plan 1 (store generation under `stateMu`, `Stop` restructure);
D1, D4, D5, D6.

**Delivers working software:** jumping to a procedure, view or trigger opens its
real source.

**Separate for a reason:** it introduces the server's first writes to the user's
filesystem, a directory lifecycle and pruning subsystem, and a security posture
(what is written, where, with what permissions, for how long) that nothing else
in this spec touches. It is also the feature most likely to be cut on review,
and keeping it last means cutting it costs nothing already built.

## Architecture

### 0. Shared shape

Three placement rules keep the fork upstreamable:

1. **Capability, not driver check, wherever possible.** Handlers type-assert
   `database.PlanExplainer`, `database.DDLDescriber`, etc. A non-InterBase
   driver that later implements one gets the feature for free.
2. **Driver identity only for parser/lexer-shaped behavior**, matching the
   existing `c.Driver == dialect.DatabaseDriverInterBase` checks at
   `candidates.go:168`/`:221`.
3. **Driver imports only under the build tag.** New tagged/untagged file pairs
   follow `interbase_native.go` / `interbase_stub.go`. Exactly one new pair is
   needed, for error classification (§6.5).

New files:

| File | Tag | Contents |
| --- | --- | --- |
| `internal/handler/explain.go` | none | `explainQuery` command |
| `internal/handler/interbase_hover.go` | none | DDL hover augmentation + memo |
| `internal/handler/interbase_definition.go` | none | source snapshot store |
| `internal/handler/dispatch.go` | none | selective async handler + cancel registry |
| `internal/completer/interbase_candidates.go` | none | new candidate generators |
| `internal/database/result.go` | none | `QueryResult`, `ColumnMeta`, `ScanRowsWithTypes` |
| `internal/database/interbase_failure_native.go` | `interbase,cgo,linux,amd64` | cancellation classification |
| `internal/database/interbase_failure_stub.go` | inverse | classification no-op |

Modified existing files, called out because three of them are driver-neutral
shared code and one is CI: `main.go` (wrap the handler with the dispatcher),
`internal/handler/handler.go` (`stateMu`, `connMu`, `$/cancelRequest`, `Stop`),
`internal/database/worker.go` (`dbRepo` accessors under `w.lock`),
`parser/parser.go` and `parser/parseutil/position.go` (the `EXECUTE PROCEDURE`
group and syntax position), `internal/completer/completer.go`
(`getSortTextPrefix` cases and new `Complete` branches),
`.github/workflows/test.yaml` and `Makefile` (`-race`).

### 1. Explain SQL code action

**Command.** `CommandExplainQuery = "explainQuery"` next to the existing
constants; a `{Title: "Explain SQL", Command: CommandExplainQuery, Arguments:
[]interface{}{params.TextDocument.URI}}` entry in
`handleTextDocumentCodeAction`, in the same unconditional list as "Execute
Query"; a `case CommandExplainQuery: return s.explainQuery(ctx, params)` in
`handleWorkspaceExecuteCommand`. *Decision:* advertise unconditionally rather
than filtering by driver, because the existing handler advertises every command
unconditionally and a client that caches code actions across connection switches
would otherwise show a stale list.

**Argument handling** mirrors `executeQuery`: `Arguments[0]` is the document
URI; `params.Range`, when present, narrows the text via `extractRangeText`;
statements are split with `getStatementsWithDriver(text, s.parserDriver())`.
`-show-vertical` is not accepted — a plan is not a table.

**Capability.** `repo, err := s.newDBRepository(ctx)`, then
`explainer, ok := repo.(database.PlanExplainer)`. When `!ok`, return
`fmt.Errorf("explain is not supported by the %s driver", repo.Driver())`.

**Statement admission.** For each statement, `database.QueryExecType(query, "")`
gives a type string. Admitted: anything the map classifies as a query (`SELECT`,
`WITH`, `VALUES`, …) plus `INSERT`, `UPDATE`, `DELETE`, `EXECUTE`. Everything
else — DDL, transaction control, `SET`, … — is refused with
`"Explain supports SELECT, INSERT, UPDATE, DELETE and EXECUTE PROCEDURE statements; got <TYPE>."`
*Decision:* refusing DDL costs one condition and avoids handing the user an
always-empty plan for `CREATE TABLE`, which reads like a bug.

**Rendering.** A plain string, like every other command. For one statement:

```
PLAN JOIN (CITY NATURAL, COUNTRY INDEX (RDB$PRIMARY7))
```

For several, each is preceded by `-- statement N` and a blank line separates
them. A non-query statement is preceded by a one-line banner:

```
-- statement 2 (prepared only; nothing was inserted, updated or deleted)
```

*Decision:* the banner is rendered for the user, not just documented, because
"explain my UPDATE" is exactly the moment a user needs to be told nothing ran.

**Empty plan.** `ExplainPlan` returning `("", nil)` renders:

```
No plan text. The statement prepared successfully and InterBase reported no plan
for it. Nothing was executed.
```

*Decision:* two sentences, not one, because "empty plan" and "nothing ran" are
independent facts and the driver README is explicit that an empty plan never
implies execution.

**Cancellation.** `explainQuery` runs on the async path (§6.1) and inherits the
same cancellation reporting (§6.5). A canceled prepare is non-mutating, so it
can only produce a `CancellationError`, never an `UncertainOutcomeError`.

### 2. Procedure and UDF awareness in completion

Follows existing conventions: candidates are `lsp.CompletionItem` values with a
`Kind`, a `Detail` string, and a markdown `Documentation`, produced by methods
on `*Completer` in a new `internal/completer/interbase_candidates.go`, gated on
`c.Driver == dialect.DatabaseDriverInterBase` and on the cache having the
relevant objects.

**New completion types** in the existing `completionType` enum:
`CompletionTypeProcedure`, `CompletionTypeProcedureName`,
`CompletionTypeGenerator`. `CompletionTypeView` already exists and is finally
consumed.

**Kinds, details and sort prefixes.** `getSortTextPrefix` gains three cases so
the new kinds do not fall into the keyword bucket:

| Object | `Kind` | `Detail` | sort prefix |
| --- | --- | --- | --- |
| view | `ClassCompletion` | `view` | `1` (existing, sorts with tables) |
| selectable procedure in `FROM` | `MethodCompletion` | `selectable procedure` | `10` (new) |
| procedure after `EXECUTE PROCEDURE` | `MethodCompletion` | `procedure` | `10` (new) |
| procedure output parameter | `FieldCompletion` | `column from "<PROC>"` | `0` (existing) |
| external function | `FunctionCompletion` | `external function` | `10` (existing) |
| generator | `ValueCompletion` | `generator` | `11` (new) |

*Decision:* reuse `MethodCompletion`/`ValueCompletion` rather than invent a
parallel mechanism; they give distinct editor icons and the sort switch is
written to be extended.

**Contexts.**

- *Procedure name after `EXECUTE PROCEDURE`.* Requires a parser change: add
  `"EXECUTE": {"PROCEDURE"}` to `multiKeywordMap` and a new
  `parseutil.ExecuteProcedure` syntax position matched by
  `genKeywordMatcher([]string{"EXECUTE PROCEDURE"})`, placed in
  `CheckSyntaxPosition` before the `TableReference` case. `getCompletionTypes`
  maps it to
  `[]completionType{CompletionTypeProcedureName, CompletionTypeKeyword}`.

  *Decision, and its real blast radius:* `multiKeywordMap` is
  dialect-independent — `parser.go:112` applies it on every parse — so this does
  change parsing for other drivers wherever the literal sequence appears.
  PostgreSQL's `EXECUTE stmt` and MSSQL's `EXECUTE proc` are unaffected because
  `PROCEDURE` does not follow, but **PostgreSQL's legacy trigger syntax
  `CREATE TRIGGER … FOR EACH ROW EXECUTE PROCEDURE f()` contains exactly this
  sequence** and is still accepted by current PostgreSQL (superseded by
  `EXECUTE FUNCTION` in PG 11, not removed). For that statement the syntax
  position after the keywords changes from `Unknown` to `ExecuteProcedure`.
  That is precisely why `CompletionTypeKeyword` is retained in the branch:
  without it, a PostgreSQL user writing a trigger would lose the keyword
  candidates they get today and receive nothing, because no procedure cache
  exists for that driver. With it, the PostgreSQL user keeps exactly today's
  behavior and the InterBase user gains procedure names. Known limitation,
  shared with the existing
  `DELETE FROM` handling: irregular internal whitespace
  (`EXECUTE&nbsp;&nbsp;PROCEDURE`) does not match, because `IsMatchKeyword`
  compares `node.String()`.
- *Selectable procedures in `FROM`.* `CompletionTypeTable` contexts also emit
  procedure candidates for procedures with at least one output parameter.
  InterBase selectable procedures are legal wherever a relation is.
- *Procedure output parameters as columns.* `columnCandidates` gains an
  InterBase fallback: when `ColumnDescs(table.Name)` misses and
  `DBCache.Procedure(table.Name)` hits, generate field candidates from
  `OutputParameters` with detail `column from "<PROC>"`. This covers both
  `SELECT <cursor> FROM MYPROC(1)` and `MYPROC.<cursor>`.
- *External functions.* Appended to the existing `CompletionTypeFunction`
  branch alongside `dialect.DataBaseFunctions(c.Driver)` — no new type, no new
  context, since a UDF is callable exactly where a built-in function is.
- *Generators.* Emitted only when the cursor is inside the argument list of a
  call whose function name is `GEN_ID` (case-insensitive), detected from the
  enclosing `ast.FunctionLiteral` — the same detection §3 uses. *Decision:*
  offering every generator in every expression position would bury column
  candidates; `GEN_ID(` is where a generator name is actually required.
- *Views.* `CompletionTypeView` branches in `Complete` call a new
  `c.ViewCandidates(ctx.parent)`, mirroring `TableCandidates`, with the D4
  disjointness requirement (or the `EqualFold` de-duplication fallback).

**Case-insensitivity.** All lookups go through the D4 accessors, which are
case-insensitive. `filterCandidates` already upper-cases both sides, so a user
typing `myp` matches `MYPROC` without further work.

**Documentation body.** Procedures render a markdown block: the description if
present, then an input parameter list and an output parameter list, each entry
`` - NAME: `TYPE` (input) ``, with nullability rendered only when known (§3).
Generators render `` `GEN` generator `` plus description. External functions
render the argument types, the return position, and the module/entry point, all
labelled as declaration metadata.

### 3. Signature help for procedures

**Trigger.** `SignatureHelpTypeExecuteProcedure` added to the
`signatureHelpType` enum. Detection is *not* done through
`CheckSyntaxPosition`, which is position-shaped rather than node-shaped; instead
`SignatureHelpWithDriver` gains a branch, before the existing `InsertValue`
case, that walks `nodeWalker.CurNodes()` for an `*ast.FunctionLiteral`, takes its
first token as the callee name, and proceeds when
`dbCache.Procedure(name)` hits. *Decision:* keying on "the callee is a known
procedure" rather than on a preceding `EXECUTE PROCEDURE` makes the same code
serve `SELECT * FROM MYPROC(?)`, which is the other place procedure arguments
are typed.

**Active parameter.** The `*ast.FunctionLiteral`'s `ast.Parenthesis` child
contains an `*ast.IdentifierList`; `GetIndex(pos)` gives the index, exactly as
the insert path does. An empty argument list yields index 0.

**Rendering.**

```
Label:         MYPROC (IN_CODE, IN_AMOUNT)
Documentation: MYPROC procedure — 2 input parameters, 1 output parameter
Parameters:
  {Label: "IN_CODE",   Documentation: "`VARCHAR(3)` input NOT NULL"}
  {Label: "IN_AMOUNT", Documentation: "`NUMERIC(18, 2)` input"}
```

**Nullability.** Rendered **only** when `Nullable.Valid && !Nullable.Bool`, as
the single word `NOT NULL`. When `Nullable` is not valid — the common case per
`schema/README.md` — nothing is rendered. *Decision:* absence is the honest
encoding of unknown; printing "nullable" would assert a fact the catalog does
not contain, and printing "unknown nullability" is noise in a one-line tooltip.
The same rule governs the hover parameter list.

**Output parameters** are not offered as signature parameters (they are not
arguments), but the count appears in the signature documentation so the user can
tell a selectable procedure from an executable one.

**Known limitation.** `MYPROC (1, 2)` with a space before the parenthesis is not
grouped into a `FunctionLiteral` by `parseFunctions`, so no signature help
appears. This matches existing behavior for built-in functions and is not worked
around here.

### 4. Hover backed by real DDL

**Placement.** `hoverWithDriver` stays a pure cache-only function and keeps its
current output. The database-backed part lives in
`handleTextDocumentHover`, after the pure call succeeds:

1. Resolve a hover *target* — `(kind database.ObjectKind, name string)` — with a
   new `resolveInterBaseHoverTarget(text, params, cache, driver)` in
   `internal/handler/interbase_hover.go`, reusing the same `NodeWalker` and
   identifier matchers as `hoverWithDriver`. A target is produced only for an
   identifier that names a cached table, view, procedure, trigger, generator, or
   external function; columns and aliases produce none.
2. If a target exists and `repo` implements `database.DDLDescriber`, call
   `ObjectDDL` under a **3-second derived context**.
3. On success, append to the existing markdown content:

   ````
   ---

   ```sql
   CREATE PROCEDURE ...
   ```
   ````
4. On `errors.Is(err, database.ErrUnsupportedDDL)`, append exactly one italic
   line built from `UnsupportedDDLDetail`, e.g.
   `*DDL not shown: the catalog does not record declaration nullability for parameter IN_AMOUNT.*`
5. On any other error (including the 3-second timeout), append nothing and
   `log.Printf` it. The hover response is the unchanged summary.

*Decisions.* (a) Append rather than replace, so the markdown column table that
existing tests assert survives and the user gets both views. (b) Lazy per-hover
fetch rather than caching all DDL at `ReCache` time, because DDL for every
object is large and mostly unread. (c) A 3-second bound, because today's request
dispatch is synchronous for hover (§6.1 only moves `workspace/executeCommand`
off that path), so a slow catalog read would otherwise freeze the whole server.
(d) A never-failing hover: a DDL problem must never surface as a JSON-RPC error
or an error popup, because the user asked for documentation, not for DDL.

**Memoisation.** A `map[ddlKey]string` on `Server` guarded by the same mutex as
the rest of the mutable server state, keyed by `{generation, kind, name}` where
`generation` is an `int` bumped by `reconnectionDB`. Cleared wholesale on
reconnect. Hover fires on every cursor rest over the same token; without this,
each one is a catalog round trip.

**Object-by-object behavior.**

| Hovered object | Summary part | DDL part |
| --- | --- | --- |
| table | existing column table | `ObjectDDL` output, or the note line when a computed column blocks it |
| view | column table from `ViewDesc.Columns` | `ObjectDDL`, else the note line; `ViewSource` is included in the summary regardless |
| procedure | name, description, input/output parameter list (nullability per §3) | `ObjectDDL`, else the note line; the verbatim `Source` is included in the summary regardless |
| trigger | name, relation, event, active flag | `ObjectDDL`, else note; verbatim `Source` in the summary |
| generator | name, description | `ObjectDDL` (`CREATE GENERATOR`) |
| external function (`Function`) | name, arguments, return position, module, entry point | **never attempted** — `schema` documents `Function` as permanently unsupported, so calling it would guarantee a wasted round trip and a note line |
| `DatabaseFile`, `Shadow` | not reachable | these are never identifiers in a SQL statement, so they are out of scope for hover entirely |

The user never sees a fabricated declaration: what is not renderable as DDL is
shown as the catalog's own summary fields, or as verbatim catalog source text
with no synthesized header.

### 5. Go-to-definition for procedures, views, and triggers

**This is the feature that is materially harder than it looks, and this section
specifies the smaller version.**

The hard part is not finding the source — the catalog has it. It is that LSP's
`Location` is a URI plus a range, and there is no portable way to hand a client
text that has no file. The obvious design, a `sqls-interbase://` virtual
document, requires every client to register a content provider: VS Code can (via
an extension change we do not control), and Neovim, Emacs `lsp-mode` and Vim
cannot without per-client work. Returning a URI the client cannot open is worse
than returning nothing. **Rejected.**

**The shipped design: read-only source snapshots as real files.**

- **Store.** `internal/handler/interbase_definition.go` owns a
  `sourceSnapshotStore` with an **injected root**:

  ```go
  type sourceSnapshotStore struct {
      root string // absolute; empty means "resolve from the user cache dir"
      // ...
  }

  func defaultSnapshotRoot() (string, error) {
      cacheDir, err := os.UserCacheDir() // returns (string, error)
      if err != nil {
          return "", fmt.Errorf("locate user cache directory: %w", err)
      }
      return filepath.Join(cacheDir, "sqls", "interbase-sources"), nil
  }
  ```

  *Decision:* the root is a field, resolved once at construction, not an inline
  `os.UserCacheDir()` call at each use. Two reasons. First,
  `os.UserCacheDir` returns `(string, error)`, so it cannot be nested inside
  `filepath.Join` at all — an earlier draft of this spec had exactly that
  non-compiling shape and a planner would have transcribed it. Second,
  `t.Setenv("XDG_CACHE_HOME", …)` only redirects `os.UserCacheDir` on
  Linux and BSD; on macOS it resolves to `~/Library/Caches` regardless, so a
  prune test that relied on the environment variable would operate on a real
  user directory. CI is ubuntu-only, so that would never be caught there. Tests
  set `root` to `t.TempDir()` directly and the hazard disappears.

  When `defaultSnapshotRoot` fails, go-to-definition for database-resident
  objects is disabled for the session and logged once; every other feature is
  unaffected.
- **URI scheme.** Ordinary `file://`. Everything that can open a file can open
  these.
- **Directory layout.** One directory per server process per connection:
  `<root>/<hash>-<pid>/`, where `<hash>` is the first 16 hex characters of
  `sha256(attachment string)`. The attachment string is hashed, never written:
  it can contain a host and a path, and must not be left on disk in clear form.
  Directories are created `0o700`, files `0o600`.
- **File naming.** `<KIND>/<url.PathEscape(NAME)>.sql`, e.g.
  `procedure/MY%24PROC.sql`. Catalog names legitimately contain `$` and may
  contain spaces or quotes, so escaping is mandatory.
- **Content.** A banner comment, then the body:

  ```sql
  -- sqls: read-only snapshot of InterBase PROCEDURE "MYPROC"
  -- connection: local_ib    generated: 2026-09-19T10:04:11Z
  -- Editing this file does not change the database.
  CREATE PROCEDURE "MYPROC" (...) AS BEGIN ... END
  ```
- **Body selection.** `ObjectDDL` output when it succeeds. On
  `ErrUnsupportedDDL` — the normal case for procedures, per `schema/README.md` —
  the body is the **verbatim** `Source` / `ViewSource` from the catalog,
  preceded by one more comment line naming the blocking feature. No `CREATE`
  header is synthesized in that case. When neither is available, definition
  returns `nil, nil` and the client shows "no definition found".
- **Range.** The generator records the 0-based line and UTF-16 character offset
  of the first occurrence of the object name after the banner, and returns a
  zero-width range there; `(0,0)` if the name does not appear (possible when
  `GenerateDDL` quotes it differently than the catalog spells it).
- **Lifetime.** Written or overwritten on every definition request — a snapshot
  is never served stale. At server start, sibling directories with an mtime
  older than 24 hours are removed. *Decision:* mtime pruning rather than
  PID-liveness probing, because it is portable, needs no signals, and cannot
  delete a concurrent instance's live directory (which is why the PID is in the
  name).
- **Shutdown cleanup must not depend on `Server.Stop` reaching its end.**
  `Stop` (`handler.go:72-78`) is:

  ```go
  func (s *Server) Stop() error {
      if err := s.dbConn.Close(); err != nil {
          return err   // <- everything after this is skipped
      }
      s.worker.Stop()
      return nil
  }
  ```

  A failing `dbConn.Close()` — entirely plausible against a half-dead InterBase
  attachment — returns early, so snapshot removal appended after that line would
  silently not run, and a shutdown test asserting removal would be flaky by
  construction. `Stop` is therefore restructured so cleanup is unconditional:

  ```go
  func (s *Server) Stop() error {
      defer s.snapshots.RemoveAll() // best effort, logs its own failure
      defer s.worker.Stop()
      return s.dbConn.Close()
  }
  ```

  This also fixes `worker.Stop()` being skipped on the same path, which is a
  pre-existing goroutine leak on the error branch. `handleExit`
  (`handler.go:199-205`) calls `s.dbConn.Close()` and then `s.Stop()`, so
  `DBConnection.Close` must stay idempotent; it is (`driver.go:25-43` nil-guards
  its receiver), but a second call on a non-nil connection whose `Conn` is
  already closed returns `sql.DB.Close`'s nil, so the double call is safe.

**Resolution order** in `definitionWithDriver`: the existing alias/subquery
resolution runs first and wins. Only when it yields nothing, the driver is
InterBase, and the identifier under the cursor names a cached procedure, view,
or trigger, does the snapshot path run. This keeps every existing definition
test passing unchanged.

**Plumbing.** `definitionWithDriver` is currently pure and has no repository.
Rather than thread a repository through the pure function, `handleDefinition`
calls it first and, on an empty result, runs
`s.interBaseDefinition(ctx, params, f.Text)`, which does its own resolution,
`ObjectDDL` call, and snapshot write.

**Deliberately not built:** navigation to a specific line inside a procedure
body; `textDocument/references` across catalog source; watching for catalog
changes to invalidate snapshots; a writable round trip (`ALTER PROCEDURE` on
save). Each is a separate project, and the last one is a foot-gun in a language
server.

### 6. Execution semantics for query results

#### 6.1 Cancellation, end to end

Three parts, in sqls, because the transport has none (correction 2).

**a. Selective async dispatch.** A new `internal/handler/dispatch.go` provides a
`jsonrpc2.Handler` wrapper:

```go
func NewDispatcher(inner jsonrpc2.Handler) jsonrpc2.Handler
```

whose `Handle` runs `workspace/executeCommand` in its own goroutine and
everything else inline, exactly as today. `main.go` and the test harness wrap
`jsonrpc2.HandlerWithError(server.Handle)` with it. *Decision:* selective rather
than `jsonrpc2.AsyncHandler`, because making every request concurrent turns the
unsynchronised `Server` fields into a race on day one; this way exactly one
method is concurrent and exactly the state it touches needs guarding.

**b. Cancel registry.** `Server.handle` gains
`case "$/cancelRequest":` which unmarshals `{id}` and calls the registered
`context.CancelFunc`. `handleWorkspaceExecuteCommand` derives
`ctx, cancel := context.WithCancel(ctx)`, registers `cancel` under `req.ID`
in a mutex-guarded `map[jsonrpc2.ID]context.CancelFunc`, and deregisters on
return. Because dispatch is now non-blocking for that method, the cancel
notification can actually be read while the query runs. `Server.handle` needs
access to `req.ID`, which it already has.

**c. State guarding.** A `sync.RWMutex` named `stateMu` on `Server`. The audit
below covers every field declared at `internal/handler/handler.go:23-42`; each
one is classified, because an incomplete list is the failure mode that ships a
race.

| Field | Written by | Read by | Treatment |
| --- | --- | --- | --- |
| `files` (map) | `openFile`/`updateFile`/`closeFile` (inline) | every handler; `executeQuery`/`explainQuery` (async) | `stateMu` on every access, **plus** the copy rule below |
| `dbConn` | `reconnectionDB:350` (async-reachable) | `newDBRepository:387`, `parserDriver:434` | `stateMu` |
| `curDBCfg` | `newDBConnection:376` | `newDBRepository:387` | `stateMu` |
| `curDBName` | `switchDatabase:391` (async) | `newDBConnection:373` | `stateMu` |
| `curConnectionIndex` | `switchConnections:459` (async) | `newDBConnection:367` | `stateMu` |
| `WSCfg` | `handleWorkspaceDidChangeConfiguration:315` (**inline**) | `getConfig:423` ← `topConnection`/`showConnections`/`switchConnections` (**async**) | `stateMu` — a genuine inline-writer/async-reader race |
| `initOptionDBConfig` | `handleInitialize:171` (inline, once) | `topConnection:399` (async-reachable) | `stateMu` — write-once, but read from the async path, so the read is still guarded |
| `SpecificFileCfg`, `DefaultFileCfg` | `main.go:133`/`:140`, **before** `jsonrpc2.NewConn:151` | `getConfig:421`/`:425` | write-once-before-serving; stated as an invariant, not locked. Any future writer after serving begins must take `stateMu` |
| `worker` | `NewServer` | everywhere | pointer never reassigned; the *contents* are the worker's problem, see (d) |
| DDL memo (§4), snapshot store generation (§5) | new | new | `stateMu` |

**The copy rule for `files`.** `updateFile` (`handler.go:296-303`) mutates
`f.Text` **through the stored `*File` pointer**. Holding `stateMu` only while
looking the pointer up is therefore not enough — a reader that keeps the `*File`
and reads `f.Text` after releasing the lock races with a concurrent
`didChange`. Every reader must copy the field to a local `string` **while still
holding the lock**:

```go
s.stateMu.RLock()
f, ok := s.files[uri]
var text string
if ok {
    text = f.Text // copy under the lock, never retain f
}
s.stateMu.RUnlock()
```

`executeQuery` (`execute_command.go:125-141`) is the concrete offender today: it
retains `f` and reads `f.Text` sixteen lines later.

**d. The `Worker.dbRepo` race, which `stateMu` cannot fix.**
`Worker.ReCache` (`internal/database/worker.go:75-82`) writes `w.dbRepo = repo`
with no lock, and the worker goroutine reads it at `worker.go:58`
(`NewDBCacheUpdater(w.dbRepo)`) and `worker.go:85`. The existing `w.lock`
(`worker.go:15`) guards only `dbCache`. No mutex on `Server` can close this: the
reader is a goroutine inside `Worker` that no server lock is held across.

This is **pre-existing**, not introduced here: `ReCache` already runs on the
handler goroutine while the worker goroutine may still be reading `dbRepo` from
the previous `updateAdditionalCache` send. Making
`switchDatabase`/`switchConnections` async (they reach `reconnectionDB:355` →
`ReCache`) widens the window rather than creating it, and turning on `-race` in
CI (see below) is likely to surface it.

Fix, inside `internal/database/worker.go`: add `setRepo`/`repo` accessors taking
`w.lock`, have `ReCache` use `setRepo`, and have the goroutine at `worker.go:58`
and `updateAllCache` at `:85` read through `repo()`. This is a small, additive,
driver-neutral fix and is upstreamable on its own.

**e. Race detection is a deliverable, not an assumption.** The repository does
**not** run the race detector today:
`.github/workflows/test.yaml:21` is
`go test -coverprofile coverage.out -covermode atomic ./...` (note `-covermode
atomic` without `-race`), and `Makefile:37` (`test: build`) is
`go test -v ./...`. Neither passes `-race`. Since this sub-project introduces
the server's first concurrency, the concurrency plan must **add** it:

- a `-race` step in `.github/workflows/test.yaml`, and
- a `make test-race` target running `go test -race ./...`.

Adding `-race` may fail the first run because of (d). That is the point of
adding it, and (d) is scheduled in the same plan.

No timeout is added. With non-blocking dispatch a stalled query no longer
freezes the server, and the driver README is explicit that only process
supervision is a hard bound — a sqls-side timeout would imply a guarantee the
stack cannot make.

#### 6.1bis Command-versus-command concurrency

Making all of `workspace/executeCommand` async means `executeQuery`,
`explainQuery`, `switchDatabase` and `switchConnections` can now run at the same
time — and the last two call `reconnectionDB` (`handler.go:341`), which calls
`s.dbConn.Close()` and then reassigns `s.dbConn`. The §6.1c pattern of
"snapshot the repository under the lock, then do I/O" hands a query a
repository whose `*sql.DB` a concurrent switch may close. The spec must state a
policy, so here it is.

**First, precisely what goes wrong.** It is *not* memory-unsafe.
`database/sql`'s `DB.Close` is documented (`$GOROOT/src/database/sql/sql.go:925-927`)
as: "Close closes the database and prevents new queries from starting. Close
then waits for all queries that have started processing on the server to
finish." So an in-flight query is not torn down under the caller. The two real
hazards are:

1. a query that has snapshotted the repository but has **not yet started** when
   `Close` completes fails with `sql: database is closed` — a confusing error
   attributed to the user's SQL; and
2. `reconnectionDB` **blocks** until in-flight queries drain. With InterBase's
   best-effort cancellation that can be ~10 seconds (driver README: a
   row-lock wait returned after roughly 9.2–10 s). If `reconnectionDB` held
   `stateMu` for its duration, every inline request — hover, completion,
   didChange — would block behind it, which is a worse outcome than the
   original bug.

**Policy: serialise connection-mutating commands against in-flight database
work, with a second lock.** A `sync.RWMutex` named `connMu` on `Server`,
distinct from `stateMu`:

- `executeQuery`, `explainQuery`, `showDatabases`, `showSchemas`, `showTables`
  hold `connMu.RLock()` for the whole of their database work, including
  rendering;
- `switchDatabase` and `switchConnections` hold `connMu.Lock()` across
  `reconnectionDB`;
- `handleInitialize` and `handleWorkspaceDidChangeConfiguration` also take
  `connMu.Lock()` around their `reconnectionDB` call, since they reconnect too;
- `stateMu` is used only for short, non-blocking field accesses. **Lock
  ordering: `connMu` before `stateMu`, never the reverse**, and `stateMu` is
  never held across any I/O. `reconnectionDB` therefore performs `Close`,
  `Open` and `ReCache` while holding only `connMu.Lock()`, taking
  `stateMu.Lock()` only for the pointer assignments.

*Decision:* serialise rather than keep the old `*sql.DB` alive until its users
drain. Reference-counting connections would mean a switch returns "done" while
the previous database is still being queried and its results are about to land
in the pane — a confusing lie to the user — and it adds a lifetime subsystem to
close a hazard that a reader/writer lock closes in ten lines. The cost is that
switching connections waits for a running query, which is the behavior a user
would predict anyway.

Hover, completion, signature help and definition stay inline and take only
`stateMu`, so a long-running query never makes the editor feel dead.

#### 6.2 Read-only transaction for read-only statements

New driver-neutral optional capability in `internal/database/database.go`:

```go
type ReadOnlyQuerier interface {
    QueryReadOnly(ctx context.Context, query string) (*QueryResult, error)
}
```

`InterBaseDBRepository` implements it in the **untagged** `interbase_common.go`,
because it needs nothing beyond `database/sql`:

```go
tx, err := db.Conn.BeginTx(ctx, &sql.TxOptions{
    ReadOnly:  true,
    Isolation: sql.LevelReadCommitted,
})
// query, ScanRowsWithTypes, rows.Close(), then tx.Rollback()
```

The result is fully materialised inside the repository, so the transaction's
lifetime never escapes it — which is what makes this safe to call from the
handler without leaking a transaction on an early return. `s.query` uses it when
the assertion succeeds and falls back to the current `repo.Query` +
`database.ScanRows` path otherwise.

`EXECUTE PROCEDURE` **never** uses this path: per the driver README, an implicit
procedure query commits its write transaction, so a procedure call is a write
even when it returns a row.

#### 6.3 Typed rendering

New `internal/database/result.go`:

```go
type ColumnMeta struct {
    Name             string
    DatabaseTypeName string
    Nullable         sql.NullBool
    Length           sql.NullInt64
    Precision        sql.NullInt64
    Scale            sql.NullInt64
}

type QueryResult struct {
    Columns []ColumnMeta
    Rows    [][]string
    Notes   []string
    // Complete is false when scanning stopped early because of an error.
    Complete bool
}

// RenderOptions controls driver-sensitive cell formatting. The zero value
// reproduces the existing ScanRows output exactly.
type RenderOptions struct {
    // DistinguishNull renders SQL NULL as the literal NULL instead of an
    // empty cell. Set only for InterBase.
    DistinguishNull bool
    // MaxCellRunes caps rendered cell width. Zero means DefaultMaxCellRunes.
    MaxCellRunes int
}

// DefaultMaxCellRunes is the display cap applied when RenderOptions.MaxCellRunes
// is zero.
const DefaultMaxCellRunes = 512

func ScanRowsWithTypes(rows *sql.Rows, opts RenderOptions) (*QueryResult, error)
```

**Partial-result contract.** `ScanRowsWithTypes` returns a **non-nil
`*QueryResult` together with a non-nil error** when `rows.Next`, `rows.Scan` or
`rows.Err` fails partway: `Rows` holds everything scanned before the failure,
`Columns` is populated (column metadata is available before the first row), and
`Complete` is false. On success it returns `(result, nil)` with
`Complete == true`. It returns `(nil, err)` only when `rows.ColumnTypes()`
itself fails, i.e. when there is nothing to report.

This is a deliberate departure from `ScanRows`
(`internal/database/scan_row.go:30`), which returns `nil, err` and discards
every scanned row. `s.query` (`execute_command.go:279`) today compounds that by
returning `"", err`. Both are why the BLOB-limit failure in
"User-Visible Behavior" can show the rows that preceded the oversized value:
`s.query` must render a non-nil result **before** reporting the error, emit the
`N rows in set (incomplete)` footer when `Complete` is false, and return the
rendered string as the command result rather than propagating a bare error.
Without this contract that user-visible section is unimplementable.

`RowsAffected`-style paths are unaffected.

`ScanRowsWithTypes` calls `rows.ColumnTypes()` once, allocates per-column
destinations from `ScanType()` where it is concrete (`string`, `int64`,
`float64`, `bool`, `time.Time`, `[]byte`) and `any` otherwise, and formats:

| Case | Rendering |
| --- | --- |
| NULL | `NULL` (opt-in via `RenderOptions.DistinguishNull`) |
| empty string | empty cell |
| `time.Time` | `RFC3339Nano`, as today |
| `int64`, `float64`, `bool` | `%v` |
| scaled `NUMERIC`/`DECIMAL` (scan type `string`) | verbatim exact decimal text |
| `DatabaseTypeName == "BLOB"`, scan type `[]byte` | `<BLOB n bytes>` |
| `DatabaseTypeName == "BLOB"`, scan type `string` | first `MaxCellRunes` characters + `…(truncated, n characters)` |
| any cell over `MaxCellRunes` characters | same truncation marker |

*Decision:* `DistinguishNull` defaults to false and is set to true only for
InterBase, so no other driver's rendering or test output changes. It matters for
InterBase specifically because the driver guarantees "empty values remain
distinct from NULL" and the current renderer destroys that distinction.

*Decision:* `DefaultMaxCellRunes = 512` is a display cap, unrelated to the
driver's 64 MiB materialisation limit. It exists so a 40 MiB text BLOB that the
driver *did* materialise does not get pushed through JSON-RPC into the editor.
It is a named constant reachable through `RenderOptions.MaxCellRunes` rather
than a literal, so the tests can set a small cap without building huge fixtures.
This is stated in the user documentation so the cap is never mistaken for
truncated data.

The header row and `%d rows in set` footer stay exactly as they are; the
vertical writer is unchanged.

#### 6.4 `EXECUTE PROCEDURE` routing

In `executeQuery`, for the InterBase driver only, a statement whose
`QueryExecType` is `EXECUTE` is routed by looking up the procedure name — the
identifier following `EXECUTE PROCEDURE`, parsed from the already-parsed
statement — in the cache:

| Cache state | Path | Rationale |
| --- | --- | --- |
| procedure with ≥1 output parameter | `repo.Query` (driver `QueryContext`), rendered as a table, footer `1 row in set` | driver supports it and returns exactly one row |
| procedure with 0 output parameters | `repo.Exec` | driver requires `ExecContext` here |
| procedure not in cache | `repo.Exec` | see below |
| not InterBase | unchanged behavior | |

*Decision on the unknown case:* use `Exec` and surface the driver's rejection
rather than trying `Query` first and falling back. The driver rejects a
statement whose server-reported type is not result-producing, and rejects
output-producing procedures from `ExecContext`, in both cases on the basis of
the prepared statement type — but "try one path, then the other" is a shape that
can execute a mutating procedure twice if that reasoning is ever wrong, and a
stale cache is not worth that risk. The error message tells the user to refresh
the connection (`switchConnections` to the same index re-caches).

A one-row procedure result is rendered with an explicit note:
`EXECUTE PROCEDURE returns at most one row.`

#### 6.5 Failure classification

`internal/database/interbase_failure_native.go` (tagged) provides:

```go
type FailureKind int

const (
    FailureNone FailureKind = iota
    FailureCanceled
    FailureUncertain
)

func ClassifyFailure(err error) (FailureKind, string) // kind, operation
```

implemented with `errors.As` against `*interbase.UncertainOutcomeError` and
`*interbase.CancellationError` — checked in that order, because an uncertain
outcome wraps a cancellation. The stub returns `(FailureNone, "")`. No error
string is ever matched. `executeQuery`, `explainQuery`, and the hover/definition
paths render the messages in §"User-Visible Behavior".

## User-Visible Behavior and Failure Modes

Every message below is what the user reads in the results pane or hover popup.

**Plan is empty.**

```
-- statement 1
No plan text. The statement prepared successfully and InterBase reported no plan
for it. Nothing was executed.
```

**Explaining a DML statement.**

```
-- statement 1 (prepared only; nothing was inserted, updated or deleted)
PLAN (CITY INDEX (PK_CITY))
```

**Explaining an unsupported statement kind.**

```
Explain supports SELECT, INSERT, UPDATE, DELETE and EXECUTE PROCEDURE
statements; got CREATE TABLE.
```

**Explain on a driver without the capability.** JSON-RPC error:
`explain is not supported by the mysql driver`.

**DDL is unsupported (hover).** The usual hover content, then one italic line:

> *DDL not shown: the catalog does not record declaration nullability for
> parameter `IN_AMOUNT`.*

The procedure's verbatim source and parameter list are still shown above it. No
popup error, no fabricated `CREATE PROCEDURE` header. Hovering an external
function shows its declaration metadata and never mentions DDL at all.

**DDL is unsupported (go-to-definition).** The snapshot opens and reads:

```sql
-- sqls: read-only snapshot of InterBase PROCEDURE "MYPROC"
-- connection: local_ib    generated: 2026-09-19T10:04:11Z
-- Editing this file does not change the database.
-- Executable DDL could not be reproduced: procedure parameter nullability is
-- not recorded in the catalog. The verbatim catalog source follows.
BEGIN
  ...
END
```

**Query cancelled, outcome known.**

```
Cancelled. The statement was stopped before it finished.
```

Rendered when `ClassifyFailure` reports `FailureCanceled`. No rows are shown.

**Query cancelled, outcome uncertain.**

```
Cancelled, but the outcome is UNCERTAIN.

InterBase could not confirm whether this statement took effect. Do not re-run it
until you have checked the database state — reconcile by operation id or by
querying the affected rows.
```

Rendered when `ClassifyFailure` reports `FailureUncertain`. This is the one
message in the feature set that is deliberately loud: the driver README is
explicit that canceled writes must not be blindly retried, and the results pane
is the only place the user will see this.

**Cancellation that arrived too late.** If the statement completed before the
native cancellation took effect, the driver returns the successful result and
sqls renders it normally, with a note:

```
Note: the cancellation request arrived after the statement completed; the result
below is the real result.
```

This follows the README's "the executing native result remains authoritative".

**BLOB exceeds the 64 MiB materialisation limit.** The fetch fails partway. The
pane shows the rows fetched before the failure, then:

```
5 rows in set (incomplete)

Fetch failed: interbase: BLOB result exceeds the materialization limit

One or more BLOB columns in this result are larger than the driver's 64 MiB
materialisation limit. Re-run the query without the BLOB column, or select a
substring of it.
```

The hint line is emitted only when `ColumnTypes` reports at least one `BLOB`
column, so the advice is never wrong; the error text itself is passed through
verbatim rather than matched on. Partial rows are shown rather than discarded
because knowing *which* row broke is the fastest route to the oversized value.

**A BLOB that fits but is large.** Rendered as `<BLOB 4194304 bytes>` (binary)
or the first 512 characters plus `…(truncated, 1048576 characters)` (text
subtype). The display cap is documented so it is never mistaken for data loss.

**`EXECUTE PROCEDURE` on an unknown procedure.**

```
Exec failed: <driver error>

MYPROC is not in the catalog cache. If it was created after this connection
opened, switch to this connection again to refresh the cache.
```

**No InterBase capability present** (ordinary build, or non-InterBase driver):
every feature degrades to today's behavior. Completion offers tables and
keywords; hover shows the summary; definition resolves aliases; execution uses
`ScanRows`. Nothing errors.

## Testing Strategy

Following the repository's existing patterns: handler features are exercised
through a real `jsonrpc2` pipe with `TestContext`
(`internal/handler/handler_test.go:19`) and a mock repository installed via
`worker.ReCache` plus `tx.server.dbConn = &database.DBConnection{Driver:
dialect.DatabaseDriverInterBase}`, exactly as
`configureInterBaseTestServer` (`internal/handler/interbase_test.go:144`) does
today. Completer features are exercised as direct unit tests like
`TestComplete`. Live tests follow `internal/database/interbase_live_test.go`:
`//go:build interbase && cgo && linux && amd64` plus a `t.Skip` when
`INTERBASE_DATABASE`/`INTERBASE_USER`/`INTERBASE_PASSWORD` are unset.

A shared `configureInterBaseCapabilityServer(t, tx)` helper extends the existing
one with a `MockInterBaseDBRepository` (D8) carrying two procedures (`MYPROC`
with two inputs and one output, `DOWORK` with inputs only), one view, one
trigger, one generator, and one external function.

### Feature 1 — Explain

- `TestInterBaseExplainCodeActionIsAdvertised` — `textDocument/codeAction`
  returns a command with `Title == "Explain SQL"` and
  `Command == CommandExplainQuery`.
- `TestInterBaseExplainQueryRendersPlan` — `MockExplainPlan` returns a fixed
  `PLAN (...)`; assert the output contains it and the mock was called exactly
  once with the statement text.
- `TestInterBaseExplainQueryEmptyPlanReportsNoPlan` — mock returns `("", nil)`;
  assert the "No plan text" wording and that no error is returned.
- `TestInterBaseExplainQueryDMLShowsPreparedOnlyBanner` — `UPDATE ...`; assert
  the banner and that `MockExec`/`MockQuery` were never called.
- `TestInterBaseExplainQueryRejectsUnsupportedStatement` — `CREATE TABLE ...`;
  assert the refusal message and no `ExplainPlan` call.
- `TestExplainQueryWithoutCapabilityReportsUnsupportedDriver` — plain
  `MockDBRepository`; assert the error mentions the driver.

### Feature 2 — Completion

- `TestInterBaseProcedureCompletionAfterExecuteProcedure` — text
  `execute procedure `, cursor at end; assert `MYPROC` present with
  `Kind == lsp.MethodCompletion` and `Detail == "procedure"`, and that table
  candidates are absent.
- `TestInterBaseSelectableProcedureCompletionInFromClause` — `select * from `;
  assert `MYPROC` present (has output) and `DOWORK` absent (no output).
- `TestInterBaseProcedureOutputParameterColumnCompletion` —
  `select  from myproc`; assert the output parameter appears as a
  `FieldCompletion` with detail `column from "MYPROC"`.
- `TestInterBaseExternalFunctionCompletionInSelectExpr` — assert the UDF appears
  beside built-in function candidates with `Detail == "external function"`.
- `TestInterBaseGeneratorCompletionInsideGenId` — `select gen_id(` offers the
  generator; `select upper(` does not.
- `TestInterBaseViewCandidatesAreNotDuplicatedWithTables` — with a cache where a
  view name also appears in `SortedTables()`, assert exactly one candidate with
  that label.
- `TestInterBaseCompletionIsCaseInsensitive` — lowercase `myp` prefix matches
  `MYPROC`.
- `TestExecuteProcedureMultiKeywordParsing` (in `parser`) — assert
  `execute procedure foo` yields an `ast.MultiKeyword` whose `String()` is
  `execute procedure`, and `TestCheckSyntaxPosition` gains an
  `ExecuteProcedure` case.
- `TestCompletionKindSortPrefixesAreAssigned` — table test asserting
  `MethodCompletion`/`ValueCompletion` do not fall into the `"9999"` bucket.

### Feature 3 — Signature help

- `TestInterBaseSignatureHelpForExecuteProcedure` — table-driven over cursor
  columns like `genSingleRecordInsertTest`, asserting the full
  `lsp.SignatureHelp` with `cmp.Diff`, including `ActiveParameter` transitions
  across the comma.
- `TestInterBaseSignatureHelpForSelectableProcedureCall` —
  `select * from myproc(` produces the same signature.
- `TestInterBaseSignatureHelpOmitsUnknownNullability` — parameter with
  `Nullable: sql.NullBool{}` renders documentation with no nullability word at
  all; a parameter with `{Bool: false, Valid: true}` renders `NOT NULL`.
- `TestSignatureHelpUnknownCalleeReturnsNil` — a function name that is not a
  cached procedure returns `nil, nil`, preserving today's behavior.

### Feature 4 — Hover

- `TestInterBaseHoverProcedureAppendsDDL` — `MockObjectDDL` returns a
  `CREATE PROCEDURE` string; assert the response contains both the parameter
  summary and the fenced SQL block.
- `TestInterBaseHoverUnsupportedDDLFallsBackToSummary` — `MockObjectDDL` returns
  an error wrapping `database.ErrUnsupportedDDL` with feature text; assert the
  summary is intact, the note line contains the feature text, no fenced block
  appears, and `conn.Call` returns no error.
- `TestInterBaseHoverDDLErrorIsNotSurfacedAsRequestError` — `MockObjectDDL`
  returns a plain error; assert the hover succeeds with the unchanged summary.
- `TestInterBaseHoverTableStillShowsColumnTable` — the existing
  `TestInterBaseDialect1LanguageServerHover` assertions must still hold with the
  capability mock installed.
- `TestInterBaseHoverExternalFunctionNeverCallsObjectDDL` — assert the mock call
  counter stays zero.
- `TestInterBaseHoverMemoisesObjectDDL` — two hovers on the same identifier
  produce one `MockObjectDDL` call; a `reconnectionDB` in between produces two.

### Feature 5 — Definition

All construct the store with `root: t.TempDir()` directly. They do **not** use
`t.Setenv("XDG_CACHE_HOME", …)`: that only steers `os.UserCacheDir` on Linux and
BSD, so on macOS the prune test would delete from a real user cache directory,
and ubuntu-only CI would never reveal it.

- `TestInterBaseDefinitionProcedureWritesReadOnlySnapshot` — assert the returned
  `lsp.Location.URI` has a `file://` scheme, the file exists with mode `0o600`
  in a `0o700` directory, the banner names the object, and the body is the
  `ObjectDDL` output.
- `TestInterBaseDefinitionUnsupportedDDLUsesVerbatimSource` — assert the body is
  `Source` with no synthesized `CREATE` line, and that the blocking feature is
  named in a comment.
- `TestInterBaseDefinitionRangePointsAtObjectName` — assert the range's line and
  character match the first occurrence of the name after the banner.
- `TestInterBaseDefinitionFallsBackToAliasResolution` — a document with aliases
  resolves the alias, not the procedure, keeping `definitionTestCases` green.
- `TestInterBaseDefinitionUnknownObjectReturnsNil` — no capability, or unknown
  name, returns `nil, nil`.
- `TestInterBaseDefinitionSnapshotsRemovedOnShutdown` — after `Server.Stop`, the
  process directory is gone.
- `TestSnapshotsRemovedEvenWhenConnectionCloseFails` — a connection whose
  `Close` returns an error; assert `Stop` still removes the snapshot directory
  and still stops the worker. This is the regression test for the restructured
  `Stop` in §5, and it fails against today's early-return shape.
- `TestInterBaseDefinitionPrunesStaleSnapshotDirectories` — a sibling directory
  backdated past 24 hours is removed at start; a fresh one is not.
- `TestInterBaseDefinitionEscapesCatalogNames` — an object named `MY$PROC "X"`
  produces a path with no unescaped `$`, quote, or separator.

### Feature 6 — Execution

- `TestExecuteCommandDispatchesAsynchronously` — a mock whose `Query` blocks on
  a channel; assert a second request (`textDocument/hover`) completes while it
  is in flight.
- `TestExecuteQueryHonoursCancelRequest` — issue `workspace/executeCommand` from
  a goroutine, send `$/cancelRequest` with the same id, assert the call returns
  within a short bound and the mock saw a canceled context.
- `TestCancelRequestForUnknownIDIsIgnored` — no panic, no error.
- `TestDidChangeDuringAsyncQueryDoesNotRaceOnFileText` — run a blocking
  `executeQuery` while hammering `textDocument/didChange` on the same URI;
  meaningful only under `-race`, and it is the regression test for the §6.1c
  copy rule.
- `TestWorkspaceConfigurationChangeDuringAsyncCommandDoesNotRace` — the same
  shape for `WSCfg`: inline `workspace/didChangeConfiguration` against an
  in-flight `showConnections`.
- `TestSwitchConnectionWaitsForInFlightQuery` — assert `switchConnections`
  blocks until a running `executeQuery` finishes, and that the query result is
  returned intact rather than failing with `sql: database is closed`. This is
  the §6.1bis regression test.
- `TestQueryAfterSwitchUsesNewConnection` — a query issued after a completed
  switch reaches the new repository, proving the `connMu` window closes.
- `TestWorkerReCacheIsRaceFreeUnderConcurrentUpdates` (in `database`) — call
  `ReCache` repeatedly while the worker goroutine services updates; meaningful
  under `-race`, and it covers the pre-existing `Worker.dbRepo` race fixed in
  §6.1d.
- CI: the `-race` step added in §6.1e is itself the acceptance criterion for
  Plan 1 — the plan is not done until `go test -race ./...` is green in
  `.github/workflows/test.yaml` and reproducible through `make test-race`.
- `TestExecuteQueryUsesReadOnlyTransactionForSelect` — a mock implementing
  `ReadOnlyQuerier` records that `QueryReadOnly`, not `Query`, was used for
  `SELECT`.
- `TestExecuteQueryFallsBackWhenReadOnlyQuerierAbsent` — plain mock still works.
- `TestExecuteProcedureWithOutputUsesQueryPath` /
  `TestExecuteProcedureWithoutOutputUsesExecPath` — assert routing from the
  cached procedure's output arity, and that the one-row note appears only in the
  first.
- `TestExecuteProcedureUnknownProcedureUsesExecAndExplainsCacheRefresh` — assert
  `Query` was never called and the refresh hint is in the message.
- `TestScanRowsWithTypesDistinguishesNullFromEmptyString` (in `database`, using
  a local `database/sql/driver` fixture as `schema`'s unit suite does) — `NULL`
  vs `""`.
- `TestScanRowsWithTypesRendersBlobPlaceholder` — binary BLOB renders
  `<BLOB n bytes>`; text BLOB truncates at 512 characters.
- `TestScanRowsWithTypesRendersScaledNumericExactly` — a `NUMERIC(18,2)` column
  whose scan type is `string` passes through unmodified.
- `TestScanRowsWithTypesDefaultsPreserveExistingRendering` — with a zero
  `RenderOptions`, output matches `ScanRows` for the existing
  `scan_row_test.go` fixtures.
- `TestScanRowsWithTypesReturnsPartialRowsOnScanFailure` — a fixture driver
  that fails on the third `Next`; assert the returned `*QueryResult` is non-nil
  alongside the error, holds two rows, and has `Complete == false`.
- `TestQueryRendersPartialResultBeforeReportingError` — `s.query` with that
  fixture renders the two rows and the `2 rows in set (incomplete)` footer
  rather than returning `"", err`.
- `TestInterBaseClassifyFailureUncertainOutcome` (tagged `interbase`) —
  construct an `*interbase.UncertainOutcomeError` wrapping a
  `*interbase.CancellationError`; assert `FailureUncertain`, and assert the
  reverse nesting yields `FailureCanceled`.
- `TestQueryFailureMessagesMatchClassification` — the uncertain message contains
  "UNCERTAIN" and "Do not re-run"; the canceled message does not.
- `TestBlobLimitHintOnlyWithBlobColumn` — the hint appears when `ColumnTypes`
  reports a `BLOB` column and not otherwise.

### Live, gated

In `internal/database/interbase_live_test.go` and a new
`internal/handler/interbase_live_test.go` with the same build tag and skip:

- `TestInterBaseLiveExplainPlanDoesNotExecuteDML` — `ExplainPlan` an
  `UPDATE ... WHERE 1=0` against a read-only account; assert no error other than
  a permission error, and that a following `SELECT COUNT(*)` is unchanged.
- `TestInterBaseLiveExplainPlanOnSelectReturnsPlanText` — assert the result
  starts with `PLAN`.
- `TestInterBaseLiveExecuteProcedureWithOutputIsRejectedByExec` — assert
  `ExecContext` on an output-producing procedure returns an error; the
  no-side-effect guarantee itself is covered by the driver's own integration
  suite, which owns a writable fixture.
- `TestInterBaseLiveQueryCancellationReturnsTypedError` — cancel a deliberately
  slow catalog cross join; assert `ClassifyFailure` reports `FailureCanceled` or
  `FailureUncertain` and never `FailureNone`.
- `TestInterBaseLiveObjectDDLRoundTrip` — for every procedure in the target
  database, assert `ObjectDDL` either succeeds or fails with
  `database.ErrUnsupportedDDL`, and never with anything else.

## Documentation

- **`README.md`**: flip `- [ ] Explain SQL` to `- [x] Explain SQL` with a nested
  bullet `(InterBase only; other drivers report that the command is
  unsupported)`. Add an "InterBase editor features" subsection under the
  existing "InterBase Build" section covering: what Explain does and, in one
  sentence, that it prepares without executing; which objects appear in
  completion; that hover shows DDL when the catalog can reproduce it and says so
  when it cannot; that go-to-definition opens a read-only snapshot file under
  the user cache directory, where that directory is, and that editing it does
  not change the database; that query results distinguish `NULL` from the empty
  string; that cells are display-capped at 512 characters; and that cancelling a
  query is best effort, with the uncertain-outcome warning quoted.
- **`doc/develop.md`**: two short sections. First, the capability-interface
  pattern — new InterBase behavior goes behind an optional interface asserted at
  the call site, driver imports stay behind the `interbase` build tag, and
  shared code is extended additively — with the file table from §0 as the map.
  Second, the **server concurrency invariants**, because they are the kind of
  rule a future contributor breaks silently: which methods run async, that
  `connMu` is taken before `stateMu` and never the reverse, that `stateMu` is
  never held across I/O, that any new `Server` field must be added to the §6.1c
  audit table, that readers of `files` copy `Text` under the lock rather than
  retaining the `*File`, and that `make test-race` is the check that catches
  violations.
- **Code comments**: only where the reason is not visible in the code, matching
  the surrounding density. Four places earn one: why `EXECUTE PROCEDURE` never
  uses the read-only transaction; why the unknown-procedure case does not retry
  across paths; why nullability is omitted rather than rendered as unknown; and
  why the snapshot directory name contains a hash rather than the attachment
  string.

## Deliberate omissions and risks

**Left out on purpose** (beyond the Excluded list): no per-object DDL prefetch;
no configuration knobs; no new LSP capabilities advertised in
`handleInitialize` beyond what is already there — Explain rides the existing
`CodeActionProvider`, and definition rides the existing `DefinitionProvider`; no
progress reporting for long queries (`WorkDoneProgress` is declared false today
and turning it on is a separate, client-visible change).

**Genuine risks.**

1. *The async dispatch change is the riskiest thing here.* It converts a
   single-threaded server into a two-threaded one. The locks in §6.1c/6.1bis
   must cover every mutable `Server` field, and the `Worker.dbRepo` fix in
   §6.1d is outside any server lock. **There is no existing race-detection
   safety net to fall back on**: neither `.github/workflows/test.yaml:21` nor
   `Makefile:37` passes `-race`, so adding it is a deliverable of the
   concurrency plan (§6.1e), not a mitigation that already exists. Expect the
   first `-race` run to fail on the pre-existing `Worker.dbRepo` race, which is
   scheduled in the same plan. Sequencing mitigates the rest: this work is
   Plan 1 and lands alone, with nothing else in flight on top of it.
2. *`EXECUTE PROCEDURE` routing depends on a cache that can be stale.* A
   procedure created or altered after connect routes on old output arity. The
   chosen behavior fails safely (one rejected statement, a clear message) rather
   than guessing, but it is a real papercut.
3. *Go-to-definition writes catalog source to the user's disk.* Modes are
   restrictive and the directory is pruned, but a crashed process leaves
   business logic in the cache directory for up to 24 hours. This is documented
   in the README rather than hidden. Anyone for whom that is unacceptable should
   not use this feature; there is no way to satisfy portable LSP navigation
   without it.
4. *The `EXECUTE PROCEDURE` multi-keyword addition touches shared parser state.*
   It fires on that literal sequence for **every** dialect, and PostgreSQL's
   legacy `CREATE TRIGGER … EXECUTE PROCEDURE f()` contains it, so PostgreSQL
   parsing does change. The retained `CompletionTypeKeyword` fallback (§2) means
   no PostgreSQL user loses a candidate they get today, but this remains the one
   change in this sub-project that alters behavior for other drivers and the one
   to scrutinise before upstreaming.
5. *Cancellation is best effort and cannot be made better from sqls.* A stalled
   native call outlives its context; the user may see the cancel take seconds.
   The messages say "stopped", never "guaranteed stopped", precisely because of
   this.
6. *Hover latency.* A 3-second bound on a synchronous handler is still up to
   three seconds of frozen server on a slow link. The memo makes repeats free,
   but the first hover on a cold object is exposed. If this proves painful in
   practice, the fix is to move hover onto the async path too — deliberately not
   done here, because it widens the concurrency surface for a feature that is
   usually cache-only.
7. *`connMu` makes connection switching wait for a running query*, which against
   an InterBase statement stuck on a row lock can be ~10 seconds (driver
   README). The alternative — letting a switch report success while the old
   connection's results are still arriving into the pane — was judged worse, but
   a user who switches during a slow query will notice the pause.
