# Catalog-Backed Editor Surfaces Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the four surfaces an InterBase developer touches every minute — Explain SQL, completion, signature help and hover — aware of procedures, external functions, generators and views, backed by the real catalog and by the driver's prepare-only plan.

**Architecture:** One new driver-neutral rendering file, `internal/database/catalog_doc.go`, owns every markdown fragment built from a catalog descriptor, so the "omit what the catalog did not tell us" rules are written and tested exactly once and consumed by both the completer and the handler. `internal/handler/explain.go` adds an `explainQuery` command whose plan rendering is a free function over a `database.ExplainRepository`, and `internal/handler/interbase_hover.go` resolves the identifier under the cursor to a catalog object, renders the summary the pure hover cannot, and appends real DDL under a 3-second bound with a per-connection-generation memo. `internal/completer/interbase_candidates.go` adds candidate generators, and one entry in the parser's `multiKeywordMap` plus one new syntax position give `EXECUTE PROCEDURE` a completion context.

**Tech Stack:** Go 1.25.7, standard library only (`context`, `sort`, `strings`, `time`, `database/sql`), `github.com/sourcegraph/jsonrpc2@v0.2.1`, `github.com/google/go-cmp` in tests. Nothing in this plan imports `interbase-go`, and this plan adds no build-tagged file.

**Spec:** `docs/superpowers/specs/2026-09-19-interbase-editor-features-design.md` — this plan implements **Plan 3 of 4** from that spec's "Plan decomposition" section: features 1–4 (§1 Explain, §2 completion, §3 signature help, §4 hover), plus the editor-feature paragraphs of the README. Feature 5 (§5) and all of §6 belong to other plans and must **not** be built here.

**Builds on:**

- `docs/superpowers/plans/2026-09-19-server-concurrency-cancellation.md` (Plan 1), which must be complete before this plan starts. This plan consumes Plan 1's `Server.stateMu`, `Server.connMu`, `(*Server).fileText`, `cancellationNotice`, `cancelledError`, and `make test-race`.
- Sub-project 2's catalog work, for Tasks 3 and 5–8. Tasks 2 and 4 depend on nothing outside this repository. Task 1 opens with a precondition step that verifies the contract symbols exist and stops if they do not.

**Runs alongside:** Plan 4 (`docs/superpowers/plans/2026-09-19-interbase-definition-snapshots.md`) is being executed in parallel and touches `internal/handler/handler.go` and `README.md`. Two shared names are flagged in File Structure rather than claimed here. Plan 2 (`docs/superpowers/plans/2026-09-19-interbase-results-pane.md`) deliberately left the `"EXECUTE": {"PROCEDURE"}` entry in `multiKeywordMap` to this plan; Task 4 owns it.

## Global Constraints

Copied from the spec. Every task's requirements implicitly include this section.

- **Nothing this plan consumes from sub-project 2 exists yet.** `DDLRepository`, `ExplainRepository`, `ObjectKind`, `ErrObjectNotFound`, `ErrUnsupportedDDL`, `UnsupportedDDLDetail`, every descriptor type, and every `DBCache` catalog accessor are specified in `docs/superpowers/specs/2026-09-19-interbase-dialect-and-catalog-design.md` §4.4/§4.5 and are **not implemented**. Task 1 Step 1 verifies them and stops if they are missing.
- **Lock ordering: `connMu` before `stateMu`, never the reverse**, and `stateMu` is never held across any I/O. Inherited verbatim from Plan 1 and binding on every task here. **A 3-second `ObjectDDL` call is I/O.** The hover memo is read under `stateMu.RLock`, the lock is released, the catalog round trip happens, and the result is stored under `stateMu.Lock`.
- **Hover, completion and signature help stay on the inline dispatch path** (Plan 1: "Only `workspace/executeCommand` is dispatched asynchronously"). They take `stateMu` and never `connMu`. `explainQuery` is a `workspace/executeCommand` command, so it runs async and takes `connMu.RLock()` for the whole of its database work, exactly like `executeQuery`.
- **Hover appends DDL, never substitutes it.** The markdown column table existing tests assert must survive. "The user never sees a fabricated declaration": what is not renderable as DDL is shown as the catalog's own summary fields or as verbatim catalog source, with **no synthesized `CREATE` header**.
- On `errors.Is(err, database.ErrUnsupportedDDL)`, hover appends **exactly one italic line** built from `database.UnsupportedDDLDetail(err)`; with `ok == false` it degrades to `_DDL unavailable._` — "never to a rendered driver message".
- On `errors.Is(err, database.ErrObjectNotFound)`, hover appends **nothing at all**, "not even a note: … a stale-cache footnote on a hover popup is noise the user cannot act on."
- On any other error, including the 3-second timeout, hover appends nothing and `log.Printf`s it. "A DDL problem must never surface as a JSON-RPC error or an error popup, because the user asked for documentation, not for DDL."
- **`ObjectKindFunction` always returns `ErrUnsupportedDDL`** (contract D6). Hover for an external function **never calls `ObjectDDL`** and never mentions DDL at all.
- **Unsupported DDL is the common case for procedures**, because `RDB$PROCEDURE_PARAMETERS` has no declaration nullability flag. The degradation path is the normal path, not an edge case.
- **Nullability renders only as `NOT NULL` and only when `Nullable.Valid && !Nullable.Bool`.** When `Nullable` is not valid, nothing is rendered — not "nullable", not "unknown". "Absence is the honest encoding of unknown."
- **Empty `Type`, `ReturnType` and `Event` mean "the catalog did not tell us" and the row is omitted.** "No feature emits a placeholder such as `<unknown>`, and none suppresses an object merely because a type is missing: a UDF with unrenderable arguments is still completable by name." CHAR and VARCHAR function arguments render `""` **permanently** — `RDB$CHARACTER_LENGTH` is never populated for function arguments — measured at 1 argument in 357 across three production InterBase 15.1 databases.
- **Views stay in `SchemaTables`.** `SortedViews()` is additive metadata and does not subtract from `SortedTables()`. "The resolution is not de-duplication but **placement**": view candidates are emitted only where `CompletionTypeView` is set and `CompletionTypeTable` is *not*; everywhere tables are offered the existing table candidate is the view's candidate and gains only the `view` detail. "That keeps one candidate per object with no name comparison anywhere."
- **Catalog accessors normalise the name they are given** (contract §4.5: "callers pass the identifier text as the user typed it and never upper-case at the call site"). **Never write `strings.ToUpper` at a lookup site in this plan.**
- **Explain must not overclaim safety and must not under-explain it.** DDL is refused; DML gets a banner "stating plainly that the statement was prepared only and nothing was inserted, updated or deleted"; an empty plan gets "two sentences, not one, because 'empty plan' and 'nothing ran' are independent facts".
- **The capability mock must be a distinct type from `MockDBRepository`** — "if `MockDBRepository` itself satisfied the capability interfaces, every existing handler test would start passing the type assertions and panic on nil func fields."
- **No eager DDL caching.** "DDL for every object is large and mostly unread", and `ReCache` runs on every connect and every `switchConnections`.
- **No new configuration keys.** "Every feature here is automatic when the driver is InterBase and the capability is present."
- **No new LSP capabilities** are advertised in `handleInitialize`: "Explain rides the existing `CodeActionProvider`."
- **Deliberately excluded, do not build:** trigger/domain/role/index/privilege/shadow completion; procedure **input parameter name** completion ("DSQL has no named parameters"); `NEXT VALUE FOR <generator>` completion; rewriting table hover as `CREATE TABLE` DDL; diagnostics from prepare failures.
- Verification commands: `go test ./...`, `make test-race`, and for tagged code `CGO_ENABLED=1 go build -tags interbase ./...`.

## File Structure

| File | Status | Responsibility |
| --- | --- | --- |
| `internal/database/interbase_mock.go` | Create | `MockCapabilityRepository` — a `DBRepository` that also implements `DDLRepository` and `ExplainRepository`, with call recording |
| `internal/database/interbase_mock_test.go` | Create | pins that `MockDBRepository` does **not** satisfy the capability interfaces and that the new type does |
| `internal/database/catalog_doc.go` | Create | every markdown fragment rendered from a catalog descriptor: procedures, parameters, views, generators, functions, triggers. Driver-neutral, no LSP types |
| `internal/database/catalog_doc_test.go` | Create | the omission rules — unknown nullability, empty `Type`/`ReturnType`/`Event` |
| `internal/handler/explain.go` | Create | `CommandExplainQuery` handling, statement admission, plan rendering |
| `internal/handler/explain_test.go` | Create | rendering, refusal, empty plan, array-column failure, capability absence, code-action advertisement |
| `internal/handler/execute_command.go` | Modify (`:24-32`, `:44-80`, `:94-110`) | the command constant, the "Explain SQL" code action, the dispatch case |
| `parser/parser.go` | Modify (`:263-274`) | the `"EXECUTE": {"PROCEDURE"}` multi-keyword entry |
| `parser/parser_test.go` | Modify (append) | the `EXECUTE PROCEDURE` grouping test |
| `parser/parseutil/position.go` | Modify (`:11-23`, `:99-107`) | the `ExecuteProcedure` syntax position |
| `parser/parseutil/position_test.go` | Modify (append) | position cases, including the argument-list regression pin |
| `parser/parseutil/call.go` | Create | `EnclosingCall`/`CallInfo`: the callee name and active-argument index of the call the cursor is inside. Consumed by both the completer and the handler, so it lives in neither |
| `parser/parseutil/call_test.go` | Create | callee resolution, the inside/on-the-name distinction, and the active-parameter index |
| `internal/completer/completer.go` | Modify (`:22-70`, `:123-200`, `:212-249`, `:396-400`) | `CompletionTypeProcedureName`, the new `Complete` branches, the sort-prefix cases, the `ExecuteProcedure` context |
| `internal/completer/candidates.go` | Modify (`:45-99`, `:384-402`) | procedure output parameters as columns; the `view` detail on an existing table candidate |
| `internal/completer/interbase_candidates.go` | Create | procedure, selectable-procedure, view, generator and external-function candidates |
| `internal/completer/interbase_candidates_test.go` | Create | the completer fixture and every feature-2 test |
| `internal/handler/signature_help.go` | Modify (`:43-107`) | the procedure branch before the `InsertValue` case |
| `internal/handler/signature_help_test.go` | Modify (append) | procedure signature cases |
| `internal/handler/interbase_hover.go` | Create | target resolution, catalog summary, DDL appendix, the memo |
| `internal/handler/interbase_hover_test.go` | Create | the shared handler catalog fixture and every feature-4 test |
| `internal/handler/hover.go` | Modify (`:22-45`) | call the InterBase path after the pure hover |
| `internal/handler/handler.go` | Modify (struct, `NewServer`, `reconnectionDB`) | the `ddlMemo` field and the `connGeneration` counter |
| `doc/develop.md` | Modify (concurrency audit table) | classify the new `Server` fields |
| `README.md` | Modify | flip `- [ ] Explain SQL`; the `#### InterBase editor features` subsection |

**Two collision notes, because Plan 4 is in flight on the same files:**

1. **`Server.connGeneration`.** Spec §6.1c lists "DDL memo (§4), snapshot store generation (§5)" as *one* row of the `stateMu` audit table, and §4 keys this plan's memo on "an `int` bumped by `reconnectionDB`" — the same counter Plan 4's snapshot store needs. **Neither plan owns it.** Plan 4's Task 3 Step 3 greps for `connGeneration` first and consumes an existing field; Task 8 Step 3 here does the same. The name is fixed as `connGeneration` in both plans so they converge without coordination. Whichever lands second adds only its own field (`ddlMemo` here, `snapshots` there).
2. **The README heading `#### InterBase editor features`.** This plan creates it for features 1–4; Plan 4's Task 7 nests under it when it exists and creates it when it does not. Task 9 here does the same in reverse.

**Why the rendering lives in `internal/database` rather than in each consumer.** Both the completer (candidate `Documentation`) and the handler (hover content) need the same procedure, function and generator markdown, and both must obey the same omission rules. `internal/database/database.go:85-181` already hosts `ColumnDoc`, `TableDoc`, `SubqueryDoc` and `SubqueryColumnDoc` for exactly this reason, and both packages already import `database`. A new sibling file follows the established pattern and means the "unknown nullability renders nothing" rule has one implementation and one test.

**What is not reachable end-to-end, stated rather than papered over.** `handleTextDocumentHover` and `explainQuery` obtain their repository from `s.newDBRepository`, which goes through `database.CreateRepository` and the package-level `driverFactories` map. `internal/database/interbase_common.go:17` already registers a factory for `dialect.DatabaseDriverInterBase`, and `RegisterFactory` **panics** on a duplicate registration (`internal/database/driver.go:52-57`), so a test cannot install a capability-bearing repository under the InterBase driver name. The same constraint that bound Plan 4 binds the two features here that need a repository, and the same workaround applies: **`explainStatements` and `(*Server).interBaseHoverDDL` take the capability as a parameter** and the tests call them directly. Features 2 and 3 are unaffected — `Completer.Complete` and `SignatureHelpWithDriver` read only the `*DBCache`, so their tests need no server and no repository at all. What goes through a real `jsonrpc2` round trip is the code-action advertisement and the no-capability degradation of hover, both of which work with the existing `configureInterBaseTestServer`.

**Five places the real code contradicts the spec.** Each is resolved in the task that hits it and listed here so a reviewer sees them together.

1. **The `ExecuteProcedure` syntax-position case cannot go "before the `TableReference` case"** (spec §2, "Contexts"). Measured: for `execute procedure myproc(` with the cursor inside the parenthesis, `NodeWalker.PrevNodesIs(true, ExpectKeyword: ["EXECUTE PROCEDURE"])` is already **true** — the walker checks every path depth, and at statement depth the node before the `FunctionLiteral` is the `MultiKeyword`. That position resolves to `InsertColumn` today. A case placed before `TableReference` would steal it and offer procedure names where the user is typing arguments. The case goes **last, after `isInsertColumns`**. Task 4 pins this with a regression test.
2. **Explain's single-statement rendering.** §1 "Rendering" shows a bare `PLAN …` for one statement, but every sample in "User-Visible Behavior" shows `-- statement 1` even when there is only one. The header is always emitted; the User-Visible samples are the literal user-facing text and win.
3. **Two parameter renderings.** §2 asks for `` - NAME: `TYPE` (input) `` and §3 for `` `VARCHAR(3)` input NOT NULL ``. Unified on §3's form, with the list entry being `` - NAME: `TYPE` input NOT NULL ``, so one renderer serves both surfaces.
4. **Hover cannot always "append after the pure call succeeds"** (§4). `hoverWithDriver` has no catalog knowledge, so for a procedure, generator or external function identifier it returns `ErrNoHover` and there is nothing to append to. The InterBase path therefore *renders* the summary for catalog-only kinds and *appends* for tables. Feature 4 would otherwise never fire on a procedure, which is the object it exists for.
5. **`SortedFunctions()` exists.** §2 "Enumeration" records that the contract has no such accessor and tells the completer to range over `CatalogCache.Functions`. The adjudicated contract (`…-interbase-dialect-and-catalog-design.md` §4.5) **does** declare `func (dc *DBCache) SortedFunctions() []string`, added for exactly this consumer. Use the accessor; never reach into `DBCache.Catalog` from the completer.

---

### Task 1: The capability mock

Spec D8. Sub-project 2's testing strategy is supposed to provide repository mocks for the capability interfaces; the spec says that if it does not, "sub-project 3 adds a wrapper type in `internal/database/interbase_mock.go` rather than modifying `MockDBRepository`". This task builds it there unconditionally, because every later task in this plan needs a repository that answers `ObjectDDL` and `ExplainPlan` and records what it was asked.

The type must be **distinct** from `MockDBRepository`. `internal/handler` has eleven test files whose servers run through `NewMockDBRepository`; if that type satisfied `DDLRepository`, the hover path's type assertion would succeed in every one of them and then call a nil func field.

**Files:**
- Create: `internal/database/interbase_mock.go`
- Test: `internal/database/interbase_mock_test.go`

**Interfaces:**
- Consumes: from sub-project 2's contract — `database.ObjectKind`, `database.DDLRepository`, `database.ExplainRepository`, `database.ErrObjectNotFound`.
- Produces:
  - `type ObjectDDLCall struct { Kind ObjectKind; Name string }`
  - `type MockCapabilityRepository struct { *MockDBRepository; MockObjectDDL func(context.Context, ObjectKind, string) (string, error); MockExplainPlan func(context.Context, string) (string, error) }`
  - `func NewMockCapabilityRepository() *MockCapabilityRepository`
  - `func (m *MockCapabilityRepository) ObjectDDLCalls() []ObjectDDLCall`
  - `func (m *MockCapabilityRepository) ExplainPlanCalls() []string`

- [ ] **Step 1: Verify sub-project 2's contract has landed**

Run:

```bash
grep -n 'type DDLRepository\|type ExplainRepository\|ErrObjectNotFound\|ErrUnsupportedDDL\|func UnsupportedDDLDetail\|ObjectKindProcedure\|ObjectKindGenerator\|ObjectKindFunction\|ObjectKindView\|ObjectKindTable' internal/database/capability.go
grep -n 'func (dc \*DBCache) HasCatalog\|func (dc \*DBCache) View\|func (dc \*DBCache) Procedure\|func (dc \*DBCache) Generator\|func (dc \*DBCache) Function\|func (dc \*DBCache) Trigger\|SortedProcedures\|SortedViews\|SortedGenerators\|SortedFunctions' internal/database/cache.go
grep -n 'type ProcedureDesc\|type ProcedureParameterDesc\|type ViewDesc\|type GeneratorDesc\|type FunctionDesc\|type FunctionArgumentDesc\|type TriggerDesc\|ParameterInput\|ParameterOutput' internal/database/capability.go
```

Expected: every name found. If any is missing, **stop** and report that sub-project 2's catalog plan has not landed. Do **not** stub the contract locally — a local definition collides with sub-project 2's when it arrives.

Then confirm the singular accessors normalise their argument, which is what makes a user's lowercase `myproc` find the catalog's `MYPROC`:

Run: `grep -n 'func (dc \*DBCache) Procedure' -A 8 internal/database/cache.go`

Expected: the body upper-cases the argument or matches with `strings.EqualFold`, matching `columnDatabaseKey` (`internal/database/cache.go:186-188`). If it is an exact match, that is contract D11's flagged risk: fix it in `cache.go` and note it. **Do not** add `strings.ToUpper` at any call site in this plan — hover, completion, signature help and Plan 2's procedure routing all depend on the same normalisation, and patching one call site hides the bug from the other three.

- [ ] **Step 2: Write the failing tests**

Create `internal/database/interbase_mock_test.go`:

```go
package database

import (
	"context"
	"errors"
	"testing"
)

// TestMockDBRepositoryDoesNotSatisfyCapabilityInterfaces is the guard that
// keeps the capability mock a separate type. If MockDBRepository ever grows
// ObjectDDL or ExplainPlan, every existing handler test would start passing
// the type assertions in the hover and explain paths and then panic on a nil
// func field. This test fails before that panic can reach anybody.
func TestMockDBRepositoryDoesNotSatisfyCapabilityInterfaces(t *testing.T) {
	var repo DBRepository = NewMockDBRepository(nil)
	if _, ok := repo.(DDLRepository); ok {
		t.Error("MockDBRepository satisfies DDLRepository; it must not")
	}
	if _, ok := repo.(ExplainRepository); ok {
		t.Error("MockDBRepository satisfies ExplainRepository; it must not")
	}
}

func TestMockCapabilityRepositorySatisfiesCapabilityInterfaces(t *testing.T) {
	var repo DBRepository = NewMockCapabilityRepository()
	if _, ok := repo.(DDLRepository); !ok {
		t.Error("MockCapabilityRepository does not satisfy DDLRepository")
	}
	if _, ok := repo.(ExplainRepository); !ok {
		t.Error("MockCapabilityRepository does not satisfy ExplainRepository")
	}
}

func TestMockCapabilityRepositoryRecordsCalls(t *testing.T) {
	repo := NewMockCapabilityRepository()
	repo.MockObjectDDL = func(_ context.Context, kind ObjectKind, name string) (string, error) {
		return "CREATE " + string(kind) + " " + name, nil
	}
	repo.MockExplainPlan = func(context.Context, string) (string, error) {
		return "PLAN (CITY NATURAL)", nil
	}

	if _, err := repo.ObjectDDL(context.Background(), ObjectKindProcedure, "MYPROC"); err != nil {
		t.Fatal("ObjectDDL:", err)
	}
	if _, err := repo.ExplainPlan(context.Background(), "SELECT 1"); err != nil {
		t.Fatal("ExplainPlan:", err)
	}

	ddlCalls := repo.ObjectDDLCalls()
	if len(ddlCalls) != 1 || ddlCalls[0].Kind != ObjectKindProcedure || ddlCalls[0].Name != "MYPROC" {
		t.Errorf("ObjectDDLCalls() = %+v, want one {procedure MYPROC} call", ddlCalls)
	}
	if got := repo.ExplainPlanCalls(); len(got) != 1 || got[0] != "SELECT 1" {
		t.Errorf("ExplainPlanCalls() = %v, want [\"SELECT 1\"]", got)
	}
}

func TestMockCapabilityRepositoryDefaultsToObjectNotFound(t *testing.T) {
	// The zero-configuration mock must not answer with a fabricated CREATE
	// statement: a test that forgets to set MockObjectDDL should see the
	// not-found branch, which renders nothing, rather than invented DDL.
	repo := NewMockCapabilityRepository()
	if _, err := repo.ObjectDDL(context.Background(), ObjectKindTable, "CITY"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("default ObjectDDL error = %v, want ErrObjectNotFound", err)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test -run 'TestMockDBRepositoryDoesNotSatisfy|TestMockCapabilityRepository' ./internal/database/ -v`
Expected: FAIL to compile — `undefined: NewMockCapabilityRepository`.

- [ ] **Step 4: Write the minimal implementation**

Create `internal/database/interbase_mock.go`:

```go
package database

import (
	"context"
	"sync"

	"github.com/sqls-server/sqls/dialect"
)

// ObjectDDLCall records one ObjectDDL request made against the mock.
type ObjectDDLCall struct {
	Kind ObjectKind
	Name string
}

// MockCapabilityRepository is a DBRepository that also implements the optional
// InterBase capability interfaces.
//
// It is deliberately a separate type from MockDBRepository rather than extra
// fields on it. Every handler test builds a MockDBRepository; if that type
// satisfied DDLRepository and ExplainRepository, the type assertions in the
// hover and explain paths would succeed everywhere and then call a nil func
// field.
type MockCapabilityRepository struct {
	*MockDBRepository

	MockObjectDDL   func(context.Context, ObjectKind, string) (string, error)
	MockExplainPlan func(context.Context, string) (string, error)

	mu               sync.Mutex
	objectDDLCalls   []ObjectDDLCall
	explainPlanCalls []string
}

func NewMockCapabilityRepository() *MockCapabilityRepository {
	return &MockCapabilityRepository{
		// NewMockDBRepository ignores its argument entirely.
		MockDBRepository: NewMockDBRepository(nil).(*MockDBRepository),
		MockObjectDDL: func(context.Context, ObjectKind, string) (string, error) {
			return "", ErrObjectNotFound
		},
		MockExplainPlan: func(context.Context, string) (string, error) {
			return "", nil
		},
	}
}

func (m *MockCapabilityRepository) Driver() dialect.DatabaseDriver {
	return dialect.DatabaseDriverInterBase
}

func (m *MockCapabilityRepository) ObjectDDL(ctx context.Context, kind ObjectKind, name string) (string, error) {
	m.mu.Lock()
	m.objectDDLCalls = append(m.objectDDLCalls, ObjectDDLCall{Kind: kind, Name: name})
	m.mu.Unlock()
	return m.MockObjectDDL(ctx, kind, name)
}

func (m *MockCapabilityRepository) ExplainPlan(ctx context.Context, query string) (string, error) {
	m.mu.Lock()
	m.explainPlanCalls = append(m.explainPlanCalls, query)
	m.mu.Unlock()
	return m.MockExplainPlan(ctx, query)
}

func (m *MockCapabilityRepository) ObjectDDLCalls() []ObjectDDLCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ObjectDDLCall(nil), m.objectDDLCalls...)
}

func (m *MockCapabilityRepository) ExplainPlanCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.explainPlanCalls...)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -run 'TestMockDBRepositoryDoesNotSatisfy|TestMockCapabilityRepository' ./internal/database/ -v`
Expected: every test PASS.

