# Metadata Performance Validation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Produce reproducible evidence for startup, useful metadata latency, failure isolation, query reduction, and responsiveness under background loading.

**Architecture:** Deterministic in-process tests establish liveness/correctness. A small stdio LSP benchmark client measures real binaries against the same database and probe document. Query-count and timing instrumentation reports stage-specific results, with incomplete/failed runs kept separate from successful timings.

**Tech Stack:** Go 1.25.7 testing/benchmark packages, os/exec, sourcegraph/jsonrpc2, existing native InterBase test environment, JSON reports.

**Spec:** `docs/superpowers/specs/2026-09-23-progressive-metadata-readiness-design.md`, §§8–9.

## Global Constraints

- Go 1.25.7; use the standard library for scheduling and synchronization.
- Keep DBRepository unchanged; add optional capabilities for specialized loading.
- Lock ordering: connMu before stateMu, never the reverse; stateMu never spans I/O.
- Published metadata and descriptors are immutable; every result is generation-fenced.
- Native InterBase code remains behind interbase && cgo && linux && amd64.
- Metadata uses read-only transactions; do not run DDL or write production data.
- Cancellation is best effort; never release a concurrency slot before its call returns.
- Preserve configured/resolved/source dialect distinctions and full catalog identifiers.
- No credentials, source SQL, or raw driver errors in client progress/status payloads.
- Run go test ./... and make test-race; tagged changes also require CGO_ENABLED=1 go test -tags interbase ./... and CGO_ENABLED=1 go build -tags interbase ./....

## Review Focus

1. Failed or timed-out loads must not look faster than successful loads (Tasks 1–2).
2. The old binary lacks the new status command and returns completion arrays (Task 2).
3. Benchmark polling/progress/logging must not dominate the work measured (Tasks 1–2).
4. Slow old-generation work and repeated refreshes must not grow workers or retained snapshots (Task 3).
5. Warm database/server cache must not be described as a cold database cache (Task 4).

## Prerequisites and files

Requires Plan 3. Retrieve the baseline binary and exact sqls/driver revisions
recorded by Plan 1. Build the candidate to a different path, never over the
baseline. New files:

- `internal/database/metadata_metrics.go` and tests: bounded metadata metrics.
- `script/metadata-benchmark/main.go` and tests: stdio benchmark runner/report.
- `internal/handler/readiness_acceptance_test.go`: integrated failure/liveness tests.
- `internal/handler/metadata_benchmark_test.go`: allocation and cache-only benchmarks.
- `doc/metadata-performance.md`: reproducible measurement procedure and findings.

Production changes are limited to completing measurements already specified by
the design. New performance mechanisms beyond Plans 1–3 require evidence and a
separate scoped change, rather than speculative tuning during this plan.

---

### Task 1: Record job timing, query counts, and pool pressure

**Files:** Create `internal/database/metadata_metrics.go`, `metadata_metrics_test.go`; modify `metadata_loader.go`, `interbase_metadata_jobs.go`, and `internal/handler/metadata_status.go` for aggregate reporting where needed.

**Interfaces:** Plan 1's MetadataStatus/MetadataPatch carry query-count fields;
add a counting queryer used by InterBase transaction wrappers:

```go
type metadataQueryCounter struct {
    queryer schema.Queryer
    queries int // owned by one job goroutine; no shared counter
}
func (q *metadataQueryCounter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
    q.queries++
    return q.queryer.QueryContext(ctx, query, args...)
}
```

After a read, preserve query metrics even on error/cleanup failure. Generic
repositories report QueriesKnown=false rather than a fictitious zero. Record
QueuedAt when jobs become pending, StartedAt when Run begins, FinishedAt at its
terminal transition. Emit optional structured logs under SQLS_METADATA_TRACE=1;
one job terminal event and one generation summary suffice. Fields: generation,
kind, state, queue_ms, run_ms, count, queries (null if unknown). No query text or
source/connection credential data.

- [ ] **Step 1: Add metrics tests for success, failure after two queries, cancellation while queued, and supersession.**

