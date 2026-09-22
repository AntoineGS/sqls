# InterBase Symbol Navigation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `gd`, `grr`, and `grn` resolve InterBase procedure variables across statements, and make `gd` navigate to database table/column definitions.

**Architecture:** Add a request-local, token-based `internal/sqlsymbol` package for procedure declarations, local occurrences, and SQL relation contexts. LSP handlers share that analysis and adapt byte spans to UTF-16 ranges. Database definitions extend the existing catalog and snapshot path.

**Tech Stack:** Go 1.25.7+, existing sqls dialect/token packages, sourcegraph/jsonrpc2, existing InterBase catalog and DDL capabilities. No new module dependencies.

**Spec:** `docs/superpowers/specs/2026-09-22-interbase-symbol-navigation-design.md`, including the optional-prefix review clarification.

## Global Constraints

- “Presence or absence of `:` is not a sufficient variable-versus-column rule.”
- “Nested procedural blocks share the containing procedure's declarations.”
- “Separate procedures have distinct symbol identities even for equal names.”
- “Comments, string literals, and identifier substrings are never occurrences.”
- “An unresolved local must not fall through into a same-spelled catalog object.”
- “Analysis is request-local and built from the current in-memory document.”
- “Use zero-based UTF-16 positions at the LSP boundary and explicit conversions to the token/source representation.”
- “If unsupported syntax could hide additional occurrences of the selected symbol, reject rename rather than return a knowingly partial edit; read-only navigation can still use proven results.”
- “Editing source or a generated snapshot does not execute SQL.”
- Keep the current module dependencies and Go floor. Native builds require Linux/amd64, CGO, `/opt/interbase/include`, `/opt/interbase/lib/libgds.so`, and the sibling `interbase-go` checkout.

---

## Execution setup and file structure

At execution time, use `using-git-worktrees` to create an isolated branch. Prefer
a sibling worktree such as `../sqls-symbol-navigation`, so the existing
`replace interbase-go => ../interbase-go` still resolves. Do not change `go.mod`
to compensate for worktree placement. Inspect applicable `AGENTS.md`, current
status, and the approved spec before implementation.

Tasks are sequential because all three editor operations share one resolver.

| Files | Responsibility |
| --- | --- |
| `internal/sqlsymbol/source.go`, `source_test.go` | Exact source spans and dialect-aware identifier identity |
| `internal/sqlsymbol/procedure.go`, `procedure_test.go` | Procedure boundaries, parameters, variable declarations |
| `internal/sqlsymbol/resolve.go`, `resolve_test.go` | Local binding, occurrences, ambiguity |
| `internal/sqlsymbol/sql.go`, `sql_test.go` | SQL roles and nested relation scopes |
| `internal/sqlsymbol/rename.go`, `rename_test.go` | Replacement validation, collisions, source edits |
| `internal/sqlsymbol/ddl.go`, `ddl_test.go` | Declaration spans in generated DDL |
| `internal/handler/symbol.go`, `symbol_test.go` | Shared LSP adapters and local-operation helpers |
| `internal/handler/references.go`, `references_test.go` | New references handler and protocol tests |
| `internal/handler/definition.go`, `definition_test.go` | Local definition routing and fallback precedence |
| `internal/handler/rename.go`, `rename_test.go` | Procedure-local rename routing |
| `internal/handler/interbase_definition.go`, `interbase_definition_test.go` | Snapshot targets and full dialect propagation |
| `internal/handler/interbase_relation_definition.go`, `interbase_relation_definition_test.go` | Catalog ownership and table/column navigation |
| `internal/lsp/lsp.go`, `internal/handler/handler.go`, `handler_test.go` | Reference types, dispatch, advertised capability |
| `internal/sqlsymbol/acceptance_test.go`, `benchmark_test.go` | Full-file acceptance and request-cost measurement |
| `README.md` | Supported symbols, mappings, native rebuild instructions |

Reuse existing `token.NewTokenizer`, `dialect.DialectForDriverVariant`,
`Server.fileText`, `Server.parserDriverVariant`, `DBCache.ColumnDescs`,
`DBCache.ColumnDatabase`, `DBCache.SchemaTables`, and `DDLRepository.ObjectDDL`.
Reuse the snapshot timeout, storage, banner, and cleanup code.
Existing test helpers include `newTestContext`, `textDocumentDidOpen`,
`newDefinitionServer`, `newStubDDLRepository`, and `newTestSnapshotStore`.

## Shared interface contract

All spans are half-open UTF-8 byte offsets into the original text. This package
does not import `internal/lsp` or `internal/database`.