- [ ] **Step 6: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/database/interbase_mock.go internal/database/interbase_mock_test.go
git commit -m "test: add a capability repository mock distinct from MockDBRepository"
```

---

### Task 2: The Explain SQL command and code action

Spec §1 and the first four entries of "User-Visible Behavior". This task depends on nothing from sub-project 2 except `database.ExplainRepository`, which Task 1 already verified.

`handleTextDocumentCodeAction` (`internal/handler/execute_command.go:34`) advertises seven commands unconditionally with no driver check. Explain joins that list unconditionally too, "because the existing handler advertises every command unconditionally and a client that caches code actions across connection switches would otherwise show a stale list".

**Statement admission**, measured against `database.QueryExecType(query, "")` (`internal/database/query_type.go:246`), which splits on whitespace and takes the first token:

| Statement | `QueryExecType` | Admitted |
| --- | --- | --- |
| `SELECT * FROM CITY` | `("SELECT", true)` | yes, as a query |
| `WITH X AS (…) SELECT …` | `("WITH", true)` | yes, as a query |
| `INSERT INTO CITY …` | `("INSERT", false)` | yes, with the banner |
| `UPDATE CITY SET …` | `("UPDATE", false)` | yes, with the banner |
| `DELETE FROM CITY …` | `("DELETE", false)` | yes, with the banner |
| `EXECUTE PROCEDURE MYPROC(1)` | `("EXECUTE", false)` | yes, with the banner |
| `CREATE TABLE city (…)` | `("CREATE TABLE", false)` | refused |
| `COMMIT` | `("COMMIT", false)` | refused |
| `SET TRANSACTION READ ONLY` | `("SET TRANSACTION", false)` | refused |

**Files:**
- Create: `internal/handler/explain.go`
- Create: `internal/handler/explain_test.go`
- Modify: `internal/handler/execute_command.go:24-32` (constant), `:44-80` (code action list), `:94-110` (dispatch)

**Interfaces:**
- Consumes: `database.ExplainRepository` (Task 1 precondition); `database.MockCapabilityRepository` (Task 1); from Plan 1 — `(*Server).fileText`, `Server.connMu`, `cancellationNotice`, `cancelledError`.
- Produces:
  - `const CommandExplainQuery = "explainQuery"`
  - `func (s *Server) explainQuery(ctx context.Context, params lsp.ExecuteCommandParams) (interface{}, error)`
  - `func explainStatements(ctx context.Context, explainer database.ExplainRepository, queries []string) (string, error)`
  - `func explainRepositoryFor(repo database.DBRepository) (database.ExplainRepository, error)`

- [ ] **Step 1: Write the failing tests**

Create `internal/handler/explain_test.go`:

```go
package handler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

func TestExplainStatementsRendersOnePlanPerStatement(t *testing.T) {
	repo := database.NewMockCapabilityRepository()
	repo.MockExplainPlan = func(_ context.Context, query string) (string, error) {
		if strings.Contains(query, "COUNTRY") {
			return "PLAN (COUNTRY INDEX (RDB$PRIMARY7))", nil
		}
		return "PLAN (CITY NATURAL)", nil
	}

	got, err := explainStatements(context.Background(), repo, []string{
		"SELECT * FROM CITY",
		"SELECT * FROM COUNTRY",
	})
	if err != nil {
		t.Fatal("explainStatements:", err)
	}

	for _, want := range []string{
		"-- statement 1",
		"PLAN (CITY NATURAL)",
		"-- statement 2",
		"PLAN (COUNTRY INDEX (RDB$PRIMARY7))",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("explain output missing %q:\n%s", want, got)
		}
	}
	if calls := repo.ExplainPlanCalls(); len(calls) != 2 || calls[0] != "SELECT * FROM CITY" {
		t.Errorf("ExplainPlanCalls() = %v, want both statements verbatim", calls)
	}
	// A SELECT is not prepared-only news: the banner belongs to DML alone.
	if strings.Contains(got, "prepared only") {
		t.Errorf("a SELECT carried the DML banner:\n%s", got)
	}
}

func TestExplainStatementsEmptyPlanReportsNoPlan(t *testing.T) {
	repo := database.NewMockCapabilityRepository()
	repo.MockExplainPlan = func(context.Context, string) (string, error) { return "", nil }

	got, err := explainStatements(context.Background(), repo, []string{"SELECT * FROM CITY"})
	if err != nil {
		t.Fatal("an empty plan is not an error:", err)
	}
	// Two independent facts, both required: the plan was empty, and nothing
	// ran. A naive implementation prints the empty string and the pane is
	// blank, which reads as a broken command.
	if !strings.Contains(got, "No plan text.") {
		t.Errorf("output does not report the empty plan:\n%s", got)
	}
	if !strings.Contains(got, "Nothing was executed.") {
		t.Errorf("output does not say nothing ran:\n%s", got)
	}
	if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(got), "-- statement 1")) == "" {
		t.Errorf("output is a bare header with no body:\n%q", got)
	}
}

func TestExplainStatementsDMLShowsPreparedOnlyBanner(t *testing.T) {
	repo := database.NewMockCapabilityRepository()
	repo.MockExplainPlan = func(context.Context, string) (string, error) {
		return "PLAN (CITY INDEX (PK_CITY))", nil
	}

	got, err := explainStatements(context.Background(), repo, []string{
		"UPDATE CITY SET NAME = 'x' WHERE ID = 1",
	})
	if err != nil {
		t.Fatal("explainStatements:", err)
	}
	if !strings.Contains(got, "-- statement 1 (prepared only; nothing was inserted, updated or deleted)") {
		t.Errorf("UPDATE is missing the prepared-only banner:\n%s", got)
	}
	if !strings.Contains(got, "PLAN (CITY INDEX (PK_CITY))") {
		t.Errorf("the plan itself is missing:\n%s", got)
	}
}

func TestExplainStatementsBannersEveryMutatingKind(t *testing.T) {
	repo := database.NewMockCapabilityRepository()
	repo.MockExplainPlan = func(context.Context, string) (string, error) { return "PLAN (X NATURAL)", nil }

	for _, query := range []string{
		"INSERT INTO CITY (ID) VALUES (1)",
		"UPDATE CITY SET NAME = 'x'",
		"DELETE FROM CITY WHERE ID = 1",
		"EXECUTE PROCEDURE MYPROC(1)",
	} {
		got, err := explainStatements(context.Background(), repo, []string{query})
		if err != nil {
			t.Fatalf("explainStatements(%q): %v", query, err)
		}
		if !strings.Contains(got, "prepared only") {
			t.Errorf("%q was explained with no prepared-only banner:\n%s", query, got)
		}
	}
}

func TestExplainStatementsRefusesUnsupportedStatement(t *testing.T) {
	repo := database.NewMockCapabilityRepository()

	got, err := explainStatements(context.Background(), repo, []string{"CREATE TABLE city (id integer)"})
	if err != nil {
		t.Fatal("a refusal is rendered, not returned as an error:", err)
	}
	if !strings.Contains(got, "Explain supports SELECT, INSERT, UPDATE, DELETE and EXECUTE PROCEDURE") {
		t.Errorf("output does not explain the refusal:\n%s", got)
	}
	// The statement kind must be named, or the user cannot tell which of
	// several statements was refused.
	if !strings.Contains(got, "CREATE TABLE") {
		t.Errorf("output does not name the refused statement kind:\n%s", got)
	}
	if calls := repo.ExplainPlanCalls(); len(calls) != 0 {
		t.Errorf("ExplainPlan was called %d times for a refused statement, want 0", len(calls))
	}
}

func TestExplainStatementsRefusalDoesNotStopLaterStatements(t *testing.T) {
	repo := database.NewMockCapabilityRepository()
	repo.MockExplainPlan = func(context.Context, string) (string, error) { return "PLAN (CITY NATURAL)", nil }

	got, err := explainStatements(context.Background(), repo, []string{
		"COMMIT",
		"SELECT * FROM CITY",
	})
	if err != nil {
		t.Fatal("explainStatements:", err)
	}
	if !strings.Contains(got, "PLAN (CITY NATURAL)") {
		t.Errorf("a refused first statement suppressed the second:\n%s", got)
	}
}

func TestExplainStatementsExplainsArrayColumnFailure(t *testing.T) {
	repo := database.NewMockCapabilityRepository()
	repo.MockExplainPlan = func(context.Context, string) (string, error) {
		// The driver's real wording: the pooled prepare path passes
		// allow_arrays = 0, so output-type validation rejects the statement
		// instead of returning a plan (native.c:4718).
		return "", errors.New("interbase: prepare statement: array results are unsupported by database/sql")
	}

	got, err := explainStatements(context.Background(), repo, []string{"SELECT ARR FROM T"})
	if err != nil {
		t.Fatal("an array column is a known failure, not a command error:", err)
	}
	if !strings.Contains(got, "array column") {
		t.Errorf("output does not name the cause:\n%s", got)
	}
	if !strings.Contains(got, "array results are unsupported by database/sql") {
		t.Errorf("output hides the driver's own message:\n%s", got)
	}
}

func TestExplainStatementsPropagatesOtherErrors(t *testing.T) {
	repo := database.NewMockCapabilityRepository()
	sentinel := errors.New("interbase: dynamic SQL error: token unknown - line 1, column 8")
	repo.MockExplainPlan = func(context.Context, string) (string, error) { return "", sentinel }

	if _, err := explainStatements(context.Background(), repo, []string{"SELECT FROM"}); !errors.Is(err, sentinel) {
		t.Errorf("explainStatements error = %v, want the driver error", err)
	}
}

func TestExplainRepositoryForReportsTheDriverWhenUnsupported(t *testing.T) {
	_, err := explainRepositoryFor(database.NewMockDBRepository(nil))
	if err == nil {
		t.Fatal("a repository without ExplainPlan must be refused")
	}
	if !strings.Contains(err.Error(), "mock") {
		t.Errorf("error = %q, want it to name the driver", err)
	}
	if _, err := explainRepositoryFor(database.NewMockCapabilityRepository()); err != nil {
		t.Errorf("a capability repository was refused: %v", err)
	}
}

func TestExplainCodeActionIsAdvertised(t *testing.T) {
	tx := newTestContext()
	tx.setup(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()

	tx.textDocumentDidOpen(t, testFileURI, "SELECT * FROM CITY")

	var got []lsp.Command
	if err := tx.conn.Call(tx.ctx, "textDocument/codeAction", lsp.CodeActionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
	}, &got); err != nil {
		t.Fatal("conn.Call textDocument/codeAction:", err)
	}

	var found *lsp.Command
	for i, command := range got {
		if command.Command == CommandExplainQuery {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("no %q command in %+v", CommandExplainQuery, got)
	}
	if found.Title != "Explain SQL" {
		t.Errorf("title = %q, want %q", found.Title, "Explain SQL")
	}
	if len(found.Arguments) != 1 || found.Arguments[0] != testFileURI {
		t.Errorf("arguments = %v, want the document URI", found.Arguments)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestExplain' ./internal/handler/ -v`
Expected: FAIL to compile — `undefined: explainStatements`, `undefined: explainRepositoryFor`, `undefined: CommandExplainQuery`.

- [ ] **Step 3: Write the minimal implementation**

Create `internal/handler/explain.go`:

```go
package handler

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

const (
	explainRefusal = "Explain supports SELECT, INSERT, UPDATE, DELETE and EXECUTE PROCEDURE\n" +
		"statements; got %s."

	// Two sentences, not one: "the plan was empty" and "nothing ran" are
	// independent facts, and the driver README is explicit that an empty plan
	// never implies execution.
	explainNoPlan = "No plan text. The statement prepared successfully and InterBase reported no plan\n" +
		"for it. Nothing was executed."

	explainPreparedOnly = "prepared only; nothing was inserted, updated or deleted"

	explainArrayColumn = "Explain is unavailable for this statement: it returns an array column, and the\n" +
		"prepare path sqls uses rejects array results. Remove the array column from the\n" +
		"select list to see its plan."
)

// explainMutatingTypes are the QueryExecType results that are admitted but are
// not queries. Everything QueryExecType reports as a query is admitted too;
// everything else is refused.
var explainMutatingTypes = map[string]bool{
	"INSERT":  true,
	"UPDATE":  true,
	"DELETE":  true,
	"EXECUTE": true,
}

// explainArrayColumnMarker is matched in the driver's error text because the
// driver exposes no typed error for it: output-type validation rejects array
// results before a plan exists (native.c:4718, reached because the pooled
// prepare path passes allow_arrays = 0). The raw message is still shown, so
// matching only adds an explanation and never hides anything.
const explainArrayColumnMarker = "array results are unsupported"

func (s *Server) explainQuery(ctx context.Context, params lsp.ExecuteCommandParams) (result interface{}, err error) {
	s.connMu.RLock()
	defer s.connMu.RUnlock()

	if len(params.Arguments) == 0 {
		return nil, fmt.Errorf("required arguments were not provided: <File URI>")
	}
	uri, ok := params.Arguments[0].(string)
	if !ok {
		return nil, fmt.Errorf("specify the file uri as a string")
	}
	text, ok := s.fileText(uri)
	if !ok {
		return nil, fmt.Errorf("document not found, %q", uri)
	}

	// -show-vertical is deliberately not accepted: a plan is not a table.
	if params.Range != nil {
		text = extractRangeText(
			text,
			params.Range.Start.Line,
			params.Range.Start.Character,
			params.Range.End.Line,
			params.Range.End.Character,
		)
	}
	stmts, err := getStatementsWithDriver(text, s.parserDriver())
	if err != nil {
		return nil, err
	}

	repo, err := s.newDBRepository(ctx)
	if err != nil {
		return nil, err
	}
	explainer, err := explainRepositoryFor(repo)
	if err != nil {
		return nil, err
	}

	queries := make([]string, 0, len(stmts))
	for _, stmt := range stmts {
		query := strings.TrimSpace(stmt.String())
		if query == "" {
			continue
		}
		queries = append(queries, query)
	}

	rendered, err := explainStatements(ctx, explainer, queries)
	if err != nil {
		if notice := cancellationNotice(ctx, err); notice != "" {
			return nil, &cancelledError{rendered: notice}
		}
		return nil, err
	}
	return rendered, nil
}

// explainRepositoryFor reports whether the active repository can explain. The
// capability, not the driver name, is the gate: a non-InterBase repository
// that later implements ExplainPlan gets the feature for free.
func explainRepositoryFor(repo database.DBRepository) (database.ExplainRepository, error) {
	explainer, ok := repo.(database.ExplainRepository)
	if !ok {
		return nil, fmt.Errorf("explain is not supported by the %s driver", repo.Driver())
	}
	return explainer, nil
}

// explainStatements renders one plan per statement. It takes the capability
// rather than a *Server so it is reachable from a test: the InterBase driver
// name is already claimed in database.driverFactories and RegisterFactory
// panics on a duplicate, so no test can install a capability-bearing
// repository under it.
func explainStatements(ctx context.Context, explainer database.ExplainRepository, queries []string) (string, error) {
	buf := new(bytes.Buffer)
	for i, query := range queries {
		if i > 0 {
			fmt.Fprintln(buf)
		}

		typ, isQuery := database.QueryExecType(query, "")
		switch {
		case isQuery:
			fmt.Fprintf(buf, "-- statement %d\n", i+1)
		case explainMutatingTypes[typ]:
			fmt.Fprintf(buf, "-- statement %d (%s)\n", i+1, explainPreparedOnly)
		default:
			// Refusing DDL costs one condition and avoids handing the user an
			// always-empty plan for CREATE TABLE, which reads like a bug.
			fmt.Fprintf(buf, "-- statement %d\n", i+1)
			fmt.Fprintf(buf, explainRefusal+"\n", typ)
			continue
		}

		plan, err := explainer.ExplainPlan(ctx, query)
		if err != nil {
			if strings.Contains(err.Error(), explainArrayColumnMarker) {
				fmt.Fprintln(buf, explainArrayColumn)
				fmt.Fprintln(buf, err.Error())
				continue
			}
			return "", err
		}
		if strings.TrimSpace(plan) == "" {
			fmt.Fprintln(buf, explainNoPlan)
			continue
		}
		fmt.Fprintln(buf, strings.TrimRight(plan, "\n"))
	}
	return buf.String(), nil
}
```

In `internal/handler/execute_command.go`, add the constant to the block at `:24-32`:

```go
	CommandShowTables       = "showTables"
	CommandExplainQuery     = "explainQuery"
)
```

Add the code action immediately after the "Execute Query" entry (`:49`):

```go
		{
			Title:     "Explain SQL",
			Command:   CommandExplainQuery,
			Arguments: []interface{}{params.TextDocument.URI},
		},
```

Add the dispatch case after `case CommandShowTables:` (`:107-108`):

```go
	case CommandExplainQuery:
		return s.explainQuery(ctx, params)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestExplain' ./internal/handler/ -v`
Expected: every test PASS.

- [ ] **Step 5: Verify the existing code-action test still holds**

Run: `go test -run 'TestInitialized|CodeAction' ./internal/handler/ -v`
Expected: PASS. `handleInitialize` is untouched — Explain rides the existing `CodeActionProvider` and advertises no new capability.

- [ ] **Step 6: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/handler/explain.go internal/handler/explain_test.go internal/handler/execute_command.go
git commit -m "feat: add an Explain SQL code action backed by the driver's prepare-only plan"
```

---
### Task 3: Catalog markdown rendering

Spec §2 "Documentation body", §3 "Rendering", §4 "Object-by-object behavior", and D10's rendering table. Both the completer and the hover handler need the same markdown built from the same descriptors under the same omission rules, so it is written once, in `internal/database`, beside the existing `ColumnDoc`/`TableDoc`/`SubqueryDoc` (`internal/database/database.go:85-181`). Both packages already import `database`.

**This is where the two rules that must not be got wrong live**, so it is also where they are tested:

- **Nullability renders only as `NOT NULL`, only when `Nullable.Valid && !Nullable.Bool`.** An invalid `Nullable` renders neither "nullable" nor "unknown". `schema/README.md` says invalid is the *normal* case for procedure parameters.
- **An empty `Type`, `ReturnType` or `Event` omits its element**, never a placeholder, and never hides the object.

**Files:**
- Create: `internal/database/catalog_doc.go`
- Test: `internal/database/catalog_doc_test.go`
- Modify: `internal/database/database.go:103-116` (extract the column-table writer so `ViewDoc` can reuse it)

**Interfaces:**
- Consumes: from sub-project 2's contract — `ProcedureDesc`, `ProcedureParameterDesc`, `ParameterDirection` with `ParameterInput`/`ParameterOutput`, `ViewDesc`, `GeneratorDesc`, `FunctionDesc`, `FunctionArgumentDesc`, `TriggerDesc`.
- Produces:
  - `func ParameterDoc(param *ProcedureParameterDesc) string`
  - `func ProcedureDoc(desc *ProcedureDesc) string`
  - `func ProcedureSignatureLabel(desc *ProcedureDesc) string`
  - `func ProcedureSignatureDoc(desc *ProcedureDesc) string`
  - `func ViewDoc(desc *ViewDesc) string`
  - `func GeneratorDoc(desc *GeneratorDesc) string`
  - `func FunctionDoc(desc *FunctionDesc) string`
  - `func TriggerDoc(desc *TriggerDesc) string`
  - `func writeColumnTable(buf *bytes.Buffer, cols []*ColumnDesc)` — unexported, extracted from `TableDoc`.

- [ ] **Step 1: Write the failing tests**

Create `internal/database/catalog_doc_test.go`:

```go
package database

import (
	"database/sql"
	"strings"
	"testing"
)

func testProcedureDesc() *ProcedureDesc {
	return &ProcedureDesc{
		Name:        "MYPROC",
		Description: sql.NullString{String: "Totals an order.", Valid: true},
		Source:      sql.NullString{String: "BEGIN\n  SUSPEND;\nEND", Valid: true},
		InputParameters: []*ProcedureParameterDesc{
			{Name: "IN_CODE", Position: 0, Direction: ParameterInput, Type: "VARCHAR(3)", Nullable: sql.NullBool{Bool: false, Valid: true}},
			{Name: "IN_AMOUNT", Position: 1, Direction: ParameterInput, Type: "NUMERIC(18, 2)", Nullable: sql.NullBool{}},
		},
		OutputParameters: []*ProcedureParameterDesc{
			{Name: "OUT_TOTAL", Position: 0, Direction: ParameterOutput, Type: "INTEGER", Nullable: sql.NullBool{}},
		},
	}
}

func TestParameterDocRendersNotNullOnlyWhenKnownFalse(t *testing.T) {
	cases := []struct {
		name  string
		param *ProcedureParameterDesc
		want  string
	}{
		{
			name:  "known not null",
			param: &ProcedureParameterDesc{Name: "IN_CODE", Direction: ParameterInput, Type: "VARCHAR(3)", Nullable: sql.NullBool{Bool: false, Valid: true}},
			want:  "`VARCHAR(3)` input NOT NULL",
		},
		{
			// The normal case: RDB$PROCEDURE_PARAMETERS has no declaration
			// nullability flag, so Valid is false. Rendering "nullable" here
			// would assert a fact the catalog does not contain.
			name:  "unknown nullability renders nothing",
			param: &ProcedureParameterDesc{Name: "IN_AMOUNT", Direction: ParameterInput, Type: "NUMERIC(18, 2)", Nullable: sql.NullBool{}},
			want:  "`NUMERIC(18, 2)` input",
		},
		{
			name:  "known nullable renders nothing",
			param: &ProcedureParameterDesc{Name: "IN_NOTE", Direction: ParameterInput, Type: "VARCHAR(80)", Nullable: sql.NullBool{Bool: true, Valid: true}},
			want:  "`VARCHAR(80)` input",
		},
		{
			name:  "unrenderable type omits the type element",
			param: &ProcedureParameterDesc{Name: "IN_BLOB", Direction: ParameterInput, Type: "", Nullable: sql.NullBool{}},
			want:  "input",
		},
		{
			name:  "output direction",
			param: &ProcedureParameterDesc{Name: "OUT_TOTAL", Direction: ParameterOutput, Type: "INTEGER"},
			want:  "`INTEGER` output",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := ParameterDoc(tt.param)
			if got != tt.want {
				t.Errorf("ParameterDoc = %q, want %q", got, tt.want)
			}
			// Guard against a renderer that invents a word for the unknown
			// case. This is the assertion that fails against a naive
			// implementation that maps Nullable to "nullable"/"unknown".
			for _, forbidden := range []string{"nullable", "NULLABLE", "unknown", "<unknown>", "NULL "} {
				if strings.Contains(got, forbidden) {
					t.Errorf("ParameterDoc = %q, must not contain %q", got, forbidden)
				}
			}
		})
	}
}

func TestProcedureDocRendersParametersDescriptionAndSource(t *testing.T) {
	got := ProcedureDoc(testProcedureDesc())

	for _, want := range []string{
		"`MYPROC` procedure",
		"Totals an order.",
		"- IN_CODE: `VARCHAR(3)` input NOT NULL",
		"- IN_AMOUNT: `NUMERIC(18, 2)` input",
		"- OUT_TOTAL: `INTEGER` output",
		"BEGIN\n  SUSPEND;\nEND",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ProcedureDoc missing %q:\n%s", want, got)
		}
	}
	// The catalog source is preserved verbatim and never wrapped in a
	// synthesized CREATE header.
	if strings.Contains(got, "CREATE PROCEDURE") {
		t.Errorf("ProcedureDoc fabricated a CREATE header:\n%s", got)
	}
}

func TestProcedureDocOmitsAbsentSections(t *testing.T) {
	got := ProcedureDoc(&ProcedureDesc{Name: "DOWORK"})

	if !strings.Contains(got, "`DOWORK` procedure") {
		t.Errorf("ProcedureDoc lost the name:\n%s", got)
	}
	for _, forbidden := range []string{"Input parameters", "Output parameters", "Source"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("ProcedureDoc rendered an empty %q section:\n%s", forbidden, got)
		}
	}
}

