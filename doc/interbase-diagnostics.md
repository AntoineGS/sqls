# InterBase SQL diagnostics

`sqls` performs static analysis of InterBase SQL (SELECT/DML, `CREATE
PROCEDURE`, `CREATE TRIGGER`) and publishes findings as LSP
`textDocument/publishDiagnostics` notifications. Diagnostics are computed
entirely from the document's own text plus whatever catalog metadata has
already loaded; they never issue additional database queries.

Diagnostics only run for connections whose driver is `interbase`. Every
other driver always publishes an empty `diagnostics` array.

## Rule codes

Every finding carries a stable `code` and a default severity level. The
default level is what a rule reports when its `diagnostics.rules` entry is
absent or set to `default`.

| Code | Default level | Description |
| --- | --- | --- |
| `interbase-unused` | `hint` | A declared local variable or parameter that is never read. |
| `interbase-string-truncation` | `warning` | A provable width mismatch assigning a wider source expression to a narrower destination column or variable. |
| `interbase-singleton-select` | `warning` | A singleton `SELECT` (`SELECT ... INTO`, or a query expected to return at most one row) whose predicates do not provably constrain a complete primary key or unique index for every relation it reads. |
| `interbase-null-comparison` | `warning` | A direct `= NULL` / `<> NULL` / `!= NULL` comparison, which InterBase always evaluates to `UNKNOWN` rather than true/false. Use `IS [NOT] NULL` instead. |
| `interbase-unknown-variable` | `error` | A procedural-variable position (an assignment target, or a `SELECT ... INTO` / `EXECUTE ... RETURNING_VALUES` target) that does not match any declaration in scope. |
| `interbase-duplicate-declaration` | `error` | A local variable or parameter declaration that reuses a name already declared earlier in the same procedure or trigger. |
| `interbase-unknown-relation` | `error` | A name in a relation position (`FROM`/`JOIN`/`UPDATE` target/`INSERT INTO` target/`DELETE FROM` target) that does not match any known table, view, or procedure. |
| `interbase-unknown-column` | `error` | A qualified or unqualified column reference that does not match any column of any relation it could plausibly belong to. |
| `interbase-unknown-qualifier` | `error` | A qualifier (`alias.column`) that does not match any relation alias or table name in scope. |
| `interbase-ambiguous-column` | `error` | An unqualified column reference matched by more than one relation in the innermost scope. |
| `interbase-target-count` | `error` | A mismatch between a statement's own target list (`INSERT` column list, `SELECT ... INTO` targets, a `UNION` arm's projection, or an `EXECUTE PROCEDURE RETURNING_VALUES` list) and the number of values/columns/outputs it is matched against. |
| `interbase-procedure-arity` | `error` | A mismatch between the number of arguments supplied to a procedure call and its known declared input count. |
| `interbase-invalid-assignment` | `error` | An assignment (procedure local, `UPDATE`/`INSERT` target, `SELECT ... INTO`, `EXECUTE PROCEDURE` input, or trigger `NEW.<col>` write) whose source is proven to never fit its destination's engine-enforced storage range. Also reports an explicit `CAST(... AS type)` whose own inner conversion is proven invalid, at the `CAST`'s own span, even when the assignment's outer destination would otherwise accept the `CAST`'s result. `RETURNING_VALUES` targets are listed among this rule's contexts structurally but can never actually produce a finding for it: a procedure's declared `OUTPUT` type has no source expression of its own to judge (see `interbase-lossy-assignment`, which can and does fire there). |
| `interbase-lossy-assignment` | `off` | Same eight assignment contexts as `interbase-invalid-assignment` (including `RETURNING_VALUES` targets, where this rule -- unlike `interbase-invalid-assignment` -- can and does fire), but for a source proven to be *accepted* while still narrowing, rounding, or truncating some value of its own declared range or scale. A `CAST(...)` source is judged like any other expression here, using the `CAST`'s own declared type/value as its result -- there is no suppression for an explicit `CAST`. Off by default; enable it with an explicit `diagnostics.rules` entry. |

Every check withholds a finding rather than guessing: a code is reported
only when the relevant catalog metadata has finished loading and proves the
condition. As metadata categories (relations, columns, procedures,
domains, views, indexes) become ready, previously withheld findings appear
in a later publication for the same document.

### Safe examples and limitations for the assignment rules

A widening assignment (for example, assigning a `SMALLINT` source into an
`INTEGER` destination, or a literal that already fits its destination's
range and scale exactly) never fires either assignment rule. Narrowing an
`INTEGER` source into a `SMALLINT` destination, or assigning a literal that
requires rounding to fit its destination's scale but still lands in range
(for example `100.5` into an `INTEGER` column), fires
`interbase-lossy-assignment` once enabled, never
`interbase-invalid-assignment`. Wrapping the source in an explicit `CAST`
does not suppress `interbase-lossy-assignment`: the `CAST` result is
checked against the real destination exactly like any other expression,
using the `CAST`'s own declared type as its type/value -- for example
`CAST(A AS INTEGER)` assigned into a narrower `SMALLINT` destination still
fires, because the `CAST` converted to `INTEGER`, a type unrelated to (and
wider than) the actual destination. `interbase-invalid-assignment`'s own
`CAST` handling is separate: it judges the `CAST`'s own inner conversion
independently, at the `CAST`'s own span.

Both assignment rules withhold entirely, rather than guess, whenever a
destination or source type cannot be resolved to a known InterBase storage
range -- this notably includes `FLOAT`/`DOUBLE PRECISION` and other
approximate-numeric types, whose storage layout is not currently modeled
(see [interbase-diagnostic-semantics.md](interbase-diagnostic-semantics.md)
for the full list of rules left disabled for lack of a verified rule).

## Configuring rule levels

Add a `diagnostics.rules` section to your `sqls` configuration (the same
YAML/JSON file used for `connections` and `lowercaseKeywords`), mapping a
rule code to a level:

- `default` — use the rule's built-in default level (the table above).
- `off` — never compute or report that code's findings.
- `error`, `warning`, `information`, or `hint` — always report at that
  severity when the rule fires.

```yaml
diagnostics:
  rules:
    interbase-unknown-variable: error
    interbase-null-comparison: warning
    interbase-ambiguous-column: error
```

An unknown rule code or an unknown level fails configuration loading with a
clear error, rather than being silently ignored.

Configuration precedence follows the same rule as every other `sqls`
setting: a file-specific configuration overrides the workspace
configuration, which overrides the default file configuration.

A configuration change sent via `workspace/didChangeConfiguration` is
applied immediately: every open document is re-evaluated under the new
rule policy, and diagnostics already published under the previous policy
are superseded even if the underlying connection and catalog metadata are
unchanged.
