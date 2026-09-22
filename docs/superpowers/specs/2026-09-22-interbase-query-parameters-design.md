# Named query parameters with remembered Neovim prompts

Status: approved and implemented on `feat/interbase-query-parameters`. Verified
by the offline suites and by a read-only live pass against a staged build on the
authorized connections; installation of that build as the local sqls binary is
still pending.

## Intent and agreed behavior

The user wants to execute Delphi-style SQL containing placeholders such as
`:EMPLYID` from Neovim, enter the values interactively, and see the usual sqls
results. The user explicitly selected **previous values prefilled** on later
runs. Values remain in memory, scoped to the connection and query.

Example:

```sql
SELECT * FROM EMPLOYEES WHERE EMPLYID = :EMPLYID;
```

The SQL buffer remains unchanged. The value is a bound parameter, never SQL
text interpolation. Repeating a placeholder reuses one entered value.

## Existing system and implementation boundary

- Neovim currently uses `nanotee/sqls.nvim` and the locally built native sqls.
- `executeQuery` currently takes a document URI, optional vertical-output flag,
  and optional range. It cannot accept bound values.
- `DBRepository.Exec` and `Query` have no argument parameters. Keep this shared
  interface unchanged; add optional parameter-execution capabilities instead.
- `interbase-go` already supports positional binding through `database/sql`;
  it does not support named parameters. No driver API change is needed.
- Preserve read-only SELECT transactions, output-producing procedure routing,
  cancellation classification, partial results, and once-only execution.
- Implement the Neovim adapter in the user's maintained configuration, using
  the installed plugin's result display. Do not edit its downloaded checkout.

This first version supports named input parameters on InterBase connections.
Unparameterized execution continues working for existing clients and drivers.
PSQL declarations/bodies and unnamed `?` input prompts are outside this feature.

## Chosen approach

Use two stages: discover parameters without executing SQL, then submit values
for execution. This fits Neovim's asynchronous prompts without keeping a
database connection lock or transaction open while the user is typing.

Client-only text replacement would be smaller but would conflate values with
SQL syntax and require quoting rules in the editor. It is rejected. Adding
named-parameter support to the native driver would widen the driver contract
unnecessarily; server-side translation to existing positional bindings is
sufficient.

### Discovery

A sqls-specific command accepts the same document/range selection as execution.
It returns:

- unique parameter names in first-appearance order;
- an opaque stable connection identity, excluding passwords;
- the current connection generation for stale-submission detection;
- an opaque identity for the exact selected SQL and resolved dialect;
- enough selection/document information to validate the eventual submission.

Discovery performs no SQL execution and needs no parameter-value storage on
the server. An unchanged selected query has the same query identity; changing
its SQL invalidates the old prompt context.

### Recognition and rewriting

- A placeholder is `:` followed by `[A-Za-z_][A-Za-z0-9_$]*`.
- Names are case-insensitive, consistent with ordinary unquoted InterBase
  identifiers; prompts retain the first spelling for display.
- Ignore names inside string literals, quoted identifiers, line comments and
  block comments. Honor doubled quotes and the resolved SQL dialect.
- Do not interpret `::` or `:=` as named placeholders.
- Rewrite genuine named occurrences to `?` and construct a positional argument
  list in occurrence order. Repeated occurrences duplicate the bound value in
  that list, not the UI prompt.
- Scan and validate the entire selected batch before executing its first
  statement. Reject mixed named/unnamed placeholders and unsupported statement
  forms before running any statement.
- Apply this to SELECT, INSERT, UPDATE, DELETE and EXECUTE PROCEDURE input
  expressions. Do not interpret PSQL local-variable references as user inputs.
- Correct the observed leading-comment classification defect for these
  execution paths: classification uses the first meaningful SQL token.

### Submission and execution

The execution request includes tagged values and the discovery identity.
The server rechecks the selected SQL and active connection under its existing
locks. If either changed while prompting, reject the submission and ask the
user to run the command again; do not silently execute another buffer version
or reconnect to another database.