func TestProcedureSignatureLabelAndDoc(t *testing.T) {
	desc := testProcedureDesc()
	if got, want := ProcedureSignatureLabel(desc), "MYPROC (IN_CODE, IN_AMOUNT)"; got != want {
		t.Errorf("ProcedureSignatureLabel = %q, want %q", got, want)
	}
	if got, want := ProcedureSignatureDoc(desc), "MYPROC procedure — 2 input parameters, 1 output parameter"; got != want {
		t.Errorf("ProcedureSignatureDoc = %q, want %q", got, want)
	}
	if got, want := ProcedureSignatureLabel(&ProcedureDesc{Name: "NOARGS"}), "NOARGS ()"; got != want {
		t.Errorf("ProcedureSignatureLabel with no inputs = %q, want %q", got, want)
	}
}

func TestGeneratorDocRendersNameAndNothingElse(t *testing.T) {
	got := GeneratorDoc(&GeneratorDesc{Name: "GEN_ORDER_ID", ID: sql.NullInt64{Int64: 3, Valid: true}})
	if got != "`GEN_ORDER_ID` generator" {
		t.Errorf("GeneratorDoc = %q, want %q", got, "`GEN_ORDER_ID` generator")
	}
}

func TestFunctionDocRendersUnrenderableArgumentsWithoutPlaceholders(t *testing.T) {
	desc := &FunctionDesc{
		Name:        "MYUDF",
		ReturnType:  "DOUBLE PRECISION",
		Description: sql.NullString{String: "Rounds half up.", Valid: true},
		ModuleName:  sql.NullString{String: "udflib", Valid: true},
		EntryPoint:  sql.NullString{String: "myudf", Valid: true},
		Arguments: []*FunctionArgumentDesc{
			{Name: "", Position: sql.NullInt64{Int64: 1, Valid: true}, Type: "DOUBLE PRECISION"},
			// RDB$CHARACTER_LENGTH is never populated for function arguments,
			// so a CHAR/VARCHAR argument renders "" permanently.
			{Name: "", Position: sql.NullInt64{Int64: 2, Valid: true}, Type: ""},
		},
	}

	got := FunctionDoc(desc)
	for _, want := range []string{
		"`MYUDF` external function",
		"Rounds half up.",
		"- argument 1: `DOUBLE PRECISION`",
		"- argument 2",
		"Returns `DOUBLE PRECISION`.",
		"`udflib`",
		"`myudf`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FunctionDoc missing %q:\n%s", want, got)
		}
	}
	// The untyped argument keeps its label and gains nothing else. A naive
	// renderer emits "- argument 2: ``" or "- argument 2: <unknown>"; both
	// fail here.
	if strings.Contains(got, "argument 2: ") || strings.Contains(got, "<unknown>") || strings.Contains(got, "``") {
		t.Errorf("FunctionDoc rendered a placeholder for the unknown argument type:\n%s", got)
	}
}

func TestFunctionDocOmitsReturnsLineWhenReturnTypeIsEmpty(t *testing.T) {
	got := FunctionDoc(&FunctionDesc{
		Name:      "MYUDF",
		Arguments: []*FunctionArgumentDesc{{Position: sql.NullInt64{Int64: 1, Valid: true}, Type: ""}},
	})
	if !strings.Contains(got, "`MYUDF` external function") {
		t.Errorf("a UDF with no renderable type at all lost its name:\n%s", got)
	}
	if !strings.Contains(got, "- argument 1") {
		t.Errorf("a UDF with no renderable argument type lost its argument list:\n%s", got)
	}
	if strings.Contains(got, "Returns") {
		t.Errorf("FunctionDoc rendered a returns line for an empty ReturnType:\n%s", got)
	}
}

func TestTriggerDocOmitsEmptyEvent(t *testing.T) {
	desc := &TriggerDesc{
		Name:         "MYTRIGGER",
		RelationName: sql.NullString{String: "CITY", Valid: true},
		Event:        "",
		Active:       sql.NullBool{Bool: true, Valid: true},
		Source:       sql.NullString{String: "BEGIN\n  NEW.ID = 1;\nEND", Valid: true},
	}

	got := TriggerDoc(desc)
	for _, want := range []string{"`MYTRIGGER` trigger", "CITY", "active", "NEW.ID = 1;"} {
		if !strings.Contains(got, want) {
			t.Errorf("TriggerDoc missing %q:\n%s", want, got)
		}
	}
	// An empty Event means the catalog did not decode it. The whole line is
	// omitted; nothing stands in for it.
	if strings.Contains(got, "<unknown>") || strings.Contains(got, "Event") || strings.Contains(got, "``") {
		t.Errorf("TriggerDoc rendered a placeholder event:\n%s", got)
	}

	desc.Event = "BEFORE INSERT"
	if got := TriggerDoc(desc); !strings.Contains(got, "BEFORE INSERT") {
		t.Errorf("TriggerDoc dropped a known event:\n%s", got)
	}
}

