# InterBase Procedure Symbol Navigation — Design Spec

Date: 2026-09-22
Status: proposed; awaiting written-spec review

## Purpose and agreed scope

Make go-to-definition, find-references, and rename useful while editing InterBase
stored procedure source in Neovim or another LSP client.

The user approved this scope:

- Definition, references, and rename for procedure-local variables and input/output
  parameters throughout their containing procedure in the current document.
- Definition for database tables and columns through the active InterBase
  connection and the existing source-snapshot mechanism.
- Case-aware symbol identity, SQL/procedural context awareness, and exclusion of
  comments and strings from references and edits.

Reference example, inspected locally:
`OneDrive/Dev/2026-09-14 - 5-IMPORTEXTERNALORDER_SHOPIFYPOS.sql`.

| Action | Acceptance result |
| --- | --- |
| Definition on `ORDERTOTAL` at line 847 | Declaration at line 55 in the same document |
| Definition on `HEADEREMPLYID_TEMP` | Declaration at line 42 |
| Rename `HEADEREMPLYID_TEMP` at line 326 | Edit declaration at 42 and uses at 324 and 326 |
| References on a declared variable | All resolved uses in that procedure, across semicolons |
| Definition on `amountpaid` at line 847 | `AMOUNTPAID` declaration in a `CUSTOMERINVOICE` table snapshot |

Line numbers describe the inspected revision of the example. Automated tests use
small synthetic fixtures expressing these same cases rather than copying the
business procedure into the repository.

## Current behavior and cause

- `internal/handler/handler.go` advertises rename and definition but neither
  advertises nor dispatches `textDocument/references`. This explains the standard
  Neovim `grr` unsupported response.
- `internal/handler/rename.go` calls `parseutil.ExtractIdenfiers`, which searches
  only the focused statement, and then compares identifier strings exactly.
- `parser/parser.go` groups statements at every semicolon; it does not model an
  InterBase procedure as a declaration scope. Uses at lines 324 and 326 therefore
  fall into different rename searches.
- `internal/handler/definition.go` resolves in-document aliases, not procedural
  declarations.
- `internal/handler/interbase_definition.go` supports catalog procedures, views,
  and triggers. It does not classify tables or columns as snapshot targets.
- The database layer already supplies table/column metadata through `DBCache`
  and table DDL through `DDLRepository.ObjectDDL` with `ObjectKindTable`.

## Approach

Add a small, shared, token-based symbol-resolution layer rather than expanding
whole-document spelling matches or replacing the SQL parser with a complete
InterBase grammar.

The layer recognizes procedure declarations and the procedural/SQL contexts
needed to distinguish their variables from database identifiers. All three LSP
operations use the same symbol identity and occurrence ranges. Existing AST
helpers remain available for SQL relation and alias analysis where their output
is sufficient; they do not define procedure scope through semicolon grouping.

### Component boundaries

1. **`internal/sqlsymbol` (new package):** pure analysis of document text and a
   `dialect.DriverVariant`. Owns token positions, procedure scopes, declarations,
   local occurrences, and SQL relation/column context. Returns semantic results
   and source spans, without database access, filesystem access, server state,
   or an LSP dependency.
2. **Handler integration:** `definition.go`, `rename.go`, a new `references.go`,
   and small shared adapter helpers translate LSP UTF-16 positions to source
   spans and results back to locations/edits. Handlers own request validation,
   document lookup, dialect selection, and operation-specific errors.
3. **Database definition integration:** `interbase_definition.go` resolves a
   relation/column descriptor against `DBCache` and extends its existing bounded
   DDL lookup and snapshot-writing path. No new database capability is needed.
4. **Protocol wiring:** `internal/lsp/lsp.go` gains reference request/context
   types; `handler.go` registers the method and advertises `ReferencesProvider`.

Analysis is request-local and built from the current in-memory document. Avoid a
long-lived symbol index in this version: edits cannot leave stale symbol ranges,
and local operations do not require catalog I/O. Use bounded passes over tokens
and index occurrences by declaration rather than rescanning the procedure for
every occurrence.

