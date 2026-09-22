# InterBase Named Query Parameters Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute InterBase SQL containing named parameters from Neovim, prompt for typed values, and prefill subsequent prompts without changing the SQL buffer.

**Architecture:** A pure scanner rewrites named occurrences to positional markers. A stateless discovery command identifies the selection and connection; execution revalidates that identity and binds submitted values through optional repository capabilities. The Neovim adapter owns the asynchronous prompts and bounded in-memory prefill cache.

**Tech Stack:** Go, existing sqls parser/dialect and database/sql, existing interbase-go positional bindings, Neovim 0.11+ Lua APIs, nanotee/sqls.nvim preview/filetype conventions.

**Spec:** `docs/superpowers/specs/2026-09-22-interbase-query-parameters-design.md` (approved by user).

## Global Constraints

- The SQL buffer remains unchanged. The value is a bound parameter, never SQL text interpolation.
- Keep this shared interface unchanged; add optional parameter-execution capabilities instead. (`DBRepository`.)
- Preserve read-only SELECT transactions, output-producing procedure routing, cancellation classification, partial results, and once-only execution.
- No driver API change is needed.
- Do not edit its downloaded checkout. (`nanotee/sqls.nvim`.)
- This first version supports named input parameters on InterBase connections.
- Unparameterized execution continues working for existing clients and drivers.
- PSQL declarations/bodies and unnamed `?` input prompts are outside this feature.
- Keep at most 100 query entries per client, evicting least-recently-used entries.
- Do not include entered values in adapter diagnostic messages.
- Live validation on the user-authorized centrale and NRF01 connections is limited to bounded read-only parameterized SELECTs and prepare-only Explain; use fixtures for mutation/cancellation behavior.
- The unrelated catalog-startup performance problem remains a separate task.
- Preserve `connMu` before `stateMu`; no database I/O under `stateMu`. No lock/transaction is retained while the user answers prompts.
- All dispatched agents use an explicitly selected Anthropic model. Coordinator owns independent review; workers do not dispatch children.

## Workspace, ownership and execution order

Server worktree: `~/gits/ibwt/sqls-params/sqls`, branch
`feat/interbase-query-parameters`; implementation base is `0912dfe`. Sibling
`interbase-go` is a read-only symlink to the canonical driver checkout.
Baseline `go test ./... -count=1` passed when the design was committed.

Create a sibling configurations worktree for the client task at execution time.
The live `Both/Neovim/nvim/lua/plugins/lspconfig.lua` contains existing uncommitted
setup/Delphi changes. Preserve a copy of that exact live file in the execution
ledger directory and seed the configurations worktree with it. New adapter/test
files can be committed there; keep the seeded/wiring file unstaged. At deployment,
apply only the adapter-wiring delta relative to the saved live baseline. Do not
commit or overwrite the user's other changes. If its touched region changed in
the meantime, reconcile against the new live file rather than replacing it.

Tasks are sequential: scanner → values/repository → discovery → execution →
Neovim adapter → integration. Each has a focused test/review boundary. Server
tasks 3 and 4 share handler/lsp files and must not run concurrently.

### File map

| File | Responsibility |
| --- | --- |
| `internal/queryparams/scan.go`, `scan_test.go` | Lexical recognition, statement spans, positional rewrite, first-token handling |
| `internal/queryparams/value.go`, `value_test.go` | Tagged wire values, validation and occurrence-ordered argument arrays |
| `internal/database/parameter.go` | Optional bound execution interfaces |
| `internal/database/interbase_common.go` | Argument-aware pooled and read-only execution, shared with legacy methods |
| `internal/database/interbase_readonly_test.go`, `interbase_parameter_test.go` | Driver-argument and transaction/resource evidence |
| `internal/database/query_type.go`, `query_type_test.go` | Comment-aware existing routing classification |
| `internal/lsp/query_parameters.go`, `internal/lsp/lsp.go` | Versioned discovery/submission wire contract and command advertisement |
| `internal/handler/query_parameters.go`, `query_parameters_test.go` | Selection identity, discovery, submission validation, batch preflight |
| `internal/handler/query_parameters_fixture_test.go` | Isolated recording backend for command/race tests |
| `internal/handler/execute_command.go`, `explain.go`, `handler.go` | Dispatch, existing result/routing path integration, capability advertisement |
| `configurations/Both/Neovim/nvim/lua/sqls_parameters.lua` | Adapter, prompts, cache, execution and result preview |
| `configurations/Both/Neovim/nvim/tests/sqls_parameters_test.lua` | Stateful asynchronous behavior regressions; no declarative/keybinding tests |
| `configurations/Both/Neovim/nvim/lua/plugins/lspconfig.lua` | Small integration delta to the existing SQL setup |
| `README.md`, `doc/develop.md` | User workflow and extension contract |

