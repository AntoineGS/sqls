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
then time `switchConnections` for connection `"1"`. Refresh timings start just
before the switch command; warm-up is excluded. Each run has its own watchdog.
Timeouts terminate and reap their server process. Polling is fixed at 100 ms and
completion probes use the cache-only LSP completion request (no DDL hover).

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

Retain alongside the report (not in it) the database identifier, table/object
counts, server and driver commits/versions, host/OS, client-to-database network
placement, and whether database/server caches were warm or cold. Run at least ten
trials on both a small and a large database. Report medians/p50 and p95, all
failures, and whether they meet the lab targets in the readiness design; do not
present target values as measurements. Since timing includes the configured
network placement and a coarse baseline observer, state those caveats when
comparing results.
