# InterBase legacy catalog text decoding

Status: implementation handoff approved by the user's takeover request. Database repair and production-database writes are not authorized.

Rollout update: the owner confirmed `WIN1252` for the affected connection during implementation. Earlier `WIN1250` examples were provisional, not deployment defaults.

## Goal and assumptions

Read existing legacy procedure source correctly, complete the sqls catalog load, and restore the `interbase-singleton-select` diagnostic for the partial `BRANCH_CONFIG` key query.

Planning assumption: existing databases stay untouched. The user requested a handoff plan before answering whether database repair was permissible. This design therefore solves reading compatibility, not stored-data migration.

## Evidence

- On the configured nrf01 connection, all 1,775 procedure names and descriptions were readable. Reading `RDB$PROCEDURE_SOURCE` failed at `ACCOUNTINGPERIOD` with SQLCODE -314/native 335544565.
- The same source failed through WIN1250, UTF8, WIN1252, and ISO8859_1 attachments.
- `RDB$PROCEDURE_SOURCE` is declared text BLOB/charset ID 3 (`UNICODE_FSS`). Raw reading succeeded; `ACCOUNTINGPERIOD` contained single-byte `C9` in `CRÉATION`.
- 406 of 1,775 source values failed UTF-8 validation. This measures byte validity, not their exact original encoding. The remaining values are not proven to be Unicode; many may be ASCII.
- MDExplorer: `multidev/Common/MetadataProvider.InterBase.pas` uses `TIBExtract`; `Composantes/IBX_Copy/IBExtract.pas` reads `AsTrimString`. `IBBlob.pas:TIBBlobStream.OpenBlob` calls `isc_open_blob2(..., 0, nil)`. `IBSQL.pas:GetAsString/GetAsCPString` decodes raw bytes with `Database.CharacterSetCodePage`.
- ChainDriveAPI's SV1020, Centrale, and SuperformUser configurations use `CharacterSet=NONE`. No procedure-source reader was established there.
- interbase-go `native.c:ib_read_blob` instead generates a BLOB parameter block requesting conversion from the declared column charset to UTF8.
- sqls currently requires a complete extended catalog before using unique-index metadata. The procedure failure prevents that catalog from completing.

## Chosen approach

Add an explicit **database/sql materialized catalog text** policy to interbase-go, exposed by sqls as a connection-specific setting. On opted-in connections, known catalog text BLOBs are opened without conversion parameters and decoded locally using the specified single-byte encoding. Reuse the driver's bounded BLOB reader and strict iconv conversion.

Alternatives considered:

1. Globally read every text BLOB using the attachment charset: matches part of IBX behavior but changes unrelated user data semantics. Rejected.
2. Retry conversion errors using a guessed encoding: can silently change valid text and conceals configuration mistakes. Rejected.
3. Explicit, narrowly scoped catalog override: selected because it is deterministic, preserves the default, and addresses catalog source/default/description reads across sqls entry points.

This is not automatic encoding detection. Enabling the override asserts that the covered catalog values use one specified legacy encoding. Mixed non-ASCII encodings within those values need a separate, explicit design; byte-validity guessing is not a substitute.

## Public contract

Driver configuration:

```go
// CatalogTextCharset overrides decoding of materialized database/sql text
// BLOBs in the documented catalog fields. Empty honors declared charsets.
// It does not change attachment encoding or direct-API BLOB streams.
CatalogTextCharset string
```

Allowed nonempty values: `WIN1250`, `WIN1252`, `ISO8859_1`, `ASCII`. Trim surrounding whitespace and normalize case. Empty/whitespace means disabled. Reject other values, including `NONE`, `OCTETS`, `UTF8`, `UNICODE_FSS`, and embedded NULs. Unicode databases use the default declared-charset path; this override is deliberately for legacy single-byte catalog text.

sqls configuration:

```yaml
connections:
  - driver: interbase
    # Other connection fields remain as configured.
    params:
      charset: WIN1250
    interbase:
      catalogTextCharset: WIN1250
```

Attachment charset and catalog-text charset are independent. Do not derive one from the other. Do not enable the option automatically for SQL Dialect 1.