```go
if !status.QueriesKnown || status.Queries != 2 { t.Fatalf("lost failed-job counts: %+v", status) }
if status.State != MetadataFailed { t.Fatal("failure recorded as successful timing") }
```

For queue cancellation, StartedAt stays zero and reported run_ms is zero, not a
negative subtraction. Preserve failure counts separately from successful counts.

- [ ] **Step 2: Run** `go test ./internal/database -run TestMetadataMetrics -count=1`; expect missing/incorrect query accounting.
- [ ] **Step 3: Implement counting wrappers and metric summaries.** Use the same
counted Queryer for width discovery and data reads. Driver-backed simple jobs use
schema.New(counter), not schema.New(tx), so metadata width queries are included.
Do not count transaction begin/rollback as SELECTs. Snapshot db.Stats before and
after load for tagged benchmark reporting; WaitCount/WaitDuration are pool-wide
and must be labeled so concurrent interactive work is not attributed exclusively
to metadata. Keep only current-generation aggregates.
- [ ] **Step 4: Run** `go test -race ./internal/database -run 'TestMetadataMetrics|TestInterBaseMetadata' -count=1`; query-count budgets from Plan 2 remain unchanged.
- [ ] **Step 5: Commit** `perf(metadata): expose bounded job timing and query metrics`.

### Task 2: Add a real stdio LSP benchmark runner

**Files:** Create `script/metadata-benchmark/main.go`, `main_test.go`; create initial procedure in `doc/metadata-performance.md`.

**Interfaces:** Runner flags:

```text
-server PATH        sqls binary (required)
-config PATH        existing sqls YAML config (required; never copy into report)
-probe PATH         JSON probe document and expected labels (required)
-runs N             default 10, require N >= 1
-scenario NAME      cold-process or refresh, default cold-process
-output PATH        JSON report (required)
-timeout DURATION   per-run watchdog, default 5m
```

Probe file schema uses existing lsp.Position:

```go
type probe struct {
    Text string `json:"text"`
    TablePosition lsp.Position `json:"tablePosition"`
    ColumnPosition lsp.Position `json:"columnPosition"`
    ExpectedTable string `json:"expectedTable"`
    ExpectedColumn string `json:"expectedColumn"`
}
type runResult struct {
    Outcome string `json:"outcome"` // success, degraded, timeout, transport-error
    ProtocolReadyMS *float64 `json:"protocolReadyMs"`
    AttachReadyMS *float64 `json:"attachReadyMs"`
    RelationReadyMS *float64 `json:"relationReadyMs"`
    BasicReadyMS *float64 `json:"basicReadyMs"`
    SettledMS *float64 `json:"settledMs"`
    CompletionLatencyMS []float64 `json:"completionLatencyMs"`
    Observer string `json:"observer"` // status or legacy-log
}
```

Include version strings, platform, run count, scenario, polling interval, and
probe hash in the report; no config text, source document, credentials, or raw
stderr. Null is an unavailable metric, not zero. Success requires table and column
sentinels plus nondegraded settlement; a missing sentinel is not success even if
the loader says ready.

- [ ] **Step 1: Add fake-child protocol tests.** Use Go's helper-process test
pattern with a test-specific environment flag, stdin/stdout framed JSON-RPC,
and fixture responses. Cover successful initialize, delayed initialized work,
array/List completion decoding, no status command, failed catalog, stalled child,
out-of-order responses, and stderr noise. Assert timeout kills/reaps the child
and leaves outcome timeout with unset readiness metrics.

```go
func decodeCompletion(raw json.RawMessage) ([]lsp.CompletionItem, error) {
    var list lsp.CompletionList
    if len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '[' {
        var items []lsp.CompletionItem
        err := json.Unmarshal(raw, &items)
        return items, err
    }
    err := json.Unmarshal(raw, &list)
    return list.Items, err
}
```

This helper accepts both historical and candidate completion responses. Add null
handling explicitly in tests; null means no items.