## Exact cross-task contracts

### Pure scanner and values

The new package does not import handler, database or lsp packages.

```go
package queryparams

type Parameter struct {
    Name string `json:"name"` // first spelling, without colon
    Key  string `json:"key"`  // ASCII uppercase
}
type Statement struct {
    SQL  string   // original text, only genuine :name occurrences rewritten
    Keys []string // canonical key per ? occurrence
}
type Batch struct {
    Parameters []Parameter // unique, first-appearance order across statements
    Statements []Statement
}
func Compile(text string, sqlDialect int) (Batch, error)
func TrimLeadingTrivia(text string) string

type Value struct {
    Name  string `json:"name"`
    Type  string `json:"type"`
    Value string `json:"value"`
}
func Convert(value Value) (any, error)
func Bind(batch Batch, values []Value) ([][]any, error)
```

`Compile` is the named-parameter batch compiler, not a replacement general SQL
parser. Call it on the parameter-aware InterBase path. A no-parameter discovery
returns an empty parameter list and lets ordinary SQL take the existing path.
When a selection contains any real parameter markers, compile/validate the
entire selection before submitting anything. Supported statement starts are
SELECT, INSERT, UPDATE, DELETE, and EXECUTE PROCEDURE. Accept WITH-prefixed
InterBase SELECTs on the existing WITH query-classification path as well.
Reject PSQL/DDL batches
containing named markers, mixed named/positional markers, and bare positional
markers in this discovery flow. Comments/literals containing `?` do not count.

Type wire strings: `text`, `integer`, `number`, `date`, `timestamp`, `boolean`,
`null`. Number means finite float64; integer means int64. Empty text is valid.
NULL's value must be empty, while text `NULL` is ordinary text. Booleans use
`true`/`false`. Dates use `2006-01-02`; timestamps use
`2006-01-02 15:04:05[.fraction]` with at most nine fractional digits and UTC as
the neutral Go location (no wall-clock timezone conversion). Numeric/date
types permit surrounding whitespace via `strings.TrimSpace`; text does not.
Conversion errors identify parameter name/type, never echo its input value.

### Repository capabilities

```go
type ParameterizedRepository interface {
    ExecParams(context.Context, string, []any) (sql.Result, error)
    QueryParams(context.Context, string, []any) (*sql.Rows, error)
}
type ParameterizedReadOnlyQuerier interface {
    QueryReadOnlyParams(context.Context, string, []any) (*QueryResult, error)
}
```

### Wire protocol

Command name: `getQueryParameters`, advertised alongside the existing commands
in `ServerCapabilities.ExecuteCommandProvider.Commands`. No experimental field
is required. Discovery request uses existing URI/Range fields.

```go
// internal/lsp/query_parameters.go
type QueryParameterContext struct {
    Version              int    `json:"version"` // exactly 1
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
// Add to ExecuteCommandParams:
// ParameterValues *QueryParameterSubmission `json:"parameterValues,omitempty"`
```

`connectionKey` is the hex SHA-256 of JSON encoding an explicit identity tuple:
configured driver, alias, attachment string, host, port, path, database name,
user, role, charset, and effective connection database name. Do not serialize
the entire config: passwords and TLS secrets must not enter this tuple.
Preserve database path case. Nil config/connection returns an error, not a shared
empty identity. `queryKey` hashes JSON `[resolvedDialect, selectedSQL]`.
`documentKey` hashes the complete current document. It is submission validation
only and is not part of the prefill key. Whole-document validation deliberately
rejects even outside-selection edits made while prompting; identical SQL in
another buffer can still reuse prefills.

Generation is checked on submission but excluded from prefill keys so switching
away and back can recover values. Client-instance scoping prevents a restarted
server's initial generation from making an old prompt valid.

## Task 1: SQL-aware recognition and comment-safe classification

**Files:** create `internal/queryparams/scan.go`, `scan_test.go`; modify
`internal/database/query_type.go`, `query_type_test.go`.

**Consumes:** approved placeholder grammar and InterBase dialect numbers 1/3.
**Produces:** `Parameter`, `Statement`, `Batch`, `Compile`, `TrimLeadingTrivia`
with the exact contracts above.

- [ ] **Step 1: Write failing lexical and routing regressions.** Start with:

```go
func TestCompileRepeatedNamesAndIgnoredColons(t *testing.T) {
    source := "-- :ignored\nSELECT :EMPLYID, ':literal', :emplyid FROM T;"
    batch, err := Compile(source, 3)
    if err != nil { t.Fatal(err) }
    wantNames := []Parameter{{Name: "EMPLYID", Key: "EMPLYID"}}
    if !reflect.DeepEqual(batch.Parameters, wantNames) {
        t.Fatalf("parameters = %#v", batch.Parameters)
    }
    wantSQL := "-- :ignored\nSELECT ?, ':literal', ? FROM T"
    if batch.Statements[0].SQL != wantSQL { t.Fatal(batch.Statements[0].SQL) }
    if !reflect.DeepEqual(batch.Statements[0].Keys, []string{"EMPLYID", "EMPLYID"}) {
        t.Fatal(batch.Statements[0].Keys)
    }
}
```

Add table cases for doubled single/double quotes in both dialects; line and
block comments; semicolons inside literals/comments; non-ASCII text before a
placeholder; `$` in names; `::`, `:=`, `:9`; repeated names across two statements;
unterminated quote/comment; `SELECT :x, ?`; `SELECT ?`; comment-only input; and
`CREATE PROCEDURE ... :local` / `EXECUTE BLOCK ... :local` rejection. Assert the
exact preserved/replaced SQL and argument order, not only parameter counts.
Include a WITH-prefixed SELECT whose parameter occurs in the CTE and is reused
in the outer SELECT, verifying one prompt and two positional arguments.

Add `QueryExecType("/* heading */\n-- note\nSELECT 1", "")` expecting SELECT/true,
and comment-prefixed UPDATE expecting UPDATE/false. Pin legacy SELECT INTO and
PRAGMA behavior so trimming does not replace the existing classifier rules.

- [ ] **Step 2: Run the failing tests.**

```sh
go test ./internal/queryparams ./internal/database -run 'TestCompile|TestQueryExecType' -count=1
```

- [ ] **Step 3: Implement the scanner as a byte-offset state machine.** States:
ordinary, single-quoted, double-quoted, line-comment, block-comment. Only ordinary
state recognizes parameter names and statement separators. Doubled delimiters
stay in the quoted state; copy original UTF-8 bytes verbatim. Finish a statement
at an unquoted semicolon; omit that separator, trim outer whitespace, ignore
comment-only fragments. Block comments follow the existing InterBase lexer;
do not invent nested-comment behavior. In the ordinary state, consume `::` and
`:=` as two-byte sequences before testing for `:name`.

```go
// Existing QueryExecType, before its empty-prefix check:
prefix = queryparams.TrimLeadingTrivia(prefix)
```

`TrimLeadingTrivia` repeatedly consumes whitespace and complete leading
comments, returns an empty string for trivia-only input, and does not strip
comments inside statements. Compile owns malformed-input errors.

- [ ] **Step 4: Run focused tests and formatting.**

```sh
gofmt -w internal/queryparams/scan.go internal/queryparams/scan_test.go internal/database/query_type.go internal/database/query_type_test.go
go test ./internal/queryparams ./internal/database -count=1
git diff --check
```

- [ ] **Step 5: Commit only these files.** Message:
`feat: recognize named SQL parameters without touching literals`.

## Task 2: Typed values and bound repository execution

**Files:** create `internal/queryparams/value.go`, `value_test.go`,
`internal/database/parameter.go`, `interbase_parameter_test.go`; modify
`internal/database/interbase_common.go`, `interbase_readonly_test.go`.

**Consumes:** Task 1 batch/occurrence order.
**Produces:** `Value`, `Convert`, `Bind`, and both optional repository interfaces.

- [ ] **Step 1: Write failing conversion and binding tests.**

```go
func TestBindPreservesIntegerPrecisionAndRepeatedValues(t *testing.T) {
    batch, err := Compile("SELECT :ID, :id, :EMPTY, :N FROM T", 3)
    if err != nil { t.Fatal(err) }
    args, err := Bind(batch, []Value{
        {Name: "id", Type: "integer", Value: "9007199254740993"},
        {Name: "EMPTY", Type: "text", Value: ""},
        {Name: "N", Type: "null", Value: ""},
    })
    if err != nil { t.Fatal(err) }
    want := []any{int64(9007199254740993), int64(9007199254740993), "", nil}
    if !reflect.DeepEqual(args[0], want) { t.Fatalf("got %#v", args[0]) }
}
```

Pin int64 min/max/overflow, negative and scientific floating point, rejection of
NaN/Inf/overflow and nondecimal syntax, leap dates, timestamp fractions,
timezones rejected, boolean validation, text apostrophes/newlines/leading zeros,
missing/extra/duplicate case-folded names, invalid type and nonempty NULL value.
Assert an invalid value in statement two returns no usable batch arguments.
Assert errors do not contain a distinctive secret-like invalid input string.

