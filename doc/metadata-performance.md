# Metadata readiness performance procedure

This harness measures a real `sqls` executable through its stdio Language Server
Protocol (LSP) interface. It records protocol initialization, connection attach,
relation/basic completion readiness, settled/degraded state, and request latency.
Use a stable, non-production database and a probe whose table and column labels
are unambiguous sentinel names present in that database.

## Prepare a run

Build the candidate binary and preserve the exact baseline binary separately.
The runner never copies or includes the config file, SQL text, credentials, or
stderr in its JSON report. The supplied config should point only at the selected
benchmark database. The probe JSON shape is:

```json
{
  "text": "select * from CUSTOMER where CUSTOMER_ID = 1;",
  "tablePosition": { "line": 0, "character": 14 },
  "columnPosition": { "line": 0, "character": 35 },
  "expectedTable": "CUSTOMER",
  "expectedColumn": "CUSTOMER_ID"
}
```

Positions are zero-based LSP positions. Put the table position where table
completion is relevant, and the column position where column completion is
relevant. Use the same probe, database, and config semantics for baseline and
candidate. Keep config files outside source control and restrict their file
permissions.

## Run

```sh
go run ./script/metadata-benchmark \
  -server /absolute/path/to/sqls \
  -config /secure/path/to/sqls.yml \
  -probe /path/to/probe.json \
  -runs 10 \
  -scenario cold-process \
  -timeout 5m \
  -output /path/to/cold-report.json
```

Use `-scenario refresh` to start one process, initialize and warm its metadata,
then wait for both expected completion sentinels and nondegraded settlement
before timing `switchConnections` for connection `"1"`. Refresh timings start
just before the switch command; warm-up is excluded. Each run has its own
watchdog. Timeouts terminate and reap their server process. Polling is fixed at
100 ms and completion probes use the cache-only LSP completion request (no DDL
hover).

## Interpret and report

Reports contain runtime/platform and server version strings, run counts and
outcomes, polling interval, the SHA-256 of the exact probe document, per-run
measurements, and p50/p95 percentiles. Failed or degraded runs count toward the
failure count but are excluded from successful timing percentiles. Null means a
milestone was unavailable; it is not a zero-duration result. The baseline uses
coarse primary/catalog completion log events and completion sentinels; it has no
attach milestone. Candidate status milestones come from the advertised
`sqls.showMetadataStatus` pull command. A run is successful only when both probe
sentinels are present and metadata is settled without degradation.

`loadingCompletionLatencyMs` reports pooled p50/p95 cache-only completion
response latency samples from successful runs. Samples are included only when
the request was dispatched while metadata was loading; post-settlement probes
are not included. The runner keeps at most one outstanding request per surface
and caps loading samples at five requests per surface per measured run. It may
issue one validation request after settlement only when that surface's sentinel
was not observed during loading. A settled run missing either sentinel is
classified degraded, not successful; an already-degraded settlement terminates
immediately even if either sentinel has not appeared.

Retain alongside the report (not in it) the database identifier, table/object
counts, server and driver commits/versions, host/OS, client-to-database network
placement, and whether database/server caches were warm or cold. Run at least ten
trials on both a small and a large database. Report medians/p50 and p95, all
failures, and whether they meet the lab targets in the readiness design; do not
present target values as measurements. Since timing includes the configured
network placement and a coarse baseline observer, state those caveats when
comparing results.

## Acceptance run: 2026-09-24

### Outcome

**Database acceptance is BLOCKED / NOT MEASURED.** No local small/large database
configurations or sentinel probes were supplied. In accordance with the run
instructions, no live database was accessed and no credentials or secrets were
searched for. Therefore no database timing is inferred from these local tests or
benchmarks:

| Acceptance item | Result |
| --- | --- |
| Small database: baseline/candidate, 10 cold-process runs each | **NOT MEASURED** — no config/probe |
| Small database: baseline/candidate, 10 refresh runs each | **NOT MEASURED** — no config/probe |
| Large database: baseline/candidate, 10 cold-process runs each | **NOT MEASURED** — no config/probe |
| Large database: baseline/candidate, 10 refresh runs each | **NOT MEASURED** — no config/probe |
| Dialect 1/3 live parity and native `CAST` behavior | **BLOCKED** — no corresponding live setup |
| Protocol-ready p95 <250 ms; cache-only p95 <100 ms | **NOT MEASURED** |
| Large-database basic/settled median improvement >=30% | **NOT MEASURED** |
| On-demand DDL latency | **NOT MEASURED** |

