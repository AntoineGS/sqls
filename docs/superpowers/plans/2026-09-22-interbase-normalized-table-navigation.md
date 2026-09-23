# InterBase Normalized Table Navigation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `gd` reach the correct table and column catalog entries when executable InterBase table DDL is unavailable, without misrepresenting reconstructed numeric types as original SQL.

**Architecture:** Keep strict `GenerateDDL` unchanged. Add a separate non-executable, catalog-backed table description with exact identifier byte spans and provenance for normalized legacy types; expose it through an optional InterBase repository capability. The definition handler tries strict DDL first and writes a commented informational snapshot only on `ErrUnsupportedDDL` for a confirmed table.

**Tech Stack:** Go, `interbase-go/schema`, `sqls/internal/database`, LSP UTF-16 ranges and source-snapshot store.

**Spec:** `docs/superpowers/specs/2026-09-22-interbase-catalog-ddl-accuracy-design.md`.

**Prerequisite:** Implement and verify `docs/superpowers/plans/2026-09-22-interbase-driver-catalog-identifiers.md` so `schema.Catalog.Table` returns complete names and columns.

## Global Constraints

- Executable `GenerateDDL` and `DDLRepository.ObjectDDL` remain strict and unchanged for legacy scaled DOUBLE.
- A normalized `NUMERIC(15, -scale)` for type 27/subtype 0/NULL precision is labeled reconstructed; print the recorded type, scale, subtype, and precision too.
- Snapshot content is read-only, plainly non-executable, and has direct byte-span mapping rather than synthetic `CREATE TABLE` parsing.
- Never synthesize a result on not-found, stale cache, ambiguous ownership, connection failure, timeout, or missing column.
- The example SQL file and configured databases are read-only; editor install/restart needs a separate request.

## Review Focus

- A `NUMERIC(15, 2)` normalized label does not silently become executable DDL (Task 1).
- A source table and another table with a shared 22-byte prefix cannot mix columns (Tasks 1 and 2).
- Unicode and quoted identifiers map to the correct UTF-16 range inside the snapshot (Task 3).
- Catalog I/O errors and stale cache do not produce a fabricated snapshot (Task 3).
- Existing view/procedure source fallback and successful table DDL keep their current behavior (Task 3).

## File map

- `../interbase-go/schema/table_description.go` (new): pure renderer of `schema.Relation` to a commented, non-executable catalog description with byte spans and type provenance.
- `../interbase-go/schema/table_description_test.go` (new): legacy numeric, trustworthy type, quoted name, and unknown metadata cases.
- `internal/database/capability.go`: optional table-description repository interface and neutral description/span value types.
- `internal/database/interbase_description.go` (new): retrieve the exact-name table and convert the schema description into the capability value.
- `internal/database/interbase_catalog.go`: reuse the existing normalized legacy numeric rule rather than creating a conflicting LSP-only type mapping.
- `internal/handler/interbase_relation_definition.go`: handle an unsupported strict DDL result via the optional description capability and write a comment-only snapshot; use returned spans for table and column ranges.
- `internal/handler/interbase_relation_definition_test.go`, `internal/database/interbase_catalog_test.go`: capability and LSP regressions.

---

### Task 1: A truthful non-executable schema description

**Files:** Create `../interbase-go/schema/table_description.go`, `../interbase-go/schema/table_description_test.go`.

**Interfaces:** Produce `schema.CatalogDescription{Body string, Table schema.DescriptionSpan, Columns []schema.DescriptionColumn}`; `DescriptionSpan{Start,End int}` indexes bytes in `Body`, and `DescriptionColumn{Name string, Span DescriptionSpan}` keeps catalog order. Implement `func (r Relation) DescribeCatalog() (CatalogDescription, error)` independently of `GenerateDDL`, and `func (d Domain) CatalogTypeLabel() (label string, normalized bool)` for display-only type rendering.

