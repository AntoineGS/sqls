# InterBase Catalog Names and Legacy DDL Navigation — Design Spec

Date: 2026-09-22
Status: proposed for written review

## Purpose and agreed behavior

Make InterBase catalog reads and table navigation accurate on real databases,
including older Dialect 1 metadata. The user wants the truncation fixed at its
source, rather than patched by name matching in the editor. When the catalog
does not retain enough information to reproduce an original SQL declaration,
the user chose strict executable DDL alongside a clearly labeled normalized
catalog view. The source of every displayed name and type must remain clear.

Acceptance examples in the read-only file
`~/OneDrive/Dev/2026-09-09 - 3-IMPORTEXTERNALORDER.sql`:

- `gd` on `import_order_line_items` at line 265 and on
  `import_order_payment` at line 266 reaches the correct table name in a
  database-backed snapshot; column definitions reach their corresponding
  catalog column entries.
- The former table must retain its complete 23-character identifier, including
  its final `S`, throughout driver catalog lookups and column ownership.
- The latter has column `AMOUNT` sourced from `RDB$16082`: field type 27
  (DOUBLE), scale -2, subtype 0, precision NULL. Navigation remains available
  without presenting a guessed original numeric declaration as fact.

The example file and database are read-only verification inputs; implementation
does not change them. The user's active Neovim client is not replaced or
restarted as part of implementing this spec without a separate request.

## Confirmed causes and limits

`interbase-go/native.c` currently calculates the number of characters in
`SQL_TEXT` as the descriptor's byte width divided by a charset maximum width.
On this catalog, a UNICODE_FSS identifier descriptor is 67 bytes and contains
the complete string, but the driver's decode stops after 22 ASCII characters.
The separately implemented `sqls` bulk completion cache already casts these
identifiers to `VARCHAR(67)` and sees complete names. The driver's one-shot
`schema.Catalog.Table` still reads the uncast `CHAR` column; its truncated
relation name cannot load columns, and `GenerateDDL` returns `columns are
unavailable`.

Simply uncapping all `SQL_TEXT` is unsafe: a different descriptor can hold a
`CHAR(5)` value in 20 UTF8 bytes, and the existing native test requires five
characters of padding rather than all 20 bytes. The descriptor need not supply
the declared character width. A decoder must not infer intentional trailing
spaces from content, and an unknown width must not become a fabricated exact
length.

`IMPORT_ORDER_PAYMENT` has a separate problem. Its metadata is complete as
stored, but does not contain the original SQL spelling or precision for the
legacy scaled DOUBLE. `interbase-go/schema/ddl.go` rejects this in the strict
DDL renderer. Both `sqls/internal/database/interbase_catalog.go` and MDExplorer
(`~/gits/multidev/Common/MetadataProvider.InterBase.pas:530-544`) display a
conventional `NUMERIC(15, 2)`. MDExplorer's IBX table extractor labels this
branch “Take a stab at numerics and decimals”
(`Composantes/IBX_Copy/IBExtract.pas:515-527`). The reconstruction is useful,
but does not prove that the original declaration had that keyword or precision.
Replaying this normalized text under another SQL dialect could change storage
semantics. A table's original `CREATE TABLE` text is not available as a
verbatim catalog source, unlike procedure or view source.

## Component design and data flow

### 1. Driver catalog identifier reads (`../interbase-go/schema`)

Move the `VARCHAR` projection that already works in the `sqls` bulk loader to
the reusable driver catalog reader. Apply it to **every identifier returned by
the catalog API** that participates in matching or grouping: relation, column,
domain, constraint, index, procedure, charset/collation, and related object
names. Keep joins, predicates, and orderings on their original catalog columns;
only selected result values change representation. Avoid hand-patching
`Catalog.Table` or matching truncated names to guessed suffixes. Identifier
width must be established from the catalog's declared byte width for the
relevant source domain and checked against the SQL dialect's supported
`VARCHAR` width; an unsupported width is a visible read error, never a silent
truncation. Preserve exact-name filtering and case, then strip only fixed
catalog padding at the schema boundary. The existing `sqls` bulk projection
continues to work; it can share a strategy later but is not a prerequisite for
the driver fix.

Catalog invariants: a full relation name yields its full ordered column list;
distinct long identifiers sharing a 22-character prefix remain distinct;
index and constraint references point to the same full names returned by the
object listing. A failed or incomplete catalog read cannot be mistaken for an
empty table.

### 2. General `SQL_TEXT` decoding (`../interbase-go/native.c`)

