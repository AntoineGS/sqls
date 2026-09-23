# InterBase Native CHAR Decoding Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop `SQL_TEXT` from silently dropping full identifier and user-data bytes while preserving fixed `CHAR` padding whenever its declared character width is known.

**Architecture:** Use the already-populated `cursor->metadata[index].length` for known base-column widths and `cursor->fixed_text_lengths[index]` for validated fixed casts. Decode complete bytes before taking a character prefix; with no provable width, return the complete fixed buffer rather than an incorrect `sqllen / maxBytesPerCharacter` prefix. The driver catalog accessors from the companion plan use `VARCHAR` projections for catalog fields whose character width is NULL.

**Tech Stack:** C11, InterBase SQLDA, Go native adapter, ASan native tests.

**Spec:** `../sqls/docs/superpowers/specs/2026-09-22-interbase-catalog-ddl-accuracy-design.md` (paths relative to `../interbase-go`).

## Global Constraints

- Preserve real trailing spaces, including in literals; do not trim user data.
- Never divide byte capacity by charset maximum width to cap a value.
- Do not split multibyte sequences; decode/convert before applying a known character count.
- No new per-row catalog queries: `ib_cursor_describe_metadata` already calls `ib_metadata_catalog` before fetch, and `ib_catalog_apply_row` supplies `cursor->metadata[index].length` when `RDB$CHARACTER_LENGTH` is present.
- Unknown width is a documented full padded buffer, not a fabricated exact CHAR length.

## Review Focus

- UTF8 `CHAR(5)` backed by 20 bytes still returns exactly five characters (Task 1).
- A catalog UNICODE_FSS `CHAR` with NULL character length returns the complete 29-character name, not its first 22 (Task 1).
- Trailing spaces in a literal or fixed cast are preserved (Task 1).
- A non-UTF8 attachment with a multibyte source preserves complete characters after conversion (Task 2).
- A known width inconsistent with its descriptor cannot read out of bounds or silently truncate (Task 2).

## File map

- `native.c`: replace the `ib_text_character_count_for_charset` result cap in the SQL_TEXT result branch and adapt the UTF8 and non-UTF8 paths to a single validated character-prefix decision. Use existing `cursor->metadata` and `fixed_text_lengths` populated before fetch.
- `tests/native_values_test.c`: deterministic SQLDA tests for known/unknown widths, padding, literals and invalid widths.
- `integration/metadata_test.go`: disposable-fixture coverage for real attributed CHAR metadata; never mutate a production DB.
- `schema/README.md`: document unknown-width and strict catalog read behavior when relevant.

---

### Task 1: Known versus unknown fixed CHAR widths

**Files:** Modify `native.c`, `tests/native_values_test.c`.

**Interfaces:** Consume `cursor->metadata[index].has_length`/`.length` and `cursor->fixed_text_lengths[index]`; produce the same `ib_cursor_column(cursor, index, view, error)` API, with `view->length` based on a validated known character count or the full `sqllen` buffer.

- [ ] **Step 1: Write failing C tests.** Make two SQLDA `SQL_TEXT` columns: an attributed UTF8 `CHAR(5)` with `sqllen=20` and `cursor.metadata[0].length=5, has_length=1`, expecting `"AA   "`; and an attributed UNICODE_FSS catalog identifier with `sqllen=67`, NULL/unknown character width and 29 ASCII name bytes followed by spaces, expecting all 29 bytes present (full 67-byte padded result). Keep the existing literal/trailing-space test. Allocate/free cursor metadata with the test's existing helpers.

```c
require_success(ib_cursor_column(&cursor, 0, &view, &error), error, "known CHAR");
require_condition(view.length == 5U && memcmp(view.bytes, "AA   ", 5U) == 0,
    "known CHAR width lost padding or gained capacity padding");
```

