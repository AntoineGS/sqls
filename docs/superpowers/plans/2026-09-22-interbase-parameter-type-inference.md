# InterBase Input-Parameter Type Inference Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ask for a value directly when InterBase describes a named input's type, keep the picker when it cannot, and toggle SQL NULL with Ctrl-T in the Snacks value prompt.

**Architecture:** Expose copied input SQLDA metadata from prepare-only `interbase-go`; adapt the driver descriptors to a small optional sqls repository interface. Discovery aligns positional descriptors with compiled named-marker occurrences and returns optional suggestions; the Neovim adapter consumes them and supplies the existing typed submission format.

**Tech Stack:** Go, C/cgo, InterBase DSQL SQLDA, Neovim Lua, Snacks input, existing sqls JSON-RPC protocol.

**Spec:** `docs/superpowers/specs/2026-09-22-interbase-parameter-type-inference-design.md`

## Global Constraints

- Work across three checkouts: `~/gits/interbase-go` (driver), this sqls worktree (the `go.mod` replacement points at `../interbase-go`), and `~/gits/ibwt/sqls-params/configurations` (Neovim). At execution time use `using-git-worktrees` for any additional checkout; never overwrite unrelated changes, including that configurations checkout's `plugins/lspconfig.lua` edit.
- If the driver is developed in another linked worktree, use a temporary `/tmp/opencode/sqls-input-types/go.work` with `use` entries for sqls and the driver and a workspace `replace interbase-go =>` that driver checkout. Set `GOWORK` to the absolute work-file path for tagged tests/builds; do not commit machine-local module paths or change the existing `../interbase-go` symlink. From the driver worktree set `DRIVER_WORKTREE=$PWD`; from this sqls worktree set `SQLS_WORKTREE=$PWD`; in `/tmp/opencode/sqls-input-types` run `go work init "$SQLS_WORKTREE" "$DRIVER_WORKTREE"` then `go work edit -replace="interbase-go=$DRIVER_WORKTREE"`.
- Prepare-only metadata inspection must never execute SQL or create an attachment separate from the supplied `*sql.Conn`; obey cancellation and close statements on success and failure.
- Only InterBase discovers types. Keep the version-1 JSON-RPC discovery and submission protocol compatible with existing clients and sqls's current stale-context checks.
- A suggestion is emitted only when all occurrences of a name have compatible descriptors; missing or conflicting information falls back to the picker.
- Decimal values with scale must remain exact strings, not `float64`. Keep `NULL` distinct from empty text.
- Ctrl-T belongs to sqls Snacks value prompts only; other input providers retain the type picker and its `null` option.

## Review Focus

1. One `:ID` used as both integer and text across statements must use the picker; Task 3 tests the conflict.
2. `FOR SELECT ... WHERE ID = :id INTO :output DO` must describe only `:id` after normalization; Task 3 tests this selected fragment.
3. `NUMERIC(18,2)` values beyond binary float precision must reach the driver unchanged as strings; Tasks 3 and 4 test this.
4. A canceled metadata request must not produce a prompt response; Task 3 tests cancellation separately from ordinary prepare errors.
5. If Snacks is unavailable, `NULL` must still be accessible via the old picker; Task 4 tests this fallback.

## File map and interfaces

- Driver: `~/gits/interbase-go/native.h`, `native.c`, `native.go` expose a bounded accessor for the prepared statement's input SQLDA, copying fields into `InputDescriptor` values. `introspection.go` adds `DescribeInputs(ctx context.Context, conn *sql.Conn, query string) ([]InputDescriptor, error)` and a separate connection-local `InputDescriber` interface; `introspection_test.go` and `integration/introspection_test.go` verify it.
- sqls: `internal/database/parameter.go` owns `InputDescriptor{Kind string; Subtype, Scale, Precision int; Nullable bool}` and the optional `InputDescriber` repository interface. `internal/database/interbase_native.go` implements that interface using the pooled driver helper; `internal/database/interbase_parameter_test.go` tests adaptation. `internal/queryparams/scan.go` adds optional `InferredType` and `DatabaseType` to `Parameter`; `internal/handler/parameter_type_inference.go` maps descriptors to wire types and occurrence keys; `internal/handler/query_parameters.go` calls it during discovery. `internal/handler/parameter_type_inference_test.go` and `query_parameters_test.go` exercise matching and fallback.
- Configurations: `Both/Neovim/nvim/lua/sqls_parameters.lua` consumes the optional fields and scopes the Snacks Ctrl-T action; `Both/Neovim/nvim/tests/sqls_parameters_test.lua` covers real adapter callbacks with fake LSP and UI.