Extend the existing `txRecorder` under its mutex to record query strings,
`[]driver.NamedValue` copies and rows-close counts. Add an ExecContext recorder
returning `driver.RowsAffected(1)`. Test bound methods actually deliver positional
arguments to the SQL driver, rather than assert only a rewritten SQL string.
The read-only regression asserts options, exactly one rollback, no commit,
closed rows, and the same partial-result behavior on fetch errors.

- [ ] **Step 2: Observe failures.**

```sh
go test ./internal/queryparams ./internal/database -run 'TestBind|TestConvert|TestInterBase.*Params' -count=1
```

- [ ] **Step 3: Implement validation and forwarding.** Canonicalize/validate
all names, convert every value, then build every statement's argument slice.
Only after these stages succeed may Bind return executable arguments.

```go
func (db *InterBaseDBRepository) ExecParams(ctx context.Context, query string, args []any) (sql.Result, error) {
    if db == nil || db.Conn == nil { return nil, errors.New("interbase: database connection is nil") }
    return db.Conn.ExecContext(ctx, query, args...)
}
func (db *InterBaseDBRepository) QueryParams(ctx context.Context, query string, args []any) (*sql.Rows, error) {
    if db == nil || db.Conn == nil { return nil, errors.New("interbase: database connection is nil") }
    return db.Conn.QueryContext(ctx, query, args...)
}
```

Make legacy Exec/Query call these with nil arguments. Move the existing
QueryReadOnly body into QueryReadOnlyParams; change its tx.QueryContext call to
forward `args...`, and have QueryReadOnly delegate with nil. Keep its exact
read-only/read-committed options, defers and ScanRowsWithTypes call. These files
remain untagged: they import database/sql, not interbase-go. Keep the plain
MockDBRepository free of the new capabilities.

- [ ] **Step 4: Verify.**

```sh
go test ./internal/queryparams ./internal/database -count=1
go test -race ./internal/database -run 'TestInterBase.*(ReadOnly|Params)' -count=1
CGO_ENABLED=1 go test -tags interbase ./internal/database -count=1
```

- [ ] **Step 5: Commit owned files.** Message:
`feat: bind typed InterBase query arguments through optional capabilities`.

## Task 3: Stateless parameter discovery and stale-submission identity

**Files:** create `internal/lsp/query_parameters.go`,
`internal/handler/query_parameters.go`, `query_parameters_test.go`,
`query_parameters_fixture_test.go`; modify `internal/lsp/lsp.go`,
`internal/handler/handler.go`, `execute_command.go`.

**Consumes:** Compile, wire types and hashing contract above.
**Produces:** `CommandGetQueryParameters = "getQueryParameters"`;
`(*Server).getQueryParameters(ctx context.Context, params lsp.ExecuteCommandParams) (interface{}, error)`;
`(*Server).parameterSelection(params lsp.ExecuteCommandParams) (parameterSelection, error)`.

```go
type parameterSelection struct {
    Text string
    Context lsp.QueryParameterContext
    Variant dialect.DriverVariant
}
```

The selection helper requires the caller to hold connMu.RLock, copies text and
identity scalars under stateMu, releases stateMu, then hashes/extracts/compiles.
Use existing UTF-16 `extractRangeText`; reject invalid/reversed/out-of-document
coordinates before calling its clamping implementation. Do not retain `*File`
or hash a mutable config map after releasing the lock.

- [ ] **Step 1: Write discovery regressions using direct Server state.**

```go
func TestParameterDiscoveryDoesNotNeedCatalogOrDatabaseIO(t *testing.T) {
    s := NewServer()
    defer s.worker.Stop()
    s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
    s.curDBCfg = &database.DBConfig{Driver: dialect.DatabaseDriverInterBase, Alias: "nrf01", Host: "test", Path: "db.ib"}
    s.connGeneration = 7
    s.files["file:///query.sql"] = &File{Text: "SELECT :ID, :id FROM T"}
    result, err := s.getQueryParameters(context.Background(), lsp.ExecuteCommandParams{
        Command: CommandGetQueryParameters, Arguments: []interface{}{"file:///query.sql"},
    })
    if err != nil { t.Fatal(err) }
    got := result.(lsp.QueryParameterDiscovery)
    if !got.Supported || len(got.Parameters) != 1 || got.ConnectionGeneration != 7 { t.Fatalf("%+v", got) }
}
```

The nil SQL pool makes unintended DB access fail. Add identity cases: changed
alias/host/path/user/role/database produces a different connection key; password
changes do not enter the key; changing generation preserves connectionKey;
different dialect changes queryKey; same selected text in another document
shares queryKey but documentKey follows full document content. Non-InterBase
discovery returns supported=false and no parameters. Invalid URI/range fails.

- [ ] **Step 2: Run failures.**

```sh
go test ./internal/handler -run 'TestParameterDiscovery|TestParameterIdentity' -count=1
```

