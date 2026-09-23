# InterBase static diagnostics for unused symbols and string widths

## Intent and scope

Give InterBase SQL authors inline hints for unused procedure declarations and warnings where a known string expression can exceed a known destination width. The first release covers procedure assignments and table writes, including `UPDATE`, `INSERT ... VALUES`, and `INSERT ... SELECT`. Diagnostics are advisory: a wider source type warns even though a particular runtime value might fit. This is an InterBase-first feature, not a cross-dialect SQL linter.

Only unambiguous, statically supported constructs produce diagnostics. Unknown types, incomplete statements and unsupported expressions do not imply an overflow. No SQL is executed or prepared to obtain diagnostic types.

## Diagnostic contract

- A local `DECLARE VARIABLE` or input parameter with no proven read gets an `Unused` Hint (severity 4) on its declaration. Assigning to it does not constitute a read. Output parameters are exempt. An unresolved or ambiguous occurrence that could read the symbol suppresses its unused hint rather than creating a false positive.
- A known source maximum character width greater than its destination's known maximum gets a `Possible string truncation` Warning (severity 2) at the assignment, target column, or corresponding projected expression. The message names source and destination widths and identifies the assignment. A fitting literal or inferred expression does not warn.
- All diagnostics have stable `sqls` source and rule codes and use UTF-16 LSP ranges. An empty list replaces old diagnostics when a document no longer has findings.
- Comparing widths means **character** lengths, not byte storage lengths. `CHAR(n)` and `VARCHAR(n)` supply bounds; unknown-length text, unsupported types, and expressions without a safe maximum remain unknown.

## Analysis model

Extend the existing `internal/sqlsymbol` InterBase document analysis rather than building a parallel name binder. Preserve its symbol resolution and SQL ownership rules. Add a small, independently testable diagnostic pass with these inputs: source text and bound occurrences, declared procedure types, and an optional read-only snapshot of the cached catalog. It returns source spans and rule findings without depending on LSP or a live connection. Expose enough declaration/type and occurrence-role information to distinguish reads from writes while leaving rename/reference semantics intact.

The width evaluator returns either a proven upper bound or unknown. Its initial rules cover quoted string literals (Unicode character count with SQL escaping), resolved variables and catalog columns, explicit `CAST(... AS CHAR(n)/VARCHAR(n))`, string concatenation (sum of known operand bounds), bounded `SUBSTRING` with a statically known length, and `TRIM` (cannot increase a known input bound). Function names with no explicitly tested width rule, nonconstant bounds, numeric/nullable results whose string width is not defined, ambiguous column ownership, and unsupported compound expressions yield unknown. A cast's declared result width is its result bound; explicit narrowing in the cast is not separately diagnosed by the assignment rule. Width inference is an upper-bound computation, not a prediction of actual runtime values.

Match sources to destinations only when their ownership and ordering are established:

- In procedures: local/output-parameter assignments and positional output assignments such as `SELECT ... INTO` when the source projection and targets can be matched safely.
- In `UPDATE`: each supported `SET` pair, resolving the target from the updated relation and the source from local symbols or visible SQL relations.
- In `INSERT ... VALUES`: each value against its explicitly named destination column, for each values row supported by the InterBase grammar.
- In `INSERT ... SELECT`: each projected expression against its explicitly named destination column by position; source relations, aliases and joins must resolve unambiguously. Expand stars only when both ownership and catalog projection order are proven; otherwise skip the affected projection. Do not guess implicit target column ordering, derived projection types, or nested query mappings.

Keep parsing tolerant of in-progress edits: a malformed region suppresses findings dependent on that region while independent, fully parsed procedure declarations/statements can still report. When uncertainty affects whether a declaration has been read, prefer suppressing its unused hint. A genuinely valid but currently unsupported expression is likewise silent, not an error diagnostic.

## LSP integration and lifecycle

Publish `textDocument/publishDiagnostics` for InterBase documents on open, change, and save when save supplies changed text. Use the in-memory document snapshot, active dialect and available cache snapshot without holding mutable server state across analysis or network publication. Include the document version and guard against delayed computations overwriting newer edits or a newer connection generation. Clear diagnostics on close, when switching away from InterBase, and when findings disappear. Recompute open InterBase documents on connection/catalog changes so a newly available catalog enables table-width warnings and stale table warnings disappear after a switch.

The handler converts analysis spans to LSP ranges with the existing UTF-16 position helpers; protocol notification types live in `internal/lsp`. Catalog access is read-only and in-memory during edits. Other dialects do not produce these InterBase-specific findings. Diagnostic failures are logged or suppressed without breaking normal document notifications.

## Verification

Test the pure analysis on local/input/output declarations, read-vs-write and ambiguous occurrences, literal and field-to-variable widths, `CAST`, concatenation, `SUBSTRING`, `TRIM`, unknown functions, `UPDATE` with aliases, multi-column/multi-row `INSERT ... VALUES`, and ordered `INSERT ... SELECT` with both resolvable and unresolved projections. Check exact source ranges and conservative absence of false positives, including incomplete edits and unavailable catalog metadata. Test LSP notifications for severities/codes, UTF-16 positions, replacement on edit, close/switch clearing, cache refresh and prevention of stale publications. Run the Go test suite and race-sensitive handler tests relevant to the document lifecycle.
