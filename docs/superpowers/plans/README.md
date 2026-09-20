# InterBase integration — execution order

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
| `internal/database` capability mock | catalog migration, editor surfaces | Both add a capability-bearing mock distinct from `MockDBRepository`. Resolve to one type when the second lands rather than carrying two. |

## What is deliberately not here

Services Manager administration — backup, restore, sweep, validate,
statistics, logs — is out of scope for the whole project. The driver exposes it
and the existing MCP servers already cover it for these databases; an editor
language server is a poor host for it.