---

### Task 1: Expose prepared input descriptors in interbase-go

**Files:**
- Modify: `~/gits/interbase-go/native.h`, `native.c`, `native.go`, `introspection.go`
- Test: `~/gits/interbase-go/introspection_test.go`, `integration/introspection_test.go`
- Test (native accessor if the C fixture is used): `~/gits/interbase-go/tests/native_prepared_test.c`

**Interfaces:**
- Consumes: existing `(*nativeConnection).prepare`, `(*nativeStatement).close`, `(*conn).Plan`, `Plan(ctx, *sql.Conn, query)` lifecycle patterns.
- Produces: `type InputDescriptor struct { Kind string; Subtype, Scale, Precision int; Nullable bool }`; `type InputDescriber interface { DescribeInputs(context.Context, string) ([]InputDescriptor, error) }`; and `DescribeInputs(context.Context, *sql.Conn, string) ([]InputDescriptor, error)`. `Kind` is the stable database type family (`CHAR`, `VARCHAR`, `SMALLINT`, `INTEGER`, `BIGINT`, `FLOAT`, `DOUBLE PRECISION`, `DATE`, `TIME`, `TIMESTAMP`, `BOOLEAN`, `BLOB`, `ARRAY`, or `UNKNOWN`). Preserve SQLDA position and copy all fields before closing the statement.

- [ ] **Step 1: Write failing lifecycle and data tests.** Add tests mirroring `TestConnPlanReturnsPlanAndClosesTheStatementExactlyOnce`: a native prepare override returns two descriptors, an `execOverride` fails the test if called, and `closeOverride` increments a counter. Assert exact descriptor order and one close. Add tests for zero inputs, descriptor read plus close both failing (`errors.Is` on both), bad/closed connection, and canceled context without invoking prepare. Add a live SELECT with e.g. `WHERE ID = ? AND COUNTRY = ?`, assert reported `INTEGER`/`VARCHAR`, and verify a read-only DML prepare does not insert any row. Example red assertion:

  ```go
  got, err := connection.DescribeInputs(context.Background(),
      "SELECT ID FROM GO_COUNTRY WHERE ID = ? AND COUNTRY = ?")
  if err != nil { t.Fatal(err) }
  if len(got) != 2 || got[0].Kind != "INTEGER" || got[1].Kind != "VARCHAR" {
      t.Fatalf("descriptors = %+v", got)
  }
  ```

- [ ] **Step 2: Run red.** From the driver checkout run `go test ./... -run 'Test(ConnDescribeInputs|NativeStatementInputDescriptors)' -count=1`; expect compile errors for `DescribeInputs` and `InputDescriptor`.
- [ ] **Step 3: Add the smallest native accessor and Go API.** In `native.h` introduce `ib_input_metadata` with `sql_type`, `sql_subtype`, `sql_scale`, `sql_precision`, `nullable` and `ib_statement_input_metadata(statement,index,&out,&error)`. In `native.c` validate `statement->input`, `index < sqld`, take `sql_type & ~1`, and copy only numeric SQLDA fields; return an error on invalid input. In `native.go`, make `(*nativeStatement).inputDescriptors() ([]InputDescriptor,error)` use `ib_statement_num_input` and the accessor under `nativegate.Global`; normalize C SQL type codes into `Kind` and include an override hook for unit tests. In `introspection.go`, add the pooled wrapper and connection method using `Plan`'s validation, prepare, cancellation/error classification, statement close, and connection invalidation patterns; factor a private prepare/inspect helper with `Plan` if needed to avoid divergent cleanup.

  ```go
  type InputDescriptor struct {
      Kind string
      Subtype, Scale, Precision int
      Nullable bool
  }
  type InputDescriber interface {
      DescribeInputs(ctx context.Context, query string) ([]InputDescriptor, error)
  }
  ```