No p50/p95, database object/query counts, failure counts, peak active jobs, or
small/large database comparison is available. There are zero database samples,
not zero-millisecond samples. These targets remain acceptance criteria rather
than results. The baseline executable was preserved; candidate construction did
not overwrite it.

### Provenance and artifacts

- sqls source revision: `bc13d88109002d7e0f01e3bd98ef63bfef287525` (baseline
  `bc13d88`); InterBase driver replacement revision:
  `b0effe31a418407e60cd42491ac3904b7e67d350` (`../interbase-go`).
- Host: Linux/amd64, Intel Core i7-9700 CPU @ 3.00GHz; Go toolchain reported
  `go1.27.1-X:nodwarf5`, CGO enabled. This differs from the design's declared
  Go 1.25.7 and is recorded as the actual measurement environment.
- Candidate: built with `CGO_ENABLED=1 go build -tags interbase` at
  `/tmp/opencode/sqls-performance-candidate`; SHA-256
  `fab09cd9da0cc91ab2c37c9ef13ac35498e55ef3abcd72a8ae9fb2a7c7ed1bf6`.
- Runner: built from `./script/metadata-benchmark` at
  `/tmp/opencode/sqls-metadata-benchmark`; SHA-256
  `fab6e66adb95ed18046953ace1ca8f4fb1cd5675465c4b079c85d0a736cdc3fb`.
- Preserved baseline binary at `/tmp/opencode/sqls-performance-baseline`,
  SHA-256 `86ec5c0cd3a7ca3380eff7fc9a19ad63cf65beab0eb0ce35795b13f130a5f027`.
  Its source revision could not be established from the available local
  provenance, so no baseline commit/version is asserted.
- No InterBase server/version, dialect, database identity or size, relation /
  column / procedure / index counts, client-to-database network placement, or
  server/OS cache state can be reported without database access. No benchmark
  runner workload was executed, so there are no runner artifacts to retain.

### Local allocation and latency samples (not database acceptance)

Ran
`go test ./internal/handler -run '^$' -bench 'BenchmarkMetadataDiagnosticCatalogReuse|BenchmarkCompletionDuringMetadataLoad' -benchmem -count=5`
on the host above. Five benchmark samples were collected per case. These are
local fixture/process timings, **not cold database starts, refreshes, or
production measurements**. Timing and allocation observations are reported
separately:

| Local benchmark case | ns/op (five samples) | B/op | allocs/op |
| --- | --- | ---: | ---: |
| Diagnostic catalog, same prewarmed cache | 14.50, 14.61, 14.59, 14.47, 14.44 | 0 | 0 |
| Diagnostic catalog, new-cache conversion | 245,651; 268,159; 232,094; 266,117; 266,913 | 196,249–196,251 | 802 |
| JSON-RPC completion with metadata job held behind test gate | 161,468; 158,531; 160,819; 168,094; 163,767 | 57,819–58,018 | 548 |

The completion benchmark measures a request against a deterministic local
fixture while a metadata job is gated; it does not measure DB/network latency
or end-to-end protocol-ready p95. The same-cache case is explicitly prewarmed
and must not be interpreted as a cold process or cold database result.

### Correctness and verification (separate from performance)

All listed deterministic/local checks passed:

- `CGO_ENABLED=1 go build -tags interbase -o /tmp/opencode/sqls-performance-candidate .`
- `go build -o /tmp/opencode/sqls-metadata-benchmark ./script/metadata-benchmark`
- `go test ./...`
- `make test-race` (`go test -race ./...`)
- `CGO_ENABLED=1 go test -tags interbase ./...`
- `CGO_ENABLED=1 go build -tags interbase ./...`
- The five-sample local `-benchmem` command above.

The tagged tests/build establish compile and test correctness for the native
build-tag path, not live InterBase parity or behavior. Opt-in live tests were
not run: the necessary existing local configs/probes were unavailable. The
reporting work did not access live services.

### Instrumentation limits and caveats

No runner `-trace` diagnostic was collected because there was no database run to
trace. Pool wait deltas are **unavailable**: the baseline status surface does
not expose them; native driver/integration instrumentation would be required,
and no such live trace was available. Do not infer pool waits from job timing.
The implementation notes also warn that an early terminal trace for a
cancelled runner can omit late-attempt query counts; a trace captured before
native calls drain may consequently be incomplete. No query counts are claimed
here. No cold-cache state was manufactured by sweeping/restarting a shared
database. A future acceptance run should use existing authorized small/large
configs and probes, alternate binary order, collect the specified ten successful
samples plus failures, and record cache warmth, placement, counts, and trace
limitations alongside the output.
