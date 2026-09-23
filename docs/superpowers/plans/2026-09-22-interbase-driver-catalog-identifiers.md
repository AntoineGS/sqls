# InterBase Driver Catalog Identifiers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Return full, unambiguous catalog identifiers from `interbase-go/schema`, including 23+ character table names and colliding constraint prefixes.

**Architecture:** Resolve identifier byte widths from `RDB$RELATION_FIELDS`/`RDB$FIELDS` once per catalog reader. Render result-only `CAST(... AS VARCHAR(width))` projections for catalog identifiers, preserving original JOIN/WHERE/ORDER expressions and scan ordinals. Fail visibly if a width is unavailable or cannot be represented; never match truncated names.

**Tech Stack:** Go `database/sql`, `interbase-go/schema`, SQL fixture driver, native InterBase read-only acceptance.

**Spec:** `../sqls/docs/superpowers/specs/2026-09-22-interbase-catalog-ddl-accuracy-design.md` (paths in this plan are relative to `../interbase-go`).

## Global Constraints

- No writes to user databases or the SQL example file; live acceptance is read-only.
- Keep `Catalog`'s exact-name/case contract and its public methods' return types.
- Use catalog-derived byte widths, not an unverified universal 67-byte limit.
- Preserve original JOIN, WHERE, ORDER BY and scan ordinals; cast only returned identifier values.
- Reject absent/invalid widths and SQL width errors rather than returning partial metadata.

## Review Focus

- Two relation names with the same 22-character prefix remain distinct through `Table` and `Columns` (Task 2).
- Missing width metadata aborts the read; it never creates a false empty relation (Task 1).
- Identifier fields of nullable LEFT JOINs remain nullable after CAST (Task 3).
- Case-sensitive exact-name lookup stays exact for mixed-case quoted names (Task 2).
- Different catalog byte widths produce corresponding VARCHAR projections; unsupported widths fail (Task 1).

## File map

- `schema/catalog_identifiers.go` (new): read/validate width metadata and render identifier projection expressions, using the catalog reader's existing `Queryer`.
- `schema/schema.go`: use those projections for core relation, column, domain and procedure reads; keep source ordinals.
- `schema/catalog_extended.go`: use the same helper for extended catalog objects, constraints, aliases, and nullable joins.
- `schema/schema_test.go`, `schema/extended_schema_test.go`: fixture SQL and identity regressions.
- `schema/catalog_identifiers_test.go` (new): width validation and projection tests.

---

### Task 1: Width-aware identifier projection

**Files:** Create `schema/catalog_identifiers.go`, `schema/catalog_identifiers_test.go`; modify the fixture query dispatcher in `schema/schema_test.go` only as required to answer the new width query.

**Interfaces:** Consume `Catalog.queryer.QueryContext(ctx, query, args...)`. Produce `(*Catalog).identifierWidths(ctx) (map[identifierField]int, error)` and `catalogIdentifier(ref string, width int) (string, error)`, where `identifierField{relation, field string}` identifies a catalog column; callers keep using the same `Catalog` and positional scans. Use a mutex or `sync.Once` to keep a single consistent loaded width map per catalog instance.

- [ ] **Step 1: Write a failing fixture test** that returns `RDB$RELATION_NAME` width 67 and `RDB$FIELD_NAME` width 31 and asserts the two rendered projections differ, with the original filter still uncast. Include `RDB$RELATION_CONSTRAINTS` (longer than 22 characters) in the width-map rows so the bootstrap itself cannot truncate its key. Also return zero, NULL, and a width rejected by the database in separate cases and assert errors; no silently substituted width.

```go
want := `CAST(r.RDB$RELATION_NAME AS VARCHAR(67))`
if got != want { t.Fatalf("projection = %q", got) }
// An absent identifierField or a non-positive width must return an error.
```

- [ ] **Step 2: Run** `go test ./schema -run 'TestCatalogIdentifier' -count=1`; expect a missing helper or failed width assertion.
- [ ] **Step 3: Implement** two numeric-only bootstrap lookups: the declared byte widths of `RDB$RELATIONS.RDB$RELATION_NAME` and `RDB$RELATION_FIELDS.RDB$FIELD_NAME`, filtered with bound short catalog literals. Then run one metadata query joining `RDB$RELATION_FIELDS rf` with `RDB$FIELDS f` on `rf.RDB$FIELD_SOURCE = f.RDB$FIELD_NAME`; SELECT the relation and field names via `CAST(... AS VARCHAR(bootstrapWidth))` and `f.RDB$FIELD_LENGTH`. This prevents the width map itself truncating `RDB$RELATION_CONSTRAINTS`. Trim fixed catalog padding at this boundary. Quote no user identifiers into SQL. `catalogIdentifier` formats `CAST(%s AS VARCHAR(%d))`; callers supply only hard-coded, audited query expressions. Propagate a failed width read and let the database reject an unsupported SQL cast rather than invent a max length.
```go
func catalogIdentifier(ref string, width int) (string, error) {
    if width <= 0 { return "", fmt.Errorf("schema: invalid catalog identifier width %d", width) }
    return fmt.Sprintf("CAST(%s AS VARCHAR(%d))", ref, width), nil
}
// Bootstrap the two known numeric field lengths before projecting the width
// map's relation and field names as VARCHAR values.
```

