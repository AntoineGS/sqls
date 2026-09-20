# InterBase Catalog Migration, Capabilities and Cache Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the three hand-written `RDB$` catalog queries and the numeric field-type switch with the driver's pure-Go `schema.Catalog`, add the optional capability interfaces and descriptor types that sub-project 3 consumes, and extend `DBCache` with the new object kinds — without changing `DBRepository` or touching any other driver.

**Architecture:** `internal/database/capability.go` holds the driver-neutral contract: `CatalogRepository`, `DDLRepository`, `ExplainRepository`, `CatalogSnapshotRepository`, the descriptor types, `ObjectKind`, the two sentinels and `UnsupportedDDLDetail`. `internal/database/interbase_catalog.go` (untagged, because `schema` is pure Go) maps `schema.Relation`/`Constraint`/`Procedure`/`Trigger`/`Domain`/`Index`/`Sequence`/`Function` onto those descriptors, and renders types through `interBaseTypeName`, which applies one Dialect 1 override, delegates to `Domain.SQLType()`, and falls back to the **retained** `interBaseColumnType` switch on error so a rendering can only be upgraded, never degraded. `internal/database/interbase_ddl.go` implements `ObjectDDL` over `GenerateDDL()` and wraps `schema.UnsupportedDDLError` so both `errors.Is` and the structured accessor work without `capability.go` importing the driver. `CatalogSnapshot` returns a **new** repository bound to one read-only transaction that has read relations and constraints once; `DBCacheGenerator` opens one per cache build. The worker builds the extended catalog in the asynchronous secondary pass, independently of the column pass.

**Tech Stack:** Go 1.25.7, `database/sql`, `interbase-go/schema` (pure Go, stdlib-only imports, no cgo, no build tag), `github.com/mattn/go-sqlite3` for the in-memory catalog fixture, table-driven tests in the package under test.

**Spec:** `docs/superpowers/specs/2026-09-19-interbase-dialect-and-catalog-design.md`

This plan implements **plan 2 of 3** from that spec's "Plan decomposition": §4.3, §4.4, §4.5, the `interbase_catalog_test.go` / `capability_test.go` / `cache_test.go` suites, the live `TestInterBaseLiveCatalogObjectsSurface` / `TestInterBaseLiveObjectDDL` / `TestInterBaseLiveExplainPlan`, and README item 4. It does **not** touch §4.1/§4.2 (dialect resolution and the propagation seam — plan 1) or §4.6/§4.7 (connection configuration and database identity — plan 3).

**Sibling plans this one consumes and must not contradict:**

- `docs/superpowers/plans/2026-09-19-interbase-dialect-propagation.md` (plan 1) owns `ConnFactory`, `RegisterConnFactory`, `CreateRepositoryFromConnection`, and the `InterBaseDBRepository` fields `SQLDialect int` and `DatabaseName string`. Every edit below is written against that **post-plan-1** shape and says so at the edit site. Plan 1's Task 9 adds both fields and populates them; this plan is the first reader of `SQLDialect`.
- `docs/superpowers/plans/2026-09-19-interbase-connection-config.md` (plan 3) owns `interBaseConnectionConfig`, the widened charset allowlist, and `CurrentDatabase`/`Databases`/`switchDatabase`. This plan does not modify any of them.

**If plan 1 has not landed yet:** `SQLDialect` and `DatabaseName` do not exist. Add nothing; the two-field addition is plan 1's Task 9. Every task below except Task 3's dialect table can be completed against a repository without `SQLDialect` by treating the dialect as the constant `0`; but the clean order is plan 1 first, since its zero value already means Dialect 3.

## Global Constraints

Values copied verbatim from the spec. Every task's requirements implicitly include this section.

- "`DBRepository` (`internal/database/database.go:24`) does not change." No method is added to, removed from, or re-signed on that interface. Every new capability is an **optional** interface reached by type assertion; absence is the normal case.
- "§4.4 is the authoritative cross-spec contract." Sub-project 3's spec (`docs/superpowers/specs/2026-09-19-interbase-editor-features-design.md` §D1–D9) is already aligned to it. Interface names, method names, descriptor field names and types, sentinel names and cache accessor names are **reproduced exactly**. Do not rename anything, do not add fields, do not "improve" the shape. If something seems wrong, report it rather than changing it.
- "The tiebreak rule for anything not named here is: use the name and type that the driver's `schema` package already uses." That is why descriptors carry `sql.Null*` fields and `schema`'s spellings (`RelationName`, `OwnerName`, `DefaultSource`, `ValidationSource`, `CharacterSetName`, `CollationName`, `ModuleName`).
- "Cache *accessor* names follow sqls's own vocabulary (`DBCache.SortedTables`, `ColumnDescs(tableName)`), so `IndexesForTable` reads 'table' while `IndexDesc.RelationName` reads 'relation'; the boundary is deliberate — descriptors mirror the catalog, accessors mirror the cache."
- "`schema` is pure Go (stdlib-only imports)… Catalog code therefore lives **outside** the `interbase` build tag." `capability.go`, `interbase_catalog.go` and `interbase_ddl.go` carry no build tag; only `ExplainPlan` is tagged. Task 3 Step 1 verifies the pure-Go claim rather than assuming it.
- "`ExplainRepository` is *absent* rather than stubbed on untagged builds — a capability interface should not be implemented by a method that always fails."
- **Type rendering, rule order** (§4.3): 1. `domain == nil` → `""`. 2. Dialect 1 override, applied before delegating: field type 35 → `DATE`. 3. Otherwise delegate to `domain.SQLType()`. 4. "On error, fall back to the retained `interBaseColumnType` switch rather than to `TYPE(n)`. `TYPE(n)` is reached only where that switch reaches it today — an unrecognized field type — and an invalid `FieldType` yields `""`."
- "`interBaseNullability`, `interBaseColumnType`, `interBaseCharacterLength` and `interBaseNumericType` are **also kept** … re-sourced to read their inputs from `schema.Domain` (`FieldType`, `FieldSubType`, `FieldLength`, `FieldScale`, `FieldPrecision`, `CharacterLength`) instead of `interBaseColumnRow`. Their logic is unchanged." "The net effect: `SQLType()` upgrades rendering where it is strictly better … and the existing switch guarantees no case regresses."
- "The three `interBase*Query` constants and `interBaseColumnRow`, `interBaseColumnDescription`, `parseInterBaseForeignKeys` are deleted. `interBaseDefault`/`interBaseEffectiveDefault` … are kept: `schema` returns `DefaultSource` verbatim, including the keyword."
- "For the one-line `ColumnDesc.Type` the rendered text is trimmed at the first ` CHARACTER SET ` or ` COLLATE ` … the full text is kept for `DomainDesc.Type` and DDL."
- "`ColumnDesc` fields map as: `Type` from the renderer, `Null` = `"NO"` when the column's `NullFlag` or its domain's `NullFlag` is set, `Key` = `"YES"` when the column is a primary-key segment, `Default` from the column default falling back to the domain default, `Extra` = `"COMPUTED"` for computed columns … `Schema` = `""`."
- "Views stay in `SchemaTables`." `SchemaTables` continues to return tables **and** views together, exactly as `interBaseRelationsQuery` does today.
- "`ErrUnsupportedDDL` from these accessors is a normal, expected result, not a failure… The renderer therefore maps `errors.Is(err, schema.ErrUnsupportedDDL)` from `Trigger.Event()`, `FunctionArgument.SQLType()` and `Function.ReturnType()` to `""` for that one field and **carries on**: it never aborts the catalog build, never propagates the error to `DescribeFunctions`/`DescribeTriggers`, and never drops the surrounding descriptor. … A non-`ErrUnsupportedDDL` error is still returned."
- "`FunctionArgumentDesc.Type` is permanently `""` for CHAR and VARCHAR arguments: `RDB$CHARACTER_LENGTH` is not populated for function arguments." Measured: of 357 external-function arguments across three production InterBase 15.1 databases, 1 was CHAR and 0 VARCHAR; CSTRING accounted for 166, INTEGER 97, TIMESTAMP 51, DOUBLE 26, BLOB 16.
- **`ProcedureParameterDesc.Domain` is the user-versus-system domain test**: set to the resolved domain name when **both** hold, `""` otherwise — "1. the name is non-empty and does not begin with `RDB$`, compared case-insensitively after right-trimming catalog padding; and 2. the domain's `SystemFlag` is NULL or `0`."
- "A `nil, nil` result from a singular getter becomes `ErrObjectNotFound`. Returning `("", nil)` for an unknown object is specifically rejected."
- "`function` → always `ErrUnsupportedDDL`."
- "Returning a *new* repository rather than mutating the receiver is deliberate: `ReCache` runs on a handler goroutine while the worker's secondary pass runs on its own goroutine (`internal/database/worker.go:49-69`), so a shared mutable snapshot field would race."
- "The InterBase snapshot begins one read-only transaction (`db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})`, `schema.New(tx)`), reads `Relations(ctx, "")` and `Constraints(ctx, "")` **once each** … `close` rolls the transaction back."
- "`ObjectDDL` and `ExplainPlan` are interactive one-shots outside any cache build and use the `*sql.DB` directly."
- "All maps are keyed by the upper-cased object name." "Every singular accessor **normalises the name it is given**; callers pass the identifier text as the user typed it and never upper-case at the call site." "`IndexesForTable` and `TriggersForTable` normalise their table argument the same way."
- "A nil `*CatalogCache` means the active repository does not implement `CatalogRepository`." All accessors are nil-safe.
- "**The two passes must run independently.** The existing loop body `continue`s on a `GenerateDBCacheSecondary` error (`worker.go:59-63`) … Each pass is therefore attempted, logged and swapped in on its own; neither error path short-circuits the other."
- "**The update signal must not block a handler.** … The send becomes a non-blocking `select { case w.update <- struct{}{}: default: }`."
- "A catalog error is logged and leaves the previous `*CatalogCache` in place, exactly as the existing secondary column pass does."
- Sub-project 3's D8: "the capability mock must be a **distinct type** from `MockDBRepository`, because if `MockDBRepository` itself satisfied the capability interfaces, every existing handler test would start passing the type assertions and panic on nil func fields."
- "Services-backed administration (backup, restore, sweep, user management) is out of scope for the entire project."
- Explicitly deferred by the spec and **out of scope here**: "`schema` roles, dependencies and privileges in the cache" and "Bulk (non-N+1) catalog reads". Do not add them; if one seems necessary, report it.

### Dependency: three `schema` accessors are not implemented yet

`TriggerDesc.Event`, `FunctionArgumentDesc.Type` and `FunctionDesc.ReturnType` are populated from `(Trigger) Event() (string, error)`, `(FunctionArgument) SQLType() (string, error)` and `(Function) ReturnType() (string, error)`. Their **spec and plan are written, their signatures are final, and the code does not exist**:

- Spec: `/home/a.simard@multidev.local/gits/interbase-go/docs/superpowers/specs/2026-09-19-schema-catalog-accessors-design.md`
- Plan: `/home/a.simard@multidev.local/gits/interbase-go/docs/superpowers/plans/2026-09-19-schema-catalog-accessors.md`

Verified against the current checkout at planning time: `grep -n 'func (t Trigger) Event\|func (a FunctionArgument) SQLType\|func (f Function) ReturnType' schema/*.go` returns nothing.

**Consequence for execution order.** Tasks 1–11 do not reference the accessors; the three fields are populated as `""` with a comment naming the companion spec, which is exactly the documented "undecodable" value, so no consumer breaks. **Task 12 is the only task that will not compile until the driver work lands**, and it is last. Do not start Task 12 until `go build interbase-go/schema` exposes all three methods.

### Dependency: `interbase.Plan` is not implemented yet

Task 10's `ExplainPlan` calls sub-project 1's `interbase.Plan(ctx, conn, query)` (spec: `/home/a.simard@multidev.local/gits/interbase-go/docs/superpowers/specs/2026-09-19-pooled-introspection-design.md`). That code lives entirely behind `//go:build interbase && cgo && linux && amd64`, so `go test ./...` stays green whether or not it exists. Task 10 is sequenced after everything untagged.

### Build and test commands

```shell
go test ./...                                   # the whole offline suite; must be green at every task boundary
go test ./internal/database/ -count=1 -v        # this plan's main suite
CGO_ENABLED=0 go build interbase-go/schema      # proves the catalog dependency needs no cgo
CGO_ENABLED=1 go build -tags interbase ./...    # compiles the tagged InterBase adapter
CGO_ENABLED=1 go test -tags interbase ./...     # the native suite
make test                                       # build, then go test -v ./...  (Makefile:37)
```

The driver lives at `/home/a.simard@multidev.local/gits/interbase-go` via `replace interbase-go => ../interbase-go` in `go.mod`. `/opt/interbase` is present on this machine, so the tagged build runs here.

Live InterBase tests skip unless `INTERBASE_DATABASE`, `INTERBASE_USER` and `INTERBASE_PASSWORD` are set:

```shell
timeout 60s go test -tags interbase ./internal/database -run '^TestInterBaseLive' -count=1 -v -timeout=50s
```

### Committing

Other agents are active in this repository. **Never use `git add -A` or `git add .`** — every commit step below names its files explicitly. If `git add` or `git commit` reports that `index.lock` exists, wait a moment and retry, up to three times.

---

## File Structure

**Created:**

- `internal/database/capability.go` (no build tag) — the driver-neutral contract: the four capability interfaces, `ObjectKind` and its constants, the eight descriptor types, `ParameterDirection`, `ErrObjectNotFound`, `ErrUnsupportedDDL`, `unsupportedDDLDetailer` and `UnsupportedDDLDetail`. Imports `context`, `database/sql`, `errors` and nothing else — in particular **no `schema` import**, which is what keeps it upstreamable.
- `internal/database/capability_test.go` (no build tag) — contract tests: the sentinels, `UnsupportedDDLDetail`, the InterBase compile-time assertions, and the guard that no other driver's repository satisfies any capability.
- `internal/database/interbase_catalog.go` (no build tag) — `schema.Catalog` → descriptors: the read path for `SchemaTables`/`DescribeDatabaseTable*`/`DescribeForeignKeysBySchema`, dialect-aware type rendering with the retained fallback switch, the seven `CatalogRepository` methods, and `CatalogSnapshot`.
- `internal/database/interbase_catalog_test.go` (no build tag) — the SQLite catalog fixture and every offline InterBase catalog test.
- `internal/database/interbase_ddl.go` (no build tag) — `ObjectDDL` and `interBaseUnsupportedDDL`.
- `internal/database/cache_test.go` (no build tag) — `CatalogCache` generation, the `DBCache` accessors, the snapshot counting test and the worker tests.
- `internal/database/capability_mock.go` (no build tag) — `MockCatalogDBRepository`, the distinct capability mock sub-project 3 consumes.

**Modified:**

- `internal/database/interbase_common.go` — deletes `interBaseRelationsQuery`, `interBaseColumnsQuery`, `interBaseForeignKeysQuery`, `interBaseColumnRow`, `interBaseColumnDescription`, `parseInterBaseForeignKeys`, `relationNames`, `describeColumns`, `interBaseColumnType`, `interBaseNumericType`, `interBaseCharacterLength` and the four `DBRepository` read methods, which move to `interbase_catalog.go`. Keeps `interBaseAttachment`, `interBaseCharset`, the struct, the constructors, `interBaseNullability`, `interBaseDefault`, `interBaseEffectiveDefault`, `Exec` and `Query`.
- `internal/database/cache.go` — `DBCache.Catalog`, the catalog accessors, `GenerateCatalogCache`, and the snapshot wrapper around the three generate entry points.
- `internal/database/worker.go` — `setCatalogCache`, the independent catalog pass in `Start`, and the non-blocking `updateAdditionalCache`.
- `internal/database/interbase_test.go` — the old fixture (`openInterBaseCatalogFixture`, `:293-414`), the migrated test (`:13-122`) and `TestInterBaseCatalogQueriesAvoidUnsupportedTrimFunction` (`:280-291`, which asserts on three constants this plan deletes) are removed in Task 3. Everything else in that file stays.
- `internal/database/interbase_native.go` (tagged) — `ExplainPlan`.
- `internal/database/interbase_stub_test.go` (tagged `!interbase || !cgo || !linux || !amd64`) — the "`ExplainRepository` is absent" assertion.
- `internal/database/interbase_live_test.go` (tagged) — three live tests.
- `README.md:290-348` — item 4 of the documentation rewrite (metadata depth only; items 1, 2, 3 and 5 belong to plans 1 and 3).

### Fixture naming, and why the spec's name is not used immediately

The spec names the fixture helper `openInterBaseCatalogFixture(t) *sql.DB` — but that name is **already taken** by the fixture for the hand-written queries (`interbase_test.go:293`), which Task 3 deletes. Task 2 therefore introduces the new one as `openInterBaseSchemaFixture(t) *sql.DB` and keeps that name permanently; renaming it after Task 3 would churn two files for no behavior change. The name is test-local and no other plan references it.

### Collision: who owns `TestInterBaseCurrentDatabaseAndDatabases`

The spec lists this test under **this plan's** `interbase_catalog_test.go` (§6), but §4.7 — the behavior it asserts — is **plan 3's** scope, and plan 3 already implements it in `interbase_config_test.go` (that plan's Task 4, with an explicit note deferring to this plan if this one lands first). Two files in one Go package cannot both declare it.

**Decision: plan 3 owns it, in `internal/database/interbase_config_test.go`.** This plan does **not** declare `TestInterBaseCurrentDatabaseAndDatabases`, because this plan does not change `CurrentDatabase` or `Databases` — they keep returning `""` and `[]string{}` (`interbase_common.go:109-115`) until plan 3 wires `DatabaseName` into them. A test here would either assert today's inert behavior under a name that promises plan 3's behavior, or assert behavior this plan does not implement. The existing inert-behavior assertions at `interbase_test.go:18-23` are preserved verbatim inside this plan's migrated repository test (Task 3), so the "no identity" branch stays covered at every commit.

If an executor finds that plan 3 has already landed its copy, nothing needs doing here.

### Two places where the real code contradicts the spec

Both were found by running the code, not by reading it. They are handled inside the tasks and repeated here so a reviewer sees them without reading every step.

1. **`Catalog.Domains` cannot run on plain SQLite.** Its query appends `AND f.RDB$FIELD_NAME NOT STARTING WITH 'RDB$'` (`schema/catalog_extended.go:304`). `STARTING WITH` is InterBase syntax; SQLite rejects it (`sqlite3 :memory: "… WHERE a NOT STARTING WITH 'x'"` → `Parse error … near "STARTING"`). The spec's testing strategy assumes the whole extended catalog is reachable from the SQLite fixture; it is not, for this one method. Task 2 solves it in the **test** layer with a tiny `database/sql` driver that wraps `go-sqlite3` and rewrites that one clause — no production seam, no change to how the repository builds its catalog.
2. **InterBase CHAR padding has no SQLite equivalent, and `schema` re-queries children by trimmed name.** The existing fixture right-pads every identifier to 31 characters (`interbase_test.go:416-418`) and the hand-written queries join padded-to-padded. `schema` trims identifiers when it scans them and then passes the **trimmed** name back as a bind parameter for columns, index segments, procedure parameters and function arguments; on SQLite `'CUSTOMER' = 'CUSTOMER   '` is false, so every child read returns empty. Measured with a prototype: with padded names `Relations` returned 4 relations with **0 columns** each and `Constraints` returned 3 constraints with **0 columns**. The new fixture therefore stores **unpadded** identifiers in every column used as a join key or bind parameter, and keeps padding on display-only columns (`RDB$OWNER_NAME`, `RDB$CHARACTER_SET_NAME`, `RDB$COLLATION_NAME`, `RDB$MODULE_NAME`, `RDB$ENTRYPOINT`, and the two `RDB$FIELD_NAME` columns that are only ever read back) so the trimming behavior stays pinned.

---

## Task 1: The capability contract

`internal/database/capability.go` is the file sub-project 3 compiles against. It is reproduced from spec §4.4 exactly; this task's test is the contract's own regression guard.

**Files:**
- Create: `internal/database/capability.go`
- Create: `internal/database/capability_test.go`

**Interfaces:**
- Consumes: `DBRepository` (`internal/database/database.go:24-36`), unchanged.
- Produces (all in package `database`):
  - `type CatalogRepository interface { DescribeViews(ctx context.Context) ([]*ViewDesc, error); DescribeProcedures(ctx context.Context) ([]*ProcedureDesc, error); DescribeGenerators(ctx context.Context) ([]*GeneratorDesc, error); DescribeTriggers(ctx context.Context) ([]*TriggerDesc, error); DescribeDomains(ctx context.Context) ([]*DomainDesc, error); DescribeIndexes(ctx context.Context) ([]*IndexDesc, error); DescribeFunctions(ctx context.Context) ([]*FunctionDesc, error) }`
  - `type DDLRepository interface { ObjectDDL(ctx context.Context, kind ObjectKind, name string) (string, error) }`
  - `type ExplainRepository interface { ExplainPlan(ctx context.Context, query string) (string, error) }`
  - `type CatalogSnapshotRepository interface { CatalogSnapshot(ctx context.Context) (repo DBRepository, close func() error, err error) }`
  - `type ObjectKind string` with `ObjectKindTable`, `ObjectKindView`, `ObjectKindProcedure`, `ObjectKindTrigger`, `ObjectKindDomain`, `ObjectKindIndex`, `ObjectKindGenerator`, `ObjectKindFunction`
  - `type ParameterDirection string` with `ParameterInput`, `ParameterOutput`
  - `ViewDesc`, `GeneratorDesc`, `ProcedureDesc`, `ProcedureParameterDesc`, `TriggerDesc`, `DomainDesc`, `IndexDesc`, `FunctionDesc`, `FunctionArgumentDesc`
  - `var ErrObjectNotFound`, `var ErrUnsupportedDDL`
  - `func UnsupportedDDLDetail(err error) (object, name, feature string, ok bool)`

- [ ] **Step 1: Write the failing test**

Create `internal/database/capability_test.go`:

```go
package database

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

// detailedError stands in for a driver-specific error that carries a
// structured reason. capability.go must reach it through errors.As without
// importing any driver package.
type detailedError struct {
	object  string
	name    string
	feature string
}

func (e *detailedError) Error() string { return fmt.Sprintf("test: %s %q: %s", e.object, e.name, e.feature) }
func (e *detailedError) Unwrap() error { return ErrUnsupportedDDL }
func (e *detailedError) UnsupportedDDLDetail() (string, string, string) {
	return e.object, e.name, e.feature
}

func TestUnsupportedDDLDetail(t *testing.T) {
	detailed := &detailedError{object: "table", name: "ORDERS", feature: `column "TOTAL" is computed`}

	tests := []struct {
		name        string
		err         error
		wantObject  string
		wantName    string
		wantFeature string
		wantOK      bool
	}{
		{
			name:        "a detailed error reports its structure",
			err:         detailed,
			wantObject:  "table",
			wantName:    "ORDERS",
			wantFeature: `column "TOTAL" is computed`,
			wantOK:      true,
		},
		{
			name:        "a wrapped detailed error is still reachable",
			err:         fmt.Errorf("interbase: %w", detailed),
			wantObject:  "table",
			wantName:    "ORDERS",
			wantFeature: `column "TOTAL" is computed`,
			wantOK:      true,
		},
		{name: "a bare sentinel carries no detail", err: ErrUnsupportedDDL},
		{name: "an unrelated error carries no detail", err: errors.New("boom")},
		{name: "a nil error carries no detail", err: nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object, name, feature, ok := UnsupportedDDLDetail(test.err)
			if ok != test.wantOK {
				t.Fatalf("UnsupportedDDLDetail() ok = %v, want %v", ok, test.wantOK)
			}
			if object != test.wantObject || name != test.wantName || feature != test.wantFeature {
				t.Fatalf("UnsupportedDDLDetail() = (%q, %q, %q), want (%q, %q, %q)",
					object, name, feature, test.wantObject, test.wantName, test.wantFeature)
			}
		})
	}

	// Both the sentinel check and the structured accessor must work on the
	// same error: sub-project 3 branches on the first and renders the second.
	if !errors.Is(detailed, ErrUnsupportedDDL) {
		t.Error("a detailed unsupported-DDL error must satisfy errors.Is(err, ErrUnsupportedDDL)")
	}
	if errors.Is(ErrObjectNotFound, ErrUnsupportedDDL) {
		t.Error("the two sentinels must stay distinct")
	}
}

func TestCapabilityDescriptorShape(t *testing.T) {
	// This test exists so a later edit cannot silently rename or retype a
	// contract field: sub-project 3 compiles against these exact names.
	view := &ViewDesc{
		Schema:      "",
		Name:        "CUSTOMER_VIEW",
		OwnerName:   sql.NullString{String: "SYSDBA", Valid: true},
		ViewSource:  sql.NullString{String: "SELECT 1 FROM RDB$DATABASE", Valid: true},
		Description: sql.NullString{},
		Columns:     []*ColumnDesc{{ColumnBase: ColumnBase{Table: "CUSTOMER_VIEW", Name: "ID"}, Type: "INTEGER"}},
	}
	procedure := &ProcedureDesc{
		Name:   "ADD_CUSTOMER",
		Source: sql.NullString{String: "BEGIN END", Valid: true},
		InputParameters: []*ProcedureParameterDesc{{
			Name:      "EMAIL",
			Position:  0,
			Direction: ParameterInput,
			Type:      "VARCHAR(100)",
			Domain:    "EMAIL_ADDRESS",
			Nullable:  sql.NullBool{Bool: false, Valid: true},
		}},
		OutputParameters: []*ProcedureParameterDesc{{Name: "NEW_ID", Position: 0, Direction: ParameterOutput, Type: "INTEGER"}},
	}
	trigger := &TriggerDesc{
		Name:         "CUSTOMER_BI",
		RelationName: sql.NullString{String: "CUSTOMER", Valid: true},
		Event:        "BEFORE INSERT",
		Sequence:     sql.NullInt64{Int64: 0, Valid: true},
		Active:       sql.NullBool{Bool: true, Valid: true},
	}
	domain := &DomainDesc{
		Name:             "EMAIL_ADDRESS",
		Type:             `VARCHAR(100) CHARACTER SET "UTF8"`,
		Nullable:         sql.NullBool{Bool: false, Valid: true},
		DefaultSource:    sql.NullString{String: "DEFAULT 'a@b'", Valid: true},
		ValidationSource: sql.NullString{String: "CHECK (VALUE LIKE '%@%')", Valid: true},
		CharacterSetName: sql.NullString{String: "UTF8", Valid: true},
		CollationName:    sql.NullString{},
	}
	index := &IndexDesc{
		Name:           "IDX_CUSTOMER_PK",
		RelationName:   "CUSTOMER",
		Columns:        []string{"ID"},
		Expression:     sql.NullString{},
		Unique:         sql.NullBool{Bool: true, Valid: true},
		Active:         sql.NullBool{Bool: true, Valid: true},
		ConstraintName: sql.NullString{String: "PK_CUSTOMER", Valid: true},
	}
	function := &FunctionDesc{
		Name:           "F_LTRIM",
		ReturnType:     "CSTRING(255)",
		ReturnPosition: sql.NullInt64{Int64: 1, Valid: true},
		Arguments:      []*FunctionArgumentDesc{{Name: "F_LTRIM_1", Position: sql.NullInt64{Int64: 1, Valid: true}, Type: "CSTRING(255)"}},
		ModuleName:     sql.NullString{String: "ib_udf", Valid: true},
		EntryPoint:     sql.NullString{String: "IB_LTRIM", Valid: true},
	}
	generator := &GeneratorDesc{Name: "GEN_CUSTOMER_ID", ID: sql.NullInt64{Int64: 1, Valid: true}}

	if view.Columns[0].Name != "ID" || procedure.InputParameters[0].Direction != ParameterInput {
		t.Fatal("descriptor composition is wrong")
	}
	if trigger.Event != "BEFORE INSERT" || domain.CollationName.Valid || !index.Unique.Bool {
		t.Fatal("descriptor fields are wrong")
	}
	if function.Arguments[0].Type != "CSTRING(255)" || !generator.ID.Valid {
		t.Fatal("descriptor fields are wrong")
	}

	wantKinds := []ObjectKind{
		ObjectKindTable, ObjectKindView, ObjectKindProcedure, ObjectKindTrigger,
		ObjectKindDomain, ObjectKindIndex, ObjectKindGenerator, ObjectKindFunction,
	}
	wantText := []string{"table", "view", "procedure", "trigger", "domain", "index", "generator", "function"}
	for i, kind := range wantKinds {
		if string(kind) != wantText[i] {
			t.Errorf("ObjectKind %d = %q, want %q", i, kind, wantText[i])
		}
	}
	if string(ParameterInput) != "input" || string(ParameterOutput) != "output" {
		t.Errorf("ParameterDirection values = (%q, %q), want (\"input\", \"output\")", ParameterInput, ParameterOutput)
	}
}

func TestNonInterBaseRepositoriesDoNotImplementCapabilities(t *testing.T) {
	// The guard that this change stays additive: no other driver's repository
	// may accidentally satisfy a capability, and MockDBRepository must not
	// either, or every existing handler test would start taking the capability
	// branch and panic on a nil func field.
	repositories := map[dialect.DatabaseDriver]DBRepository{
		dialect.DatabaseDriverMySQL:      NewMySQLDBRepository(nil),
		dialect.DatabaseDriverPostgreSQL: NewPostgreSQLDBRepository(nil),
		dialect.DatabaseDriverSQLite3:    NewSQLite3DBRepository(nil),
		dialect.DatabaseDriverMssql:      NewMssqlDBRepository(nil),
		dialect.DatabaseDriverH2:         NewH2DBRepository(nil),
		dialect.DatabaseDriverVertica:    NewVerticaDBRepository(nil),
		dialect.DatabaseDriverClickhouse: NewClickhouseRepository(nil),
		dialect.DatabaseDriverOracle:     NewOracleDBRepository(nil),
		dialect.DatabaseDriver("mock"):   NewMockDBRepository(nil),
	}

	for driver, repository := range repositories {
		t.Run(string(driver), func(t *testing.T) {
			if _, ok := repository.(CatalogRepository); ok {
				t.Errorf("%s must not implement CatalogRepository", driver)
			}
			if _, ok := repository.(DDLRepository); ok {
				t.Errorf("%s must not implement DDLRepository", driver)
			}
			if _, ok := repository.(ExplainRepository); ok {
				t.Errorf("%s must not implement ExplainRepository", driver)
			}
			if _, ok := repository.(CatalogSnapshotRepository); ok {
				t.Errorf("%s must not implement CatalogSnapshotRepository", driver)
			}
		})
	}
}
```

Before running, confirm the seven constructor names against the source — `grep -n 'func New.*DBRepository' internal/database/*.go` — and fix any that differ. They are the exported `Factory` functions each driver registers.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/database/ -count=1 -run 'UnsupportedDDLDetail|CapabilityDescriptorShape|NonInterBaseRepositories' -v`

Expected: FAIL to build — `undefined: ErrUnsupportedDDL`, `undefined: ViewDesc`, `undefined: ObjectKindTable`, `undefined: UnsupportedDDLDetail`, and so on for every contract name.

- [ ] **Step 3: Create `internal/database/capability.go`**

```go
package database

import (
	"context"
	"database/sql"
	"errors"
)

// CatalogRepository is implemented by repositories that can enumerate catalog
// objects beyond tables and columns. All methods return objects for the whole
// attachment; sqls drivers without a schema namespace use an empty Schema.
type CatalogRepository interface {
	DescribeViews(ctx context.Context) ([]*ViewDesc, error)
	DescribeProcedures(ctx context.Context) ([]*ProcedureDesc, error)
	DescribeGenerators(ctx context.Context) ([]*GeneratorDesc, error)
	DescribeTriggers(ctx context.Context) ([]*TriggerDesc, error)
	DescribeDomains(ctx context.Context) ([]*DomainDesc, error)
	DescribeIndexes(ctx context.Context) ([]*IndexDesc, error)
	DescribeFunctions(ctx context.Context) ([]*FunctionDesc, error)
}

// DDLRepository is implemented by repositories that can reproduce an object's
// definition. Callers must handle ErrObjectNotFound and ErrUnsupportedDDL.
type DDLRepository interface {
	ObjectDDL(ctx context.Context, kind ObjectKind, name string) (string, error)
}

// ExplainRepository is implemented by repositories that can return a server
// query plan without executing the statement's result set.
type ExplainRepository interface {
	ExplainPlan(ctx context.Context, query string) (string, error)
}

// CatalogSnapshotRepository is implemented by repositories that can serve a
// whole cache build from one consistent catalog read. The returned repository
// is read-only and valid until close is called; the source repository is
// unaffected and remains usable concurrently.
type CatalogSnapshotRepository interface {
	CatalogSnapshot(ctx context.Context) (repo DBRepository, close func() error, err error)
}

var (
	// ErrObjectNotFound reports that the named catalog object does not exist.
	ErrObjectNotFound = errors.New("database: catalog object not found")
	// ErrUnsupportedDDL reports that the catalog cannot reproduce the object's
	// definition faithfully. Use UnsupportedDDLDetail for the structured reason.
	ErrUnsupportedDDL = errors.New("database: DDL is unavailable for this object")
)

// unsupportedDDLDetailer is implemented by driver-specific errors that carry a
// structured reason. It keeps this file free of any driver import.
type unsupportedDDLDetailer interface {
	UnsupportedDDLDetail() (object, name, feature string)
}

// UnsupportedDDLDetail reports the structured reason behind an
// ErrUnsupportedDDL error: the object kind, the object name, and the metadata
// facet that could not be rendered. ok is false when err carries no detail.
func UnsupportedDDLDetail(err error) (object, name, feature string, ok bool) {
	var detailer unsupportedDDLDetailer
	if !errors.As(err, &detailer) {
		return "", "", "", false
	}
	object, name, feature = detailer.UnsupportedDDLDetail()
	return object, name, feature, true
}

// ObjectKind identifies a catalog object kind for capability lookups.
type ObjectKind string

const (
	ObjectKindTable     ObjectKind = "table"
	ObjectKindView      ObjectKind = "view"
	ObjectKindProcedure ObjectKind = "procedure"
	ObjectKindTrigger   ObjectKind = "trigger"
	ObjectKindDomain    ObjectKind = "domain"
	ObjectKindIndex     ObjectKind = "index"
	ObjectKindGenerator ObjectKind = "generator"
	ObjectKindFunction  ObjectKind = "function"
)

// ViewDesc describes a view, its source text, and its resolved columns.
type ViewDesc struct {
	Schema      string // "" for InterBase
	Name        string
	OwnerName   sql.NullString
	ViewSource  sql.NullString
	Description sql.NullString
	Columns     []*ColumnDesc // ordered; same rendering as table columns
}

// GeneratorDesc describes a generator/sequence. InterBase's own DDL is
// CREATE GENERATOR, and the catalog carries nothing but identity.
type GeneratorDesc struct {
	Schema string
	Name   string
	ID     sql.NullInt64
}

// ProcedureDesc describes a stored procedure and its ordered parameters.
type ProcedureDesc struct {
	Schema           string
	Name             string
	OwnerName        sql.NullString
	Source           sql.NullString // PSQL body verbatim
	Description      sql.NullString
	InputParameters  []*ProcedureParameterDesc // ordered by Position
	OutputParameters []*ProcedureParameterDesc // ordered by Position
}

// ParameterDirection identifies a parameter's direction. The values match
// schema.ParameterInput and schema.ParameterOutput.
type ParameterDirection string

const (
	ParameterInput  ParameterDirection = "input"
	ParameterOutput ParameterDirection = "output"
)

// ProcedureParameterDesc describes one procedure parameter.
type ProcedureParameterDesc struct {
	Name        string
	Position    int
	Direction   ParameterDirection
	Type        string         // rendered for the resolved dialect; "" when unrenderable
	Domain      string         // user domain name; "" for an inline type
	Nullable    sql.NullBool   // invalid when the catalog cannot determine it
	Description sql.NullString
}

// TriggerDesc describes a DML or database trigger.
type TriggerDesc struct {
	Schema       string
	Name         string
	RelationName sql.NullString // invalid for a database-level trigger
	Event        string         // e.g. "BEFORE INSERT"; "" when undecodable
	Sequence     sql.NullInt64
	Active       sql.NullBool
	Source       sql.NullString
	Description  sql.NullString
}

// DomainDesc describes a user domain.
type DomainDesc struct {
	Schema           string
	Name             string
	Type             string // full rendering, including CHARACTER SET/COLLATE
	Nullable         sql.NullBool
	DefaultSource    sql.NullString
	ValidationSource sql.NullString // CHECK text, verbatim
	CharacterSetName sql.NullString
	CollationName    sql.NullString
	Description      sql.NullString
}

// IndexDesc describes an index and its ordered segments.
type IndexDesc struct {
	Schema         string
	Name           string
	RelationName   string
	Columns        []string       // ordered segment names; empty for an expression index
	Expression     sql.NullString // invalid for a segment index
	Unique         sql.NullBool
	Active         sql.NullBool
	ConstraintName sql.NullString // owning constraint; invalid when standalone
	Description    sql.NullString
}

// FunctionDesc describes an external function (UDF) declaration. sqls never
// invokes a UDF; this is declaration metadata only.
type FunctionDesc struct {
	Schema         string
	Name           string
	ReturnType     string // rendered; "" when the catalog cannot render it
	ReturnPosition sql.NullInt64
	Arguments      []*FunctionArgumentDesc // ordered by Position
	ModuleName     sql.NullString
	EntryPoint     sql.NullString
	Description    sql.NullString
}

// FunctionArgumentDesc describes one external function argument.
type FunctionArgumentDesc struct {
	Name     string
	Position sql.NullInt64
	Type     string // rendered; "" when the catalog cannot render it
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/database/ -count=1 -run 'UnsupportedDDLDetail|CapabilityDescriptorShape|NonInterBaseRepositories' -v`
Expected: PASS, every subtest.

- [ ] **Step 5: Run the whole suite**

Run: `go test ./...`
Expected: every package `ok`. Nothing else references these names yet, so any failure means a name collision with an existing identifier in package `database` — rename nothing in the contract; report it instead.

- [ ] **Step 6: Commit**

```bash
git add internal/database/capability.go internal/database/capability_test.go
git commit -m "feat(database): add the driver-neutral catalog capability contract"
```

---

## Task 2: The SQLite catalog fixture `schema` can read

Test-only. The fixture is what makes every later task testable with plain `go test ./...`, and it has two non-obvious problems — the `NOT STARTING WITH` clause and CHAR padding — that a reviewer should be able to judge on their own.

One consequence worth stating so nobody later reads it as a regression: this file
imports `github.com/mattn/go-sqlite3` **by name** (it needs `sqlite3.SQLiteDriver`
to wrap), where today the package only ever reaches SQLite through
`sql.Open("sqlite3", …)`. That makes `go test ./internal/database` fail to
*compile* under `CGO_ENABLED=0`, where today it compiles and fails at run time
instead. Nothing real is lost — the suite already requires a working SQLite
driver — and the untagged *production* build is unaffected, because this is a
`_test.go` file.

**Files:**
- Create: `internal/database/interbase_catalog_test.go`
- Test: itself

**Interfaces:**
- Consumes: `interBaseFixed(value string) string` (`internal/database/interbase_test.go:416-418`), which right-pads to 31 characters; `schema.New`, `schema.Catalog` (`interbase-go/schema`).
- Produces (test scope):
  - `func openInterBaseSchemaFixture(t *testing.T) *sql.DB` — an in-memory catalog the `schema` package can read end to end.
  - `func interBaseFixtureCountPrepares(t *testing.T) func() int64` — resets and reads the statement counter; Task 8 uses it to measure the N+1 mitigation.
  - The registered driver name `"sqlite3_interbase_catalog"`.

- [ ] **Step 1: Write the fixture and its smoke test**

Create `internal/database/interbase_catalog_test.go`:

```go
package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
	"interbase-go/schema"
)

// The schema package's domain list filters system domains with
// "RDB$FIELD_NAME NOT STARTING WITH 'RDB$'" (schema/catalog_extended.go:304).
// STARTING WITH is InterBase syntax and SQLite rejects it outright, so the
// fixture is reached through a driver that rewrites that one clause into the
// LIKE form SQLite understands. Nothing in production does this: it exists so
// the pure-Go catalog reader can be exercised without a server.
type interBaseFixtureDriver struct{ inner driver.Driver }

func (d interBaseFixtureDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return interBaseFixtureConn{Conn: conn}, nil
}

type interBaseFixtureConn struct{ driver.Conn }

func interBaseFixtureRewrite(query string) string {
	return strings.ReplaceAll(query, "NOT STARTING WITH 'RDB$'", "NOT LIKE 'RDB$%'")
}

// interBaseFixturePrepares counts every statement the fixture prepares. Because
// this wrapper forces all traffic through PrepareContext (see below), the count
// is the exact round-trip count for a catalog build, which is what turns Task
// 8's N+1 claim into a measurement instead of an argument.
var interBaseFixturePrepares atomic.Int64

// interBaseFixtureCountPrepares resets the counter and returns a reader for it.
// Tests using it must not run in parallel with each other.
func interBaseFixtureCountPrepares(t *testing.T) func() int64 {
	t.Helper()
	interBaseFixturePrepares.Store(0)
	return interBaseFixturePrepares.Load
}

// Embedding driver.Conn promotes only the driver.Conn methods, so this wrapper
// deliberately does not satisfy driver.QueryerContext; database/sql therefore
// routes every statement through PrepareContext, where the rewrite applies.
func (c interBaseFixtureConn) Prepare(query string) (driver.Stmt, error) {
	interBaseFixturePrepares.Add(1)
	return c.Conn.Prepare(interBaseFixtureRewrite(query))
}

func (c interBaseFixtureConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	interBaseFixturePrepares.Add(1)
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, interBaseFixtureRewrite(query))
	}
	return c.Conn.Prepare(interBaseFixtureRewrite(query))
}