```go
// source.go
type Span struct { Start, End int }
type Name struct { Text string; Quoted bool }
func (n Name) Key() string
func (n Name) MatchesCatalogName(actual string) bool
type Edit struct { Span Span; NewText string }

// procedure.go
type SymbolKind uint8
const (
    Variable SymbolKind = iota
    InputParameter
    OutputParameter
)
type Symbol struct {
    Name Name
    Kind SymbolKind
    Declaration Span
    Scope Span
    Uses []Span
    RenameBlocked string
}
type Analysis struct {
    Text string
    Variant dialect.DriverVariant
    Symbols []*Symbol
    // Private token, procedure, and SQL-scope indexes are added by Tasks 1–4.
}
func Analyze(text string, dv dialect.DriverVariant) (*Analysis, error)

// resolve.go / sql.go
type Role uint8
const (
    Outside Role = iota
    Local
    Relation
    Column
    Alias
    Callable
    Other
    Ambiguous
)
type RelationRef struct { Name Name; Alias *Name }
type SQLReference struct {
    Name Name
    Qualifier *Name
    Scopes [][]RelationRef // Innermost query first; correlated outer scopes follow.
}
type Resolution struct {
    Role Role
    Span Span
    InProcedure bool
    Symbol *Symbol
    SQL *SQLReference
}
func (a *Analysis) Resolve(offset int) Resolution
func (a *Analysis) References(symbol *Symbol, includeDeclaration bool) []Span

// rename.go
func (a *Analysis) Rename(symbol *Symbol, newName string) ([]Edit, error)

// ddl.go
func TableDeclaration(text string, table Name) (Span, bool)
func ColumnDeclaration(text string, table, column Name) (Span, bool)
```

`Name.Key` returns decoded text for quoted names and uppercase text for unquoted
names. Thus `foo` and `"FOO"` match, while `"foo"` is distinct. Catalog names are
already decoded: compare them with `Key()` exactly. Symbol pointer identity ties
each occurrence to one declaration in one procedure; do not copy symbols when
binding references. `Uses` excludes declarations. `References` returns sorted,
deduplicated spans, adding the declaration only when requested.

## Task 1: Source spans and dialect-correct names

**Files:** create `internal/sqlsymbol/source.go`, `source_test.go`.

**Consumes:** existing tokenizer and dialect variant types.

**Produces:** `Span`, `Name`, `Edit`, the two `Name` methods; private
`lex(text string, dv dialect.DriverVariant) ([]lexeme, error)` and
`lexeme { Token *token.Token; Span Span }`.

- [ ] **Write failing source-position and identity tests:**

```go
func TestLexPreservesOriginalSpans(t *testing.T) {
    text := "/* 😀 */\r\namountpaid = :amountpaid;"
    dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase,
        Variant: dialect.SQLVariantInterBase1}
    tokens, err := lex(text, dv)
    if err != nil { t.Fatal(err) }
    var got []Span
    for _, tok := range tokens {
        if text[tok.Span.Start:tok.Span.End] == "amountpaid" {
            got = append(got, tok.Span)
        }
    }
    first, last := strings.Index(text, "amountpaid"), strings.LastIndex(text, "amountpaid")
    want := []Span{{first, first + 10}, {last, last + 10}}
    if diff := cmp.Diff(want, got); diff != "" { t.Fatal(diff) }
}

func TestNameIdentity(t *testing.T) {
    if (Name{Text: "foo"}).Key() != (Name{Text: "FOO", Quoted: true}).Key() {
        t.Fatal("unquoted foo must match quoted uppercase FOO")
    }
    if (Name{Text: "foo"}).Key() == (Name{Text: "foo", Quoted: true}).Key() {
        t.Fatal("quoted lowercase foo is distinct")
    }
}
```

  Add table cases for doubled quotes, single-quoted escapes, Dialect 1
  double-quoted strings, Dialect 3 quoted identifiers, tabs, multiline strings,
  and comments. Decode identifier escapes from original spelling; do not assume
  `SQLWord.Value` has already removed doubled quotes.
- [ ] **Run red:** `go test ./internal/sqlsymbol -run 'TestLex|TestName' -count=1`.
  Expected initial failure: missing types/functions.
- [ ] **Implement the lexical adapter and name methods:**

```go
func (n Name) Key() string {
    if n.Quoted { return n.Text }
    return strings.ToUpper(n.Text)
}
func (n Name) MatchesCatalogName(actual string) bool { return n.Key() == actual }

// Core loop inside lex, using the selected dialect:
for {
    start := tokenizer.Scanner.Pos().Offset
    tok, err := tokenizer.NextToken()
    if errors.Is(err, io.EOF) { break }
    if err != nil { return nil, err }
    end := tokenizer.Scanner.Pos().Offset
    result = append(result, lexeme{Token: tok, Span: Span{start, end}})
}
```

  Verify scanner offsets with tests: they must identify the next source rune,
  not the last scanned token start. Do not reconstruct offsets from token
  strings because the lexer normalizes CRLF and some escapes. Keep original
  spans even when trivia is omitted from significant-token indexes. If a lexer
  defect is exposed, make the smallest tested change in `token/lexer.go` rather
  than writing a second SQL lexer.
- [ ] **Run green and lexical regressions:**
  `go test ./internal/sqlsymbol ./token ./dialect -count=1`.
- [ ] **Commit:** `feat: add dialect-aware symbol source spans`.

## Task 2: Procedure scopes and declarations

**Files:** create `internal/sqlsymbol/procedure.go`, `procedure_test.go`;
extend `source.go` for active script terminators.

**Consumes:** Task 1 `lex`, `Span`, `Name`.

**Produces:** `SymbolKind`, `Symbol`, `Analysis`, `Analyze`; private
`procedure { Span Span; Symbols map[string][]*Symbol }` and a `procedures`
index on `Analysis`.

