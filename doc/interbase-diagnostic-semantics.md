# InterBase diagnostic type/conversion semantics

This document is the authoritative ledger for every assignment-compatibility
rule `internal/sqlsymbol/diagnostic_types.go` (`parseDiagnosticType`,
`assignmentCompatibility`) enables or deliberately leaves disabled. Per the
Milestone B ground rule: do not invent an expected native InterBase result;
if a reference is inconclusive, the rule stays unknown, not enabled.

Live-probe status for every rule below is **NOT VERIFIED — no live database
available this session.** Live verification against a real InterBase server
is a separate, later gate; nothing in this document should be read as having
been confirmed against a running engine.

## Enabled rules

### 1. SMALLINT / INTEGER / BIGINT exact range

- **Rule:** SMALLINT is -32768..32767, INTEGER is -2147483648..2147483647,
  BIGINT is -9223372036854775808..9223372036854775807 (16/32/64-bit signed
  two's-complement).
- **Dialect(s):** both (not dialect-sensitive).
- **Authoritative source:** standard two's-complement integer range, not
  InterBase-specific. These are the same bounds any conventional SQL engine
  uses for a 16/32/64-bit signed integer column; no InterBase-specific
  citation is needed or claimed.
- **Test fixture:** `TestParseDiagnosticTypeExactIntegers`,
  `TestAssignmentCompatibilityRequiredFixtures` (the brief's own
  `SMALLINT <- 32767/32768/-32768/-32769`,
  `INTEGER <- SMALLINT reference`, `SMALLINT <- INTEGER reference` cases).
- **Live-probe status:** NOT VERIFIED — no live database available this
  session.

### 2. NUMERIC(p, s) / DECIMAL(p, s) exact value range, derived from declared precision/scale alone

- **Rule:** a NUMERIC(p, s)/DECIMAL(p, s) declaration's exact value range is
  `-(10^p - 1)/10^s .. (10^p - 1)/10^s` — the range provable from the decimal
  arithmetic of a p-digit number with s fractional digits, independent of
  whatever fixed-width storage the engine actually chose for the field.
- **Dialect(s):** both (not dialect-sensitive). The identical rendered string
  form also covers the legacy Dialect-1 case where DOUBLE PRECISION carries a
  scaled NUMERIC/DECIMAL declaration (see rule 5 below); this task cannot and
  does not distinguish that case from a genuinely fixed-point field, so both
  are modeled identically as `familyExactNumeric`.
- **Authoritative source:**
  `internal/database/interbase_catalog.go:134-159` (`interBaseNumericType`)
  is the exact renderer this parser targets: `fmt.Sprintf("%s(%d, %d)",
  numericName, precision, scale)` at line 158. The explicit warning this
  task was given — **do not silently infer NUMERIC storage limits from
  decimal precision alone** — is honored by deriving the range from
  precision/scale arithmetic directly rather than from `naturalPrecision`
  (lines 147-150), which `interBaseNumericType`'s own comment documents as a
  *display* default only, never a storage-width signal
  (`internal/database/interbase_catalog.go:134-150`).
- **Test fixture:** `TestParseDiagnosticTypeNumericRoundTrip`.
- **Live-probe status:** NOT VERIFIED — no live database available this
  session.

### 3. CHAR(n) / VARCHAR(n) / CSTRING(n) declared character width

- **Rule:** the parenthesized integer is the type's declared maximum
  character count (not a byte length).
- **Dialect(s):** both.
- **Authoritative source:** `internal/database/interbase_catalog.go:89`,
  `:109`, `:111` (`fmt.Sprintf("CHAR(%d)", ...)`, `"VARCHAR(%d)"`,
  `"CSTRING(%d)"`, via `interBaseCharacterLength`, lines 121-132).
- **Test fixture:** `TestParseDiagnosticTypeCharacterWidth`,
  `TestAssignmentCompatibilityCharacterWidth`.
- **Live-probe status:** NOT VERIFIED — no live database available this
  session.

### 4. CHARACTER SET / COLLATE suffix stripping

- **Rule:** a rendered type string may carry a trailing ` CHARACTER SET
  <name>` and/or ` COLLATE <name>` clause (from `Domain.SQLType()`); these
  are parsed and discarded before base-type matching, never folded into
  `CharacterWidth`.
- **Dialect(s):** both.
- **Authoritative source:** `internal/database/interbase_catalog.go:46-55`
  (`interBaseColumnTypeName`), whose exact stripping approach
  (`strings.Index(rendered, " CHARACTER SET ")` / `" COLLATE "`, cutting at
  whichever appears first) is reused verbatim by
  `diagnostic_types.go`'s `stripTypeSuffixes`.
- **Test fixture:**
  `TestParseDiagnosticTypeCharacterSetCollateSuffixStripped`.
- **Live-probe status:** NOT VERIFIED — no live database available this
  session.

### 5. Dialect 1 DATE vs. Dialect 3 DATE/TIMESTAMP

- **Rule:** RDB$FIELD_TYPE 35 renders as `"DATE"` under SQL Dialect 1 and
  `"TIMESTAMP"` under Dialect 3; Dialect 1 has no separate TIMESTAMP keyword
  at all. This parser therefore treats every Dialect 1 `"DATE"` string as
  `familyDateTime` (date+time), and every Dialect 3 `"DATE"` string as
  `familyDateOnly` (date, no time) — the two are **not** the same semantic
  type, and an assignment between an sqlType parsed under Dialect 1 and one
  parsed under Dialect 3 for the literal text "DATE" is a cross-family
  conversion (`outcomeUnknown`), never treated as identical.
- **Known residual ambiguity (documented limitation, not silently omitted):**
  a bare Dialect 1 `"DATE"` string is genuinely ambiguous between
  RDB$FIELD_TYPE 12 (a real date-only field, which *also* always renders as
  `"DATE"` — `internal/database/interbase_catalog.go:85`, `:107`) and
  RDB$FIELD_TYPE 35 (the timestamp-under-Dialect-1 case,
  `internal/database/interbase_catalog.go:33-34`). The rendered string alone
  cannot distinguish which RDB$FIELD_TYPE produced it. This parser resolves
  the ambiguity toward the wider interpretation (`familyDateTime`), which is
  safe-direction only: it can cause a genuinely date-only Dialect 1 column to
  be treated as if it also carried a time component (suppressing a
  would-be-found narrowing case), but it can never fabricate a false
  "safe"/"invalid" verdict about a value that does not fit. This is
  documented, not silently omitted, per the task's completeness discipline.
- **Dialect(s):** Dialect 1 and Dialect 3 (this is the rule's entire reason
  for existing).
- **Authoritative source:** `internal/database/interbase_catalog.go:20-21`,
  `:29-39` (`interBaseTypeName`'s dialect branch and its own doc comment),
  independently confirmed by `dialect/variant_test.go`, which asserts
  `TIMESTAMP`/`TIME` keywords do not exist in the Dialect 1 keyword list.
- **Test fixture:** `TestParseDiagnosticTypeDialectSensitiveDate`,
  `TestAssignmentCompatibilityDialectSensitiveDateTime`, and the brief's own
  `"unverified dialect-specific conversion: unknown"` fixture in
  `TestAssignmentCompatibilityRequiredFixtures`.
- **Live-probe status:** NOT VERIFIED — no live database available this
  session.

### 6. Identical-family same-type safety (DATE-into-DATE, TIME-into-TIME, BOOLEAN-into-BOOLEAN)

- **Rule:** a source and destination that parse to the same verified,
  non-numeric, non-character family (`familyDateOnly`, `familyDateTime`,
  `familyTime`, `familyBoolean`) are safe to assign — no loss is possible
  between two declarations of the identical type.
- **Dialect(s):** both.
- **Authoritative source:** trivial identity (same family implies same
  value domain); no InterBase-specific citation needed beyond the
  family-assignment rules already cited above that determine when two
  declarations do or do not share a family.
- **Test fixture:** `TestAssignmentCompatibilityDialectSensitiveDateTime`
  (the `<dialect> X <- <dialect> X: safe` cases).
- **Live-probe status:** NOT VERIFIED — no live database available this
  session.

### 7. Domain resolution (single-level and domain-of-domain, cycle-safe)

- **Rule:** a type declaration that is not a recognized base-type string is
  tried as a user domain name via `SemanticCatalog.DomainInfo`, but only
  when that lookup reports `Present`. A domain whose own `Type` names
  another domain is followed for up to `maxDomainResolutionDepth` (8)
  levels; a domain that resolves back to a name already being resolved
  (a cycle, direct or indirect) is refused and reported unknown rather than
  looping.
- **Dialect(s):** both (domain resolution itself is not dialect-sensitive;
  the dialect only affects how the domain's *eventual* base-type string is
  interpreted, per rule 5).
- **Authoritative source:** `internal/sqlsymbol/diagnostic_catalog.go`'s
  `DomainFact`/`SemanticCatalog.DomainInfo` contract (Task 1), which this
  task consumes as given. No claim is made about how many levels of
  domain-on-domain nesting a real InterBase schema can actually construct;
  the depth cap is an engineering safety bound, not an InterBase-specific
  fact.
- **Test fixture:** `TestParseDiagnosticTypeDomainResolution`,
  `TestParseDiagnosticTypeDomainOfDomain`,
  `TestParseDiagnosticTypeUnknownDomain`,
  `TestParseDiagnosticTypeDomainCycle`.
- **Live-probe status:** NOT VERIFIED — no live database available this
  session.

### 8. Unrenderable / unrecognized text stays unknown

- **Rule:** `TYPE(n)` (the catalog's own fallback for an RDB$FIELD_TYPE code
  it cannot render — `internal/database/interbase_catalog.go:117`), an array
  declaration, or any other text not matching a recognized grammar parses to
  `ok=false`, never a guessed family.
- **Dialect(s):** both.
- **Authoritative source:**
  `internal/database/interbase_catalog.go:116-118`.
- **Test fixture:** `TestParseDiagnosticTypeUnrenderableFallback`.
- **Live-probe status:** NOT VERIFIED — no live database available this
  session.

## Rules left disabled/unknown (inconclusive or out of scope)

- **FLOAT / DOUBLE PRECISION exact range or float-to-exact-numeric
  conversion.** No exact range is asserted for `familyApproximate`
  (`sqlType.Min`/`Max` are always `nil`), and any conversion into or out of
  `familyApproximate` — including into an exact numeric or another
  approximate declaration — reports `outcomeUnknown` unconditionally
  (`assignmentCompatibility`'s `default` switch branch). This is
  deliberate: proving a specific FLOAT/DOUBLE PRECISION runtime value fits
  an exact numeric range requires knowing that specific value, which is
  usually unavailable for a non-literal expression, and this task found no
  narrow, citable InterBase rule that would let it assert more than that
  without guessing. **UNVERIFIED — treated as unknown, not enabled.**
- **TIMESTAMP-into-DATE and DATE-into-TIMESTAMP conversions (Dialect 3).**
  Even though both are date/time-related families with a plausible
  "truncate the time part" / "assume midnight" real-engine behavior, this
  task found no citation in this codebase's existing rendering logic (or
  elsewhere available this session) proving which of safe / possible-loss /
  definitely-invalid actually applies, so the cross-family check in
  `assignmentCompatibility` reports `outcomeUnknown` for both directions
  rather than asserting a guessed direction. **UNVERIFIED — treated as
  unknown, not enabled.** See
  `TestAssignmentCompatibilityDialectSensitiveDateTime`.
- **String-to-numeric / numeric-to-string implicit conversion.** InterBase
  permits some implicit string/numeric coercions at the engine level, but
  this task found no authoritative, narrow rule in the available grounding
  material precise enough to assert a range/safety verdict without
  guessing. `assignmentCompatibility` never special-cases a
  `familyCharacter`-to-numeric-family (or the reverse) pair; it falls
  through the ordinary cross-family check and reports `outcomeUnknown`.
  **UNVERIFIED — treated as unknown, not enabled.**
- **BLOB / BLOB_ID / QUAD conversions of any kind**, including
  BLOB-into-BLOB. No range, width, or subtype-compatibility concept is
  modeled for `familyBlob`; every conversion involving it reports
  `outcomeUnknown` via the same `default` branch as `familyApproximate`.
  **UNVERIFIED — treated as unknown, not enabled.**
- **Array declarations.** `parseDiagnosticType` has no grammar for an array
  type suffix (`... ARRAY [n:m]` or similar); such a declaration simply
  fails to match any recognized base-type form and, if it is also not a
  domain name, returns `ok=false`. No array-specific parsing was
  implemented; this is an explicit scope limitation, not a silent gap.
  **UNVERIFIED — treated as unknown, not enabled.**
- **NUMERIC(p, s) storage-width-based range (e.g., assuming NUMERIC(4,0) is
  bounded by SMALLINT's ±32767 because 4 is SMALLINT's natural display
  precision).** Explicitly rejected per the brief's own instruction; see
  rule 2 above for why the precision/scale-derived range is used instead.
  **Not a rule this task enables in any form.**

## Dialect-sensitivity coverage note

The only rule with verified InterBase-specific dialect-sensitive grounding
found during this task is rule 5 (DATE/TIMESTAMP). No additional
dialect-sensitive numeric or string rule was found with a citation strong
enough to enable; in particular, this task did **not** invent a
dialect-specific NUMERIC/DECIMAL precision rule, a dialect-specific
CHAR/VARCHAR width rule, or a dialect-specific integer range rule — none of
those are dialect-sensitive in this codebase's own rendering logic, and none
are asserted to be so here.