- [ ] **Step 4: Run green and check the native fixture.** Run `go test ./... -run 'Test(ConnDescribeInputs|NativeStatementInputDescriptors)' -count=1`; run `make test-native` if `tests/native_prepared_test.c` was changed. Run the live `integration/introspection_test.go` with the existing InterBase fixture, and verify no row was written. If no live fixture is configured, use the same read-only NRF01 smoke connection used for sqls in Task 4 and report the integration test as pending until then.
- [ ] **Step 5: Commit driver work.** Stage only the driver files above; `git diff --cached --check`; commit `feat(interbase): expose prepared input descriptors`. Record the driver commit ID for Task 2.

### Task 2: Adapt driver metadata to a sqls repository capability

**Files:**
- Modify: `internal/database/parameter.go`, `internal/database/interbase_native.go`
- Test: `internal/database/interbase_parameter_test.go`, create `internal/database/interbase_input_native_test.go` (`interbase && cgo && linux && amd64`)

**Interfaces:**
- Consumes: Task 1's `interbase.DescribeInputs` and `interbase.InputDescriptor`.
- Produces: `database.InputDescriptor` with matching fields and `database.InputDescriber { DescribeInputs(context.Context, string) ([]InputDescriptor,error) }`, implemented only by the native InterBase repository. A stub or other driver without that capability stays unchanged.

- [ ] **Step 1: Write failing adapter tests.** In the new tagged test file, test the exact mapping from a driver's `InputDescriptor{Kind:"INTEGER",Scale:0}` to `database.InputDescriptor` using a private conversion helper; assert that nullability and scale survive. In the untagged test, verify a repository without native capability does not gain it. Example:

  ```go
  got := inputDescriptorFromDriver(interbase.InputDescriptor{
      Kind: "BIGINT", Subtype: 2, Scale: -2, Precision: 18, Nullable: true,
  })
  if got.Kind != "BIGINT" || got.Scale != -2 || got.Precision != 18 || !got.Nullable {
      t.Fatalf("input descriptor = %+v", got)
  }
  ```

- [ ] **Step 2: Run red.** Run `CGO_ENABLED=1 CGO_CFLAGS='-I/opt/interbase/include' CGO_LDFLAGS='-L/opt/interbase/lib -Wl,-rpath,/opt/interbase/lib -lgds' go test -tags interbase ./internal/database -run 'TestInterBaseInputDescriptor' -count=1` with `GOWORK` pointing at the temporary work file if needed; expect missing helper/interface.
- [ ] **Step 3: Implement the optional adapter.** Add the public repository interface and plain descriptor in `parameter.go` (untagged build). In `interbase_native.go`, acquire `db.Conn.Conn(ctx)`, defer Close, call `interbase.DescribeInputs(ctx,conn,query)`, and copy the returned slice into database descriptors; propagate errors to the caller to decide fallback. Keep the existing `ExplainPlan` unchanged.
- [ ] **Step 4: Run green and both build modes.** Run the tagged targeted test, `go test ./internal/database`, and the tagged `go test -tags interbase ./internal/database` with the same CGO flags. No stubs should inadvertently implement `InputDescriber`.
- [ ] **Step 5: Commit sqls adapter.** Stage only Task 2 files and commit `feat(interbase): adapt input descriptors for sqls`.

### Task 3: Infer discovery types without changing execution

**Files:**
- Modify: `internal/queryparams/scan.go`, `internal/handler/query_parameters.go`
- Create: `internal/handler/parameter_type_inference.go`, `internal/handler/parameter_type_inference_test.go`
- Test: `internal/handler/query_parameters_test.go`, `internal/handler/interbase_select_execution_test.go`, `internal/handler/query_parameters_fixture_test.go` only where fixture capability is needed