func (c interBaseFixtureConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if beginner, ok := c.Conn.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}

func init() {
	sql.Register("sqlite3_interbase_catalog", interBaseFixtureDriver{inner: &sqlite3.SQLiteDriver{}})
}

var interBaseFixtureSequence atomic.Int64

// openInterBaseSchemaFixture builds an in-memory RDB$ catalog that the schema
// package reads exactly as it reads a real one.
//
// Identifiers used as a join key or a bind parameter are stored UNPADDED.
// InterBase CHAR comparison pads both operands, so 'CUSTOMER' = 'CUSTOMER   '
// there; SQLite compares TEXT byte for byte, and schema re-queries columns,
// index segments, procedure parameters and function arguments with the name it
// already trimmed. Padded keys therefore yield relations with zero columns.
// Display-only columns stay padded so the trimming behavior remains pinned.
//
// The database is shared-cache in-memory with a unique name per call, so a
// read-only transaction and a second pooled connection can be open at once.
func openInterBaseSchemaFixture(t *testing.T) *sql.DB {
	t.Helper()

	name := fmt.Sprintf("file:interbase_catalog_%d?mode=memory&cache=shared", interBaseFixtureSequence.Add(1))
	db, err := sql.Open("sqlite3_interbase_catalog", name)
	if err != nil {
		t.Fatalf("sql.Open(sqlite3_interbase_catalog) error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxIdleConns(4)

	for _, statement := range interBaseFixtureTables {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create catalog fixture table: %v: %s", err, statement)
		}
	}

	insert := func(statement string, args ...any) {
		t.Helper()
		if _, err := db.Exec(statement, args...); err != nil {
			t.Fatalf("insert catalog fixture row: %v: %s", err, statement)
		}
	}

	// RDB$RELATIONS: three tables, one view, one system relation.
	relation := func(name string, id int, viewBLR, viewSource any, systemFlag int) {
		insert(`INSERT INTO "RDB$RELATIONS" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			name, id, viewSource, nil, nil, interBaseFixed("SYSDBA"), nil, 8, 1, nil, 0,
			interBaseFixed("PERSISTENT"), systemFlag, viewBLR)
	}
	relation("CHILD", 1, nil, nil, 0)
	relation("CUSTOMER", 2, nil, nil, 0)
	relation("CUSTOMER_VIEW", 3, "view blr", "SELECT ID FROM CUSTOMER", 0)
	relation("PARENT", 4, nil, nil, 0)
	relation("RDB$SYSTEM", 5, nil, nil, 1)

	// RDB$RELATION_FIELDS. ORPHAN's field source has no RDB$FIELDS row: the
	// old inner JOIN dropped it, schema LEFT JOINs and keeps it.
	field := func(relationName, name, source string, position int, nullFlag, defaultSource, computedFlag any) {
		_ = computedFlag
		insert(`INSERT INTO "RDB$RELATION_FIELDS" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			interBaseFixed(name), relationName, source, position, nil, position, nil, 0, nil,
			nullFlag, defaultSource, nil, nil, nil)
	}
	field("CHILD", "CHILD_B", "CHILD_B", 0, nil, nil, nil)
	field("CHILD", "CHILD_A", "CHILD_A", 1, nil, nil, nil)
	field("CHILD", "ORPHAN", "NO_SUCH_DOMAIN", 2, nil, nil, nil)
	field("CUSTOMER", "ID", "CUSTOMER_ID", 0, 1, nil, nil)
	field("CUSTOMER", "CODE", "CUSTOMER_CODE", 1, nil, nil, nil)
	field("CUSTOMER", "CREATED", "CUSTOMER_CREATED", 2, nil, nil, nil)
	field("CUSTOMER", "AMOUNT", "CUSTOMER_AMOUNT", 3, nil, nil, nil)
	field("CUSTOMER", "LABEL", "CUSTOMER_LABEL", 4, nil, " DEFAULT '  seeded  '   ", nil)
	field("CUSTOMER", "INHERITED", "CUSTOMER_INHERITED", 5, nil, nil, nil)
	field("CUSTOMER", "OVERRIDE", "CUSTOMER_OVERRIDE", 6, nil, " DEFAULT 'column' ", nil)
	field("CUSTOMER", "DEFAULT_NULL", "CUSTOMER_DEFAULT_NULL", 7, nil, " DEFAULT NULL ", nil)
	field("CUSTOMER", "REQUIRED", "CUSTOMER_REQUIRED", 8, 1, nil, nil)
	field("CUSTOMER", "DOUBLE_AMOUNT", "CUSTOMER_DOUBLE_AMOUNT", 9, nil, nil, nil)
	field("CUSTOMER", "TOTAL", "CUSTOMER_TOTAL", 10, nil, nil, nil)
	field("CUSTOMER_VIEW", "VIEW_ID", "CUSTOMER_ID", 0, nil, nil, nil)
	field("PARENT", "PARENT_B", "PARENT_B", 0, nil, nil, nil)
	field("PARENT", "PARENT_A", "PARENT_A", 1, nil, nil, nil)

	// RDB$FIELDS. Column order matches schema's domain projection exactly.
	domain := func(name string, fieldType int, subType, length, scale, precision, characterLength,
		characterSetID, collationID, nullFlag, defaultSource, validationSource, computedSource, dimensions any) {
		insert(`INSERT INTO "RDB$FIELDS" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			name, validationSource, computedSource, defaultSource, length, scale, fieldType, subType,
			nil, 0, nil, nil, nil, nil, dimensions, nullFlag, characterLength, collationID,
			characterSetID, precision)
	}
	domain("CHILD_B", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("CHILD_A", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("CUSTOMER_ID", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("CUSTOMER_CODE", 14, 0, 10, 0, nil, 10, 4, 2, nil, nil, nil, nil, nil)
	domain("CUSTOMER_CREATED", 35, 0, 8, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("CUSTOMER_AMOUNT", 8, 1, 4, -2, 9, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("CUSTOMER_LABEL", 37, 0, 20, 0, nil, 20, nil, nil, nil, nil, nil, nil, nil)
	domain("CUSTOMER_INHERITED", 37, 0, 20, 0, nil, 20, nil, nil, 1, " DEFAULT 'domain' ", nil, nil, nil)
	domain("CUSTOMER_OVERRIDE", 37, 0, 20, 0, nil, 20, nil, nil, nil, " DEFAULT 'domain' ", nil, nil, nil)
	domain("CUSTOMER_DEFAULT_NULL", 37, 0, 20, 0, nil, 20, nil, nil, nil, " DEFAULT 'domain' ", nil, nil, nil)
	domain("CUSTOMER_REQUIRED", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("CUSTOMER_DOUBLE_AMOUNT", 27, 1, 8, 0, 15, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("CUSTOMER_TOTAL", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, "COMPUTED BY (AMOUNT * 2)", nil)
	domain("PARENT_B", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("PARENT_A", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	// A user domain and a system domain, for the procedure-parameter test.
	domain("EMAIL_ADDRESS", 37, 0, 100, 0, nil, 100, nil, nil, 1, " DEFAULT 'a@b' ",
		"CHECK (VALUE LIKE '%@%')", nil, nil)
	domain("RDB$1", 8, 0, 4, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	domain("LEGACY_FLAG", 7, 0, 2, 0, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	insert(`INSERT INTO "RDB$CHARACTER_SETS" VALUES (?,?)`, 4, interBaseFixed("UTF8"))
	insert(`INSERT INTO "RDB$COLLATIONS" VALUES (?,?,?)`, 4, 2, interBaseFixed("UNICODE"))
	insert(`INSERT INTO "RDB$VIEW_RELATIONS" VALUES (?,?,?)`, "CUSTOMER_VIEW", 1, "CUSTOMER")

	constraint := func(name, kind, relationName, indexName string) {
		insert(`INSERT INTO "RDB$RELATION_CONSTRAINTS" VALUES (?,?,?,?,?,?)`,
			name, kind, relationName, nil, nil, indexName)
	}
	constraint("FK_CHILD", "FOREIGN KEY", "CHILD", "IDX_CHILD_FK")
	constraint("PK_CUSTOMER", "PRIMARY KEY", "CUSTOMER", "IDX_CUSTOMER_PK")
	constraint("PK_PARENT", "PRIMARY KEY", "PARENT", "IDX_PARENT_PK")
	insert(`INSERT INTO "RDB$REF_CONSTRAINTS" VALUES (?,?,?,?,?)`,
		"FK_CHILD", "PK_PARENT", nil, "RESTRICT", "RESTRICT")

	// RDB$INDEX_TYPE must be 0 (ascending), not NULL: Index.GenerateDDL
	// refuses a NULL direction, and Task 7 generates DDL for an index.
	index := func(name, relationName string, id int, uniqueFlag, inactive, segmentCount any) {
		insert(`INSERT INTO "RDB$INDICES" VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			name, relationName, id, uniqueFlag, nil, segmentCount, inactive, 0, nil, 0, nil, nil)
	}
	index("IDX_CHILD_FK", "CHILD", 1, 0, 0, 2)
	index("IDX_CUSTOMER_CODE", "CUSTOMER", 2, 1, 1, 1)
	index("IDX_CUSTOMER_PK", "CUSTOMER", 3, 1, 0, 1)
	index("IDX_PARENT_PK", "PARENT", 4, 1, 0, 2)

	segment := func(indexName, fieldName string, position int) {
		insert(`INSERT INTO "RDB$INDEX_SEGMENTS" VALUES (?,?,?,?)`,
			indexName, interBaseFixed(fieldName), position, nil)
	}
	segment("IDX_CUSTOMER_PK", "ID", 0)
	segment("IDX_PARENT_PK", "PARENT_B", 0)
	segment("IDX_PARENT_PK", "PARENT_A", 1)
	segment("IDX_CHILD_FK", "CHILD_B", 0)
	segment("IDX_CHILD_FK", "CHILD_A", 1)
	segment("IDX_CUSTOMER_CODE", "CODE", 0)

	insert(`INSERT INTO "RDB$PROCEDURES" VALUES (?,?,?,?,?,?,?,?,?)`,
		"ADD_CUSTOMER", 1, 2, 1, nil, "BEGIN NEW_ID = 1; END", nil, interBaseFixed("SYSDBA"), 0)
	parameter := func(name string, number, parameterType int, fieldSource string) {
		insert(`INSERT INTO "RDB$PROCEDURE_PARAMETERS" VALUES (?,?,?,?,?,?,?)`,
			interBaseFixed(name), "ADD_CUSTOMER", number, parameterType, fieldSource, nil, 0)
	}
	parameter("EMAIL", 0, 0, "EMAIL_ADDRESS")
	parameter("CODE", 1, 0, "RDB$1")
	parameter("NEW_ID", 0, 1, "CUSTOMER_ID")

	// Trigger type 1 decodes to "BEFORE INSERT", 17 to "BEFORE INSERT OR
	// UPDATE", and 4096 sets a bit the decoder rejects, which is the
	// undecodable case. A NULL relation name is a database-level trigger.
	trigger := func(name string, relationName any, triggerType any, inactive int) {
		insert(`INSERT INTO "RDB$TRIGGERS" VALUES (?,?,?,?,?,?,?,?,?)`,
			name, relationName, 0, triggerType, "AS BEGIN END", nil, inactive, 0, 0)
	}
	trigger("CUSTOMER_BI", "CUSTOMER", 1, 0)
	trigger("CUSTOMER_MULTI", "CUSTOMER", 17, 1)
	trigger("CUSTOMER_ODD", "CUSTOMER", 4096, 0)
	trigger("DB_CONNECT", nil, nil, 0)

	insert(`INSERT INTO "RDB$GENERATORS" VALUES (?,?,?)`, "GEN_CUSTOMER_ID", 1, 0)

	// RDB$RETURN_ARGUMENT is 1: the return value IS input argument 1, which
	// must still appear exactly once in Arguments. Every argument has a NULL
	// RDB$CHARACTER_LENGTH, which is what every measured production row has.
	insert(`INSERT INTO "RDB$FUNCTIONS" VALUES (?,?,?,?,?,?,?)`,
		"F_LTRIM", 0, nil, interBaseFixed("ib_udf"), interBaseFixed("IB_LTRIM"), 1, 0)
	argument := func(position, mechanism, fieldLength, fieldType int, characterSetID any) {
		insert(`INSERT INTO "RDB$FUNCTION_ARGUMENTS" VALUES (?,?,?,?,?,?,?,?,?,?)`,
			"F_LTRIM", position, mechanism, fieldLength, 0, fieldType, 0, characterSetID, nil, nil)
	}
	argument(1, 1, 255, 40, 0) // CSTRING(255): renders from RDB$FIELD_LENGTH
	argument(2, 1, 10, 14, 0)  // CHAR: no character length, renders ""
	argument(3, 1, 4, 8, nil)  // INTEGER

	return db
}

var interBaseFixtureTables = []string{
	`CREATE TABLE "RDB$RELATIONS" ("RDB$RELATION_NAME" TEXT, "RDB$RELATION_ID" INTEGER, "RDB$VIEW_SOURCE" TEXT, "RDB$DESCRIPTION" TEXT, "RDB$SECURITY_CLASS" TEXT, "RDB$OWNER_NAME" TEXT, "RDB$DEFAULT_CLASS" TEXT, "RDB$DBKEY_LENGTH" INTEGER, "RDB$FORMAT" INTEGER, "RDB$EXTERNAL_FILE" TEXT, "RDB$FLAGS" INTEGER, "RDB$RELATION_TYPE" TEXT, "RDB$SYSTEM_FLAG" INTEGER, "RDB$VIEW_BLR" TEXT)`,
	`CREATE TABLE "RDB$RELATION_FIELDS" ("RDB$FIELD_NAME" TEXT, "RDB$RELATION_NAME" TEXT, "RDB$FIELD_SOURCE" TEXT, "RDB$FIELD_POSITION" INTEGER, "RDB$UPDATE_FLAG" INTEGER, "RDB$FIELD_ID" INTEGER, "RDB$DESCRIPTION" TEXT, "RDB$SYSTEM_FLAG" INTEGER, "RDB$SECURITY_CLASS" TEXT, "RDB$NULL_FLAG" INTEGER, "RDB$DEFAULT_SOURCE" TEXT, "RDB$COLLATION_ID" INTEGER, "RDB$BASE_FIELD" TEXT, "RDB$VIEW_CONTEXT" INTEGER)`,
	`CREATE TABLE "RDB$FIELDS" ("RDB$FIELD_NAME" TEXT, "RDB$VALIDATION_SOURCE" TEXT, "RDB$COMPUTED_SOURCE" TEXT, "RDB$DEFAULT_SOURCE" TEXT, "RDB$FIELD_LENGTH" INTEGER, "RDB$FIELD_SCALE" INTEGER, "RDB$FIELD_TYPE" INTEGER, "RDB$FIELD_SUB_TYPE" INTEGER, "RDB$DESCRIPTION" TEXT, "RDB$SYSTEM_FLAG" INTEGER, "RDB$SEGMENT_LENGTH" INTEGER, "RDB$EXTERNAL_LENGTH" INTEGER, "RDB$EXTERNAL_SCALE" INTEGER, "RDB$EXTERNAL_TYPE" INTEGER, "RDB$DIMENSIONS" INTEGER, "RDB$NULL_FLAG" INTEGER, "RDB$CHARACTER_LENGTH" INTEGER, "RDB$COLLATION_ID" INTEGER, "RDB$CHARACTER_SET_ID" INTEGER, "RDB$FIELD_PRECISION" INTEGER)`,
	`CREATE TABLE "RDB$CHARACTER_SETS" ("RDB$CHARACTER_SET_ID" INTEGER, "RDB$CHARACTER_SET_NAME" TEXT)`,
	`CREATE TABLE "RDB$COLLATIONS" ("RDB$CHARACTER_SET_ID" INTEGER, "RDB$COLLATION_ID" INTEGER, "RDB$COLLATION_NAME" TEXT)`,
	`CREATE TABLE "RDB$VIEW_RELATIONS" ("RDB$VIEW_NAME" TEXT, "RDB$VIEW_CONTEXT" INTEGER, "RDB$RELATION_NAME" TEXT)`,
	`CREATE TABLE "RDB$RELATION_CONSTRAINTS" ("RDB$CONSTRAINT_NAME" TEXT, "RDB$CONSTRAINT_TYPE" TEXT, "RDB$RELATION_NAME" TEXT, "RDB$DEFERRABLE" TEXT, "RDB$INITIALLY_DEFERRED" TEXT, "RDB$INDEX_NAME" TEXT)`,
	`CREATE TABLE "RDB$REF_CONSTRAINTS" ("RDB$CONSTRAINT_NAME" TEXT, "RDB$CONST_NAME_UQ" TEXT, "RDB$MATCH_OPTION" TEXT, "RDB$UPDATE_RULE" TEXT, "RDB$DELETE_RULE" TEXT)`,
	`CREATE TABLE "RDB$CHECK_CONSTRAINTS" ("RDB$CONSTRAINT_NAME" TEXT, "RDB$TRIGGER_NAME" TEXT)`,
	`CREATE TABLE "RDB$INDICES" ("RDB$INDEX_NAME" TEXT, "RDB$RELATION_NAME" TEXT, "RDB$INDEX_ID" INTEGER, "RDB$UNIQUE_FLAG" INTEGER, "RDB$DESCRIPTION" TEXT, "RDB$SEGMENT_COUNT" INTEGER, "RDB$INDEX_INACTIVE" INTEGER, "RDB$INDEX_TYPE" INTEGER, "RDB$FOREIGN_KEY" TEXT, "RDB$SYSTEM_FLAG" INTEGER, "RDB$EXPRESSION_SOURCE" TEXT, "RDB$STATISTICS" REAL)`,
	`CREATE TABLE "RDB$INDEX_SEGMENTS" ("RDB$INDEX_NAME" TEXT, "RDB$FIELD_NAME" TEXT, "RDB$FIELD_POSITION" INTEGER, "RDB$STATISTICS" REAL)`,
	`CREATE TABLE "RDB$PROCEDURES" ("RDB$PROCEDURE_NAME" TEXT, "RDB$PROCEDURE_ID" INTEGER, "RDB$PROCEDURE_INPUTS" INTEGER, "RDB$PROCEDURE_OUTPUTS" INTEGER, "RDB$DESCRIPTION" TEXT, "RDB$PROCEDURE_SOURCE" TEXT, "RDB$SECURITY_CLASS" TEXT, "RDB$OWNER_NAME" TEXT, "RDB$SYSTEM_FLAG" INTEGER)`,
	`CREATE TABLE "RDB$PROCEDURE_PARAMETERS" ("RDB$PARAMETER_NAME" TEXT, "RDB$PROCEDURE_NAME" TEXT, "RDB$PARAMETER_NUMBER" INTEGER, "RDB$PARAMETER_TYPE" INTEGER, "RDB$FIELD_SOURCE" TEXT, "RDB$DESCRIPTION" TEXT, "RDB$SYSTEM_FLAG" INTEGER)`,
	`CREATE TABLE "RDB$TRIGGERS" ("RDB$TRIGGER_NAME" TEXT, "RDB$RELATION_NAME" TEXT, "RDB$TRIGGER_SEQUENCE" INTEGER, "RDB$TRIGGER_TYPE" INTEGER, "RDB$TRIGGER_SOURCE" TEXT, "RDB$DESCRIPTION" TEXT, "RDB$TRIGGER_INACTIVE" INTEGER, "RDB$SYSTEM_FLAG" INTEGER, "RDB$FLAGS" INTEGER)`,
	`CREATE TABLE "RDB$GENERATORS" ("RDB$GENERATOR_NAME" TEXT, "RDB$GENERATOR_ID" INTEGER, "RDB$SYSTEM_FLAG" INTEGER)`,
	`CREATE TABLE "RDB$FUNCTIONS" ("RDB$FUNCTION_NAME" TEXT, "RDB$FUNCTION_TYPE" INTEGER, "RDB$DESCRIPTION" TEXT, "RDB$MODULE_NAME" TEXT, "RDB$ENTRYPOINT" TEXT, "RDB$RETURN_ARGUMENT" INTEGER, "RDB$SYSTEM_FLAG" INTEGER)`,
	`CREATE TABLE "RDB$FUNCTION_ARGUMENTS" ("RDB$FUNCTION_NAME" TEXT, "RDB$ARGUMENT_POSITION" INTEGER, "RDB$MECHANISM" INTEGER, "RDB$FIELD_LENGTH" INTEGER, "RDB$FIELD_SCALE" INTEGER, "RDB$FIELD_TYPE" INTEGER, "RDB$FIELD_SUB_TYPE" INTEGER, "RDB$CHARACTER_SET_ID" INTEGER, "RDB$FIELD_PRECISION" INTEGER, "RDB$CHARACTER_LENGTH" INTEGER)`,
}

func TestInterBaseSchemaFixtureFeedsTheCatalogReader(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	ctx := context.Background()
	catalog := schema.New(db)

	relations, err := catalog.Relations(ctx, "")
	if err != nil {
		t.Fatalf("Relations() error = %v", err)
	}
	wantColumns := map[string]int{"CHILD": 3, "CUSTOMER": 11, "CUSTOMER_VIEW": 1, "PARENT": 2}
	if len(relations) != len(wantColumns) {
		t.Fatalf("Relations() returned %d relations, want %d (the system relation must be filtered)", len(relations), len(wantColumns))
	}
	for _, relation := range relations {
		want, ok := wantColumns[relation.Name]
		if !ok {
			t.Fatalf("unexpected relation %q", relation.Name)
		}
		// Zero columns here means the fixture padded a join key: schema
		// re-queries columns with the trimmed relation name.
		if len(relation.Columns) != want {
			t.Errorf("relation %q has %d columns, want %d", relation.Name, len(relation.Columns), want)
		}
	}

	constraints, err := catalog.Constraints(ctx, "")
	if err != nil {
		t.Fatalf("Constraints() error = %v", err)
	}
	if len(constraints) != 3 {
		t.Fatalf("Constraints() returned %d constraints, want 3", len(constraints))
	}
	foreignKey := constraints[0]
	if foreignKey.Name != "FK_CHILD" {
		t.Fatalf("first constraint = %q, want FK_CHILD (catalog order is by name)", foreignKey.Name)
	}
	if got, want := strings.Join(foreignKey.Columns, ","), "CHILD_B,CHILD_A"; got != want {
		t.Errorf("FK columns = %q, want %q", got, want)
	}
	if got, want := strings.Join(foreignKey.ReferencedColumns, ","), "PARENT_B,PARENT_A"; got != want {
		t.Errorf("FK referenced columns = %q, want %q", got, want)
	}

	// The rewrite guard: Domains is the one method whose SQL SQLite rejects.
	domains, err := catalog.Domains(ctx, "")
	if err != nil {
		t.Fatalf("Domains() error = %v (the NOT STARTING WITH rewrite is missing or wrong)", err)
	}
	// Assert presence before absence. The loop below asserts nothing at all on
	// an empty result set, so without this the test that advertises itself as
	// the rewrite guard would pass against a fixture returning no domains.
	userDomains := map[string]bool{}
	for _, domain := range domains {
		userDomains[domain.Name] = true
	}
	for _, want := range []string{"EMAIL_ADDRESS", "CUSTOMER_CODE"} {
		if !userDomains[want] {
			t.Errorf("Domains() did not return the user domain %q; got %v", want, domains)
		}
	}
	for _, domain := range domains {
		if strings.HasPrefix(domain.Name, "RDB$") {
			t.Errorf("Domains() returned the system domain %q", domain.Name)
		}
	}

	procedures, err := catalog.Procedures(ctx, "")
	if err != nil {
		t.Fatalf("Procedures() error = %v", err)
	}
	if len(procedures) != 1 || len(procedures[0].InputParameters) != 2 || len(procedures[0].OutputParameters) != 1 {
		t.Fatalf("Procedures() = %d procedures with %d/%d parameters, want 1 with 2/1",
			len(procedures), len(procedures[0].InputParameters), len(procedures[0].OutputParameters))
	}

	functions, err := catalog.Functions(ctx, "")
	if err != nil {
		t.Fatalf("Functions() error = %v", err)
	}
	if len(functions) != 1 || len(functions[0].Arguments) != 3 {
		t.Fatalf("Functions() = %d functions with %d arguments, want 1 with 3", len(functions), len(functions[0].Arguments))
	}

	// Display-only padding must be trimmed by the reader, not by the fixture.
	if got := relations[0].OwnerName.String; got != "SYSDBA" {
		t.Errorf("relation owner = %q, want %q (catalog padding must be trimmed)", got, "SYSDBA")
	}
}

func TestInterBaseSchemaFixtureSupportsReadOnlyTransactions(t *testing.T) {
	// The snapshot capability begins a read-only transaction and keeps it open
	// while the rest of the pool stays usable. Pin that the fixture supports
	// both, so a snapshot failure later is a real defect and not the fixture.
	db := openInterBaseSchemaFixture(t)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("BeginTx(ReadOnly) error = %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	relations, err := schema.New(tx).Relations(ctx, "")
	if err != nil {
		t.Fatalf("Relations() through a transaction error = %v", err)
	}
	if len(relations) != 4 {
		t.Fatalf("Relations() through a transaction returned %d relations, want 4", len(relations))
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "RDB$RELATIONS"`).Scan(&count); err != nil {
		t.Fatalf("a second connection must stay usable while the snapshot transaction is open: %v", err)
	}
	if count != 5 {
		t.Fatalf("second connection saw %d relations, want 5 (shared-cache memory database)", count)
	}
}
```

- [ ] **Step 2: Run the fixture tests to verify they pass**

Run: `go test ./internal/database/ -count=1 -run 'InterBaseSchemaFixture' -v`

Expected: PASS. These two tests have no production code to write — they exist to make the fixture's two hazards visible and reviewable. If `Domains()` fails with a SQLite parse error near `STARTING`, the rewriting driver is not wired. If a relation reports 0 columns, a join key was padded.

- [ ] **Step 3: Run the whole suite**

Run: `go test ./...`
Expected: every package `ok`. The old fixture and its test in `interbase_test.go` are untouched and still pass; the two fixtures coexist for exactly one task.

- [ ] **Step 4: Commit**

```bash
git add internal/database/interbase_catalog_test.go
git commit -m "test(database): add an in-memory RDB\$ fixture the schema reader can read"
```

---

## Task 3: Dialect-aware type rendering over `schema.Domain`

The regression guard for the whole migration. The four retained helpers are **moved** from `interbase_common.go` into `interbase_catalog.go` and re-sourced to read `*schema.Domain`; their logic is unchanged. `interBaseTypeName` wraps them with the Dialect 1 override and the `SQLType()` delegation.

`interBaseColumnDescription` (`interbase_common.go:267-291`) is the only caller of the moved switch and it still holds an `interBaseColumnRow`. It gets a six-line adapter that builds a `schema.Domain` from the row, **which Task 4 deletes together with the whole function.** That adapter is what keeps `go test ./...` green at this task boundary; do not build anything else on it.

**Files:**
- Create: `internal/database/interbase_catalog.go`
- Modify: `internal/database/interbase_common.go:267-291` (the adapter), `:327-410` (delete the three moved functions)
- Test: `internal/database/interbase_catalog_test.go`

**Interfaces:**
- Consumes: `schema.Domain` and `(schema.Domain).SQLType() (string, error)` (`interbase-go/schema/ddl.go:188-194`); `InterBaseDBRepository.SQLDialect int` (plan 1, Task 9 — read but not written here).
- Produces:
  - `func interBaseTypeName(domain *schema.Domain, sqlDialect int) string` — the full rendering, including any ` CHARACTER SET ` / ` COLLATE ` suffix.
  - `func interBaseColumnTypeName(domain *schema.Domain, sqlDialect int) string` — the one-line form: `interBaseTypeName` trimmed at the first ` CHARACTER SET ` or ` COLLATE `.
  - `func interBaseColumnType(domain *schema.Domain) string` — the retained switch, now domain-sourced. Returns `""` for a nil domain or an invalid `FieldType`.
  - `func interBaseNumericType(base string, naturalPrecision int64, domain *schema.Domain) string`
  - `func interBaseCharacterLength(domain *schema.Domain) int64`
  - `interBaseNullability`, `interBaseDefault`, `interBaseEffectiveDefault` keep their current names, signatures and bodies in `interbase_common.go`.

- [ ] **Step 1: Verify the catalog dependency needs no cgo**

The spec places this file outside the `interbase` build tag because `schema` is pure Go. Verify rather than assume:

Run: `cd /home/a.simard@multidev.local/gits/sqls && CGO_ENABLED=0 go build interbase-go/schema`
Expected: no output, exit 0.

Then confirm the root driver package is the part that needs cgo, which is why nothing untagged may import it:

Run: `CGO_ENABLED=0 go build interbase-go`
Expected: FAIL with `undefined: nativeConnection` and similar. If `interbase-go/schema` also fails, **stop**: the whole untagged placement in §4.3 is invalid and must be reported, not worked around.

- [ ] **Step 2: Write the failing test**

Append to `internal/database/interbase_catalog_test.go`:

```go
func interBaseNullInt(value int64) sql.NullInt64 {
	return sql.NullInt64{Int64: value, Valid: true}
}

func interBaseNullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: true}
}

func TestInterBaseTypeRenderingByDialect(t *testing.T) {
	tests := []struct {
		name       string
		domain     *schema.Domain
		wantDialect1 string
		wantDialect3 string
	}{
		// The spec's core table.
		{
			name:         "field type 35 is the only dialect-dependent rule",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(35)},
			wantDialect1: "DATE",
			wantDialect3: "TIMESTAMP",
		},
		{
			name:         "field type 12 is DATE in both dialects",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(12)},
			wantDialect1: "DATE", wantDialect3: "DATE",
		},
		{
			name:         "field type 13 is TIME in both dialects",
			domain:       &schema.Domain{Name: "D", FieldType: interBaseNullInt(13)},
			wantDialect1: "TIME", wantDialect3: "TIME",
		},
		{
			name: "integer with a numeric subtype renders NUMERIC",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(8), FieldSubType: interBaseNullInt(1),
				FieldScale: interBaseNullInt(-2), FieldPrecision: interBaseNullInt(9)},
			wantDialect1: "NUMERIC(9, 2)", wantDialect3: "NUMERIC(9, 2)",
		},
		{
			name: "scaled DOUBLE without a numeric subtype is dialect 1 fixed point",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(27), FieldSubType: interBaseNullInt(0),
				FieldScale: interBaseNullInt(-2)},
			wantDialect1: "NUMERIC(15, 2)", wantDialect3: "NUMERIC(15, 2)",
		},
		{
			name: "DOUBLE with subtype 2 renders DECIMAL",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(27), FieldSubType: interBaseNullInt(2),
				FieldScale: interBaseNullInt(-4), FieldPrecision: interBaseNullInt(18)},
			wantDialect1: "DECIMAL(18, 4)", wantDialect3: "DECIMAL(18, 4)",
		},
		{
			name: "unscaled DOUBLE stays DOUBLE PRECISION",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(27), FieldSubType: interBaseNullInt(0),
				FieldScale: interBaseNullInt(0)},
			wantDialect1: "DOUBLE PRECISION", wantDialect3: "DOUBLE PRECISION",
		},
		{
			name: "CHAR reports its character length without the charset suffix",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(14), CharacterLength: interBaseNullInt(10),
				CharacterSetID: interBaseNullInt(4), CharacterSetName: interBaseNullString(interBaseFixed("UTF8"))},
			wantDialect1: "CHAR(10)", wantDialect3: "CHAR(10)",
		},
		{
			name:   "VARCHAR reports its character length",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(37), CharacterLength: interBaseNullInt(20)},
			wantDialect1: "VARCHAR(20)", wantDialect3: "VARCHAR(20)",
		},
		{
			name: "a text BLOB reports its subtype",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(261), FieldSubType: interBaseNullInt(1)},
			wantDialect1: "BLOB SUB_TYPE TEXT", wantDialect3: "BLOB SUB_TYPE TEXT",
		},
		{
			name:   "QUAD survives through the retained switch",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(9)},
			wantDialect1: "QUAD", wantDialect3: "QUAD",
		},
		{
			name:   "BLOB_ID survives through the retained switch",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(45)},
			wantDialect1: "BLOB_ID", wantDialect3: "BLOB_ID",
		},
		{
			name:   "CSTRING survives through the retained switch",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(40), CharacterLength: interBaseNullInt(32)},
			wantDialect1: "CSTRING(32)", wantDialect3: "CSTRING(32)",
		},
		{
			name:   "an unrecognized field type is named, not dropped",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(99)},
			wantDialect1: "TYPE(99)", wantDialect3: "TYPE(99)",
		},
		{name: "a nil domain renders nothing", domain: nil, wantDialect1: "", wantDialect3: ""},

		// Every remaining path on which Domain.SQLType() returns
		// ErrUnsupportedDDL and the retained switch must catch it. Spec §4.3.
		{
			name:   "an array falls back to its base type name",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(8), Dimensions: interBaseNullInt(1)},
			wantDialect1: "INTEGER", wantDialect3: "INTEGER",
		},
		{
			name: "a numeric subtype with no precision uses the natural precision",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(8), FieldSubType: interBaseNullInt(1),
				FieldScale: interBaseNullInt(-2)},
			wantDialect1: "NUMERIC(9, 2)", wantDialect3: "NUMERIC(9, 2)",
		},
		{
			name: "a numeric subtype with no scale renders scale zero",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(8), FieldSubType: interBaseNullInt(1),
				FieldPrecision: interBaseNullInt(9)},
			wantDialect1: "NUMERIC(9, 0)", wantDialect3: "NUMERIC(9, 0)",
		},
		{
			name: "a positive scale on subtype 0 renders the plain base name",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(8), FieldScale: interBaseNullInt(2)},
			wantDialect1: "INTEGER", wantDialect3: "INTEGER",
		},
		{
			name: "CHAR with a NULL character length falls back to the field length",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(14), FieldLength: interBaseNullInt(10)},
			wantDialect1: "CHAR(10)", wantDialect3: "CHAR(10)",
		},
		{
			name: "an unavailable charset name still renders a plain CHAR",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(14), CharacterLength: interBaseNullInt(5),
				CharacterSetID: interBaseNullInt(4)},
			wantDialect1: "CHAR(5)", wantDialect3: "CHAR(5)",
		},
		{
			name: "an unavailable collation name still renders a plain CHAR",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(14), CharacterLength: interBaseNullInt(5),
				CollationID: interBaseNullInt(2)},
			wantDialect1: "CHAR(5)", wantDialect3: "CHAR(5)",
		},
		{
			name:   "an invalid field type renders nothing rather than TYPE(0)",
			domain: &schema.Domain{Name: "D"},
			wantDialect1: "", wantDialect3: "",
		},

		// The remaining switch arms, so a later edit cannot drop one.
		{
			name:   "SMALLINT with a negative scale",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(7), FieldScale: interBaseNullInt(-1)},
			wantDialect1: "NUMERIC(4, 1)", wantDialect3: "NUMERIC(4, 1)",
		},
		{
			name:   "BIGINT", domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(16)},
			wantDialect1: "BIGINT", wantDialect3: "BIGINT",
		},
		{
			name:   "FLOAT", domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(10)},
			wantDialect1: "FLOAT", wantDialect3: "FLOAT",
		},
		{
			name:   "BOOLEAN", domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(17)},
			wantDialect1: "BOOLEAN", wantDialect3: "BOOLEAN",
		},

		// Two cases where SQLType() succeeds and disagrees with the old
		// switch. They are upgrades, not regressions, but they change text a
		// user sees, so they are pinned rather than discovered. See the
		// "documented divergences" note under this task.
		{
			name: "a numeric subtype with zero scale is NUMERIC, not DOUBLE PRECISION",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(27), FieldSubType: interBaseNullInt(1),
				FieldScale: interBaseNullInt(0), FieldPrecision: interBaseNullInt(15)},
			wantDialect1: "NUMERIC(15, 0)", wantDialect3: "NUMERIC(15, 0)",
		},
		{
			name:   "a binary BLOB names its subtype",
			domain: &schema.Domain{Name: "D", FieldType: interBaseNullInt(261), FieldSubType: interBaseNullInt(0)},
			wantDialect1: "BLOB SUB_TYPE BINARY", wantDialect3: "BLOB SUB_TYPE BINARY",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := interBaseColumnTypeName(test.domain, 1); got != test.wantDialect1 {
				t.Errorf("dialect 1 column type = %q, want %q", got, test.wantDialect1)
			}
			if got := interBaseColumnTypeName(test.domain, 3); got != test.wantDialect3 {
				t.Errorf("dialect 3 column type = %q, want %q", got, test.wantDialect3)
			}
			// Zero means dialect 3, matching the driver's normalizeDialect and
			// the zero value of InterBaseDBRepository.SQLDialect.
			if got := interBaseColumnTypeName(test.domain, 0); got != test.wantDialect3 {
				t.Errorf("dialect 0 column type = %q, want the dialect 3 rendering %q", got, test.wantDialect3)
			}
		})
	}
}

func TestInterBaseTypeNameKeepsCharsetAndCollationOutOfTheColumnForm(t *testing.T) {
	// The full rendering is what DomainDesc.Type and DDL carry; the trimmed
	// one is what the completion detail line carries. Both come from the same
	// renderer, so this pins the cut rather than a second code path.
	domain := &schema.Domain{
		Name:             "EMAIL_ADDRESS",
		FieldType:        interBaseNullInt(14),
		CharacterLength:  interBaseNullInt(10),
		CharacterSetID:   interBaseNullInt(4),
		CharacterSetName: interBaseNullString(interBaseFixed("UTF8")),
		CollationID:      interBaseNullInt(2),
		CollationName:    interBaseNullString(interBaseFixed("UNICODE")),
	}

	wantFull := `CHAR(10) CHARACTER SET "UTF8" COLLATE "UNICODE"`
	if got := interBaseTypeName(domain, 3); got != wantFull {
		t.Errorf("interBaseTypeName() = %q, want %q", got, wantFull)
	}
	if got := interBaseColumnTypeName(domain, 3); got != "CHAR(10)" {
		t.Errorf("interBaseColumnTypeName() = %q, want %q", got, "CHAR(10)")
	}

	charsetOnly := &schema.Domain{
		Name:             "CODE",
		FieldType:        interBaseNullInt(37),
		CharacterLength:  interBaseNullInt(20),
		CharacterSetID:   interBaseNullInt(4),
		CharacterSetName: interBaseNullString(interBaseFixed("UTF8")),
	}
	if got, want := interBaseTypeName(charsetOnly, 3), `VARCHAR(20) CHARACTER SET "UTF8"`; got != want {
		t.Errorf("interBaseTypeName() = %q, want %q", got, want)
	}
	if got := interBaseColumnTypeName(charsetOnly, 3); got != "VARCHAR(20)" {
		t.Errorf("interBaseColumnTypeName() = %q, want %q", got, "VARCHAR(20)")
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/database/ -count=1 -run 'TypeRenderingByDialect|CharsetAndCollationOutOfTheColumnForm' -v`

Expected: FAIL to build — `undefined: interBaseTypeName`, `undefined: interBaseColumnTypeName`.

**What a wrong implementation looks like here**, so the executor can tell a real defect from a typo. An implementation that delegates to `SQLType()` and renders `TYPE(n)` or `""` on error compiles and passes 15 of the 27 rows. It fails exactly these, each with a visible wrong value:

| row | naive result | required |
| --- | --- | --- |
| scaled DOUBLE without a numeric subtype | `""` or `TYPE(27)` | `NUMERIC(15, 2)` |
| QUAD | `TYPE(9)` | `QUAD` |
| BLOB_ID | `TYPE(45)` | `BLOB_ID` |
| CSTRING | `TYPE(40)` | `CSTRING(32)` |
| array | `TYPE(8)` | `INTEGER` |
| numeric subtype with no precision | `TYPE(8)` | `NUMERIC(9, 2)` |
| numeric subtype with no scale | `TYPE(8)` | `NUMERIC(9, 0)` |
| positive scale on subtype 0 | `TYPE(8)` | `INTEGER` |
| CHAR with a NULL character length | `TYPE(14)` | `CHAR(10)` |
| unavailable charset name | `TYPE(14)` | `CHAR(5)` |
| unavailable collation name | `TYPE(14)` | `CHAR(5)` |
| invalid field type | `TYPE(0)` | `""` |

An implementation that keeps only the switch and never calls `SQLType()` fails the opposite set: field type 35 under dialect 3 renders `DATE` instead of `TIMESTAMP`, and both BLOB rows render bare `BLOB`.

- [ ] **Step 4: Create `internal/database/interbase_catalog.go` with the renderer**

```go
package database

import (
	"fmt"
	"strings"

	"interbase-go/schema"
)

// interBaseTypeName renders the column/parameter type for sqlDialect, in the
// full form that includes any CHARACTER SET and COLLATE suffix.
//
// The rules, in order (spec §4.3):
//
//  1. A nil domain renders nothing: the catalog row resolved to no RDB$FIELDS
//     entry, so there is no type to report.
//  2. Under SQL Dialect 1, field type 35 is DATE. Dialect 1 has no separate
//     TIMESTAMP, and this is the only genuinely dialect-dependent rule.
//  3. Otherwise delegate to Domain.SQLType(), which is Dialect 3 correct and
//     renders charset, collation and validated precision/scale.
//  4. On error, fall back to the retained interBaseColumnType switch rather
//     than to TYPE(n). SQLType() returns ErrUnsupportedDDL on a dozen paths the
//     switch renders correctly today, so the fallback is what makes the
//     migration behavior preserving: SQLType() can upgrade a rendering and the
//     switch guarantees none regresses.
func interBaseTypeName(domain *schema.Domain, sqlDialect int) string {
	if domain == nil {
		return ""
	}
	if sqlDialect == 1 && domain.FieldType.Valid && domain.FieldType.Int64 == interBaseFieldTypeTimestamp {
		return "DATE"
	}
	if rendered, err := domain.SQLType(); err == nil {
		return rendered
	}
	return interBaseColumnType(domain)
}

// interBaseColumnTypeName renders the one-line form used for ColumnDesc.Type.
// SQLType appends CHARACTER SET and COLLATE clauses and the completion detail
// line must stay short, so the text is cut at the first of them. The cut is
// safe: no base type name contains either keyword.
func interBaseColumnTypeName(domain *schema.Domain, sqlDialect int) string {
	rendered := interBaseTypeName(domain, sqlDialect)
	if index := strings.Index(rendered, " CHARACTER SET "); index >= 0 {
		rendered = rendered[:index]
	}
	if index := strings.Index(rendered, " COLLATE "); index >= 0 {
		rendered = rendered[:index]
	}
	return rendered
}

// interBaseFieldTypeTimestamp is RDB$FIELD_TYPE 35, which Dialect 3 calls
// TIMESTAMP and Dialect 1 calls DATE.
const interBaseFieldTypeTimestamp = 35

// interBaseColumnType is the fixed-dialect type switch sqls has always used,
// re-sourced to read schema.Domain instead of a hand-scanned catalog row. Its
// logic is unchanged. It is retained as the exhaustive fallback for
// interBaseTypeName; TYPE(n) is reached exactly where it is reached today.
func interBaseColumnType(domain *schema.Domain) string {
	if domain == nil || !domain.FieldType.Valid {
		return ""
	}
	fieldType := domain.FieldType.Int64
	switch fieldType {
	case 7:
		return interBaseNumericType("SMALLINT", 4, domain)
	case 8:
		return interBaseNumericType("INTEGER", 9, domain)
	case 9:
		return "QUAD"
	case 10:
		return "FLOAT"
	case 12:
		return "DATE"
	case 13:
		return "TIME"
	case 14:
		return fmt.Sprintf("CHAR(%d)", interBaseCharacterLength(domain))
	case 16:
		return interBaseNumericType("BIGINT", 18, domain)
	case 17:
		return "BOOLEAN"
	case 27:
		// In Dialect 1, scaled NUMERIC/DECIMAL values can use DOUBLE
		// PRECISION as their underlying field type. A subtype without a
		// negative scale does not carry a fixed-point declaration, so keep
		// the underlying DOUBLE PRECISION rather than inventing (15, 0).
		if domain.FieldScale.Valid && domain.FieldScale.Int64 < 0 {
			return interBaseNumericType("DOUBLE PRECISION", 15, domain)
		}
		return "DOUBLE PRECISION"
	case interBaseFieldTypeTimestamp:
		// Dialect 1 uses field type 35 for DATE. The dialect 3 distinction is
		// made by interBaseTypeName, which reaches this switch only as a
		// fallback.
		return "DATE"
	case 37:
		return fmt.Sprintf("VARCHAR(%d)", interBaseCharacterLength(domain))
	case 40:
		return fmt.Sprintf("CSTRING(%d)", interBaseCharacterLength(domain))
	case 45:
		return "BLOB_ID"
	case 261:
		return "BLOB"
	default:
		return fmt.Sprintf("TYPE(%d)", fieldType)
	}
}

func interBaseCharacterLength(domain *schema.Domain) int64 {
	if domain == nil {
		return 0
	}
	if domain.CharacterLength.Valid {
		return domain.CharacterLength.Int64
	}
	if domain.FieldLength.Valid {
		return domain.FieldLength.Int64
	}
	return 0
}

func interBaseNumericType(base string, naturalPrecision int64, domain *schema.Domain) string {
	subtype := int64(0)
	if domain.FieldSubType.Valid {
		subtype = domain.FieldSubType.Int64
	}
	scale := int64(0)
	if domain.FieldScale.Valid {
		scale = domain.FieldScale.Int64
	}
	if subtype == 0 && scale >= 0 {
		return base
	}

	precision := naturalPrecision
	if domain.FieldPrecision.Valid && domain.FieldPrecision.Int64 > 0 {
		precision = domain.FieldPrecision.Int64
	}
	numericName := "NUMERIC"
	if subtype == 2 {
		numericName = "DECIMAL"
	}
	if scale < 0 {
		scale = -scale
	}
	return fmt.Sprintf("%s(%d, %d)", numericName, precision, scale)
}
```

- [ ] **Step 5: Delete the moved functions and adapt their one caller**

In `internal/database/interbase_common.go`, delete `interBaseColumnType` (`:327-373`), `interBaseCharacterLength` (`:375-383`) and `interBaseNumericType` (`:385-410`) — they now live in `interbase_catalog.go`. Keep `interBaseNullability`, `interBaseDefault` and `interBaseEffectiveDefault` exactly where they are.

Add `"interbase-go/schema"` to the import block, and in `interBaseColumnDescription` replace:

```go
	typ := interBaseColumnType(row)
```

with:

```go
	// Throwaway adapter: the retained switch now reads schema.Domain, and this
	// function is deleted whole in the next task together with
	// interBaseColumnRow. Do not build anything else on it.
	typ := interBaseColumnType(&schema.Domain{
		FieldType:       row.fieldType,
		FieldSubType:    row.fieldSubtype,
		FieldLength:     row.fieldLength,
		FieldScale:      row.fieldScale,
		FieldPrecision:  row.fieldPrecision,
		CharacterLength: row.characterLength,
	})
```

- [ ] **Step 6: Run the new tests to verify they pass**

Run: `go test ./internal/database/ -count=1 -run 'TypeRenderingByDialect|CharsetAndCollationOutOfTheColumnForm' -v`
Expected: PASS, every subtest.

- [ ] **Step 7: Run the whole suite**

Run: `go test ./...`

Expected: every package `ok`. In particular `TestInterBaseCatalogRepositoryReadsTablesViewsColumnsAndCompositeForeignKeys` (`interbase_test.go:13`) must still pass unchanged: the switch's inputs and logic are identical, so it renders the same strings, including `DATE` for the type-35 column and `DOUBLE PRECISION` for the subtype-1 DOUBLE. If either of those changed, the adapter is wired to `interBaseTypeName` instead of `interBaseColumnType`.

- [ ] **Step 8: Commit**

```bash
git add internal/database/interbase_catalog.go internal/database/interbase_catalog_test.go internal/database/interbase_common.go
git commit -m "feat(database): render InterBase types from schema.Domain with a dialect-aware wrapper"
```

### Documented divergences, recorded here because they change text a user sees

Both are cases where `Domain.SQLType()` **succeeds** and returns something other than the old switch, so §4.3's "the switch guarantees no case regresses" — which is about the error path — does not cover them. Neither is a regression; both are pinned by the table above.

| catalog metadata | today | after this task | why the new value is right |
| --- | --- | --- | --- |
| field type 27, subtype 1 or 2, scale 0, precision *p* | `DOUBLE PRECISION` | `NUMERIC(p, 0)` / `DECIMAL(p, 0)` | The subtype *is* the declaration: the column was declared `NUMERIC(p, 0)` and stored in a double. The old comment reasoned that a non-negative scale carries no fixed-point declaration, but the subtype does. |
| field type 261 with any subtype | `BLOB` | `BLOB SUB_TYPE TEXT`, `BLOB SUB_TYPE BINARY`, … | §4.3 names `BLOB SUB_TYPE TEXT` as an intended upgrade; the other subtypes come from the same branch and cannot be suppressed without special-casing. |

---

## Task 4: Read the `DBRepository` surface through `schema.Catalog`

The migration itself. The three hand-written queries and every helper that parsed their rows are deleted; `SchemaTables`, `DescribeDatabaseTable`, `DescribeDatabaseTableBySchema` and `DescribeForeignKeysBySchema` move to `interbase_catalog.go` and read through `schema`.

**Files:**
- Modify: `internal/database/interbase_catalog.go` (add the read path)
- Modify: `internal/database/interbase_common.go:128-291`, `:327` area, `:412-492` (delete), and the struct at `:93-95` (add the unexported snapshot field)
- Modify: `internal/database/interbase_test.go:13-122` (delete the migrated test), `:280-291` (delete the TRIM test), `:293-414` (delete the old fixture)
- Test: `internal/database/interbase_catalog_test.go`

**Interfaces:**
- Consumes: `interBaseColumnTypeName` (Task 3); `interBaseNullability(columnNullFlag, domainNullFlag sql.NullInt64) string` and `interBaseEffectiveDefault(columnSource, domainSource sql.NullString) sql.NullString` (`interbase_common.go:293-325`, unchanged); `InterBaseDBRepository.SQLDialect`/`DatabaseName` (plan 1).
- Produces:
  - `type interBaseCatalogSnapshot struct { catalog *schema.Catalog; relations []schema.Relation; constraints []schema.Constraint }` and the unexported field `InterBaseDBRepository.snapshot *interBaseCatalogSnapshot`. **Task 8 is what constructs one**; here it is always nil and every accessor reads through the `*sql.DB`.
  - `func (db *InterBaseDBRepository) catalogReader() (*schema.Catalog, error)`
  - `func (db *InterBaseDBRepository) relations(ctx context.Context) ([]schema.Relation, error)`
  - `func (db *InterBaseDBRepository) constraints(ctx context.Context) ([]schema.Constraint, error)`
  - `func interBasePrimaryKeyColumns(constraints []schema.Constraint) map[string]struct{}`
  - `func interBaseRelationColumnKey(relationName, columnName string) string`
  - The four `DBRepository` methods keep their exact signatures.

- [ ] **Step 1: Write the failing test**

Append to `internal/database/interbase_catalog_test.go`. This is the spec's behavior-preservation test: it carries the assertions from `interbase_test.go:13-122` verbatim except where a type is documented to change, and it is why the migration can be called behavior preserving rather than merely believed to be.

```go
func TestInterBaseRepositoryReadsTablesViewsColumnsAndCompositeForeignKeys(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	// Preserved verbatim from the pre-migration test: this repository has no
	// DatabaseName, and CurrentDatabase/Databases stay inert until plan 3
	// wires §4.7. This is the "no identity" regression guard.
	if got, err := repository.CurrentDatabase(ctx); err != nil || got != "" {
		t.Fatalf("CurrentDatabase() = (%q, %v), want (empty, nil)", got, err)
	}
	if got, err := repository.Databases(ctx); err != nil || !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("Databases() = (%#v, %v), want empty list", got, err)
	}
	if got, err := repository.CurrentSchema(ctx); err != nil || got != "" {
		t.Fatalf("CurrentSchema() = (%q, %v), want (empty, nil)", got, err)
	}
	if got, err := repository.Schemas(ctx); err != nil || !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("Schemas() = (%#v, %v), want synthetic empty schema", got, err)
	}

	// Views stay in SchemaTables: the extended view cache is additive
	// metadata, not a replacement, so views keep completing in FROM position.
	wantSchemaTables := map[string][]string{
		"": {"CHILD", "CUSTOMER", "CUSTOMER_VIEW", "PARENT"},
	}
	gotSchemaTables, err := repository.SchemaTables(ctx)
	if err != nil {
		t.Fatalf("SchemaTables() error = %v", err)
	}
	if !reflect.DeepEqual(gotSchemaTables, wantSchemaTables) {
		t.Fatalf("SchemaTables() = %#v, want %#v", gotSchemaTables, wantSchemaTables)
	}

	gotColumns, err := repository.DescribeDatabaseTable(ctx)
	if err != nil {
		t.Fatalf("DescribeDatabaseTable() error = %v", err)
	}
	wantColumns := []struct {
		table, name, typ, nullable, key, extra, defaultValue string
		defaultValid                                         bool
	}{
		{table: "CHILD", name: "CHILD_B", typ: "INTEGER", nullable: "YES", key: "NO"},
		{table: "CHILD", name: "CHILD_A", typ: "INTEGER", nullable: "YES", key: "NO"},
		// Kept with an empty type rather than dropped; see the named test below.
		{table: "CHILD", name: "ORPHAN", typ: "", nullable: "YES", key: "NO"},
		{table: "CUSTOMER", name: "ID", typ: "INTEGER", nullable: "NO", key: "YES"},
		{table: "CUSTOMER", name: "CODE", typ: "CHAR(10)", nullable: "YES", key: "NO"},
		// Documented type change: dialect 3 distinguishes TIMESTAMP from DATE.
		{table: "CUSTOMER", name: "CREATED", typ: "TIMESTAMP", nullable: "YES", key: "NO"},
		{table: "CUSTOMER", name: "AMOUNT", typ: "NUMERIC(9, 2)", nullable: "YES", key: "NO"},
		{table: "CUSTOMER", name: "LABEL", typ: "VARCHAR(20)", nullable: "YES", key: "NO", defaultValue: "'  seeded  '", defaultValid: true},
		{table: "CUSTOMER", name: "INHERITED", typ: "VARCHAR(20)", nullable: "NO", key: "NO", defaultValue: "'domain'", defaultValid: true},
		{table: "CUSTOMER", name: "OVERRIDE", typ: "VARCHAR(20)", nullable: "YES", key: "NO", defaultValue: "'column'", defaultValid: true},
		{table: "CUSTOMER", name: "DEFAULT_NULL", typ: "VARCHAR(20)", nullable: "YES", key: "NO", defaultValue: "NULL", defaultValid: true},
		{table: "CUSTOMER", name: "REQUIRED", typ: "INTEGER", nullable: "NO", key: "NO"},
		// Documented type change: the numeric subtype is the declaration.
		{table: "CUSTOMER", name: "DOUBLE_AMOUNT", typ: "NUMERIC(15, 0)", nullable: "YES", key: "NO"},
		// New: computed columns are common and hover should say so.
		{table: "CUSTOMER", name: "TOTAL", typ: "INTEGER", nullable: "YES", key: "NO", extra: "COMPUTED"},
		{table: "CUSTOMER_VIEW", name: "VIEW_ID", typ: "INTEGER", nullable: "YES", key: "NO"},
		{table: "PARENT", name: "PARENT_B", typ: "INTEGER", nullable: "YES", key: "YES"},
		{table: "PARENT", name: "PARENT_A", typ: "INTEGER", nullable: "YES", key: "YES"},
	}
	if len(gotColumns) != len(wantColumns) {
		t.Fatalf("DescribeDatabaseTable() returned %d columns, want %d", len(gotColumns), len(wantColumns))
	}
	for i, want := range wantColumns {
		got := gotColumns[i]
		if got.Schema != "" {
			t.Errorf("column %d schema = %q, want synthetic empty schema", i, got.Schema)
		}
		if got.Table != want.table || got.Name != want.name || got.Type != want.typ ||
			got.Null != want.nullable || got.Key != want.key || got.Extra != want.extra {
			t.Errorf("column %d = (%q, %q, %q, %q, %q, %q), want (%q, %q, %q, %q, %q, %q)",
				i, got.Table, got.Name, got.Type, got.Null, got.Key, got.Extra,
				want.table, want.name, want.typ, want.nullable, want.key, want.extra)
		}
		if got.Default.Valid != want.defaultValid || got.Default.String != want.defaultValue {
			t.Errorf("column %d default = %#v, want %#v", i, got.Default,
				sql.NullString{String: want.defaultValue, Valid: want.defaultValid})
		}
	}

	bySchema, err := repository.DescribeDatabaseTableBySchema(ctx, "ignored-schema")
	if err != nil {
		t.Fatalf("DescribeDatabaseTableBySchema() error = %v", err)
	}
	if len(bySchema) != len(gotColumns) {
		t.Fatalf("DescribeDatabaseTableBySchema() returned %d columns, want %d", len(bySchema), len(gotColumns))
	}
	for i := range gotColumns {
		if bySchema[i].Table != gotColumns[i].Table || bySchema[i].Name != gotColumns[i].Name {
			t.Errorf("schema column %d = %s.%s, want %s.%s", i,
				bySchema[i].Table, bySchema[i].Name, gotColumns[i].Table, gotColumns[i].Name)
		}
	}

	foreignKeys, err := repository.DescribeForeignKeysBySchema(ctx, "ignored-schema")
	if err != nil {
		t.Fatalf("DescribeForeignKeysBySchema() error = %v", err)
	}
	if len(foreignKeys) != 1 || len(*foreignKeys[0]) != 2 {
		t.Fatalf("DescribeForeignKeysBySchema() = %#v, want one two-column foreign key", foreignKeys)
	}
	wantForeignKeyTables := [][2]string{{"CHILD", "PARENT"}, {"CHILD", "PARENT"}}
	wantForeignKeyNames := [][2]string{{"CHILD_B", "PARENT_B"}, {"CHILD_A", "PARENT_A"}}
	for i, pair := range *foreignKeys[0] {
		if pair[0].Schema != "" || pair[1].Schema != "" ||
			pair[0].Table != wantForeignKeyTables[i][0] || pair[1].Table != wantForeignKeyTables[i][1] ||
			pair[0].Name != wantForeignKeyNames[i][0] || pair[1].Name != wantForeignKeyNames[i][1] {
			t.Errorf("foreign key pair %d = %#v, want %s.%s -> %s.%s", i, pair,
				wantForeignKeyTables[i][0], wantForeignKeyNames[i][0],
				wantForeignKeyTables[i][1], wantForeignKeyNames[i][1])
		}
	}
}

func TestInterBaseColumnTypesFollowTheRepositoryDialect(t *testing.T) {
	// The repository's SQLDialect must actually reach the renderer. Plan 1
	// populates it from the resolved connection variant; a zero value means
	// dialect 3, matching interbase-go's own normalizeDialect.
	db := openInterBaseSchemaFixture(t)
	ctx := context.Background()

	tests := []struct {
		sqlDialect int
		want       string
	}{
		{sqlDialect: 0, want: "TIMESTAMP"},
		{sqlDialect: 1, want: "DATE"},
		{sqlDialect: 3, want: "TIMESTAMP"},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("dialect %d", test.sqlDialect), func(t *testing.T) {
			repository := &InterBaseDBRepository{Conn: db, SQLDialect: test.sqlDialect}
			columns, err := repository.DescribeDatabaseTable(ctx)
			if err != nil {
				t.Fatalf("DescribeDatabaseTable() error = %v", err)
			}
			for _, column := range columns {
				if column.Table == "CUSTOMER" && column.Name == "CREATED" {
					if column.Type != test.want {
						t.Fatalf("CUSTOMER.CREATED type = %q, want %q", column.Type, test.want)
					}
					return
				}
			}
			t.Fatal("CUSTOMER.CREATED was not returned")
		})
	}
}

func TestInterBaseColumnWithoutDomainRowIsRetained(t *testing.T) {
	// A deliberate, asserted behavior change. The old inner JOIN RDB$FIELDS
	// (interbase_common.go:195-196) silently dropped a column whose
	// RDB$FIELD_SOURCE had no RDB$FIELDS row; schema LEFT JOINs and yields
	// Domain == nil. Showing a column with an unknown type beats hiding a
	// column that exists.
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}

	columns, err := repository.DescribeDatabaseTable(context.Background())
	if err != nil {
		t.Fatalf("DescribeDatabaseTable() error = %v", err)
	}

	var orphan *ColumnDesc
	for _, column := range columns {
		if column.Table == "CHILD" && column.Name == "ORPHAN" {
			orphan = column
		}
	}
	if orphan == nil {
		t.Fatal("a column whose field source has no RDB$FIELDS row was dropped; it must be kept with an empty type")
	}
	if orphan.Type != "" {
		t.Errorf("orphan column type = %q, want an empty type", orphan.Type)
	}
	if orphan.Null != "YES" || orphan.Key != "NO" || orphan.Default.Valid {
		t.Errorf("orphan column = %#v, want nullable, non-key, no default", orphan)
	}
}
```

Add `"fmt"` and `"reflect"` to the test file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/database/ -count=1 -run 'RepositoryReadsTablesViews|ColumnTypesFollowTheRepositoryDialect|ColumnWithoutDomainRowIsRetained' -v`

Expected: FAIL. `TestInterBaseRepositoryReadsTablesViewsColumnsAndCompositeForeignKeys` fails on the column list: the pre-migration reader drops `CHILD.ORPHAN` (inner join), returns `DATE` for `CUSTOMER.CREATED` (fixed dialect), `DOUBLE PRECISION` for `DOUBLE_AMOUNT`, and an empty `Extra` for `TOTAL`, so the count assertion trips first with 16 columns instead of 17.

- [ ] **Step 3: Write the read path**

Append to `internal/database/interbase_catalog.go`:

```go
// interBaseCatalogSnapshot holds one consistent catalog read. A repository
// carrying one serves every cache-build read from it instead of issuing fresh
// queries; see CatalogSnapshot.
type interBaseCatalogSnapshot struct {
	catalog     *schema.Catalog
	relations   []schema.Relation
	constraints []schema.Constraint
}

// catalogReader returns the catalog to read through: the snapshot's
// transaction-bound one when this repository is a snapshot, a fresh one over
// the pooled *sql.DB otherwise.
func (db *InterBaseDBRepository) catalogReader() (*schema.Catalog, error) {
	if db == nil {
		return nil, errors.New("interbase: database connection is nil")
	}
	if db.snapshot != nil {
		return db.snapshot.catalog, nil
	}
	if db.Conn == nil {
		return nil, errors.New("interbase: database connection is nil")
	}
	return schema.New(db.Conn), nil
}

// relations returns every user table and view with its ordered columns.
// Relations issues one column query per relation, so a snapshot reads it once
// per cache build rather than once per repository method.
func (db *InterBaseDBRepository) relations(ctx context.Context) ([]schema.Relation, error) {
	if db != nil && db.snapshot != nil {
		return db.snapshot.relations, nil
	}
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	return catalog.Relations(ctx, "")
}

// constraints returns every user-relation constraint with its resolved
// columns. Constraints loads the enforcing and referenced index per
// constraint, so the same snapshot rule applies.
func (db *InterBaseDBRepository) constraints(ctx context.Context) ([]schema.Constraint, error) {
	if db != nil && db.snapshot != nil {
		return db.snapshot.constraints, nil
	}
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	return catalog.Constraints(ctx, "")
}

func (db *InterBaseDBRepository) SchemaTables(ctx context.Context) (map[string][]string, error) {
	relations, err := db.relations(ctx)
	if err != nil {
		return nil, err
	}
	// Tables and views together, exactly as interBaseRelationsQuery returned
	// them: the extended view cache is additive metadata, not a replacement.
	names := make([]string, 0, len(relations))
	for _, relation := range relations {
		names = append(names, relation.Name)
	}
	return map[string][]string{"": names}, nil
}

func (db *InterBaseDBRepository) DescribeDatabaseTable(ctx context.Context) ([]*ColumnDesc, error) {
	return db.describeColumns(ctx)
}

func (db *InterBaseDBRepository) DescribeDatabaseTableBySchema(ctx context.Context, _ string) ([]*ColumnDesc, error) {
	return db.describeColumns(ctx)
}

func (db *InterBaseDBRepository) describeColumns(ctx context.Context) ([]*ColumnDesc, error) {
	relations, err := db.relations(ctx)
	if err != nil {
		return nil, err
	}
	constraints, err := db.constraints(ctx)
	if err != nil {
		return nil, err
	}
	primaryKeys := interBasePrimaryKeyColumns(constraints)

	result := make([]*ColumnDesc, 0)
	for _, relation := range relations {
		for _, column := range relation.Columns {
			result = append(result, db.columnDescription(relation.Name, column, primaryKeys))
		}
	}
	return result, nil
}

// columnDescription maps one catalog column onto the shared descriptor. A
// column whose field source resolved to no RDB$FIELDS row keeps its place with
// an empty type rather than disappearing.
func (db *InterBaseDBRepository) columnDescription(relationName string, column schema.Column, primaryKeys map[string]struct{}) *ColumnDesc {
	key := "NO"
	if _, ok := primaryKeys[interBaseRelationColumnKey(relationName, column.Name)]; ok {
		key = "YES"
	}

	var domainNullFlag sql.NullInt64
	var domainDefault sql.NullString
	if column.Domain != nil {
		domainNullFlag = column.Domain.NullFlag
		domainDefault = column.Domain.DefaultSource
	}

	extra := ""
	if interBaseIsComputed(column) {
		extra = "COMPUTED"
	}

	return &ColumnDesc{
		ColumnBase: ColumnBase{
			Schema: "",
			Table:  relationName,
			Name:   column.Name,
		},
		Type:    interBaseColumnTypeName(column.Domain, db.SQLDialect),
		Null:    interBaseNullability(column.NullFlag, domainNullFlag),
		Key:     key,
		Default: interBaseEffectiveDefault(column.DefaultSource, domainDefault),
		Extra:   extra,
	}
}

// interBaseIsComputed reports whether a column carries a COMPUTED BY source.
// InterBase stores it on the column's RDB$FIELDS row, which schema surfaces on
// both the column and its domain depending on the projection, so both are
// consulted.
func interBaseIsComputed(column schema.Column) bool {
	if column.ComputedSource.Valid && strings.TrimSpace(column.ComputedSource.String) != "" {
		return true
	}
	if column.Domain != nil && column.Domain.ComputedSource.Valid &&
		strings.TrimSpace(column.Domain.ComputedSource.String) != "" {
		return true
	}
	return false
}

// interBasePrimaryKeyColumns indexes every primary-key column segment so a
// column's key flag is one map lookup rather than a per-column subquery.
func interBasePrimaryKeyColumns(constraints []schema.Constraint) map[string]struct{} {
	primaryKeys := make(map[string]struct{})
	for _, constraint := range constraints {
		if !strings.EqualFold(strings.TrimSpace(constraint.ConstraintType), string(schema.ConstraintPrimaryKey)) {
			continue
		}
		for _, column := range constraint.Columns {
			primaryKeys[interBaseRelationColumnKey(constraint.RelationName, column)] = struct{}{}
		}
	}
	return primaryKeys
}

func interBaseRelationColumnKey(relationName, columnName string) string {
	return strings.ToUpper(strings.TrimSpace(relationName)) + "\t" + strings.ToUpper(strings.TrimSpace(columnName))
}

func (db *InterBaseDBRepository) DescribeForeignKeysBySchema(ctx context.Context, _ string) ([]*ForeignKey, error) {
	constraints, err := db.constraints(ctx)
	if err != nil {
		return nil, err
	}

	foreignKeys := make([]*ForeignKey, 0)
	for _, constraint := range constraints {
		if !strings.EqualFold(strings.TrimSpace(constraint.ConstraintType), string(schema.ConstraintForeignKey)) {
			continue
		}
		// A constraint whose enforcing or referenced index could not be
		// resolved carries no column pairing; skipping it drops one foreign
		// key rather than returning half of one.
		if len(constraint.Columns) == 0 || len(constraint.Columns) != len(constraint.ReferencedColumns) {
			continue
		}
		foreignKey := new(ForeignKey)
		for i, column := range constraint.Columns {
			left := &ColumnBase{Schema: "", Table: constraint.RelationName, Name: column}
			right := &ColumnBase{Schema: "", Table: constraint.ReferencedRelationName, Name: constraint.ReferencedColumns[i]}
			*foreignKey = append(*foreignKey, [2]*ColumnBase{left, right})
		}
		foreignKeys = append(foreignKeys, foreignKey)
	}
	return foreignKeys, nil
}
```

Add `"context"`, `"database/sql"` and `"errors"` to `interbase_catalog.go`'s imports.

- [ ] **Step 4: Delete the hand-written catalog access**

In `internal/database/interbase_common.go`, delete:

- `SchemaTables` and `relationNames` (`:128-160`)
- `interBaseRelationsQuery` (`:162-167`), `interBaseColumnsQuery` (`:169-199`), `interBaseForeignKeysQuery` (`:412-437`)
- `interBaseColumnRow` (`:201-215`)
- `DescribeDatabaseTable`, `DescribeDatabaseTableBySchema`, `describeColumns` (`:217-265`)
- `interBaseColumnDescription` (`:267-291`), which takes the throwaway adapter from Task 3 with it
- `DescribeForeignKeysBySchema` and `parseInterBaseForeignKeys` (`:439-492`)

Then remove `"interbase-go/schema"` and any other now-unused import from that file. What must remain: `interBaseDefaultPort`, `init`, `interBaseAttachment`, `interBaseCharset`, `InterBaseDBRepository`, `var _ DBRepository = (*InterBaseDBRepository)(nil)`, both constructors, `Driver`, `CurrentDatabase`, `Databases`, `CurrentSchema`, `Schemas`, `interBaseNullability`, `interBaseDefault`, `interBaseEffectiveDefault`, `Exec`, `Query`.

Add the snapshot field to the struct, which is plan 1's shape plus one unexported field:

```go
type InterBaseDBRepository struct {
	Conn *sql.DB
	// SQLDialect is 1 or 3; zero is treated as 3, matching the driver default.
	SQLDialect int
	// DatabaseName is the attachment string; empty when unknown.
	DatabaseName string

	// snapshot is set only on a repository returned by CatalogSnapshot. It
	// binds this repository to one read-only transaction that has already read
	// relations and constraints. The source repository is never mutated, so a
	// cache build on the worker goroutine cannot race a ReCache on a handler
	// goroutine.
	snapshot *interBaseCatalogSnapshot
}
```

- [ ] **Step 5: Delete the superseded tests**

In `internal/database/interbase_test.go`, delete:

- `TestInterBaseCatalogRepositoryReadsTablesViewsColumnsAndCompositeForeignKeys` (`:13-122`) — replaced by `TestInterBaseRepositoryReadsTablesViewsColumnsAndCompositeForeignKeys` in `interbase_catalog_test.go`
- `TestInterBaseCatalogQueriesAvoidUnsupportedTrimFunction` (`:280-291`) — it asserts on the three query constants this task deletes, and `schema` owns its own SQL now
- `openInterBaseCatalogFixture` (`:293-414`)

**Keep `interBaseFixed` (`:416-418`)** — the new fixture uses it for the display-only padded columns. Keep every other test in the file untouched.

- [ ] **Step 6: Run the database tests to verify they pass**

Run: `go test ./internal/database/ -count=1 -v`

Expected: PASS. If `TestInterBaseRepositoryReadsTablesViewsColumnsAndCompositeForeignKeys` reports 4 relations with 0 columns each, the fixture padded a join key — re-read the File Structure note on CHAR padding before touching the production code.

- [ ] **Step 7: Run the whole suite**

Run: `go test ./...`
Expected: every package `ok`.

- [ ] **Step 8: Commit**

```bash
git add internal/database/interbase_catalog.go internal/database/interbase_catalog_test.go internal/database/interbase_common.go internal/database/interbase_test.go
git commit -m "refactor(database): read the InterBase catalog through schema.Catalog"
```

---

## Task 5: `CatalogRepository` — views, generators, domains, indexes

Four of the seven methods. They share one shape — list from the catalog, map to a descriptor — and none of them depends on the unimplemented driver accessors.

**Files:**
- Modify: `internal/database/interbase_catalog.go`
- Test: `internal/database/interbase_catalog_test.go`

**Interfaces:**
- Consumes: `ViewDesc`, `GeneratorDesc`, `DomainDesc`, `IndexDesc` (Task 1); `interBaseTypeName`, `interBaseColumnTypeName` (Task 3); `catalogReader`, `columnDescription` (Task 4).
- Produces:
  - `func (db *InterBaseDBRepository) DescribeViews(ctx context.Context) ([]*ViewDesc, error)`
  - `func (db *InterBaseDBRepository) DescribeGenerators(ctx context.Context) ([]*GeneratorDesc, error)`
  - `func (db *InterBaseDBRepository) DescribeDomains(ctx context.Context) ([]*DomainDesc, error)`
  - `func (db *InterBaseDBRepository) DescribeIndexes(ctx context.Context) ([]*IndexDesc, error)`
  - `func interBaseFlagIsSet(flag sql.NullInt64) sql.NullBool`
  - `func interBaseFlagIsClear(flag sql.NullInt64) sql.NullBool`

- [ ] **Step 1: Write the failing test**

Append to `internal/database/interbase_catalog_test.go`:

```go
func TestInterBaseDescribesViewsGeneratorsDomainsAndIndexes(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	views, err := repository.DescribeViews(ctx)
	if err != nil {
		t.Fatalf("DescribeViews() error = %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("DescribeViews() returned %d views, want 1", len(views))
	}
	view := views[0]
	if view.Schema != "" || view.Name != "CUSTOMER_VIEW" {
		t.Errorf("view identity = (%q, %q), want (\"\", \"CUSTOMER_VIEW\")", view.Schema, view.Name)
	}
	if got, want := view.OwnerName.String, "SYSDBA"; !view.OwnerName.Valid || got != want {
		t.Errorf("view owner = %#v, want %q", view.OwnerName, want)
	}
	if got, want := view.ViewSource.String, "SELECT ID FROM CUSTOMER"; !view.ViewSource.Valid || got != want {
		t.Errorf("view source = %#v, want %q", view.ViewSource, want)
	}
	if len(view.Columns) != 1 || view.Columns[0].Name != "VIEW_ID" || view.Columns[0].Type != "INTEGER" {
		t.Fatalf("view columns = %#v, want one INTEGER VIEW_ID", view.Columns)
	}
	if view.Columns[0].Table != "CUSTOMER_VIEW" {
		t.Errorf("view column table = %q, want %q", view.Columns[0].Table, "CUSTOMER_VIEW")
	}

	generators, err := repository.DescribeGenerators(ctx)
	if err != nil {
		t.Fatalf("DescribeGenerators() error = %v", err)
	}
	if len(generators) != 1 || generators[0].Name != "GEN_CUSTOMER_ID" {
		t.Fatalf("DescribeGenerators() = %#v, want one GEN_CUSTOMER_ID", generators)
	}
	if !generators[0].ID.Valid || generators[0].ID.Int64 != 1 {
		t.Errorf("generator id = %#v, want 1", generators[0].ID)
	}

	domains, err := repository.DescribeDomains(ctx)
	if err != nil {
		t.Fatalf("DescribeDomains() error = %v", err)
	}
	byName := make(map[string]*DomainDesc, len(domains))
	for _, domain := range domains {
		if strings.HasPrefix(domain.Name, "RDB$") {
			t.Errorf("DescribeDomains() returned the system domain %q", domain.Name)
		}
		byName[domain.Name] = domain
	}

	email, ok := byName["EMAIL_ADDRESS"]
	if !ok {
		t.Fatalf("DescribeDomains() did not return EMAIL_ADDRESS: %v", byName)
	}
	if email.Type != "VARCHAR(100)" {
		t.Errorf("EMAIL_ADDRESS type = %q, want %q", email.Type, "VARCHAR(100)")
	}
	if !email.Nullable.Valid || email.Nullable.Bool {
		t.Errorf("EMAIL_ADDRESS nullable = %#v, want a valid false", email.Nullable)
	}
	if got, want := email.DefaultSource.String, " DEFAULT 'a@b' "; email.DefaultSource.String != want {
		t.Errorf("EMAIL_ADDRESS default = %q, want the verbatim catalog text %q", got, want)
	}
	if got, want := email.ValidationSource.String, "CHECK (VALUE LIKE '%@%')"; got != want {
		t.Errorf("EMAIL_ADDRESS validation = %q, want %q", got, want)
	}

	// DomainDesc.Type keeps the charset and collation suffix that
	// ColumnDesc.Type drops, and the charset/collation are separate fields.
	code, ok := byName["CUSTOMER_CODE"]
	if !ok {
		t.Fatalf("DescribeDomains() did not return CUSTOMER_CODE")
	}
	if got, want := code.Type, `CHAR(10) CHARACTER SET "UTF8" COLLATE "UNICODE"`; got != want {
		t.Errorf("CUSTOMER_CODE type = %q, want the full rendering %q", got, want)
	}
	if got, want := code.CharacterSetName.String, "UTF8"; !code.CharacterSetName.Valid || got != want {
		t.Errorf("CUSTOMER_CODE charset = %#v, want %q", code.CharacterSetName, want)
	}
	if got, want := code.CollationName.String, "UNICODE"; !code.CollationName.Valid || got != want {
		t.Errorf("CUSTOMER_CODE collation = %#v, want %q", code.CollationName, want)
	}
	if byName["CUSTOMER_LABEL"].Nullable.Valid {
		t.Errorf("CUSTOMER_LABEL nullable = %#v, want an invalid NullBool when RDB$NULL_FLAG is NULL",
			byName["CUSTOMER_LABEL"].Nullable)
	}

	indexes, err := repository.DescribeIndexes(ctx)
	if err != nil {
		t.Fatalf("DescribeIndexes() error = %v", err)
	}
	indexByName := make(map[string]*IndexDesc, len(indexes))
	for _, index := range indexes {
		indexByName[index.Name] = index
	}
	if len(indexes) != 4 {
		t.Fatalf("DescribeIndexes() returned %d indexes, want 4: %v", len(indexes), indexByName)
	}

	primaryKey := indexByName["IDX_PARENT_PK"]
	if primaryKey == nil {
		t.Fatal("DescribeIndexes() did not return IDX_PARENT_PK")
	}
	if primaryKey.RelationName != "PARENT" {
		t.Errorf("IDX_PARENT_PK relation = %q, want %q", primaryKey.RelationName, "PARENT")
	}
	if got, want := strings.Join(primaryKey.Columns, ","), "PARENT_B,PARENT_A"; got != want {
		t.Errorf("IDX_PARENT_PK segments = %q, want them ordered %q", got, want)
	}
	if !primaryKey.Unique.Valid || !primaryKey.Unique.Bool {
		t.Errorf("IDX_PARENT_PK unique = %#v, want a valid true", primaryKey.Unique)
	}
	if !primaryKey.Active.Valid || !primaryKey.Active.Bool {
		t.Errorf("IDX_PARENT_PK active = %#v, want a valid true", primaryKey.Active)
	}
	if got, want := primaryKey.ConstraintName.String, "PK_PARENT"; !primaryKey.ConstraintName.Valid || got != want {
		t.Errorf("IDX_PARENT_PK constraint = %#v, want %q", primaryKey.ConstraintName, want)
	}
	if primaryKey.Expression.Valid {
		t.Errorf("IDX_PARENT_PK expression = %#v, want invalid for a segment index", primaryKey.Expression)
	}

	// RDB$INDEX_INACTIVE is an INACTIVE flag; Active is its inverse, and a
	// standalone index has no owning constraint.
	inactive := indexByName["IDX_CUSTOMER_CODE"]
	if inactive == nil {
		t.Fatal("DescribeIndexes() did not return IDX_CUSTOMER_CODE")
	}
	if !inactive.Active.Valid || inactive.Active.Bool {
		t.Errorf("IDX_CUSTOMER_CODE active = %#v, want a valid false", inactive.Active)
	}
	if inactive.ConstraintName.Valid && inactive.ConstraintName.String != "" {
		t.Errorf("IDX_CUSTOMER_CODE constraint = %#v, want invalid for a standalone index", inactive.ConstraintName)
	}
	if !indexByName["IDX_CHILD_FK"].Unique.Valid || indexByName["IDX_CHILD_FK"].Unique.Bool {
		t.Errorf("IDX_CHILD_FK unique = %#v, want a valid false", indexByName["IDX_CHILD_FK"].Unique)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/database/ -count=1 -run 'DescribesViewsGeneratorsDomainsAndIndexes' -v`
Expected: FAIL to build — `repository.DescribeViews undefined`, and the same for the other three.

- [ ] **Step 3: Implement the four methods**

Append to `internal/database/interbase_catalog.go`:

```go
// interBaseFlagIsSet renders an InterBase flag as "the flag is set", leaving
// an unset flag unknown rather than false.
func interBaseFlagIsSet(flag sql.NullInt64) sql.NullBool {
	if !flag.Valid {
		return sql.NullBool{}
	}
	return sql.NullBool{Bool: flag.Int64 != 0, Valid: true}
}

// interBaseFlagIsClear renders an InterBase flag as "the flag is clear". The
// catalog stores INACTIVE flags, and the descriptors carry Active.
func interBaseFlagIsClear(flag sql.NullInt64) sql.NullBool {
	if !flag.Valid {
		return sql.NullBool{}
	}
	return sql.NullBool{Bool: flag.Int64 == 0, Valid: true}
}

func (db *InterBaseDBRepository) DescribeViews(ctx context.Context) ([]*ViewDesc, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	views, err := catalog.Views(ctx, "")
	if err != nil {
		return nil, err
	}

	// A view has no primary key, so no constraint read is needed: passing an
	// empty index keeps the column rendering identical to a table's.
	noPrimaryKeys := map[string]struct{}{}
	result := make([]*ViewDesc, 0, len(views))
	for _, view := range views {
		columns := make([]*ColumnDesc, 0, len(view.Columns))
		for _, column := range view.Columns {
			columns = append(columns, db.columnDescription(view.Name, column, noPrimaryKeys))
		}
		result = append(result, &ViewDesc{
			Schema:      "",
			Name:        view.Name,
			OwnerName:   view.OwnerName,
			ViewSource:  view.ViewSource,
			Description: view.Description,
			Columns:     columns,
		})
	}
	return result, nil
}

func (db *InterBaseDBRepository) DescribeGenerators(ctx context.Context) ([]*GeneratorDesc, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	generators, err := catalog.Generators(ctx, "")
	if err != nil {
		return nil, err
	}
	result := make([]*GeneratorDesc, 0, len(generators))
	for _, generator := range generators {
		result = append(result, &GeneratorDesc{Schema: "", Name: generator.Name, ID: generator.ID})
	}
	return result, nil
}

func (db *InterBaseDBRepository) DescribeDomains(ctx context.Context) ([]*DomainDesc, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	domains, err := catalog.Domains(ctx, "")
	if err != nil {
		return nil, err
	}
	result := make([]*DomainDesc, 0, len(domains))
	for index := range domains {
		domain := &domains[index]
		result = append(result, &DomainDesc{
			Schema: "",
			Name:   domain.Name,
			// The full rendering: a domain declaration is where the charset
			// and collation belong, and DDL reproduces it verbatim.
			Type:             interBaseTypeName(domain, db.SQLDialect),
			Nullable:         domain.Nullable,
			DefaultSource:    domain.DefaultSource,
			ValidationSource: domain.ValidationSource,
			CharacterSetName: domain.CharacterSetName,
			CollationName:    domain.CollationName,
			Description:      domain.Description,
		})
	}
	return result, nil
}

func (db *InterBaseDBRepository) DescribeIndexes(ctx context.Context) ([]*IndexDesc, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	indexes, err := catalog.Indexes(ctx, "")
	if err != nil {
		return nil, err
	}
	result := make([]*IndexDesc, 0, len(indexes))
	for _, index := range indexes {
		columns := make([]string, 0, len(index.Segments))
		for _, segment := range index.Segments {
			columns = append(columns, segment.FieldName)
		}
		result = append(result, &IndexDesc{
			Schema:         "",
			Name:           index.Name,
			RelationName:   index.RelationName,
			Columns:        columns,
			Expression:     index.Expression,
			Unique:         interBaseFlagIsSet(index.UniqueFlag),
			Active:         interBaseFlagIsClear(index.Inactive),
			ConstraintName: index.ConstraintName,
			Description:    index.Description,
		})
	}
	return result, nil
}
```

`schema.Domain.Nullable` is already the `sql.NullBool` form of `RDB$NULL_FLAG` (`schema/schema.go:901-905`: invalid when the flag is NULL, otherwise `flag == 0`), so `DomainDesc.Nullable` takes it unchanged rather than recomputing it.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/database/ -count=1 -run 'DescribesViewsGeneratorsDomainsAndIndexes' -v`
Expected: PASS.

- [ ] **Step 5: Run the whole suite**

Run: `go test ./...`
Expected: every package `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/database/interbase_catalog.go internal/database/interbase_catalog_test.go
git commit -m "feat(database): describe InterBase views, generators, domains and indexes"
```

---

## Task 6: `CatalogRepository` — procedures, triggers, functions

The remaining three methods, plus the user-versus-system domain predicate. `TriggerDesc.Event`, `FunctionArgumentDesc.Type` and `FunctionDesc.ReturnType` are populated as `""` here; **Task 12 fills them in and replaces the assertions that pin the placeholder.**

**Files:**
- Modify: `internal/database/interbase_catalog.go`
- Test: `internal/database/interbase_catalog_test.go`

**Interfaces:**
- Consumes: `ProcedureDesc`, `ProcedureParameterDesc`, `ParameterDirection`, `TriggerDesc`, `FunctionDesc`, `FunctionArgumentDesc` (Task 1); `interBaseTypeName` (Task 3); `catalogReader` (Task 4); `interBaseFlagIsClear` (Task 5).
- Produces:
  - `func (db *InterBaseDBRepository) DescribeProcedures(ctx context.Context) ([]*ProcedureDesc, error)`
  - `func (db *InterBaseDBRepository) DescribeTriggers(ctx context.Context) ([]*TriggerDesc, error)`
  - `func (db *InterBaseDBRepository) DescribeFunctions(ctx context.Context) ([]*FunctionDesc, error)`
  - `func interBaseUserDomainName(fieldSource sql.NullString, domain *schema.Domain) string`
  - `var _ CatalogRepository = (*InterBaseDBRepository)(nil)`

- [ ] **Step 1: Write the failing test**

Append to `internal/database/interbase_catalog_test.go`:

```go
func TestInterBaseDescribesProceduresTriggersAndFunctions(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	procedures, err := repository.DescribeProcedures(ctx)
	if err != nil {
		t.Fatalf("DescribeProcedures() error = %v", err)
	}
	if len(procedures) != 1 {
		t.Fatalf("DescribeProcedures() returned %d procedures, want 1", len(procedures))
	}
	procedure := procedures[0]
	if procedure.Schema != "" || procedure.Name != "ADD_CUSTOMER" {
		t.Errorf("procedure identity = (%q, %q), want (\"\", \"ADD_CUSTOMER\")", procedure.Schema, procedure.Name)
	}
	if got, want := procedure.OwnerName.String, "SYSDBA"; !procedure.OwnerName.Valid || got != want {
		t.Errorf("procedure owner = %#v, want %q", procedure.OwnerName, want)
	}
	if got, want := procedure.Source.String, "BEGIN NEW_ID = 1; END"; !procedure.Source.Valid || got != want {
		t.Errorf("procedure source = %#v, want the verbatim PSQL body %q", procedure.Source, want)
	}
	if len(procedure.InputParameters) != 2 || len(procedure.OutputParameters) != 1 {
		t.Fatalf("procedure has %d input and %d output parameters, want 2 and 1",
			len(procedure.InputParameters), len(procedure.OutputParameters))
	}

	wantInputs := []struct {
		name      string
		position  int
		typ       string
		domain    string
		nullable  sql.NullBool
	}{
		{name: "EMAIL", position: 0, typ: "VARCHAR(100)", domain: "EMAIL_ADDRESS", nullable: sql.NullBool{Bool: false, Valid: true}},
		{name: "CODE", position: 1, typ: "INTEGER", domain: "", nullable: sql.NullBool{}},
	}
	for i, want := range wantInputs {
		got := procedure.InputParameters[i]
		if got.Name != want.name || got.Position != want.position || got.Type != want.typ || got.Domain != want.domain {
			t.Errorf("input parameter %d = (%q, %d, %q, %q), want (%q, %d, %q, %q)",
				i, got.Name, got.Position, got.Type, got.Domain, want.name, want.position, want.typ, want.domain)
		}
		if got.Direction != ParameterInput {
			t.Errorf("input parameter %d direction = %q, want %q", i, got.Direction, ParameterInput)
		}
		if got.Nullable != want.nullable {
			t.Errorf("input parameter %d nullable = %#v, want %#v", i, got.Nullable, want.nullable)
		}
	}

	output := procedure.OutputParameters[0]
	if output.Name != "NEW_ID" || output.Direction != ParameterOutput || output.Type != "INTEGER" {
		t.Errorf("output parameter = (%q, %q, %q), want (\"NEW_ID\", %q, \"INTEGER\")",
			output.Name, output.Direction, output.Type, ParameterOutput)
	}
	if output.Domain != "CUSTOMER_ID" {
		t.Errorf("output parameter domain = %q, want %q", output.Domain, "CUSTOMER_ID")
	}

	triggers, err := repository.DescribeTriggers(ctx)
	if err != nil {
		t.Fatalf("DescribeTriggers() error = %v", err)
	}
	triggerByName := make(map[string]*TriggerDesc, len(triggers))
	for _, trigger := range triggers {
		triggerByName[trigger.Name] = trigger
	}
	if len(triggers) != 4 {
		t.Fatalf("DescribeTriggers() returned %d triggers, want 4: %v", len(triggers), triggerByName)
	}

	before := triggerByName["CUSTOMER_BI"]
	if before == nil {
		t.Fatal("DescribeTriggers() did not return CUSTOMER_BI")
	}
	if got, want := before.RelationName.String, "CUSTOMER"; !before.RelationName.Valid || got != want {
		t.Errorf("CUSTOMER_BI relation = %#v, want %q", before.RelationName, want)
	}
	if !before.Active.Valid || !before.Active.Bool {
		t.Errorf("CUSTOMER_BI active = %#v, want a valid true", before.Active)
	}
	if got, want := before.Source.String, "AS BEGIN END"; !before.Source.Valid || got != want {
		t.Errorf("CUSTOMER_BI source = %#v, want %q", before.Source, want)
	}
	if !before.Sequence.Valid || before.Sequence.Int64 != 0 {
		t.Errorf("CUSTOMER_BI sequence = %#v, want 0", before.Sequence)
	}

	// RDB$TRIGGER_INACTIVE is an INACTIVE flag, so Active is its inverse.
	if multi := triggerByName["CUSTOMER_MULTI"]; multi == nil || !multi.Active.Valid || multi.Active.Bool {
		t.Errorf("CUSTOMER_MULTI active = %#v, want a valid false", multi)
	}
	// A database-level trigger has no relation.
	if database := triggerByName["DB_CONNECT"]; database == nil || database.RelationName.Valid {
		t.Errorf("DB_CONNECT relation = %#v, want invalid for a database-level trigger", database)
	}

	functions, err := repository.DescribeFunctions(ctx)
	if err != nil {
		t.Fatalf("DescribeFunctions() error = %v", err)
	}
	if len(functions) != 1 {
		t.Fatalf("DescribeFunctions() returned %d functions, want 1", len(functions))
	}
	function := functions[0]
	if function.Name != "F_LTRIM" {
		t.Errorf("function name = %q, want %q", function.Name, "F_LTRIM")
	}
	if got, want := function.ModuleName.String, "ib_udf"; !function.ModuleName.Valid || got != want {
		t.Errorf("function module = %#v, want %q", function.ModuleName, want)
	}
	if got, want := function.EntryPoint.String, "IB_LTRIM"; !function.EntryPoint.Valid || got != want {
		t.Errorf("function entry point = %#v, want %q", function.EntryPoint, want)
	}
	if !function.ReturnPosition.Valid || function.ReturnPosition.Int64 != 1 {
		t.Errorf("function return position = %#v, want 1", function.ReturnPosition)
	}
	// The return argument IS input argument 1, and must still appear exactly
	// once in Arguments rather than being lifted out of the list.
	if len(function.Arguments) != 3 {
		t.Fatalf("function has %d arguments, want 3", len(function.Arguments))
	}
	positions := make([]int64, 0, len(function.Arguments))
	for _, argument := range function.Arguments {
		if !argument.Position.Valid {
			t.Fatalf("argument %q has no position", argument.Name)
		}
		positions = append(positions, argument.Position.Int64)
	}
	if !reflect.DeepEqual(positions, []int64{1, 2, 3}) {
		t.Errorf("argument positions = %v, want [1 2 3] in catalog order", positions)
	}
}

func TestInterBaseProcedureParameterDomainIsUserOnly(t *testing.T) {
	// The two-condition test from §4.4: a parameter declared with an inline
	// type gets a system-generated RDB$ domain, which must never be shown as a
	// domain reference. When Domain is "", Type carries the inline rendering.
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}

	procedures, err := repository.DescribeProcedures(context.Background())
	if err != nil {
		t.Fatalf("DescribeProcedures() error = %v", err)
	}
	parameters := procedures[0].InputParameters

	if got, want := parameters[0].Domain, "EMAIL_ADDRESS"; got != want {
		t.Errorf("a user domain must be reported: Domain = %q, want %q", got, want)
	}
	if got, want := parameters[1].Domain, ""; got != want {
		t.Errorf("an RDB$-prefixed system domain must not be reported: Domain = %q, want %q", got, want)
	}
	if got, want := parameters[1].Type, "INTEGER"; got != want {
		t.Errorf("an inline parameter must still render its type: Type = %q, want %q", got, want)
	}

	// The second condition, exercised directly because the fixture's system
	// domain is caught by the name rule first: a user-named domain flagged as
	// a system object is equally not a domain reference.
	systemFlagged := &schema.Domain{Name: "LEGACY_FLAG", SystemFlag: interBaseNullInt(1)}
	if got := interBaseUserDomainName(interBaseNullString(interBaseFixed("LEGACY_FLAG")), systemFlagged); got != "" {
		t.Errorf("a system-flagged domain must not be reported: got %q, want an empty string", got)
	}
	userFlagged := &schema.Domain{Name: "LEGACY_FLAG", SystemFlag: interBaseNullInt(0)}
	if got, want := interBaseUserDomainName(interBaseNullString(interBaseFixed("LEGACY_FLAG")), userFlagged), "LEGACY_FLAG"; got != want {
		t.Errorf("a user domain with SystemFlag 0 must be reported: got %q, want %q", got, want)
	}
	nullFlagged := &schema.Domain{Name: "LEGACY_FLAG"}
	if got, want := interBaseUserDomainName(interBaseNullString("LEGACY_FLAG"), nullFlagged), "LEGACY_FLAG"; got != want {
		t.Errorf("a user domain with a NULL SystemFlag must be reported: got %q, want %q", got, want)
	}
	if got := interBaseUserDomainName(interBaseNullString("rdb$99"), nil); got != "" {
		t.Errorf("the RDB$ prefix test must be case insensitive: got %q, want an empty string", got)
	}
	if got := interBaseUserDomainName(sql.NullString{}, nil); got != "" {
		t.Errorf("a NULL field source must report no domain: got %q", got)
	}
}

func TestInterBaseUndecodableFieldsAreEmptyUntilDriverAccessorsLand(t *testing.T) {
	// TriggerDesc.Event, FunctionArgumentDesc.Type and FunctionDesc.ReturnType
	// are populated by schema.Trigger.Event, schema.FunctionArgument.SQLType
	// and schema.Function.ReturnType, specified in the companion driver spec
	// docs/superpowers/specs/2026-09-19-schema-catalog-accessors-design.md and
	// not yet implemented. "" is the documented undecodable value, so every
	// consumer already handles it. The final task of this plan replaces this
	// test with the real assertions; until then this pins that the fields
	// exist, are empty, and that nothing else about the descriptor degrades.
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	triggers, err := repository.DescribeTriggers(ctx)
	if err != nil {
		t.Fatalf("DescribeTriggers() error = %v", err)
	}
	for _, trigger := range triggers {
		if trigger.Event != "" {
			t.Errorf("trigger %q event = %q, want \"\" until schema.Trigger.Event lands", trigger.Name, trigger.Event)
		}
		if trigger.Name == "" || !trigger.Source.Valid {
			t.Errorf("trigger %#v lost surrounding metadata", trigger)
		}
	}

	functions, err := repository.DescribeFunctions(ctx)
	if err != nil {
		t.Fatalf("DescribeFunctions() error = %v", err)
	}
	for _, function := range functions {
		if function.ReturnType != "" {
			t.Errorf("function %q return type = %q, want \"\" until schema.Function.ReturnType lands",
				function.Name, function.ReturnType)
		}
		for _, argument := range function.Arguments {
			if argument.Type != "" {
				t.Errorf("argument %q type = %q, want \"\" until schema.FunctionArgument.SQLType lands",
					argument.Name, argument.Type)
			}
			if argument.Name == "" || !argument.Position.Valid {
				t.Errorf("argument %#v lost surrounding metadata", argument)
			}
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/database/ -count=1 -run 'DescribesProceduresTriggersAndFunctions|ProcedureParameterDomainIsUserOnly|UndecodableFieldsAreEmpty' -v`
Expected: FAIL to build — `repository.DescribeProcedures undefined`, `undefined: interBaseUserDomainName`.

- [ ] **Step 3: Implement the three methods and the domain predicate**

Append to `internal/database/interbase_catalog.go`:

```go
var _ CatalogRepository = (*InterBaseDBRepository)(nil)

func (db *InterBaseDBRepository) DescribeProcedures(ctx context.Context) ([]*ProcedureDesc, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	procedures, err := catalog.Procedures(ctx, "")
	if err != nil {
		return nil, err
	}
	result := make([]*ProcedureDesc, 0, len(procedures))
	for _, procedure := range procedures {
		result = append(result, &ProcedureDesc{
			Schema:           "",
			Name:             procedure.Name,
			OwnerName:        procedure.OwnerName,
			Source:           procedure.Source,
			Description:      procedure.Description,
			InputParameters:  db.parameterDescriptions(procedure.InputParameters, ParameterInput),
			OutputParameters: db.parameterDescriptions(procedure.OutputParameters, ParameterOutput),
		})
	}
	return result, nil
}

// parameterDescriptions maps one direction's parameters, already ordered by
// RDB$PARAMETER_NUMBER by the catalog query. Type keeps the full rendering:
// a parameter is a declaration, and when Domain is empty the inline type is
// exactly what the user wrote, charset included.
func (db *InterBaseDBRepository) parameterDescriptions(parameters []schema.ProcedureParameter, direction ParameterDirection) []*ProcedureParameterDesc {
	result := make([]*ProcedureParameterDesc, 0, len(parameters))
	for index := range parameters {
		parameter := &parameters[index]
		position := index
		if parameter.Number.Valid {
			position = int(parameter.Number.Int64)
		}
		result = append(result, &ProcedureParameterDesc{
			Name:        parameter.Name,
			Position:    position,
			Direction:   direction,
			Type:        interBaseTypeName(parameter.Domain, db.SQLDialect),
			Domain:      interBaseUserDomainName(parameter.FieldSource, parameter.Domain),
			Nullable:    parameter.Nullable,
			Description: parameter.Description,
		})
	}
	return result
}

// interBaseUserDomainName implements the user-versus-system domain test from
// §4.4. A parameter declared with an inline type gets a system-generated RDB$
// domain that must not be shown as a domain reference, so the name is reported
// only when both conditions hold: it is non-empty and does not begin with
// "RDB$" (case insensitively, after right-trimming catalog padding), and the
// domain's SystemFlag is NULL or 0.
//
// The driver's own predicate, userDomainReference (schema/ddl.go:407-412), is
// unexported and the companion driver spec declined to export it, so this is a
// deliberate second copy of a two-condition rule rather than drift. If the
// driver ever exports it, delete this and call the exported form.
func interBaseUserDomainName(fieldSource sql.NullString, domain *schema.Domain) string {
	if !fieldSource.Valid {
		return ""
	}
	name := strings.TrimRight(fieldSource.String, " ")
	if name == "" {
		return ""
	}
	if len(name) >= len("RDB$") && strings.EqualFold(name[:len("RDB$")], "RDB$") {
		return ""
	}
	if domain != nil && domain.SystemFlag.Valid && domain.SystemFlag.Int64 != 0 {
		return ""
	}
	return name
}

func (db *InterBaseDBRepository) DescribeTriggers(ctx context.Context) ([]*TriggerDesc, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	triggers, err := catalog.Triggers(ctx, "")
	if err != nil {
		return nil, err
	}
	result := make([]*TriggerDesc, 0, len(triggers))
	for _, trigger := range triggers {
		result = append(result, &TriggerDesc{
			Schema:       "",
			Name:         trigger.Name,
			RelationName: trigger.RelationName,
			// Event is decoded by schema.Trigger.Event, which the companion
			// driver spec adds. "" is the documented undecodable value.
			Event:       "",
			Sequence:    trigger.Sequence,
			Active:      interBaseFlagIsClear(trigger.Inactive),
			Source:      trigger.Source,
			Description: trigger.Description,
		})
	}
	return result, nil
}

func (db *InterBaseDBRepository) DescribeFunctions(ctx context.Context) ([]*FunctionDesc, error) {
	catalog, err := db.catalogReader()
	if err != nil {
		return nil, err
	}
	functions, err := catalog.Functions(ctx, "")
	if err != nil {
		return nil, err
	}
	result := make([]*FunctionDesc, 0, len(functions))
	for _, function := range functions {
		arguments := make([]*FunctionArgumentDesc, 0, len(function.Arguments))
		for _, argument := range function.Arguments {
			arguments = append(arguments, &FunctionArgumentDesc{
				Name:     argument.Name,
				Position: argument.Position,
				// Rendered by schema.FunctionArgument.SQLType, which the
				// companion driver spec adds.
				Type: "",
			})
		}
		result = append(result, &FunctionDesc{
			Schema: "",
			Name:   function.Name,
			// Resolved by schema.Function.ReturnType, which the companion
			// driver spec adds. RDB$RETURN_ARGUMENT is a position, not an
			// index into Arguments, so sqls never indexes the slice with it.
			ReturnType:     "",
			ReturnPosition: function.ReturnArgument,
			Arguments:      arguments,
			ModuleName:     function.ModuleName,
			EntryPoint:     function.EntryPoint,
			Description:    function.Description,
		})
	}
	return result, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/database/ -count=1 -run 'DescribesProceduresTriggersAndFunctions|ProcedureParameterDomainIsUserOnly|UndecodableFieldsAreEmpty' -v`
Expected: PASS.

- [ ] **Step 5: Assert the capability is now satisfied**

Append to `internal/database/capability_test.go`:

```go
func TestInterBaseRepositoryImplementsCapabilities(t *testing.T) {
	var (
		_ CatalogRepository         = (*InterBaseDBRepository)(nil)
		_ CatalogSnapshotRepository = (*InterBaseDBRepository)(nil)
		_ DDLRepository             = (*InterBaseDBRepository)(nil)
	)

	repository := DBRepository(&InterBaseDBRepository{})
	if _, ok := repository.(CatalogRepository); !ok {
		t.Error("*InterBaseDBRepository must implement CatalogRepository at run time, not only at compile time")
	}
	if _, ok := repository.(DDLRepository); !ok {
		t.Error("*InterBaseDBRepository must implement DDLRepository")
	}
	if _, ok := repository.(CatalogSnapshotRepository); !ok {
		t.Error("*InterBaseDBRepository must implement CatalogSnapshotRepository")
	}
}
```

This will not compile until Tasks 7 and 8 land `ObjectDDL` and `CatalogSnapshot`. **Write it now but leave the `DDLRepository` and `CatalogSnapshotRepository` lines commented out with a `// Task 7` / `// Task 8` marker, and uncomment each in its own task's step.** Doing it this way means the assertion is authored once, next to the rest of the contract test, rather than three times.

- [ ] **Step 6: Run the whole suite**

Run: `go test ./...`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/database/interbase_catalog.go internal/database/interbase_catalog_test.go internal/database/capability_test.go
git commit -m "feat(database): describe InterBase procedures, triggers and external functions"
```

---

## Task 7: `ObjectDDL` and the structured unsupported-DDL error

`DDLRepository` over `GenerateDDL()`. The interesting half is the error mapping: a driver refusal must satisfy both `errors.Is(err, ErrUnsupportedDDL)` and `UnsupportedDDLDetail`, without `capability.go` importing `schema`.

**Files:**
- Create: `internal/database/interbase_ddl.go`
- Modify: `internal/database/capability_test.go` (uncomment the `DDLRepository` assertion)
- Test: `internal/database/interbase_catalog_test.go`

**Interfaces:**
- Consumes: `ObjectKind` and its constants, `ErrObjectNotFound`, `ErrUnsupportedDDL`, `unsupportedDDLDetailer`, `UnsupportedDDLDetail` (Task 1); `schema.DDLer`, `schema.UnsupportedDDLError`, `schema.ErrUnsupportedDDL` (`schema/ddl.go:15-49`).
- Produces:
  - `func (db *InterBaseDBRepository) ObjectDDL(ctx context.Context, kind ObjectKind, name string) (string, error)`
  - `type interBaseUnsupportedDDL struct{ detail *schema.UnsupportedDDLError }` with `Error`, `Unwrap` and `UnsupportedDDLDetail`
  - `func interBaseWrapDDLError(err error) error`

**Name matching.** `schema` matches catalog names exactly and does not case-fold (`Catalog.Table(ctx, "CUSTOMER   ")` returns nil, verified). `ObjectDDL` passes the caller's name through unchanged so a genuinely lower-case delimited identifier stays reachable. Callers resolve the object through `DBCache` first — which does normalise — and pass the descriptor's `Name`, which is the catalog's own spelling.

- [ ] **Step 1: Write the failing test**

Append to `internal/database/interbase_catalog_test.go`:

```go
func TestInterBaseObjectDDL(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	rendered := []struct {
		name     string
		kind     ObjectKind
		object   string
		contains []string
	}{
		{
			name: "a plain table", kind: ObjectKindTable, object: "PARENT",
			contains: []string{`CREATE TABLE "PARENT"`, `"PARENT_B"`, `CONSTRAINT "PK_PARENT" PRIMARY KEY`},
		},
		{
			name: "a view", kind: ObjectKindView, object: "CUSTOMER_VIEW",
			contains: []string{`CREATE VIEW "CUSTOMER_VIEW"`, "SELECT ID FROM CUSTOMER"},
		},
		{
			name: "a generator uses InterBase's own keyword", kind: ObjectKindGenerator, object: "GEN_CUSTOMER_ID",
			contains: []string{`CREATE GENERATOR "GEN_CUSTOMER_ID"`},
		},
		{
			name: "a domain", kind: ObjectKindDomain, object: "EMAIL_ADDRESS",
			contains: []string{`CREATE DOMAIN "EMAIL_ADDRESS" AS VARCHAR(100)`, "CHECK (VALUE LIKE '%@%')"},
		},
		{
			name: "a trigger", kind: ObjectKindTrigger, object: "CUSTOMER_BI",
			contains: []string{`CREATE TRIGGER "CUSTOMER_BI" FOR "CUSTOMER"`, "BEFORE INSERT"},
		},
		{
			name: "an index", kind: ObjectKindIndex, object: "IDX_PARENT_PK",
			contains: []string{`CREATE UNIQUE ASCENDING INDEX "IDX_PARENT_PK" ON "PARENT"`, `"PARENT_B", "PARENT_A"`},
		},
	}
	for _, test := range rendered {
		t.Run(test.name, func(t *testing.T) {
			ddl, err := repository.ObjectDDL(ctx, test.kind, test.object)
			if err != nil {
				t.Fatalf("ObjectDDL(%s, %q) error = %v", test.kind, test.object, err)
			}
			for _, want := range test.contains {
				if !strings.Contains(ddl, want) {
					t.Errorf("ObjectDDL(%s, %q) = %q, want it to contain %q", test.kind, test.object, ddl, want)
				}
			}
		})
	}

	unsupported := []struct {
		name        string
		kind        ObjectKind
		object      string
		wantObject  string
		wantFeature string
	}{
		{
			// Computed columns block table DDL, and they are common in the
			// target database, so this is the normal case rather than an edge.
			name: "a table with a computed column", kind: ObjectKindTable, object: "CUSTOMER",
			wantObject: "table", wantFeature: "TOTAL",
		},
		{
			// schema/README.md: procedure parameter nullability is unknown
			// unless a non-nullable domain proves it.
			name: "a procedure with unknown parameter nullability", kind: ObjectKindProcedure, object: "ADD_CUSTOMER",
			wantObject: "procedure", wantFeature: "CODE",
		},
		{
			name: "an external function is never reproducible", kind: ObjectKindFunction, object: "F_LTRIM",
			wantObject: "external function", wantFeature: "external calling convention",
		},
	}
	for _, test := range unsupported {
		t.Run(test.name, func(t *testing.T) {
			ddl, err := repository.ObjectDDL(ctx, test.kind, test.object)
			if !errors.Is(err, ErrUnsupportedDDL) {
				t.Fatalf("ObjectDDL(%s, %q) error = %v, want it to satisfy errors.Is(err, ErrUnsupportedDDL)",
					test.kind, test.object, err)
			}
			if errors.Is(err, ErrObjectNotFound) {
				t.Fatalf("an existing object must not report ErrObjectNotFound: %v", err)
			}
			if ddl != "" {
				t.Errorf("ObjectDDL(%s, %q) = %q, want no text alongside the error", test.kind, test.object, ddl)
			}
			// Structural, not a substring match on the message: sub-project 3
			// renders the reason from these three fields.
			object, name, feature, ok := UnsupportedDDLDetail(err)
			if !ok {
				t.Fatalf("UnsupportedDDLDetail(%v) reported ok == false, want the structured reason", err)
			}
			if object != test.wantObject {
				t.Errorf("detail object = %q, want %q", object, test.wantObject)
			}
			if name != test.object {
				t.Errorf("detail name = %q, want %q", name, test.object)
			}
			if !strings.Contains(feature, test.wantFeature) {
				t.Errorf("detail feature = %q, want it to name %q", feature, test.wantFeature)
			}
		})
	}

	// "No such object" and "the object exists but has no renderable DDL" are
	// the one distinction sub-project 3 needs in order to choose between
	// showing nothing and showing a reason, so ("", nil) is never returned.
	kinds := []ObjectKind{
		ObjectKindTable, ObjectKindView, ObjectKindProcedure, ObjectKindTrigger,
		ObjectKindDomain, ObjectKindIndex, ObjectKindGenerator, ObjectKindFunction,
	}
	for _, kind := range kinds {
		t.Run("unknown "+string(kind), func(t *testing.T) {
			ddl, err := repository.ObjectDDL(ctx, kind, "NO_SUCH_OBJECT")
			if !errors.Is(err, ErrObjectNotFound) {
				t.Fatalf("ObjectDDL(%s, unknown) error = %v, want ErrObjectNotFound", kind, err)
			}
			if ddl != "" {
				t.Errorf("ObjectDDL(%s, unknown) = %q, want no text", kind, ddl)
			}
		})
	}

	if _, err := repository.ObjectDDL(ctx, ObjectKindTable, ""); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("ObjectDDL with an empty name error = %v, want ErrObjectNotFound", err)
	}
	_, err := repository.ObjectDDL(ctx, ObjectKind("nonsense"), "PARENT")
	if err == nil {
		t.Fatal("ObjectDDL with an unknown kind returned a nil error")
	}
	if errors.Is(err, ErrObjectNotFound) || errors.Is(err, ErrUnsupportedDDL) {
		t.Errorf("an unknown object kind is a caller bug, not a catalog outcome: %v", err)
	}
}

func TestInterBaseUnsupportedDDLWithoutDetailStillMatchesTheSentinel(t *testing.T) {
	// The driver may return a bare schema.ErrUnsupportedDDL with no structured
	// reason. errors.Is must still hold; UnsupportedDDLDetail reports ok ==
	// false and sub-project 3 degrades to a generic line.
	wrapped := interBaseWrapDDLError(schema.ErrUnsupportedDDL)
	if !errors.Is(wrapped, ErrUnsupportedDDL) {
		t.Fatalf("interBaseWrapDDLError(bare sentinel) = %v, want it to satisfy errors.Is(err, ErrUnsupportedDDL)", wrapped)
	}
	if _, _, _, ok := UnsupportedDDLDetail(wrapped); ok {
		t.Error("a bare sentinel must report ok == false")
	}

	// An unrelated error passes through untouched: a real catalog fault must
	// not be relabelled as an unsupported-DDL outcome.
	fault := errors.New("interbase: connection reset")
	if got := interBaseWrapDDLError(fault); !errors.Is(got, fault) {
		t.Errorf("interBaseWrapDDLError(other) = %v, want the original error", got)
	}
	if errors.Is(interBaseWrapDDLError(fault), ErrUnsupportedDDL) {
		t.Error("an unrelated error must not satisfy errors.Is(err, ErrUnsupportedDDL)")
	}
	if interBaseWrapDDLError(nil) != nil {
		t.Error("interBaseWrapDDLError(nil) must be nil")
	}
}
```

Add `"errors"` to the test file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/database/ -count=1 -run 'InterBaseObjectDDL|UnsupportedDDLWithoutDetail' -v`
Expected: FAIL to build — `repository.ObjectDDL undefined`, `undefined: interBaseWrapDDLError`.

- [ ] **Step 3: Create `internal/database/interbase_ddl.go`**

```go
package database

import (
	"context"
	"errors"
	"fmt"

	"interbase-go/schema"
)

var _ DDLRepository = (*InterBaseDBRepository)(nil)

// ObjectDDL reproduces an object's definition from the catalog.
//
// The object is always looked up first, so "no such object" and "the object
// exists but has no renderable DDL" stay distinguishable: the first is
// ErrObjectNotFound, the second ErrUnsupportedDDL. Returning ("", nil) for an
// unknown object would conflate them, and that distinction is the one a caller
// needs in order to choose between showing nothing and showing a reason.
//
// This is an interactive one-shot outside any cache build, so it reads through
// the pooled *sql.DB rather than through a snapshot.
func (db *InterBaseDBRepository) ObjectDDL(ctx context.Context, kind ObjectKind, name string) (string, error) {
	if db == nil || db.Conn == nil {
		return "", errors.New("interbase: database connection is nil")
	}
	if name == "" {
		return "", ErrObjectNotFound
	}
	catalog := schema.New(db.Conn)

	var generator schema.DDLer
	switch kind {
	case ObjectKindTable:
		// Table also loads constraints, indexes and triggers, which
		// Relation.GenerateDDL requires.
		object, err := catalog.Table(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	case ObjectKindView:
		object, err := catalog.View(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	case ObjectKindProcedure:
		object, err := catalog.Procedure(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	case ObjectKindTrigger:
		object, err := catalog.Trigger(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	case ObjectKindDomain:
		object, err := catalog.Domain(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	case ObjectKindIndex:
		object, err := catalog.Index(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	case ObjectKindGenerator:
		// InterBase's own DDL for this catalog object is CREATE GENERATOR.
		object, err := catalog.Generator(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	case ObjectKindFunction:
		// An external function's creation semantics include calling-convention
		// policy the catalog does not record, so GenerateDDL always refuses.
		// The lookup still runs, so an unknown name is ErrObjectNotFound.
		object, err := catalog.Function(ctx, name)
		if err != nil {
			return "", err
		}
		if object == nil {
			return "", ErrObjectNotFound
		}
		generator = *object
	default:
		return "", fmt.Errorf("interbase: unsupported object kind %q", kind)
	}

	ddl, err := generator.GenerateDDL()
	if err != nil {
		return "", interBaseWrapDDLError(err)
	}
	return ddl, nil
}

// interBaseUnsupportedDDL adapts the driver's structured refusal to the
// driver-neutral contract: errors.Is reaches the shared sentinel through
// Unwrap, and UnsupportedDDLDetail reaches the reason through the method,
// so capability.go never imports schema.
type interBaseUnsupportedDDL struct{ detail *schema.UnsupportedDDLError }

func (e *interBaseUnsupportedDDL) Error() string { return "interbase: " + e.detail.Error() }
func (e *interBaseUnsupportedDDL) Unwrap() error { return ErrUnsupportedDDL }
func (e *interBaseUnsupportedDDL) UnsupportedDDLDetail() (string, string, string) {
	return e.detail.Object, e.detail.Name, e.detail.Feature
}

// interBaseWrapDDLError maps a driver refusal onto ErrUnsupportedDDL, keeping
// the structured reason when the driver supplied one. Any other error is a
// real catalog fault and passes through unchanged.
func interBaseWrapDDLError(err error) error {
	if err == nil || !errors.Is(err, schema.ErrUnsupportedDDL) {
		return err
	}
	var detail *schema.UnsupportedDDLError
	if errors.As(err, &detail) && detail != nil {
		return &interBaseUnsupportedDDL{detail: detail}
	}
	return fmt.Errorf("interbase: %w", ErrUnsupportedDDL)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/database/ -count=1 -run 'InterBaseObjectDDL|UnsupportedDDLWithoutDetail' -v`

Expected: PASS, every subtest. If one of the six rendered subtests fails with an `ErrUnsupportedDDL`, read the detail's feature string: it names the exact catalog metadata the fixture is missing. Fix the fixture row, not the assertion — every one of these six was verified to render against this fixture before the plan was written.

- [ ] **Step 5: Uncomment the `DDLRepository` compile-time assertion**

In `internal/database/capability_test.go`, uncomment the `_ DDLRepository = (*InterBaseDBRepository)(nil)` line and its runtime counterpart in `TestInterBaseRepositoryImplementsCapabilities`.

Run: `go test ./internal/database/ -count=1 -run 'ImplementsCapabilities' -v`
Expected: PASS.

- [ ] **Step 6: Run the whole suite**

Run: `go test ./...`

Expected: every package `ok`, including `TestNonInterBaseRepositoriesDoNotImplementCapabilities` — `ObjectDDL` must not have been added to any shared type.

- [ ] **Step 7: Commit**

```bash
git add internal/database/interbase_ddl.go internal/database/interbase_catalog_test.go internal/database/capability_test.go
git commit -m "feat(database): reproduce InterBase object DDL with a structured unsupported reason"
```

---

## Task 8: `CatalogSnapshot` and one catalog read per cache build

The N+1 mitigation, and the only structural one available without changing the driver. `GenerateDBCachePrimary` calls `SchemaTables`, then `DescribeDatabaseTableBySchema`, then `DescribeForeignKeysBySchema` (`cache.go:43-59`); mapping each independently onto `catalog.Relations` would walk every relation twice, because `Relations` issues one column query per relation. A snapshot collapses `2 + 2R + 4K` round trips to `2 + R + 4K`, under one consistency boundary — the per-relation term halves, which is the term that grows with the schema. (The constant is 2, not 1: one `Relations` query and one `Constraints` query. The spec phrases it as `1 + R + 4K` by folding the constraints query into the `4K`; the arithmetic below is the exact count.)

**Files:**
- Modify: `internal/database/interbase_catalog.go` (add `CatalogSnapshot`)
- Modify: `internal/database/cache.go:19-65` (wrap the generate entry points)
- Modify: `internal/database/capability_test.go` (uncomment the `CatalogSnapshotRepository` assertion)
- Create: `internal/database/cache_test.go`
- Test: `internal/database/interbase_catalog_test.go`

**Interfaces:**
- Consumes: `CatalogSnapshotRepository` (Task 1); `interBaseCatalogSnapshot`, `relations`, `constraints` (Task 4).
- Produces:
  - `func (db *InterBaseDBRepository) CatalogSnapshot(ctx context.Context) (DBRepository, func() error, error)`
  - `func (u *DBCacheGenerator) snapshot(ctx context.Context) (*DBCacheGenerator, func() error)` (unexported)
  - `GenerateDBCachePrimary` and `GenerateDBCacheSecondary` keep their exact signatures; their bodies move to unexported `generateDBCachePrimary` / `generateDBCacheSecondary`.

- [ ] **Step 1: Write the failing test**

Append to `internal/database/interbase_catalog_test.go`:

```go
func TestInterBaseCatalogSnapshotServesReadsFromOneTransaction(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	source := &InterBaseDBRepository{Conn: db, SQLDialect: 3, DatabaseName: "/srv/interbase/example.ib"}
	ctx := context.Background()

	snapshot, closeSnapshot, err := source.CatalogSnapshot(ctx)
	if err != nil {
		t.Fatalf("CatalogSnapshot() error = %v", err)
	}
	if closeSnapshot == nil {
		t.Fatal("CatalogSnapshot() returned a nil closer")
	}

	// A new repository, not the receiver: ReCache runs on a handler goroutine
	// while the worker's secondary pass runs on its own, so a shared mutable
	// snapshot field would race.
	if snapshot == DBRepository(source) {
		t.Fatal("CatalogSnapshot() returned the source repository; it must return a new one")
	}
	if source.snapshot != nil {
		t.Fatal("CatalogSnapshot() mutated the source repository")
	}
	bound, ok := snapshot.(*InterBaseDBRepository)
	if !ok {
		t.Fatalf("CatalogSnapshot() = %T, want *InterBaseDBRepository", snapshot)
	}
	if bound.snapshot == nil {
		t.Fatal("the returned repository is not bound to a snapshot")
	}
	if bound.SQLDialect != source.SQLDialect || bound.DatabaseName != source.DatabaseName {
		t.Errorf("snapshot repository = (%d, %q), want the source's (%d, %q)",
			bound.SQLDialect, bound.DatabaseName, source.SQLDialect, source.DatabaseName)
	}

	// The snapshot serves the same answers as the direct path.
	directTables, err := source.SchemaTables(ctx)
	if err != nil {
		t.Fatalf("SchemaTables() on the source error = %v", err)
	}
	snapshotTables, err := snapshot.SchemaTables(ctx)
	if err != nil {
		t.Fatalf("SchemaTables() on the snapshot error = %v", err)
	}
	if !reflect.DeepEqual(directTables, snapshotTables) {
		t.Errorf("snapshot SchemaTables() = %#v, want the direct result %#v", snapshotTables, directTables)
	}

	directColumns, err := source.DescribeDatabaseTableBySchema(ctx, "")
	if err != nil {
		t.Fatalf("DescribeDatabaseTableBySchema() on the source error = %v", err)
	}
	snapshotColumns, err := snapshot.DescribeDatabaseTableBySchema(ctx, "")
	if err != nil {
		t.Fatalf("DescribeDatabaseTableBySchema() on the snapshot error = %v", err)
	}
	if len(snapshotColumns) != len(directColumns) {
		t.Fatalf("snapshot returned %d columns, want %d", len(snapshotColumns), len(directColumns))
	}

	foreignKeys, err := snapshot.DescribeForeignKeysBySchema(ctx, "")
	if err != nil {
		t.Fatalf("DescribeForeignKeysBySchema() on the snapshot error = %v", err)
	}
	if len(foreignKeys) != 1 {
		t.Errorf("snapshot returned %d foreign keys, want 1", len(foreignKeys))
	}

	// Extended-catalog reads run on the same transaction.
	if views, err := bound.DescribeViews(ctx); err != nil || len(views) != 1 {
		t.Errorf("DescribeViews() on the snapshot = (%d views, %v), want (1, nil)", len(views), err)
	}

	// The source repository is unaffected and remains usable concurrently.
	if _, err := source.DescribeViews(ctx); err != nil {
		t.Errorf("the source repository must stay usable while a snapshot is open: %v", err)
	}

	if err := closeSnapshot(); err != nil {
		t.Errorf("close() error = %v", err)
	}
	if _, err := source.SchemaTables(ctx); err != nil {
		t.Errorf("the source repository must stay usable after the snapshot closes: %v", err)
	}
}

func TestInterBaseCatalogSnapshotHalvesThePerRelationReads(t *testing.T) {
	// The reason CatalogSnapshot exists is round-trip count, so count them
	// rather than asserting the structure and trusting the arithmetic. The
	// fixture's driver funnels every statement through PrepareContext, so the
	// counter is the exact number of statements a cache build issues.
	db := openInterBaseSchemaFixture(t)
	repo := &InterBaseDBRepository{Conn: db, SQLDialect: 3}
	ctx := context.Background()

	// The direct path: SchemaTables and DescribeDatabaseTableBySchema each walk
	// every relation, because Relations issues one column query per relation.
	prepares := interBaseFixtureCountPrepares(t)
	if _, err := repo.SchemaTables(ctx); err != nil {
		t.Fatalf("SchemaTables() error = %v", err)
	}
	if _, err := repo.DescribeDatabaseTableBySchema(ctx, ""); err != nil {
		t.Fatalf("DescribeDatabaseTableBySchema() error = %v", err)
	}
	if _, err := repo.DescribeForeignKeysBySchema(ctx, ""); err != nil {
		t.Fatalf("DescribeForeignKeysBySchema() error = %v", err)
	}
	direct := prepares()

	// The snapshot path: one Relations read shared by both.
	prepares = interBaseFixtureCountPrepares(t)
	snapshot, closeSnapshot, err := repo.CatalogSnapshot(ctx)
	if err != nil {
		t.Fatalf("CatalogSnapshot() error = %v", err)
	}
	if _, err := snapshot.SchemaTables(ctx); err != nil {
		t.Fatalf("snapshot SchemaTables() error = %v", err)
	}
	if _, err := snapshot.DescribeDatabaseTableBySchema(ctx, ""); err != nil {
		t.Fatalf("snapshot DescribeDatabaseTableBySchema() error = %v", err)
	}
	if _, err := snapshot.DescribeForeignKeysBySchema(ctx, ""); err != nil {
		t.Fatalf("snapshot DescribeForeignKeysBySchema() error = %v", err)
	}
	if err := closeSnapshot(); err != nil {
		t.Fatalf("close() error = %v", err)
	}
	snapshotted := prepares()

	// Do not pin an exact number: it moves whenever the fixture gains a
	// relation or a constraint, and the claim is about growth, not a constant.
	// The per-relation term is what halves, so the snapshot path must issue
	// strictly fewer statements than the direct path for the same answers.
	if snapshotted >= direct {
		t.Errorf("snapshot issued %d statements, direct issued %d; the snapshot must share one relation read",
			snapshotted, direct)
	}
	t.Logf("catalog reads: direct %d statements, snapshot %d", direct, snapshotted)
}
```

That test is the evidence for the claim in this task's preamble. If it ever
reports `snapshot >= direct`, the snapshot is being rebuilt per call rather than
reused, and the mitigation is not doing anything — stop and fix it rather than
relaxing the assertion.

Create `internal/database/cache_test.go`:

```go
package database

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/sqls-server/sqls/dialect"
)

// countingSnapshotRepository records how a cache build reached it: which
// repository served each read, and how many snapshots were opened and closed.
type countingSnapshotRepository struct {
	*MockDBRepository

	snapshotSupported bool
	// served is the shared tally; a snapshot shares its source's pointer so
	// the test can see which object answered.
	served *[]string
	opened *int
	closed *int
	// isSnapshot marks a repository handed out by CatalogSnapshot.
	isSnapshot bool
}

func newCountingSnapshotRepository(snapshotSupported bool) *countingSnapshotRepository {
	served := make([]string, 0)
	opened := 0
	closed := 0
	repository := &countingSnapshotRepository{
		MockDBRepository:  NewMockDBRepository(nil).(*MockDBRepository),
		snapshotSupported: snapshotSupported,
		served:            &served,
		opened:            &opened,
		closed:            &closed,
	}
	return repository
}

func (r *countingSnapshotRepository) record(method string) {
	source := "direct"
	if r.isSnapshot {
		source = "snapshot"
	}
	*r.served = append(*r.served, source+":"+method)
}

func (r *countingSnapshotRepository) SchemaTables(ctx context.Context) (map[string][]string, error) {
	r.record("SchemaTables")
	return r.MockDBRepository.SchemaTables(ctx)
}

func (r *countingSnapshotRepository) DescribeDatabaseTableBySchema(ctx context.Context, schemaName string) ([]*ColumnDesc, error) {
	r.record("DescribeDatabaseTableBySchema")
	return r.MockDBRepository.DescribeDatabaseTableBySchema(ctx, schemaName)
}

func (r *countingSnapshotRepository) DescribeForeignKeysBySchema(ctx context.Context, schemaName string) ([]*ForeignKey, error) {
	r.record("DescribeForeignKeysBySchema")
	return r.MockDBRepository.DescribeForeignKeysBySchema(ctx, schemaName)
}

func (r *countingSnapshotRepository) CatalogSnapshot(ctx context.Context) (DBRepository, func() error, error) {
	if !r.snapshotSupported {
		return nil, nil, errors.New("this fake does not support snapshots")
	}
	*r.opened++
	bound := &countingSnapshotRepository{
		MockDBRepository:  r.MockDBRepository,
		snapshotSupported: true,
		served:            r.served,
		opened:            r.opened,
		closed:            r.closed,
		isSnapshot:        true,
	}
	return bound, func() error { *r.closed++; return nil }, nil
}

// plainRepository is the same fake without the capability, so the direct path
// is exercised by a type that cannot accidentally satisfy the interface.
type plainRepository struct {
	*MockDBRepository
	served *[]string
}

func (r *plainRepository) SchemaTables(ctx context.Context) (map[string][]string, error) {
	*r.served = append(*r.served, "direct:SchemaTables")
	return r.MockDBRepository.SchemaTables(ctx)
}

func TestCacheBuildUsesOneCatalogSnapshot(t *testing.T) {
	repository := newCountingSnapshotRepository(true)
	if _, ok := DBRepository(repository).(CatalogSnapshotRepository); !ok {
		t.Fatal("the fake must implement CatalogSnapshotRepository")
	}

	if _, err := NewDBCacheUpdater(repository).GenerateDBCachePrimary(context.Background()); err != nil {
		t.Fatalf("GenerateDBCachePrimary() error = %v", err)
	}

	if *repository.opened != 1 {
		t.Errorf("CatalogSnapshot was opened %d times, want exactly 1 per cache build", *repository.opened)
	}
	if *repository.closed != 1 {
		t.Errorf("the snapshot was closed %d times, want exactly 1", *repository.closed)
	}
	want := []string{
		"snapshot:SchemaTables",
		"snapshot:DescribeDatabaseTableBySchema",
		"snapshot:DescribeForeignKeysBySchema",
	}
	if !reflect.DeepEqual(*repository.served, want) {
		t.Errorf("reads served = %v, want every primary-pass read served from the snapshot %v", *repository.served, want)
	}
}

func TestCacheBuildWithoutSnapshotCapabilityUsesTheRepositoryDirectly(t *testing.T) {
	served := make([]string, 0)
	repository := &plainRepository{MockDBRepository: NewMockDBRepository(nil).(*MockDBRepository), served: &served}
	if _, ok := DBRepository(repository).(CatalogSnapshotRepository); ok {
		t.Fatal("the plain fake must not implement CatalogSnapshotRepository")
	}

	cache, err := NewDBCacheUpdater(repository).GenerateDBCachePrimary(context.Background())
	if err != nil {
		t.Fatalf("GenerateDBCachePrimary() error = %v", err)
	}
	if cache == nil || len(cache.SchemaTables) == 0 {
		t.Fatal("the direct path must still build a usable cache")
	}
	if len(served) != 1 || served[0] != "direct:SchemaTables" {
		t.Errorf("reads served = %v, want the repository itself to answer", served)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/database/ -count=1 -run 'CatalogSnapshotServesReads|CacheBuildUsesOneCatalogSnapshot|CacheBuildWithoutSnapshotCapability' -v`

Expected: FAIL to build — `source.CatalogSnapshot undefined`. After `CatalogSnapshot` exists but before `cache.go` is wired, `TestCacheBuildUsesOneCatalogSnapshot` fails with `CatalogSnapshot was opened 0 times, want exactly 1` and `reads served = [direct:SchemaTables direct:DescribeDatabaseTableBySchema direct:DescribeForeignKeysBySchema]`. That is the honest failure: the interface can exist and be ignored, and this is what catches it.

- [ ] **Step 3: Implement `CatalogSnapshot`**

Append to `internal/database/interbase_catalog.go`:

```go
var _ CatalogSnapshotRepository = (*InterBaseDBRepository)(nil)

// CatalogSnapshot returns a read-only repository bound to one transaction that
// has already read every relation and constraint, so a whole cache build costs
// one catalog read instead of one per method.
//
// The returned repository is new. The receiver is never mutated, because
// ReCache runs on a handler goroutine while the worker's secondary pass runs
// on its own; a shared mutable snapshot field would race.
func (db *InterBaseDBRepository) CatalogSnapshot(ctx context.Context) (DBRepository, func() error, error) {
	if db == nil || db.Conn == nil {
		return nil, nil, errors.New("interbase: database connection is nil")
	}
	if db.snapshot != nil {
		// Already bound; nesting would open a second transaction for nothing.
		return db, func() error { return nil }, nil
	}

	tx, err := db.Conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	catalog := schema.New(tx)
	relations, err := catalog.Relations(ctx, "")
	if err != nil {
		_ = tx.Rollback()
		return nil, nil, err
	}
	constraints, err := catalog.Constraints(ctx, "")
	if err != nil {
		_ = tx.Rollback()
		return nil, nil, err
	}

	bound := &InterBaseDBRepository{
		Conn:         db.Conn,
		SQLDialect:   db.SQLDialect,
		DatabaseName: db.DatabaseName,
		snapshot: &interBaseCatalogSnapshot{
			catalog:     catalog,
			relations:   relations,
			constraints: constraints,
		},
	}
	// The snapshot is read-only, so rolling back is the whole of closing it.
	return bound, tx.Rollback, nil
}
```

- [ ] **Step 4: Wire the cache build to open one snapshot**

In `internal/database/cache.go`, add `"log"` to the imports and insert before `GenerateDBCachePrimary`:

```go
// snapshot returns a generator whose reads are served from one consistent
// catalog read, plus the closer that ends it. A repository without the
// capability is used directly with a no-op closer, which is the normal case
// for every driver but InterBase.
func (u *DBCacheGenerator) snapshot(ctx context.Context) (*DBCacheGenerator, func() error) {
	noop := func() error { return nil }
	source, ok := u.repo.(CatalogSnapshotRepository)
	if !ok {
		return u, noop
	}
	repo, closeSnapshot, err := source.CatalogSnapshot(ctx)
	if err != nil || repo == nil {
		// A snapshot is an optimisation, not a requirement: log the reason and
		// build the cache the slow way rather than failing the whole pass.
		if closeSnapshot != nil {
			_ = closeSnapshot()
		}
		if err != nil {
			log.Println("db cache: catalog snapshot unavailable:", err)
		}
		return u, noop
	}
	return &DBCacheGenerator{repo: repo}, closeSnapshot
}
```

Then rename the existing `GenerateDBCachePrimary` body to `generateDBCachePrimary` and the existing `GenerateDBCacheSecondary` body to `generateDBCacheSecondary`, leaving both bodies otherwise untouched, and add the two exported wrappers in their place:

```go
func (u *DBCacheGenerator) GenerateDBCachePrimary(ctx context.Context) (*DBCache, error) {
	generator, closeSnapshot := u.snapshot(ctx)
	defer func() { _ = closeSnapshot() }()
	return generator.generateDBCachePrimary(ctx)
}

func (u *DBCacheGenerator) GenerateDBCacheSecondary(ctx context.Context) (map[string][]*ColumnDesc, error) {
	generator, closeSnapshot := u.snapshot(ctx)
	defer func() { _ = closeSnapshot() }()
	return generator.generateDBCacheSecondary(ctx)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/database/ -count=1 -run 'CatalogSnapshotServesReads|CacheBuildUsesOneCatalogSnapshot|CacheBuildWithoutSnapshotCapability' -v`
Expected: PASS.

- [ ] **Step 6: Uncomment the `CatalogSnapshotRepository` compile-time assertion**

In `internal/database/capability_test.go`, uncomment the `_ CatalogSnapshotRepository = (*InterBaseDBRepository)(nil)` line and its runtime counterpart.

Run: `go test ./internal/database/ -count=1 -run 'ImplementsCapabilities' -v`
Expected: PASS.

- [ ] **Step 7: Run the whole suite**

Run: `go test ./...`

Expected: every package `ok`. `internal/completer` builds caches through `NewDBCacheUpdater(repo).GenerateDBCachePrimary` at `completer_test.go:358,400,436` with a `MockDBRepository`, which has no snapshot capability, so it takes the direct path unchanged.

- [ ] **Step 8: Commit**

```bash
git add internal/database/interbase_catalog.go internal/database/interbase_catalog_test.go internal/database/cache.go internal/database/cache_test.go internal/database/capability_test.go
git commit -m "feat(database): serve a whole cache build from one InterBase catalog snapshot"
```

---

## Task 9: `CatalogCache`, its accessors, and the worker's independent catalog pass

The consumer-facing half of §4.5: the cache the editor features read, the generator that fills it, the capability mock sub-project 3 tests against, and the two worker details the spec calls out.

**Files:**
- Modify: `internal/database/cache.go:130-136` (`DBCache`), and append the cache type, accessors and `GenerateCatalogCache`
- Modify: `internal/database/worker.go:37-47` (add `setCatalogCache`), `:49-69` (independent passes), `:95-97` (non-blocking send)
- Create: `internal/database/capability_mock.go`
- Test: `internal/database/cache_test.go`

**Interfaces:**
- Consumes: every descriptor type and `CatalogRepository` (Task 1); `(*DBCacheGenerator).snapshot` (Task 8).
- Produces:
  - `type CatalogCache struct { Views, Procedures, Generators, Domains, Functions, Indexes, Triggers map[string]*…; IndexesByTable, TriggersByTable map[string][]*… }`
  - `DBCache.Catalog *CatalogCache`
  - `func (dc *DBCache) HasCatalog() bool`, `View`, `Procedure`, `Generator`, `Domain`, `Function`, `Index`, `Trigger`, `IndexesForTable`, `TriggersForTable`, `SortedProcedures`, `SortedViews`, `SortedGenerators`, `SortedFunctions`
  - `func (u *DBCacheGenerator) GenerateCatalogCache(ctx context.Context) (*CatalogCache, bool, error)`
  - `func (w *Worker) setCatalogCache(c *CatalogCache)`
  - `type MockCatalogDBRepository struct { *MockDBRepository; … }` with `MockDescribeViews`, `MockDescribeProcedures`, `MockDescribeGenerators`, `MockDescribeTriggers`, `MockDescribeDomains`, `MockDescribeIndexes`, `MockDescribeFunctions`, `MockObjectDDL`, `MockExplainPlan`

- [ ] **Step 1: Write the failing test**

Append to `internal/database/cache_test.go`:

```go
func catalogTestRepository() *MockCatalogDBRepository {
	repository := NewMockCatalogDBRepository(nil)
	repository.MockDescribeViews = func(context.Context) ([]*ViewDesc, error) {
		return []*ViewDesc{{Name: "Customer_View", ViewSource: sql.NullString{String: "SELECT 1", Valid: true}}}, nil
	}
	repository.MockDescribeProcedures = func(context.Context) ([]*ProcedureDesc, error) {
		return []*ProcedureDesc{
			{Name: "Add_Customer", Source: sql.NullString{String: "BEGIN END", Valid: true}},
			{Name: "Drop_Customer"},
		}, nil
	}
	repository.MockDescribeGenerators = func(context.Context) ([]*GeneratorDesc, error) {
		return []*GeneratorDesc{{Name: "Gen_Customer_Id", ID: sql.NullInt64{Int64: 1, Valid: true}}}, nil
	}
	repository.MockDescribeDomains = func(context.Context) ([]*DomainDesc, error) {
		return []*DomainDesc{{Name: "Email_Address", Type: "VARCHAR(100)"}}, nil
	}
	repository.MockDescribeFunctions = func(context.Context) ([]*FunctionDesc, error) {
		return []*FunctionDesc{{Name: "F_Ltrim"}, {Name: "F_Rtrim"}}, nil
	}
	repository.MockDescribeIndexes = func(context.Context) ([]*IndexDesc, error) {
		return []*IndexDesc{
			{Name: "Idx_Customer_Pk", RelationName: "Customer", Columns: []string{"ID"}},
			{Name: "Idx_Customer_Code", RelationName: "Customer", Columns: []string{"CODE"}},
			{Name: "Idx_Parent_Pk", RelationName: "Parent", Columns: []string{"PARENT_B"}},
		}, nil
	}
	repository.MockDescribeTriggers = func(context.Context) ([]*TriggerDesc, error) {
		return []*TriggerDesc{
			{Name: "Customer_Bi", RelationName: sql.NullString{String: "Customer", Valid: true}},
			{Name: "Db_Connect"},
		}, nil
	}
	return repository
}

func TestGenerateCatalogCacheFromCapabilityRepository(t *testing.T) {
	repository := catalogTestRepository()

	catalog, ok, err := NewDBCacheUpdater(repository).GenerateCatalogCache(context.Background())
	if err != nil {
		t.Fatalf("GenerateCatalogCache() error = %v", err)
	}
	if !ok {
		t.Fatal("GenerateCatalogCache() reported no extended catalog for a CatalogRepository")
	}
	if catalog == nil {
		t.Fatal("GenerateCatalogCache() returned a nil catalog with ok == true")
	}

	// Keys are upper-cased, because InterBase stores catalog names upper-cased
	// while users type them lower-cased.
	for _, key := range []string{"CUSTOMER_VIEW", "ADD_CUSTOMER", "GEN_CUSTOMER_ID", "EMAIL_ADDRESS", "F_LTRIM", "IDX_CUSTOMER_PK", "CUSTOMER_BI"} {
		found := false
		for _, keys := range []map[string]bool{
			catalogKeySet(catalog.Views), catalogKeySet(catalog.Procedures), catalogKeySet(catalog.Generators),
			catalogKeySet(catalog.Domains), catalogKeySet(catalog.Functions), catalogKeySet(catalog.Indexes),
			catalogKeySet(catalog.Triggers),
		} {
			if keys[key] {
				found = true
			}
		}
		if !found {
			t.Errorf("no cache map is keyed by %q", key)
		}
	}

	if got, want := len(catalog.IndexesByTable["CUSTOMER"]), 2; got != want {
		t.Errorf("IndexesByTable[CUSTOMER] = %d indexes, want %d", got, want)
	}
	if got, want := len(catalog.TriggersByTable["CUSTOMER"]), 1; got != want {
		t.Errorf("TriggersByTable[CUSTOMER] = %d triggers, want %d", got, want)
	}
	// A database-level trigger has no relation and must not be grouped under
	// the empty table name.
	if got := len(catalog.TriggersByTable[""]); got != 0 {
		t.Errorf("TriggersByTable[\"\"] = %d triggers, want 0", got)
	}

	cache := &DBCache{Catalog: catalog}
	if !cache.HasCatalog() {
		t.Error("HasCatalog() = false, want true")
	}

	// Every singular accessor normalises the name it is given: callers pass
	// the identifier text as the user typed it and never upper-case at the
	// call site.
	if view, ok := cache.View("customer_view"); !ok || view.Name != "Customer_View" {
		t.Errorf("View(lower case) = (%#v, %v), want the cached view", view, ok)
	}
	if procedure, ok := cache.Procedure("Add_Customer"); !ok || !procedure.Source.Valid {
		t.Errorf("Procedure() = (%#v, %v), want the cached procedure", procedure, ok)
	}
	if generator, ok := cache.Generator("gen_customer_id"); !ok || generator.ID.Int64 != 1 {
		t.Errorf("Generator(lower case) = (%#v, %v), want the cached generator", generator, ok)
	}
	if domain, ok := cache.Domain("email_address"); !ok || domain.Type != "VARCHAR(100)" {
		t.Errorf("Domain(lower case) = (%#v, %v), want the cached domain", domain, ok)
	}
	if function, ok := cache.Function("f_ltrim"); !ok || function.Name != "F_Ltrim" {
		t.Errorf("Function(lower case) = (%#v, %v), want the cached function", function, ok)
	}
	if index, ok := cache.Index("idx_parent_pk"); !ok || index.RelationName != "Parent" {
		t.Errorf("Index(lower case) = (%#v, %v), want the cached index", index, ok)
	}
	if trigger, ok := cache.Trigger("customer_bi"); !ok || trigger.Name != "Customer_Bi" {
		t.Errorf("Trigger(lower case) = (%#v, %v), want the cached trigger", trigger, ok)
	}
	if _, ok := cache.View("no_such_view"); ok {
		t.Error("View(unknown) reported ok == true")
	}

	// The grouped accessors normalise their table argument the same way.
	if got := len(cache.IndexesForTable("customer")); got != 2 {
		t.Errorf("IndexesForTable(lower case) = %d indexes, want 2", got)
	}
	if got := len(cache.TriggersForTable("customer")); got != 1 {
		t.Errorf("TriggersForTable(lower case) = %d triggers, want 1", got)
	}
	if got := len(cache.IndexesForTable("no_such_table")); got != 0 {
		t.Errorf("IndexesForTable(unknown) = %d indexes, want 0", got)
	}

	if got, want := cache.SortedProcedures(), []string{"Add_Customer", "Drop_Customer"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SortedProcedures() = %v, want %v", got, want)
	}
	if got, want := cache.SortedViews(), []string{"Customer_View"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SortedViews() = %v, want %v", got, want)
	}
	if got, want := cache.SortedGenerators(), []string{"Gen_Customer_Id"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SortedGenerators() = %v, want %v", got, want)
	}
	if got, want := cache.SortedFunctions(), []string{"F_Ltrim", "F_Rtrim"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SortedFunctions() = %v, want %v", got, want)
	}
}

func catalogKeySet[V any](source map[string]V) map[string]bool {
	keys := make(map[string]bool, len(source))
	for key := range source {
		keys[key] = true
	}
	return keys
}

func TestGenerateCatalogCacheWithoutCapability(t *testing.T) {
	catalog, ok, err := NewDBCacheUpdater(NewMockDBRepository(nil)).GenerateCatalogCache(context.Background())
	if err != nil {
		t.Fatalf("GenerateCatalogCache() error = %v", err)
	}
	if ok || catalog != nil {
		t.Fatalf("GenerateCatalogCache() = (%#v, %v), want (nil, false) for a repository without the capability", catalog, ok)
	}

	// Every accessor is nil-safe: a nil *CatalogCache is the normal state for
	// every driver but InterBase, and for InterBase before the first
	// successful secondary pass.
	for _, cache := range []*DBCache{{}, {Catalog: nil}, nil} {
		if cache.HasCatalog() {
			t.Error("HasCatalog() = true without a catalog")
		}
		if _, ok := cache.View("anything"); ok {
			t.Error("View() reported ok == true without a catalog")
		}
		if _, ok := cache.Procedure("anything"); ok {
			t.Error("Procedure() reported ok == true without a catalog")
		}
		if _, ok := cache.Generator("anything"); ok {
			t.Error("Generator() reported ok == true without a catalog")
		}
		if _, ok := cache.Domain("anything"); ok {
			t.Error("Domain() reported ok == true without a catalog")
		}
		if _, ok := cache.Function("anything"); ok {
			t.Error("Function() reported ok == true without a catalog")
		}
		if _, ok := cache.Index("anything"); ok {
			t.Error("Index() reported ok == true without a catalog")
		}
		if _, ok := cache.Trigger("anything"); ok {
			t.Error("Trigger() reported ok == true without a catalog")
		}
		if len(cache.IndexesForTable("anything")) != 0 || len(cache.TriggersForTable("anything")) != 0 {
			t.Error("a grouped accessor returned rows without a catalog")
		}
		if len(cache.SortedProcedures()) != 0 || len(cache.SortedViews()) != 0 ||
			len(cache.SortedGenerators()) != 0 || len(cache.SortedFunctions()) != 0 {
			t.Error("a sorted accessor returned names without a catalog")
		}
	}
}

func TestWorkerSwapsCatalogCache(t *testing.T) {
	worker := NewWorker()
	worker.Start()
	t.Cleanup(worker.Stop)

	if err := worker.ReCache(context.Background(), catalogTestRepository()); err != nil {
		t.Fatalf("ReCache() error = %v", err)
	}
	before := worker.Cache()
	if before == nil {
		t.Fatal("ReCache() left no primary cache")
	}
	if before.HasCatalog() {
		t.Fatal("the primary pass must not build the extended catalog; it is the secondary pass's job")
	}

	after := waitForCatalog(t, worker)
	if _, ok := after.Procedure("add_customer"); !ok {
		t.Error("the swapped catalog does not contain the procedure")
	}
	// Copy on write, mirroring setColumnCache: a reader holding the previous
	// *DBCache keeps seeing a consistent snapshot.
	if before.HasCatalog() {
		t.Error("the previously returned *DBCache was mutated in place")
	}
	if before == after {
		t.Error("the worker must swap in a new *DBCache rather than mutate the old one")
	}
}

func TestWorkerCatalogPassRunsDespiteColumnPassError(t *testing.T) {
	// worker.go's loop body continues on a GenerateDBCacheSecondary error, so
	// appending the catalog build after it would silently skip the catalog
	// whenever the column pass failed. Each pass must be attempted on its own.
	repository := catalogTestRepository()
	columnFailure := errors.New("describe database table failed")
	repository.MockDescribeDatabaseTable = func(context.Context) ([]*ColumnDesc, error) {
		return nil, columnFailure
	}

	worker := NewWorker()
	worker.Start()
	t.Cleanup(worker.Stop)

	if err := worker.ReCache(context.Background(), repository); err != nil {
		t.Fatalf("ReCache() error = %v", err)
	}

	catalog := waitForCatalog(t, worker)
	if _, ok := catalog.View("customer_view"); !ok {
		t.Error("the catalog pass did not complete even though only the column pass failed")
	}
}

func waitForCatalog(t *testing.T, worker *Worker) *DBCache {
	t.Helper()
	// The worker offers no completion signal, so poll. Two seconds is far more
	// than an in-process fake needs and short enough to fail fast.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cache := worker.Cache(); cache.HasCatalog() {
			return cache
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the worker never swapped in an extended catalog")
	return nil
}

func TestWorkerUpdateSignalDoesNotBlock(t *testing.T) {
	// updateAdditionalCache is reached from ReCache <- reconnectionDB <-
	// handleWorkspaceDidChangeConfiguration, an LSP handler. A blocking send
	// on the size-1 update channel would make a configuration change wait for
	// a whole catalog pass. The worker goroutine is deliberately NOT started
	// here and the buffered slot is pre-filled, so a blocking send never
	// returns.
	worker := NewWorker()
	worker.update <- struct{}{}

	done := make(chan error, 1)
	go func() {
		done <- worker.ReCache(context.Background(), NewMockDBRepository(nil))
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ReCache() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReCache() blocked on a full update channel; the send must be non-blocking")
	}

	// Dropping a duplicate signal loses nothing: the queued pass will read
	// state at least as fresh.
	if len(worker.update) != 1 {
		t.Errorf("update channel holds %d signals, want the one already queued", len(worker.update))
	}
}

func TestMockCatalogDBRepositoryIsDistinctFromMockDBRepository(t *testing.T) {
	// If MockDBRepository itself satisfied the capability interfaces, every
	// existing handler test would start taking the capability branch and panic
	// on a nil func field.
	plain := DBRepository(NewMockDBRepository(nil))
	if _, ok := plain.(CatalogRepository); ok {
		t.Error("MockDBRepository must not implement CatalogRepository")
	}

	capable := DBRepository(NewMockCatalogDBRepository(nil))
	for name, ok := range map[string]bool{
		"CatalogRepository": func() bool { _, ok := capable.(CatalogRepository); return ok }(),
		"DDLRepository":     func() bool { _, ok := capable.(DDLRepository); return ok }(),
		"ExplainRepository": func() bool { _, ok := capable.(ExplainRepository); return ok }(),
	} {
		if !ok {
			t.Errorf("MockCatalogDBRepository must implement %s", name)
		}
	}
	if got, want := capable.Driver(), dialect.DatabaseDriver("mock"); got != want {
		t.Errorf("mock driver = %q, want %q", got, want)
	}

	// Unset func fields answer empty rather than panicking, so a test that
	// exercises one capability need not stub all of them.
	bare := NewMockCatalogDBRepository(nil)
	views, err := bare.DescribeViews(context.Background())
	if err != nil || len(views) != 0 {
		t.Errorf("DescribeViews() on an unstubbed mock = (%v, %v), want (empty, nil)", views, err)
	}
	if ddl, err := bare.ObjectDDL(context.Background(), ObjectKindTable, "T"); err != nil || ddl != "" {
		t.Errorf("ObjectDDL() on an unstubbed mock = (%q, %v), want (\"\", nil)", ddl, err)
	}
}
```

`sort` is imported by the test file for later use by the accessors' own package; if `go vet` reports it unused in the test file, drop it from the test imports — it belongs to `cache.go`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/database/ -count=1 -run 'CatalogCache|Worker|MockCatalogDBRepository' -v`

Expected: FAIL to build — `undefined: NewMockCatalogDBRepository`, `undefined: CatalogCache`, `cache.HasCatalog undefined`, `u.GenerateCatalogCache undefined`.

Two of these fail for reasons worth naming, because each would still fail against a plausible-looking wrong implementation:

- `TestWorkerCatalogPassRunsDespiteColumnPassError` fails against an implementation that appends the catalog build after the existing `continue`-on-error column pass: the column pass errors, the loop `continue`s, the catalog is never built, and `waitForCatalog` times out after two seconds with "the worker never swapped in an extended catalog".
- `TestWorkerUpdateSignalDoesNotBlock` fails against the current blocking send by hanging: the goroutine never delivers to `done`, and the two-second `select` arm reports "ReCache() blocked on a full update channel". There is no handshake anywhere in this test that could create a happens-before edge and make it pass accidentally — the worker goroutine is never started, so nothing can ever drain the channel.

- [ ] **Step 3: Create the capability mock**

Create `internal/database/capability_mock.go`:

```go
package database

import (
	"context"
	"database/sql"
)

// MockCatalogDBRepository is a DBRepository that also implements the optional
// catalog capabilities. It is deliberately a separate type from
// MockDBRepository: if MockDBRepository itself satisfied these interfaces,
// every existing test that builds one would start taking the capability branch
// in production code and panic on a nil func field.
//
// An unset Mock… field answers with an empty result and a nil error, so a test
// stubs only the capability it exercises.
type MockCatalogDBRepository struct {
	*MockDBRepository

	MockDescribeViews      func(context.Context) ([]*ViewDesc, error)
	MockDescribeProcedures func(context.Context) ([]*ProcedureDesc, error)
	MockDescribeGenerators func(context.Context) ([]*GeneratorDesc, error)
	MockDescribeTriggers   func(context.Context) ([]*TriggerDesc, error)
	MockDescribeDomains    func(context.Context) ([]*DomainDesc, error)
	MockDescribeIndexes    func(context.Context) ([]*IndexDesc, error)
	MockDescribeFunctions  func(context.Context) ([]*FunctionDesc, error)
	MockObjectDDL          func(context.Context, ObjectKind, string) (string, error)
	MockExplainPlan        func(context.Context, string) (string, error)
}

var (
	_ DBRepository      = (*MockCatalogDBRepository)(nil)
	_ CatalogRepository = (*MockCatalogDBRepository)(nil)
	_ DDLRepository     = (*MockCatalogDBRepository)(nil)
	_ ExplainRepository = (*MockCatalogDBRepository)(nil)
)

// NewMockCatalogDBRepository returns a capability mock backed by the same
// dummy data as NewMockDBRepository, with every capability unstubbed.
func NewMockCatalogDBRepository(db *sql.DB) *MockCatalogDBRepository {
	return &MockCatalogDBRepository{
		MockDBRepository: NewMockDBRepository(db).(*MockDBRepository),
	}
}

func (m *MockCatalogDBRepository) DescribeViews(ctx context.Context) ([]*ViewDesc, error) {
	if m.MockDescribeViews == nil {
		return nil, nil
	}
	return m.MockDescribeViews(ctx)
}

func (m *MockCatalogDBRepository) DescribeProcedures(ctx context.Context) ([]*ProcedureDesc, error) {
	if m.MockDescribeProcedures == nil {
		return nil, nil
	}
	return m.MockDescribeProcedures(ctx)
}

func (m *MockCatalogDBRepository) DescribeGenerators(ctx context.Context) ([]*GeneratorDesc, error) {
	if m.MockDescribeGenerators == nil {
		return nil, nil
	}
	return m.MockDescribeGenerators(ctx)
}

func (m *MockCatalogDBRepository) DescribeTriggers(ctx context.Context) ([]*TriggerDesc, error) {
	if m.MockDescribeTriggers == nil {
		return nil, nil
	}
	return m.MockDescribeTriggers(ctx)
}

func (m *MockCatalogDBRepository) DescribeDomains(ctx context.Context) ([]*DomainDesc, error) {
	if m.MockDescribeDomains == nil {
		return nil, nil
	}
	return m.MockDescribeDomains(ctx)
}

func (m *MockCatalogDBRepository) DescribeIndexes(ctx context.Context) ([]*IndexDesc, error) {
	if m.MockDescribeIndexes == nil {
		return nil, nil
	}
	return m.MockDescribeIndexes(ctx)
}

func (m *MockCatalogDBRepository) DescribeFunctions(ctx context.Context) ([]*FunctionDesc, error) {
	if m.MockDescribeFunctions == nil {
		return nil, nil
	}
	return m.MockDescribeFunctions(ctx)
}

func (m *MockCatalogDBRepository) ObjectDDL(ctx context.Context, kind ObjectKind, name string) (string, error) {
	if m.MockObjectDDL == nil {
		return "", nil
	}
	return m.MockObjectDDL(ctx, kind, name)
}

func (m *MockCatalogDBRepository) ExplainPlan(ctx context.Context, query string) (string, error) {
	if m.MockExplainPlan == nil {
		return "", nil
	}
	return m.MockExplainPlan(ctx, query)
}
```

- [ ] **Step 4: Add the cache type, the accessors and the generator**

In `internal/database/cache.go`, add `Catalog` to `DBCache`:

```go
type DBCache struct {
	defaultSchema     string
	Schemas           map[string]string
	SchemaTables      map[string][]string
	ColumnsWithParent map[string][]*ColumnDesc
	ForeignKeys       map[string]map[string][]*ForeignKey
	// Catalog holds extended catalog objects. It is nil when the active
	// repository does not implement CatalogRepository, and also before the
	// first successful secondary pass.
	Catalog *CatalogCache
}
```

and append:

```go
// CatalogCache holds extended catalog objects. A nil *CatalogCache means the
// active repository does not implement CatalogRepository. All maps are keyed by
// the upper-cased object name.
type CatalogCache struct {
	Views           map[string]*ViewDesc
	Procedures      map[string]*ProcedureDesc
	Generators      map[string]*GeneratorDesc
	Domains         map[string]*DomainDesc
	Functions       map[string]*FunctionDesc
	Indexes         map[string]*IndexDesc
	IndexesByTable  map[string][]*IndexDesc
	Triggers        map[string]*TriggerDesc
	TriggersByTable map[string][]*TriggerDesc
}

// catalogCacheKey normalises an object name for cache lookup. InterBase stores
// catalog names upper-cased while users type them lower-cased, so an
// exact-match accessor would silently miss for every lower-case identifier and
// the failure would look like missing metadata rather than a lookup bug. This
// follows the existing convention: columnDatabaseKey upper-cases its arguments
// and DBCache.Column matches with strings.EqualFold.
func catalogCacheKey(name string) string {
	return strings.ToUpper(strings.TrimSpace(name))
}

// HasCatalog reports whether the active repository produced an extended
// catalog. It is the single gate a feature uses before touching catalog data.
func (dc *DBCache) HasCatalog() bool {
	return dc != nil && dc.Catalog != nil
}

func (dc *DBCache) View(name string) (*ViewDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	view, ok := dc.Catalog.Views[catalogCacheKey(name)]
	return view, ok
}

func (dc *DBCache) Procedure(name string) (*ProcedureDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	procedure, ok := dc.Catalog.Procedures[catalogCacheKey(name)]
	return procedure, ok
}

func (dc *DBCache) Generator(name string) (*GeneratorDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	generator, ok := dc.Catalog.Generators[catalogCacheKey(name)]
	return generator, ok
}

func (dc *DBCache) Domain(name string) (*DomainDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	domain, ok := dc.Catalog.Domains[catalogCacheKey(name)]
	return domain, ok
}

func (dc *DBCache) Function(name string) (*FunctionDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	function, ok := dc.Catalog.Functions[catalogCacheKey(name)]
	return function, ok
}

func (dc *DBCache) Index(name string) (*IndexDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	index, ok := dc.Catalog.Indexes[catalogCacheKey(name)]
	return index, ok
}

func (dc *DBCache) Trigger(name string) (*TriggerDesc, bool) {
	if !dc.HasCatalog() {
		return nil, false
	}
	trigger, ok := dc.Catalog.Triggers[catalogCacheKey(name)]
	return trigger, ok
}

func (dc *DBCache) IndexesForTable(table string) []*IndexDesc {
	if !dc.HasCatalog() {
		return nil
	}
	return dc.Catalog.IndexesByTable[catalogCacheKey(table)]
}

func (dc *DBCache) TriggersForTable(table string) []*TriggerDesc {
	if !dc.HasCatalog() {
		return nil
	}
	return dc.Catalog.TriggersByTable[catalogCacheKey(table)]
}

func (dc *DBCache) SortedProcedures() []string {
	if !dc.HasCatalog() {
		return nil
	}
	names := make([]string, 0, len(dc.Catalog.Procedures))
	for _, procedure := range dc.Catalog.Procedures {
		names = append(names, procedure.Name)
	}
	sort.Strings(names)
	return names
}

func (dc *DBCache) SortedViews() []string {
	if !dc.HasCatalog() {
		return nil
	}
	names := make([]string, 0, len(dc.Catalog.Views))
	for _, view := range dc.Catalog.Views {
		names = append(names, view.Name)
	}
	sort.Strings(names)
	return names
}

func (dc *DBCache) SortedGenerators() []string {
	if !dc.HasCatalog() {
		return nil
	}
	names := make([]string, 0, len(dc.Catalog.Generators))
	for _, generator := range dc.Catalog.Generators {
		names = append(names, generator.Name)
	}
	sort.Strings(names)
	return names
}

// SortedFunctions exists because external functions are the one object kind a
// consumer must enumerate rather than look up.
func (dc *DBCache) SortedFunctions() []string {
	if !dc.HasCatalog() {
		return nil
	}
	names := make([]string, 0, len(dc.Catalog.Functions))
	for _, function := range dc.Catalog.Functions {
		names = append(names, function.Name)
	}
	sort.Strings(names)
	return names
}

// GenerateCatalogCache returns nil, false, nil when the repository has no
// extended catalog.
func (u *DBCacheGenerator) GenerateCatalogCache(ctx context.Context) (*CatalogCache, bool, error) {
	generator, closeSnapshot := u.snapshot(ctx)
	defer func() { _ = closeSnapshot() }()
	return generator.generateCatalogCache(ctx)
}

func (u *DBCacheGenerator) generateCatalogCache(ctx context.Context) (*CatalogCache, bool, error) {
	source, ok := u.repo.(CatalogRepository)
	if !ok {
		return nil, false, nil
	}

	catalog := &CatalogCache{
		Views:           map[string]*ViewDesc{},
		Procedures:      map[string]*ProcedureDesc{},
		Generators:      map[string]*GeneratorDesc{},
		Domains:         map[string]*DomainDesc{},
		Functions:       map[string]*FunctionDesc{},
		Indexes:         map[string]*IndexDesc{},
		IndexesByTable:  map[string][]*IndexDesc{},
		Triggers:        map[string]*TriggerDesc{},
		TriggersByTable: map[string][]*TriggerDesc{},
	}

	views, err := source.DescribeViews(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, view := range views {
		catalog.Views[catalogCacheKey(view.Name)] = view
	}

	procedures, err := source.DescribeProcedures(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, procedure := range procedures {
		catalog.Procedures[catalogCacheKey(procedure.Name)] = procedure
	}

	generators, err := source.DescribeGenerators(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, generator := range generators {
		catalog.Generators[catalogCacheKey(generator.Name)] = generator
	}

	domains, err := source.DescribeDomains(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, domain := range domains {
		catalog.Domains[catalogCacheKey(domain.Name)] = domain
	}

	functions, err := source.DescribeFunctions(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, function := range functions {
		catalog.Functions[catalogCacheKey(function.Name)] = function
	}

	indexes, err := source.DescribeIndexes(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, index := range indexes {
		catalog.Indexes[catalogCacheKey(index.Name)] = index
		if index.RelationName == "" {
			continue
		}
		key := catalogCacheKey(index.RelationName)
		catalog.IndexesByTable[key] = append(catalog.IndexesByTable[key], index)
	}

	triggers, err := source.DescribeTriggers(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, trigger := range triggers {
		catalog.Triggers[catalogCacheKey(trigger.Name)] = trigger
		// A database-level trigger has no relation and is reachable by name
		// only; grouping it under the empty table name would be a phantom.
		if !trigger.RelationName.Valid || strings.TrimSpace(trigger.RelationName.String) == "" {
			continue
		}
		key := catalogCacheKey(trigger.RelationName.String)
		catalog.TriggersByTable[key] = append(catalog.TriggersByTable[key], trigger)
	}

	return catalog, true, nil
}
```

- [ ] **Step 5: Make the worker build and swap the catalog independently**

In `internal/database/worker.go`, add after `setColumnCache`:

```go
func (w *Worker) setCatalogCache(c *CatalogCache) {
	w.lock.Lock()
	defer w.lock.Unlock()
	if w.dbCache != nil {
		// Swap in a copy so that readers holding the previous
		// *DBCache keep seeing a consistent snapshot.
		newCache := *w.dbCache
		newCache.Catalog = c
		w.dbCache = &newCache
	}
}
```

Replace the `case <-w.update:` body in `Start`:

```go
			case <-w.update:
				generator := NewDBCacheUpdater(w.dbRepo)
				// The two passes are independent. This loop used to continue
				// on a secondary-pass error, so appending the catalog build
				// after it would silently skip the catalog whenever the column
				// pass failed.
				if col, err := generator.GenerateDBCacheSecondary(context.Background()); err != nil {
					log.Println(err)
				} else {
					w.setColumnCache(col)
					log.Println("db worker: Update db cache secondary complete")
				}
				if catalog, ok, err := generator.GenerateCatalogCache(context.Background()); err != nil {
					// A catalog error leaves the previous *CatalogCache in
					// place, exactly as the column pass does.
					log.Println(err)
				} else if ok {
					w.setCatalogCache(catalog)
					log.Println("db worker: Update catalog cache complete")
				}
```

Replace `updateAdditionalCache`:

```go
func (w *Worker) updateAdditionalCache() {
	// Non-blocking: this is reached from an LSP handler through ReCache, and a
	// long catalog pass must not make a configuration change wait. A full slot
	// already holds a pending request, so dropping a duplicate signal loses
	// nothing — the in-flight or queued pass will read state that is at least
	// as fresh.
	select {
	case w.update <- struct{}{}:
	default:
	}
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/database/ -count=1 -run 'CatalogCache|Worker|MockCatalogDBRepository' -v`
Expected: PASS.

- [ ] **Step 7: Run the whole suite, including the race detector**

Run: `go test ./...`
Expected: every package `ok`.

Run: `go test -race ./internal/database/ -count=1`

Expected: `ok`, no race reports. The catalog pass runs on the worker goroutine and `TestWorkerSwapsCatalogCache` reads `Worker.Cache()` from the test goroutine, so this is the run that would catch a cache swap done outside the mutex.

- [ ] **Step 8: Commit**

```bash
git add internal/database/cache.go internal/database/cache_test.go internal/database/worker.go internal/database/capability_mock.go
git commit -m "feat(database): cache extended catalog objects in an independent worker pass"
```

---

## Task 10: `ExplainPlan` behind the build tag, and the live suite

The only tagged code in this plan. `ExplainRepository` is implemented in `interbase_native.go` and therefore **absent** on untagged builds — a capability interface must not be implemented by a method that always fails.

**Depends on** sub-project 1's `interbase.Plan(ctx context.Context, conn *sql.Conn, query string) (string, error)` (spec: `/home/a.simard@multidev.local/gits/interbase-go/docs/superpowers/specs/2026-09-19-pooled-introspection-design.md`). Everything in this task lives behind `//go:build interbase && cgo && linux && amd64`, so `go test ./...` stays green whether or not that code exists yet. If it does not, complete Task 11 first and come back.

**Files:**
- Modify: `internal/database/interbase_native.go` (append `ExplainPlan`)
- Modify: `internal/database/interbase_stub_test.go` (the absence assertion)
- Modify: `internal/database/interbase_live_test.go` (three live tests)

**Interfaces:**
- Consumes: `ExplainRepository` (Task 1); `CatalogRepository`, `DDLRepository` (Tasks 5–7); `GenerateCatalogCache` (Task 9); `interbase.Plan` (sub-project 1).
- Produces: `func (db *InterBaseDBRepository) ExplainPlan(ctx context.Context, query string) (string, error)`, tagged.

### A correction to the spec's test placement

Spec §6 lists `TestExplainRepositoryAbsentWithoutNativeBuild` in `internal/database/capability_test.go` and annotates it "(untagged file)". That placement cannot work: an untagged file is compiled under the tagged build **as well**, where `*InterBaseDBRepository` does implement `ExplainRepository`, so the assertion would fail every `-tags interbase` run. The test belongs in `interbase_stub_test.go`, whose build tag is the exact inverse (`!interbase || !cgo || !linux || !amd64`), with its positive counterpart in the tagged `interbase_live_test.go`. Same two assertions, correct files.

- [ ] **Step 1: Write the failing tests**

Append to `internal/database/interbase_stub_test.go` (build tag `!interbase || !cgo || !linux || !amd64`):

```go
func TestExplainRepositoryAbsentWithoutNativeBuild(t *testing.T) {
	// A query plan needs the native client, so on an untagged build the
	// capability is absent rather than stubbed: a capability interface must
	// not be implemented by a method that always fails. Sub-project 3's code
	// action simply does not offer.
	repository := DBRepository(&InterBaseDBRepository{})
	if _, ok := repository.(ExplainRepository); ok {
		t.Error("*InterBaseDBRepository must not implement ExplainRepository without the interbase build tag")
	}
	// The untagged capabilities are still present.
	if _, ok := repository.(CatalogRepository); !ok {
		t.Error("*InterBaseDBRepository must implement CatalogRepository on every build")
	}
	if _, ok := repository.(DDLRepository); !ok {
		t.Error("*InterBaseDBRepository must implement DDLRepository on every build")
	}
}
```

Append to `internal/database/interbase_live_test.go` (build tag `interbase && cgo && linux && amd64`):

```go
func TestExplainRepositoryPresentWithNativeBuild(t *testing.T) {
	repository := DBRepository(&InterBaseDBRepository{})
	if _, ok := repository.(ExplainRepository); !ok {
		t.Error("*InterBaseDBRepository must implement ExplainRepository under the interbase build tag")
	}
}

// interBaseLiveRepository opens the configured live database or skips.
func interBaseLiveRepository(t *testing.T) *InterBaseDBRepository {
	t.Helper()
	databaseName := os.Getenv("INTERBASE_DATABASE")
	user := os.Getenv("INTERBASE_USER")
	password, passwordSet := os.LookupEnv("INTERBASE_PASSWORD")
	if databaseName == "" || user == "" || !passwordSet {
		t.Skip("set INTERBASE_DATABASE, INTERBASE_USER, and INTERBASE_PASSWORD to run the live InterBase test")
	}

	connection, err := Open(&DBConfig{
		Driver:         dialect.DatabaseDriverInterBase,
		DataSourceName: databaseName,
		User:           user,
		Passwd:         password,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	return &InterBaseDBRepository{Conn: connection.Conn, DatabaseName: databaseName}
}

func TestInterBaseLiveCatalogObjectsSurface(t *testing.T) {
	repository := interBaseLiveRepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	generator := NewDBCacheUpdater(repository)

	started := time.Now()
	cache, err := generator.GenerateDBCachePrimary(ctx)
	if err != nil {
		t.Fatalf("GenerateDBCachePrimary() error = %v", err)
	}
	primaryElapsed := time.Since(started)
	tables := cache.SortedTables()
	if len(tables) == 0 {
		t.Fatal("GenerateDBCachePrimary() cached no tables")
	}
	columnCount := 0
	for _, columns := range cache.ColumnsWithParent {
		columnCount += len(columns)
	}
	if columnCount == 0 {
		t.Fatal("GenerateDBCachePrimary() cached no columns")
	}

	started = time.Now()
	catalog, ok, err := generator.GenerateCatalogCache(ctx)
	if err != nil {
		t.Fatalf("GenerateCatalogCache() error = %v", err)
	}
	catalogElapsed := time.Since(started)
	if !ok || catalog == nil {
		t.Fatal("GenerateCatalogCache() reported no extended catalog for an InterBase repository")
	}

	// The measurement that feeds the escalation trigger in the spec's Risk 1:
	// a primary pass above 5s or a catalog pass above 30s means the fix is a
	// bulk projection in the driver's schema package, not hand-written SQL
	// here. Logged rather than asserted, because it is a property of the
	// database under test.
	t.Logf("primary pass: %d tables, %d columns, %d foreign-key tables in %s",
		len(tables), columnCount, len(cache.ForeignKeys), primaryElapsed)
	t.Logf("catalog pass: %d views, %d procedures, %d generators, %d domains, %d indexes, %d triggers, %d functions in %s",
		len(catalog.Views), len(catalog.Procedures), len(catalog.Generators), len(catalog.Domains),
		len(catalog.Indexes), len(catalog.Triggers), len(catalog.Functions), catalogElapsed)
}

func TestInterBaseLiveObjectDDL(t *testing.T) {
	repository := interBaseLiveRepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	// Both outcomes are expected in a real database: computed columns block
	// table DDL and unknown parameter nullability blocks procedure DDL.
	assertRenderedOrExplained := func(t *testing.T, kind ObjectKind, name, wantPrefix string) {
		t.Helper()
		ddl, err := repository.ObjectDDL(ctx, kind, name)
		switch {
		case err == nil:
			if !strings.HasPrefix(strings.TrimSpace(ddl), wantPrefix) {
				t.Errorf("ObjectDDL(%s, %q) = %q, want it to start with %q", kind, name, ddl, wantPrefix)
			}
		case errors.Is(err, ErrUnsupportedDDL):
			object, detailName, feature, ok := UnsupportedDDLDetail(err)
			t.Logf("ObjectDDL(%s, %q) is unavailable: object=%q name=%q feature=%q detailed=%v",
				kind, name, object, detailName, feature, ok)
		default:
			t.Errorf("ObjectDDL(%s, %q) error = %v, want nil or ErrUnsupportedDDL", kind, name, err)
		}
	}

	views, err := repository.DescribeViews(ctx)
	if err != nil {
		t.Fatalf("DescribeViews() error = %v", err)
	}
	tables, err := repository.SchemaTables(ctx)
	if err != nil {
		t.Fatalf("SchemaTables() error = %v", err)
	}
	viewNames := map[string]bool{}
	for _, view := range views {
		viewNames[view.Name] = true
	}
	for _, name := range tables[""] {
		if viewNames[name] {
			continue
		}
		assertRenderedOrExplained(t, ObjectKindTable, name, "CREATE TABLE")
		break
	}

	procedures, err := repository.DescribeProcedures(ctx)
	if err != nil {
		t.Fatalf("DescribeProcedures() error = %v", err)
	}
	if len(procedures) > 0 {
		assertRenderedOrExplained(t, ObjectKindProcedure, procedures[0].Name, "CREATE PROCEDURE")
	}

	if _, err := repository.ObjectDDL(ctx, ObjectKindTable, "SQLS_NO_SUCH_TABLE_XYZ"); !errors.Is(err, ErrObjectNotFound) {
		t.Errorf("ObjectDDL(table, unknown) error = %v, want ErrObjectNotFound", err)
	}
}

func TestInterBaseLiveExplainPlan(t *testing.T) {
	repository := interBaseLiveRepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plan, err := repository.ExplainPlan(ctx, "SELECT RDB$RELATION_ID FROM RDB$DATABASE")
	if err != nil {
		t.Fatalf("ExplainPlan() error = %v", err)
	}
	if strings.TrimSpace(plan) == "" {
		t.Fatal("ExplainPlan() returned empty plan text")
	}
	t.Logf("plan: %s", plan)
}
```

Add `"errors"` and `"strings"` to `interbase_live_test.go`'s imports if they are not already there; `context`, `os`, `time`, `testing` and `dialect` are.

- [ ] **Step 2: Run the untagged absence test to verify it fails**

Run: `go test ./internal/database/ -count=1 -run 'ExplainRepositoryAbsentWithoutNativeBuild' -v`

Expected: PASS immediately — nothing implements `ExplainPlan` yet, so the absence already holds. This test is a guard against a later mistake (someone adding `ExplainPlan` to the untagged file), not a red-green cycle. Its value is that it fails loudly if the method is ever moved out of the tagged file.

Run: `CGO_ENABLED=1 go test -tags interbase ./internal/database/ -count=1 -run 'ExplainRepositoryPresentWithNativeBuild' -v`
Expected: FAIL — `*InterBaseDBRepository must implement ExplainRepository under the interbase build tag`.

- [ ] **Step 3: Implement `ExplainPlan` in the tagged file**

Append to `internal/database/interbase_native.go`:

```go
var _ ExplainRepository = (*InterBaseDBRepository)(nil)

// ExplainPlan returns the server's query plan without executing the
// statement's result set. It acquires its own *sql.Conn because the plan is
// read from the prepared statement on that connection, and it returns the plan
// text unchanged.
//
// This is an interactive one-shot outside any cache build, so it uses the
// pooled *sql.DB rather than a catalog snapshot.
func (db *InterBaseDBRepository) ExplainPlan(ctx context.Context, query string) (string, error) {
	if db == nil || db.Conn == nil {
		return "", errors.New("interbase: database connection is nil")
	}
	conn, err := db.Conn.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	plan, err := interbase.Plan(ctx, conn, query)
	if err != nil {
		return "", fmt.Errorf("interbase: explain plan: %w", err)
	}
	return plan, nil
}
```

`context`, `errors`, `fmt` and `interbase` are already imported by that file.

- [ ] **Step 4: Run the tagged build and suite**

Run: `CGO_ENABLED=1 go build -tags interbase ./...`
Expected: no output, exit 0.

Run: `CGO_ENABLED=1 go test -tags interbase ./internal/database/ -count=1 -v`
Expected: PASS. The three live tests skip unless the `INTERBASE_*` variables are set; `TestExplainRepositoryPresentWithNativeBuild` runs unconditionally.

- [ ] **Step 5: Run the live suite where a server is available**

Run: `timeout 60s go test -tags interbase ./internal/database -run '^TestInterBaseLive' -count=1 -v -timeout=50s`

Expected: PASS, or `SKIP` when the environment variables are unset. Record the two elapsed times `TestInterBaseLiveCatalogObjectsSurface` logs: a primary pass above 5 seconds or a catalog pass above 30 seconds is the escalation trigger, and the response is a bulk projection added to the driver's `schema` package — a driver change, deliberately out of this plan's scope. Do **not** respond by re-introducing hand-written `RDB$` queries in sqls.

- [ ] **Step 6: Run the untagged suite**

Run: `go test ./...`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/database/interbase_native.go internal/database/interbase_stub_test.go internal/database/interbase_live_test.go
git commit -m "feat(database): expose InterBase query plans as a tagged capability"
```

---

## Task 11: README item 4 — metadata depth

Only item 4. Items 1 (dialects), 2 (connection keys), 3 (TLS) and 5 (single attachment) belong to plans 1 and 3; the section title and the "Dialect 3 is not supported" sentence are plan 1's to remove. Editing only one sentence here means the three plans can land in any order without conflicting edits to the same lines.

**Files:**
- Modify: `README.md:325-329` (the metadata paragraph)

**Interfaces:**
- Consumes: the behavior delivered by Tasks 4–9.
- Produces: no code.

- [ ] **Step 1: Replace the metadata sentence**

In `README.md`, the paragraph currently reads:

```
Completion and hover use user table/view, column, primary-key, and foreign-key
metadata. InterBase has no schema namespace or database enumeration through this
adapter, so switching databases is not supported; configure separate connections
instead. Dialect 1 `DATE` includes both date and time. Dialect 3 is not supported.
```

Replace **only its first sentence** with the following, leaving the rest of the paragraph untouched:

```
Completion and hover use tables, views, columns, and primary and foreign keys.
The cache also holds procedures with their parameters, triggers, generators,
domains, indexes, and external-function declarations, refreshed by the
background worker after the first connection rather than on demand. Object DDL
is reproduced from the catalog and is unavailable for external functions,
database files, shadows, tables with computed columns, and procedures whose
parameter nullability the catalog does not record; in those cases sqls shows
the cached summary and says why the DDL is missing.
```

- [ ] **Step 2: Verify the claims the sentence makes**

Every clause must be true of the code as committed, so check each one rather than trusting the draft:

Run: `grep -n 'ObjectKindFunction' internal/database/interbase_ddl.go`
Expected: the function case, which always reaches a refusing `GenerateDDL`.

Run: `grep -n 'GenerateCatalogCache' internal/database/worker.go`
Expected: the call inside the `case <-w.update:` body — the background worker, not a lazy per-object fetch.

Run: `grep -n 'DatabaseFile\|Shadow' /home/a.simard@multidev.local/gits/interbase-go/schema/ddl.go`
Expected: their `GenerateDDL` methods returning `ErrUnsupportedDDL`, which is where "database files, shadows" comes from.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: describe the InterBase metadata the cache now holds"
```

---

## Task 12: Trigger events and external-function types, from the driver's own decoders

**Do not start this task until the companion driver accessors exist.** They are specified and planned but not implemented:

- Spec: `/home/a.simard@multidev.local/gits/interbase-go/docs/superpowers/specs/2026-09-19-schema-catalog-accessors-design.md`
- Plan: `/home/a.simard@multidev.local/gits/interbase-go/docs/superpowers/plans/2026-09-19-schema-catalog-accessors.md`

Everything before this task ships with `TriggerDesc.Event`, `FunctionArgumentDesc.Type` and `FunctionDesc.ReturnType` rendering `""`, which is exactly the documented "undecodable" value, so no consumer breaks while this waits.

sqls reimplements none of these decoders. `triggerEvent` is unexported (`schema/ddl.go:871-913`) and there is no `FunctionArgument` renderer at all; copying either into sqls would guarantee drift.

**Files:**
- Modify: `internal/database/interbase_catalog.go` (`DescribeTriggers`, `DescribeFunctions`)
- Modify: `internal/database/interbase_catalog_test.go` (replace the placeholder test)

**Interfaces:**
- Consumes: `func (t schema.Trigger) Event() (string, error)`, `func (a schema.FunctionArgument) SQLType() (string, error)`, `func (f schema.Function) ReturnType() (string, error)`; `schema.ErrUnsupportedDDL`.
- Produces: `func interBaseOptionalRendering(rendered string, err error) (string, error)` — `("", nil)` when the error wraps `schema.ErrUnsupportedDDL`, `(rendered, nil)` on success, `("", err)` otherwise.

- [ ] **Step 1: Confirm the accessors exist**

Run: `grep -n 'func (t Trigger) Event\|func (a FunctionArgument) SQLType\|func (f Function) ReturnType' /home/a.simard@multidev.local/gits/interbase-go/schema/ddl.go`

Expected: three matches. If there are fewer, **stop and report**; the rest of this plan is complete and shippable without this task.

Run: `CGO_ENABLED=0 go build interbase-go/schema`
Expected: no output, exit 0.

- [ ] **Step 2: Replace the placeholder test with the real ones**

In `internal/database/interbase_catalog_test.go`, delete `TestInterBaseUndecodableFieldsAreEmptyUntilDriverAccessorsLand` and append:

```go
func TestInterBaseTriggerEventIsVerbatim(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}

	triggers, err := repository.DescribeTriggers(context.Background())
	if err != nil {
		t.Fatalf("DescribeTriggers() error = %v", err)
	}
	byName := make(map[string]*TriggerDesc, len(triggers))
	for _, trigger := range triggers {
		byName[trigger.Name] = trigger
	}

	if got, want := byName["CUSTOMER_BI"].Event, "BEFORE INSERT"; got != want {
		t.Errorf("CUSTOMER_BI event = %q, want %q", got, want)
	}
	// A multi-event trigger yields one joined string. sqls does not split it,
	// does not re-decode it, and specifies no parsing of it; a consumer that
	// wants the parts splits on " OR ".
	if got, want := byName["CUSTOMER_MULTI"].Event, "BEFORE INSERT OR UPDATE"; got != want {
		t.Errorf("CUSTOMER_MULTI event = %q, want the joined string %q", got, want)
	}

	// An undecodable trigger type and a NULL trigger type both yield "" with
	// the rest of the descriptor intact: ErrUnsupportedDDL from these
	// accessors is a normal result, not a failure.
	for _, name := range []string{"CUSTOMER_ODD", "DB_CONNECT"} {
		trigger := byName[name]
		if trigger == nil {
			t.Fatalf("DescribeTriggers() did not return %s", name)
		}
		if trigger.Event != "" {
			t.Errorf("%s event = %q, want an empty string", name, trigger.Event)
		}
		if trigger.Name != name || !trigger.Source.Valid || !trigger.Active.Valid {
			t.Errorf("%s lost surrounding metadata: %#v", name, trigger)
		}
	}
	if len(triggers) != 4 {
		t.Errorf("DescribeTriggers() returned %d triggers, want 4: an undecodable event must not drop the trigger", len(triggers))
	}
}

func TestInterBaseFunctionArgumentTypeDegradesToEmpty(t *testing.T) {
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}

	// DescribeFunctions must return a nil error: the degradation rule is
	// asserted, not assumed.
	functions, err := repository.DescribeFunctions(context.Background())
	if err != nil {
		t.Fatalf("DescribeFunctions() error = %v, want nil despite an unrenderable argument", err)
	}
	if len(functions) != 1 {
		t.Fatalf("DescribeFunctions() returned %d functions, want 1", len(functions))
	}
	function := functions[0]

	byPosition := make(map[int64]*FunctionArgumentDesc, len(function.Arguments))
	for _, argument := range function.Arguments {
		byPosition[argument.Position.Int64] = argument
	}
	if len(byPosition) != 3 {
		t.Fatalf("function has %d arguments, want 3: an unrenderable argument must not be dropped", len(byPosition))
	}

	// CSTRING renders from RDB$FIELD_LENGTH, which is the shape every measured
	// production row has: of 357 arguments across three production databases,
	// 166 were CSTRING and RDB$CHARACTER_LENGTH was NULL in all 357.
	if got, want := byPosition[1].Type, "CSTRING(255)"; got != want {
		t.Errorf("CSTRING argument type = %q, want %q", got, want)
	}
	// CHAR and VARCHAR arguments render "" permanently, not just until the
	// accessors land: RDB$CHARACTER_LENGTH is never populated for function
	// arguments, so the renderer has no length to declare. Measured exposure:
	// 1 CHAR and 0 VARCHAR arguments in 357.
	if got := byPosition[2].Type; got != "" {
		t.Errorf("CHAR argument type = %q, want an empty string: no character length is available", got)
	}
	if got, want := byPosition[3].Type, "INTEGER"; got != want {
		t.Errorf("INTEGER argument type = %q, want %q", got, want)
	}

	// The surrounding descriptor is still fully populated.
	if function.Name != "F_LTRIM" || !function.ModuleName.Valid || !function.EntryPoint.Valid {
		t.Errorf("the surrounding FunctionDesc lost metadata: %#v", function)
	}
	if byPosition[2].Name == "" {
		t.Error("the unrenderable argument lost its name")
	}
}

func TestInterBaseFunctionReturnTypeUsesDriverResolution(t *testing.T) {
	// RDB$RETURN_ARGUMENT holds an argument POSITION, not an index into
	// Arguments. The fixture's return argument is position 1, which is also an
	// input argument, so the same argument legitimately appears both in
	// Arguments and as ReturnType. This guards against indexing Arguments by
	// ReturnArgument and against lifting the return out of the list.
	db := openInterBaseSchemaFixture(t)
	repository := &InterBaseDBRepository{Conn: db, SQLDialect: 3}

	functions, err := repository.DescribeFunctions(context.Background())
	if err != nil {
		t.Fatalf("DescribeFunctions() error = %v", err)
	}
	function := functions[0]

	if !function.ReturnPosition.Valid || function.ReturnPosition.Int64 != 1 {
		t.Fatalf("return position = %#v, want 1", function.ReturnPosition)
	}
	if got, want := function.ReturnType, "CSTRING(255)"; got != want {
		t.Errorf("return type = %q, want the rendering of argument 1, %q", got, want)
	}

	// Indexing Arguments[1] would have picked position 2, the CHAR argument,
	// and produced "" here — so this assertion is what distinguishes the two
	// implementations.
	appearances := 0
	for _, argument := range function.Arguments {
		if argument.Position.Valid && argument.Position.Int64 == 1 {
			appearances++
		}
	}
	if appearances != 1 {
		t.Errorf("the return argument appears %d times in Arguments, want exactly 1", appearances)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/database/ -count=1 -run 'TriggerEventIsVerbatim|FunctionArgumentTypeDegradesToEmpty|FunctionReturnTypeUsesDriverResolution' -v`

Expected: FAIL, with the placeholder's empty strings showing through:

- `CUSTOMER_BI event = "", want "BEFORE INSERT"`
- `CSTRING argument type = "", want "CSTRING(255)"`
- `return type = "", want the rendering of argument 1, "CSTRING(255)"`

- [ ] **Step 4: Populate the three fields**

In `internal/database/interbase_catalog.go`, add the helper:

```go
// interBaseOptionalRendering applies the degradation rule the catalog
// accessors need. ErrUnsupportedDDL from Trigger.Event,
// FunctionArgument.SQLType and Function.ReturnType is a normal, expected
// result rather than a failure — RDB$CHARACTER_LENGTH is never populated for
// function arguments, so every CHAR and VARCHAR argument reaches it during
// ordinary operation. It maps to "" for that one field and the catalog build
// carries on: it never aborts, never propagates to
// DescribeFunctions/DescribeTriggers, and never drops the surrounding
// descriptor. Any other error is a real catalog fault and is returned.
func interBaseOptionalRendering(rendered string, err error) (string, error) {
	if err == nil {
		return rendered, nil
	}
	if errors.Is(err, schema.ErrUnsupportedDDL) {
		return "", nil
	}
	return "", err
}
```

In `DescribeTriggers`, replace the `Event: ""` line and the surrounding loop body:

```go
	for _, trigger := range triggers {
		event, err := interBaseOptionalRendering(trigger.Event())
		if err != nil {
			return nil, err
		}
		result = append(result, &TriggerDesc{
			Schema:       "",
			Name:         trigger.Name,
			RelationName: trigger.RelationName,
			// Taken verbatim: a multi-event trigger yields one joined string
			// such as "BEFORE INSERT OR UPDATE". sqls does not split it.
			Event:       event,
			Sequence:    trigger.Sequence,
			Active:      interBaseFlagIsClear(trigger.Inactive),
			Source:      trigger.Source,
			Description: trigger.Description,
		})
	}
```

In `DescribeFunctions`, replace the two `""` renderings:

```go
	for _, function := range functions {
		arguments := make([]*FunctionArgumentDesc, 0, len(function.Arguments))
		for _, argument := range function.Arguments {
			argumentType, err := interBaseOptionalRendering(argument.SQLType())
			if err != nil {
				return nil, err
			}
			arguments = append(arguments, &FunctionArgumentDesc{
				Name:     argument.Name,
				Position: argument.Position,
				Type:     argumentType,
			})
		}
		// RDB$RETURN_ARGUMENT is an argument position, not an index into
		// Arguments; the driver resolves it, so sqls never indexes the slice
		// with it. When the position names an input argument, that argument
		// legitimately appears both here and in Arguments.
		returnType, err := interBaseOptionalRendering(function.ReturnType())
		if err != nil {
			return nil, err
		}
		result = append(result, &FunctionDesc{
			Schema:         "",
			Name:           function.Name,
			ReturnType:     returnType,
			ReturnPosition: function.ReturnArgument,
			Arguments:      arguments,
			ModuleName:     function.ModuleName,
			EntryPoint:     function.EntryPoint,
			Description:    function.Description,
		})
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/database/ -count=1 -run 'TriggerEventIsVerbatim|FunctionArgumentTypeDegradesToEmpty|FunctionReturnTypeUsesDriverResolution' -v`

Expected: PASS. If `CHAR argument type` comes back as `CHAR(10)` rather than `""`, the driver substituted `FieldLength` for the missing character length — that is a defect in the driver's accessor, not here, and it must be fixed there rather than by relaxing this assertion.

- [ ] **Step 6: Run the whole suite**

Run: `go test ./...`
Expected: every package `ok`.

Run: `CGO_ENABLED=1 go test -tags interbase ./...`
Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/database/interbase_catalog.go internal/database/interbase_catalog_test.go
git commit -m "feat(database): decode trigger events and external-function types through schema"
```

---

## Spec coverage map

Where each part of this plan's scope is implemented, for a reviewer checking the plan against the spec rather than reading it end to end.

| Spec | Task |
| --- | --- |
| §4.3 placement outside the build tag | 3 (Step 1 verifies it) |
| §4.3 repository struct, `SchemaTables`, `DescribeDatabaseTable*`, `DescribeForeignKeysBySchema` | 4 |
| §4.3 deletion of the three queries and their row parsers | 4 |
| §4.3 retention and re-sourcing of the four type helpers | 3 |
| §4.3 `interBaseTypeName` and its four rules | 3 |
| §4.3 one-line `ColumnDesc.Type` trimming | 3 |
| §4.3 `ColumnDesc` field mapping, including `Extra: "COMPUTED"` | 4 |
| §4.3 `CatalogSnapshotRepository` and one read per cache build | 8 |
| §4.4 capability interfaces, descriptors, sentinels, `UnsupportedDDLDetail` | 1 |
| §4.4 views, generators, domains, indexes | 5 |
| §4.4 procedures and the user-versus-system domain test | 6 |
| §4.4 triggers and external functions | 6 (placeholders), 12 (populated) |
| §4.4 `ObjectDDL`, the kind table, `ErrObjectNotFound`, the wrapper | 7 |
| §4.4 `ExplainPlan`, tagged only | 10 |
| §4.4 D8 capability mock as a distinct type | 9 |
| §4.5 `CatalogCache`, `DBCache.Catalog`, the fourteen accessors | 9 |
| §4.5 `GenerateCatalogCache` | 9 |
| §4.5 `setCatalogCache` and independent passes | 9 |
| §4.5 non-blocking update signal | 9 |
| §6 `interbase_catalog_test.go` suite | 2, 3, 4, 5, 6, 7, 8, 12 |
| §6 `capability_test.go` suite | 1, 6, 7, 8, 10 |
| §6 `cache_test.go` suite | 8, 9 |
| §6 live `TestInterBaseLiveCatalogObjectsSurface`, `…ObjectDDL`, `…ExplainPlan` | 10 |
| §7 README item 4 | 11 |

Two §6 tests are deliberately not here. `TestInterBaseCurrentDatabaseAndDatabases` is owned by plan 3 (see the File Structure note); its inert-behavior assertions are preserved inside Task 4's migrated test so the branch stays covered. `TestExplainRepositoryAbsentWithoutNativeBuild` moved from `capability_test.go` to `interbase_stub_test.go`, because an untagged file is compiled under the tagged build too — explained in Task 10.