func TestViewDocRendersColumnsAndSource(t *testing.T) {
	desc := &ViewDesc{
		Name:       "MYVIEW",
		ViewSource: sql.NullString{String: "SELECT ID FROM CITY", Valid: true},
		Columns: []*ColumnDesc{
			{ColumnBase: ColumnBase{Table: "MYVIEW", Name: "ID"}, Type: "INTEGER"},
		},
	}

	got := ViewDoc(desc)
	for _, want := range []string{"`MYVIEW` view", "| `ID` | `INTEGER` |", "SELECT ID FROM CITY"} {
		if !strings.Contains(got, want) {
			t.Errorf("ViewDoc missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "CREATE VIEW") {
		t.Errorf("ViewDoc fabricated a CREATE header:\n%s", got)
	}
}

func TestTableDocOutputIsUnchangedByTheExtraction(t *testing.T) {
	// writeColumnTable is extracted out of TableDoc in this task. TableDoc's
	// exact output is asserted by existing hover tests, so it is pinned here
	// too, byte for byte.
	cols := []*ColumnDesc{
		{ColumnBase: ColumnBase{Table: "city", Name: "ID"}, Type: "int(11)", Key: "PRI", Extra: "auto_increment"},
	}
	want := "# `city` table\n\n\n" +
		"| Name&nbsp;&nbsp; | Type&nbsp;&nbsp; | Primary&nbsp;key&nbsp;&nbsp; | Default&nbsp;&nbsp; | Extra&nbsp;&nbsp; |\n" +
		"| :--------------- | :--------------- | :---------------------- | :------------------ | :---------------- |\n" +
		"| `ID` | `int(11)` | `PRI` | `-` | auto_increment |\n"
	if got := TableDoc("city", cols); got != want {
		t.Errorf("TableDoc output changed:\ngot:  %q\nwant: %q", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestParameterDoc|TestProcedureDoc|TestProcedureSignature|TestGeneratorDoc|TestFunctionDoc|TestTriggerDoc|TestViewDoc|TestTableDocOutput' ./internal/database/ -v`
Expected: FAIL to compile — `undefined: ParameterDoc`, `undefined: ProcedureDoc`, and the rest.

- [ ] **Step 3: Write the minimal implementation**

First, in `internal/database/database.go`, replace `TableDoc` (`:103-116`) with the extracted pair. The output is byte-identical:

```go
func TableDoc(tableName string, cols []*ColumnDesc) string {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "# `%s` table", tableName)
	fmt.Fprintln(buf)
	fmt.Fprintln(buf)
	fmt.Fprintln(buf)
	writeColumnTable(buf, cols)
	return buf.String()
}

func writeColumnTable(buf *bytes.Buffer, cols []*ColumnDesc) {
	fmt.Fprintln(buf, "| Name&nbsp;&nbsp; | Type&nbsp;&nbsp; | Primary&nbsp;key&nbsp;&nbsp; | Default&nbsp;&nbsp; | Extra&nbsp;&nbsp; |")
	fmt.Fprintln(buf, "| :--------------- | :--------------- | :---------------------- | :------------------ | :---------------- |")
	for _, col := range cols {
		fmt.Fprintf(buf, "| `%s` | `%s` | `%s` | `%s` | %s |", col.Name, col.Type, col.Key, Coalesce(col.Default.String, "-"), col.Extra)
		fmt.Fprintln(buf)
	}
}
```

Then create `internal/database/catalog_doc.go`:

```go
package database

import (
	"bytes"
	"fmt"
	"strings"
)

// ParameterDoc renders one procedure parameter as a single line, for example
// "`VARCHAR(3)` input NOT NULL".
//
// Nullability is rendered only as NOT NULL and only when the catalog proved it:
// Nullable.Valid && !Nullable.Bool. An invalid Nullable is the normal case for
// InterBase procedure parameters — RDB$PROCEDURE_PARAMETERS carries no
// declaration nullability flag — and it renders nothing at all. Printing
// "nullable" would assert a fact the catalog does not contain, and printing
// "unknown nullability" is noise in a one-line tooltip. An empty Type means the
// catalog could not render one, and the element is omitted rather than
// replaced by a placeholder.
func ParameterDoc(param *ProcedureParameterDesc) string {
	if param == nil {
		return ""
	}
	items := []string{}
	if param.Type != "" {
		items = append(items, "`"+param.Type+"`")
	}
	if param.Direction != "" {
		items = append(items, string(param.Direction))
	}
	if param.Nullable.Valid && !param.Nullable.Bool {
		items = append(items, "NOT NULL")
	}
	return strings.Join(items, " ")
}

// ProcedureDoc renders the catalog's own view of a procedure: name,
// description, parameter lists, and the verbatim PSQL body. No CREATE header is
// synthesized — what the catalog cannot reproduce as DDL is shown as its own
// fields, never as an invented declaration.
func ProcedureDoc(desc *ProcedureDesc) string {
	if desc == nil {
		return ""
	}
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "`%s` procedure\n\n", desc.Name)
	if desc.Description.Valid && strings.TrimSpace(desc.Description.String) != "" {
		fmt.Fprintf(buf, "%s\n\n", strings.TrimSpace(desc.Description.String))
	}
	writeParameterList(buf, "Input parameters:", desc.InputParameters)
	writeParameterList(buf, "Output parameters:", desc.OutputParameters)
	if desc.Source.Valid && strings.TrimSpace(desc.Source.String) != "" {
		fmt.Fprintf(buf, "Source:\n\n```sql\n%s\n```\n", strings.TrimRight(desc.Source.String, "\n"))
	}
	return buf.String()
}

func writeParameterList(buf *bytes.Buffer, heading string, params []*ProcedureParameterDesc) {
	if len(params) == 0 {
		return
	}
	fmt.Fprintf(buf, "%s\n\n", heading)
	for _, param := range params {
		if doc := ParameterDoc(param); doc != "" {
			fmt.Fprintf(buf, "- %s: %s\n", param.Name, doc)
		} else {
			fmt.Fprintf(buf, "- %s\n", param.Name)
		}
	}
	fmt.Fprintln(buf)
}

// ProcedureSignatureLabel renders the signature-help label, "MYPROC (A, B)".
// Output parameters are not arguments and never appear here.
func ProcedureSignatureLabel(desc *ProcedureDesc) string {
	if desc == nil {
		return ""
	}
	names := make([]string, 0, len(desc.InputParameters))
	for _, param := range desc.InputParameters {
		names = append(names, param.Name)
	}
	return fmt.Sprintf("%s (%s)", desc.Name, strings.Join(names, ", "))
}

// ProcedureSignatureDoc counts both parameter lists so the user can tell a
// selectable procedure from an executable one.
func ProcedureSignatureDoc(desc *ProcedureDesc) string {
	if desc == nil {
		return ""
	}
	return fmt.Sprintf("%s procedure — %s, %s",
		desc.Name,
		pluralParameters(len(desc.InputParameters), "input"),
		pluralParameters(len(desc.OutputParameters), "output"),
	)
}

func pluralParameters(n int, kind string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s parameter", n, kind)
	}
	return fmt.Sprintf("%d %s parameters", n, kind)
}

// ViewDoc renders a view as its column table plus the verbatim catalog source.
func ViewDoc(desc *ViewDesc) string {
	if desc == nil {
		return ""
	}
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "# `%s` view\n\n\n", desc.Name)
	if desc.Description.Valid && strings.TrimSpace(desc.Description.String) != "" {
		fmt.Fprintf(buf, "%s\n\n", strings.TrimSpace(desc.Description.String))
	}
	if len(desc.Columns) > 0 {
		writeColumnTable(buf, desc.Columns)
		fmt.Fprintln(buf)
	}
	if desc.ViewSource.Valid && strings.TrimSpace(desc.ViewSource.String) != "" {
		fmt.Fprintf(buf, "Source:\n\n```sql\n%s\n```\n", strings.TrimRight(desc.ViewSource.String, "\n"))
	}
	return buf.String()
}

// GeneratorDoc renders a generator's name and nothing else. GeneratorDesc
// carries only Name and ID, so there is no description to show and none is
// invented.
func GeneratorDoc(desc *GeneratorDesc) string {
	if desc == nil {
		return ""
	}
	return fmt.Sprintf("`%s` generator", desc.Name)
}

// FunctionDoc renders an external function's declaration metadata.
//
// An argument whose Type is empty renders as its label alone. That is
// permanent for CHAR and VARCHAR arguments — RDB$CHARACTER_LENGTH is never
// populated for function arguments — and it must never hide the argument or
// the function: a UDF the catalog cannot fully describe is still a UDF the
// user needs to call.
func FunctionDoc(desc *FunctionDesc) string {
	if desc == nil {
		return ""
	}
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "`%s` external function\n\n", desc.Name)
	if desc.Description.Valid && strings.TrimSpace(desc.Description.String) != "" {
		fmt.Fprintf(buf, "%s\n\n", strings.TrimSpace(desc.Description.String))
	}
	if len(desc.Arguments) > 0 {
		fmt.Fprintf(buf, "Arguments:\n\n")
		for i, arg := range desc.Arguments {
			label := arg.Name
			if label == "" {
				position := int64(i + 1)
				if arg.Position.Valid {
					position = arg.Position.Int64
				}
				label = fmt.Sprintf("argument %d", position)
			}
			if arg.Type == "" {
				fmt.Fprintf(buf, "- %s\n", label)
				continue
			}
			fmt.Fprintf(buf, "- %s: `%s`\n", label, arg.Type)
		}
		fmt.Fprintln(buf)
	}
	if desc.ReturnType != "" {
		fmt.Fprintf(buf, "Returns `%s`.\n\n", desc.ReturnType)
	}
	if desc.ModuleName.Valid && desc.ModuleName.String != "" {
		if desc.EntryPoint.Valid && desc.EntryPoint.String != "" {
			fmt.Fprintf(buf, "Declared in module `%s`, entry point `%s`.\n", desc.ModuleName.String, desc.EntryPoint.String)
		} else {
			fmt.Fprintf(buf, "Declared in module `%s`.\n", desc.ModuleName.String)
		}
	}
	return buf.String()
}

// TriggerDoc renders a trigger's catalog fields. An empty Event means the
// catalog did not decode one; the line is omitted and nothing stands in for it.
func TriggerDoc(desc *TriggerDesc) string {
	if desc == nil {
		return ""
	}
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "`%s` trigger\n\n", desc.Name)
	if desc.Description.Valid && strings.TrimSpace(desc.Description.String) != "" {
		fmt.Fprintf(buf, "%s\n\n", strings.TrimSpace(desc.Description.String))
	}
	if desc.RelationName.Valid && desc.RelationName.String != "" {
		fmt.Fprintf(buf, "On `%s`.\n\n", desc.RelationName.String)
	}
	if desc.Event != "" {
		fmt.Fprintf(buf, "`%s`\n\n", desc.Event)
	}
	if desc.Active.Valid {
		state := "inactive"
		if desc.Active.Bool {
			state = "active"
		}
		fmt.Fprintf(buf, "Currently %s.\n\n", state)
	}
	if desc.Source.Valid && strings.TrimSpace(desc.Source.String) != "" {
		fmt.Fprintf(buf, "Source:\n\n```sql\n%s\n```\n", strings.TrimRight(desc.Source.String, "\n"))
	}
	return buf.String()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestParameterDoc|TestProcedureDoc|TestProcedureSignature|TestGeneratorDoc|TestFunctionDoc|TestTriggerDoc|TestViewDoc|TestTableDocOutput' ./internal/database/ -v`
Expected: every test and subtest PASS.

- [ ] **Step 5: Prove the omission tests are not vacuous**

The `TestParameterDoc…` forbidden-substring loop and the `FunctionDoc` placeholder check only mean something if they fail against a renderer that *does* emit a placeholder. Verify by hand:

Temporarily change `ParameterDoc` so the unknown case appends `"nullable"`:

```go
	if param.Nullable.Valid && !param.Nullable.Bool {
		items = append(items, "NOT NULL")
	} else {
		items = append(items, "nullable") // temporary
	}
```

Run: `go test -run 'TestParameterDocRendersNotNullOnlyWhenKnownFalse' ./internal/database/ -v`
Expected: FAIL on both the `want` comparison and the forbidden-substring guard for the "unknown nullability" and "known nullable" subtests. **Revert the temporary change** and re-run to confirm PASS. If it passed with the temporary change in place, the test asserts nothing and must be fixed before moving on.

- [ ] **Step 6: Verify the existing hover tests still hold**

Run: `go test -run 'Hover' ./internal/handler/ -v`
Expected: PASS, including `TestInterBaseDialect1LanguageServerHover`, which asserts `TableDoc` output through the language server. The extraction in Step 3 changed no bytes.

- [ ] **Step 7: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 8: Commit**

```bash
git add internal/database/catalog_doc.go internal/database/catalog_doc_test.go internal/database/database.go
git commit -m "feat: render catalog descriptors as markdown with honest omissions"
```

---

### Task 4: `EXECUTE PROCEDURE` as a multi-keyword and a syntax position

Spec §2 "Contexts", first bullet. This plan owns the `"EXECUTE": {"PROCEDURE"}` entry: Plan 2's Task 7 deliberately parses the statement text instead so its routing work does not depend on it.

`multiKeywordMap` (`parser/parser.go:263`) is applied by `parser.go:112` on **every** parse regardless of dialect. Both `EXECUTE` and `PROCEDURE` are already `dialect.Matched` (`dialect/keyword.go:130`, `:253`), so the group fires.

**The real blast radius, stated rather than minimised.** PostgreSQL's legacy trigger syntax `CREATE TRIGGER … FOR EACH ROW EXECUTE PROCEDURE f()` contains exactly this sequence and is still accepted by current PostgreSQL — superseded by `EXECUTE FUNCTION` in PG 11, not removed. For that statement the syntax position after the two keywords changes from `Unknown` to `ExecuteProcedure`. **PostgreSQL parsing does change.** The mitigation is that `getCompletionTypes` retains `CompletionTypeKeyword` in the new branch (Task 5), so a PostgreSQL user writing a trigger keeps exactly today's keyword candidates and gains nothing else — no procedure cache exists for that driver. A test in this task pins the position change so it is a recorded decision rather than a surprise.

**Placement, and why it contradicts the spec.** §2 says to place the case "before the `TableReference` case". Measured against the real walker: for `execute procedure myproc(` with the cursor inside the parenthesis, `nw.PrevNodesIs(true, genKeywordMatcher([]string{"EXECUTE PROCEDURE"}))` is already **true**, because `PrevNodesIs` checks every path depth and at statement depth the node before the `FunctionLiteral` is the `MultiKeyword`. That position resolves to `InsertColumn` today, which is what gives the argument list its behaviour. A case before `TableReference` would capture it and offer procedure *names* where the user is typing *arguments*. The case therefore goes **last, after `isInsertColumns`**, and Step 1 pins that with a regression case.

**The call helper ships here too, and this is why.** Feature 2 needs "the cursor is inside `GEN_ID(`" and feature 3 needs "the callee is a known procedure, and which argument is the cursor on". Both are the same question about the enclosing `ast.FunctionLiteral`, and they are asked from two different packages — `internal/completer` and `internal/handler`. Neither can import the other, so the helper lives in `parser/parseutil` beside `NodeWalker`, which is what it reads. It is added in this task rather than in Task 5 so that the whole "what does the parser say about this cursor" surface lands in one reviewable change, with no parser-shaped code in the completer.

**Files:**
- Modify: `parser/parser.go:263-274`
- Modify: `parser/parser_test.go` (append a case to `TestParseMultiKeyword`, and two new tests)
- Modify: `parser/parseutil/position.go:11-23`, `:99-107`
- Modify: `parser/parseutil/position_test.go` (append cases)
- Create: `parser/parseutil/call.go`
- Create: `parser/parseutil/call_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `parseutil.ExecuteProcedure SyntaxPosition = "execute_procedure"`, and `ast.MultiKeyword` grouping for `EXECUTE PROCEDURE`.
  - `type CallInfo struct { Callee string; Inside bool; args *ast.IdentifierList }`
  - `func EnclosingCall(nw *NodeWalker) (CallInfo, bool)`
  - `func (c CallInfo) ActiveParameter(pos token.Pos) int`

- [ ] **Step 1: Write the failing tests**

In `parser/parser_test.go`, add a case to `TestParseMultiKeyword`'s `testcases` slice, after the `"delete keyword"` case:

```go
		{
			name:  "execute procedure keyword",
			input: "execute procedure",
			checkFn: func(t *testing.T, stmts []*ast.Statement, input string) {
				testStatement(t, stmts[0], 1, input)
				list := stmts[0].GetTokens()
				testMultiKeyword(t, list[0], input)
			},
		},
```

Append to `parser/parser_test.go`:

```go
// TestParseExecuteProcedureCall pins the shape the completion and signature
// help features depend on: the two keywords collapse into one MultiKeyword and
// the call itself stays a FunctionLiteral, so the callee name and the argument
// list are still reachable.
func TestParseExecuteProcedureCall(t *testing.T) {
	stmts := parseInit(t, "execute procedure myproc(1, 2)")
	if len(stmts) != 1 {
		t.Fatalf("got %d statements, want 1", len(stmts))
	}

	var (
		multiKeyword *ast.MultiKeyword
		function     *ast.FunctionLiteral
	)
	for _, node := range stmts[0].GetTokens() {
		switch v := node.(type) {
		case *ast.MultiKeyword:
			multiKeyword = v
		case *ast.FunctionLiteral:
			function = v
		}
	}

	if multiKeyword == nil {
		t.Fatalf("no MultiKeyword in %v", stmts[0].GetTokens())
	}
	if got, want := multiKeyword.String(), "execute procedure"; got != want {
		t.Errorf("MultiKeyword.String() = %q, want %q", got, want)
	}
	if function == nil {
		t.Fatalf("no FunctionLiteral in %v", stmts[0].GetTokens())
	}
	if got, want := function.String(), "myproc(1, 2)"; got != want {
		t.Errorf("FunctionLiteral.String() = %q, want %q", got, want)
	}
}

// TestParseExecuteWithoutProcedureIsNotGrouped keeps PostgreSQL's and MSSQL's
// `EXECUTE <name>` intact: the group fires only when PROCEDURE follows.
func TestParseExecuteWithoutProcedureIsNotGrouped(t *testing.T) {
	stmts := parseInit(t, "execute stmt")
	for _, node := range stmts[0].GetTokens() {
		if _, ok := node.(*ast.MultiKeyword); ok {
			t.Fatalf("EXECUTE alone was grouped into a MultiKeyword: %v", stmts[0].GetTokens())
		}
	}
}
```

In `parser/parseutil/position_test.go`, add these cases to `TestCheckSyntaxPosition`'s `tests` slice:

```go
		{
			name: "execute procedure name position",
			text: "execute procedure ",
			pos: token.Pos{
				Line: 0,
				Col:  18,
			},
			want: ExecuteProcedure,
		},
		{
			name: "execute procedure name partially typed",
			text: "execute procedure my",
			pos: token.Pos{
				Line: 0,
				Col:  20,
			},
			want: ExecuteProcedure,
		},
		{
			// Regression pin. PrevNodesIs checks every path depth, so
			// "EXECUTE PROCEDURE" is a previous node here too. If the
			// ExecuteProcedure case is placed before isInsertColumns it steals
			// this position and the user typing arguments is offered procedure
			// names instead.
			name: "execute procedure argument list stays an argument list",
			text: "execute procedure myproc(1, ",
			pos: token.Pos{
				Line: 0,
				Col:  28,
			},
			want: InsertColumn,
		},
		{
			// multiKeywordMap is dialect-independent, so PostgreSQL's legacy
			// trigger syntax gets this position too. Recorded deliberately:
			// the completion branch retains CompletionTypeKeyword precisely so
			// that this costs a PostgreSQL user nothing.
			name: "postgresql legacy trigger execute procedure",
			text: "create trigger t after insert on x for each row execute procedure ",
			pos: token.Pos{
				Line: 0,
				Col:  66,
			},
			want: ExecuteProcedure,
		},
```

Create `parser/parseutil/call_test.go`:

```go
package parseutil

import (
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/parser"
	"github.com/sqls-server/sqls/token"
)

func walkerAt(t *testing.T, text string, col int) *NodeWalker {
	t.Helper()
	parsed, err := parser.ParseWithDriver(text, dialect.DatabaseDriverInterBase)
	if err != nil {
		t.Fatal("ParseWithDriver:", err)
	}
	return NewNodeWalker(parsed, token.Pos{Line: 0, Col: col})
}

func TestEnclosingCall(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		col        int
		wantOK     bool
		wantCallee string
		wantInside bool
	}{
		{
			name:       "inside an empty argument list",
			text:       "execute procedure myproc(",
			col:        25,
			wantOK:     true,
			wantCallee: "myproc",
			wantInside: true,
		},
		{
			name:       "inside a populated argument list",
			text:       "execute procedure myproc(1, 2)",
			col:        28,
			wantOK:     true,
			wantCallee: "myproc",
			wantInside: true,
		},
		{
			name:       "selectable procedure call",
			text:       "select * from myproc(",
			col:        21,
			wantOK:     true,
			wantCallee: "myproc",
			wantInside: true,
		},
		{
			name:       "built-in function call",
			text:       "select gen_id(",
			col:        14,
			wantOK:     true,
			wantCallee: "gen_id",
			wantInside: true,
		},
		{
			// On the callee name itself, not between the parentheses.
			// Signature help and generator completion both need this
			// distinction: neither fires while the name is still being typed.
			name:       "on the callee name",
			text:       "select gen_id(1, 2)",
			col:        10,
			wantOK:     true,
			wantCallee: "gen_id",
			wantInside: false,
		},
		{
			name:   "not in a call at all",
			text:   "select * from city",
			col:    16,
			wantOK: false,
		},
		{
			name:   "parenthesis that is not a call",
			text:   "select (1 + 2",
			col:    12,
			wantOK: false,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := EnclosingCall(walkerAt(t, tt.text, tt.col))
			if ok != tt.wantOK {
				t.Fatalf("EnclosingCall ok = %v, want %v (call=%+v)", ok, tt.wantOK, got)
			}
			if !tt.wantOK {
				return
			}
			if got.Callee != tt.wantCallee {
				t.Errorf("Callee = %q, want %q", got.Callee, tt.wantCallee)
			}
			if got.Inside != tt.wantInside {
				t.Errorf("Inside = %v, want %v", got.Inside, tt.wantInside)
			}
		})
	}
}

func TestCallInfoActiveParameter(t *testing.T) {
	const text = "execute procedure myproc(123, 45)"
	cases := []struct {
		col  int
		want int
	}{
		{col: 25, want: 0},
		{col: 28, want: 0},
		{col: 29, want: 1},
		{col: 31, want: 1},
	}

	for _, tt := range cases {
		pos := token.Pos{Line: 0, Col: tt.col}
		call, ok := EnclosingCall(walkerAt(t, text, tt.col))
		if !ok {
			t.Fatalf("col %d: no enclosing call", tt.col)
		}
		if got := call.ActiveParameter(pos); got != tt.want {
			t.Errorf("col %d: ActiveParameter = %d, want %d", tt.col, got, tt.want)
		}
	}
}

func TestCallInfoActiveParameterWithNoArguments(t *testing.T) {
	// An empty argument list has no ast.IdentifierList at all, so GetIndex is
	// unreachable. The answer is 0, never -1: the cursor is on the first
	// parameter, and a negative index would render as "no active parameter"
	// in the editor.
	call, ok := EnclosingCall(walkerAt(t, "execute procedure myproc(", 25))
	if !ok {
		t.Fatal("no enclosing call")
	}
	if got := call.ActiveParameter(token.Pos{Line: 0, Col: 25}); got != 0 {
		t.Errorf("ActiveParameter = %d, want 0 for an empty argument list", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestParseMultiKeyword|TestParseExecute' ./parser/ -v && go test -run 'TestCheckSyntaxPosition|TestEnclosingCall|TestCallInfo' ./parser/parseutil/ -v`
Expected: the `parseutil` tests FAIL to compile — `undefined: ExecuteProcedure`, `undefined: EnclosingCall`. Once those compile they FAIL on the two `ExecuteProcedure` cases. The `parser` tests FAIL on `"execute procedure keyword"` and `TestParseExecuteProcedureCall`, because the two keywords are separate tokens today. `TestParseExecuteWithoutProcedureIsNotGrouped` and the `InsertColumn` regression case PASS already — they are the guards, and they must stay green through Step 3.

`parser/parseutil` already imports `parser` in `position_test.go`, so the test-only import direction in `call_test.go` introduces no cycle.

If `parseInit` is not the helper name used by the surrounding tests in `parser/parser_test.go`, use whatever those tests use to parse a string into `[]*ast.Statement`; the assertions are unchanged.

- [ ] **Step 3: Write the minimal implementation**

In `parser/parser.go`, add one entry to `multiKeywordMap` (`:263-274`):

```go
var multiKeywordMap = map[string][]string{
	"ORDER":   {"BY"},
	"GROUP":   {"BY"},
	"INSERT":  {"INTO"},
	"DELETE":  {"FROM"},
	"EXECUTE": {"PROCEDURE"},
	"INNER":   {"JOIN"},
	"CROSS":   {"JOIN"},
	"OUTER":   {"JOIN"},
	"LEFT":    {"OUTER", "JOIN"},
	"RIGHT":   {"OUTER", "JOIN"},
	"NATURAL": {"LEFT", "RIGHT", "OUTER", "JOIN"},
}
```

In `parser/parseutil/position.go`, add the constant to the block (`:11-23`):

```go
	JoinClause       SyntaxPosition = "join_clause"
	JoinOn           SyntaxPosition = "join_on"
	ExecuteProcedure SyntaxPosition = "execute_procedure"
	Unknown          SyntaxPosition = "unknown"
```

and add the case **after** `case isInsertColumns(nw):` and before `default:` (`:99-107`):

```go
	case isInsertColumns(nw):
		if isInsertValues(nw) {
			res = InsertValue
		} else {
			res = InsertColumn
		}
	case nw.PrevNodesIs(true, genKeywordMatcher([]string{
		// InterBase EXECUTE PROCEDURE. Deliberately last: PrevNodesIs matches
		// at every path depth, so this keyword pair is also "previous" when
		// the cursor is inside the call's argument list, and an earlier case
		// would take that position away from isInsertColumns.
		"EXECUTE PROCEDURE",
	})):
		res = ExecuteProcedure
	default:
		res = Unknown
	}
```

Create `parser/parseutil/call.go`:

```go
package parseutil

import (
	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/ast/astutil"
	"github.com/sqls-server/sqls/token"
)

// CallInfo describes the innermost function-style call the cursor is in.
//
// parseFunctions groups NAME( into an ast.FunctionLiteral whose first token is
// the name and whose second is the ast.Parenthesis, but only when the
// parenthesis follows the name with no whitespace. "MYPROC (1, 2)" is
// therefore not a call here, which matches sqls's existing behaviour for
// built-in functions.
type CallInfo struct {
	// Callee is the unquoted name of the called function or procedure.
	Callee string
	// Inside is true when the cursor is between the parentheses rather than
	// on the callee name.
	Inside bool

	args *ast.IdentifierList
}

var callLiteralMatcher = astutil.NodeMatcher{
	NodeTypes: []ast.NodeType{ast.TypeFunctionLiteral},
}

var callParenthesisMatcher = astutil.NodeMatcher{
	NodeTypes: []ast.NodeType{ast.TypeParenthesis},
}

var callArgumentsMatcher = astutil.NodeMatcher{
	NodeTypes: []ast.NodeType{ast.TypeIdentifierList},
}

// EnclosingCall reports the innermost call the cursor is within, if any. It is
// node-shaped rather than position-shaped, which is why it is not part of
// CheckSyntaxPosition: "the callee is a known procedure" and "the cursor is in
// a GEN_ID( argument list" are facts about the AST, not about a keyword that
// precedes the cursor.
func EnclosingCall(nw *NodeWalker) (CallInfo, bool) {
	node := nw.CurNodeBottomMatched(callLiteralMatcher)
	if node == nil {
		return CallInfo{}, false
	}
	list, ok := node.(ast.TokenList)
	if !ok {
		return CallInfo{}, false
	}
	toks := list.GetTokens()
	if len(toks) == 0 {
		return CallInfo{}, false
	}

	call := CallInfo{Callee: toks[0].String()}
	if ident, ok := toks[0].(*ast.Identifier); ok {
		call.Callee = ident.NoQuoteString()
	}
	call.Inside = nw.CurNodeIs(callParenthesisMatcher)

	if args := nw.CurNodeBottomMatched(callArgumentsMatcher); args != nil {
		if identifiers, ok := args.(*ast.IdentifierList); ok {
			call.args = identifiers
		}
	}
	return call, true
}

// ActiveParameter is the 0-based index of the argument the cursor is on,
// reusing the same helper the INSERT ... VALUES path uses.
//
// An empty argument list has no ast.IdentifierList, and GetIndex returns -1
// for a position it does not enclose. Both answer 0 here: the cursor is on the
// first parameter, and a negative index renders as "no active parameter" in
// the editor.
func (c CallInfo) ActiveParameter(pos token.Pos) int {
	if c.args == nil {
		return 0
	}
	if idx := c.args.GetIndex(pos); idx >= 0 {
		return idx
	}
	return 0
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./parser/... -v 2>&1 | tail -40`
Expected: every test PASS, including the pre-existing `TestParseMultiKeyword` cases and every other `TestCheckSyntaxPosition` case.

- [ ] **Step 5: Verify no other dialect lost a position**

Run: `go test ./parser/... ./internal/completer/... ./internal/handler/... -count=1`
Expected: `ok` for all three. The only intended change is `Unknown` → `ExecuteProcedure` after the literal keyword pair. Any other test moving means the case was placed too early — move it back after `isInsertColumns`.

- [ ] **Step 6: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add parser/parser.go parser/parser_test.go parser/parseutil/position.go parser/parseutil/position_test.go parser/parseutil/call.go parser/parseutil/call_test.go
git commit -m "feat: group EXECUTE PROCEDURE and give it a syntax position"
```

---
### Task 5: Completion candidates for procedures, views, generators and UDFs

Spec §2 in full. This is the largest task in the plan and it is one task because every branch lands in the same two functions (`Complete` and `getCompletionTypes`) and the "exactly one candidate per view" rule is only testable once all of them exist together.

**The rule that is easy to get wrong.** `SortedViews()` does **not** subtract from `SortedTables()` — a view is already a table candidate wherever tables are offered. The separation is by **context, never by comparing names**:

- `ViewCandidates` runs only where `CompletionTypeView` is set and `CompletionTypeTable` is **not**. Per `getCompletionTypes` that is the `InsertColumn` position and the three member-identifier branches.
- Everywhere tables are offered, the existing table candidate *is* the view's candidate and merely gains the `view` detail when `DBCache.View(name)` hits.

There is no dedupe pass anywhere. Getting this wrong double-offers every view in every `FROM` clause.

**A narrowing of the spec, stated.** `ViewCandidates` returns nothing for a non-`ParentTypeNone` parent, so the member-identifier branches contribute nothing in practice. `world.<cursor>` asks for a member of `world`; a view name is not one. `InsertColumn` (parent `noneParent`) is the position that actually yields view candidates. The placement rule above is unchanged — this only declines to emit in positions where the emission would be wrong anyway.

**Two of the spec's three new completion types are deliberately not added.** §2 names `CompletionTypeProcedure`, `CompletionTypeProcedureName` and `CompletionTypeGenerator`. Only `CompletionTypeProcedureName` is added here:

- `CompletionTypeProcedure` would be set in exactly the branches that already set `CompletionTypeTable` (selectable procedures are legal wherever a relation is), so it would carry no information; the selectable-procedure candidates hang off the existing `CompletionTypeTable` branch.
- `CompletionTypeGenerator` cannot be produced by `getCompletionTypes` at all. That function sees only a `SyntaxPosition`; "the cursor is inside a `GEN_ID(` call" is a *node* fact, read from the enclosing `ast.FunctionLiteral`. An unset constant is dead code.

**Files:**
- Create: `internal/completer/interbase_candidates.go`
- Create: `internal/completer/interbase_candidates_test.go`
- Modify: `internal/completer/completer.go:22-37` (the new type), `:39-70` (`String()`), `:123-200` (`Complete`), `:212-249` (`getSortTextPrefix`), `:396-405` (`getCompletionTypes`)
- Modify: `internal/completer/candidates.go:45-82` (procedure output parameters as columns), `:384-402` (the `view` detail)

**Interfaces:**
- Consumes: `database.ProcedureDoc`, `database.ViewDoc`, `database.GeneratorDoc`, `database.FunctionDoc`, `database.ParameterDoc` (Task 3); `parseutil.ExecuteProcedure`, `parseutil.EnclosingCall` and `parseutil.CallInfo` (Task 4); from the contract — `DBCache.HasCatalog/Procedure/View/Generator/Function/SortedProcedures/SortedViews/SortedGenerators/SortedFunctions`.
- Produces:
  - `const CompletionTypeProcedureName completionType`
  - `func (c *Completer) ProcedureCandidates() []lsp.CompletionItem`
  - `func (c *Completer) SelectableProcedureCandidates() []lsp.CompletionItem`
  - `func (c *Completer) ViewCandidates(parent *completionParent) []lsp.CompletionItem`
  - `func (c *Completer) GeneratorCandidates() []lsp.CompletionItem`
  - `func (c *Completer) ExternalFunctionCandidates() []lsp.CompletionItem`
  - `func (c *Completer) procedureColumnCandidates(tableName string) []lsp.CompletionItem`
  - `func insideGenIDCall(nw *parseutil.NodeWalker) bool`

- [ ] **Step 1: Write the failing tests**

Create `internal/completer/interbase_candidates_test.go`:

```go
package completer

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

// interBaseCatalogCache is the shared fixture for every feature-2 test: two
// procedures (one selectable, one not), a view that is ALSO an ordinary table
// candidate — which is the contract's normal state, since SortedViews() does
// not subtract from SortedTables() — a generator, and a UDF with one
// unrenderable argument type.
func interBaseCatalogCache(t *testing.T) *database.DBCache {
	t.Helper()
	cache := interBaseBaseCache(t)
	cache.Catalog = &database.CatalogCache{
		Procedures: map[string]*database.ProcedureDesc{
			"MYPROC": {
				Name: "MYPROC",
				InputParameters: []*database.ProcedureParameterDesc{
					{Name: "IN_CODE", Position: 0, Direction: database.ParameterInput, Type: "VARCHAR(3)", Nullable: sql.NullBool{Bool: false, Valid: true}},
					{Name: "IN_AMOUNT", Position: 1, Direction: database.ParameterInput, Type: "NUMERIC(18, 2)", Nullable: sql.NullBool{}},
				},
				OutputParameters: []*database.ProcedureParameterDesc{
					{Name: "OUT_TOTAL", Position: 0, Direction: database.ParameterOutput, Type: "INTEGER", Nullable: sql.NullBool{}},
				},
			},
			"DOWORK": {
				Name: "DOWORK",
				InputParameters: []*database.ProcedureParameterDesc{
					{Name: "IN_ID", Position: 0, Direction: database.ParameterInput, Type: "INTEGER"},
				},
			},
		},
		Views: map[string]*database.ViewDesc{
			"MYVIEW": {
				Name:       "MYVIEW",
				ViewSource: sql.NullString{String: "SELECT ID FROM CITY", Valid: true},
				Columns: []*database.ColumnDesc{
					{ColumnBase: database.ColumnBase{Table: "MYVIEW", Name: "ID"}, Type: "INTEGER"},
				},
			},
		},
		Generators: map[string]*database.GeneratorDesc{
			"GEN_ORDER_ID": {Name: "GEN_ORDER_ID", ID: sql.NullInt64{Int64: 3, Valid: true}},
		},
		Functions: map[string]*database.FunctionDesc{
			"MYUDF": {
				Name:       "MYUDF",
				ReturnType: "DOUBLE PRECISION",
				ModuleName: sql.NullString{String: "udflib", Valid: true},
				EntryPoint: sql.NullString{String: "myudf", Valid: true},
				Arguments: []*database.FunctionArgumentDesc{
					{Position: sql.NullInt64{Int64: 1, Valid: true}, Type: "DOUBLE PRECISION"},
					{Position: sql.NullInt64{Int64: 2, Valid: true}, Type: ""},
				},
			},
		},
	}
	return cache
}

// interBaseBaseCache is the same cache with no Catalog at all: the window
// before the worker's secondary pass lands, and every non-InterBase driver.
func interBaseBaseCache(t *testing.T) *database.DBCache {
	t.Helper()
	columns := []*database.ColumnDesc{
		{ColumnBase: database.ColumnBase{Schema: "MAIN", Table: "CITY", Name: "ID"}, Type: "INTEGER"},
		{ColumnBase: database.ColumnBase{Schema: "MAIN", Table: "MYVIEW", Name: "ID"}, Type: "INTEGER"},
	}
	repo := &database.MockDBRepository{
		MockDatabase:  func(context.Context) (string, error) { return "MAIN", nil },
		MockDatabases: func(context.Context) ([]string, error) { return []string{"MAIN"}, nil },
		MockDatabaseTables: func(context.Context) (map[string][]string, error) {
			// MYVIEW is here because interBaseRelationsQuery selects every
			// non-system row of RDB$RELATIONS, which includes views.
			return map[string][]string{"MAIN": {"CITY", "MYVIEW"}}, nil
		},
		MockDescribeDatabaseTableBySchema: func(context.Context, string) ([]*database.ColumnDesc, error) {
			return columns, nil
		},
		MockDescribeForeignKeysBySchema: func(context.Context, string) ([]*database.ForeignKey, error) {
			return nil, nil
		},
	}
	cache, err := database.NewDBCacheUpdater(repo).GenerateDBCachePrimary(context.Background())
	if err != nil {
		t.Fatal("GenerateDBCachePrimary:", err)
	}
	return cache
}

func completeInterBase(t *testing.T, cache *database.DBCache, text string) []lsp.CompletionItem {
	t.Helper()
	c := NewCompleter(cache)
	c.Driver = dialect.DatabaseDriverInterBase
	got, err := c.Complete(text, lsp.CompletionParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			Position: lsp.Position{Line: 0, Character: len(text)},
		},
	}, false)
	if err != nil {
		t.Fatal("Complete:", err)
	}
	return got
}

func candidatesFor(items []lsp.CompletionItem, label string) []lsp.CompletionItem {
	matched := []lsp.CompletionItem{}
	for _, item := range items {
		if item.Label == label {
			matched = append(matched, item)
		}
	}
	return matched
}

func soleCandidate(t *testing.T, items []lsp.CompletionItem, label string) lsp.CompletionItem {
	t.Helper()
	matched := candidatesFor(items, label)
	if len(matched) == 0 {
		t.Fatalf("no candidate labelled %q in %v", label, completionLabels(items))
	}
	if len(matched) > 1 {
		t.Fatalf("%d candidates labelled %q, want exactly 1: %+v", len(matched), label, matched)
	}
	return matched[0]
}

func TestInterBaseProcedureCompletionAfterExecuteProcedure(t *testing.T) {
	got := completeInterBase(t, interBaseCatalogCache(t), "execute procedure ")

	item := soleCandidate(t, got, "MYPROC")
	if item.Kind != lsp.MethodCompletion {
		t.Errorf("kind = %v, want MethodCompletion", item.Kind)
	}
	if item.Detail != "procedure" {
		t.Errorf("detail = %q, want %q", item.Detail, "procedure")
	}
	// A procedure with no output is still executable, so both are offered
	// here — unlike the FROM position.
	if len(candidatesFor(got, "DOWORK")) != 1 {
		t.Errorf("DOWORK missing after EXECUTE PROCEDURE: %v", completionLabels(got))
	}
	// Tables are not relations here. If the ExecuteProcedure branch is written
	// to include CompletionTypeTable, this fails.
	if len(candidatesFor(got, "CITY")) != 0 {
		t.Errorf("table candidate offered after EXECUTE PROCEDURE: %v", completionLabels(got))
	}
	// Keywords are retained deliberately: the multi-keyword group fires for
	// every dialect, and a PostgreSQL user writing a legacy trigger must not
	// lose the candidates they get today.
	if !completionLabels(got)["SELECT"] {
		t.Errorf("keyword candidates were dropped: %v", completionLabels(got))
	}
}

func TestInterBaseCompletionAfterExecuteProcedureIsCaseInsensitive(t *testing.T) {
	got := completeInterBase(t, interBaseCatalogCache(t), "execute procedure myp")
	if len(candidatesFor(got, "MYPROC")) != 1 {
		t.Errorf("lowercase prefix %q did not match MYPROC: %v", "myp", completionLabels(got))
	}
}

func TestInterBaseSelectableProcedureCompletionInFromClause(t *testing.T) {
	got := completeInterBase(t, interBaseCatalogCache(t), "select * from ")

	item := soleCandidate(t, got, "MYPROC")
	if item.Kind != lsp.MethodCompletion {
		t.Errorf("kind = %v, want MethodCompletion", item.Kind)
	}
	if item.Detail != "selectable procedure" {
		t.Errorf("detail = %q, want %q", item.Detail, "selectable procedure")
	}
	// DOWORK has no output parameters, so it cannot appear in a FROM clause.
	if len(candidatesFor(got, "DOWORK")) != 0 {
		t.Errorf("a procedure with no output was offered as a relation: %v", completionLabels(got))
	}
}

func TestInterBaseViewInFromPositionIsOfferedExactlyOnce(t *testing.T) {
	cache := interBaseCatalogCache(t)
	// Precondition, so this test can never pass vacuously: the view must be
	// reachable through BOTH paths, which is the contract's normal state.
	if _, ok := cache.View("MYVIEW"); !ok {
		t.Fatal("fixture error: MYVIEW is not in the view catalog")
	}
	found := false
	for _, table := range cache.SortedTables() {
		if table == "MYVIEW" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fixture error: MYVIEW is not in SortedTables() %v", cache.SortedTables())
	}

	got := completeInterBase(t, cache, "select * from ")

	// soleCandidate fails loudly at 2, which is what a naive "append view
	// candidates alongside table candidates" implementation produces.
	item := soleCandidate(t, got, "MYVIEW")
	if item.Detail != "view" {
		t.Errorf("detail = %q, want %q", item.Detail, "view")
	}
	if item.Kind != lsp.ClassCompletion {
		t.Errorf("kind = %v, want ClassCompletion so the view sorts with tables", item.Kind)
	}
	if len(candidatesFor(got, "CITY")) != 1 {
		t.Errorf("ordinary table candidate changed: %v", completionLabels(got))
	}
	if detail := soleCandidate(t, got, "CITY").Detail; detail != "table" {
		t.Errorf("CITY detail = %q, want %q", detail, "table")
	}
}

func TestInterBaseViewCandidatesInInsertColumnPosition(t *testing.T) {
	// The one position where CompletionTypeView is set and
	// CompletionTypeTable is not.
	got := completeInterBase(t, interBaseCatalogCache(t), "insert into city (")
	if len(candidatesFor(got, "MYVIEW")) != 1 {
		t.Errorf("view candidate missing where CompletionTypeView is the only relation type: %v", completionLabels(got))
	}
}

func TestInterBaseProcedureOutputParameterColumnCompletion(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{name: "select list over a selectable procedure", text: "select  from myproc"},
		{name: "member identifier", text: "select myproc."},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCompleter(interBaseCatalogCache(t))
			c.Driver = dialect.DatabaseDriverInterBase
			character := len(tt.text)
			if tt.name == "select list over a selectable procedure" {
				character = 7
			}
			got, err := c.Complete(tt.text, lsp.CompletionParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					Position: lsp.Position{Line: 0, Character: character},
				},
			}, false)
			if err != nil {
				t.Fatal("Complete:", err)
			}

			item := soleCandidate(t, got, "OUT_TOTAL")
			if item.Kind != lsp.FieldCompletion {
				t.Errorf("kind = %v, want FieldCompletion", item.Kind)
			}
			if item.Detail != `column from "MYPROC"` {
				t.Errorf("detail = %q, want %q", item.Detail, `column from "MYPROC"`)
			}
			// Input parameters are never valid text in a statement: DSQL has
			// no named parameters.
			if len(candidatesFor(got, "IN_CODE")) != 0 {
				t.Errorf("an input parameter was offered as a column: %v", completionLabels(got))
			}
		})
	}
}