- [ ] **Write the declaration regression:**

```go
func TestAnalyzeProcedureDeclarations(t *testing.T) {
    text := `ALTER PROCEDURE p (id INTEGER)
RETURNS (result NUMERIC(15,2)) AS
DECLARE VARIABLE amountpaid NUMERIC(15,2);
BEGIN
  amountpaid = 0;
END`
    a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
    if err != nil { t.Fatal(err) }
    var names []string
    for _, symbol := range a.Symbols { names = append(names, symbol.Name.Key()) }
    if diff := cmp.Diff([]string{"ID", "RESULT", "AMOUNTPAID"}, names); diff != "" {
        t.Fatal(diff)
    }
    for _, symbol := range a.Symbols {
        if symbol.Scope.Start != 0 || symbol.Scope.End != len(text) {
            t.Fatalf("wrong scope: %+v", symbol.Scope)
        }
    }
}
```

  Add fixtures for two procedures with the same local name, nested BEGIN blocks,
  CASE expressions, parameter type commas, quoted names, duplicate declarations,
  incomplete bodies, and a new header following an incomplete procedure.
  Test `SET TERM ^ ;` and `SET TERM !! ;`: ordinary SQL tokenization rejects a
  lone `!`, so active terminators must be intercepted outside strings/comments.
- [ ] **Run red:**
  `go test ./internal/sqlsymbol -run 'TestAnalyze|TestProcedure' -count=1`.
- [ ] **Implement a significant-token scope scanner:**

```text
outside: CREATE/ALTER followed by PROCEDURE starts a header
header: parse optional input parentheses and RETURNS parentheses by nesting depth
AS: collect DECLARE VARIABLE entries until the body BEGIN
body: push typed BEGIN/CASE frames; END pops its matching construct
empty body stack: close the procedure at the end of that END token
new procedure header: close any preceding incomplete scope
EOF: close an incomplete current scope at EOF
```

  Recognize keywords only in unquoted tokens, including procedural words absent
  from the generic keyword table. Parse SET TERM only between source units and
  track its active delimiter before calling the SQL lexer on that delimiter.
  Preserve original offsets; do not delete directives from the analyzed text.
  A missing body END is recoverable; an unterminated literal/comment or invalid
  declaration blocks unsafe rename rather than returning partial edits.
  Store duplicate declarations in a name bucket and mark them ambiguous.
- [ ] **Run green:** `go test ./internal/sqlsymbol -count=1`.
- [ ] **Commit:** `feat: recognize InterBase procedure declaration scopes`.

## Task 3: Local binding across procedural and SQL statements

**Files:** create `internal/sqlsymbol/resolve.go`, `resolve_test.go`; extend
`procedure.go` to build occurrences after declaration discovery.

**Consumes:** declarations, significant tokens, source spans.

**Produces:** `Role`, `Resolution`, `RelationRef`, `SQLReference`, `Resolve`,
`References`. Task 4 fills the relation candidate scopes on SQL references.

- [ ] **Write the optional-prefix regression:**

```go
func TestResolveOptionalVariablePrefixes(t *testing.T) {
    text := `ALTER PROCEDURE p AS
DECLARE VARIABLE amountpaid INTEGER;
BEGIN
  amountpaid = 0;
  amountpaid = amountpaid + 1;
  amountpaid = :amountpaid + 1;
  UPDATE customerinvoice SET amountpaid = :amountpaid;
  /* amountpaid */
END`
    a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
    if err != nil { t.Fatal(err) }
    got := a.Resolve(strings.Index(text, "amountpaid = 0"))
    if got.Role != Local || got.Symbol == nil { t.Fatalf("resolution: %+v", got) }
    if n := len(a.References(got.Symbol, true)); n != 7 {
        t.Fatalf("got %d occurrences, want 7", n)
    }
    column := strings.Index(text, "SET amountpaid") + len("SET ")
    if r := a.Resolve(column); r.Role != Column { t.Fatalf("SET target: %+v", r) }
}
```

  Add a reduced HEADEREMPLYID_TEMP fixture with a declaration and uses in two
  separate SELECT statements; require three occurrences. Add
  `payDate = F_StripTime(payDate)` and the unprefixed sum assigned to ordertotal.
  Exclude callable names, member fields, comments, strings, and another
  procedure's same-named symbol. Assert exact spans as well as counts.
- [ ] **Run red:**
  `go test ./internal/sqlsymbol -run 'TestResolve|TestReferences' -count=1`.
- [ ] **Implement context-driven binding:**

