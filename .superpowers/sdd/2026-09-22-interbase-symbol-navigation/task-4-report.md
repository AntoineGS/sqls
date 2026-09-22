# Task 4 report: SQL relation and column contexts

## Implementation summary

- Added `internal/sqlsymbol/sql.go` with a source-shape SQL ownership pass.
- Built nested query scopes from top-level `FROM`/`JOIN` sources, DML target
  relations, aliases, and correlated outer scopes; inner scopes precede outer
  scopes and alias reuse remains visible for later shadowing decisions.
- Kept derived sources as unknown `RelationRef` values with their aliases and
  did not infer lineage through derived SELECTs.
- Integrated scopes into existing `SQLReference` construction without changing
  local binding or ambiguity behavior. UPDATE/INSERT target columns use only
  the target relation scope.
- Added `internal/sqlsymbol/sql_test.go` for UPDATE, SELECT/JOIN, INSERT,
  DELETE, nested/correlated/reused aliases, and unknown derived sources.

## TDD evidence

RED command:

```text
$ go test ./internal/sqlsymbol -run 'TestSQL' -count=1
--- FAIL: TestSQLUpdateColumnContext (0.00s)
    sql_test.go:21: scopes: []
--- FAIL: TestSQLRelationScopesAndAliases (0.00s)
    sql_test.go:42: qualified SELECT scopes: []
--- FAIL: TestSQLNestedScopesShadowAliasesAndCorrelate (0.00s)
    sql_test.go:78: correlated scopes: []
--- FAIL: TestSQLDerivedRelationRetainsUnknownOwnership (0.00s)
    sql_test.go:98: derived member: {Role:3 ... SQL:...}
FAIL
```

GREEN commands and results:

```text
$ go test ./internal/sqlsymbol -run 'TestSQL' -count=1
ok   github.com/sqls-server/sqls/internal/sqlsymbol  0.004s
$ go test ./internal/sqlsymbol -count=1
ok   github.com/sqls-server/sqls/internal/sqlsymbol  0.003s
$ go test ./...
... all packages passed ...
$ go test ./internal/sqlsymbol -race -count=1
ok   github.com/sqls-server/sqls/internal/sqlsymbol  1.018s
$ go vet ./internal/sqlsymbol && git diff --check
(no output)
```

## Self-review

- Verified relation names preserve decoded quoted names, aliases remain
  separate from member columns, and relation candidates are not flattened
  across nested queries.
- Verified target-only scopes for UPDATE SET and explicit INSERT columns;
  DELETE predicates retain the target relation, while SELECT joins retain all
  current-query candidates.
- Verified unknown derived relations remain candidates, allowing later catalog
  resolution to reject unqualified ownership while still matching a concrete
  qualified relation in the same scope.
- Existing package and repository tests remain green; no local-binding logic
  was reimplemented.

## Concerns

- This is deliberately a conservative token-shape parser. Unsupported SQL
  forms such as CTE/WITH and MERGE continue through the existing ambiguity
  safety path rather than receiving inferred relation ownership.
- Function/table-valued sources and unusual dialect-specific relation syntax
  are not assigned lineage beyond the proven relation/derived-source cases.

## Fix round 1 review

### Defects addressed

- Query discovery now treats same-depth SELECTs as siblings rather than
  correlated children. This isolates UNION arms and INSERT...SELECT source
  queries from their target or preceding arm.
- Ownership queries close when the existing context pass returns to procedural
  context at FOR SELECT's DO, so statements in the DO body do not inherit the
  cursor query's relations.
- Callable relation sources consume their argument list and retain an empty
  relation name plus the source alias. DML target relation parsing explicitly
  remains non-callable so INSERT column lists are not misclassified.
- Corrected the SELECT/INSERT fixture to terminate the SELECT statement and
  added regressions for sibling isolation, FOR SELECT predicates, and callable
  source ownership.

### TDD evidence

RED command:

```text
$ go test ./internal/sqlsymbol -run 'TestSQL' -count=1
--- FAIL: TestSQLSiblingStatementsDoNotInheritScopes (0.00s)
    sql_test.go:75: first UNION scope: &{... Scopes:[[{customer} {orders}]]}
--- FAIL: TestSQLForSelectScopeEndsAtDo (0.00s)
    sql_test.go:103: FOR SELECT leaked into UPDATE: [[{audit}] [{customer}]]
--- FAIL: TestSQLCallableSourceIsUnknownWithAlias (0.00s)
    sql_test.go:118: callable source: {Name:{Text:get_rows ...} Alias:<nil>}
FAIL
```

An intermediate green attempt exposed callable parsing on an INSERT target
column list; the focused test failed with an empty target relation. Target
parsing was then made explicitly non-callable before the final green run.

GREEN commands and results:

```text
$ go test ./internal/sqlsymbol -run 'TestSQL' -count=1
ok   github.com/sqls-server/sqls/internal/sqlsymbol  0.003s
$ go test ./...
... all packages passed ...
$ go test ./internal/sqlsymbol -race -count=1
ok   github.com/sqls-server/sqls/internal/sqlsymbol  1.028s
$ go vet ./internal/sqlsymbol && git diff --check
(no output)
```

### Fix-round self-review and concerns

- The tests assert UNION and INSERT source scopes contain only their own
  concrete relation, a FOR SELECT UPDATE predicate contains only AUDIT, and a
  callable source is unknown while its joined concrete relation remains
  present.
- Parent scopes are still retained for genuinely nested parenthesized SELECTs;
  same-depth siblings are never added as outer scopes.
- Catalog nearest-qualifier behavior remains intentionally deferred to Task 7.