- [ ] **Step 1: Add a failing test** for a relation `IMPORT_ORDER_PAYMENT` with an `AMOUNT` domain carrying type 27, scale -2, subtype 0, and NULL precision. Require a comment-only body, exact table/column span substrings, recorded raw metadata, and `NUMERIC(15, 2)` explicitly marked `normalized legacy display`. Add a domain with real subtype/precision and one unknown type: the first displays recorded values, the second displays raw metadata without a fabricated SQL type. Assert `GenerateDDL` still returns `ErrUnsupportedDDL` for the legacy domain.

```go
got, err := relation.DescribeCatalog()
if err != nil { t.Fatal(err) }
if got.Body[got.Table.Start:got.Table.End] != `"IMPORT_ORDER_PAYMENT"` ||
   !strings.Contains(got.Body, "normalized legacy display") ||
   !strings.Contains(got.Body, "precision=NULL") { t.Fatalf("description = %+v", got) }
```

- [ ] **Step 2: Run** `go test ./schema -run 'TestRelationDescribeCatalog' -count=1` in `interbase-go`; expect missing method.
- [ ] **Step 3: Implement** a comment-only renderer using `Relation.Columns` and each `Column.Domain`'s recorded fields. Quote table/column names exactly as the strict renderer does; while appending a name to a `strings.Builder`, record its byte start/end. `Domain.CatalogTypeLabel` returns normalized `NUMERIC(15, -scale), true` only for type 27 with negative scale and no subtype/precision; for known type metadata it returns `Domain.SQLType()`'s label with `false`, and for unsupported types it returns an empty label with `false`. The renderer adds raw metadata and provenance in all cases, and never invents syntax for an empty label. Reject an empty table name, a missing column list, or duplicate exact column identities.
```go
func (d Domain) CatalogTypeLabel() (string, bool) {
    if d.FieldType.Valid && d.FieldType.Int64 == fieldTypeDouble &&
        d.FieldScale.Valid && d.FieldScale.Int64 < 0 &&
        (!d.FieldSubType.Valid || d.FieldSubType.Int64 == 0) && !d.FieldPrecision.Valid {
        return fmt.Sprintf("NUMERIC(15, %d)", -d.FieldScale.Int64), true
    }
    label, err := d.SQLType()
    if err != nil { return "", false }
    return label, false
}
```

- [ ] **Step 4: Run** `go test ./schema -count=1` and `go test ./... -count=1` in `interbase-go`.
- [ ] **Step 5: Commit** in `interbase-go` with `feat(schema): expose non-executable table catalog description`.

### Task 2: Optional repository capability

**Files:** Modify `internal/database/capability.go`, `internal/database/interbase_catalog_test.go`; create `internal/database/interbase_description.go`.

**Interfaces:** Produce `TableDescriptionRepository` with `TableDescription(ctx context.Context, name string) (TableDescription, error)`; neutral value types `TableDescription{Body string, Table DescriptionSpan, Columns []DescriptionColumn}` and `DescriptionSpan{Start,End int}`, `DescriptionColumn{Name string, Span DescriptionSpan}`. Define `copyCatalogDescription(d schema.CatalogDescription) TableDescription` for the mapping from Task 1's schema types; `NewInterBaseDBRepository` implements the optional interface. Mock repositories lacking it remain valid.

- [ ] **Step 1: Add a failing test** against the existing InterBase catalog fixture: exact requested table name yields a description with all ordered columns; a nonexistent name returns `ErrObjectNotFound` with no body; a first-22-character name collision cannot return another table's description. Include an incompatible/canceled context case that returns an error and no body.
- [ ] **Step 2: Run** `go test ./internal/database -run 'TestInterBaseTableDescription' -count=1`; expect missing capability.
- [ ] **Step 3: Implement** `TableDescription` with `schema.New(db.Conn).Table(ctx,name)`, verify `relation.Name == name` after the read, call `relation.DescribeCatalog()`, and copy its spans to neutral database types. Return `ErrObjectNotFound` only for an absent exact object; propagate catalog/renderer errors. In `interBaseColumnType`, call Task 1's `domain.CatalogTypeLabel()` for scaled type 27 before the existing switch, so its normalized `NUMERIC(15, -scale)` rule has one source. Do not change `Domain.SQLType` or pretend the result is strict DDL.
```go
func (db *InterBaseDBRepository) TableDescription(ctx context.Context, name string) (TableDescription, error) {
    relation, err := schema.New(db.Conn).Table(ctx, name)
    if err != nil { return TableDescription{}, err }
    if relation == nil || relation.Name != name { return TableDescription{}, ErrObjectNotFound }
    rendered, err := relation.DescribeCatalog()
    if err != nil { return TableDescription{}, err }
    return copyCatalogDescription(rendered), nil // maps ordered schema spans to neutral spans
}
```