- [ ] **Step 3: Implement wire structs, helper and dispatch.**

```go
// dispatchCommand:
case CommandGetQueryParameters:
    return s.getQueryParameters(ctx, params)
// handleInitialize result capabilities:
ExecuteCommandProvider: &lsp.ExecuteCommandOptions{Commands: []string{
    CommandExecuteQuery, CommandExplainQuery, CommandGetQueryParameters,
    CommandShowDatabases, CommandShowSchemas, CommandShowConnections,
    CommandSwitchDatabase, CommandSwitchConnection, CommandShowTables,
}},
```

Version is always 1. Return nonnil empty `parameters` arrays. No-parameter
InterBase selections still return complete identity so the client can choose
the legacy execution flow. No server-side token/value cache is added.

- [ ] **Step 4: Verify direct and JSON-RPC discovery.** Extend the existing
`newTestContext` pattern for one call through `workspace/executeCommand` so
field tags/dispatch are exercised. Register a distinct test-only factory name
`stub-query-parameters` once; do not replace the production InterBase factory.
Its DBConnection reports InterBase while its repository is a recording fixture.
Use a mutex-protected recorder, copied argument slices and channel gates for
later stale/routing tests; do not introduce a production injection hook.

```sh
go test ./internal/handler ./internal/lsp -count=1
go test -race ./internal/handler -run 'TestParameterDiscovery|TestParameterIdentity' -count=1
```

- [ ] **Step 5: Commit.** Message:
`feat: discover query parameters with connection and selection identity`.

## Task 4: Bound batch execution and prepare-only Explain

**Files:** modify `internal/handler/query_parameters.go`, its tests/fixture,
`execute_command.go`, `explain.go`; add
`internal/handler/query_parameters_execution_test.go`.

**Consumes:** all Task 1–3 contracts.
**Produces:** parameter-aware executeQuery; existing execution helpers accept
optional variadic arguments, so all their existing zero-argument call sites
remain source-compatible:

```go
func (s *Server) runStatement(ctx context.Context, query string, vertical bool, args ...any) (string, error)
func (s *Server) runRoutedStatement(ctx context.Context, query string, vertical bool, routing procedureRouting, args ...any) (string, error)
func (s *Server) query(ctx context.Context, query string, vertical bool, args ...any) (string, error)
func (s *Server) queryProcedure(ctx context.Context, query string, vertical bool, args ...any) (string, error)
func (s *Server) renderQuery(ctx context.Context, query string, vertical, allowReadOnly bool, notes []string, args ...any) (string, error)
func (s *Server) queryResult(ctx context.Context, query string, allowReadOnly bool, args ...any) (*database.QueryResult, error)
func (s *Server) exec(ctx context.Context, query string, vertical bool, args ...any) (string, error)
```

- [ ] **Step 1: Write failing execution/race regressions.** Use the isolated
factory from Task 3 with a `parameterBackend` recording query/exec/read-only
calls and arguments. Its exact test-facing methods are
`calls() []parameterCall` and `gate(query string) *stubGate`; `parameterCall`
contains `SQL string`, `Args []any`, `Route string` (`query`, `readonly`, `exec`).
Use real database/sql fixture rows for output and existing ScanRowsWithTypes.

```go
// Core assertion after discovery and submission of two equal names:
calls := backend.calls()
if len(calls) != 1 || calls[0].Route != "readonly" {
    t.Fatalf("calls = %#v", calls)
}
if !reflect.DeepEqual(calls[0].Args, []any{"000123", "000123"}) {
    t.Fatalf("arguments = %#v", calls[0].Args)
}
```

Cover: selected statement only; a batch with missing/invalid values in its last
statement causes zero calls; unsupported capability anywhere causes zero calls;
stale document/query/connection/generation and bad protocol version cause zero
calls; pointer-null values still select the bound path; repeated case-folded
names across statements; comments before SELECT; read-only fetch errors retain
partial results; native cancellation notice remains authoritative. Use cached
procedure descriptors for one output and zero output: assert query versus Exec
and exactly one call, including errors/unknown procedure fallback.

- [ ] **Step 2: Run failures, including a blocked-call race test.**

```sh
go test ./internal/handler -run 'TestParameterExecution|TestParameterSubmission|TestParameterExplain' -count=1
```

- [ ] **Step 3: Extend the existing execution path, not a second renderer.**
While holding connMu.RLock, recompute selection context and compare every
submission field. Compile the whole selection, Bind all values, and validate
each route's required capability before the first statement. Resolve procedure
routing once per statement during preflight and retain that decision for its
execution; do not redo it against a potentially replaced worker cache.

