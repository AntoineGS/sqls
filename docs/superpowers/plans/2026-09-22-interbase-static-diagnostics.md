# InterBase Static Diagnostics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish InterBase hints for unused procedure locals/inputs and warnings for statically possible string truncation in procedure assignments, UPDATE, INSERT VALUES and INSERT SELECT.

**Architecture:** Add a conservative read/write and type-aware diagnostic pass to the existing `internal/sqlsymbol` binder. Provide it with a catalog snapshot through a small interface, then convert its span findings into versioned LSP push diagnostics in the handler. Unsupported or ambiguous syntax yields no speculative warning.

**Tech Stack:** Go; existing lexer, SQL symbol analyzer, database cache and Sourcegraph jsonrpc2; Go tests.

**Spec:** `docs/superpowers/specs/2026-09-22-interbase-static-diagnostics-design.md`

## Global Constraints

- InterBase first; other dialects get no InterBase diagnostics.
- No database execution or prepare in the edit path; read cached catalog only.
- Unused locals/inputs are Hints (severity 4); output parameters exempt. Potential width mismatch is Warning (severity 2).
- Widths represent maximum characters, not bytes; unknown width/ownership is silent.
- Ranges are UTF-16; delayed computations cannot overwrite newer edits or connection generations.

## Review Focus

- An assignment-only local remains unused; a genuine read clears the hint (Task 1).
- An unresolved/ambiguous occurrence does not cause a false unused hint (Task 1).
- A quoted supplementary Unicode rune and doubled apostrophe have correct literal width (Task 2).
- An ambiguous unqualified joined column does not produce a width warning (Task 3).
- A late publication after a newer edit or connection switch never restores stale warnings (Task 4).

## File map and interfaces

- `internal/sqlsymbol/procedure.go`, `resolve.go`: retain declaration type spans and classify local read/write occurrences without changing navigation behavior.
- `internal/sqlsymbol/diagnostic.go`, `diagnostic_test.go`: semantic findings and unused rule; `Analysis.Diagnostics(c Catalog) []Finding` where `Finding` has `Span`, `Code`, `Message`, `Severity` (2 warning, 4 hint); `Catalog` exposes table column names/types in declared order without importing handler code.
- `internal/sqlsymbol/width.go`, `width_test.go`: pure expression maximum-character-width inference, unknown represented explicitly.
- `internal/sqlsymbol/assignments.go`, `assignments_test.go`: tolerant assignment/projection pairing and catalog-backed destination/source resolution.
- `internal/handler/diagnostics.go`, `diagnostics_test.go`: snapshot text/version/dialect/cache; convert findings to LSP diagnostics, publish/clear and prevent stale results.
- `internal/handler/handler.go`, `internal/lsp/lsp.go`: version bookkeeping, document lifecycle hooks and notification payload.
- `README.md`: InterBase diagnostics usage and conservative supported rules.

---

### Task 1: Bound symbol types and unused read/write hints

**Files:** Modify `internal/sqlsymbol/procedure.go`, `internal/sqlsymbol/resolve.go`; create `internal/sqlsymbol/diagnostic.go`, `internal/sqlsymbol/diagnostic_test.go`.

**Interfaces:** Produce `type Finding struct { Span Span; Code, Message string; Severity int }`, `type Catalog interface { Columns(table Name) ([]ColumnType, bool) }`, `type ColumnType struct { Name string; Type string }`, `func (a *Analysis) Diagnostics(c Catalog) []Finding`. Preserve existing `Analyze`, `Resolve` and `References` signatures. Retain original SQL type tokens for each declaration as a type span or normalized type string; record occurrence read/write separately from `Symbol.Uses`.

- [ ] **Step 1: Write failing tests.** Add a table-driven test to `diagnostic_test.go` for an input parameter never read, an assigned-only local, a read local, an output parameter, an ambiguous same-name declaration, and a SQL occurrence that cannot be proven local. Assert `Finding.Code == "interbase-unused"`, severity 4 and the exact declaration span; verify existing reference counts stay intact. Example scenario:

```go
text := "CREATE PROCEDURE P (IN_NAME VARCHAR(40)) AS DECLARE VARIABLE OUT_NAME VARCHAR(20); BEGIN OUT_NAME = 'x'; END"
a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
if err != nil { t.Fatal(err) }
got := a.Diagnostics(nil)
// Both IN_NAME and assigned-only OUT_NAME get unused findings.
```

- [ ] **Step 2: Run red.** `go test ./internal/sqlsymbol -run 'TestDiagnosticUnused' -count=1`; expect failure because `Diagnostics` does not exist.
- [ ] **Step 3: Implement minimal read/write classification.** Parse declaration type until comma/closing header parenthesis or local semicolon, preserving nested type parentheses. Add `Reads []Span` to `Symbol`. For each bound occurrence, classify assignment LHS and INTO/RETURNING output target as writes; all proved value occurrences as reads. Track uncertain occurrences on the affected symbol so the unused rule suppresses false positives. Leave `Uses` unchanged. Add `Diagnostics` returning unused findings; define `Catalog` for the later width task, initially unused.

