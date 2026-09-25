# Task 12 report — COMPLETE

## Status and commit

Implemented Task 12 and committed as `feat(diagnostics): add opt-in procedural flow advisories`.

## Implementation

- Added bounded, opt-in procedural-flow analysis for `interbase-read-before-assignment`, `interbase-output-not-assigned`, `interbase-dead-store`, and `interbase-unreachable`.
- Modeled supported blocks, assignments, IF/ELSE, WHILE, FOR SELECT, EXIT, SUSPEND, trigger locals, and trigger event-context values. Loops retain a zero-iteration edge and iterate over a bounded assignment-state fixed point.
- Tracked explicit assignment separately from known initial SQL NULL. Verified declaration defaults and input parameters start assigned; an explicit `= NULL` is an assignment. Read-before-assignment wording says the initial NULL may be unintended, not undefined memory. Output messages do not claim NULL, and outputs retain state across SUSPEND.
- Unsupported syntax/effects invalidate affected facts; exception-handler regions are modeled as unknown effects. Malformed and over-budget analyses withhold conclusions. Flow computation is skipped unless at least one flow code is enabled.
- Registered all four rules default-off, integrated severity overrides, and documented semantics and limits in `doc/interbase-diagnostics.md`.

## TDD evidence

The opt-in behavior tests were written first and failed on the missing diagnostics. During implementation, the incomplete-assignment fixture exposed an over-eager dead-store finding; the test failed before the parser was tightened to withhold conclusions for malformed statements. Trigger-local positive coverage was also added and failed before trigger-flow binding was integrated. The targeted flow suite passed after the implementation.

## Validation

- `gofmt` on changed Go files — completed.
- `git diff --check` — passed.
- `go test ./internal/sqlsymbol -run 'TestDiagnosticFlow' -count=1` — passed.
- `go test ./internal/sqlsymbol -count=1` — passed.
- `go test ./... -count=1` — passed.
- `go vet ./...` — passed.

No database I/O was added. No reviewers or subagents were dispatched.