Move runStatement's current routing/dispatch body into runRoutedStatement,
consuming the passed routing decision instead of recomputing it. Legacy
runStatement computes the decision once and delegates; the parameterized batch
executes runRoutedStatement with its saved preflight decision. This avoids a
second lookup after preflight while sharing the existing result/error path.

For a named-parameter statement, forward its exact rewritten SQL and ordered
arguments. For a statement with no arguments, retain the legacy repository
method. Reuse the existing execution loop's cancellation/result behavior.
The SQL snapshot accepted at submission is the execution input; later document
edits do not mutate it. Keep connMu held through execution to prevent reconnects.

```go
// Bound read selection inside queryResult; repo is the resolved repository:
if len(args) > 0 {
    if allowReadOnly {
        if readOnly, ok := repo.(database.ParameterizedReadOnlyQuerier); ok {
            return readOnly.QueryReadOnlyParams(ctx, query, args)
        }
        if _, ok := repo.(database.ReadOnlyQuerier); ok {
            return nil, errors.New("parameterized read-only queries are not supported by this repository")
        }
    }
    bound, ok := repo.(database.ParameterizedRepository)
    if !ok { return nil, errors.New("bound parameters are not supported by this repository") }
    rows, err := bound.QueryParams(ctx, query, args)
    if err != nil { return nil, err }
    defer rows.Close()
    return database.ScanRowsWithTypes(rows, database.RenderOptionsFor(repo.Driver()))
}
```

Use the analogous ExecParams branch in `exec`. Legacy callers with no envelope
continue using the existing parsing path. On an InterBase legacy request with
genuine named markers, return a clear missing-values/client-extension error
before execution, rather than send raw named SQL to the driver. Preserve the
normal unparameterized PSQL/DDL path for legacy requests; limit that legacy
named-marker guard to the supported top-level statement forms. The new discovery
flow explicitly refuses PSQL with variable markers, as specified for this
feature; it must never treat local PSQL variables as prompted inputs. There is
no general parser replacement.

For Explain, translate genuine named markers with Compile but do not Bind or
prompt. Feed rewritten statements through existing `explainStatements`, whose
prepare-only refusals and output stay authoritative. Zero-marker Explain takes
its existing path. Add a recorder assertion that Explain never calls Query/Exec.

- [ ] **Step 4: Run regression gates.**

```sh
go test ./... -count=1
GOFLAGS=-count=1 make test-race
CGO_ENABLED=1 go test -tags interbase ./... -count=1
```

- [ ] **Step 5: Commit.** Message:
`feat: execute parameterized batches with stale-context checks`.

## Task 5: Neovim prompts, per-query prefills and command integration

**Files:** in the configurations worktree create
`Both/Neovim/nvim/lua/sqls_parameters.lua` and
`Both/Neovim/nvim/tests/sqls_parameters_test.lua`; edit the seeded
`Both/Neovim/nvim/lua/plugins/lspconfig.lua` without staging user baseline hunks.

**Consumes:** protocol version 1 and command advertisement from Tasks 3–4.
**Produces:** public module API:

```lua
M.attach(client, bufnr)
M.execute(client_id, bufnr, opts) -- opts = { range?, vertical?, smods? }
M.code_action(command, context) -- context.bufnr, context.client_id, context.params.range
M.clear(client_id)
```

- [ ] **Step 1: Write failing headless tests for stateful behavior only.** Use
`nvim --headless -u NONE -l Both/Neovim/nvim/tests/sqls_parameters_test.lua` from
the configurations root. The runner sets package.path to the worktree module,
replaces vim.ui.input/select with callback queues, and supplies fake LSP clients
via vim.lsp.get_client_by_id. Restore every replacement after each test. The
fake records request methods/payloads and lets the test deliver responses in
chosen order. It does not open database connections or load the user's init.

```lua
-- The runner defines client_id, bufnr, fake, choose_type, enter_value,
-- answer_discovery and the module M; these helpers drive real callbacks.
M.execute(client_id, bufnr, {})
answer_discovery("nrf-key", "query-key", { { name = "EMPLYID", key = "EMPLYID" } })
choose_type("text")
enter_value("000123")
assert(fake.last_request().params.parameterValues.values[1].value == "000123")
M.execute(client_id, bufnr, {})
answer_discovery("nrf-key", "query-key", { { name = "EMPLYID", key = "EMPLYID" } })
choose_type("text")
assert(fake.last_input.default == "000123")
enter_value(nil)
assert(fake.execution_count() == 1) -- Escape did not send a second execution
```

Define runner helpers locally: answer_discovery invokes the queued discovery
callback with the version-1 response; choose_type selects the matching item in
the queued type selector; enter_value invokes the queued input callback; fake
request records cloned payloads, callbacks and request IDs, and returns true/id.
`fake.execution_count()` counts only command=executeQuery requests.

