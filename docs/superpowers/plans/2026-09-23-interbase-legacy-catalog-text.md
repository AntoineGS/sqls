# InterBase Legacy Catalog Text Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Decode existing legacy catalog text correctly and restore the real-database singleton-SELECT warning in sqls.

**Architecture:** Add an opt-in, database/sql-only catalog text charset override in interbase-go. Exact catalog text-BLOB columns use raw reads followed by strict local legacy-to-UTF8 decoding; the default and unrelated BLOB behavior retain declared-charset semantics. sqls exposes the setting and passes it to every attachment.

**Tech Stack:** Go, cgo, C11, InterBase native client/libgds, iconv, database/sql, YAML, LSP; Linux/amd64 for sqls's native adapter.

**Spec:** `docs/superpowers/specs/2026-09-23-interbase-legacy-catalog-text-design.md` (approved for this implementation handoff; database repair excluded).

## Global Constraints

- Existing databases stay untouched; this is an explicit planning assumption awaiting review.
- Allowed override values: empty, `WIN1250`, `WIN1252`, `ISO8859_1`, `ASCII`; trim whitespace and normalize case. Empty disables the override.
- Attachment charset and catalog-text charset are independent. Do not derive one from the other.
- Require materialized database/sql `SQL_BLOB` subtype 1 and exact original relation/field membership in the spec's allowlist.
- Do not replace invalid bytes or use iconv `//IGNORE`/`//TRANSLIT`.
- Preserve the 64 MiB raw materialization and converted-output bounds.
- No process-global mutable encoding setting; no per-row metadata SQL on the override path.
- Direct-API BLOB streams retain declared-charset semantics; document and test this boundary.
- No cache-independence redesign, parser changes, data migration, or general-purpose BLOB-policy framework in this work.
- Synthetic DDL/catalog writes are confined to freshly created integration fixtures. Real nrf01/Centrale checks are SELECT/read-only transactions only.
- Use `patch` for repository edits; preserve unrelated user changes.

## Review Focus

1. A query alias or a user column named `RDB$PROCEDURE_SOURCE` must not activate the override (Task 2 descriptor tests).
2. Prepared statements, explicit transactions, and a second pooled attachment must receive the same immutable policy (Tasks 1 and 3).
3. Conversion expansion, invalid single-byte input, and native cleanup failures must preserve limits and ownership (Task 2).
4. Correctly declared Unicode user BLOBs and direct streams must retain their current semantics with the override enabled (Tasks 2 and 3).
5. A passing singleton unit test does not prove actual metadata loading or LSP publication; verify real cache data and editor publication (Task 5).

---

## Handoff and repository map

Read the spec and this section before taking a task. The implementation is cross-repository and ordered:

```text
Task 1 driver configuration/state
  -> Task 2 materialized BLOB decoding
    -> Task 3 live fixture regression
      -> Task 4 sqls configuration integration
        -> Task 5 real-database and LSP acceptance
```

Do not run writers for these tasks concurrently: Tasks 1–3 share driver files, and Tasks 4–5 require the driver changes. Each task is a suitable fresh smaller-model assignment; give it the spec, global constraints, its task, and the previous task's handoff. Each numbered step is a work checkpoint; finish the task's tests before starting the next task.

Repositories in this environment:

- `sqls`: `/home/a.simard@multidev.local/gits/sqls`.
- Driver: `/home/a.simard@multidev.local/gits/interbase-go` (the sibling `../interbase-go`).
- `sqls/go.mod` currently has `replace interbase-go => ../interbase-go`.
- MDExplorer comparison source: `/home/a.simard@multidev.local/gits/multidev`.

Before execution, inspect each repo's instructions and `git status --short`, and use isolated worktrees following the worktree skill. Keep worktrees as siblings so `../interbase-go` resolves correctly, or use an uncommitted alternate modfile for local verification. Do not accidentally compile sqls against the unchanged driver checkout. Report both repository revisions and replacement resolution in the final handoff.

The investigation's temporary probe was removed. Do not depend on `/tmp/opencode/interbase-go-charset-probe` or reconstruct the previous ad-hoc binary as a product fix.