```text
declaration name                   -> Local, declaration identity
procedural assignment target       -> Local, with no inserted colon
procedural expression identifier   -> Local if declared and not a callable/member
colon + declared identifier        -> Local, identifier-only span
SELECT/FOR SELECT expression       -> SQL expression role
INSERT column list / UPDATE target -> Column
FROM/JOIN/UPDATE/INSERT INTO table  -> Relation
SELECT INTO / RETURNING_VALUES     -> Local output target, optional colon
duplicate declaration              -> Ambiguous
SQL alias declaration/qualifier    -> Alias
callable name                     -> Callable
comment/literal                   -> Other
```

  Dispatch procedural statements after BEGIN, THEN, ELSE, or a statement end;
  do not treat all text between semicolons as a single SQL context. Track IF,
  WHILE, FOR SELECT ... DO, nested expressions and SELECTs, and EXECUTE
  PROCEDURE arguments/output targets separately. The procedure-call name is
  not a local occurrence; its input arguments are value expressions.

  Bare names in SQL value expressions that could denote either a local or a
  column remain ambiguous unless precedence/ownership is proven. Block rename
  of a potentially affected local rather than treating absence of `:` as proof.
  Explicit SQL column positions do not create this ambiguity. This is the
  spec's unresolved-context policy, not a claim that unprefixed variables are
  illegal in SQL expressions.

  Unknown syntax containing a possible use of a declared name adds a specific
  `RenameBlocked` reason to that symbol. Do not block unrelated symbols.
  Build token-to-resolution and symbol-to-uses indexes in bounded passes.
  `Resolve` uses half-open containment; an identifier-end cursor must not
  accidentally select preceding text. A cursor on a variable's prefix colon
  may resolve the following identifier, but the editable span excludes `:`.
- [ ] **Run green:** `go test ./internal/sqlsymbol -count=1`.
- [ ] **Commit:** `feat: bind InterBase local references across statements`.

## Task 4: SQL relation and column contexts

**Files:** create `internal/sqlsymbol/sql.go`, `sql_test.go`; extend `resolve.go`.

**Consumes:** tokens, statement contexts, `Resolution`.

**Produces:** SQL references with nearest-query-first candidate scopes and aliases.

- [ ] **Write an UPDATE ownership-context regression:**

```go
func TestSQLUpdateColumnContext(t *testing.T) {
    text := "UPDATE customerinvoice SET amountpaid=0, balance=:ordertotal WHERE invoice=:invoice;"
    a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
    if err != nil { t.Fatal(err) }
    r := a.Resolve(strings.Index(text, "amountpaid"))
    if r.Role != Column || r.SQL == nil { t.Fatalf("resolution: %+v", r) }
    if len(r.SQL.Scopes) != 1 || len(r.SQL.Scopes[0]) != 1 {
        t.Fatalf("scopes: %+v", r.SQL.Scopes)
    }
    if r.SQL.Scopes[0][0].Name.Key() != "CUSTOMERINVOICE" {
        t.Fatalf("wrong relation: %+v", r.SQL.Scopes)
    }
}
```

  Add qualified SELECT columns, INSERT columns, DELETE predicates, joined
  tables, nested SELECTs reusing aliases, and a correlated outer qualifier.
- [ ] **Run red:** `go test ./internal/sqlsymbol -run 'TestSQL' -count=1`.
- [ ] **Build nested scopes instead of flattening relations:**

```text
UPDATE SET / INSERT target column: only the target relation is a candidate
qualified column: nearest binding of qualifier wins and shadows outer bindings
unqualified column: retain candidate relations separately for each query scope
derived/unknown relation: retain uncertainty rather than omitting the relation
```

  Represent a derived/unknown relation with empty `RelationRef.Name` and its
  known alias. Its presence makes unqualified ownership uncertain. A qualified
  column on another concrete relation can still resolve. Do not infer lineage
  through a derived SELECT. Classify the qualifier token itself as Alias when it
  denotes an alias, and the member token as Column. Existing alias/subquery
  navigation remains usable.
- [ ] **Run green:** `go test ./internal/sqlsymbol -count=1`.
- [ ] **Commit:** `feat: track SQL relation scopes for definition resolution`.

## Task 5: Local go-to-definition and LSP references

**Files:** create `internal/handler/symbol.go`, `symbol_test.go`,
`references.go`, `references_test.go`; modify `definition.go`,
`definition_test.go`, `handler.go`, `handler_test.go`, `internal/lsp/lsp.go`.

**Consumes:** `Analyze`, `Resolve`, `References`, current document text/variant.

**Produces:**

```go
// internal/lsp/lsp.go
type ReferenceContext struct { IncludeDeclaration bool `json:"includeDeclaration"` }
type ReferenceParams struct {
    TextDocumentPositionParams
    Context ReferenceContext `json:"context"`
    WorkDoneProgressParams
    PartialResultParams
}

// internal/handler/symbol.go
func symbolOffset(text string, pos lsp.Position) (int, bool)
func symbolRange(text string, span sqlsymbol.Span) (lsp.Range, bool)
func localDefinition(uri, text string, pos lsp.Position, dv dialect.DriverVariant) (lsp.Definition, bool, error)
func localReferences(uri, text string, params lsp.ReferenceParams, dv dialect.DriverVariant) ([]lsp.Location, error)

// internal/handler/references.go
func (s *Server) handleReferences(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error)
```

  `localDefinition`'s boolean means the local/ambiguous procedural path handled
  the target, including an intentional empty result. SQL roles stay available
  for catalog routing; unknown procedural identifiers do not reach a
  spelling-based fallback.

- [ ] **Write definition and references integration tests.** Reuse `TestContext`
  and a synthetic InterBase connection for local-only requests:

```go
func TestReferencesProcedureAcrossStatements(t *testing.T) {
    tx := newTestContext()
    tx.setup(t)
    defer tx.tearDown()
    tx.server.stateMu.Lock()
    tx.server.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase}
    tx.server.stateMu.Unlock()
    text := "ALTER PROCEDURE p AS\nDECLARE VARIABLE v INTEGER;\nBEGIN\nv=0;\nv=:v+1;\nEND"
    tx.textDocumentDidOpen(t, testFileURI, text)
    for _, include := range []bool{false, true} {
        params := lsp.ReferenceParams{
            TextDocumentPositionParams: lsp.TextDocumentPositionParams{
                TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI},
                Position: lsp.Position{Line: 4, Character: 3},
            },
            Context: lsp.ReferenceContext{IncludeDeclaration: include},
        }
        var got []lsp.Location
        if err := tx.conn.Call(tx.ctx, "textDocument/references", params, &got); err != nil {
            t.Fatal(err)
        }
        want := 3
        if include { want++ }
        if len(got) != want { t.Fatalf("got %d references, want %d", len(got), want) }
    }
}
```

  Assert exact ordered locations in a separate test. Check local definitions
  from both bare assignments and prefixed reads. Test absent request parameters,
  missing documents, unsupported drivers, comments, and an ambiguous local
  sharing a name with a catalog object. Update `TestInitialized` to expect
  `ReferencesProvider: true`; keep the existing boolean rename capability.
- [ ] **Run red:**
  `go test ./internal/handler -run 'TestReferences|TestLocalDefinition|TestSymbol|TestInitialized' -count=1`.
- [ ] **Implement UTF-16 adapters and dispatch.** Count surrogate pairs as two
  units, reject invalid positions, handle CRLF as one line break, and never
  slice inside an encoded character. Test an emoji before the selected name
  on the same line. Convert ranges from original text rather than token columns.

```go
// New dispatcher case:
case "textDocument/references":
    return s.handleReferences(ctx, conn, req)

// New capability in handleInitialize:
ReferencesProvider: true,
```

  Follow `handleTextDocumentRename` for request decoding/document validation.
  Run local definition before generic alias scanning or repository acquisition.
  Route proven Relation/Column roles directly to contextual catalog navigation
  (added in Task 7), rather than letting a same-spelled unrelated alias win.
  Alias and Callable roles retain their applicable existing definition paths.
  Other/Ambiguous targets inside a recognized procedure stop with no location.
  Preserve non-InterBase behavior. References on unsupported targets return empty results, not
  method-not-found. Prove local requests work without a DDL repository.
- [ ] **Run green:** `go test ./internal/handler ./internal/sqlsymbol -count=1`.
- [ ] **Commit:** `feat: add procedure-local definition and references`.

## Task 6: Scope-aware rename with preserved prefixes

**Files:** create `internal/sqlsymbol/rename.go`, `rename_test.go`; modify
`internal/handler/rename.go`, `rename_test.go`, and `symbol.go`.

**Consumes:** local occurrences, names/dialect, LSP adapters.

**Produces:** `Analysis.Rename` and handler helper:

```go
func localRename(text string, params lsp.RenameParams, dv dialect.DriverVariant) (*lsp.WorkspaceEdit, bool, error)
```

  Its boolean distinguishes outside-procedure fallback from a handled local,
  ambiguous, or unsupported procedure target.

- [ ] **Write an edit-application regression with expected SQL:**

```go
func TestRenamePreservesVariablePrefixes(t *testing.T) {
    text := `ALTER PROCEDURE p AS
DECLARE VARIABLE amountpaid INTEGER;
BEGIN
  amountpaid = 0;
  amountpaid = amountpaid + :amountpaid;
  UPDATE customerinvoice SET amountpaid = :amountpaid;
END`
    want := `ALTER PROCEDURE p AS
DECLARE VARIABLE paid INTEGER;
BEGIN
  paid = 0;
  paid = paid + :paid;
  UPDATE customerinvoice SET amountpaid = :paid;
END`
    a, err := Analyze(text, dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase})
    if err != nil { t.Fatal(err) }
    r := a.Resolve(strings.Index(text, "amountpaid = 0"))
    edits, err := a.Rename(r.Symbol, "paid")
    if err != nil { t.Fatal(err) }
    got := text
    for i := len(edits)-1; i >= 0; i-- {
        edit := edits[i]
        got = got[:edit.Span.Start] + edit.NewText + got[edit.Span.End:]
    }
    if got != want { t.Fatalf("got:\n%s\nwant:\n%s", got, want) }
}
```

  Add the HEADEREMPLYID_TEMP regression, mixed-case uses, parameters, scope
  boundaries, invalid names, comments/newlines in replacement input, reserved
  syntax keywords, collisions, case-only renames, and quoted names under
  Dialects 1 and 3. Add a JSON-RPC test opening/changing a document to a nonzero
  version; ensure the result is not tied to version zero.
- [ ] **Run red:**
  `go test ./internal/sqlsymbol ./internal/handler -run 'TestRename|TestLocalRename' -count=1`.