Required cases: repeat names once; same query on centrale starts empty;
switching back recovers NRF01; changed query does not prefill; same query in a
second buffer does; cached type is first choice; text `NULL` versus typed NULL;
int64 text is not tonumber-converted; Escape at first/last prompt; invalid value
re-prompt; context buffer changed/closed/client stopped during prompts; one
prompt flow per client at a time; clear/stop abort pending callbacks and remove
cache; 101 entries evict the least recently used; all-cancelled prompt sequences
leave previous complete values intact. Exercise selected-range and code-action
payloads and the real preview handler using a temporary Neovim buffer.

- [ ] **Step 2: Observe test failures.**

```sh
nvim --headless -u NONE -l Both/Neovim/nvim/tests/sqls_parameters_test.lua
```

- [ ] **Step 3: Implement one asynchronous adapter.** Check command
advertisement first; if the server lacks getQueryParameters, use legacy
execution. A supported server returns supported=false for other drivers, also
taking legacy execution. Never retry an execution request through another path.

Capture bufnr, URI, changedtick, range and display options when invoked; never
use the current buffer after a prompt changes focus. Reject changed/closed
buffers before submission; the server remains the authority for connection
identity. Supply bufnr explicitly to client:request so didChange is flushed.

Use a per-client cache table, monotonically increasing usage counter and LRU
eviction. Key entries by JSON encoding `{ connectionKey, queryKey }`, not string
concatenation with a separator. Deep-copy cached entries into a prompt draft;
commit the entire draft only after every prompt completes and validation passes.
Use a per-client operation epoch to make old callbacks inert after clear/stop.

```lua
local discovery_params = {
  command = "getQueryParameters",
  arguments = { uri },
  range = opts.range,
}
-- Final request payload, built only after all prompts complete:
local execution_params = {
  command = "executeQuery",
  arguments = { uri, opts.vertical and "-show-vertical" or "" },
  range = opts.range,
  parameterValues = {
    version = discovery.version,
    connectionKey = discovery.connectionKey,
    connectionGeneration = discovery.connectionGeneration,
    queryKey = discovery.queryKey,
    documentKey = discovery.documentKey,
    values = draft,
  },
}
```

Implement the continuation as named local functions, one for type selection,
one for value input, one for advance/finish; errors and nil input return without
execution. The type choices map exactly to the seven wire strings. The prior
type is first and Text is initially first. Numeric input stays a string. Local
checks catch obvious invalid input; server validation is authoritative, with no
partial batch execution. Exact int64 range checking must use decimal-string
comparison or server validation, never Lua number arithmetic.

The plugin's result-renderer function is private. Implement a small owned
preview helper matching its public UI convention: temp buffer ending in
`.sqls_output`, split rendered string into buffer lines, `pedit` using smods,
set filetype `sqls_output`. Do not monkey-patch client.request or vendor code.
This preserves the installed plugin's preview/filetype experience without
depending on a private function. Handle nil result without creating a window.

- [ ] **Step 4: Wire existing entry points without duplicating the workflow.**

```lua
-- Inside the existing sqls LspAttach block:
require("sqls_parameters").attach(client, bufnr)
-- Inside vim.lsp.config("sqls", ...):
commands = {
  executeQuery = function(command, context)
    require("sqls_parameters").code_action(command, context)
  end,
  explainQuery = function(_, context)
    require("sqls.commands").exec(context.client_id, "explainQuery")
  end,
},
on_exit = function(_, _, client_id)
  require("sqls_parameters").clear(client_id)
end,
```

Preserve any existing on_exit callback if encountered. attach schedules its
buffer-command overrides after vendor on_attach has completed. Replace
SqlsExecuteQuery/SqlsExecuteQueryVertical through nvim_buf_create_user_command
with `force=true`, preserving range and smods. Add SqlsClearParameters. Existing
Space S mappings already target these commands and stay intact. Code actions
use context.params.range when available; never reconstruct a past visual range
from the current cursor. Test supplied URI against the captured buffer.
Explain uses its existing command/keys and does not prompt.

- [ ] **Step 5: Verify and commit owned module/tests.**

```sh
nvim --headless -u NONE -l Both/Neovim/nvim/tests/sqls_parameters_test.lua
stylua --check Both/Neovim/nvim/lua/sqls_parameters.lua Both/Neovim/nvim/tests/sqls_parameters_test.lua Both/Neovim/nvim/lua/plugins/lspconfig.lua
git diff --check
git add Both/Neovim/nvim/lua/sqls_parameters.lua Both/Neovim/nvim/tests/sqls_parameters_test.lua
git commit -m "feat(nvim): prompt and remember sqls query parameters"
```

Keep wiring diff and its baseline in the coordinator review package; it is not
excluded from review just because it remains unstaged to protect user work.

