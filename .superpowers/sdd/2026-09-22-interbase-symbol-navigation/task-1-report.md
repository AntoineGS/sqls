# Task 1 report: source spans and dialect-correct names

## Implementation summary

Implemented the lexical foundation for InterBase symbol navigation:

- Added half-open UTF-8 byte `Span`, dialect-aware `Name`, and `Edit` types.
- Added `Name.Key` and `Name.MatchesCatalogName` with quoted/unquoted identity
  semantics.
- Added the private tokenizer adapter `lex` and `lexeme`, preserving exact
  source byte offsets from `text/scanner`.
- Added source-spelling identifier decoding, including doubled delimited
  identifier quotes, without relying on normalized `SQLWord.Value`.
- Added coverage for Unicode plus CRLF offsets, dialect 1/3 quote behavior,
  escaped strings, tabs, multiline strings, and comments.

## Changed files

- `internal/sqlsymbol/source.go`
- `internal/sqlsymbol/source_test.go`
- `.superpowers/sdd/2026-09-22-interbase-symbol-navigation/task-1-report.md`

## RED evidence

Command:

```text
go test ./internal/sqlsymbol -run 'TestLex|TestName' -count=1
```

Initial output before production code existed:

```text
# github.com/sqls-server/sqls/internal/sqlsymbol [github.com/sqls-server/sqls.test]
internal/sqlsymbol/source_test.go:17:17: undefined: lex
internal/sqlsymbol/source_test.go:21:12: undefined: Span
internal/sqlsymbol/source_test.go:28:12: undefined: Span
internal/sqlsymbol/source_test.go:35:6: undefined: Name
internal/sqlsymbol/source_test.go:38:6: undefined: Name
internal/sqlsymbol/source_test.go:50:14: undefined: Name
internal/sqlsymbol/source_test.go:59:16: undefined: Name
internal/sqlsymbol/source_test.go:101:16: undefined: lex
internal/sqlsymbol/source_test.go:117:17: undefined: nameFromLexeme
FAIL
```

The failure was caused by the intentionally missing lexical types/functions,
not by a test or package setup error.

## GREEN evidence

Focused lexical and regression tests:

```text
go test ./internal/sqlsymbol -run 'TestLex|TestName' -count=1
ok   github.com/sqls-server/sqls/internal/sqlsymbol 0.003s

go test ./internal/sqlsymbol ./token ./dialect -count=1
ok   github.com/sqls-server/sqls/internal/sqlsymbol 0.002s
ok   github.com/sqls-server/sqls/token 0.004s
ok   github.com/sqls-server/sqls/dialect 0.002s
```

Full suite:

```text
go test ./...
?    github.com/sqls-server/sqls [no test files]
?    github.com/sqls-server/sqls/ast [no test files]
?    github.com/sqls-server/sqls/ast/astutil [no test files]
ok   github.com/sqls-server/sqls/dialect (cached)
ok   github.com/sqls-server/sqls/internal/completer (cached)
ok   github.com/sqls-server/sqls/internal/config (cached)
ok   github.com/sqls-server/sqls/internal/database (cached)
?    github.com/sqls-server/sqls/internal/debug [no test files]
ok   github.com/sqls-server/sqls/internal/formatter (cached)
ok   github.com/sqls-server/sqls/internal/handler (cached)
?    github.com/sqls-server/sqls/internal/lsp [no test files]
ok   github.com/sqls-server/sqls/internal/sqlsymbol 0.002s
ok   github.com/sqls-server/sqls/parser (cached)
ok   github.com/sqls-server/sqls/parser/parseutil (cached)
ok   github.com/sqls-server/sqls/token (cached)
```

`go vet ./internal/sqlsymbol ./token ./dialect` also completed with no output.

## Self-review

- The implementation uses the existing tokenizer and selected
  `DialectForDriverVariant`; no second SQL lexer was introduced.
- Spans come directly from scanner byte offsets, so Unicode, CRLF, normalized
  token values, trivia, and escaped source text retain their original ranges.
- Name identity follows the shared contract: quoted names are exact and
  unquoted names are uppercased for keys.
- Tests assert the required source-position and identity behaviors and cover
  the listed dialect/string/comment edge cases.
- No unrelated files or dependencies were changed.

## Concerns

None. Future tasks should use `nameFromLexeme` when turning quoted SQL-word
tokens into `Name` values so identifier escape decoding remains source-based.