- [ ] **Step 2: Run** `go test ./script/metadata-benchmark -count=1`; expect runner/report behavior absent.
- [ ] **Step 3: Implement child lifecycle and measurement.** Start `server -config
PATH` with exec.CommandContext and stdout/stdin pipes; capture only recognized
metadata milestones from stderr, discarding other lines from report output. Use
jsonrpc2 with VSCodeObjectCodec and a small pipe ReadWriteCloser. Start the clock
immediately before process start; record initialize reply, send initialized,
then didOpen. Poll at 100 ms, at most one outstanding probe request per surface.
Use the status command only if advertised in initialize capabilities. Probe table
and column completions and require the expected labels. Sample cache-only
completion while loading, without on-demand DDL hover in that latency series.

For the baseline binary, recognize its existing `Update db cache primary
complete` and `Update catalog cache complete` log events and timestamp receipt;
these are coarse observations, not exact internal timings. Confirm basic readiness
with completion sentinels. AttachReadyMS is null on baseline. If full catalog
completion never appears, report timeout/degraded rather than inferring settlement
from silence. For refresh scenario, initialize/load once without measuring that
warmup, then invoke `switchConnections` with argument `"1"` and time each refresh;
use fresh candidate generation ids or fresh baseline completion log events.
Finish with shutdown/exit and close stdin; enforce the watchdog and reap the child.

- [ ] **Step 4: Run** `go test -race ./script/metadata-benchmark -count=1`; verify result aggregation excludes failed runs from successful timing percentiles but always reports their count.
- [ ] **Step 5: Commit** `test(perf): add reproducible stdio readiness benchmark runner`.

### Task 3: Add integrated acceptance and allocation benchmarks

**Files:** Create `internal/handler/readiness_acceptance_test.go`, `metadata_benchmark_test.go`; extend `internal/database/metadata_loader_test.go` as needed for drainage assertions.

**Interfaces:** Use Plan 3's opener seam and Plan 1's optional-plan fixture. No new production API. Add benchmarks named BenchmarkMetadataDiagnosticCatalogReuse and BenchmarkCompletionDuringMetadataLoad.

- [ ] **Step 1: Add gated end-to-end acceptance cases.** Use net.Pipe JSON-RPC,
not direct handler calls. Cover blocked attach with initialize/format response;
blocked columns with keyword/table completion; failed views with successful
procedures; failed primary metadata with successful extended metadata; slow old
load followed by switching; and stopped server with pending query/notification.

```go
select {
case <-formatReturned: // opener gate is still closed here
case <-time.After(2*time.Second): t.Fatal("formatting blocked behind attach")
}
select {
case <-openerReleased: t.Fatal("test accidentally released its blocking condition")
default:
}
```

Use actual active-counter assertions for worker bounds. Do not assert an exact
runtime.NumGoroutine count; standard-library goroutines make that flaky. Loop 100
refresh generations with releaseable gates and call loader.Wait after Stop;
assert all test driver transactions/rows close and only current metadata remains
referenced by loader/diagnostic memo. Run failure cases for every job kind using
table-driven fixtures, plus one recovered panic through the whole server.

- [ ] **Step 2: Run** `go test -race ./internal/handler ./internal/database -run 'TestReadinessAcceptance|TestMetadataLoader' -count=1`; all invariants must pass without sleeps used as performance proofs.
- [ ] **Step 3: Add allocation/latency benchmarks.** Build a static large DBCache
once outside timing with explicit readiness and representative columns/indexes.
Use b.ReportAllocs and b.ResetTimer; benchmark repeated diagnosticCatalogFor on
the same pointer and completion while an unrelated metadata job is gated. Benchmark
one new cache pointer separately to retain the actual conversion cost. Keep test
fixture construction and transaction waits outside timed loops.

```go
b.ReportAllocs()
b.ResetTimer()
for i := 0; i < b.N; i++ {
    got := server.diagnosticCatalogFor(cache)
    if got == nil { b.Fatal("missing derived catalog") }
}
```