- [ ] **Step 4: Run** `go test ./internal/database -count=1` and `go test ./... -count=1` in `sqls`.
- [ ] **Step 5: Commit** the capability and tests in `sqls` with `feat(interbase): expose exact catalog table descriptions`.

### Task 3: Definition fallback and end-to-end acceptance

**Files:** Modify `internal/handler/interbase_relation_definition.go`, `internal/handler/interbase_relation_definition_test.go`; extend `internal/sqlsymbol/acceptance_test.go` only if its existing read-only file hook applies.

**Interfaces:** Consume Task 2's `database.TableDescriptionRepository`; use the same `definitionDDLTimeout`, `renderSnapshot`, `sourceSnapshotStore.write`, `snapshotBodyOffset`, `symbolRange`, and `snapshotURI` as the existing definition path.

- [ ] **Step 1: Add failing handler tests** for `gd` on full table `IMPORT_ORDER_LINE_ITEMS` and on `IMPORT_ORDER_PAYMENT.AMOUNT` when mocked `ObjectDDL` returns `ErrUnsupportedDDL`. Have the optional capability return comment-only description and spans; assert the snapshot contains the full table name, the normalized type note, and the returned UTF-16 range selects exactly the requested quoted name. Add negative cases for `ErrObjectNotFound`, offline error, canceled context, absent capability and ambiguous column ownership. Existing successful-`CREATE TABLE` and view tests must remain green.
- [ ] **Step 2: Run** `go test ./internal/handler -run 'TestInterBaseRelationDefinition' -count=1`; expect no fallback locations before implementation.
- [ ] **Step 3: In the existing definition path**, only on `errors.Is(ddlErr, database.ErrUnsupportedDDL)` for `ObjectKindTable`, type-assert `TableDescriptionRepository`, fetch the exact table with the remaining timeout context, and map the resolved column to one exact description column. Prefix a banner note explaining strict DDL refusal and that the body is informational; let `renderSnapshot` comment the note. Convert the selected body byte span to a content span using `snapshotBodyOffset`, then use `symbolRange` for UTF-16. Never call `TableDeclaration`/`ColumnDeclaration` on description text. Keep existing success and error branches otherwise unchanged.
```go
if target.kind == database.ObjectKindTable && errors.Is(ddlErr, database.ErrUnsupportedDDL) {
    source, hasDescription := repo.(database.TableDescriptionRepository)
    if !hasDescription { return nil, nil }
    description, descriptionErr := source.TableDescription(ddlCtx, target.name)
    if descriptionErr != nil { return nil, nil }
    // Find target.column in description.Columns by exact catalog identity,
    // render the informational body, and offset its recorded byte span.
}
```

- [ ] **Step 4: Run** `go test ./internal/handler ./internal/database ./internal/sqlsymbol -count=1`, `go test ./... -count=1`, InterBase-tagged tests, and native `go build` from `sqls` with the locally replaced driver. Run the optional read-only acceptance against the user's SQL file and confirm the two named locations; do not install the build in the active Neovim session. Check both repositories with `git status --short`.
- [ ] **Step 5: Commit** the handler and acceptance tests in `sqls` with `fix(interbase): navigate table metadata when strict DDL is unavailable`.

**Completion gate:** A whole-change review checks driver/catalog identity, comment-only description spans, error boundaries, and strict DDL preservation. Report the built artifact and read-only verification separately from any editor rollout.