### File responsibilities

| Repository/file | Responsibility |
|---|---|
| driver `catalog_text.go` (new) | Optional charset normalization and native-ID mapping |
| driver `catalog_text_test.go` (new) | Public configuration/normalization contracts |
| driver `interbase.go` | New `Config.CatalogTextCharset`, validation, connector canonicalization |
| driver `native.go`, `native.h`, `native.c` | Install immutable option after attach/create; exact catalog matching; raw materialization/local conversion |
| driver `tests/native_blob_test.c` | Native policy/byte/resource regressions using the existing stub harness |
| driver `integration/catalog_text_test.go` (new) | Disposable real-server catalog regression |
| driver `README.md` | Scope, usage, encoding responsibility, direct-stream boundary |
| sqls `internal/database/config.go` | YAML/JSON `interbase.catalogTextCharset` and validation |
| sqls `internal/database/interbase_common.go` | Untagged validation/projection |
| sqls `internal/database/interbase_native.go` | Tagged driver mapping |
| sqls `internal/database/interbase_config_test.go`, `interbase_live_test.go` | Mapping, validation parity, reflection-based field checks |
| sqls `internal/handler/interbase_catalog_text_live_test.go` (new) | Opt-in read-only real catalog/diagnostic acceptance |
| sqls `README.md` | Configuration and deployment verification |

Existing large files receive targeted changes. Do not split `native.c` as part of this work. Lines in the investigation may drift; find symbols rather than relying on line numbers.

## Task 1: Add validated driver configuration and native connection state

**Repository:** interbase-go.

**Files:** create `catalog_text.go`, `catalog_text_test.go`; modify `interbase.go`, `native.go`, `native.h`, `native.c`; test `driver_test.go` and native tests as needed.

**Consumes:** `Config`, `validateConfig`, `NewConnector`, `openNativeContext`, `createNativeContext`, `ib_connection`, existing transaction-view copies.

**Produces these exact interfaces:**

```go
// Added to Config.
CatalogTextCharset string

func normalizeCatalogTextCharset(value string) (string, error)
func catalogTextCharsetID(value string) (int16, error)
```

```c
/* native.h; zero disables; reject other unsupported IDs. */
int ib_connection_set_catalog_text_charset(ib_connection *connection,
    int charset, char **error);

/* field in struct ib_connection in native.c */
short catalog_text_charset;
```

- [ ] **1. Add normalization and connector tests.** Use package `interbase`, imports `testing`, `strings`. Define the table below and assert `normalizeCatalogTextCharset`, then inspect `NewConnector(...).(*connector).cfg.CatalogTextCharset` on accepted values. Use `Config{Database:"test.ib", User:"tester", CatalogTextCharset: input}` so no connection is opened.

```go
tests := []struct{ in, want string; invalid bool }{
    {"", "", false}, {"  ", "", false},
    {" win1250 ", "WIN1250", false}, {"win1252", "WIN1252", false},
    {"iso8859_1", "ISO8859_1", false}, {"ascii", "ASCII", false},
    {"NONE", "", true}, {"OCTETS", "", true},
    {"UTF8", "", true}, {"UNICODE_FSS", "", true},
    {"WIN1250\x00", "", true}, {"unknown", "", true},
}
// For invalid values require an error mentioning CatalogTextCharset or
// catalog text charset, not credentials or the entire connection config.
```

Also test native IDs `"" -> 0`, `ASCII -> 2`, `ISO8859_1 -> 21`, `WIN1250 -> 51`, `WIN1252 -> 53`. Test that setting `Charset:"UTF8"` leaves it UTF8 while catalog charset is WIN1250, and vice versa.

- [ ] **2. Run the new tests and record the expected failure.**

```sh
go test . -run 'Test.*CatalogTextCharset' -count=1
```

Expected before implementation: missing field/functions. Do not alter existing `normalizeCharset` semantics to make this pass.

- [ ] **3. Implement the pure Go contract.** The normalizer is deliberately separate because empty means disabled, whereas `normalizeCharset("")` means UTF8.