func TestInterBaseExternalFunctionCompletionInSelectExpr(t *testing.T) {
	got := completeInterBase(t, interBaseCatalogCache(t), "select ")

	item := soleCandidate(t, got, "MYUDF")
	if item.Kind != lsp.FunctionCompletion {
		t.Errorf("kind = %v, want FunctionCompletion", item.Kind)
	}
	if item.Detail != "external function" {
		t.Errorf("detail = %q, want %q", item.Detail, "external function")
	}
	// Built-in functions keep coming: a UDF is callable exactly where one is.
	if !completionLabels(got)["GEN_ID"] {
		t.Errorf("built-in function candidates were lost: %v", completionLabels(got))
	}
}

func TestInterBaseExternalFunctionCompletionWithUnrenderableArgumentType(t *testing.T) {
	got := completeInterBase(t, interBaseCatalogCache(t), "select ")

	item := soleCandidate(t, got, "MYUDF")
	if item.Documentation == nil {
		t.Fatal("the UDF candidate carries no documentation")
	}
	doc := item.Documentation.Value
	if !strings.Contains(doc, "argument 1: `DOUBLE PRECISION`") {
		t.Errorf("documentation lost the renderable argument:\n%s", doc)
	}
	if !strings.Contains(doc, "argument 2") {
		t.Errorf("documentation dropped the unrenderable argument entirely:\n%s", doc)
	}
	// The object must be completable by name even though one argument type is
	// permanently unrenderable, and no placeholder may appear anywhere in it.
	for _, forbidden := range []string{"<unknown>", "argument 2: ", "``", "UNKNOWN"} {
		if strings.Contains(doc, forbidden) {
			t.Errorf("documentation contains the placeholder %q:\n%s", forbidden, doc)
		}
	}
}

func TestInterBaseGeneratorCompletionInsideGenId(t *testing.T) {
	got := completeInterBase(t, interBaseCatalogCache(t), "select gen_id(")

	item := soleCandidate(t, got, "GEN_ORDER_ID")
	if item.Kind != lsp.ValueCompletion {
		t.Errorf("kind = %v, want ValueCompletion", item.Kind)
	}
	if item.Detail != "generator" {
		t.Errorf("detail = %q, want %q", item.Detail, "generator")
	}
}

func TestInterBaseGeneratorCompletionIsScopedToGenId(t *testing.T) {
	// Offering every generator in every expression position would bury column
	// candidates. These three are the positions a naive implementation leaks
	// into.
	for _, text := range []string{"select upper(", "select ", "select * from "} {
		got := completeInterBase(t, interBaseCatalogCache(t), text)
		if len(candidatesFor(got, "GEN_ORDER_ID")) != 0 {
			t.Errorf("generator offered at %q: %v", text, completionLabels(got))
		}
	}
}

func TestInterBaseCompletionDegradesWithoutCatalog(t *testing.T) {
	// The window between initialize and the worker's first successful
	// secondary pass, and every ordinary build.
	cache := interBaseBaseCache(t)
	if cache.HasCatalog() {
		t.Fatal("fixture error: the base cache must have no catalog")
	}

	got := completeInterBase(t, cache, "select * from ")
	if len(candidatesFor(got, "CITY")) != 1 {
		t.Errorf("today's table candidates were lost: %v", completionLabels(got))
	}
	for _, absent := range []string{"MYPROC", "DOWORK", "GEN_ORDER_ID", "MYUDF"} {
		if len(candidatesFor(got, absent)) != 0 {
			t.Errorf("%q was offered with no catalog: %v", absent, completionLabels(got))
		}
	}

	if got := completeInterBase(t, cache, "execute procedure "); len(got) == 0 {
		t.Error("completion returned nothing at all; keywords must still be offered")
	}
	if got := completeInterBase(t, cache, "select gen_id("); len(candidatesFor(got, "GEN_ORDER_ID")) != 0 {
		t.Error("a generator was offered with no catalog")
	}
}

func TestInterBaseCandidatesAreNotOfferedToOtherDrivers(t *testing.T) {
	c := NewCompleter(interBaseCatalogCache(t))
	c.Driver = dialect.DatabaseDriverPostgreSQL

	got, err := c.Complete("execute procedure ", lsp.CompletionParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			Position: lsp.Position{Line: 0, Character: 18},
		},
	}, false)
	if err != nil {
		t.Fatal("Complete:", err)
	}
	if len(candidatesFor(got, "MYPROC")) != 0 {
		t.Errorf("a procedure candidate reached a PostgreSQL user: %v", completionLabels(got))
	}
	// This is the mitigation for the shared multiKeywordMap change: the
	// PostgreSQL user keeps exactly the candidates they get today.
	if len(got) == 0 {
		t.Error("the PostgreSQL user received nothing; keyword candidates must be retained")
	}
}