Replace the maximum-bytes-per-character *cap* with a length decision based on
evidence available for that result column. Preserve known declared `CHAR`
widths, including explicit fixed-width casts and attributable columns when
their catalog character length can be proved; preserve literal trailing
spaces and distinguish them from fixed-width padding. When no declared width
is knowable, do not discard bytes by dividing `sqllen` by a charset maximum;
return the full fixed-width buffer with its padding rather than silently losing
identifier or user data. Schema catalog accessors must use the precise
`VARCHAR` projections above instead of relying on this conservative unknown-
width fallback. Keep charset conversion on complete byte sequences and guard
against allocation/length overflow. Do not trim user strings in the decoder.

This is a general driver correction, but it does not claim that an old fixed
`CHAR` descriptor lacking any declared-character-width metadata can reveal
whether trailing spaces were intentional. Tests must distinguish known-width
results from this explicitly unknown-width case.

### 3. Strict DDL versus normalized catalog view

Keep `schema.Domain.SQLType` and `schema.Relation.GenerateDDL` strict: do not
silently turn `RDB$16082` into an allegedly faithful executable declaration.
Add a separate read-only, explicitly non-executable catalog description surface
for table navigation. Its table and column names come from complete catalog
identity, and its type labels follow the same convention as MDExplorer for
legacy scaled DOUBLE: `NUMERIC(15, -scale)` annotated as a normalized legacy
display, not recovered SQL. If a field supplies real subtype and precision,
display the actual recorded values without that annotation. Other uncertain
or unsupported types retain their raw type metadata and a reason; no guessed
type is inserted into an executable `CREATE TABLE`.

`sqls/internal/handler/interbase_relation_definition.go` first requests
strict DDL as today. For `ErrUnsupportedDDL` on a confirmed catalog table, it
creates a labeled informational snapshot with exact table and column names,
type provenance, and the DDL refusal reason. The snapshot is not SQL to run;
definition locations use recorded spans of the rendered names rather than a
`CREATE TABLE` parser. Do not treat an absent object, missing/ambiguous column
ownership, a failed catalog read, or a connection error as unsupported DDL:
those cases continue to return no misleading definition. Views and procedure
verbatim-source behavior stays on its existing path.

The normalized type interpretation should have a single shared rule in the
driver/database presentation layer, reused by the `sqls` fallback and display
surfaces, rather than a new conflicting type table inside the LSP handler.
Adding a distinct description API must not change the strict `DDLRepository`
contract or silently emit executable SQL. The implementation plan will pin the
smallest API boundary that allows this reuse while leaving mocks and non-
InterBase drivers unaffected.

## Verification and rollout

1. Red/green driver tests: long catalog identifiers (including collision pairs),
   exact-name column/constraint lookup, a catalog domain with a byte width
   different from the live fixture, and an unsupported `VARCHAR` width. Include
   native `CHAR` tests for known-width UTF8/UNICODE_FSS and single-byte padding,
   actual multibyte characters, trailing-space literals, fixed casts, unknown
   widths, and non-UTF8 attachments.
2. Red/green schema and `sqls` tests: strict DDL still rejects legacy scaled
   DOUBLE; the separate display states the recorded `(type=27, scale=-2,
   subtype=0, precision=NULL)` and normalized `NUMERIC(15, 2)`; both table and
   column `gd` lead to correct informational spans when strict DDL refuses;
   successful executable DDL still takes precedence. Not-found and I/O errors
   never produce a fabricated snapshot.
3. Read-only live checks against the configured InterBase connection: compare
   driver catalog names/columns with independent `VARCHAR` catalog reads;
   exercise both named lines and another long-name collision; inspect native
   values and strict rejection before and after the change. Do not run generated
   DDL against a production database. If replay equivalence of a normalized
   type is ever required, test it only on a disposable Dialect 1 fixture and
   compare its physical metadata; this spec does not promote normalized text
   to executable DDL.
4. Run relevant driver native, schema, integration and `sqls` unit/protocol
   suites, then build a native `sqls` binary from the local replaced driver.
   Installing/restarting the editor is a distinct, user-directed acceptance
   step after tests pass.

## Boundaries

No changes to the live SQL file, database schema, or MDExplorer. No guessing
original numeric precision, relaxing strict DDL's error contract, or mutating
LSP results to point to an unrelated table. This work has two repositories:
`interbase-go` owns catalog and native decoding; `sqls` owns the LSP snapshot
and definition experience. Source-level fixes land before depending on them in
the editor.
