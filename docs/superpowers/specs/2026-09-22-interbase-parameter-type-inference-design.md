# InterBase input-parameter type inference

## Goal and scope

For InterBase queries run from Neovim, prompt directly for a value when the
engine can reliably describe the named input parameter's type. Retain the
existing type picker when the type is unavailable, unsupported, or ambiguous.
In the Snacks value popup, Ctrl-T toggles SQL `NULL` without losing the draft
value. This changes discovery and editor prompting; statement execution still
uses the existing validated, bound-parameter path. Other SQL drivers and
procedure-definition editing remain as they are.

The existing parameter scanner knows names and occurrence order, not types.
`interbase-go` already obtains input SQLDA descriptors with
`isc_dsql_describe_bind` during native prepare, but exposes only the input count.
Metadata must come from preparing the executable SQL, not from guessing column
types by parsing SQL or from converting every input to text.

## Driver boundary

Add a prepare-only input-description API to `interbase-go`, patterned after
its pooled `Plan(ctx, *sql.Conn, query)` helper. Use a separate optional
driver-connection interface rather than changing the existing `Introspector`
interface. Return a positional slice of copied descriptors (SQL type, subtype,
scale, precision, and nullability where the engine supplies them). Native code
copies fields from the prepared statement's input SQLDA before closing it; no
native pointer or connection-scoped state escapes to Go callers.

Preparing must not execute SELECT, DML, or procedures. Reuse the driver's
current prepare transaction, cancellation, connection-invalidating error
handling, and close-on-every-path conventions. The helper accepts an existing
`*sql.Conn` so it does not open another attachment. Unit tests cover positional
metadata, empty inputs, cancellation, and cleanup; live integration tests
confirm engine-reported types without executing statements.

## sqls discovery and binding

`getQueryParameters` continues to compile the selected SQL (including the
standalone `FOR SELECT`/`INTO` normalization) to discover unique names and the
order of `?` occurrences. Only when there are named inputs does it prepare
each executable statement for descriptors through an optional InterBase
repository capability. Match descriptor positions to the compiled occurrence
keys, including repeated names and multi-statement selections. Suggest a type
for a name only if every occurrence has a compatible engine type. A missing,
unexpected-count, unsupported, or conflicting descriptor leaves that name
without a suggestion; other names in the selection can still be inferred.

Map character inputs to `text`; unscaled integer inputs to `integer`;
floating-point inputs to `number`; dates, timestamps, and booleans to their
existing wire types. For scaled exact `NUMERIC`/`DECIMAL`, use decimal text
bound as a string: `number` currently becomes `float64`, which can lose
precision, whereas the driver handles a decimal string against a scaled input
descriptor. Expose a human-readable engine type label for prompts so a
decimal value is not misleadingly labeled “text.” Types without a safe
existing value conversion (including TIME, binary BLOB, and arrays) use the
picker. Preserve dialect-specific limitations; do not force an inferred type
when an exact mapping is uncertain.

Add optional `inferredType` and `databaseType` label fields to each discovery
parameter; leave the current discovery protocol version and submission shape
compatible with older clients. Inference never modifies the SQL sent for
execution.
Preparation errors and per-statement metadata timeouts fall back to the
picker for the affected statement; request cancellation stops discovery.
Fallback does not execute the statement, fabricate a type, or suppress a later
execution error. Connection/query identity checks and server-side value
validation remain authoritative. Do not persist inferred types across
connection generations or schema changes.

## Neovim interaction

For a parameter with `inferredType`, go straight to a value popup instead of
showing `vim.ui.select` for type. Use the inferred type for conversion and
validation, even if a remembered type differs; only reuse a remembered value
when it is compatible. Booleans accept `true`/`false` in a value popup so the
NULL toggle is available there too. Without an inference, keep the current
type picker and its `null` choice.

When Snacks owns `vim.ui.input`, configure only these sqls value popups with a
Ctrl-T action. It visibly toggles a NULL state, preserves text typed before
the toggle, bypasses non-NULL validation while active, and submits the existing
`{type="null", value=""}` format on confirmation. Toggling off restores the
draft value and validation. A remembered NULL reopens in NULL state. Canceling
still abandons the draft without changing remembered values. If another
`vim.ui.input` provider is active, use the existing type picker so NULL remains
accessible; no global input keymap is installed.

## Verification

Driver unit and native live tests verify described types and prepare-only
behavior. sqls unit tests cover descriptor-to-name matching, repeated names,
multiple statements, no-connection/failed-prepare fallback, and decimal
precision preservation. Neovim headless tests cover direct value entry,
Ctrl-T NULL on/off and cancellation, remembered values, boolean input, and
picker fallback. End-to-end read-only InterBase and installed Neovim smoke
checks verify the normal SQL execution and editor flow before installation.