func TestCompletionKindSortPrefixesAreAssigned(t *testing.T) {
	cases := []struct {
		kind lsp.CompletionItemKind
		want string
	}{
		{lsp.FieldCompletion, "0"},
		{lsp.ClassCompletion, "1"},
		{lsp.FunctionCompletion, "10"},
		{lsp.MethodCompletion, "10"},
		{lsp.ValueCompletion, "11"},
		{lsp.KeywordCompletion, "9999"},
	}
	for _, tt := range cases {
		if got := getSortTextPrefix(tt.kind); got != tt.want {
			t.Errorf("getSortTextPrefix(%v) = %q, want %q", tt.kind, got, tt.want)
		}
	}
	// The failure this pins: a new kind left in the catch-all list sorts below
	// every keyword, which is worse than not offering it.
	for _, kind := range []lsp.CompletionItemKind{lsp.MethodCompletion, lsp.ValueCompletion} {
		if getSortTextPrefix(kind) == getSortTextPrefix(lsp.KeywordCompletion) {
			t.Errorf("kind %v is still in the keyword sort bucket", kind)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestInterBase|TestCompletionKindSortPrefixes' ./internal/completer/ -v`
Expected: the file compiles — every symbol it names already exists, either in the completer or in sub-project 2's contract — and then:

- `TestCompletionKindSortPrefixesAreAssigned` FAILS with `getSortTextPrefix(2) = "9999", want "10"`: `MethodCompletion` and `ValueCompletion` are in the catch-all list today.
- Every `TestInterBase…` catalog test FAILS on a missing candidate, e.g. `no candidate labelled "MYPROC"`.
- `TestInterBaseViewInFromPositionIsOfferedExactlyOnce` FAILS on the `view` detail, not on the count: the view is already offered once, as a table.
- `TestInterBaseCompletionDegradesWithoutCatalog` and `TestInterBaseCandidatesAreNotOfferedToOtherDrivers` PASS from the start — they are the guards, and they must stay green through Step 3.

`completionLabels` already exists in `internal/completer/completer_test.go`; do not redeclare it.

- [ ] **Step 3: Write the minimal implementation**

Create `internal/completer/interbase_candidates.go`:

```go
package completer

import (
	"strings"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/parser/parseutil"
)

// genIDFunctionName is the only call in which a generator name is required.
// Offering every generator in every expression position would bury column
// candidates.
const genIDFunctionName = "GEN_ID"

// catalogEnabled gates every candidate generator in this file. HasCatalog() is
// false both on a non-InterBase connection and in the window before the
// worker's secondary catalog pass lands, and in both cases completion must
// degrade silently to today's behaviour.
func (c *Completer) catalogEnabled() bool {
	return c.Driver == dialect.DatabaseDriverInterBase && c.DBCache.HasCatalog()
}

// ProcedureCandidates offers every procedure, for the EXECUTE PROCEDURE
// position. A procedure with no output parameters is still executable, so it
// is offered here even though it cannot appear in a FROM clause.
func (c *Completer) ProcedureCandidates() []lsp.CompletionItem {
	if !c.catalogEnabled() {
		return nil
	}
	candidates := []lsp.CompletionItem{}
	for _, name := range c.DBCache.SortedProcedures() {
		desc, ok := c.DBCache.Procedure(name)
		if !ok {
			continue
		}
		candidates = append(candidates, procedureCandidate(desc, "procedure"))
	}
	return candidates
}

// SelectableProcedureCandidates offers only procedures with at least one
// output parameter: an InterBase selectable procedure is legal wherever a
// relation is, and one without output is not selectable.
func (c *Completer) SelectableProcedureCandidates() []lsp.CompletionItem {
	if !c.catalogEnabled() {
		return nil
	}
	candidates := []lsp.CompletionItem{}
	for _, name := range c.DBCache.SortedProcedures() {
		desc, ok := c.DBCache.Procedure(name)
		if !ok || len(desc.OutputParameters) == 0 {
			continue
		}
		candidates = append(candidates, procedureCandidate(desc, "selectable procedure"))
	}
	return candidates
}

func procedureCandidate(desc *database.ProcedureDesc, detail string) lsp.CompletionItem {
	return lsp.CompletionItem{
		Label:  desc.Name,
		Kind:   lsp.MethodCompletion,
		Detail: detail,
		Documentation: &lsp.MarkupContent{
			Kind:  lsp.Markdown,
			Value: database.ProcedureDoc(desc),
		},
	}
}

// ViewCandidates is called only where CompletionTypeView is set and
// CompletionTypeTable is not. Views stay in SchemaTables, so in every position
// that offers tables the existing table candidate already represents the view
// and gains only the "view" detail; emitting here as well would offer every
// view twice.
//
// A non-ParentTypeNone parent yields nothing: those branches are the
// member-identifier positions, and a view name is not a member of a table.
func (c *Completer) ViewCandidates(parent *completionParent) []lsp.CompletionItem {
	if !c.catalogEnabled() || parent.Type != ParentTypeNone {
		return nil
	}
	candidates := []lsp.CompletionItem{}
	for _, name := range c.DBCache.SortedViews() {
		desc, ok := c.DBCache.View(name)
		if !ok {
			continue
		}
		candidates = append(candidates, viewCandidate(desc))
	}
	return candidates
}

func viewCandidate(desc *database.ViewDesc) lsp.CompletionItem {
	return lsp.CompletionItem{
		Label:  desc.Name,
		Kind:   lsp.ClassCompletion,
		Detail: "view",
		Documentation: &lsp.MarkupContent{
			Kind:  lsp.Markdown,
			Value: database.ViewDoc(desc),
		},
	}
}

func (c *Completer) GeneratorCandidates() []lsp.CompletionItem {
	if !c.catalogEnabled() {
		return nil
	}
	candidates := []lsp.CompletionItem{}
	for _, name := range c.DBCache.SortedGenerators() {
		desc, ok := c.DBCache.Generator(name)
		if !ok {
			continue
		}
		candidates = append(candidates, lsp.CompletionItem{
			Label:  desc.Name,
			Kind:   lsp.ValueCompletion,
			Detail: "generator",
			Documentation: &lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: database.GeneratorDoc(desc),
			},
		})
	}
	return candidates
}

// ExternalFunctionCandidates offers UDFs beside the built-in functions. A UDF
// whose argument types the catalog cannot render is still offered: the name is
// what the user needs.
func (c *Completer) ExternalFunctionCandidates() []lsp.CompletionItem {
	if !c.catalogEnabled() {
		return nil
	}
	candidates := []lsp.CompletionItem{}
	for _, name := range c.DBCache.SortedFunctions() {
		desc, ok := c.DBCache.Function(name)
		if !ok {
			continue
		}
		candidates = append(candidates, lsp.CompletionItem{
			Label:  desc.Name,
			Kind:   lsp.FunctionCompletion,
			Detail: "external function",
			Documentation: &lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: database.FunctionDoc(desc),
			},
		})
	}
	return candidates
}

// procedureColumnCandidates turns a selectable procedure's output parameters
// into column candidates. Input parameters are deliberately absent: DSQL has
// no named parameters, so an input parameter name is never valid statement
// text.
func (c *Completer) procedureColumnCandidates(tableName string) []lsp.CompletionItem {
	if !c.catalogEnabled() {
		return nil
	}
	desc, ok := c.DBCache.Procedure(tableName)
	if !ok {
		return nil
	}
	candidates := []lsp.CompletionItem{}
	for _, param := range desc.OutputParameters {
		candidates = append(candidates, lsp.CompletionItem{
			Label:  param.Name,
			Kind:   lsp.FieldCompletion,
			Detail: columnDetail(desc.Name),
			Documentation: &lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: database.ParameterDoc(param),
			},
		})
	}
	return candidates
}

// insideGenIDCall reports whether the cursor is in the argument list of a
// GEN_ID call. Being on the callee name is not enough: the user typing
// "gen_i" wants the function, not a generator.
func insideGenIDCall(nw *parseutil.NodeWalker) bool {
	call, ok := parseutil.EnclosingCall(nw)
	if !ok || !call.Inside {
		return false
	}
	return strings.EqualFold(call.Callee, genIDFunctionName)
}
```

In `internal/completer/completer.go`, add the completion type to the `const` block (`:22-37`), after `CompletionTypeView`:

```go
	CompletionTypeView
	CompletionTypeProcedureName
	CompletionTypeSubQuery
```

and a `String()` case (`:39-70`), after the `CompletionTypeView` case:

```go
	case CompletionTypeProcedureName:
		return "ProcedureName"
```

In `Complete`, inside the `if c.DBCache != nil` block: append to the `CompletionTypeTable` branch (`:138-148`), after `items = append(items, candidates...)`:

```go
		if completionTypeIs(ctx.types, CompletionTypeTable) {
			excl := definedTables
			if completionTypeIs(ctx.types, CompletionTypeJoin) {
				excl = nil
			}
			candidates := c.TableCandidates(ctx.parent, excl)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
			// An InterBase selectable procedure is legal wherever a relation
			// is. Views are deliberately NOT added here: they are already in
			// SchemaTables, so TableCandidates has offered them once.
			items = append(items, c.SelectableProcedureCandidates()...)
		}
		if completionTypeIs(ctx.types, CompletionTypeProcedureName) {
			items = append(items, c.ProcedureCandidates()...)
		}
		if completionTypeIs(ctx.types, CompletionTypeView) && !completionTypeIs(ctx.types, CompletionTypeTable) {
			items = append(items, c.ViewCandidates(ctx.parent)...)
		}
		if insideGenIDCall(nodeWalker) {
			items = append(items, c.GeneratorCandidates()...)
		}
```

Append to the `CompletionTypeFunction` branch (`:192-195`):

```go
	if completionTypeIs(ctx.types, CompletionTypeFunction) {
		drivers := dialect.DataBaseFunctions(c.Driver)
		items = append(items, c.functionCandidates(lowercaseKeywords, drivers)...)
		// A UDF is callable exactly where a built-in function is, so it needs
		// no new completion type and no new context.
		items = append(items, c.ExternalFunctionCandidates()...)
	}
```

Replace `getSortTextPrefix` (`:212-249`) so the two new kinds leave the keyword bucket:

```go
func getSortTextPrefix(kind lsp.CompletionItemKind) string {
	switch kind {
	case lsp.SnippetCompletion:
		return "00"
	case lsp.FieldCompletion:
		return "0"
	case lsp.ClassCompletion:
		return "1"
	case lsp.ModuleCompletion:
		return "2"
	case lsp.FunctionCompletion:
		return "10"
	case lsp.MethodCompletion:
		// Procedures, beside the external functions they resemble.
		return "10"
	case lsp.ValueCompletion:
		// Generators, just below procedures and functions.
		return "11"
	case
		lsp.ColorCompletion,
		lsp.ConstantCompletion,
		lsp.ConstructorCompletion,
		lsp.EnumCompletion,
		lsp.EnumMemberCompletion,
		lsp.EventCompletion,
		lsp.FileCompletion,
		lsp.FolderCompletion,
		lsp.InterfaceCompletion,
		lsp.KeywordCompletion,
		lsp.OperatorCompletion,
		lsp.PropertyCompletion,
		lsp.ReferenceCompletion,
		lsp.StructCompletion,
		lsp.TextCompletion,
		lsp.TypeParameterCompletion,
		lsp.UnitCompletion,
		lsp.VariableCompletion:
		return "9999"
	default:
		return "9999"
	}
}
```

In `getCompletionTypes`, add the branch before `default:` (`:396-405`):

```go
	case syntaxPos == parseutil.InsertColumn:
		t = []completionType{
			CompletionTypeColumn,
			CompletionTypeView,
		}
	case syntaxPos == parseutil.ExecuteProcedure:
		// CompletionTypeKeyword is retained deliberately. multiKeywordMap is
		// dialect-independent, so PostgreSQL's legacy
		// "CREATE TRIGGER … EXECUTE PROCEDURE f()" reaches this branch too;
		// without the keywords a PostgreSQL user would receive nothing at all,
		// because no procedure cache exists for that driver.
		t = []completionType{
			CompletionTypeProcedureName,
			CompletionTypeKeyword,
		}
	default:
```

In `internal/completer/candidates.go`, extend `columnCandidates` (`:45-82`) with the procedure fallback in both relevant branches:

```go
	case ParentTypeNone:
		for _, table := range targetTables {
			if table.DatabaseSchema != "" && table.Name != "" {
				columns, ok := c.DBCache.ColumnDatabase(table.DatabaseSchema, table.Name)
				if !ok {
					continue
				}
				candidates = append(candidates, generateColumnCandidates(table.Name, columns)...)
			} else if table.Name != "" {
				columns, ok := c.DBCache.ColumnDescs(table.Name)
				if ok {
					candidates = append(candidates, generateColumnCandidates(table.Name, columns)...)
					continue
				}
				// The relation is not a table. On InterBase it may be a
				// selectable procedure, whose output parameters are its
				// columns.
				candidates = append(candidates, c.procedureColumnCandidates(table.Name)...)
			}
		}
	case ParentTypeSchema:
		// pass
	case ParentTypeTable:
		for _, table := range targetTables {
			if table.Name != parent.Name && table.Alias != parent.Name {
				continue
			}
			columns, ok := c.DBCache.ColumnDescs(table.Name)
			if ok {
				candidates = append(candidates, generateColumnCandidates(table.Name, columns)...)
				continue
			}
			candidates = append(candidates, c.procedureColumnCandidates(table.Name)...)
		}
```

and give an existing table candidate the `view` detail (`:384-402`):

```go
func generateTableCandidates(tables []string, dbCache *database.DBCache) []lsp.CompletionItem {
	candidates := []lsp.CompletionItem{}
	for _, tableName := range tables {
		candidate := lsp.CompletionItem{
			Label:  tableName,
			Kind:   lsp.ClassCompletion,
			Detail: "table",
		}
		// Views live in SchemaTables alongside tables, so this candidate is
		// already the view's only candidate. It is relabelled here rather than
		// duplicated by a separate view generator. DBCache.View is nil-safe
		// and returns false for every driver without a catalog, so no other
		// driver's output changes.
		if view, ok := dbCache.View(tableName); ok {
			candidate.Detail = "view"
			candidate.Documentation = &lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: database.ViewDoc(view),
			}
			candidates = append(candidates, candidate)
			continue
		}
		cols, ok := dbCache.ColumnDescs(tableName)
		if ok {
			candidate.Documentation = &lsp.MarkupContent{
				Kind:  lsp.Markdown,
				Value: database.TableDoc(tableName, cols),
			}
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestInterBase|TestCompletionKindSortPrefixes' ./internal/completer/ -v`
Expected: every test and subtest PASS.

- [ ] **Step 5: Prove the single-view test is not vacuous**

`TestInterBaseViewInFromPositionIsOfferedExactlyOnce` is the test most likely to pass against a wrong implementation, so verify it fails against the wrong one. Temporarily change the `CompletionTypeView` branch in `Complete` to drop its guard:

```go
		if completionTypeIs(ctx.types, CompletionTypeView) {
			items = append(items, c.ViewCandidates(ctx.parent)...)
		}
```

Run: `go test -run 'TestInterBaseViewInFromPositionIsOfferedExactlyOnce' ./internal/completer/ -v`
Expected: FAIL with `2 candidates labelled "MYVIEW", want exactly 1`. **Revert the temporary change** and re-run to confirm PASS. If it still passed, the fixture's view is not reachable through both paths and the two `t.Fatal` preconditions at the top of the test need fixing before moving on.

- [ ] **Step 6: Verify no existing completion behaviour moved**

Run: `go test ./internal/completer/... ./internal/handler/... -count=1 -v 2>&1 | grep -E '^(--- )?(FAIL|ok)'`
Expected: no `FAIL`. `TestComplete`, `TestCompleteInterBaseDialect1DollarIdentifier`, `TestCompleteInterBaseJoin…` and `internal/handler`'s `TestCompletion…` all assert today's output and must be unchanged — none of them has a catalog, so every new branch is inert for them.

- [ ] **Step 7: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 8: Commit**

```bash
git add internal/completer/interbase_candidates.go internal/completer/interbase_candidates_test.go internal/completer/completer.go internal/completer/candidates.go
git commit -m "feat: complete procedures, views, generators and external functions"
```

---
### Task 6: Signature help for procedure calls

Spec §3. `SignatureHelpWithDriver` (`internal/handler/signature_help.go:43`) serves exactly one case today, `SignatureHelpTypeInsertValue`, and gets its active-parameter index from `ast.IdentifierList.GetIndex(pos)`. The new branch reuses that same index helper through `parseutil.EnclosingCall` (Task 4).

**The trigger is "the callee is a known procedure", not "EXECUTE PROCEDURE precedes it."** That makes the same code serve `SELECT * FROM MYPROC(?)`, which is the other place procedure arguments are typed, and it means the branch needs no syntax position at all — `CheckSyntaxPosition` is position-shaped, and this is a node fact.

**Known limitation, not worked around:** `MYPROC (1, 2)` with a space before the parenthesis is not grouped into a `FunctionLiteral` by `parseFunctions` (`parser/parser.go:213` requires the parenthesis to follow immediately), so no signature help appears. This matches today's behaviour for built-in functions.

**Files:**
- Modify: `internal/handler/signature_help.go:43-107` (the new branch), `:109-124` (the enum)
- Modify: `internal/handler/signature_help_test.go` (append)

**Interfaces:**
- Consumes: `parseutil.EnclosingCall` and `parseutil.CallInfo.ActiveParameter` (Task 4); `database.ProcedureSignatureLabel`, `database.ProcedureSignatureDoc`, `database.ParameterDoc` (Task 3); `DBCache.Procedure` from the contract.
- Produces:
  - `const SignatureHelpTypeExecuteProcedure signatureHelpType`
  - `func procedureSignatureHelp(desc *database.ProcedureDesc, activeParameter int) *lsp.SignatureHelp`

- [ ] **Step 1: Write the failing tests**

Append to `internal/handler/signature_help_test.go`:

```go
func interBaseSignatureCache(t *testing.T) *database.DBCache {
	t.Helper()
	return &database.DBCache{
		Catalog: &database.CatalogCache{
			Procedures: map[string]*database.ProcedureDesc{
				"MYPROC": {
					Name: "MYPROC",
					InputParameters: []*database.ProcedureParameterDesc{
						{Name: "IN_CODE", Position: 0, Direction: database.ParameterInput, Type: "VARCHAR(3)", Nullable: sql.NullBool{Bool: false, Valid: true}},
						{Name: "IN_AMOUNT", Position: 1, Direction: database.ParameterInput, Type: "NUMERIC(18, 2)", Nullable: sql.NullBool{}},
					},
					OutputParameters: []*database.ProcedureParameterDesc{
						{Name: "OUT_TOTAL", Position: 0, Direction: database.ParameterOutput, Type: "INTEGER", Nullable: sql.NullBool{}},
					},
				},
			},
		},
	}
}

func wantProcedureSignature(activeParameter int) lsp.SignatureHelp {
	return lsp.SignatureHelp{
		Signatures: []lsp.SignatureInformation{
			{
				Label:         "MYPROC (IN_CODE, IN_AMOUNT)",
				Documentation: "MYPROC procedure — 2 input parameters, 1 output parameter",
				Parameters: []lsp.ParameterInformation{
					{
						Label:         "IN_CODE",
						Documentation: "`VARCHAR(3)` input NOT NULL",
					},
					{
						// Nullable is invalid, which is the normal case. No
						// nullability word appears at all.
						Label:         "IN_AMOUNT",
						Documentation: "`NUMERIC(18, 2)` input",
					},
				},
			},
		},
		ActiveSignature: 0.0,
		ActiveParameter: float64(activeParameter),
	}
}

func TestInterBaseSignatureHelpForExecuteProcedure(t *testing.T) {
	const input = "execute procedure myproc(123, 45)"
	cases := []struct {
		col                 int
		wantActiveParameter int
	}{
		{col: 25, wantActiveParameter: 0},
		{col: 28, wantActiveParameter: 0},
		{col: 29, wantActiveParameter: 1},
		{col: 31, wantActiveParameter: 1},
	}

	for _, tt := range cases {
		t.Run(fmt.Sprintf("col %d", tt.col), func(t *testing.T) {
			got, err := SignatureHelpWithDriver(input, lsp.SignatureHelpParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
					Position:     lsp.Position{Line: 0, Character: tt.col},
				},
			}, interBaseSignatureCache(t), dialect.DatabaseDriverInterBase)
			if err != nil {
				t.Fatal("SignatureHelpWithDriver:", err)
			}
			if got == nil {
				t.Fatal("no signature help for a known procedure call")
			}
			if diff := cmp.Diff(wantProcedureSignature(tt.wantActiveParameter), *got); diff != "" {
				t.Errorf("signature help mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestInterBaseSignatureHelpForSelectableProcedureCall(t *testing.T) {
	// The same signature, reached without EXECUTE PROCEDURE at all. This is
	// why the trigger is "the callee is a known procedure".
	const input = "select * from myproc("
	got, err := SignatureHelpWithDriver(input, lsp.SignatureHelpParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: len(input)},
		},
	}, interBaseSignatureCache(t), dialect.DatabaseDriverInterBase)
	if err != nil {
		t.Fatal("SignatureHelpWithDriver:", err)
	}
	if got == nil {
		t.Fatal("no signature help for a selectable procedure call")
	}
	if diff := cmp.Diff(wantProcedureSignature(0), *got); diff != "" {
		t.Errorf("signature help mismatch (-want +got):\n%s", diff)
	}
}

func TestInterBaseSignatureHelpOmitsUnknownNullability(t *testing.T) {
	const input = "execute procedure myproc("
	got, err := SignatureHelpWithDriver(input, lsp.SignatureHelpParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: len(input)},
		},
	}, interBaseSignatureCache(t), dialect.DatabaseDriverInterBase)
	if err != nil {
		t.Fatal("SignatureHelpWithDriver:", err)
	}
	if got == nil {
		t.Fatal("no signature help")
	}

	params := got.Signatures[0].Parameters
	if len(params) != 2 {
		t.Fatalf("got %d parameters, want 2", len(params))
	}
	if !strings.Contains(params[0].Documentation, "NOT NULL") {
		t.Errorf("a proven-non-nullable parameter lost its NOT NULL: %q", params[0].Documentation)
	}
	// The assertion that fails against a renderer that maps unknown to a word.
	for _, forbidden := range []string{"NOT NULL", "nullable", "unknown", "NULL"} {
		if strings.Contains(params[1].Documentation, forbidden) {
			t.Errorf("unknown nullability rendered %q in %q", forbidden, params[1].Documentation)
		}
	}
}

func TestSignatureHelpUnknownCalleeReturnsNil(t *testing.T) {
	for _, input := range []string{
		"select upper(",
		"execute procedure nosuchproc(",
		"select * from city",
	} {
		got, err := SignatureHelpWithDriver(input, lsp.SignatureHelpParams{
			TextDocumentPositionParams: lsp.TextDocumentPositionParams{
				TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
				Position:     lsp.Position{Line: 0, Character: len(input)},
			},
		}, interBaseSignatureCache(t), dialect.DatabaseDriverInterBase)
		if err != nil {
			t.Fatalf("SignatureHelpWithDriver(%q): %v", input, err)
		}
		if got != nil {
			t.Errorf("SignatureHelpWithDriver(%q) = %+v, want nil", input, got)
		}
	}
}

func TestSignatureHelpWithoutCatalogReturnsNil(t *testing.T) {
	// The window before the worker's catalog pass lands, and every other
	// driver: today's behaviour, unchanged.
	const input = "execute procedure myproc("
	got, err := SignatureHelpWithDriver(input, lsp.SignatureHelpParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: len(input)},
		},
	}, &database.DBCache{}, dialect.DatabaseDriverInterBase)
	if err != nil {
		t.Fatal("SignatureHelpWithDriver:", err)
	}
	if got != nil {
		t.Errorf("SignatureHelpWithDriver = %+v, want nil with no catalog", got)
	}
}
```

Add `"database/sql"`, `"strings"` and `"github.com/sqls-server/sqls/dialect"` to the file's imports; `fmt`, `cmp`, `database` and `lsp` are already there.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'SignatureHelp' ./internal/handler/ -v`
Expected: the four InterBase tests FAIL with "no signature help for a known procedure call" — `SignatureHelpWithDriver` falls through to `default: return nil, nil` today. `TestSignatureHelpUnknownCalleeReturnsNil`, `TestSignatureHelpWithoutCatalogReturnsNil` and the existing `signatureHelpTestCases` PASS from the start; they are the guards and must stay green.

- [ ] **Step 3: Write the minimal implementation**

In `internal/handler/signature_help.go`, add the enum member (`:111-115`):

```go
const (
	_ signatureHelpType = iota
	SignatureHelpTypeInsertValue
	SignatureHelpTypeExecuteProcedure
	SignatureHelpTypeUnknown = 99
)
```

and its `String()` case (`:117-124`):

```go
	case SignatureHelpTypeExecuteProcedure:
		return "ExecuteProcedure"
```

Insert the branch into `SignatureHelpWithDriver` immediately before `case signatureHelpIs(types, SignatureHelpTypeInsertValue):` (`:60-61`):

```go
	switch {
	case procedureCallSignature(nodeWalker, pos, dbCache) != nil:
		// Keyed on "the callee is a known procedure" rather than on a
		// preceding EXECUTE PROCEDURE, so the same code serves
		// SELECT * FROM MYPROC(?), which is the other place procedure
		// arguments are typed. CheckSyntaxPosition is not consulted: it is
		// position-shaped and this is a node fact.
		return procedureCallSignature(nodeWalker, pos, dbCache), nil
	case signatureHelpIs(types, SignatureHelpTypeInsertValue):
```

and add, below `SignatureHelpWithDriver`:

```go
// procedureCallSignature returns signature help when the cursor is inside the
// argument list of a call whose callee is a cached procedure, and nil
// otherwise. It is nil-safe on every input, which is what lets the switch
// above use it as a guard.
func procedureCallSignature(nw *parseutil.NodeWalker, pos token.Pos, dbCache *database.DBCache) *lsp.SignatureHelp {
	if !dbCache.HasCatalog() {
		return nil
	}
	call, ok := parseutil.EnclosingCall(nw)
	if !ok || !call.Inside {
		return nil
	}
	// The accessor normalises the name it is given; the identifier text goes
	// in exactly as the user typed it.
	desc, ok := dbCache.Procedure(call.Callee)
	if !ok {
		return nil
	}
	return procedureSignatureHelp(desc, call.ActiveParameter(pos))
}

// procedureSignatureHelp renders the tooltip. Output parameters are not
// arguments, so they are not offered as signature parameters, but their count
// appears in the documentation so the user can tell a selectable procedure
// from an executable one.
func procedureSignatureHelp(desc *database.ProcedureDesc, activeParameter int) *lsp.SignatureHelp {
	params := []lsp.ParameterInformation{}
	for _, param := range desc.InputParameters {
		params = append(params, lsp.ParameterInformation{
			Label:         param.Name,
			Documentation: database.ParameterDoc(param),
		})
	}
	return &lsp.SignatureHelp{
		Signatures: []lsp.SignatureInformation{
			{
				Label:         database.ProcedureSignatureLabel(desc),
				Documentation: database.ProcedureSignatureDoc(desc),
				Parameters:    params,
			},
		},
		ActiveSignature: 0.0,
		ActiveParameter: float64(activeParameter),
	}
}
```

Calling `procedureCallSignature` twice is deliberate: it is pure, allocation-light and runs only when the cursor is inside a parenthesis, and the alternative is restructuring the whole `switch` into `if`s, which would move the existing `InsertValue` code for no behavioural gain.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'SignatureHelp' ./internal/handler/ -v`
Expected: every test and subtest PASS, including all 18 existing `signatureHelpTestCases`.

- [ ] **Step 5: Verify the insert path is untouched for other drivers**

Run: `go test -run 'TestSignatureHelpMain|signatureHelp' ./internal/handler/ -count=1 -v 2>&1 | grep -c -- '--- PASS'`
Expected: a non-zero count with no `--- FAIL`. The new branch returns nil for every non-InterBase fixture, because none has a catalog.

- [ ] **Step 6: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/handler/signature_help.go internal/handler/signature_help_test.go
git commit -m "feat: signature help for EXECUTE PROCEDURE and selectable procedure calls"
```

---

### Task 7: Hover backed by real DDL

Spec §4 and the "DDL is unsupported (hover)", "The object is gone from the catalog" and "A trigger event … is unknown" entries of User-Visible Behavior.

**Where the spec and the code disagree, and how it is resolved.** §4 says the database-backed part runs "after the pure call succeeds". `hoverWithDriver` is cache-only and knows nothing about procedures, generators or external functions, so for those identifiers it returns `ErrNoHover` and there is nothing to append to — feature 4 would never fire on the object it exists for. The InterBase path therefore **renders** the summary for catalog-only kinds and **appends** for tables. For a view it renders `ViewDoc`, which supersedes the pure hover's column table for the same identifier: it is the same columns plus the view's source, and no existing test asserts view hover. The constraint that is preserved exactly is the one that matters — **a table's column table is never replaced**, and a test pins it.

**Resolution order** is View, Procedure, Trigger, Generator, Function, then Table. Views must precede tables: a view is in `SchemaTables` too, and asking for `ObjectKindTable` DDL for a view is the wrong question.

**Files:**
- Create: `internal/handler/interbase_hover.go`
- Create: `internal/handler/interbase_hover_test.go`
- Modify: `internal/handler/hover.go:22-45`
- Modify: `internal/database/interbase_mock.go` (add the unsupported-DDL error constructor)

**Interfaces:**
- Consumes: `database.ProcedureDoc`/`ViewDoc`/`GeneratorDoc`/`FunctionDoc`/`TriggerDoc` (Task 3); `database.MockCapabilityRepository` (Task 1); from Plan 1 — `(*Server).fileText`; from the contract — `DBCache.HasCatalog/View/Procedure/Trigger/Generator/Function`, `DDLRepository`, `ErrUnsupportedDDL`, `ErrObjectNotFound`, `UnsupportedDDLDetail`.
- Produces:
  - `type hoverTarget struct { kind database.ObjectKind; name string }`
  - `func resolveInterBaseHoverTarget(text string, params lsp.HoverParams, dbCache *database.DBCache, driver dialect.DatabaseDriver) (hoverTarget, lsp.Range, bool)`
  - `func interBaseHoverSummary(target hoverTarget, dbCache *database.DBCache) string`
  - `func unsupportedDDLNote(err error) string`
  - `func renderObjectDDL(ctx context.Context, repo database.DDLRepository, kind database.ObjectKind, name string) (string, bool)` — the appendix, and whether the outcome is deterministic enough to memoise (Task 8).
  - `func (s *Server) interBaseHover(ctx context.Context, repo database.DBRepository, dbCache *database.DBCache, params lsp.HoverParams, text string, base *lsp.Hover) *lsp.Hover`
  - `const hoverDDLTimeout = 3 * time.Second`
  - In `internal/database`: `func NewUnsupportedDDLError(object, name, feature string) error`

- [ ] **Step 1: Check whether sub-project 2 already exports an unsupported-DDL constructor**

`UnsupportedDDLDetail` is `errors.As` against an **unexported** interface in `internal/database/capability.go`, so a test in `internal/handler` cannot build an error that carries structured detail. The constructor must live in package `database`.

Run: `grep -n 'unsupportedDDLDetailer\|UnsupportedDDLDetail\|func NewUnsupportedDDL' -A 6 internal/database/capability.go`

Expected: the unexported interface and its method. If sub-project 2 already exports a constructor, **consume it** and skip the `interbase_mock.go` addition in Step 3; use its name everywhere `NewUnsupportedDDLError` appears below. If the interface's method name or signature differs from `UnsupportedDDLDetail() (object, name, feature string)`, match what is actually declared — the type in Step 3 exists only to satisfy it.

- [ ] **Step 2: Write the failing tests**

Create `internal/handler/interbase_hover_test.go`:

```go
package handler

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

// interBaseHoverCache is the shared handler-side catalog fixture. CITY is both
// a real table and a cached relation; MYVIEW is a view that is also in
// SchemaTables, as the contract guarantees; MYTRIGGER has an empty Event,
// which is the normal case until the driver-side accessor spec lands; MYUDF
// has one unrenderable argument type.
func interBaseHoverCache(t *testing.T) *database.DBCache {
	t.Helper()
	columns := []*database.ColumnDesc{
		{ColumnBase: database.ColumnBase{Table: "CITY", Name: "ID"}, Type: "INTEGER"},
		{ColumnBase: database.ColumnBase{Table: "MYVIEW", Name: "ID"}, Type: "INTEGER"},
	}
	repo := &database.MockDBRepository{
		MockDatabase:  func(context.Context) (string, error) { return "", nil },
		MockDatabases: func(context.Context) ([]string, error) { return []string{""}, nil },
		MockDatabaseTables: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"": {"CITY", "MYVIEW"}}, nil
		},
		MockDescribeDatabaseTableBySchema: func(context.Context, string) ([]*database.ColumnDesc, error) {
			return columns, nil
		},
		MockDescribeForeignKeysBySchema: func(context.Context, string) ([]*database.ForeignKey, error) {
			return nil, nil
		},
	}
	cache, err := database.NewDBCacheUpdater(repo).GenerateDBCachePrimary(context.Background())
	if err != nil {
		t.Fatal("GenerateDBCachePrimary:", err)
	}
	cache.Catalog = &database.CatalogCache{
		Procedures: map[string]*database.ProcedureDesc{
			"MYPROC": {
				Name:   "MYPROC",
				Source: sql.NullString{String: "BEGIN\n  SUSPEND;\nEND", Valid: true},
				InputParameters: []*database.ProcedureParameterDesc{
					{Name: "IN_AMOUNT", Position: 0, Direction: database.ParameterInput, Type: "NUMERIC(18, 2)", Nullable: sql.NullBool{}},
				},
				OutputParameters: []*database.ProcedureParameterDesc{
					{Name: "OUT_TOTAL", Position: 0, Direction: database.ParameterOutput, Type: "INTEGER"},
				},
			},
		},
		Views: map[string]*database.ViewDesc{
			"MYVIEW": {
				Name:       "MYVIEW",
				ViewSource: sql.NullString{String: "SELECT ID FROM CITY", Valid: true},
				Columns:    []*database.ColumnDesc{{ColumnBase: database.ColumnBase{Table: "MYVIEW", Name: "ID"}, Type: "INTEGER"}},
			},
		},
		Generators: map[string]*database.GeneratorDesc{
			"GEN_ORDER_ID": {Name: "GEN_ORDER_ID", ID: sql.NullInt64{Int64: 3, Valid: true}},
		},
		Functions: map[string]*database.FunctionDesc{
			"MYUDF": {
				Name:       "MYUDF",
				ReturnType: "DOUBLE PRECISION",
				Arguments: []*database.FunctionArgumentDesc{
					{Position: sql.NullInt64{Int64: 1, Valid: true}, Type: ""},
				},
			},
		},
		Triggers: map[string]*database.TriggerDesc{
			"MYTRIGGER": {
				Name:         "MYTRIGGER",
				RelationName: sql.NullString{String: "CITY", Valid: true},
				Event:        "",
				Active:       sql.NullBool{Bool: true, Valid: true},
				Source:       sql.NullString{String: "BEGIN\n  NEW.ID = 1;\nEND", Valid: true},
			},
		},
	}
	return cache
}

// interBaseHoverServer builds a Server wired for the InterBase parser without
// a jsonrpc2 connection. The repository is injected at the call site because
// interbase_common.go's init already claims the InterBase driver name in
// database.driverFactories and RegisterFactory panics on a duplicate.
func interBaseHoverServer(t *testing.T) *Server {
	t.Helper()
	server := NewServer()
	t.Cleanup(server.worker.Stop)
	server.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
	return server
}

func hoverAt(t *testing.T, server *Server, repo database.DBRepository, cache *database.DBCache, text string, character int) *lsp.Hover {
	t.Helper()
	params := lsp.HoverParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: character},
		},
	}
	base, err := hoverWithDriver(text, params, cache, dialect.DatabaseDriverInterBase)
	if err != nil && !errors.Is(err, ErrNoHover) {
		t.Fatal("hoverWithDriver:", err)
	}
	return server.interBaseHover(context.Background(), repo, cache, params, text, base)
}

func TestResolveInterBaseHoverTarget(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		character int
		driver    dialect.DatabaseDriver
		catalog   bool
		wantOK    bool
		wantKind  database.ObjectKind
		wantName  string
	}{
		{name: "procedure", text: "execute procedure myproc", character: 20, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: true, wantKind: database.ObjectKindProcedure, wantName: "MYPROC"},
		{name: "view", text: "select * from myview", character: 16, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: true, wantKind: database.ObjectKindView, wantName: "MYVIEW"},
		{name: "generator", text: "select gen_id(gen_order_id, 1) from rdb$database", character: 18, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: true, wantKind: database.ObjectKindGenerator, wantName: "GEN_ORDER_ID"},
		{name: "external function", text: "select myudf(1)", character: 9, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: true, wantKind: database.ObjectKindFunction, wantName: "MYUDF"},
		{name: "trigger", text: "select mytrigger", character: 10, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: true, wantKind: database.ObjectKindTrigger, wantName: "MYTRIGGER"},
		{name: "table", text: "select * from city", character: 16, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: true, wantKind: database.ObjectKindTable, wantName: "CITY"},
		{name: "column produces no target", text: "select id from city", character: 8, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: false},
		{name: "alias produces no target", text: "select * from city c", character: 19, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: false},
		{name: "unknown identifier", text: "select * from nosuchthing", character: 16, driver: dialect.DatabaseDriverInterBase, catalog: true, wantOK: false},
		{name: "no catalog yet", text: "execute procedure myproc", character: 20, driver: dialect.DatabaseDriverInterBase, catalog: false, wantOK: false},
		{name: "not interbase", text: "execute procedure myproc", character: 20, driver: dialect.DatabaseDriverPostgreSQL, catalog: true, wantOK: false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cache := interBaseHoverCache(t)
			if !tt.catalog {
				cache.Catalog = nil
			}
			params := lsp.HoverParams{
				TextDocumentPositionParams: lsp.TextDocumentPositionParams{
					TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
					Position:     lsp.Position{Line: 0, Character: tt.character},
				},
			}
			got, _, ok := resolveInterBaseHoverTarget(tt.text, params, cache, tt.driver)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (target=%+v)", ok, tt.wantOK, got)
			}
			if !tt.wantOK {
				return
			}
			if got.kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", got.kind, tt.wantKind)
			}
			// The catalog spelling, not the user's: it is what ObjectDDL is
			// asked for and what the memo is keyed on.
			if got.name != tt.wantName {
				t.Errorf("name = %q, want %q", got.name, tt.wantName)
			}
		})
	}
}

func TestResolveInterBaseHoverTargetIsCaseInsensitiveWithoutUpperCasingAtTheCallSite(t *testing.T) {
	params := lsp.HoverParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: 20},
		},
	}
	got, _, ok := resolveInterBaseHoverTarget("execute procedure MyProc", params, interBaseHoverCache(t), dialect.DatabaseDriverInterBase)
	if !ok {
		t.Fatal("a mixed-case identifier did not resolve; DBCache accessors must normalise the name they are given")
	}
	if got.name != "MYPROC" {
		t.Errorf("name = %q, want %q", got.name, "MYPROC")
	}
}

func TestInterBaseHoverProcedureAppendsDDL(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "CREATE PROCEDURE MYPROC (IN_AMOUNT NUMERIC(18, 2)) RETURNS (OUT_TOTAL INTEGER) AS BEGIN SUSPEND; END", nil
	}

	got := hoverAt(t, server, repo, interBaseHoverCache(t), "execute procedure myproc", 20)
	if got == nil {
		t.Fatal("no hover for a cached procedure")
	}
	for _, want := range []string{
		"`MYPROC` procedure",
		"IN_AMOUNT",
		"OUT_TOTAL",
		"```sql",
		"CREATE PROCEDURE MYPROC",
	} {
		if !strings.Contains(got.Contents.Value, want) {
			t.Errorf("hover missing %q:\n%s", want, got.Contents.Value)
		}
	}
	if calls := repo.ObjectDDLCalls(); len(calls) != 1 || calls[0].Kind != database.ObjectKindProcedure || calls[0].Name != "MYPROC" {
		t.Errorf("ObjectDDLCalls() = %+v, want one {procedure MYPROC}", calls)
	}
}

func TestInterBaseHoverUnsupportedDDLFallsBackToSummary(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "", database.NewUnsupportedDDLError("procedure", "MYPROC", `parameter "IN_AMOUNT" nullability is unknown`)
	}

	got := hoverAt(t, server, repo, interBaseHoverCache(t), "execute procedure myproc", 20)
	if got == nil {
		t.Fatal("no hover; unsupported DDL must never suppress the summary")
	}
	value := got.Contents.Value

	// The summary survives intact. This is the common case for procedures, so
	// it is the case that must not degrade to nothing.
	for _, want := range []string{"`MYPROC` procedure", "IN_AMOUNT", "BEGIN\n  SUSPEND;\nEND"} {
		if !strings.Contains(value, want) {
			t.Errorf("hover lost %q from the summary:\n%s", want, value)
		}
	}
	// Exactly one italic note, naming object, name and blocking feature.
	for _, want := range []string{"_DDL unavailable:", "procedure", `"MYPROC"`, `parameter "IN_AMOUNT" nullability is unknown`} {
		if !strings.Contains(value, want) {
			t.Errorf("note missing %q:\n%s", want, value)
		}
	}
	if strings.Count(value, "_DDL unavailable") != 1 {
		t.Errorf("want exactly one DDL note, got %d:\n%s", strings.Count(value, "_DDL unavailable"), value)
	}
	// No fenced block, and above all no fabricated declaration.
	if strings.Contains(value, "```sql\nCREATE") {
		t.Errorf("a CREATE statement was fabricated:\n%s", value)
	}
}

