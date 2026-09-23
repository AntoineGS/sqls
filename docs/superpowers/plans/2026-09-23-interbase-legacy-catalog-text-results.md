# InterBase legacy catalog text acceptance results

## Scope and safety

- sqls revision under test: `a15aba999212968222640d144c693f82c3b602a0` (branch `feat/interbase-legacy-catalog-text`).
- Local `interbase-go` replacement: `../interbase-go`, revision `8009fefe498428adb36420b5eabd8938eae3d3f3`.
- Real validation was performed with the owner-confirmed `WIN1252` setting. `É` alone was not treated as code-page evidence; the owner confirmed the intended code page.
- The opt-in test loads the existing config and complete SQL document from the environment, copies the connection and nested InterBase config, and changes only the in-memory copy. It issues catalog reads only. No SQL from the document was executed, no business-database writes were performed, and no personal config/editor files were changed.
- Config/document paths and connection information are intentionally omitted. Set `SQLS_LEGACY_CATALOG_CONFIG`, `SQLS_LEGACY_CATALOG_CHARSET`, and `SQLS_LEGACY_CATALOG_DOCUMENT` to absolute config/document paths and the confirmed charset to run the acceptance test.

## Real catalog and document acceptance

Command (environment values intentionally omitted):

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

The first full-document run exposed a test-only coordinate mistake: the assertion used the absolute document offset instead of LSP's line-relative UTF-16 character. The observed diagnostic was at zero-based `272:4–272:10`; the assertion now derives line-relative UTF-16 coordinates and the final live run passes. No catalog or document acceptance condition was relaxed.

Opt-in guard check (environment variables absent):

```sh
CGO_ENABLED=1 go test -tags interbase ./internal/handler \
  -run '^TestInterBaseLiveLegacyCatalogSingleton$' -count=1 -v -timeout=6m
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
