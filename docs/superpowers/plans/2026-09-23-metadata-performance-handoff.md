# sqls performance and progressive readiness — handoff index

**Status:** planning package, ready for review; implementation has not started.

**User goal:** faster useful LSP availability and metadata loading, InterBase
first, with independent loading jobs surviving another job's failure. The user
approved progressive readiness and requested plans for later smaller-agent work.

**Design:** [Progressive metadata loading and LSP readiness](../specs/2026-09-23-progressive-metadata-readiness-design.md)

## Execution order

| Order | Plan | Tasks | Deliverable |
| --- | --- | --- | --- |
| 1 | [Metadata loader foundation](2026-09-23-metadata-loader-foundation.md) | 5 | explicit completeness, immutable merges, bounded failure-isolated scheduler, safe consumers |
| 2 | [InterBase metadata jobs](2026-09-23-interbase-metadata-jobs.md) | 6 | independent read-only jobs, bounded query counts, bulk extended metadata |
| 3 | [LSP progressive readiness](2026-09-23-lsp-progressive-readiness.md) | 6 | background attachment, server cutover, coherent snapshots, diagnostics coalescing, progress/status |
| 4 | [Performance validation](2026-09-23-metadata-performance-validation.md) | 4 | stdio benchmark harness, integrated regression tests, measured acceptance report |

**21 reviewable tasks total.** Give an agent one task with its prerequisites,
not the whole package as an instruction to implement everything at once.
Plans 1–2 keep the new loader isolated until Plan 3 activates it. These are
dependency-ordered changes to shared internals, not four parallel branches to
launch blindly.

Plan 2's views/indexes, procedures, and functions readers are separable only
after Tasks 1–2 establish transaction/identifier helpers. They still share
descriptor mapping extraction in interbase_catalog.go and final plan assembly;
prefer serial execution for small-agent handoff unless one integrator owns those
shared edits. No multi-agent execution is required just to use these plans.

## Fixed decisions agents should not reopen independently

- initialize performs no database I/O; bootstrap begins after initialized.
- Three InterBase metadata jobs maximum; one for generic drivers initially.
- The global limit counts cancelled but not-yet-returned calls from old generations.
- One read-only transaction per database job; never share a transaction for parallel I/O.
- Categories publish independently; a failed category does not clear successful siblings.
- Per-category completeness is explicit. HasCatalog is only a presence check.
- Every connection transition clears visible metadata and fences late results.
- Primary keys are independent of columns and enrich copies when they arrive.
- Bulk query reduction accompanies concurrency; no per-object goroutine fan-out.
- No automatic retry or persistent metadata cache in this iteration.
- Pull-based `sqls.showMetadataStatus` only; defer push progress because the
  pinned JSON-RPC writer cannot bound blocked writes.
- Query execution serialization against connection replacement is preserved.
- Performance targets are acceptance goals, not measurements already obtained.

## Shared-file ownership

| File/API | Owner | Rule for downstream work |
| --- | --- | --- |
| metadata.go contracts | Plan 1 Task 1 | preserve names/types; discuss a needed change with integrator |
| metadata_merge.go | Plan 1 Task 2 | the only category publication/PK enrichment path |
| metadata_loader.go | Plan 1 Task 3 | Plan 4 adds metrics, not a second scheduler |
| DBCache readiness and consumer gates | Plan 1 Tasks 1/5 | new fixtures explicitly mark ready categories |
| interbase_metadata_jobs.go | Plan 2 Tasks 1/5 | all DB jobs own one transaction; no eager CatalogSnapshot |
| interbase_catalog.go mappers | Plan 2 Tasks 3–5 | extract pure existing behavior, preserve legacy accessors |
| connection_lifecycle.go and Server fields | Plan 3 Task 2 | later tasks use coordinator, never start independent attachment goroutines |
| editor_snapshot.go | Plan 3 Task 3 | one source of generation/variant/cache identity per editor operation |
| diagnostics callbacks/memo | Plan 3 Task 4 | one bounded consumer, retain only the current derived catalog |
| status/progress | Plan 3 Task 5 | reporter failure cannot fail metadata loading |
| doc/develop.md | each owning task | update touched invariants; reconcile obsolete 2026-09-19 claims |

## Small-agent assignment template

```text
Implement Plan <number>, Task <number> only.
Read:
  docs/superpowers/specs/2026-09-23-progressive-metadata-readiness-design.md
  docs/superpowers/plans/<the assigned plan filename>
Prerequisite commits: <integrator supplies the actual commit ids>
Use the exact Interfaces contract in that task and the plan's Global Constraints.
Run its failing regression first, implement the task, then run the stated checks.
Do not modify sibling repositories or silently change another task's API.
Report changed files, test commands/results, actual commit id, and any blocker.
Stop after the assigned task for integration/review.
```

The angle-bracket fields above are dispatch-time assignment fields, not missing
implementation requirements. The integrator fills them when creating an agent
assignment. The plan and spec must travel with the task because a smaller agent
has no memory of this planning conversation.

## Review gates and evidence

At each task boundary review its behavior and tests, especially publication
immutability, readiness/negative conclusions, cancellation, and native connection
ownership. Do not accept tests that merely verify the implementation's function
call order without checking user-visible results or resource bounds.

After Plan 1: scheduler/race proof and baseline artifact provenance.
After Plan 2: descriptor equivalence, fixed query budgets, native/tagged checks.
After Plan 3: real JSON-RPC handshake/read-loop liveness, switching/shutdown,
client progress fallback, and full regression/race results.
After Plan 4: small/large workload report with failed-run counts and p50/p95.

The key deterministic assertions are: initialize returns while attach is blocked;
an independent job publishes despite another job's failure; old generations
cannot publish; and real in-flight calls never exceed the global budget.

Lab targets are protocol-ready p95 <250 ms, cache-only completion p95 <100 ms
during loading, and >=30% lower large-database median basic-ready/settled times.
Record misses honestly; CI uses deterministic gates/query counts rather than
fragile wall-clock thresholds.

## Remaining environment decisions at execution time

Choose the small/large read-only benchmark databases and local probe documents;
record their dialects and object counts. Confirm the tagged native toolchain and
InterBase client library are available. These are benchmark inputs, not blockers
to the unit-tested architecture work. Live runs are never substituted with a
skipped test and described as verified.