func TestInterBaseHoverUnsupportedDDLWithoutDetailDegradesToBareNote(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	driverMessage := "interbase: schema: GenerateDDL refused for reasons of its own"
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		// A bare sentinel with no structured detail: UnsupportedDDLDetail
		// reports ok == false.
		return "", fmt.Errorf("%s: %w", driverMessage, database.ErrUnsupportedDDL)
	}

	got := hoverAt(t, server, repo, interBaseHoverCache(t), "execute procedure myproc", 20)
	if got == nil {
		t.Fatal("no hover")
	}
	value := got.Contents.Value

	if !strings.Contains(value, "_DDL unavailable._") {
		t.Errorf("note is not the bare form:\n%s", value)
	}
	// The failure this pins: a note built by printing err.Error(). The driver
	// message is not user-facing documentation.
	if strings.Contains(value, driverMessage) || strings.Contains(value, "GenerateDDL") {
		t.Errorf("a driver message leaked into the hover note:\n%s", value)
	}
}

func TestInterBaseHoverObjectNotFoundAppendsNothing(t *testing.T) {
	cache := interBaseHoverCache(t)
	server := interBaseHoverServer(t)

	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "", database.ErrObjectNotFound
	}
	withRepo := hoverAt(t, server, repo, cache, "execute procedure myproc", 20)

	// The same hover with no DDLRepository at all. A stale cache is not
	// something the user did, so the two must be byte-identical: no note, no
	// fenced block, no marker of any kind.
	plain := hoverAt(t, interBaseHoverServer(t), database.NewMockDBRepository(nil), cache, "execute procedure myproc", 20)

	if withRepo == nil || plain == nil {
		t.Fatal("no hover in one of the two arrangements")
	}
	if withRepo.Contents.Value != plain.Contents.Value {
		t.Errorf("ErrObjectNotFound changed the hover:\ngot:  %q\nwant: %q", withRepo.Contents.Value, plain.Contents.Value)
	}
	if strings.Contains(withRepo.Contents.Value, "stale") || strings.Contains(withRepo.Contents.Value, "unavailable") {
		t.Errorf("a footnote the user cannot act on was added:\n%s", withRepo.Contents.Value)
	}
	if len(repo.ObjectDDLCalls()) != 1 {
		t.Errorf("ObjectDDL was called %d times, want 1", len(repo.ObjectDDLCalls()))
	}
}

func TestInterBaseHoverDDLErrorIsNotSurfacedAsRequestError(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "", errors.New("interbase: connection reset")
	}

	got := hoverAt(t, server, repo, interBaseHoverCache(t), "execute procedure myproc", 20)
	if got == nil {
		t.Fatal("a DDL failure suppressed the hover; the user asked for documentation, not for DDL")
	}
	if !strings.Contains(got.Contents.Value, "`MYPROC` procedure") {
		t.Errorf("the summary was lost:\n%s", got.Contents.Value)
	}
	if strings.Contains(got.Contents.Value, "connection reset") {
		t.Errorf("a driver error reached the popup:\n%s", got.Contents.Value)
	}
}

func TestInterBaseHoverExternalFunctionNeverCallsObjectDDL(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "CREATE FUNCTION MYUDF", nil
	}

	got := hoverAt(t, server, repo, interBaseHoverCache(t), "select myudf(1)", 9)
	if got == nil {
		t.Fatal("no hover for a cached external function")
	}
	// ObjectKindFunction always returns ErrUnsupportedDDL, so calling it would
	// guarantee a wasted round trip and a note line on every hover.
	if calls := repo.ObjectDDLCalls(); len(calls) != 0 {
		t.Errorf("ObjectDDL was called for an external function: %+v", calls)
	}
	value := got.Contents.Value
	if !strings.Contains(value, "`MYUDF` external function") {
		t.Errorf("hover lost the function summary:\n%s", value)
	}
	if strings.Contains(value, "DDL") {
		t.Errorf("hovering an external function mentioned DDL:\n%s", value)
	}
	// The argument type is permanently unrenderable; the argument is still
	// listed and nothing stands in for the type.
	if !strings.Contains(value, "argument 1") {
		t.Errorf("the unrenderable argument was dropped:\n%s", value)
	}
	if strings.Contains(value, "<unknown>") || strings.Contains(value, "argument 1: ") {
		t.Errorf("a placeholder was rendered for the unknown argument type:\n%s", value)
	}
}

func TestInterBaseHoverTriggerOmitsEmptyEvent(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "", database.ErrObjectNotFound
	}

	got := hoverAt(t, server, repo, interBaseHoverCache(t), "select mytrigger", 10)
	if got == nil {
		t.Fatal("no hover for a cached trigger")
	}
	value := got.Contents.Value
	for _, want := range []string{"`MYTRIGGER` trigger", "CITY", "active", "NEW.ID = 1;"} {
		if !strings.Contains(value, want) {
			t.Errorf("hover missing %q:\n%s", want, value)
		}
	}
	// An empty Event omits the line. A naive renderer prints "``" or
	// "<unknown>" here.
	if strings.Contains(value, "<unknown>") || strings.Contains(value, "``\n") {
		t.Errorf("a placeholder event was rendered:\n%s", value)
	}
}

func TestInterBaseHoverGeneratorRendersNameOnly(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "CREATE GENERATOR GEN_ORDER_ID", nil
	}

	got := hoverAt(t, server, repo, interBaseHoverCache(t), "select gen_id(gen_order_id, 1) from rdb$database", 18)
	if got == nil {
		t.Fatal("no hover for a cached generator")
	}
	if !strings.Contains(got.Contents.Value, "`GEN_ORDER_ID` generator") {
		t.Errorf("hover lost the generator name:\n%s", got.Contents.Value)
	}
	if !strings.Contains(got.Contents.Value, "CREATE GENERATOR GEN_ORDER_ID") {
		t.Errorf("hover lost the generator DDL:\n%s", got.Contents.Value)
	}
}

func TestInterBaseHoverTableStillShowsColumnTable(t *testing.T) {
	server := interBaseHoverServer(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "CREATE TABLE CITY (ID INTEGER)", nil
	}
	cache := interBaseHoverCache(t)

	plain := hoverAt(t, interBaseHoverServer(t), database.NewMockDBRepository(nil), cache, "select * from city", 16)
	got := hoverAt(t, server, repo, cache, "select * from city", 16)
	if plain == nil || got == nil {
		t.Fatal("no hover for a table")
	}

	// DDL is appended, never substituted: the existing markdown column table
	// must still be there, unchanged, as its own prefix.
	if !strings.HasPrefix(got.Contents.Value, plain.Contents.Value) {
		t.Errorf("the column table was replaced rather than appended to:\ngot:  %q\nbase: %q", got.Contents.Value, plain.Contents.Value)
	}
	if !strings.Contains(got.Contents.Value, "CREATE TABLE CITY") {
		t.Errorf("the table DDL was not appended:\n%s", got.Contents.Value)
	}
}

func TestInterBaseHoverWithoutCapabilityIsUnchanged(t *testing.T) {
	// A plain MockDBRepository, an ordinary build, and every non-InterBase
	// driver all land here: the summary, and nothing else.
	server := interBaseHoverServer(t)
	cache := interBaseHoverCache(t)

	got := hoverAt(t, server, database.NewMockDBRepository(nil), cache, "execute procedure myproc", 20)
	if got == nil {
		t.Fatal("no hover without a DDL capability")
	}
	if !strings.Contains(got.Contents.Value, "`MYPROC` procedure") {
		t.Errorf("the catalog summary needs no capability:\n%s", got.Contents.Value)
	}
	if strings.Contains(got.Contents.Value, "DDL unavailable") {
		t.Errorf("a missing capability produced a note:\n%s", got.Contents.Value)
	}
}

func TestInterBaseHoverEndToEndKeepsExistingColumnHover(t *testing.T) {
	// Through a real jsonrpc2 round trip with the pre-existing fixture, which
	// has no catalog: the whole InterBase path must be inert.
	tx := newTestContext()
	tx.initServer(t)
	defer tx.tearDown()
	defer tx.server.worker.Stop()
	configureInterBaseTestServer(t, tx)

	tx.textDocumentDidOpen(t, testFileURI, "select rdb$relation_id from rdb$database")
	var got lsp.Hover
	if err := tx.conn.Call(tx.ctx, "textDocument/hover", lsp.HoverParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: 11},
		},
	}, &got); err != nil {
		t.Fatal("conn.Call textDocument/hover:", err)
	}
	if !strings.Contains(got.Contents.Value, "`RDB$RELATION_ID` column") {
		t.Fatalf("hover = %q, want the existing column metadata", got.Contents.Value)
	}
}
```

Add `"fmt"` to the import block.

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test -run 'TestResolveInterBaseHoverTarget|TestInterBaseHover' ./internal/handler/ -v`
Expected: FAIL to compile — `undefined: resolveInterBaseHoverTarget`, `undefined: (*Server).interBaseHover`, `undefined: database.NewUnsupportedDDLError`.

- [ ] **Step 4: Write the minimal implementation**

Append to `internal/database/interbase_mock.go` (skip if Step 1 found an exported constructor):

```go
// unsupportedDDLError carries the structured detail UnsupportedDDLDetail
// extracts. It lives here rather than in a test because the interface
// UnsupportedDDLDetail matches is unexported, so only this package can
// implement it — and a handler test must be able to build one.
type unsupportedDDLError struct {
	object  string
	name    string
	feature string
}

func (e *unsupportedDDLError) Error() string {
	return fmt.Sprintf("database: DDL is unavailable for %s %q: %s", e.object, e.name, e.feature)
}

func (e *unsupportedDDLError) Unwrap() error { return ErrUnsupportedDDL }

func (e *unsupportedDDLError) UnsupportedDDLDetail() (object, name, feature string) {
	return e.object, e.name, e.feature
}

// NewUnsupportedDDLError builds an error that satisfies
// errors.Is(err, ErrUnsupportedDDL) and yields ok == true from
// UnsupportedDDLDetail.
func NewUnsupportedDDLError(object, name, feature string) error {
	return &unsupportedDDLError{object: object, name: name, feature: feature}
}
```

Add `"fmt"` to that file's imports.

Create `internal/handler/interbase_hover.go`:

```go
package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/ast/astutil"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/parser"
	"github.com/sqls-server/sqls/parser/parseutil"
	"github.com/sqls-server/sqls/token"
)

// hoverDDLTimeout bounds the catalog round trip. textDocument/hover stays on
// the inline dispatch path — Plan 1 moves only workspace/executeCommand off it
// — so an unbounded catalog read would freeze the whole server.
const hoverDDLTimeout = 3 * time.Second

// hoverTarget is a catalog object the cursor is on.
type hoverTarget struct {
	kind database.ObjectKind
	name string
}

var hoverIdentifierMatcher = astutil.NodeMatcher{
	NodeTypes: []ast.NodeType{ast.TypeIdentifier},
}

var hoverMemberIdentifierMatcher = astutil.NodeMatcher{
	NodeTypes: []ast.NodeType{ast.TypeMemberIdentifier},
}

// resolveInterBaseHoverTarget classifies the identifier under the cursor
// against the catalog and returns the range to highlight. It is pure: no I/O,
// no server state.
//
// Views are resolved before tables. A view is in SchemaTables too, so a
// table-first order would ask ObjectDDL for CREATE TABLE of a view.
func resolveInterBaseHoverTarget(text string, params lsp.HoverParams, dbCache *database.DBCache, driver dialect.DatabaseDriver) (hoverTarget, lsp.Range, bool) {
	if driver != dialect.DatabaseDriverInterBase || !dbCache.HasCatalog() {
		return hoverTarget{}, lsp.Range{}, false
	}

	parsed, err := parser.ParseWithDriver(text, driver)
	if err != nil {
		return hoverTarget{}, lsp.Range{}, false
	}
	pos := token.Pos{
		Line: params.Position.Line,
		Col:  params.Position.Character + 1,
	}
	nodeWalker := parseutil.NewNodeWalker(parsed, pos)
	node := nodeWalker.CurNodeBottomMatched(hoverIdentifierMatcher)
	if node == nil {
		return hoverTarget{}, lsp.Range{}, false
	}
	ident, ok := node.(*ast.Identifier)
	if !ok {
		return hoverTarget{}, lsp.Range{}, false
	}
	name := ident.NoQuoteString()

	// A member identifier's child is a column, not an object. Columns and
	// aliases produce no target.
	if member := nodeWalker.CurNodeTopMatched(hoverMemberIdentifierMatcher); member != nil {
		if mi, ok := member.(*ast.MemberIdentifier); ok && mi.ChildTok != nil && mi.ChildTok.NoQuoteString() == name {
			return hoverTarget{}, lsp.Range{}, false
		}
	}

	identRange := lsp.Range{
		Start: lsp.Position{Line: ident.Pos().Line, Character: ident.Pos().Col},
		End:   lsp.Position{Line: ident.End().Line, Character: ident.End().Col},
	}

	// The accessors normalise the name they are given, so the identifier text
	// goes in exactly as the user typed it. Never upper-case here.
	if desc, ok := dbCache.View(name); ok {
		return hoverTarget{kind: database.ObjectKindView, name: desc.Name}, identRange, true
	}
	if desc, ok := dbCache.Procedure(name); ok {
		return hoverTarget{kind: database.ObjectKindProcedure, name: desc.Name}, identRange, true
	}
	if desc, ok := dbCache.Trigger(name); ok {
		return hoverTarget{kind: database.ObjectKindTrigger, name: desc.Name}, identRange, true
	}
	if desc, ok := dbCache.Generator(name); ok {
		return hoverTarget{kind: database.ObjectKindGenerator, name: desc.Name}, identRange, true
	}
	if desc, ok := dbCache.Function(name); ok {
		return hoverTarget{kind: database.ObjectKindFunction, name: desc.Name}, identRange, true
	}
	if _, ok := dbCache.ColumnDescs(name); ok {
		return hoverTarget{kind: database.ObjectKindTable, name: name}, identRange, true
	}
	return hoverTarget{}, lsp.Range{}, false
}

// interBaseHoverSummary renders what the catalog knows and the pure,
// cache-only hover could not. It returns "" for a table, whose markdown column
// table hoverWithDriver already produced and which must never be replaced.
func interBaseHoverSummary(target hoverTarget, dbCache *database.DBCache) string {
	switch target.kind {
	case database.ObjectKindView:
		if desc, ok := dbCache.View(target.name); ok {
			return database.ViewDoc(desc)
		}
	case database.ObjectKindProcedure:
		if desc, ok := dbCache.Procedure(target.name); ok {
			return database.ProcedureDoc(desc)
		}
	case database.ObjectKindTrigger:
		if desc, ok := dbCache.Trigger(target.name); ok {
			return database.TriggerDoc(desc)
		}
	case database.ObjectKindGenerator:
		if desc, ok := dbCache.Generator(target.name); ok {
			return database.GeneratorDoc(desc)
		}
	case database.ObjectKindFunction:
		if desc, ok := dbCache.Function(target.name); ok {
			return database.FunctionDoc(desc)
		}
	}
	return ""
}

// unsupportedDDLNote is the single italic line appended when the catalog
// cannot reproduce executable DDL. With no structured detail it degrades to a
// bare note rather than rendering a driver message: the user asked for
// documentation.
func unsupportedDDLNote(err error) string {
	object, name, feature, ok := database.UnsupportedDDLDetail(err)
	if !ok {
		return "_DDL unavailable._"
	}
	return fmt.Sprintf("_DDL unavailable: %s %q: %s._", object, name, strings.TrimRight(feature, "."))
}

// renderObjectDDL returns the markdown to append after the summary, and
// whether the outcome is deterministic for this connection and therefore safe
// to memoise. A transport failure is not: it may succeed on the next hover.
func renderObjectDDL(ctx context.Context, repo database.DDLRepository, kind database.ObjectKind, name string) (string, bool) {
	ddlCtx, cancel := context.WithTimeout(ctx, hoverDDLTimeout)
	defer cancel()

	ddl, err := repo.ObjectDDL(ddlCtx, kind, name)
	switch {
	case err == nil:
		if strings.TrimSpace(ddl) == "" {
			return "", true
		}
		return fmt.Sprintf("\n\n---\n\n```sql\n%s\n```\n", strings.TrimRight(ddl, "\n")), true
	case errors.Is(err, database.ErrUnsupportedDDL):
		return "\n\n" + unsupportedDDLNote(err) + "\n", true
	case errors.Is(err, database.ErrObjectNotFound):
		// The cache named an object the catalog does not have, which means the
		// cache is stale. A stale-cache footnote on a hover popup is noise the
		// user cannot act on, so nothing at all is appended.
		return "", true
	default:
		log.Printf("sqls: object DDL for %s %q: %v", kind, name, err)
		return "", false
	}
}

// interBaseHover composes the catalog summary and the DDL appendix, returning
// nil when this path contributes nothing and the caller should use base.
//
// repo is a parameter rather than read off the server so a test can inject a
// capability-bearing repository: interbase_common.go's init already claims the
// InterBase driver name in database.driverFactories and RegisterFactory panics
// on a duplicate.
func (s *Server) interBaseHover(ctx context.Context, repo database.DBRepository, dbCache *database.DBCache, params lsp.HoverParams, text string, base *lsp.Hover) *lsp.Hover {
	target, identRange, ok := resolveInterBaseHoverTarget(text, params, dbCache, s.parserDriver())
	if !ok {
		return nil
	}

	value := interBaseHoverSummary(target, dbCache)
	if value == "" {
		if base == nil {
			return nil
		}
		value = base.Contents.Value
	}
	if base != nil {
		identRange = base.Range
	}

	value += s.objectDDLMarkdown(ctx, repo, target)

	return &lsp.Hover{
		Contents: lsp.MarkupContent{Kind: lsp.Markdown, Value: value},
		Range:    identRange,
	}
}

// objectDDLMarkdown fetches and renders the DDL appendix, or "" when there is
// none to show. ObjectKindFunction is never attempted: the contract states it
// always returns ErrUnsupportedDDL, so calling it would guarantee a wasted
// round trip and a note line on every hover of an external function.
func (s *Server) objectDDLMarkdown(ctx context.Context, repo database.DBRepository, target hoverTarget) string {
	if target.kind == database.ObjectKindFunction || repo == nil {
		return ""
	}
	ddlRepo, ok := repo.(database.DDLRepository)
	if !ok {
		return ""
	}
	rendered, _ := renderObjectDDL(ctx, ddlRepo, target.kind, target.name)
	return rendered
}
```

In `internal/handler/hover.go`, replace the body of `handleTextDocumentHover` after the unmarshal (`:32-44`). **This is the post-Plan-1 shape**: Plan 1 Task 5 already replaced the `s.files[...]` lookup with `s.fileText`:

```go
	text, ok := s.fileText(params.TextDocument.URI)
	if !ok {
		return nil, fmt.Errorf("document not found: %s", params.TextDocument.URI)
	}

	dbCache := s.worker.Cache()
	res, err := hoverWithDriver(text, params, dbCache, s.parserDriver())
	if err != nil && !errors.Is(err, ErrNoHover) {
		return nil, err
	}

	// Not having a repository is not a hover failure: the catalog summary
	// needs none, and a DDL problem must never surface as a JSON-RPC error.
	repo, repoErr := s.newDBRepository(ctx)
	if repoErr != nil {
		repo = nil
	}
	if augmented := s.interBaseHover(ctx, repo, dbCache, params, text, res); augmented != nil {
		return augmented, nil
	}
	if err != nil {
		return nil, nil
	}
	return res, nil
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -run 'TestResolveInterBaseHoverTarget|TestInterBaseHover' ./internal/handler/ -v`
Expected: every test and subtest PASS.

- [ ] **Step 6: Prove the degradation tests are not vacuous**

Three of these tests must fail against a naive implementation. Verify each, one at a time, reverting between.

a. Temporarily make `renderObjectDDL` treat not-found like any other refusal:

```go
	case errors.Is(err, database.ErrObjectNotFound):
		return "\n\n_DDL unavailable: the object is no longer in the catalog._\n", true
```

Run: `go test -run 'TestInterBaseHoverObjectNotFoundAppendsNothing' ./internal/handler/ -v`
Expected: FAIL with a diff between the two hovers. Revert.

b. Temporarily make `unsupportedDDLNote` fall back to the driver message:

```go
	if !ok {
		return "_" + err.Error() + "_"
	}
```

Run: `go test -run 'TestInterBaseHoverUnsupportedDDLWithoutDetailDegradesToBareNote' ./internal/handler/ -v`
Expected: FAIL on both the bare-note assertion and the leaked-message assertion. Revert.

c. Temporarily drop the `ObjectKindFunction` guard in `objectDDLMarkdown`.

Run: `go test -run 'TestInterBaseHoverExternalFunctionNeverCallsObjectDDL' ./internal/handler/ -v`
Expected: FAIL with `ObjectDDL was called for an external function`. Revert.

Re-run all three after reverting and confirm PASS. Any of them passing with the temporary change in place means the test asserts nothing and must be fixed before moving on.

- [ ] **Step 7: Verify existing hover behaviour is unchanged**

Run: `go test -run 'Hover' ./internal/handler/ -count=1 -v 2>&1 | grep -E '^--- ' | sort | uniq -c`
Expected: no `--- FAIL`. `TestInterBaseDialect1LanguageServerHover` and every case in `hover_test.go` assert today's output; none of their fixtures has a catalog, so `resolveInterBaseHoverTarget` returns false and the path is inert.

- [ ] **Step 8: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 9: Commit**

```bash
git add internal/handler/interbase_hover.go internal/handler/interbase_hover_test.go internal/handler/hover.go internal/database/interbase_mock.go
git commit -m "feat: append real DDL to InterBase hover with honest degradation"
```

---
### Task 8: Memoise the DDL round trip per connection generation

Spec §4, "Memoisation": "Hover fires on every cursor rest over the same token; without this, each one is a catalog round trip."

**The lock rule this task exists to respect.** Plan 1's invariant is `connMu` before `stateMu`, and **`stateMu` is never held across any I/O**. A 3-second `ObjectDDL` call is I/O. The memo therefore runs read-under-`RLock` → release → round trip → store-under-`Lock`. Two hovers on the same cold object can race and both make the call; that is the correct trade, because the alternative holds a server-wide lock across a network round trip on the inline dispatch path.

**Not every outcome is memoised.** Success, `ErrUnsupportedDDL` and `ErrObjectNotFound` are deterministic for a connection and are cached. A transport failure or a timeout is not — caching `""` for it would permanently suppress DDL for that object until the next reconnect. `renderObjectDDL` (Task 7) already returns that distinction as its second result.

**`Server.connGeneration` is shared with Plan 4.** Spec §6.1c lists "DDL memo (§4), snapshot store generation (§5)" as one row of the `stateMu` audit table. Neither plan owns the field; Step 1 greps for it and consumes it when Plan 4 landed first.

**Files:**
- Modify: `internal/handler/handler.go` (struct, `NewServer`, `reconnectionDB`)
- Modify: `internal/handler/interbase_hover.go` (`objectDDLMarkdown`)
- Modify: `internal/handler/interbase_hover_test.go` (append)
- Modify: `doc/develop.md` (the concurrency audit table Plan 1 created)