```go
func normalizeCatalogTextCharset(value string) (string, error) {
    value = strings.ToUpper(strings.TrimSpace(value))
    switch value {
    case "", "WIN1250", "WIN1252", "ISO8859_1", "ASCII":
        return value, nil
    default:
        return "", fmt.Errorf("interbase: unsupported catalog text charset %q", value)
    }
}
```

`catalogTextCharsetID` calls that normalizer and uses the exact ID mapping in step 1. Add validation in `validateConfig`; save the normalized value in `NewConnector`. Document the database/sql-only policy on the exported field.

- [ ] **4. Add and install native state.** Implement the setter with null-connection rejection and the same ID allowlist. Zero-initialized native connections retain disabled behavior. Call it from both native attach and create paths after the handle exists, under the existing native gate and before returning/publishing the connection. Follow the adjacent default-TPB failure cleanup: attach closes a failed handle; create preserves its existing return/ownership contract. Do not change DPB charset or the database default charset.

```c
switch (charset) {
case 0: case IB_CHARSET_ASCII: case IB_CHARSET_ISO8859_1:
case IB_CHARSET_WIN1250: case IB_CHARSET_WIN1252:
    connection->catalog_text_charset = (short) charset;
    return 0;
default:
    return ib_fail(error, "unsupported catalog text charset ID");
}
```

Inspect `transaction->view = *connection` and distributed participant copies to confirm propagation. Use a native unit assertion for both an ordinary transaction view and a distributed view; no global variable and no late mutation when a pool is already in use.

- [ ] **5. Verify and checkpoint.**

```sh
go test . -run 'Test.*CatalogTextCharset|TestConfig' -count=1
make test-native
git diff --check
```

Commit only Task 1 files with `feat: configure legacy catalog text decoding`. Handoff: field/signatures, accepted values, native install sites, command results, commit SHA. The option exists but becomes functional in Task 2; do not release Task 1 independently.

## Task 2: Implement exact catalog classification and raw/local decoding

**Repository:** interbase-go.

**Files:** modify `native.c`, `tests/native_blob_test.c`; update `README.md`.

**Consumes:** Task 1's `ib_connection.catalog_text_charset`; existing `ib_read_blob`, `ib_convert_to_utf8`, `ib_blob_cleanup`, `ib_cursor_column`.

**Produces:** unchanged `ib_read_blob` signature with opt-in behavior; an internal helper:

```c
static short ib_catalog_text_override(const ib_cursor *cursor,
    const XSQLVAR *variable);
```

Returns the configured charset ID only for materialized, allowlisted text BLOBs; otherwise zero. It must also return zero for `cursor->allow_arrays` (the direct-API route).

- [ ] **1. Extend the native stub to supply controlled payloads.** Retain the existing 90,000-byte generated payload as the default for old tests. Add an opt-in byte pointer/length and injected read failure. Reset these between tests. Add a macro replacement for `isc_blob_lookup_desc2` with the exact prototype from installed `ibase.h`; return a text descriptor with charset 3. Track lookup, BPB generation, raw-open, filtered-open, segment-read, close, and cancel counts.

```c
static const unsigned char legacy[] = {'C','R',0xc9,'A','T','I','O','N','\r','\n'};
static const unsigned char want[] = {'C','R',0xc3,0x89,'A','T','I','O','N','\r','\n'};
/* Initialize a real one-column SQLDA with SQL_BLOB|1, subtype=1,
 * sqlind=0, sqldata=&blob_id, original RDB$PROCEDURES /
 * RDB$PROCEDURE_SOURCE names and cursor.fetched=1.
 * Call ib_cursor_column, not only the classifier, to catch double decoding. */
```

- [ ] **2. Write failing end-to-end native assertions.** Configure attachment charset UTF8 and catalog charset WIN1250. Require `view.kind == IB_VALUE_STRING`, exact `want` bytes/length, one raw open, zero BPB calls, zero descriptor lookups, and a closed BLOB. Repeat with attachment charset WIN1252: bytes must stay identical. Add a genuinely distinguishing byte test: raw `0xA5` decodes to `Ą` (`C4 84`) under WIN1250 and `¥` (`C2 A5`) under WIN1252.