```go
type ColumnType struct { Name, Type string }
type Catalog interface { Columns(table Name) ([]ColumnType, bool) }
type Finding struct { Span Span; Code, Message string; Severity int }
func (a *Analysis) Diagnostics(c Catalog) []Finding {
    findings := make([]Finding, 0)
    for _, symbol := range a.Symbols {
        if symbol.Kind == OutputParameter || len(symbol.Reads) != 0 || symbol.RenameBlocked != "" { continue }
        findings = append(findings, Finding{Span: symbol.Declaration, Code: "interbase-unused", Message: "Unused declaration", Severity: 4})
    }
    return findings
}
```

- [ ] **Step 4: Run green and regressions.** `go test ./internal/sqlsymbol -count=1`; verify rename/reference tests still pass. Add a test for an unsupported, possibly reading occurrence; the hint must be suppressed.
- [ ] **Step 5: Commit.** `git add internal/sqlsymbol && git commit -m 'feat(interbase): detect unused procedure declarations'`.

### Task 2: Proven string expression width evaluator

**Files:** Create `internal/sqlsymbol/width.go`, `internal/sqlsymbol/width_test.go`; adjust `internal/sqlsymbol/procedure.go` only if declaration type extraction needs a fix.

**Interfaces:** Consume `Analysis`, `Catalog`, declaration type strings from Task 1. Produce `func stringTypeWidth(typeName string) (int, bool)` and `func (a *Analysis) expressionWidth(items []lexeme, c Catalog) (int, bool)` (the implementer may make expression token bounds explicit, but keep unknown distinct from zero). The catalog must not be queried over the network.

- [ ] **Step 1: Write failing table tests.** Assert `VARCHAR(40)` → 40, `CHAR(20) CHARACTER SET UTF8` → 20, `BLOB` → unknown, literal `'a''😀'` → 3 Unicode characters, `X || Y` → sum of widths when both known, `CAST(X AS VARCHAR(7))` → 7, bounded `SUBSTRING(X FROM 1 FOR 5)` → at most 5, `TRIM(X)` → at most X's width; an unknown function or ambiguous SQL column must return unknown. Include `SELECT` source alias and quoted identifier cases.

```go
width, ok := stringTypeWidth("VARCHAR(40)")
if !ok || width != 40 { t.Fatalf("width = %d, %v", width, ok) }
if _, ok := stringTypeWidth("BLOB"); ok { t.Fatal("unknown width treated as known") }
```

- [ ] **Step 2: Run red.** `go test ./internal/sqlsymbol -run 'TestStringTypeWidth|TestExpressionWidth' -count=1`; expect undefined width functions.
- [ ] **Step 3: Implement.** Reuse significant InterBase lexemes and `Analysis.Resolve` for sources. Match complete expression forms with balanced parentheses; sum concatenation bounds, cap substring by literal nonnegative length, propagate trim's upper bound, use cast result width. Decode doubled SQL quotes and count Unicode code points. Use existing SQL ownership plus `Catalog.Columns` to resolve columns; if more than one owner remains, return unknown. Never infer unknown function widths.
- [ ] **Step 4: Run green.** `go test ./internal/sqlsymbol -count=1`.
- [ ] **Step 5: Commit.** `git add internal/sqlsymbol && git commit -m 'feat(interbase): infer conservative string expression widths'`.

### Task 3: Assignment and projection matching

**Files:** Create `internal/sqlsymbol/assignments.go`, `internal/sqlsymbol/assignments_test.go`; modify `internal/sqlsymbol/diagnostic.go` to append width findings.

**Interfaces:** Consume `Catalog`, `ColumnType`, `Analysis.expressionWidth` and the declaration type widths from Tasks 1–2; produce `[]Finding` via `Analysis.Diagnostics`. Code `interbase-string-truncation`, severity 2. Findings must identify exact source/destination widths.

- [ ] **Step 1: Write failing tests.** Use an in-memory `Catalog` mapping `SRC.VALUE` to `VARCHAR(40)` and `DST.VALUE` to `VARCHAR(20)`. Test local `SMALL = LARGE`, SQL `UPDATE DST SET VALUE = SRC.VALUE` with resolvable scopes, `INSERT INTO DST (VALUE) VALUES ('abcdefghijklmnopqrstu')`, multiple columns and rows, `INSERT INTO DST (VALUE) SELECT SRC.VALUE FROM SRC`, concatenation/cast/trim/substr projections, and `SELECT ... INTO` local. Verify precise warning position and no warning for fitting literals, ambiguous joined columns, implicit target order, unsupported expressions or incomplete statements.

```go
catalog := testCatalog{columns: map[string][]ColumnType{
    "SRC": {{Name: "VALUE", Type: "VARCHAR(40)"}},
    "DST": {{Name: "VALUE", Type: "VARCHAR(20)"}},
}}
findings := a.Diagnostics(catalog)
if len(findings) != 1 || findings[0].Code != "interbase-string-truncation" {
    t.Fatalf("diagnostics = %+v", findings)
}
```

