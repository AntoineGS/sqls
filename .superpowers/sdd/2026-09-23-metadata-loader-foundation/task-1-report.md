# Task 1 implementation report — metadata state and loader contracts

## Changed files

- `internal/database/metadata.go` — declared metadata kind/state constants, status/snapshot/patch/job/plan contracts, optional plan repository interface, and snapshot settlement/degradation helpers.
- `internal/database/metadata_test.go` — readiness distinction, nil/empty behavior, column readiness, and terminal/degraded snapshot coverage.
- `internal/database/cache.go` — added metadata and primary-key membership maps; added explicit readiness helpers; successful legacy primary generation marks schemas, relations, current columns, and foreign keys ready.
- `internal/database/cache_test.go` — added copy-on-write legacy setter readiness regression coverage.
- `internal/database/worker.go` — legacy all-column and catalog publication now copy Metadata and mark their successfully populated categories ready.

Existing planning/spec files were left untouched and excluded from the commit.

## TDD evidence

- Red: `go test ./internal/database -run TestMetadataReady -count=1` failed at compile time with the expected missing-contract errors (`DBCache.Metadata`, `MetadataKind`, `MetadataReady`, and related symbols undefined).
- Extended red: `go test ./internal/database -run 'TestMetadataReady|TestMetadataReadiness|TestWorkerLegacySetters' -count=1` likewise failed to compile before implementation, confirming the newly exercised readiness/setter APIs were missing.
- Green: the same focused command passed after implementation: `ok github.com/sqls-server/sqls/internal/database 0.005s`.
- Package suite: `go test ./internal/database -count=1` passed: `ok github.com/sqls-server/sqls/internal/database 0.431s`.

## Full verification

- `go test ./...` passed across every package. The database package completed in 0.567s and the handler package in 4.410s; packages without tests reported `[no test files]`.
- `go test -race ./internal/database` passed: `ok github.com/sqls-server/sqls/internal/database 1.664s`.
- No test failures were observed.

## Design concerns / deviations

- No plan deviations. `MetadataReady()` with no requested categories returns false rather than treating an empty conjunction as ready; this avoids accidental readiness from an unspecified requirement set.
- Legacy catalog readiness is assigned only after the existing catalog pass reports success. No readiness is inferred from non-nil/empty maps, `HasCatalog` semantics are unchanged, and no primary-key readiness is fabricated because the legacy primary path does not return independent PK membership.
- The working tree contained pre-existing planning/spec documentation changes. They were preserved and not staged.

## Commit

Implementation commit: `a33d1a5` (`feat(metadata): define category readiness and load contracts`).