Run:

```sh
cc -std=c11 -Wall -Wextra -g -O1 -fsanitize=address -fno-omit-frame-pointer -I/opt/interbase/include tests/native_blob_test.c -L/opt/interbase/lib -Wl,-rpath,/opt/interbase/lib -lgds -o /tmp/opencode/native_blob_catalog_test
ASAN_OPTIONS=detect_leaks=1 /tmp/opencode/native_blob_catalog_test
```

Expected before the fix: declared-charset/filtered-open path selected or exact-output assertion fails. If SDK paths differ, use the values in the driver's Makefile.

- [ ] **3. Implement the classifier.** Encode the exact relation/field pairs in the spec as a static constant table. Check type/subtype and direct-vs-materialized mode first. Compare each original identifier with `length == strlen(expected)` followed by `memcmp`; reject negative/oversized descriptor lengths. The fixed-size SQLDA arrays are not necessarily NUL terminated.

```c
/* Required comparison shape, used for both identifiers. */
static int ib_catalog_name_is(const char *name, short length,
    const char *expected)
{
    size_t n = strlen(expected);
    return length >= 0 && length <= METADATALENGTH &&
        (size_t) length == n && memcmp(name, expected, n) == 0;
}
```

- [ ] **4. Integrate without duplicating the BLOB lifecycle.** At entry to `ib_read_blob`, compute `override_charset`. If nonzero, bypass descriptor lookup/BPB generation and use the existing unfiltered open/segment loop/close. After successful close, convert the complete buffer, free the raw allocation exactly once, set output length and `*is_utf8=1`, and return the converted buffer.

```c
/* After existing successful materialization and close. */
if (override_charset != 0) {
    size_t converted_length = 0U;
    char *converted = ib_convert_to_utf8(data, data_length,
        override_charset, &converted_length, error);
    free(data);
    if (converted == NULL) {
        /* Add length-bounded relation/field and selected charset context
         * while preserving the original error and allocation ownership. */
        return NULL;
    }
    *is_utf8 = 1;
    *length = converted_length;
    return converted;
}
```

Implement error-context construction with `snprintf` bounded `%.*s` identifiers and the configured charset name, joining/preserving the converter error through the existing error helpers. Never include BLOB contents. `ib_cursor_column` must see already-converted output and skip its attachment-code-page conversion. Preserve existing non-override behavior verbatim.

- [ ] **5. Add boundary/regression cases in the same stub.** Each must assert output or selected native calls, not just implementation flags:

| Input | Required assertion |
|---|---|
| Each allowlist pair | override selected for subtype 1 |
| `USER_TABLE.RDB$PROCEDURE_SOURCE` | existing declared-charset path |
| Catalog-like result alias on user field | existing path; aliases never determine policy |
| Allowlisted original field with unrelated alias | override still selected |
| Wrong-case or prefix/suffix relation/field | no override |
| Missing/invalid descriptor lengths | no overread; no override |
| Catalog binary BLOB/subtype 0 or BLR | original bytes, no text conversion |
| Direct cursor (`allow_arrays=1`) | helper returns zero; existing BlobRef semantics |
| Disabled override, declared Unicode source | BPB source charset 3/target 59 retained |
| User Unicode text BLOB with override on | same declared-charset behavior |
| Null value | null result, no open |
| Empty non-null text | empty string, successful close |
| Embedded NUL/CRLF/trailing spaces | exact byte-length preservation |
| 90,000-byte segmented legacy text | exact expanded UTF-8 output; segment boundaries preserved logically |
| ASCII override and raw C9 | error; source contents absent; handle already closed |
| Conversion expands beyond 64 MiB | bounded conversion failure, no leaked buffer |
| Open/read/close failure | original error retained; cleanup and broken-state rules preserved |

Use a high-byte repeated payload for expansion rather than exposing real source. Reuse existing allocation-failure controls; ASAN is the resource check. Do not change all generic text conversion to accommodate these cases.