- [ ] **Implement replacement validation and edits.** Require one identifier
  token consuming the entire proposed name, with valid dialect characters and
  no surrounding trivia. Reject reserved syntax words using InterBase rules;
  do not blindly equate the generic completion keyword inventory with reserved
  words. Decode quotes for collision checks while retaining the requested
  source spelling as `NewText`. Another declaration with the same semantic name
  in the same procedure is a collision; renaming a symbol to its own spelling
  or changing only its unquoted case is allowed.

```go
// After replacement validation and same-scope collision checks:
if symbol.RenameBlocked != "" {
    return nil, fmt.Errorf("cannot rename %s: %s", symbol.Name.Text, symbol.RenameBlocked)
}
spans := a.References(symbol, true)
edits := make([]Edit, 0, len(spans))
for _, span := range spans {
    edits = append(edits, Edit{Span: span, NewText: newName})
}
return edits, nil
```

  Reject nil/foreign symbol pointers instead of panicking. Convert spans to
  `WorkspaceEdit{Changes: map[string][]lsp.TextEdit{uri: edits}}`.
  Inside a recognized procedure, reject ambiguous and database-column rename
  targets with descriptive errors rather than applying the old spelling-only
  fallback. Preserve existing outside-procedure/non-InterBase behavior.
- [ ] **Run green:** `go test ./internal/sqlsymbol ./internal/handler -count=1`.
- [ ] **Commit:** `feat: rename procedure locals across SQL statements`.

## Task 7: Catalog-backed table and column definitions

**Files:** create `internal/sqlsymbol/ddl.go`, `ddl_test.go`,
`internal/handler/interbase_relation_definition.go`,
`interbase_relation_definition_test.go`; modify `interbase_definition.go`,
`interbase_definition_test.go`, and `definition.go`.

**Consumes:** SQL contexts, `DBCache`, `DDLRepository`, snapshots, `symbolRange`.

**Produces:** `TableDeclaration`, `ColumnDeclaration`; catalog adapter:

```go
func resolveRelationTarget(ref sqlsymbol.SQLReference, role sqlsymbol.Role, cache *database.DBCache) (snapshotTarget, bool)
```

  Extend `snapshotTarget` with `column *sqlsymbol.Name`. Add
  `resolveSnapshotTargetWithVariant(text string, params lsp.DefinitionParams,
  dbCache *database.DBCache, dv dialect.DriverVariant) (snapshotTarget, bool)`.
  Keep the current driver-only helper as a compatibility wrapper. Production
  requests pass `s.parserDriverVariant()` instead of losing the SQL variant.

- [ ] **Write a declaration-range regression:**

```go
func TestColumnDeclarationSkipsCommentsAndConstraints(t *testing.T) {
    ddl := `CREATE TABLE "CUSTOMERINVOICE" (
  /* AMOUNTPAID is documented here */
  "AMOUNTPAID_OLD" INTEGER,
  "AMOUNTPAID" NUMERIC(15,2),
  CONSTRAINT "AMOUNTPAID_CHECK" CHECK ("AMOUNTPAID" >= 0)
);`
    span, ok := ColumnDeclaration(ddl,
        Name{Text: "CUSTOMERINVOICE", Quoted: true},
        Name{Text: "AMOUNTPAID", Quoted: true})
    if !ok { t.Fatal("column declaration not found") }
    want := strings.Index(ddl, `"AMOUNTPAID" NUMERIC`)
    if span.Start != want || ddl[span.Start:span.End] != `"AMOUNTPAID"` {
        t.Fatalf("wrong column range: %+v", span)
    }
}
```

  Add mock-catalog tests with CUSTOMERINVOICE and AMOUNTPAID/BALANCE/INVOICE
  columns. Use the example UPDATE and require a table DDL call, a `file://`
  snapshot, and the exact column range below the banner. Use
  `newDefinitionServer` to isolate snapshot files in `t.TempDir`.
  Test missing cache/repository/capability, missing columns, unavailable DDL,
  qualifier shadowing, unknown derived tables, and ambiguous columns. Include
  a Dialect 1 source with quoted generated DDL, global temporary tables, external
  table filenames containing punctuation, and an explicit view column list.
- [ ] **Run red:**
  `go test ./internal/sqlsymbol ./internal/handler -run 'TestColumnDeclaration|TestTableDeclaration|TestInterBaseRelation|TestResolveRelation' -count=1`.
- [ ] **Implement catalog matching and DDL scanning.** For each query scope,
  bind qualifiers first, then validate candidate relation/column descriptors.
  A unique owner resolves; multiple matches or unavailable candidate metadata
  do not. An inner binding shadows an outer one even when its metadata is
  absent. Table recognition uses the basic table cache; an extended catalog is
  not required just to recognize a known table. Use available view descriptors
  to distinguish views and preserve their existing DDL behavior.