## Task 6: Document, integrate and verify the installed workflow

**Files:** modify sqls `README.md`, `doc/develop.md`; deploy the reviewed Neovim
module/wiring; update local `~/.local/share/sqls/NEOVIM-INTERBASE.md` and add
`~/.local/share/sqls/interbase-parameters.sql` without credentials.

**Consumes:** reviewed Task 1–5 commits and the uncommitted wiring delta.
**Produces:** verified local binary/client integration and documented limitations.

- [ ] **Step 1: Update docs with actual protocol and UI.** Replace the README
statement that execute does not prompt. Document named grammar, types, repeated
names, selection scope, in-memory lifetime/LRU, clear command, cancellation,
stale selection rejection, and exact decimal text-plus-CAST guidance. Keep
catalog-startup latency as an existing independent limitation. Update developer
docs with optional interfaces and the version-1 discovery/submission fields.

Use this parameterized SELECT-only smoke file:

```sql
SELECT CAST(:EMPLYID AS VARCHAR(30)) AS EMPLYID,
       CAST(:emplyid AS VARCHAR(30)) AS SAME_ID
FROM RDB$DATABASE;

SELECT CAST(:EMPTY_VALUE AS VARCHAR(30)) AS EMPTY_VALUE,
       CAST(:NULL_VALUE AS VARCHAR(30)) AS NULL_VALUE,
       CAST(:BIG_ID AS NUMERIC(18,0)) AS BIG_ID
FROM RDB$DATABASE;
```

Run BIG_ID only on a dialect that supports its declared type; a separate
VARCHAR CAST checks lossless large-integer transfer on Dialect 1. Do not force
a connection dialect to make a test pass.

- [ ] **Step 2: Run final offline gates and independent review.**

```sh
go build ./...
go vet ./...
go test ./... -count=1
GOFLAGS=-count=1 make test-race
CGO_ENABLED=1 go build -tags interbase ./...
CGO_ENABLED=1 go vet -tags interbase ./...
CGO_ENABLED=1 go test -tags interbase ./... -count=1
```

Run the retained Lua behavioral test separately in the configurations worktree.
Review the full server range from `0912dfe`, the new client module/tests, and the
saved baseline-to-wiring diff. Do not claim existing live tests ran if skipped.

- [ ] **Step 3: Build a staging binary and smoke-test with the reviewed adapter.**

```sh
CGO_ENABLED=1 go build -tags interbase -o /tmp/opencode/sqls-parameters .
```

Use the existing private configuration via `--config`, never copy credentials
into the tests or plan. A temporary headless Neovim session loads the worktree
adapter and simulates this user's UIEnter event before waiting for sqls.
Allow up to 180 seconds for the observed NRF01 catalog startup. Drive prompt
responses for text `000123`, quoted text `O'Brien`, empty text, NULL and integer
`9007199254740993`; assert the rendered cells, not merely result headings.
Run twice to observe actual prefilled defaults, cancel a later prompt and assert
no execute request, switch centrale and verify empty defaults, then switch back
and verify NRF01 prefills return. Select a single statement and test Explain
without any input prompt. Exercise the real result-preview command.

- [ ] **Step 4: Integrate locally after clean review/checks.** Preserve the
current binary/config and live wiring baseline. Fast-forward local branches
where authorized and clean, apply only the reviewed wiring delta, and atomically
replace `~/gits/sqls/sqls` with the tested binary. Keep credentials private,
preserve unrelated dirty dotfiles, and do not push. Run one installed Neovim
smoke check and record exact observed results/remaining limitations.

- [ ] **Step 5: Commit only the server documentation and report final state.**
Message: `docs: explain named parameter prompts and binding protocol`.
Leave unrelated existing configuration changes unstaged. Report the installed
binary, client module, clear command, keys, live checks, and any deferred issues.

## Plan self-review checklist

- [x] Spec recognition/rewrite and leading comments → Task 1.
- [x] Typed binding, int64 precision and read-only lifetime → Task 2.
- [x] Stateless discovery, stable connection/query keys, stale context → Task 3.
- [x] Whole-batch validation, once-only routing, result/cancellation and Explain → Task 4.
- [x] Prefilled types/values, isolation, cancellation, 100-entry LRU and command paths → Task 5.
- [x] User documentation, direct SELECT-only verification and local install → Task 6.
- [x] Shared file/interface boundaries identified; no concurrent shared-file implementers.
- [x] Names/signatures in task consumers match the cross-task contracts.
- [x] Neovim's private renderer and existing dirty configuration have explicit integration handling.

Execution method already requested by the user: coordinator-led specialist
delegation with independent review, Anthropic models explicitly selected.
Await user review of this plan before starting implementation.