- [ ] **6. Document and verify.** Add README usage showing `Config{Charset:"UTF8", CatalogTextCharset:"WIN1250"}`, the exact scope/default/direct-stream boundary, and deterministic behavior for mixed catalog encodings. Run `make test-native`, `go test ./... -count=1 -timeout=60s`, and `git diff --check`. Commit with `fix: decode opted-in legacy catalog blobs locally`. Handoff: tests proving raw-read/local-decode, negative-scope cases, cleanup evidence, commit SHA.

## Task 3: Pin the behavior against disposable InterBase databases

**Repository:** interbase-go.

**Files:** create `integration/catalog_text_test.go`; reuse `integration/helpers_test.go` and `integration/blob_test.go` helpers without changing their defaults.

**Consumes:** functional `Config.CatalogTextCharset`, `schema.New(db).Procedures(ctx, name)`, `createFixture`, `fixtureCleanup.addAttachment`.

**Produces:** `TestCatalogTextCharsetLegacyProcedure` and `TestCatalogTextCharsetScope` live fixture regressions.

- [ ] **1. Create the minimal fixture regression.** File uses `//go:build integration`, package `integration_test`. Use `createFixture(t, 1)` and the existing cleanup graph. Create the following procedure in the disposable fixture only:

```sql
CREATE PROCEDURE GO_LEGACY_SOURCE RETURNS (RESULT INTEGER)
AS BEGIN RESULT = 1; SUSPEND; END
```

Use `[]byte` binding to replace that fixture's catalog source with this exact legacy byte payload; the driver's existing `IB_ARGUMENT_BYTES` branch in `ib_bind_input_mode` writes BLOB bytes without text transcoding:

```go
raw := append([]byte("/* CR"), 0xc9)
raw = append(raw, []byte("ATION */\r\nBEGIN RESULT = 1; SUSPEND; END")...)
_, err := setupDB.ExecContext(ctx,
    "UPDATE RDB$PROCEDURES SET RDB$PROCEDURE_SOURCE = ? WHERE RDB$PROCEDURE_NAME = ?",
    raw, "GO_LEGACY_SOURCE")
if err != nil { t.Fatal(err) }
```

`setupDB` is the fixture-owned pool returned by the existing `openDatabase` helper for `fixture.ConnectionString()`, never an environment-selected business database. Assert rows affected is one. If the engine disallows catalog updates, stop and report fixture setup failure; do not weaken the regression to an ordinary user-table BLOB or mark it passing.

- [ ] **2. Open strict and overridden pools.** Use `interbase.NewConnector` and `sql.OpenDB`, registering each in `fixtureCleanup`:

```go
connector, err := interbase.NewConnector(interbase.Config{
    Database: fixture.ConnectionString(), User: cfg.User, Password: cfg.Password,
    Dialect: 1, Charset: "UTF8", CatalogTextCharset: "WIN1250",
})
if err != nil { t.Fatal(err) }
legacyDB := sql.OpenDB(connector)
cleanup.addAttachment(legacyDB)
```

Strict pool uses the same settings with an empty override. Assert strict `QueryRowContext` fails on this source; overridden read equals `"/* CRÉATION */\r\nBEGIN RESULT = 1; SUSPEND; END"` exactly. Assert `utf8.ValidString` as well as exact text. Read metadata and confirm the source field remains charset 3: the read option must not change it.

- [ ] **3. Exercise all consumers of the new state.** Repeat the exact-byte assertion with a prepared statement, a read-only explicit `sql.Tx`, and two simultaneously held `db.Conn(ctx)` values (set max-open-connections to 2). Call `schema.New(legacyDB).Procedures(ctx,"GO_LEGACY_SOURCE")`; assert one procedure and its full source text. These establish propagation beyond a one-shot query.

- [ ] **4. Exercise default and negative scope.** In a separate Unicode fixture create a Unicode procedure comment `/* ěšč */` on a UTF8 attachment, read it with the override absent, and require exact text. Use BMP characters here because UNICODE_FSS catalog storage is not a promise of supplementary-character support. In a legacy-enabled pool create a user table with a UTF8 text BLOB and store/read `ěšč 😀`: require exact Unicode text. Query its column with alias `RDB$PROCEDURE_SOURCE`; it must still use declared semantics. Keep the existing direct-stream integration cases in the targeted run; add a direct attachment configured with the override and prove its user UTF8 BlobRef read remains unchanged.