- [ ] **Step 4: Run** the focused test, then `go test ./schema -count=1`. Update fixture dispatch solely to serve the new metadata query while retaining its strict query-shape checks.
- [ ] **Step 5: Commit** in `interbase-go`: `git add schema/catalog_identifiers.go schema/catalog_identifiers_test.go schema/schema_test.go && git commit -m 'fix(schema): derive catalog identifier projection widths'`.

### Task 2: Full identities in core catalog accessors

**Files:** Modify `schema/schema.go`, `schema/schema_test.go`; use Task 1's helper.

**Interfaces:** `Catalog.Tables`, `Catalog.Table`, `Catalog.Columns`, `Catalog.Domains`, `Catalog.Procedures`, and procedure parameter accessors keep their public signatures. Query assembly uses `identifierWidths(ctx)` and `catalogIdentifier` before calling `c.query`.

- [ ] **Step 1: Add a failing fixture** containing `IMPORT_ORDER_LINE_ITEMS` and another full table name sharing its first 22 characters; give each different columns and assert `Table(ctx, fullName)` resolves only its own columns. Include a quoted mixed-case identifier and exact-case miss. Assert SQL projections cast returned identifiers while `WHERE ... = ?`, joins, and `ORDER BY` remain on original catalog columns.

```go
got, err := New(db).Table(ctx, "IMPORT_ORDER_LINE_ITEMS")
if err != nil || got == nil || got.Name != "IMPORT_ORDER_LINE_ITEMS" || len(got.Columns) == 0 {
    t.Fatalf("Table(full name) = %#v, %v", got, err)
}
```

- [ ] **Step 2: Run** `go test ./schema -run 'TestCatalogFullIdentifiers|TestCatalogProjectionAndOrderingContracts' -count=1`; expect uncast-SELECT assertion failure.
- [ ] **Step 3: Change core SELECT projections** of catalog name columns in `schema.go` to `CAST(... AS VARCHAR(width)) AS existing_alias`; leave numeric, source text, filters, and positional scans intact. Preserve nullable values and `Rows.Err()` handling. Update exact-SQL fixture assertions to require the new projections and still forbid altered predicates/order.
```go
relationName, err := catalogIdentifier("r.RDB$RELATION_NAME", widths[identifierField{"RDB$RELATIONS", "RDB$RELATION_NAME"}])
if err != nil { return nil, err }
// SELECT relationName AS RDB$RELATION_NAME, ...
// WHERE r.RDB$RELATION_NAME = ? ORDER BY r.RDB$RELATION_NAME
```

- [ ] **Step 4: Run** `go test ./schema -count=1` and `go test ./... -count=1` in `interbase-go`.
- [ ] **Step 5: Commit** `schema/schema.go schema/schema_test.go` with `fix(schema): preserve complete core catalog names`.

### Task 3: Extended catalog identity and live parity

**Files:** Modify `schema/catalog_extended.go`, `schema/extended_schema_test.go`; use Task 1's helper.

**Interfaces:** All extended catalog accessors preserve their current signatures; the full name read by a relation/constraint/index/trigger/charset projection must be the name used to pair later catalog rows.

- [ ] **Step 1: Add failing tests** for distinct long constraint names sharing a 22-character prefix, index segments attached to the correct constraint, nullable `pc.RDB$RELATION_NAME` in a LEFT JOIN, and a long character-set/collation name. Assert absence remains SQL NULL, never `""` or a mispaired object.
- [ ] **Step 2: Run** `go test ./schema -run 'TestExtendedCatalogFullNames' -count=1`; expect assertion failure in uncast projected identifiers.
- [ ] **Step 3: Project every returned catalog identifier** in `catalog_extended.go` through the shared width map, including joined relation names, field names, constraint/index names, sequence/procedure/trigger names and dependency/privilege object names. Preserve original scan order and aliases. SQL text sources, descriptions, database file paths, user names and other non-catalog identifiers keep their existing semantics.
```go
constraintName, err := catalogIdentifier("c.RDB$CONSTRAINT_NAME", widths[identifierField{"RDB$RELATION_CONSTRAINTS", "RDB$CONSTRAINT_NAME"}])
if err != nil { return nil, err }
// SELECT constraintName AS RDB$CONSTRAINT_NAME, ...;
// JOIN/WHERE still use c.RDB$CONSTRAINT_NAME.
```

- [ ] **Step 4: Run** `go test ./schema -count=1`, `go test ./... -count=1`, and the repo's existing read-only live catalog acceptance against the configured InterBase connection. Compare full `Catalog.Table(ctx, "IMPORT_ORDER_LINE_ITEMS")` identity and its nonempty ordered columns with an independent `CAST(... AS VARCHAR(...))` SELECT; compare the distinct long constraint names. Do not issue DDL or DML to the user's database.
- [ ] **Step 5: Commit** the extended reader and tests with `fix(schema): preserve extended catalog identifier identity`.

**Completion gate:** Review the three commits, run `git status --short` in both repositories, and verify the driver catalog's long-name lookup independently of `sqls`. Do not install a binary or restart Neovim during this plan.