**Interfaces:**
- Consumes: `database.InputDescriber`, `database.InputDescriptor`, `queryparams.Batch.Statements[i].Keys`, `ExecutableSelects`.
- Produces: optional JSON fields `inferredType` and `databaseType` on `queryparams.Parameter`; `inferParameterTypes(context.Context, queryparams.Batch, func(context.Context, string) ([]database.InputDescriptor,error), int) ([]queryparams.Parameter,error)` called only by InterBase discovery. Keep `Bind` and `preflightBoundBatch` input format unchanged.

- [ ] **Step 1: Add failing table tests for metadata mapping and matching.** Compile literal SQL and pass a fake describer returning literal positional descriptors. Cover: same name twice with compatible types, conflicts between text/integer across statements, count mismatch, failed prepare on one statement, no parameters (zero calls), exact scaled decimal (`NUMERIC(18,2)` selects `text` wire type and preserves `"9007199254740993.25"` through `queryparams.Bind`), OCTETS and unsupported TIME fallback, and a normalized `FOR SELECT ... :id INTO :output DO` with only `id` inferred. Example:

  ```go
  batch, err := queryparams.Compile("SELECT :id FROM T; SELECT :ID FROM U", 3)
  if err != nil { t.Fatal(err) }
  result, err := inferParameterTypes(context.Background(), batch,
      func(_ context.Context, query string) ([]database.InputDescriptor, error) {
          if strings.Contains(query, "FROM T") {
              return []database.InputDescriptor{{Kind: "INTEGER"}}, nil
          }
          return []database.InputDescriptor{{Kind: "VARCHAR"}}, nil
      }, 3)
  if err != nil { t.Fatal(err) }
  if len(result) != 1 || result[0].InferredType != "" {
      t.Fatalf("conflicting descriptor should use picker: %+v", result)
  }
  ```

- [ ] **Step 2: Run red.** `go test ./internal/handler -run 'TestInferParameterTypes' -count=1`; expect missing fields and helper.
- [ ] **Step 3: Implement pure inference.** Add the optional JSON fields to `queryparams.Parameter`. Map known `Kind` families to existing wire types; reject binary text/OCTETS, arrays, TIME, unknown kinds, and uncertain dialect-1 decimals. For scaled exact integers with dialect 3, choose `text` and a label like `NUMERIC(18,2)` (or `DECIMAL(18,2)` for subtype 2); retain decimal string bytes in `Bind`. Match occurrence keys to ordered descriptors, require exact count, and mark a name uncertain if any occurrence conflicts or any statement using it failed. Produce a fresh parameter slice; never mutate batch SQL or global state.
- [ ] **Step 4: Test discovery integration and cancellation red/green.** Extend the parameter fixture with an **opt-in distinct repository wrapper** for `database.InputDescriber` so existing tests that assert no discovery I/O keep their original repository shape. For a connected InterBase fixture, assert discovery contains optional inferred fields; a nil/unavailable connection or preparation error yields the existing names and empty suggestions; an already-canceled context returns `context.Canceled`; an output target is never described. Run these tests first and observe their failure. Integrate the helper in `getQueryParameters`: do not acquire a repository unless named inputs exist; use a three-second per-statement context timeout derived from the request and fall back on its deadline, but propagate parent request cancellation. Re-run the targeted tests until green.

  ```go
  if ctx.Err() != nil { return nil, ctx.Err() }
  // For a statement-local deadline, leave that statement's parameter
  // suggestions empty; do not hide a canceled parent request.
  ```

- [ ] **Step 5: Verify execution and protocol compatibility.** Run `go test ./internal/handler ./internal/queryparams -count=1`; run `go test ./...` and the full `go test -tags interbase ./...` with the Task 2 CGO flags. Assert a legacy JSON discovery response omits new fields and bound execution still accepts `type:"text"` for exact decimal and `type:"null"` for NULL.
- [ ] **Step 6: Commit discovery.** Stage only Task 3 files and commit `feat(interbase): infer named query parameter types`.

### Task 4: Make Neovim prompt values directly with Ctrl-T NULL