Define `testCatalog` in `assignments_test.go` with `Columns(table Name) ([]ColumnType, bool)` looking up `table.Key()` in `columns` (the test also exercises quoted versus unquoted name matching).

- [ ] **Step 2: Run red.** `go test ./internal/sqlsymbol -run 'TestWidthAssignments|TestInsertSelectWidths' -count=1`; expect missing truncation findings.
- [ ] **Step 3: Implement.** Walk statement boundaries and balanced lists instead of splitting on every comma (type/function arguments contain commas). Resolve LHS from the procedure binder or target relation catalog; pair INSERT explicit column list with each VALUES row / SELECT projection in order. Resolve UPDATE target only from its updated relation. For `SELECT ... INTO`, pair projections with local output targets when parsed completely. Skip each unsupported/ambiguous pair individually. Return warnings on target spans for assignment/UPDATE/VALUES and projection spans for INSERT SELECT.
- [ ] **Step 4: Run green.** `go test ./internal/sqlsymbol -count=1` and `go test ./internal/handler -run 'Test.*(Symbol|Rename|Definition)' -count=1`.
- [ ] **Step 5: Commit.** `git add internal/sqlsymbol && git commit -m 'feat(interbase): warn on statically wider assignments'`.

### Task 4: Versioned push diagnostics and catalog refresh

**Files:** Create `internal/handler/diagnostics.go`, `internal/handler/diagnostics_test.go`; modify `internal/handler/handler.go`, `internal/lsp/lsp.go`.

**Interfaces:** Consume `Analysis.Diagnostics(Catalog)`; adapt a copied `database.DBCache` to the catalog interface using catalog names and column order from available metadata. Produce `PublishDiagnosticsParams{URI string, Version *int, Diagnostics []Diagnostic}` and notify via `conn.Notify(ctx, "textDocument/publishDiagnostics", params)`.

- [ ] **Step 1: Write failing handler tests.** In the existing JSON-RPC test harness, capture publish notifications after didOpen/didChange/didClose; assert `sqls` source, stable codes, severities 4/2, correct UTF-16 ranges after an emoji, empty list after a fix/close, fresh cache after reconnect, and latest version wins when edits overlap. A non-InterBase connection publishes no InterBase findings.

```go
type PublishDiagnosticsParams struct {
    URI string `json:"uri"`
    Version *int `json:"version,omitempty"`
    Diagnostics []Diagnostic `json:"diagnostics"`
}
```

- [ ] **Step 2: Run red.** `go test ./internal/handler -run 'TestPublish.*Diagnostics' -count=1`; expect missing publications.
- [ ] **Step 3: Implement.** Track document versions under `stateMu`; snapshot text, version, dialect, cache pointer and connection generation before analysis. Revalidate snapshot/generation before notifying (serialize concurrent per-document publication if necessary). Publish on open/change/save-with-text; clear on close and connection switch; republish open documents on catalog refresh. Never hold `stateMu` during parsing, cache I/O or RPC notification. Convert spans with `symbolRange` and sort findings deterministically. On malformed edits skip affected findings instead of failing a document notification.
- [ ] **Step 4: Run green plus race check.** `go test ./internal/handler ./internal/lsp -count=1` and `go test -race ./internal/handler -run 'TestPublish.*Diagnostics|TestDidChangeDuringAsyncQuery' -count=1`.
- [ ] **Step 5: Commit.** `git add internal/handler internal/lsp && git commit -m 'feat(lsp): publish InterBase static diagnostics'`.

### Task 5: User-facing coverage and whole-branch validation

**Files:** Modify `README.md` in the InterBase editor features section; adjust focused test files in Tasks 1–4 if final verification finds missing acceptance cases.

**Interfaces:** No new API; documentation describes enabled InterBase behavior and silent unknowns.

- [ ] **Step 1: Add acceptance tests.** Test a single document containing an unused local and an INSERT SELECT 40→20 warning, then edit to make the local read and the projection fit; expect both initial diagnostics and a subsequent empty list. Include a missing-catalog variant that still emits the unused hint without table warnings.
- [ ] **Step 2: Run red if acceptance case fails.** `go test ./internal/handler -run 'TestInterBaseDiagnosticsAcceptance' -count=1`; inspect exact failure before any fix.
- [ ] **Step 3: Implement fixes and document behavior.** Add a concise README subsection showing `VARCHAR(40)` into `VARCHAR(20)`, unused declaration hints, covered statement families, known expression forms, and the rule that unknown widths are silent. Keep examples consistent with tested SQL.
- [ ] **Step 4: Verify.** `go test ./... -count=1`, `go test -race ./internal/sqlsymbol ./internal/handler -count=1`, `git diff --check`, and `git status --short`; investigate any failure before claiming completion.
- [ ] **Step 5: Commit.** `git add README.md internal/sqlsymbol internal/handler && git commit -m 'docs: describe InterBase static diagnostics'`.