## Resolution rules

### Procedure scopes and declarations

- Recognize `CREATE PROCEDURE` and `ALTER PROCEDURE`, including input parameter
  lists, `RETURNS` lists, `AS`, and `DECLARE VARIABLE` declarations.
- Parameter-list commas inside type arguments, such as `NUMERIC(15,2)`, do not
  split declarations.
- Track the outer procedure body and nested `BEGIN`/`END` blocks. `CASE`/`END`
  expressions must not accidentally close a procedure scope.
- Support multiple procedures per file and scripts using `SET TERM`. Terminator
  directives delimit source units, not identifier occurrences.
- Nested procedural blocks share the containing procedure's declarations.
  Separate procedures have distinct symbol identities even for equal names.
- A missing final `END` while editing can leave the current scope open through
  EOF; a recognized subsequent procedure header ends the previous incomplete
  scope. Malformed or duplicate declarations make affected symbol resolution
  ambiguous rather than selecting a declaration arbitrarily.
- Trigger-local and anonymous-block scopes are future extensions; this version's
  procedure resolver does not claim to resolve them.

### Identifier identity and occurrences

- Apply the supplied InterBase dialect variant consistently. Unquoted identifiers
  fold according to InterBase rules. Delimited identifiers retain their semantic
  spelling; Dialect 1 double-quoted strings remain strings.
- A declaration is a symbol occurrence, and every use records the declaration
  it resolves to, not merely its spelling.
- `:name` resolves to a procedure variable/parameter when declared. The editable
  span covers `name`, excluding the colon.
- Bare identifiers in procedural assignment targets and procedural expressions
  resolve to declared locals. This includes conditions and arguments to scalar
  functions in procedural expressions.
- SQL relation names, column names, aliases, callable names, and qualified member
  names are classified by their syntactic role. Bare names in SQL column
  contexts must not become variable references just because a local shares the
  name. Output-target contexts such as `SELECT ... INTO` and procedure
  `RETURNING_VALUES` resolve to locals, including colon-prefixed targets.
- For `where invoice=:invoice`, the left `invoice` is a column and the right
  `invoice` is a local symbol. For `SELECT EMPLYID FROM ... INTO :EMPLYID`, only
  the output target is a local use.
- Comments, string literals, and identifier substrings are never occurrences.
- Model resolution as distinct outcomes: resolved local, resolved SQL role,
  unresolved/ambiguous procedure context, and outside a supported procedure.
  An unresolved local must not fall through into a same-spelled catalog object.

## LSP behavior

### Definition

Within a recognized InterBase procedure, resolve the cursor's semantic role
first. A local symbol returns its declaration's exact identifier range in the
current URI. A SQL relation or column goes through contextual database resolution.
An ambiguous procedural symbol returns no location.

Preserve existing alias/subquery navigation and procedure/view/trigger snapshots
where applicable, with local procedure identity taking precedence over unrelated
same-spelled aliases or catalog objects. Outside the procedural path, preserve
the existing generic definition behavior while adding contextual InterBase
table/column targets.

### References

Implement `textDocument/references` and honor `context.includeDeclaration`.
Return deterministic, source-ordered locations in the current document. Exclude
the declaration when the flag is false. Unsupported targets and unresolved
symbols return an empty result. Capability advertisement indicates method
support; it does not promise workspace-wide or database-wide reference indexing.

### Rename

For a resolved local, return edits for its declaration and all resolved uses in
the containing procedure. Apply the requested spelling consistently while
preserving occurrence prefixes such as `:`. Use the same occurrences as
references, avoiding duplicate or overlapping edits.

Validate that the replacement is one legal identifier for the selected dialect
and does not collide with another declaration in the scope. Reject invalid or
colliding names with a clear request error. If unsupported syntax could hide
additional occurrences of the selected symbol, reject rename rather than return
a knowingly partial edit; read-only navigation can still use proven results.