```text
table scan:
    find CREATE [GLOBAL TEMPORARY] TABLE and its matching identifier
    skip any EXTERNAL FILE string before the column-list parentheses
    identify the outer column-list parentheses
column scan:
    split entries at commas only at the outer list depth
    exclude entries starting CONSTRAINT/PRIMARY/FOREIGN/UNIQUE/CHECK
    match the first identifier of each column entry
    return its original source span
```

  Match decoded catalog spelling exactly after normalizing unquoted names.
  `../interbase-go/schema/ddl.go` quotes identifiers and emits explicit view
  column lists, global temporary table headers, and optional EXTERNAL FILE
  clauses. DDL scanning therefore uses Dialect 3-style delimited identifier
  rules even when the edited procedure is Dialect 1. A missing declaration
  returns false, not a substring approximation. `ColumnDeclaration` also accepts
  CREATE VIEW with an explicit column list, matching the owner name before
  scanning that list. It does not infer column lineage from a SELECT or from
  the verbatim-view-source fallback.

  Locate table/column declaration spans before writing a new-target snapshot,
  then add the banner line count to returned ranges. Reuse the existing
  three-second DDL context and storage lifecycle. Keep all current
  procedure/view/trigger fallback behavior. Hold no server-state lock across
  catalog or filesystem I/O. For unavailable table DDL, return no definition.
- [ ] **Run green:**
  `go test ./internal/sqlsymbol ./internal/handler ./internal/database -count=1`.
- [ ] **Commit:** `feat: navigate to InterBase table and column definitions`.

## Task 8: Acceptance, documentation, and native-build handoff

**Files:** modify `README.md`; create `internal/sqlsymbol/acceptance_test.go`,
`benchmark_test.go`; extend `internal/handler/symbol_test.go` with the combined
protocol regression. Update this plan's checkboxes and verification record.

**Consumes:** completed resolver/handlers and existing native configuration.

**Produces:** synthetic end-to-end acceptance, optional external-fixture checks,
benchmark measurements, documentation, and a tested native build artifact.

- [x] **Write a combined JSON-RPC regression** using a compact synthetic
  procedure containing HEADEREMPLYID_TEMP, ORDERTOTAL, and the example UPDATE.
  Check local definitions, references with/without declaration, and rename
  applied to source. Send `didChange`, then repeat a definition at its new
  position to prove that in-memory text is authoritative.
  Add this opt-in local-file acceptance test:

```go
func TestLocalInterBaseExample(t *testing.T) {
    path := os.Getenv("SQLS_SYMBOL_EXAMPLE")
    if path == "" { t.Skip("SQLS_SYMBOL_EXAMPLE not set") }
    data, err := os.ReadFile(path)
    if err != nil { t.Fatal(err) }
    text := string(data)
    a, err := Analyze(text, dialect.DriverVariant{
        Driver: dialect.DatabaseDriverInterBase,
        Variant: dialect.SQLVariantInterBase1,
    })
    if err != nil { t.Fatal(err) }
    needle := "IMPORTEXTERNALORDER_EMPLYID(:HEADEREMPLYID_TEMP"
    call := strings.Index(text, needle)
    if call < 0 { t.Fatal("expected example call not found") }
    offset := call + len("IMPORTEXTERNALORDER_EMPLYID(:")
    r := a.Resolve(offset)
    if r.Role != Local || r.Symbol == nil { t.Fatalf("resolution: %+v", r) }
    if n := len(a.References(r.Symbol, true)); n != 3 {
        t.Fatalf("HEADEREMPLYID_TEMP has %d occurrences, want 3", n)
    }
    if _, err := a.Rename(r.Symbol, "HEADER_EMPLOYEE_TEMP"); err != nil { t.Fatal(err) }
}
```

  Extend it to find ORDERTOTAL's declaration from the UPDATE and classify
  AMOUNTPAID with CUSTOMERINVOICE as its candidate owner. Never write to this
  external fixture or copy its business source into the repository. If the
  fixture changed since review, inspect differences before changing assertions.
- [x] **Run the acceptance tests before final documentation:**

```shell
go test ./internal/handler -run 'TestSymbolDocumentChanges|TestSymbolNavigationAcceptance' -count=1
SQLS_SYMBOL_EXAMPLE='/home/a.simard@multidev.local/OneDrive/Dev/2026-09-14 - 5-IMPORTEXTERNALORDER_SHOPIFYPOS.sql' go test ./internal/sqlsymbol -run '^TestLocalInterBaseExample$' -count=1 -v
```

  Full-file failures remain unmet requirements: reduce them to synthetic
  regressions, fix the resolver, and rerun the relevant tests.
- [x] **Add and run benchmarks.** Read input before timing and measure both
  analysis and representative symbol requests:

```go
func BenchmarkProcedureSymbols(b *testing.B) {
    text := "ALTER PROCEDURE p AS DECLARE VARIABLE v INTEGER; BEGIN\n" +
        strings.Repeat("v=v+1;\n", 1000) + "END"
    dv := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}
    pos := strings.Index(text, "v=v+1")
    b.ReportAllocs()
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        a, err := Analyze(text, dv)
        if err != nil { b.Fatal(err) }
        r := a.Resolve(pos)
        if r.Symbol == nil { b.Fatal("variable not resolved") }
        a.References(r.Symbol, true)
    }
}
```

  Also benchmark the external example when SQLS_SYMBOL_EXAMPLE is set. Report
  duration/allocations and investigate quadratic scaling or repeated full-text
  scans. Run:
  `go test ./internal/sqlsymbol -run '^$' -bench 'BenchmarkProcedureSymbols' -benchmem`.
