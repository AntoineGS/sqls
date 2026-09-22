# Task 5 report

## Implementation summary

- Added request-local InterBase procedure-local go-to-definition and references.
- Added source-span/LSP adapters using UTF-16 units, CRLF handling, and strict
  invalid-position/encoded-rune-boundary rejection.
- Wired `textDocument/references`, `ReferencesProvider`, and local-definition
  routing ahead of aliases/catalog acquisition.
- Kept relation/column resolutions at a contextual-catalog seam so a legacy
  spelling-based alias cannot steal them; Task 7 can attach catalog snapshots.

## Changed files

`internal/handler/symbol.go`, `symbol_test.go`, `references.go`,
`references_test.go`, `definition.go`, `definition_test.go`, `handler.go`,
`handler_test.go`, `internal/lsp/lsp.go`.

## RED

Command:

```text
go test ./internal/handler -run 'TestReferences|TestLocalDefinition|TestSymbol|TestInitialized' -count=1
```

Observed failure before implementation:

```text
internal/handler/references_test.go:22:18: undefined: lsp.ReferenceParams
internal/handler/references_test.go:26:14: undefined: localReferences
internal/handler/references_test.go:45:13: undefined: localReferences
internal/handler/references_test.go:55:16: undefined: lsp.ReferenceParams
internal/handler/references_test.go:59:14: undefined: localReferences
internal/handler/references_test.go:69:13: undefined: localReferences
internal/handler/symbol_test.go:36:15: undefined: symbolOffset
internal/handler/symbol_test.go:47:13: undefined: symbolRange
internal/handler/symbol_test.go:71:23: undefined: localDefinition
# github.com/sqls-server/sqls/internal/handler [build failed]
internal/handler/symbol_test.go:71:23: too many errors
FAIL github.com/sqls-server/sqls/internal/handler [build failed]
```

## GREEN

```text
$ go test ./internal/handler -run 'TestReferences|TestLocalDefinition|TestSymbol|TestInitialized' -count=1
ok   github.com/sqls-server/sqls/internal/handler  0.025s

$ go test ./internal/handler ./internal/sqlsymbol -count=1
ok   github.com/sqls-server/sqls/internal/handler    1.071s
ok   github.com/sqls-server/sqls/internal/sqlsymbol  0.004s

$ go test ./... -count=1
?    github.com/sqls-server/sqls [no test files]
?    github.com/sqls-server/sqls/ast [no test files]
?    github.com/sqls-server/sqls/ast/astutil [no test files]
ok   github.com/sqls-server/sqls/dialect 0.011s
ok   github.com/sqls-server/sqls/internal/completer 0.020s
ok   github.com/sqls-server/sqls/internal/config 0.009s
ok   github.com/sqls-server/sqls/internal/database 0.289s
?    github.com/sqls-server/sqls/internal/debug [no test files]
ok   github.com/sqls-server/sqls/internal/formatter 0.007s
ok   github.com/sqls-server/sqls/internal/handler 1.078s
?    github.com/sqls-server/sqls/internal/lsp [no test files]
ok   github.com/sqls-server/sqls/internal/sqlsymbol 0.009s
ok   github.com/sqls-server/sqls/parser 0.011s
ok   github.com/sqls-server/sqls/parser/parseutil 0.020s
ok   github.com/sqls-server/sqls/token 0.008s

$ git diff --check
(no output; exit 0)
```

## Self-review

- Local references include/exclude declarations through the LSP context and
  are sorted/deduplicated by `sqlsymbol.Analysis.References`.
- Local definition/reference requests never acquire a DDL repository.
- Unsupported drivers and non-local/ambiguous reference targets return empty
  results; missing parameters/documents retain existing request validation.
- Existing non-InterBase definition routing and alias/callable fallback remain
  in place.

## Concerns

Relation/column catalog snapshot materialization is intentionally deferred to
Task 7; Task 5 now prevents those proven SQL roles from falling through to an
unrelated alias spelling.