Within a recognized procedure, database columns and other unsupported targets
must not use the old spelling-only rename fallback. Return a clear unsupported
target error. Outside that path, preserve existing generic rename behavior.

Use the existing `WorkspaceEdit.Changes` representation for the new local rename
path to avoid inventing a document version: the current versioned-edit type
stores an integer and existing rename hard-codes version zero. Edits apply only
to the document from which this request's text was read.

### Positions and errors

Use zero-based UTF-16 positions at the LSP boundary and explicit conversions to
the token/source representation. Test token starts, interiors, and ends, CRLF,
and non-ASCII text before an identifier. Returned identifier ranges exclude
prefix punctuation and include any delimiters that a replacement must replace.

Malformed request parameters and missing documents follow existing handler
errors. A definition/reference miss is a normal empty result. Local resolution
uses the selected InterBase variant without requiring a fresh catalog read; the
current server still selects its variant through the active connection.

## Database table and column definitions

- Resolve tables from relation positions in `SELECT`, `INSERT`, `UPDATE`, and
  `DELETE`, including relation aliases.
- Resolve columns in those statements to their owning relation. In the user's
  example, an `UPDATE` assignment target belongs to `CUSTOMERINVOICE`.
- In a multi-relation query, use qualifiers and cached column metadata to find a
  unique owner. Keep nested query scopes separate and resolve correlated
  references only when the owner can be proven. An ambiguous unqualified column
  returns no definition rather than choosing the first table.
- Validate column ownership against the current cache. Respect exact delimited
  names when checking descriptors, even where cache accessors fold lookup keys.
- Extend the snapshot target with the owning object and an optional column
  locator. Fetch DDL for the owner using the existing repository capability,
  timeout, connection-specific storage, and cleanup behavior.
- For a column, locate its actual declaration in the DDL token structure; do not
  use the first substring match, which could land on a comment, constraint, or
  similarly named column. If no declaration can be located, return no column
  definition. Table navigation points to the table declaration.
- Retain existing procedure/view/trigger source fallback behavior. If table DDL
  cannot be reproduced, return no definition; do not invent executable DDL from
  partial metadata.

## Verification

### Pure resolver tests

- Reduced versions of the three user examples, spanning several SQL statements.
- Input/output parameters and locals; bare and colon-prefixed occurrences;
  case variations; identifier-prefix collisions.
- Same-named columns versus locals, SQL aliases, function names, nested SQL
  scopes, and output targets.
- Comments and single-/double-quoted strings under both InterBase variants;
  delimited identifiers under Dialect 3.
- Multiple procedures, nested blocks, `CASE`, `SET TERM`, and incomplete source.
- Rename collisions, invalid names, and unsupported/ambiguous contexts.
- UTF-16 conversion and exact source ranges.

### Handler and snapshot tests

- JSON-RPC reference dispatch and capability advertisement.
- Reference declaration inclusion/exclusion and deterministic locations.
- Definition/rename integration with exact locations and edits; apply edits to
  a fixture and compare the resulting SQL to the intended rename.
- Mock-catalog table and column targets, aliases, ambiguous ownership, missing
  metadata, unavailable DDL, and correct declaration ranges in snapshots.
- Existing generic alias/rename and InterBase snapshot regression tests.
- Local operations perform no DDL fetch.

Run focused resolver/handler tests during implementation, then the relevant Go
package suite and repository-wide tests under the standard build. Run the
InterBase-tagged affected tests with the local SDK when available, and record any
environmental limitation explicitly. Measure request-local analysis on the
user's full file without adding its business source to the test fixtures.

Document the supported scope and Neovim mappings in `README.md`. Final editor
verification uses the approved example and a rebuilt InterBase-enabled binary;
record separately whether the actual Neovim session was tested.

## Boundaries

This version provides local source refactoring and catalog-backed navigation.
Workspace-wide references, database-object rename/migrations, trigger-local
resolution, a full InterBase grammar, and changes to completion/hover are outside
the agreed scope. Editing source or a generated snapshot does not execute SQL.