## Scope of the override

Match the native SQLDA's original `relname` and `sqlname`, using their explicit lengths, not aliases, SQL text searches, prefix matching, or C-string assumptions. Require `SQL_BLOB` subtype 1 and a materialized database/sql read.

Exact allowlist:

| Relation | Fields |
|---|---|
| `RDB$PROCEDURES` | `RDB$PROCEDURE_SOURCE`, `RDB$DESCRIPTION` |
| `RDB$PROCEDURE_PARAMETERS` | `RDB$DESCRIPTION` |
| `RDB$TRIGGERS` | `RDB$TRIGGER_SOURCE`, `RDB$DESCRIPTION` |
| `RDB$RELATIONS` | `RDB$VIEW_SOURCE`, `RDB$DESCRIPTION` |
| `RDB$RELATION_FIELDS` | `RDB$DEFAULT_SOURCE`, `RDB$DESCRIPTION` |
| `RDB$FIELDS` | `RDB$DEFAULT_SOURCE`, `RDB$COMPUTED_SOURCE`, `RDB$VALIDATION_SOURCE`, `RDB$DESCRIPTION` |
| `RDB$INDICES` | `RDB$EXPRESSION_SOURCE`, `RDB$DESCRIPTION` |
| `RDB$FUNCTIONS` | `RDB$DESCRIPTION` |

All other fields retain existing behavior. In particular: binary BLOBs/BLR, CHAR/VARCHAR, identifier projections, user-table text BLOBs, source-less expressions, writes, and direct-API `BlobRef`/`OpenBlob` are outside this override. The public setting must document that direct streams retain declared-charset semantics.

## Implementation boundaries

- Driver Go configuration owns validation and translating legacy charset names to native IDs.
- Native connection state owns one immutable charset ID, zero meaning disabled. Install it before publishing a newly attached connection. Transaction views inherit it through their existing connection-state copies.
- Native materialized BLOB reading owns exact column classification, raw retrieval, local conversion, and buffer cleanup. On success it returns UTF-8 and marks it already converted, avoiding the attachment-charset conversion later in `ib_cursor_column`.
- `schema.Catalog` continues using the existing projections and public APIs. Its strings receive valid UTF-8 from database/sql.
- sqls only adds configuration validation and passes the field into every driver attachment, including dialect reattachment. Existing snapshot, pooled, and one-shot DDL reads consequently share the same policy.

No cache-independence redesign is included. It remains a separate follow-up after this root cause is fixed.

## Error and resource contract

- Do not replace invalid bytes or use iconv `//IGNORE`/`//TRANSLIT`.
- Local conversion failure returns an error identifying the catalog relation, field, and selected charset, without source contents or credentials.
- Existing open/read/close errors must still propagate; cleanup failures retain the existing broken-connection behavior.
- Null and empty text remain distinct. Preserve line endings, whitespace, and embedded NUL bytes using lengths.
- Preserve the 64 MiB raw materialization and converted-output bounds. Expansion during decoding must also be bounded.
- No per-row metadata SQL is added on the override path, and no process-global mutable encoding setting is introduced.

## Acceptance

1. Native regression reproduces the declared-UNICODE_FSS/single-byte case. Opt-in produces exact Unicode text and uses no conversion BPB.
2. Default catalog conversion and unrelated correctly declared text BLOB behavior remain covered by regression tests.
3. Disposable live fixtures exercise both ordinary queries and prepared statements, explicit transactions, pooled connections, and `schema.Catalog.Procedures`.
4. sqls config works in tagged and untagged builds and validates identically to the driver.
5. Read-only validation on the configured legacy database completes procedure and full catalog loading; verifies real `BRANCH_CONFIG` key metadata; produces the line-273 singleton diagnostic with the full user document.
6. An actual Neovim session running the rebuilt tagged binary receives the warning. Parser-only tests with supplied key metadata do not establish this acceptance.

Before enabling the option for a real connection, confirm the intended code page using the owner's knowledge/configuration and representative characters that distinguish WIN1250 from WIN1252. `É` alone does not distinguish them. If this cannot be established, report that rollout limitation rather than claim exact decoding is verified.