Validate the complete parameter set and all tagged values before any execution.
Missing, extra or duplicate canonical names are errors. Reject unsupported
parameter capabilities before executing any statement in a batch.

Repository adapters call `QueryContext`/`ExecContext` with arguments. The
parameterized read-only query capability binds through its existing read-only
transaction. A procedure with output still takes the existing query route and
never a read-only transaction; there is no retry on the other route.

A statement failure stops the batch with the existing result/error semantics.
This feature does not add whole-batch atomic transactions or automatic retries.

## Neovim interaction

`Space S q`, `:SqlsExecuteQuery`, the vertical variant, and the Execute Query
code action use the parameter-aware adapter. Range execution discovers and
prompts only for names within the selected statements.

For each distinct parameter:

1. Select a type. The previously used type is first; a new parameter defaults
   to Text so identifiers such as `000123` are preserved.
2. Enter the value with the previous value prefilled. NULL needs no value field.

Use the user's existing `vim.ui.select` and `vim.ui.input` integrations. Pressing
Escape at any prompt aborts the entire operation before an execution request.
Only a fully completed prompt sequence replaces the previous cached values.
There is no silent execution using remembered values.

Supported types:

| Type | Input and binding |
| --- | --- |
| Text | Literal string, including empty string; no surrounding SQL quotes needed |
| Integer | Signed decimal integer validated as int64 |
| Number | Finite floating-point number validated as float64; reject NaN/infinity |
| Date | `YYYY-MM-DD`, validated calendar date |
| Timestamp | `YYYY-MM-DD HH:MM:SS[.fraction]`, database wall-clock value without timezone conversion |
| Boolean | Explicit true/false choice |
| NULL | Bound nil; distinct from both empty string and the text `NULL` |

Transmit numeric values as tagged strings to prevent JSON/Lua from rounding
large integers before server validation. Text remains available for exact
decimal strings with an explicit SQL CAST; Number is explicitly floating point.

## Prefill lifetime and isolation

- Cache key: LSP client instance + stable connection identity + exact selected
  query identity. Value records include canonical name, type and entered text.
- Switching from NRF01 to centrale never reuses NRF01's values. Switching back
  can recover NRF01's values during the same client lifetime.
- Connection generation protects an in-progress submission, but is not the
  stable prefill key: reconnecting must not accidentally mix identities.
- Editing the selected SQL creates a new entry; identical selected SQL in
  another buffer may reuse values on the same connection.
- Keep at most 100 query entries per client, evicting least-recently-used entries.
- Clear that client's entries when the LSP client stops. Nothing is persisted
  to files, buffers, command history or the dotfiles repository by the adapter.
- Provide `:SqlsClearParameters` to clear the client's remembered values/types.
- Do not include entered values in adapter diagnostic messages.

Explain remains prepare-only. Named placeholders can be translated to positional
markers for Explain without prompting for values, since values are not needed
to prepare a plan. Existing unsupported-statement refusals still apply.

## Validation

Meaningful regression coverage must demonstrate:

1. Repeated and mixed-case names bind in occurrence order; quoted/commented
   colons, doubled quotes, `::` and `:=` do not become prompts.
2. Selection boundaries and multi-statement batches get the correct argument
   lists, with full missing/invalid-value validation before the first execution.
3. Leading comments no longer misroute SELECT through Exec.
4. Text with quotes, empty string, NULL and int64 values above 2^53 survive
   unchanged; invalid numeric/date input is rejected.
5. SELECT stays read-only, procedures execute once on the correct route, and
   cancellation and partial-result behavior remain intact.
6. Changed SQL or connection while prompting executes nothing.
7. Neovim prefills the previous type/value, isolates connections and queries,
   clears/evicts entries, and does not execute after prompt cancellation.

Run ordinary and InterBase-tagged build/vet/tests and the sqls race target.
Verify the installed Neovim adapter with controlled prompt responses and actual
result-preview handling. Live validation on the user-authorized centrale and
NRF01 connections is limited to bounded read-only parameterized SELECTs and
prepare-only Explain; use fixtures for mutation/cancellation behavior.

The unrelated catalog-startup performance problem remains a separate task.