- [ ] **Step 2: Run** `make test-native` from `interbase-go`; expect the new tests to fail in the two opposing directions (catalog name lost versus known-width padding). If ASan cannot start in this environment, run the same compile commands from `Makefile:29-35` without ASan while recording that distinction.
- [ ] **Step 3: Implement** a helper that validates the known character count against the SQLDA buffer and takes the prefix at character boundaries. For UTF8, use the existing `ib_utf8_prefix_length`; for converted charsets, convert the full buffer first and then prefix the UTF8 result by the proven count. If neither a valid metadata count nor a validated explicit CAST width is available, decode/convert the complete `sqllen` bytes. Never trim trailing blanks or use an invalid count as a cap. Maintain `view->kind`, `view->bytes` ownership, and NULL handling.
```c
/* Use a catalog-confirmed or validated cast width; unknown means no cap. */
if (cursor->metadata != NULL && cursor->metadata[index].has_length &&
    cursor->metadata[index].length > 0) {
    character_count = (size_t) cursor->metadata[index].length;
} else if (cursor->fixed_text_lengths != NULL) {
    character_count = cursor->fixed_text_lengths[index];
} else {
    character_count = 0U;
}
/* Convert the complete buffer before taking character_count characters. */
```

- [ ] **Step 4: Run** `make test-native` and `go test ./... -count=1`; confirm both new assertions and old fixed-CHAR/literal tests pass. Inspect the ASan report and `git diff --check`.
- [ ] **Step 5: Commit** `native.c tests/native_values_test.c` with `fix(native): preserve complete text when CHAR width is unknown`.

### Task 2: Multibyte conversion and metadata integration

**Files:** Modify `tests/native_values_test.c`, `native.c` for conversion/order correctness, and `integration/metadata_test.go` for a disposable fixture.

**Interfaces:** `ib_cursor_describe_metadata` and `ib_catalog_apply_row` already provide `ib_column_metadata.length`; no Go public API change. Cover both `connection_charset == IB_CHARSET_UTF8` and legacy attachment conversion.

- [ ] **Step 1: Add failing/characterization tests** for (a) known-width multibyte UTF8 `CHAR(5)` with a two-byte character and four padding spaces, (b) a non-UTF8 attachment decoding a complete UNICODE_FSS/legacy source, (c) a literal ending in spaces, and (d) an impossible declared width that must not read past `sqllen`. Add a disposable Dialect 1 fixture with a `CHAR(5)` user column to confirm the actual `RDB$CHARACTER_LENGTH` path when fixture tooling is available.
- [ ] **Step 2: Run** the focused C test binary via `make test-native` and `go test ./integration -tags integration -run 'Test.*Char.*Metadata' -count=1` only against the project's disposable integration fixture; never point integration DDL at the configured live database.
- [ ] **Step 3: Adjust byte-to-character conversion** only for observed failures. Reuse `ib_convert_to_utf8` for non-UTF8 attachments, take a character prefix after conversion, and return the same conversion error on malformed input. Reject an impossible count rather than letting pointer arithmetic exceed allocated bytes; retain the conservative full-buffer behavior when there is no count.
```c
/* For converted charsets, work on the complete converted UTF8 buffer: */
converted = ib_convert_to_utf8(variable->sqldata, (size_t) variable->sqllen,
    connection_charset, &converted_length, error);
if (converted == NULL) return -1;
source_length = character_count == 0U ? converted_length :
    ib_utf8_prefix_length(converted, converted_length, character_count);
```

- [ ] **Step 4: Run** `make test-native`, `go test ./... -count=1` and the isolated integration test when the disposable fixture exists. Separately perform read-only live `SELECT` parity for long `RDB$RELATION_NAME` and a known-width `CHAR` value, comparing cast and uncast reads without changing database data.
- [ ] **Step 5: Commit** source and tests with `fix(native): validate multibyte CHAR width and conversion`.

**Completion gate:** A driver-level uncast catalog SELECT does not lose the final `S` of `IMPORT_ORDER_LINE_ITEMS`, known-width user `CHAR` remains padded to its declared character length, and an unknown-width catalog read exposes full bytes. Do not deploy or restart the running editor during this plan.