- [ ] **5. Run live regressions through the existing runner.**

```sh
make test-integration-docker GO_TEST_ARGS='-run "TestCatalogTextCharset|TestBlobTextColumnCharsetAcrossAttachments|TestDirectTextBlobStream" -v'
```

Inspect the runner's quoting if the shell splits the expression; the equivalent direct command is `./scripts/test-integration-docker.sh -run 'TestCatalogTextCharset|TestBlobTextColumnCharsetAcrossAttachments|TestDirectTextBlobStream' -v`. Record the tests actually executed, not only the exit code. A missing SDK/server/license or skipped test is blocked verification, not success. Commit with `test: cover legacy catalog text on InterBase`. Handoff: live output and fixture behavior, commit SHA.

## Task 4: Expose the option in sqls and pin mapping parity

**Repository:** sqls; ensure its module replacement resolves to the completed driver worktree.

**Files:** modify `internal/database/config.go`, `interbase_common.go`, `interbase_native.go`, `interbase_config_test.go`, `interbase_live_test.go`, `README.md`; extend existing YAML tests in `internal/config` if useful for round-trip coverage.

**Consumes:** `interbase.Config.CatalogTextCharset string` from Tasks 1–3.

**Produces exact fields/functions:**

```go
// InterBaseConfig (config.go):
CatalogTextCharset string `json:"catalogTextCharset" yaml:"catalogTextCharset"`
// interBaseConnConfig (interbase_common.go):
CatalogTextCharset string
func interBaseCatalogTextCharset(cfg *DBConfig) (string, error)
```

- [ ] **1. Add failing untagged config tests.** Test nil config error; absent InterBase block -> disabled; whitespace -> disabled; all accepted/rejected values from Task 1. Errors name `connections[].interbase.catalogTextCharset`. Add this mapping case:

```go
cfg := &DBConfig{
    Driver: dialect.DatabaseDriverInterBase, Path: "/fixture.ib", User: "tester",
    Params: map[string]string{"charset": "UTF8"},
    InterBase: &InterBaseConfig{CatalogTextCharset: " win1250 "},
}
got, err := interBaseConnectionConfig(cfg)
if err != nil { t.Fatal(err) }
if got.Charset != "UTF8" || got.CatalogTextCharset != "WIN1250" {
    t.Fatalf("independent charset mapping failed: %#v", got)
}
```

Verify `DBConfig.Validate` rejects invalid values before dialing. Add YAML unmarshalling with nested `interbase.catalogTextCharset`, and ensure another driver with a nonnil InterBase block is still rejected.

- [ ] **2. Run failing tests, then implement the mapping.**

```sh
go test ./internal/database ./internal/config -run 'Test.*CatalogTextCharset|TestInterBaseDriverConfigMapping' -count=1
```

Implement the untagged helper with the same optional legacy allowlist; it must not import the cgo-only driver. Call it from both `DBConfig.Validate` and `interBaseConnectionConfig`. Copy the field in `interBaseDriverConfig`:

```go
CatalogTextCharset: cfg.CatalogTextCharset,
```

No schema API changes are needed. Inspect dialect reattachment and ensure it uses the same projected config rather than constructing a second config that drops the field.

- [ ] **3. Update tagged parity and reflection tests.** `interbase_live_test.go` includes tests that enumerate every `interbase.Config` field (`TestInterBaseDriverConfigMapsEveryFieldToTheDriverStruct`, `TestInterBaseDriverConfigFieldsAreAllKnown`). Add the new field to those expectations. Add a catalog-specific allowlist parity test invoking `interbase.NewConnector` without network attachment. Check that attachment charset normalization is unchanged and every accepted catalog setting reaches the driver.

- [ ] **4. Document configuration and rollout.** Add the spec's YAML setting and explicitly explain: absent = declared-charset behavior; the override is the actual encoding of known catalog BLOB bytes; it does not change normal query charset; uniform legacy catalog encoding is required; remove it for correctly encoded Unicode catalogs. Do not modify the user's personal config yet. Include native build command `make build-interbase` and note that plain `make build` does not enable the adapter.