- [ ] **Step 4: Run** `go test ./internal/handler -run '^$' -bench 'BenchmarkMetadataDiagnosticCatalogReuse|BenchmarkCompletionDuringMetadataLoad' -benchmem -count=5` and the full `go test ./...` / `make test-race` gates. Record distributions; do not add CI assertions on unstable nanoseconds.
- [ ] **Step 5: Commit** `test(lsp): cover progressive readiness and metadata-load responsiveness`.

### Task 4: Run the comparison and publish the acceptance report

**Files:** Complete `doc/metadata-performance.md`; add a compact sanitized result
summary to that document, with detailed local artifacts referenced by provenance
rather than machine-specific absolute paths committed as dependencies.

**Interfaces:** No runtime changes. Deliver a measured acceptance report with
separate correctness and timing outcomes.

- [ ] **Step 1: Build the candidate** without overwriting the baseline:

```sh
CGO_ENABLED=1 go build -tags interbase -o /tmp/opencode/sqls-performance-candidate .
go build -o /tmp/opencode/sqls-metadata-benchmark ./script/metadata-benchmark
```

- [ ] **Step 2: Run at least ten successful cold-process and refresh samples per
binary on a small and large database.** Supply CONFIG and PROBE as existing local
file paths; user credentials stay in their existing config/environment. Example:

```sh
/tmp/opencode/sqls-metadata-benchmark -server /tmp/opencode/sqls-performance-baseline -config "$CONFIG" -probe "$PROBE" -runs 10 -scenario cold-process -output /tmp/opencode/sqls-baseline-cold.json
/tmp/opencode/sqls-metadata-benchmark -server /tmp/opencode/sqls-performance-candidate -config "$CONFIG" -probe "$PROBE" -runs 10 -scenario cold-process -output /tmp/opencode/sqls-candidate-cold.json
/tmp/opencode/sqls-metadata-benchmark -server /tmp/opencode/sqls-performance-baseline -config "$CONFIG" -probe "$PROBE" -runs 10 -scenario refresh -output /tmp/opencode/sqls-baseline-refresh.json
/tmp/opencode/sqls-metadata-benchmark -server /tmp/opencode/sqls-performance-candidate -config "$CONFIG" -probe "$PROBE" -runs 10 -scenario refresh -output /tmp/opencode/sqls-candidate-refresh.json
```

Use workload-specific output filenames for the second database. Alternate
baseline/candidate order across batches. Record failure counts, not only successful
samples. Do not sweep, restart, or flush a shared database to manufacture cold
results. Cold-process means a new sqls process; server/OS cache warmth is recorded
separately. Run with tracing disabled for timing, then one trace-enabled diagnostic
run for query/pool attribution. A native integration or driver trace may supply
pool metrics that cannot be obtained from the baseline status interface; mark
unavailable values rather than guessing.

- [ ] **Step 3: Write the report.** Include sqls/driver commits, InterBase version,
database dialect, relation/column/procedure/index counts, host/network placement,
hardware, sample counts, p50/p95, peak active jobs, SELECT counts, pool wait deltas,
allocation benchmarks, and test commands/results. Compare protocol p95 <250 ms,
cache-only completion p95 <100 ms, and >=30% median improvement in large-database
basic/settled times against the spec's lab targets. If a target misses, identify
the measured slow stage and record the miss; do not silently adjust the target or
conflate missing categories with reduced latency. Include observed on-demand DDL
latency separately to identify a possible next scoped effort.
- [ ] **Step 4: Run final checks** `go test ./...`, `make test-race`, `CGO_ENABLED=1 go test -tags interbase ./...`, `CGO_ENABLED=1 go build -tags interbase ./...`, and the opt-in live metadata tests on Dialect 1 and Dialect 3 where available. Report skipped native/live checks explicitly.
- [ ] **Step 5: Commit** `docs(perf): record progressive metadata readiness measurements`.

## Completion evidence

The package is complete when the implemented behavior passes the deterministic
checks and the performance report contains actual measurements or an explicit
environment blocker. A missing native benchmark is not a claimed performance
win. Any follow-up tuning is selected from measured bottlenecks, with its own
small plan and correctness gate.