- [x] **Document the supported behavior in the InterBase editor section:**

```markdown
##### Procedure navigation and rename

For the active InterBase dialect, sqls resolves declared procedure variables
and input/output parameters across the containing procedure. Both bare and
colon-prefixed references are supported in their applicable statement contexts.
Rename preserves each occurrence's prefix and distinguishes local assignments
from SQL column targets.

In Neovim's standard LSP mappings, `grr` finds references and `grn` renames.
Use your go-to-definition mapping (commonly `gd`) for local declarations or
catalog-backed table/column definitions. Local references and rename cover the
containing procedure in the current document. Ambiguous rename requests are
rejected rather than applying a partial spelling-based replacement.

Table/column definitions require metadata from the active connection and
reproducible DDL. They use the existing source-snapshot storage and cleanup.
```

  Update the existing snapshot supported-kinds paragraph to include tables and
  columns. Document that the client needs a rebuilt native sqls binary and a
  restart to obtain the new references capability.
- [x] **Format changed Go files and run required checks.** Use `gofmt -w` only
  on changed files, then `git diff --check`, followed by:

```shell
go test ./... -count=1
go test -race ./internal/sqlsymbol ./internal/handler -count=1
CGO_ENABLED=1 go test -tags interbase ./internal/sqlsymbol ./internal/handler ./internal/database -count=1
CGO_ENABLED=1 go build -tags interbase -o /tmp/opencode/sqls-symbol-navigation .
```

  Use the existing runtime-library setup if the loader needs
  `/opt/interbase/lib`. Tagged tests should use their existing mocks; do not
  enable live tests that create or alter database objects. Record environment
  blockers separately from code failures. Repeat tests only after a relevant
  change or to resolve an outstanding concern.
- [x] **Verify the editor handoff.** Identify the executable used by the user's
  attached sqls client before replacing an installed binary. If the session is
  accessible, restart its client with the built artifact and test the example's
  line 326 rename/references and line 847 definitions. Otherwise report the
  artifact path and remaining editor checks. Do not claim an actual Neovim test
  based only on server-side assertions.
- [x] **Commit:** `docs: document and verify InterBase symbol navigation`.

## Self-review coverage map

| Spec requirement | Tasks |
| --- | --- |
| Procedure declarations, nested blocks, CASE, SET TERM, incomplete files | 1–3 |
| Optional prefixes, bare assignment/read, same-named columns, trivia exclusion | 1, 3, 6 |
| Ambiguity and avoiding partial rename | 3, 4, 6 |
| Dialect variants, quoted identity, UTF-16/CRLF | 1, 2, 5–7 |
| Definition/references/rename share symbol identity | 3, 5, 6 |
| Capability, dispatch, declaration flag, current document text | 5, 8 |
| Contextual table/column ownership and snapshots | 4, 7 |
| Generic and existing database-source regressions | 5–8 |
| Real example, performance, docs, native build/client handoff | 8 |

## Verification record

Task 8 verification (2026-09-22):

- Synthetic protocol acceptance: `go test ./internal/handler -run 'TestSymbolDocumentChanges|TestSymbolNavigationAcceptance' -count=1` — PASS.
- Read-only fixture acceptance: `SQLS_SYMBOL_EXAMPLE='/home/a.simard@multidev.local/OneDrive/Dev/2026-09-14 - 5-IMPORTEXTERNALORDER_SHOPIFYPOS.sql' go test ./internal/sqlsymbol -run '^TestLocalInterBaseExample$' -count=1 -v` — PASS; no fixture writes.
- `go test ./... -count=1` — PASS.
- `go test -race ./internal/sqlsymbol ./internal/handler -count=1` — PASS.
- `CGO_ENABLED=1 go test -tags interbase ./internal/sqlsymbol ./internal/handler ./internal/database -count=1` — PASS using existing mocks; no live DB mutation tests.
- `CGO_ENABLED=1 go build -tags interbase -o /tmp/opencode/sqls-symbol-navigation .` — PASS; ELF x86-64 artifact (46,337,032 bytes), `libgds.so` resolves at `/opt/interbase/lib/libgds.so`.
- `go test ./internal/sqlsymbol -run '^$' -bench 'BenchmarkProcedureSymbols' -benchmem -count=1` — PASS: 1,000-use benchmark 3,090,557 ns/op, 4,554,607 B/op, 28,147 allocs/op; scaling 250/500/1,000 uses = 758,020 / 1,513,977 / 3,220,494 ns/op, supporting near-linear scaling.
- With `SQLS_SYMBOL_EXAMPLE` set, `BenchmarkLocalInterBaseExample` — 7,480,066 ns/op, 7,454,051 B/op, 48,032 allocs/op.
- Editor handoff is discovery-only: Neovim config points at `~/gits/sqls/sqls`, live PID 3091738 uses that binary. It was not replaced or restarted. Actual editor rename/references on line 326 and definitions on line 847 remain unverified; use the artifact above after approval.
- Additional deferred-minor fixes: standalone CR offset/range conversions now agree; InterBase definition routing reuses request-local sqlsymbol analysis instead of re-analyzing the same document.
