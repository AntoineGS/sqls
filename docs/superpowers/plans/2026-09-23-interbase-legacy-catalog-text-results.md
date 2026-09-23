# InterBase legacy catalog text acceptance results

## Scope and safety

- sqls base revision: `a15aba999212968222640d144c693f82c3b602a0` (branch `feat/interbase-legacy-catalog-text`); full-range test revision `b6231878df4678a2f80721f022ad34bfea25edaa`; timeout-diagnostic test-only changes committed as `2b3120f727dd1a7c22643ad206a54b8b9540f7a2`.
- Local `interbase-go` replacement: `../interbase-go`, revision `8009fefe498428adb36420b5eabd8938eae3d3f3`.
- Real validation was performed with the owner-confirmed `WIN1252` setting. `É` alone was not treated as code-page evidence; the owner confirmed the intended code page.
- The opt-in test loads the existing config and complete SQL document from the environment, copies the connection and nested InterBase config, and changes only the in-memory copy. It issues catalog reads only. No SQL from the document was executed, no business-database writes were performed, and no personal config/editor files were changed.
- Config/document paths and connection information are intentionally omitted. Set `SQLS_LEGACY_CATALOG_CONFIG`, `SQLS_LEGACY_CATALOG_CHARSET`, and `SQLS_LEGACY_CATALOG_DOCUMENT` to absolute config/document paths and the confirmed charset to run the acceptance test.

## Real catalog and document acceptance

Original worker acceptance command (environment values intentionally omitted; this predated the timeout/range instrumentation):

```sh
SQLS_LEGACY_CATALOG_CONFIG="$CONFIG_PATH" \
SQLS_LEGACY_CATALOG_CHARSET=WIN1252 \
SQLS_LEGACY_CATALOG_DOCUMENT="$DOCUMENT_PATH" \
CGO_ENABLED=1 go test -tags interbase ./internal/handler \
  -run '^TestInterBaseLiveLegacyCatalogSingleton$' -count=1 -v -timeout=6m
```

**PASS** (290.43 seconds):

- Complete catalog cache loaded; 1,775 procedures were observed (measurement only, not a permanent expected count).
- Actual `ACCOUNTINGPERIOD` source was valid UTF-8, contained `CRÉATION`, and contained no replacement character.
- Inspected catalog index metadata directly. An active unique `BRANCH_CONFIG` key with exactly `BRANCHID` and `CONFIG_NAME` was present.
- Full-document analysis found `interbase-singleton-select` at one-based line 273, derived by counting newlines in the document.
- Re-analysis after adding the `BRANCHID = '00'` predicate removed the target warning while preserving unrelated diagnostics.
- No additional catalog category failed.

During coordinate debugging, the observed diagnostic range was zero-based `272:4–272:10`. The test calculates and asserts the complete start/end UTF-16 range for the actual `SELECT` token.

### Timeout investigation and post-range-fix diagnostic runs

The test now uses an eight-minute application context and logs only phase, elapsed duration, remaining deadline, error type, `errors.Is` checks for deadline/cancellation, and the context sentinel. Snapshot fallback package logging is suppressed only during `GenerateCatalogCache`, so raw backend errors cannot be emitted. Catalog repository and snapshot code paths are unchanged. No SQL/config/source values are logged.

The earlier primary run failed at exactly 300 seconds inside `GenerateCatalogCache` with a wrapped-error type and `supported=false`; it did not record `errors.Is` or context sentinel evidence, so the timeout cause was not proven. The prior worker pass took 290.43s under a five-minute context. The new runs localize most time to full catalog loading; no deeper repository decorator or production cache instrumentation was added.

Both following runs used the post-range command below, owner-confirmed WIN1252, the same environment-driven config/document inputs, and were sequential with no concurrent live run. No SQL from the document was executed; DB access was catalog/cache reads only.

| Run | Open | Primary | Secondary | Full catalog | Total | Error/context evidence |
|---|---:|---:|---:|---:|---:|---|
| Post-range run 1 | 16.5 ms | 4.941 s | 5.339 s | 4m40.614 s | 4m50.935 s | All phases succeeded; error type nil; deadline/canceled false; `ctx.Err()` nil; 3m9.065s remaining |
| Post-range run 2 | 17.2 ms | 5.530 s | 5.947 s | 4m56.392 s | 5m7.915 s | All phases succeeded; error type nil; deadline/canceled false; `ctx.Err()` nil; 2m52.085s remaining |

Both runs passed all catalog, source UTF-8/`CRÉATION`, real key, full-document warning, complete UTF-16 range, and corrected-predicate/unrelated-diagnostic assertions. 1,775 procedures were observed in each run. Each run remained well within its eight-minute app context, but similar ~5-minute durations and a prior exact-five-minute failure mean this is repeated read-only evidence, not a latency guarantee. No category-specific error was returned; snapshot fallback logs were intentionally suppressed and fallback status was not separately instrumented. The dominant measured duration is the full catalog phase.

Post-range command (executed twice sequentially; environment values intentionally omitted):

```sh
SQLS_LEGACY_CATALOG_CONFIG="$CONFIG_PATH" \
SQLS_LEGACY_CATALOG_CHARSET=WIN1252 \
SQLS_LEGACY_CATALOG_DOCUMENT="$DOCUMENT_PATH" \
CGO_ENABLED=1 go test -tags interbase ./internal/handler \
  -run '^TestInterBaseLiveLegacyCatalogSingleton$' -count=1 -v -timeout=9m
```

Opt-in guard check (environment variables absent):

```sh
CGO_ENABLED=1 go test -tags interbase ./internal/handler \
  -run '^TestInterBaseLiveLegacyCatalogSingleton$' -count=1 -v -timeout=9m
```

**PASS / expected SKIP**: test reported that all three `SQLS_LEGACY_CATALOG_*` environment variables are required; no connection was opened.

## Test suites and native build

| Command | Result |
|---|---|
| `go test ./... -count=1` | PASS |
| `CGO_ENABLED=1 go test -tags interbase ./... -count=1` | PASS |
| `make build-interbase` | PASS |
| `go version -m ./sqls` | Go `go1.27.1`; includes `-tags=interbase`, `CGO_ENABLED=1`, linux/amd64, and `vcs.revision=a15aba999212968222640d144c693f82c3b602a0` |
| `ldd ./sqls` | PASS; links `/opt/interbase/lib/libgds.so` |
| `git diff --check` | PASS |

The executable was removed after checking build metadata and linkage; it is not an acceptance artifact or commit candidate.

## Editor publication

**PENDING owner action.** Neovim publication was not validated from this worktree. The owner must enable `interbase.catalogTextCharset: WIN1252` for the intended connection, deploy the verified executable at the path used by the editor, restart its LSP client, open the full document, and inspect the new diagnostics/log interval. Confirm the warning code/range/severity and absence of the procedure-source transliteration failure. No editor settings or publication paths were changed here. Do not claim end-to-end editor acceptance until that step is observed.

## Follow-up boundary

The separate cache-independence issue remains out of scope and unchanged; no cache redesign was made as a substitute for source decoding.