**Files:**
- Modify: `~/gits/ibwt/sqls-params/configurations/Both/Neovim/nvim/lua/sqls_parameters.lua`
- Test: `~/gits/ibwt/sqls-params/configurations/Both/Neovim/nvim/tests/sqls_parameters_test.lua`
- Smoke fixture: `/tmp/opencode/sqls-input-types/` (staged script and logs only; not a tracked repository)

**Interfaces:**
- Consumes: Task 3 discovery `parameters[i].inferredType`, `parameters[i].databaseType`, and existing `version=1` context.
- Produces: existing `{name,type,value}` submission unchanged, with `type="null",value=""` on Ctrl-T NULL; no editor buffer edit.

- [ ] **Step 1: Add failing headless prompt tests.** In the existing fake UI/LSP queue, set `_G.Snacks = { input = { input = vim.ui.input } }` after installing the fake UI, and restore the old `_G.Snacks` on teardown. Provide `{name="id",key="ID",inferredType="integer",databaseType="INTEGER"}`; assert the first interaction is `vim.ui.input` (zero selects), a valid integer confirms `{type="integer",value="12"}`, a stale remembered text type is not reused, and cancellation submits nothing. Exercise the per-popup `win.actions.toggle_null` twice, asserting NULL then restored typed text; assert that only sqls's popup has a `<c-t>` binding and the NULL state is visible via `set_title`. A remembered NULL must reopen toggled. For a non-Snacks provider (clear `_G.Snacks`) or absent/unknown suggestion, assert the type picker still offers `null`. Test inferred boolean's value input and validation (rather than the old boolean select).

  ```lua
  _G.Snacks = { input = { input = vim.ui.input } }
  answer_discovery("nrf01", "query-key", {
    { name = "id", key = "ID", inferredType = "integer", databaseType = "INTEGER" },
  })
  assert(#fake.selects == 0 and #fake.inputs == 1, "known type must open value input")
  fake.inputs[1].confirm("12")
  assert(fake.last_request().params.parameterValues.values[1].type == "integer")
  ```

- [ ] **Step 2: Run red.** From the configurations checkout run `nvim --headless -u NONE -l Both/Neovim/nvim/tests/sqls_parameters_test.lua`; expect the new direct-input test to fail because the picker still opens.
- [ ] **Step 3: Implement prompt branching and scoped Snacks action.** Branch on a recognized `inferredType` and `vim.ui.input == Snacks.input.input`; where Snacks is absent use the picker to preserve NULL. Use the existing validation and remembered-value logic; do not reuse a mismatched prior type's value. For known booleans, validate input as `true`/`false`. Pass `opts.win.actions.toggle_null` and `opts.win.keys` with insert-mode `<c-t>` to just the sqls input call; store the boolean NULL state in the prompt closure, call `win:set_title(...)` to show it, preserve text on both toggles, and on confirm advance with `"null", ""` while NULL is active. An inferred NULL remembered value starts toggled; cancel does not change the cache. Existing unknown-type and no-parameter flows stay intact.
- [ ] **Step 4: Run green and smoke.** Run the headless Lua test. Build sqls in its clean linked worktree with `-tags interbase` into `/tmp/opencode/sqls-input-types/sqls` using the temporary `GOWORK` if the driver has its own worktree; connect the staged binary to a read-only InterBase database, and inspect `getQueryParameters` for `WHERE typed_column = :name` plus a selected `FOR SELECT ... INTO` fragment. Exercise direct value prompts and Ctrl-T NULL with the configured Snacks UI; check actual bound results, legacy picker fallback, decimal precision, and the previous Neovim smoke. Do not install over `~/gits/sqls/sqls` without the user's integration choice.
- [ ] **Step 5: Commit configuration work and report verification.** Stage only the adapter and its headless test (leave the unrelated `plugins/lspconfig.lua` change untouched); `git diff --cached --check`; commit `feat(nvim): prompt for inferred InterBase values`. Record all three repository SHAs and the staged binary hash. Hand off local merge/install choice after the full checks are green; do not push without authorization.
