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

Every check withholds a finding rather than guessing: a code is reported
only when the relevant catalog metadata has finished loading and proves the
condition. As metadata categories (relations, columns, procedures,
domains, views, indexes) become ready, previously withheld findings appear
in a later publication for the same document.

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