**Interfaces:**
- Consumes: `renderObjectDDL`, `(*Server).objectDDLMarkdown`, `hoverTarget` (Task 7); `Server.stateMu` (Plan 1).
- Produces:
  - `type ddlKey struct { generation int; kind database.ObjectKind; name string }`
  - `Server.connGeneration int` — guarded by `stateMu`, incremented by `reconnectionDB`. **Shared with Plan 4; consume it if it already exists.**
  - `Server.ddlMemo map[ddlKey]string` — guarded by `stateMu`.
  - `func (s *Server) connectionGeneration() int`
  - `func (s *Server) memoisedObjectDDL(ctx context.Context, repo database.DDLRepository, target hoverTarget) string`

- [ ] **Step 1: Check whether Plan 4 already added the generation counter**

Run: `grep -rn 'connGeneration' internal/handler --include='*.go'`

If `Server.connGeneration` already exists and `reconnectionDB` already increments it, **skip those two edits in Step 4** and add only `ddlMemo`, `connectionGeneration()` and `memoisedObjectDDL`. If it does not exist, add it with exactly this name so Plan 4 can consume it in turn.

- [ ] **Step 2: Write the failing tests**

Append to `internal/handler/interbase_hover_test.go`:

```go
func TestInterBaseHoverMemoisesObjectDDL(t *testing.T) {
	server := interBaseHoverServer(t)
	cache := interBaseHoverCache(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "CREATE PROCEDURE MYPROC AS BEGIN SUSPEND; END", nil
	}

	first := hoverAt(t, server, repo, cache, "execute procedure myproc", 20)
	second := hoverAt(t, server, repo, cache, "execute procedure myproc", 20)
	if first == nil || second == nil {
		t.Fatal("no hover")
	}
	if first.Contents.Value != second.Contents.Value {
		t.Errorf("the memoised hover differs from the first:\ngot:  %q\nwant: %q", second.Contents.Value, first.Contents.Value)
	}
	if calls := repo.ObjectDDLCalls(); len(calls) != 1 {
		t.Fatalf("ObjectDDL was called %d times for two hovers on the same token, want 1", len(calls))
	}

	// A reconnect invalidates everything: the new connection may be a
	// different database entirely.
	server.stateMu.Lock()
	server.connGeneration++
	server.stateMu.Unlock()

	third := hoverAt(t, server, repo, cache, "execute procedure myproc", 20)
	if third == nil {
		t.Fatal("no hover after a reconnect")
	}
	if calls := repo.ObjectDDLCalls(); len(calls) != 2 {
		t.Errorf("ObjectDDL was called %d times after a reconnect, want 2", len(calls))
	}
}

func TestInterBaseHoverMemoisesTheUnsupportedNote(t *testing.T) {
	// Unsupported DDL is the common case for procedures, so it is the case
	// that must not re-query on every cursor rest.
	server := interBaseHoverServer(t)
	cache := interBaseHoverCache(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		return "", database.NewUnsupportedDDLError("procedure", "MYPROC", `parameter "IN_AMOUNT" nullability is unknown`)
	}

	hoverAt(t, server, repo, cache, "execute procedure myproc", 20)
	got := hoverAt(t, server, repo, cache, "execute procedure myproc", 20)
	if got == nil {
		t.Fatal("no hover")
	}
	if !strings.Contains(got.Contents.Value, "_DDL unavailable:") {
		t.Errorf("the memoised note was lost:\n%s", got.Contents.Value)
	}
	if calls := repo.ObjectDDLCalls(); len(calls) != 1 {
		t.Errorf("ObjectDDL was called %d times, want 1", len(calls))
	}
}

func TestInterBaseHoverDoesNotMemoiseTransientFailures(t *testing.T) {
	server := interBaseHoverServer(t)
	cache := interBaseHoverCache(t)
	repo := database.NewMockCapabilityRepository()
	calls := 0
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("interbase: connection reset")
		}
		return "CREATE PROCEDURE MYPROC AS BEGIN SUSPEND; END", nil
	}

	if got := hoverAt(t, server, repo, cache, "execute procedure myproc", 20); got == nil {
		t.Fatal("no hover after a transport failure")
	}
	got := hoverAt(t, server, repo, cache, "execute procedure myproc", 20)
	if got == nil {
		t.Fatal("no hover on the retry")
	}
	// Memoising "" for a transport failure would suppress this object's DDL
	// until the next reconnect.
	if !strings.Contains(got.Contents.Value, "CREATE PROCEDURE MYPROC") {
		t.Errorf("a transient failure was memoised; the retry produced no DDL:\n%s", got.Contents.Value)
	}
	if len(repo.ObjectDDLCalls()) != 2 {
		t.Errorf("ObjectDDL was called %d times, want 2", len(repo.ObjectDDLCalls()))
	}
}

func TestInterBaseHoverMemoIsKeyedByObject(t *testing.T) {
	server := interBaseHoverServer(t)
	cache := interBaseHoverCache(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(_ context.Context, kind database.ObjectKind, name string) (string, error) {
		return "CREATE " + strings.ToUpper(string(kind)) + " " + name, nil
	}

	procedure := hoverAt(t, server, repo, cache, "execute procedure myproc", 20)
	view := hoverAt(t, server, repo, cache, "select * from myview", 16)
	if procedure == nil || view == nil {
		t.Fatal("no hover")
	}
	if !strings.Contains(procedure.Contents.Value, "CREATE PROCEDURE MYPROC") {
		t.Errorf("procedure hover:\n%s", procedure.Contents.Value)
	}
	// The failure this pins: a memo keyed on the name alone serves the
	// procedure's DDL for the view.
	if !strings.Contains(view.Contents.Value, "CREATE VIEW MYVIEW") {
		t.Errorf("view hover was served the wrong object's DDL:\n%s", view.Contents.Value)
	}
}

func TestHoverMemoLockIsNotHeldAcrossObjectDDL(t *testing.T) {
	server := interBaseHoverServer(t)
	cache := interBaseHoverCache(t)
	repo := database.NewMockCapabilityRepository()
	repo.MockObjectDDL = func(context.Context, database.ObjectKind, string) (string, error) {
		// Takes the very mutex the memo lives under. If stateMu were held
		// across the round trip this blocks forever, which is precisely the
		// invariant "stateMu is never held across any I/O" — and a 3-second
		// ObjectDDL call is I/O.
		server.stateMu.Lock()
		server.stateMu.Unlock()
		return "CREATE PROCEDURE MYPROC", nil
	}

	params := lsp.HoverParams{
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
			Position:     lsp.Position{Line: 0, Character: 20},
		},
	}
	done := make(chan *lsp.Hover, 1)
	go func() {
		done <- server.interBaseHover(context.Background(), repo, cache, params, "execute procedure myproc", nil)
	}()

	select {
	case got := <-done:
		if got == nil {
			t.Fatal("no hover")
		}
		if !strings.Contains(got.Contents.Value, "CREATE PROCEDURE MYPROC") {
			t.Errorf("hover lost the DDL:\n%s", got.Contents.Value)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hover deadlocked: stateMu was held across the ObjectDDL round trip")
	}
}
```

Add `"time"` to the file's imports.

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test -run 'TestInterBaseHoverMemoises|TestInterBaseHoverDoesNotMemoise|TestInterBaseHoverMemoIsKeyedByObject|TestHoverMemoLockIsNotHeldAcrossObjectDDL' ./internal/handler/ -v`
Expected: FAIL to compile — `server.connGeneration undefined` (unless Plan 4 landed it). Once that compiles, `TestInterBaseHoverMemoisesObjectDDL` FAILS with `ObjectDDL was called 2 times … want 1`, and `TestInterBaseHoverMemoisesTheUnsupportedNote` likewise. `TestInterBaseHoverDoesNotMemoiseTransientFailures`, `TestInterBaseHoverMemoIsKeyedByObject` and `TestHoverMemoLockIsNotHeldAcrossObjectDDL` PASS from the start — they are the guards against over-caching and against a lock-ordering regression, and they must stay green through Step 4.

- [ ] **Step 4: Write the minimal implementation**

In `internal/handler/handler.go`, add the fields to `Server`, below the existing mutable fields and above `worker`:

```go
	// connGeneration advances on every reconnect. It ties per-connection
	// artefacts — the hover DDL memo, and the go-to-definition snapshot
	// directory — to the connection they were produced under. Guarded by
	// stateMu.
	connGeneration int

	// ddlMemo caches the rendered DDL appendix per connection generation.
	// Hover fires on every cursor rest over the same token; without this,
	// each one is a catalog round trip. Guarded by stateMu, and never held
	// across the round trip itself.
	ddlMemo map[ddlKey]string
```

In `NewServer`, initialise the map alongside `files`:

```go
	return &Server{
		files:   make(map[string]*File),
		ddlMemo: make(map[ddlKey]string),
		worker:  worker,
	}
```

(Plan 1 and Plan 4 add their own fields to this literal; keep theirs.)

In `reconnectionDB`, extend Plan 1's guarded assignment:

```go
	s.stateMu.Lock()
	s.dbConn = dbConn
	s.connGeneration++
	// The new connection may be a different database entirely, so nothing
	// cached against the old one is still true.
	s.ddlMemo = make(map[ddlKey]string)
	s.stateMu.Unlock()
```

In `internal/handler/interbase_hover.go`, add the key type and the memo, and route `objectDDLMarkdown` through it:

```go
// ddlKey identifies one memoised DDL rendering. The generation is part of the
// key rather than a reason to clear the map, so a hover that started before a
// reconnect can never write a stale entry into the new connection's view.
type ddlKey struct {
	generation int
	kind       database.ObjectKind
	name       string
}

func (s *Server) connectionGeneration() int {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.connGeneration
}

// memoisedObjectDDL renders the DDL appendix, reusing a previous rendering for
// the same object on the same connection.
//
// The lock is taken twice and released around the round trip, never held
// across it: stateMu is a server-wide lock on the inline dispatch path, and
// ObjectDDL is bounded at three seconds. Two hovers on the same cold object
// can therefore both make the call; that duplicate is much cheaper than
// freezing every other request for the duration.
func (s *Server) memoisedObjectDDL(ctx context.Context, repo database.DDLRepository, target hoverTarget) string {
	key := ddlKey{kind: target.kind, name: target.name}

	s.stateMu.RLock()
	key.generation = s.connGeneration
	rendered, hit := s.ddlMemo[key]
	s.stateMu.RUnlock()
	if hit {
		return rendered
	}

	rendered, cacheable := renderObjectDDL(ctx, repo, target.kind, target.name)
	if !cacheable {
		// A transport failure or a timeout may succeed next time. Caching ""
		// for it would suppress this object's DDL until the next reconnect.
		return rendered
	}

	s.stateMu.Lock()
	if s.connGeneration == key.generation && s.ddlMemo != nil {
		s.ddlMemo[key] = rendered
	}
	s.stateMu.Unlock()
	return rendered
}
```

and change the last two lines of `objectDDLMarkdown`:

```go
	rendered := s.memoisedObjectDDL(ctx, ddlRepo, target)
	return rendered
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -run 'TestInterBaseHover|TestResolveInterBaseHoverTarget|TestHoverMemo' ./internal/handler/ -v`
Expected: every test PASS, including all of Task 7's.

- [ ] **Step 6: Prove the memo test is not vacuous**

Temporarily make `memoisedObjectDDL` ignore its cache — delete the `if hit { return rendered }` line.

Run: `go test -run 'TestInterBaseHoverMemoisesObjectDDL' ./internal/handler/ -v`
Expected: FAIL with `ObjectDDL was called 2 times for two hovers on the same token, want 1`. **Revert** and re-run to confirm PASS.

Then temporarily key the memo on the name alone (`key := ddlKey{name: target.name}`).

Run: `go test -run 'TestInterBaseHoverMemoIsKeyedByObject|TestInterBaseHoverMemoisesObjectDDL' ./internal/handler/ -v`
Expected: `TestInterBaseHoverMemoIsKeyedByObject` FAILS with the view served `CREATE PROCEDURE MYPROC`, and `TestInterBaseHoverMemoisesObjectDDL` FAILS on the reconnect count. **Revert** and re-run.

- [ ] **Step 7: Record the new fields in the concurrency audit table**

Plan 1's Global Constraints require that any new `Server` field is added to the audit table in `doc/develop.md` and classified. Add two rows to that table (Plan 4 adds `snapshots` to the same table; if `connGeneration` is already listed, add only `ddlMemo`):

```markdown
| `connGeneration` | `reconnectionDB` | `connectionGeneration`, `memoisedObjectDDL` (inline, hover) | `stateMu` |
| `ddlMemo` | `reconnectionDB`, `memoisedObjectDDL` | `memoisedObjectDDL` (inline, hover) | `stateMu`, **never held across the `ObjectDDL` round trip** |
```

- [ ] **Step 8: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`. The race detector is the check that matters here: `TestHoverMemoLockIsNotHeldAcrossObjectDDL` runs the hover on its own goroutine while touching `stateMu` from another.

- [ ] **Step 9: Commit**

```bash
git add internal/handler/handler.go internal/handler/interbase_hover.go internal/handler/interbase_hover_test.go doc/develop.md
git commit -m "feat: memoise hover DDL per connection generation"
```

---

### Task 9: Documentation

Spec's Documentation section: flip the README's Explain checkbox, add an "InterBase editor features" subsection, and document the capability-interface pattern in `doc/develop.md`.

**The README heading is shared with Plan 4.** Plan 4's Task 7 documents go-to-definition snapshots under the same `#### InterBase editor features` heading, nesting under it when it exists and creating it when it does not. Step 2 here does the mirror image.

**Files:**
- Modify: `README.md:51` (the checkbox) and the `### InterBase Build` section, which ends at line 90, before `## Editor Plugins` at line 92
- Modify: `doc/develop.md`

**Interfaces:**
- Consumes: every name introduced by Tasks 1–8.
- Produces: no code.

- [ ] **Step 1: Flip the Explain SQL checkbox**

Run: `grep -n 'Explain SQL' README.md`
Expected: `51:- [ ] Explain SQL`.

Replace that line with:

```markdown
- [x] Explain SQL
  - InterBase only; other drivers report that the command is unsupported.
```

- [ ] **Step 2: Find the insertion point for the editor-features subsection**

Run: `grep -n '^### InterBase Build\|^#### InterBase editor features\|^## Editor Plugins' README.md`
Expected: `### InterBase Build` around line 73 and `## Editor Plugins` around line 93 (one line lower after Step 1). `#### InterBase editor features` exists only if Plan 4 has already landed its README change.

If `#### InterBase editor features` exists, insert the text below **inside** that subsection, above whatever Plan 4 wrote. If it does not, insert the heading plus the text after the `### InterBase Build` section's last paragraph (the one ending "does not include this local integration.") and before `## Editor Plugins`.

- [ ] **Step 3: Add the documentation**

````markdown
#### InterBase editor features

On an InterBase connection sqls reads the database's own catalog and uses it in
four editor surfaces. Everything here is automatic: there are no settings, and
each feature silently falls back to its ordinary behaviour when the catalog is
not available — on another driver, on a build without the InterBase tag, and in
the short window after connecting before the catalog has been read.

**Explain SQL.** The `Explain SQL` code action shows the query plan InterBase
chose. It **prepares the statement without executing it**: nothing is inserted,
updated or deleted, and a statement that modifies data is shown with a banner
saying so. `SELECT`, `INSERT`, `UPDATE`, `DELETE` and `EXECUTE PROCEDURE` are
supported; DDL and transaction control are refused, because their plan is always
empty and an empty pane reads like a bug. A statement can prepare successfully
and still have no plan — InterBase simply reports none — and that is shown as
text rather than as a blank result. A `SELECT` whose result contains an array
column cannot be explained, because the prepare path sqls uses does not accept
array results.

**Completion.** Procedures are offered after `EXECUTE PROCEDURE`; procedures
that return output are offered wherever a table is, because an InterBase
selectable procedure is legal wherever a relation is; a selectable procedure's
output parameters are offered as its columns. External functions appear beside
the built-in functions, and generators are offered inside a `GEN_ID(` call —
only there, because offering every generator in every expression would bury the
column candidates. Views are labelled `view` rather than `table`. Identifiers
match case-insensitively, so `myproc` finds `MYPROC`.

Procedure **input parameter names** are deliberately not completed: InterBase
DSQL has no named parameters, so a parameter name is never valid text in a
statement. They appear in signature help and hover instead. Triggers are not
completed either — no SQL context in which sqls completes ever names one.

**Signature help.** Typing an argument list for a known procedure shows its
input parameters and highlights the one under the cursor, both for
`EXECUTE PROCEDURE MYPROC(…)` and for `SELECT * FROM MYPROC(…)`. A parameter is
marked `NOT NULL` only when the catalog proves it. Most InterBase procedure
parameters carry no declaration nullability at all, and those are shown with no
nullability marking rather than a guess. A space before the parenthesis —
`MYPROC (1, 2)` — is not recognised as a call, which matches sqls's existing
behaviour for built-in functions.

**Hover.** Hovering a table, view, procedure, trigger or generator shows what
the catalog knows about it and then, when InterBase can reproduce it, the real
`CREATE` statement. When it cannot, hover says so in one line naming the reason
and shows the object's verbatim catalog source instead. **sqls never invents a
`CREATE` statement it did not get from the database.** DDL that cannot be
reproduced is the ordinary case for procedures: the reference-compatible
catalog does not record whether a parameter was declared nullable, and without
that a faithful declaration cannot be written. Hovering an external function
shows its declaration metadata and never mentions DDL, because InterBase does
not reproduce DDL for external functions at all.

Some catalog values are simply absent — a trigger's event, an external
function's return type, and the type of a `CHAR` or `VARCHAR` function argument,
which InterBase never records. Wherever a value is missing the corresponding
line is left out rather than filled with a placeholder, and the object itself is
still shown.

The DDL lookup happens when you hover, not when you connect, and is bounded at
three seconds; the result is remembered until the connection is switched or
reopened.
````

- [ ] **Step 4: Add the capability-interface pattern to `doc/develop.md`**

Append to `doc/develop.md`:

````markdown
## InterBase capability interfaces

InterBase-specific behaviour is added to sqls through **optional interfaces
asserted at the call site**, never through a driver check, wherever a driver
check can be avoided. A repository that later implements one of these gets the
feature for free, and shared code stays upstreamable.

The three rules:

1. **Capability, not driver name.** Handlers type-assert
   `database.DDLRepository` and `database.ExplainRepository`, and read catalog
   data through `DBCache.HasCatalog()` rather than by asserting
   `database.CatalogRepository`. `HasCatalog()` is false on every other driver
   and in the window before the worker's catalog pass lands, and every feature
   degrades to its pre-InterBase behaviour in that case.
2. **Driver identity only for parser- and lexer-shaped behaviour**, matching the
   existing `c.Driver == dialect.DatabaseDriverInterBase` checks in
   `internal/completer/candidates.go`. Completion candidate *generators* use it
   because the surrounding completer already does; hover and explain do not.
3. **Driver imports only under the `interbase` build tag**, following the
   `interbase_native.go` / `interbase_stub.go` pair. Nothing in the editor
   features imports `interbase-go`.

| File | Tag | Contents |
| --- | --- | --- |
| `internal/database/capability.go` | none | the capability interfaces, `ObjectKind`, the sentinels |
| `internal/database/catalog_doc.go` | none | markdown rendered from catalog descriptors, shared by the completer and the handler |
| `internal/database/interbase_mock.go` | none | `MockCapabilityRepository`, deliberately a distinct type from `MockDBRepository` |
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
  `DDLRepository.ObjectDDL` or it is not shown; what the catalog cannot
  reproduce is displayed as its own fields and its verbatim source text.

**Testing against the driver registry.** `internal/database/interbase_common.go`
registers the InterBase factory in its `init`, and `RegisterFactory` panics on a
duplicate, so a test cannot install a capability-bearing repository under the
InterBase driver name. Features that need a repository therefore take it as a
parameter — `explainStatements(ctx, explainer, queries)` and
`(*Server).interBaseHover(ctx, repo, cache, …)` — and the tests call them
directly. Features that read only the `*DBCache`, which is completion and
signature help, need no server at all.
````

- [ ] **Step 5: Verify every claim in the text against the code**

Run:

```bash
grep -n 'Explain supports SELECT, INSERT, UPDATE, DELETE and EXECUTE PROCEDURE' internal/handler/explain.go
grep -n 'hoverDDLTimeout = 3 \* time.Second' internal/handler/interbase_hover.go
grep -n '_DDL unavailable._' internal/handler/interbase_hover.go
grep -n 'genIDFunctionName = "GEN_ID"' internal/completer/interbase_candidates.go
grep -n 'NOT NULL' internal/database/catalog_doc.go
grep -n 'ObjectKindFunction' internal/handler/interbase_hover.go
```

Expected: every marker found. The README claims the three-second bound, the
`GEN_ID(`-only generator scope, the `NOT NULL`-only nullability rule, the
refusal wording and the never-called `ObjectKindFunction`; each must be real.

- [ ] **Step 6: Run the whole suite**

Run: `make test-race`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add README.md doc/develop.md
git commit -m "docs: describe the InterBase editor features and the capability pattern"
```

---

## What this plan deliberately leaves to other plans

- **Feature 5, go-to-definition snapshots** — Plan 4. This plan does not touch `internal/handler/definition.go`, does not add a `snapshots` field, and does not write files.
- **§6 in full** — the results pane, cancellation, `ScanRowsWithTypes`, `ReadOnlyQuerier`, `EXECUTE PROCEDURE` **routing**, and `ClassifyFailure` — Plans 1 and 2. This plan consumes `cancellationNotice` and `cancelledError` in `explainQuery` and adds nothing to them.
- **The catalog itself** — sub-project 2. This plan adds no descriptor, no capability interface, no `DBCache` accessor and no worker change. Task 1 Step 1 stops if they are absent.
- **`TriggerDesc.Event`, `FunctionArgumentDesc.Type` and `FunctionDesc.ReturnType` being populated** — the driver-side accessor spec (D10). Every renderer here already tolerates `""`, so that work turns tests from "renders nothing" fixtures into real values without revising a line of this plan.

## Known limitations shipped on purpose

1. **`multiKeywordMap` is shared parser state and this plan changes it.** PostgreSQL's legacy `CREATE TRIGGER … FOR EACH ROW EXECUTE PROCEDURE f()` contains the literal sequence, so its syntax position after the keywords goes from `Unknown` to `ExecuteProcedure`. The branch retains `CompletionTypeKeyword`, so no PostgreSQL user loses a candidate they get today, but this is the one change here that alters behaviour for another driver and the one to scrutinise before upstreaming. `TestInterBaseCandidatesAreNotOfferedToOtherDrivers` and the PostgreSQL case in `TestCheckSyntaxPosition` pin it.
2. **Irregular whitespace inside the keyword pair does not match.** `EXECUTE&nbsp;&nbsp;PROCEDURE` is not grouped, because `IsMatchKeyword` compares `node.String()`. Shared with the existing `DELETE FROM` handling.
3. **A space before the argument parenthesis defeats signature help.** `MYPROC (1, 2)` is not a `FunctionLiteral`. Matches today's behaviour for built-in functions.
4. **Hover can freeze the server for up to three seconds on a cold object over a slow link.** Hover is on the inline dispatch path by design; the memo makes repeats free, but the first hover is exposed. Moving hover to the async path would widen the concurrency surface for a feature that is usually cache-only, and is deliberately not done here.
5. **Two concurrent hovers on the same cold object can both call `ObjectDDL`.** The memo lock is released across the round trip because `stateMu` must never be held across I/O. The duplicate is bounded by one extra round trip.
6. **A view's hover replaces the pure hover's column table with `ViewDoc`.** Same columns, plus the view's source. Tables are never replaced, which is what the "append, never substitute" rule protects and what `TestInterBaseHoverTableStillShowsColumnTable` pins.
7. **The array-column explain failure is recognised by matching the driver's message text.** The driver exposes no typed error for it — output-type validation rejects array results before a plan exists. The raw message is printed alongside the explanation, so a wording change degrades the output to today's raw error rather than hiding anything.