- [ ] **5. Verify both build modes and checkpoint.**

```sh
go list -m -f '{{.Dir}}' interbase-go
go test ./internal/database ./internal/config -count=1
CGO_ENABLED=1 go test -tags interbase ./internal/database ./internal/config -count=1
git diff --check
```

Tests named `Live` may skip without credentials; Task 5 covers actual integration. Commit with `feat(interbase): configure legacy catalog text decoding`. Handoff: YAML key, parity results, driver directory/revision, commit SHA.

## Task 5: Verify the real catalog, document, and published diagnostic

**Repository:** sqls.

**Files:** create `internal/handler/interbase_catalog_text_live_test.go`; update a results note at `docs/superpowers/plans/2026-09-23-interbase-legacy-catalog-text-results.md` during execution.

**Consumes:** real driver-enabled `database.Open`, `database.NewInterBaseDBRepositoryFromConnection`, `database.NewDBCacheUpdater`, `snapshotDiagnosticCatalog`, `diagnosticsForSnapshot`, and the existing singleton regression tests.

**Produces:** opt-in `TestInterBaseLiveLegacyCatalogSingleton` that exercises real catalog loading and full-document analysis without writing to the database; recorded editor acceptance.

- [ ] **1. Add the opt-in live test.** Use `//go:build interbase && cgo && linux && amd64`, package `handler`. Require all three environment variables below or skip with a precise reason; do not hardcode user paths into the test:

```text
SQLS_LEGACY_CATALOG_CONFIG = absolute path to existing sqls YAML
SQLS_LEGACY_CATALOG_CHARSET = confirmed legacy charset
SQLS_LEGACY_CATALOG_DOCUMENT = absolute path to full sqls_tests.sql
```

Load YAML using `config.GetConfig`. Clone the first connection and its nested `InterBaseConfig` before setting `CatalogTextCharset` in memory. Do not save the config. Use a bounded context (five minutes) and close the connection/transactions. Error logs must not print credentials or the full configuration.

```go
conn, err := database.Open(&cfg)
if err != nil { t.Fatal(err) }
defer conn.Close()
repo := database.NewInterBaseDBRepositoryFromConnection(conn)
updater := database.NewDBCacheUpdater(repo)
cache, err := updater.GenerateDBCachePrimary(ctx)
if err != nil { t.Fatal(err) }
columns, err := updater.GenerateDBCacheSecondary(ctx)
if err != nil { t.Fatal(err) }
cache.ColumnsWithParent = columns
catalog, supported, err := updater.GenerateCatalogCache(ctx)
if err != nil || !supported || catalog == nil {
    t.Fatalf("full catalog failed: supported=%v err=%v", supported, err)
}
cache.Catalog = catalog
```

Before copying this block, inspect the current return types and cache-assignment path in `worker.go`; preserve its semantics if the code has moved since planning. This is a live test of existing public methods, not a new cache implementation.

- [ ] **2. Assert text and real key data before diagnostics.** Look up `ACCOUNTINGPERIOD` in the actual catalog and assert its source contains `CRÉATION`, is valid UTF-8, and contains no replacement characters. Verify the `BRANCH_CONFIG` active unique key has exactly `BRANCHID` and `CONFIG_NAME` (case/order according to actual catalog). Inspect catalog map keys/types instead of assuming a key format. Do not inject fabricated index metadata as a substitute.

