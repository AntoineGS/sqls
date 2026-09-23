# InterBase legacy catalog text acceptance results

## Scope and safety

- sqls base revision: `a15aba999212968222640d144c693f82c3b602a0` (branch `feat/interbase-legacy-catalog-text`); full-range test revision `b6231878df4678a2f80721f022ad34bfea25edaa`; timeout-diagnostic commit `2b3120f727dd1a7c22643ad206a54b8b9540f7a2`; capability-preserving snapshot privacy forwarder commit `4c11e7f7a9d5043352e2dd191123c8a564e0f906`; deployed native binary revision `3de1b1a56c8570573cd3f74f91cbd6cb00f37af9` (later results-only commit not embedded).
- Local `interbase-go` replacement: `../interbase-go`, revision `8009fefe498428adb36420b5eabd8938eae3d3f3`.
- Real validation was performed with the owner-confirmed `WIN1252` setting. `É` alone was not treated as code-page evidence; the owner confirmed the intended code page.
- The opt-in test loads the existing config and complete SQL document from the environment, copies the connection and nested InterBase config, and changes only the in-memory copy. It issues catalog reads only. No SQL from the document was executed and no business-database writes were performed. A separate, explicitly authorized editor rollout changed the personal config and executable only after read-only validation; see Editor publication below.
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

The test uses an eight-minute application context and logs phase elapsed/deadline remaining, error type, `errors.Is` deadline/cancellation flags, and the context sentinel. The current implementation uses a test-only forwarder embedding the real DB/catalog repository capabilities and delegating every snapshot call. Successful snapshot repositories and closers are passed through unchanged; failures are recorded as sanitized type/flags and replaced with a fixed non-wrapping sentinel before the existing cache fallback can log them. Nil-repository/nil-error fallback is tracked explicitly. It does not intercept the process-global logger or alter production snapshot/cache code. This is scoped protection for snapshot-fallback errors, not a claim of universal secret-safety for arbitrary driver/process output.

The earlier primary run failed at exactly 300 seconds inside `GenerateCatalogCache` with a wrapped-error type and `supported=false`; it did not record `errors.Is` or context sentinel evidence, so the timeout cause was not proven. The prior worker pass took 290.43s under a five-minute context. The new runs localize most time to full catalog loading; no deeper repository decorator or production cache instrumentation was added.

Both following runs used the post-range command below, owner-confirmed WIN1252, the same environment-driven config/document inputs, and were sequential with no concurrent live run. No SQL from the document was executed; DB access was catalog/cache reads only.

| Run | Open | Primary | Secondary | Full catalog | Total | Error/context evidence |
|---|---:|---:|---:|---:|---:|---|
| Pre-wrapper post-range run 1 | 16.5 ms | 4.941 s | 5.339 s | 4m40.614 s | 4m50.935 s | All phases succeeded; error type nil; deadline/canceled false; `ctx.Err()` nil; 3m9.065s remaining |
| Pre-wrapper post-range run 2 | 17.2 ms | 5.530 s | 5.947 s | 4m56.392 s | 5m7.915 s | All phases succeeded; error type nil; deadline/canceled false; `ctx.Err()` nil; 2m52.085s remaining |
| Primary independent pre-wrapper run | 17.8 ms | 5.706 s | 5.644 s | 4m53.353 s | 5m4.760 s | All assertions passed; no context error; 2m55.240s remaining |

All three pre-wrapper runs passed the catalog, source UTF-8/`CRÉATION`, real key, full-document warning, complete UTF-16 range, and corrected-predicate/unrelated-diagnostic assertions. 1,775 procedures were observed in each. These passes predate the snapshot forwarder below and do not validate its behavior or prove that snapshots were used. The dominant measured duration is the full catalog phase.

The primary's independent **post-wrapper** read-only run also passed, using the command below against revision `a8c4a1362709f607264905980829aec87feb83c8`. The primary, secondary, and full-catalog snapshots each reported `snapshot=used`, with no fallback or context error. Open took 18.2 ms, primary 5.022 s (snapshot 4.990 s), secondary 4.972 s (snapshot 4.952 s), full catalog 5m0.759s (snapshot 4.963 s), and total 5m10.802s, leaving 2m49.198s on the application deadline. All 1,775 procedures, source UTF-8/`CRÉATION`, real unique key, full-document diagnostic at line 273 with complete UTF-16 range, and corrected-predicate assertions passed. This proves the optimized snapshot path was used in this run; the fake tests separately exercise fallback sanitization, but real fallback was not induced.

Post-range command (two sequential worker runs and independent primary runs, all non-concurrent; environment values intentionally omitted):

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

Snapshot-forwarder focused tests (the test file is native-tagged):

```sh
CGO_ENABLED=1 go test -tags interbase ./internal/handler \
  -run '^TestCatalogSnapshotPrivacyForwarder' -count=1 -v
go test ./internal/handler -count=1
CGO_ENABLED=1 go test -tags interbase ./internal/handler -count=1
```

**PASS**: success preserves catalog/snapshot capability, exact snapshot repository and closer; error fallback returns a fixed non-wrapping sentinel without the fake secret marker while recording safe type/deadline metadata and preserving the closer; nil-repository/nil-error fallback is recorded as fallback. Tagged and untagged handler suites pass. The post-wrapper real run above passed with snapshots used in all phases.

## Test suites and native build

| Command | Result |
|---|---|
| `go test ./... -count=1` | PASS |
| `CGO_ENABLED=1 go test -tags interbase ./... -count=1` | PASS |
| `make build-interbase` | PASS |
| `go version -m ./sqls` | Go `go1.27.1`; includes `-tags=interbase`, `CGO_ENABLED=1`, linux/amd64, and `vcs.revision=3de1b1a56c8570573cd3f74f91cbd6cb00f37af9` for the deployed native executable |
| `ldd ./sqls` | PASS; links `/opt/interbase/lib/libgds.so` |
| `git diff --check` | PASS |

The worktree executable remains an ignored build artifact, not a commit candidate. The corresponding verified binary was copied atomically into the editor path only after the owner's authorization; see Editor publication.

## Editor publication

**PASS, explicitly authorized by the owner.** The personal YAML was backed up privately before adding `interbase.catalogTextCharset: WIN1252` to the **first** InterBase connection only; the second connection remained unchanged and the config retained mode 0600. The previous editor-path executable was backed up privately, then replaced atomically with the verified native-tagged binary. The deployed binary reports `CGO_ENABLED=1`, `-tags=interbase`, revision `3de1b1a56c8570573cd3f74f91cbd6cb00f37af9`, and dynamic `libgds.so` linkage. Neither backup nor any personal config content was copied into the repository.

The active Neovim session had the complete SQL document loaded. After restarting its `sqls` LSP, a **new** client (id 3) attached to that buffer and published `interbase-singleton-select` in diagnostic namespace `nvim.lsp.sqls.3`, at zero-based **`272:4–272:10`** (one-based line 273), severity **2 (warning)**. A read of the new LSP log interval found **zero** `Cannot transliterate character between character sets` messages. The previous stopped client process was terminated after a graceful LSP disable did not exit its process; the new client remained active.

## Follow-up boundary

The separate cache-independence issue remains out of scope and unchanged; no cache redesign was made as a substitute for source decoding.
