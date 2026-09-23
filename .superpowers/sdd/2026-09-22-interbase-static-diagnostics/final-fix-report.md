# Final review fix report: InterBase static diagnostics

## Changes

- `internal/database/worker.go`: repository replacements now advance a generation. Primary-cache installation and both asynchronous secondary cache updates are accepted only when their captured generation still matches the active repository and primary cache. This prevents an in-flight old connection pass from overwriting new connection metadata.
- `internal/database/worker_test.go`: added an overlapping old/new repository pass regression. The old secondary pass is released after the new primary cache is installed while the new secondary pass remains parked; the new cache must retain its own column and reject the old column.
- `internal/sqlsymbol/procedure.go`: introduced `AnalyzeDiagnostics`, sharing the existing binder but recovering from malformed local declarations at a semicolon/body boundary. If no safe boundary exists, diagnostics for that procedure are suppressed and scanning resumes at a later procedure. `Analyze` retains strict parse errors for navigation.
- `internal/handler/diagnostics.go`: diagnostics use the tolerant analysis entry point. Catalog columns are copied only for InterBase documents (minor review finding).
- `internal/handler/diagnostics_test.go`: added malformed-plus-valid procedure coverage, including an assertion that navigation's strict `Analyze` still returns a parse error and that the valid procedure's unused hint keeps its exact range.
- `internal/sqlsymbol/assignments.go`: validates top-level FROM relation lists before pairing INSERT SELECT or SELECT INTO projections with destinations. Trailing/empty relation entries suppress dependent width findings.
- `internal/sqlsymbol/assignments_test.go`: added the incomplete `FROM SRC,;` no-warning regression and a quoted-character-set assignment warning regression.
- `internal/sqlsymbol/width.go`: character type suffix parsing accepts a nonempty, closed, escaped double-quoted charset identifier while continuing to reject malformed/unrelated suffixes.

## Red/green evidence

1. **Old secondary cache contaminates new connection**
   - Red: `go test ./internal/database -run '^TestWorkerDiscardsSecondaryColumnsFromPreviousRepository$' -count=1`
   - Output: `FAIL`; `worker_test.go:133: new primary columns were replaced by the previous repository's secondary result`.
   - Green: same command; `ok github.com/sqls-server/sqls/internal/database 0.004s`.

2. **Malformed procedure suppresses valid procedure diagnostics**
   - Red: `go test ./internal/handler -run '^TestDiagnosticsPreserveValidProcedureAfterMalformedDeclaration$' -count=1`
   - Output: `FAIL`; diagnostics were `[]`, and the test expected the valid procedure's unused hint. The strict analyzer logged `local variable INVALID is missing a type`.
   - Green: same command; `ok github.com/sqls-server/sqls/internal/handler 0.004s`.

3. **Incomplete INSERT SELECT relation list produces a warning**
   - Red: `go test ./internal/sqlsymbol -run '^TestWidthAssignmentsStaySilentWhenUnproven$' -count=1`
   - Output: `FAIL`; `incomplete_insert_select_relation_list` produced `Possible string truncation assigning SRC.VALUE (width 40) to DST.VALUE (width 20)`.
   - Green: same command; `ok github.com/sqls-server/sqls/internal/sqlsymbol 0.002s`.

4. **Quoted charset type loses its known width**
   - Red: `go test ./internal/sqlsymbol -run '^TestWidthAssignments$' -count=1`
   - Output: `FAIL`; `quoted_character_set_assignment` returned no width diagnostics, expected one.
   - Green: same command; `ok github.com/sqls-server/sqls/internal/sqlsymbol 0.002s`.

## Final verification

- `go test ./internal/database ./internal/sqlsymbol ./internal/handler` — all three packages passed.
- `go test ./...` — all repository packages passed; packages without tests reported `[no test files]`.
- `go test -race ./internal/database -run '^(TestWorkerDiscardsSecondaryColumnsFromPreviousRepository|TestWorkerReCacheIsRaceFreeUnderConcurrentUpdates)$' -count=1` — passed (`ok`, 1.214s).
- `go test -race ./internal/handler -run '^(TestDiagnosticsPreserveValidProcedureAfterMalformedDeclaration|TestPublishDiagnosticsDoesNotSendDelayedOlderVersion|TestPublishDiagnosticsDoesNotSendPriorConnectionGeneration|TestPublishDiagnosticsUsesFreshCacheSnapshot)$' -count=1` — passed (`ok`, 1.038s).
- `git diff --check` — passed.

## Remaining concerns

- If tokenization itself fails (for example, an unclosed quoted literal or comment), safe boundaries cannot be identified; diagnostics continue to be suppressed for that document snapshot. Navigation behavior remains strict by design.
- Unsupported or ambiguous SQL shapes remain silent under the conservative width-analysis contract.
