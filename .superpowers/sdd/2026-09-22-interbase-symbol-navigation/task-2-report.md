# Task 2 report: procedure scopes and declarations

## Implementation summary

- Added `SymbolKind`, `Symbol`, `Analysis`, private procedure records, and
  `Analyze` in `internal/sqlsymbol/procedure.go`.
- Added significant-token procedure scanning for `CREATE`/`ALTER PROCEDURE`,
  input/output parameter lists, `DECLARE VARIABLE`, nested `BEGIN`/`CASE`
  frames, EOF recovery, and recovery at a following procedure header.
- Duplicate declarations share a procedure name bucket and all receive an
  `ambiguous declaration` rename block. Declaration and procedure spans use
  original UTF-8 byte offsets.
- Extended `internal/sqlsymbol/source.go` with source-unit `SET TERM` tracking.
  Active delimiters are intercepted outside strings/comments before SQL
  tokenization, while `Analysis.Text` remains unchanged.
- Added `internal/sqlsymbol/procedure_test.go` covering the required
  declarations, scopes, separate procedures, nested blocks, CASE, parameter
  type commas, quoted names, duplicates, incomplete bodies, and `^`/`!!`
  terminators.

## TDD evidence

RED command:

```text
go test ./internal/sqlsymbol -run 'TestAnalyze|TestProcedure' -count=1
```

Initial failure was the expected missing-production failure: the package did
not compile with `undefined: Symbol` and `undefined: Analyze` in the new tests.

GREEN commands and results:

```text
go test ./internal/sqlsymbol -count=1   # ok
go test ./...                            # all packages passed
git diff --check                         # clean
```

## Self-review

- Confirmed declaration order and exact source spans with focused tests.
- Confirmed nested block closure does not end the containing procedure until
  the matching outer `END`.
- Confirmed custom terminators inside literals are not intercepted and lone
  `!` is only accepted when protected by an active `SET TERM` delimiter.
- No occurrence binding, resolution, or rename implementation was added.

## Concerns

- This increment intentionally returns no occurrence bindings (`Uses` remains
  nil); later tasks must add binding before navigation/rename behavior is
  complete.