Read the complete document from the environment path. Locate the exact offending statement; calculate its line by counting newlines rather than hardcoding 272 in the test (line 273 is the investigated document's current one-based location).

```go
needle := "SELECT F_LEFT(config_value, 1) FROM branch_config WHERE config_name = 'WEB_IMPORT_ALLOW_NO_PAYMENTS' into :AllowNoPayments;"
offset := strings.Index(text, needle)
if offset < 0 { t.Fatal("expected BRANCH_CONFIG reproduction is absent") }
line := strings.Count(text[:offset], "\n")
found := diagnosticsForSnapshot(documentDiagnosticsSnapshot{
    text: text, variant: conn.DriverVariant(),
    cacheSnapshot: snapshotDiagnosticCatalog(cache),
})
// Require diagnosticCode(d) == "interbase-singleton-select" at zero-based line.
```

Also analyze a copy replacing that one predicate with `config_name = 'WEB_IMPORT_ALLOW_NO_PAYMENTS' AND BRANCHID = '00'`; require the warning on that statement to disappear. Preserve other unrelated diagnostics in the full document.

- [ ] **3. Establish the code page before real validation.** Present the owner with the current WIN1250 config and the evidence limit: C9/É is shared with WIN1252. Use known expected text/configuration or representative distinguishing characters to confirm the encoding. If it is not confirmed, the implementation can be tested with synthetic fixtures, but deployment remains unverified. Do not choose a code page merely because it eliminates the exception.

- [ ] **4. Run read-only real acceptance.** For this environment, the config and document paths are `~/.config/sqls/config.yml` and `~/OneDrive/Dev/sqls_tests.sql`. After WIN1250 is confirmed, the command is:

```sh
SQLS_LEGACY_CATALOG_CONFIG="$HOME/.config/sqls/config.yml" SQLS_LEGACY_CATALOG_CHARSET=WIN1250 SQLS_LEGACY_CATALOG_DOCUMENT="$HOME/OneDrive/Dev/sqls_tests.sql" CGO_ENABLED=1 go test -tags interbase ./internal/handler -run '^TestInterBaseLiveLegacyCatalogSingleton$' -count=1 -v -timeout=6m
```

Record actual procedure count (1,775 is historical, not a permanent assertion), source check, catalog completion, actual index columns, and diagnostic line. If another catalog category fails, retain its exact error and investigate it; do not expand the override to all fields or suppress errors to pass.

- [ ] **5. Run final suites and build the executable.**

```sh
go test ./... -count=1
CGO_ENABLED=1 go test -tags interbase ./... -count=1
make build-interbase
go version -m ./sqls
ldd ./sqls
git diff --check
```

Verify `-tags=interbase`, `CGO_ENABLED=1`, and `libgds` linkage. These commands run from the intended sqls worktree. Record both sqls and driver revisions; the sqls binary's VCS revision alone does not identify a local replacement dependency.

- [ ] **6. Validate actual Neovim publication.** Have the owner enable `interbase.catalogTextCharset` on the intended connection and launch the verified executable (Neovim currently points to `~/gits/sqls/sqls`, not an arbitrary worktree binary). Restart that LSP client, open the full document, wait for catalog loading, and inspect diagnostics. Confirm `interbase-singleton-select` at the actual statement and absence of the procedure transliteration failure in the new log interval. Capture code/range/severity, not only a screenshot or absence of an error.

No real SQL statement in the document needs to be executed. If editor access/restart must be performed by the owner, report this item as pending instead of declaring complete end-to-end success.

- [ ] **7. Checkpoint the acceptance artifacts.** Results note includes exact commands, pass/fail/skip status, revisions, nonsecret metadata checks, code-page confirmation, and any pending editor step. Do not copy stored procedure bodies, passwords, or connection strings into the repo. Commit the live test and results note with `test(interbase): verify legacy catalog singleton diagnostics`.

## Definition of done and handoff format

- [ ] Spec assumptions and explicit override scope reviewed by the owner.
- [ ] Driver configuration, native raw-read/local-decode, and fixture tests pass.
- [ ] Untagged and native sqls tests pass with the intended sibling driver.
- [ ] Real read-only catalog and exact full-document diagnostic checks pass.
- [ ] Owner-confirmed catalog encoding and actual editor publication recorded.
- [ ] README covers default behavior, exact scope, and how to remove the override.
- [ ] Each repo's commits, worktree paths, test output, and deployment binary are identified.

Each task's worker should return:

```text
Task / repository / commit:
Interfaces implemented:
Tests run (commands and outcomes, including skips):
Unexpected findings or deviations:
Inputs needed by next task:
```

Keep the separate cache-independence issue in the final follow-up note. Do not make that change during this implementation or use it to substitute for successful procedure-source decoding.
