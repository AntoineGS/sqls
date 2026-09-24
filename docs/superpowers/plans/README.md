# InterBase integration — execution order

## Current performance/readiness planning package

See [the 2026-09-23 handoff index](2026-09-23-metadata-performance-handoff.md)
for the new four-plan, 21-task package covering progressive startup, isolated
metadata jobs, InterBase bulk loading, and performance verification. Its design
and dependency order supersede the older startup/cache assumptions where noted.

## Earlier integration plans

Nine implementation plans across two repositories deepen the InterBase
integration in sqls. Each plan is self-contained and states its own
dependencies; this file records the order they have to run in and the rules for
the files more than one of them touches.

Two plans live in the driver repository at `../interbase-go`:

- `interbase-go/docs/superpowers/plans/2026-09-19-pooled-introspection.md`
- `interbase-go/docs/superpowers/plans/2026-09-19-schema-catalog-accessors.md`

The specs they implement are in the sibling `specs/` directory of each
repository.

## Order

Plans on the same line have no dependency on each other and can run in
parallel or in either order.

| Wave | Plan | Repo | Depends on |
| --- | --- | --- | --- |
| 1 | Pooled introspection | interbase-go | — |
| 1 | `schema` catalog accessors | interbase-go | — |
| 1 | Server concurrency and cancellation | sqls | — |
| 2 | Dialect resolution and propagation | sqls | pooled introspection |
| 2 | Results-pane semantics | sqls | server concurrency (§6.4 also needs catalog migration) |
| 3 | Catalog migration, capabilities and cache | sqls | dialect propagation, `schema` accessors |
| 3 | Connection configuration and database identity | sqls | dialect propagation |
| 4 | Catalog-backed editor surfaces | sqls | catalog migration, server concurrency |
| 4 | Go-to-definition snapshots | sqls | catalog migration, server concurrency |

**Server concurrency goes first, not last.** It is the only sqls plan with no
dependency on the driver work, so it can run while wave 1's driver plans are in
flight — and without it the context reaching the driver is never cancelled, so
the cancelled/uncertain result rendering it ships would be unreachable code.

**Dialect propagation delivers user-visible value before any refactor.** It
fixes a live correctness bug: sqls lexes InterBase as SQL Dialect 1 while the
driver attaches as Dialect 3, so `"My Column"` is read as a string where the
server sees an identifier.

Three plans can start before the work they depend on is finished, and each says
so at the task that blocks:

- Catalog migration ships everything except three display fields until the
  `schema` accessors land; those fields render `""`, which is the documented
  undecodable value.
- Results-pane ships §6.2 and §6.3 with no catalog at all; only §6.4 waits.
- Definition snapshots' first three tasks build the store, which has no catalog
  dependency.

## Files more than one plan touches

Each rule below is restated in the plans themselves at the point it matters;
this is the index, not the authority.

| File | Plans | Rule |
| --- | --- | --- |
| `internal/database/worker.go` | server concurrency, catalog migration | Concurrency adds `w.repo()`/`setRepo()`; the catalog rewrite of the `case <-w.update:` body must keep `w.repo()` and never reintroduce `w.dbRepo`. |
| `internal/database/interbase_native.go` | dialect propagation, connection config, catalog migration | Dialect propagation owns `interBaseOpen`; the other two append or edit inside the shape it leaves. |
| `internal/handler/handler.go` | server concurrency, dialect propagation, editor surfaces, definition snapshots | Concurrency introduces `stateMu`/`connMu`; later plans add fields and classify them in `doc/develop.md`. |
| `Server.connGeneration` | editor surfaces, definition snapshots | Neither owns it. Both grep first and consume an existing field; the name is fixed so they converge without coordination. |
| `doc/develop.md` audit table | server concurrency, editor surfaces, definition snapshots | Concurrency creates it; the others add their own rows only. |
| `README.md` InterBase section | dialect propagation, catalog migration, connection config, editor surfaces, definition snapshots | Sentence-scoped ownership, so the five can land in any order. Dialect propagation owns the section title, the dialect paragraph and the two dialect sentences; catalog migration owns the metadata sentence; connection config owns the database-enumeration sentence, the connection keys and the TLS statement; editor surfaces and definition snapshots share the `#### InterBase editor features` subsection, each creating it if absent. |
| `parser/parser.go` `multiKeywordMap` | editor surfaces | Editor surfaces owns the `"EXECUTE": {"PROCEDURE"}` entry. Results-pane deliberately parses statement text instead so it does not depend on it. |
| `internal/database` capability mock | catalog migration, editor surfaces | Both add a capability-bearing mock distinct from `MockDBRepository`. They do not collide, so neither plan is blocked; merge them in a cleanup commit once both have landed — see below. |

## One cleanup after wave 4 (completed)

Completed by `bde8d68`: `MockCapabilityRepository` now lives in
`capability_mock.go` with all seven `Describe*` hooks; `interbase_mock.go` is
removed. The consolidated guard test preserves positive and negative checks
for all three capability interfaces. The scheduling rationale below is retained
as historical context.

Catalog migration adds `MockCatalogDBRepository` in `capability_mock.go`;
editor surfaces adds `MockCapabilityRepository` in `interbase_mock.go`. Both
embed `*MockDBRepository` and both exist because the plain mock must not satisfy
the capability interfaces — every existing handler test would start taking the
capability branch and panic on a nil func field.

The two were written independently on purpose: forcing them to share a type up
front would create an ordering dependency between two plans that are otherwise
independent, which is a worse trade than one cleanup commit. They do not
collide — different identifiers, different files, non-colliding test names — so
nothing fails to compile while both are present.

When both have landed, merge them: keep `MockCapabilityRepository`, which names
the general concept rather than one capability and has three consumers to the
other's one, move it into `capability_mock.go`, fold in the seven `Describe*`
fields, delete `interbase_mock.go`, and keep whichever of the two
"`MockDBRepository` must not satisfy the capability interfaces" guard tests
covers more interfaces.

## What is deliberately not here

Services Manager administration — backup, restore, sweep, validate,
statistics, logs — is out of scope for the whole project. The driver exposes it
and the existing MCP servers already cover it for these databases; an editor
language server is a poor host for it.
