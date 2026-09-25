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

### 2. NUMERIC(p, s) / DECIMAL(p, s) exact value range: storage-width-backed, corrected in Fix round 1 (C1)

- **Original rule (WRONG, superseded by this entry):** the initial version of
  this ledger claimed a NUMERIC(p,s)/DECIMAL(p,s) declaration's exact value
  range was `-(10^p - 1)/10^s .. (10^p - 1)/10^s` — the range implied purely
  by the declared digit count. **This was live-verified to be factually
  wrong** by a reviewer with read-only access to `interbase_reference`:
  `CAST(10000 AS NUMERIC(4,0))` succeeds and returns `10000`, even though
  10000 has 5 digits and exceeds the ±9999 the declared-precision rule
  would allow.
- **Corrected rule:** InterBase selects a **fixed storage width** from the
  declared precision alone, and enforces THAT storage type's exact
  two's-complement range, not a range derived from the digit count:
  - precision 1-4 → SMALLINT-backed, engine range ±32767 (scaled by
    `10^-scale`)
  - precision 5-9 → INTEGER-backed, engine range ±2147483647 (scaled)
  - precision 10-18, Dialect 3 → BIGINT-backed, engine range
    ±9223372036854775807 (scaled)
  - precision 10-18, Dialect 1 → **DOUBLE PRECISION-backed (approximate,
    not exact at all)** — see rule 9 (I2) below.
  - Live-verified: `CAST(32768 AS NUMERIC(4,0))` → numeric overflow (exceeds
    SMALLINT's ±32767, not because it exceeds 4 declared digits);
    `CAST(327.67 AS NUMERIC(4,2))` → succeeds, returns `327.67` (327.67 *
    100 = 32767, exactly SMALLINT's max, proving SMALLINT storage backs a
    4-digit, scale-2 NUMERIC).
- **`sqlType` now carries two independent bound pairs** for
  `familyExactNumeric`, precisely to keep the (wrong) declared-digit-count
  range and the (correct, engine-enforced) storage-width range distinct
  rather than conflating them again:
  - `DeclaredMin`/`DeclaredMax`: the range implied purely by `p` decimal
    digits — informational only, never used by `assignmentCompatibility`'s
    safe/definitely-invalid judgment.
  - `StorageMin`/`StorageMax`: the actual engine-enforced range, computed by
    `numericStorageBackedRange`. `assignmentCompatibility` always uses these
    for the safe/definitely-invalid boundary.
- **Dialect(s):** the storage-width **selection** is dialect-sensitive for
  precision 10-18 only (see rule 9 / I2); the SMALLINT/INTEGER buckets
  (precision 1-9) are identical across Dialect 1 and Dialect 3.
- **Authoritative source:** `interbase-go/schema/ddl.go`'s
  `dialect1NumericStorageCompatible` (lines 476-487) gives the exact
  precision→storage-type mapping this rule reuses, cross-checked against the
  reviewer's live `CAST(...)` probe results against `interbase_reference`
  quoted above. `internal/database/interbase_catalog.go:134-159`
  (`interBaseNumericType`) remains the exact string-rendering form this
  parser targets (`fmt.Sprintf("%s(%d, %d)", numericName, precision,
  scale)`), unchanged by this correction — only the *range this codebase
  derives* from that rendered string changed, not the string grammar.
- **Test fixture:** `TestParseDiagnosticTypeNumericRoundTrip` (DeclaredMin/
  DeclaredMax round-trip), `TestParseDiagnosticTypeNumericStorageWidth`
  (StorageMin/StorageMax per precision bucket),
  `TestAssignmentCompatibilityNumericStorageWidthLiveVerified` (the exact
  live-verified `10000`/`32768`/`327.67` values from the reviewer's probes).
- **Live-probe status:** VERIFIED by the reviewer's own read-only
  `CAST(...) FROM RDB$DATABASE` probes against `interbase_reference` (cited
  above); this is one of the rules in this ledger with actual live
  confirmation, not merely a codebase-rendering citation. See rule 10 (N1)
  below for a follow-up correction to how a literal is compared against
  this range.

### 10. Literal rounding happens BEFORE the storage-range check, not after (added in Fix round 2, N1)

- **Rule:** InterBase rounds a literal half-away-from-zero to the
  destination's declared scale, THEN checks the ROUNDED value against the
  destination's storage-enforced range (rule 2/9) — not the other way
  around. A literal whose raw, unrounded value lies outside the storage
  range but whose rounded value lies inside it is accepted by the engine,
  not rejected.
- **Corrected bug (was wrong in Fix round 1):** Fix round 1's C1 correction
  compared the RAW literal against `StorageMin`/`StorageMax` before
  considering rounding at all, which falsely reported a value that rounds
  into range as `outcomeDefinitelyInvalid`.
- **Live-verified proof (against `interbase_reference`, Dialect 1):**
  `CAST(32767.4 AS SMALLINT)` succeeds, returns `32767` (rounds down into
  range); `CAST(-32768.4 AS SMALLINT)` succeeds, returns `-32768`;
  `CAST(2147483647.4 AS INTEGER)` succeeds; `CAST(327.674 AS NUMERIC(4,2))`
  succeeds, returns `327.67`; `CAST(-327.684 AS NUMERIC(4,2))` succeeds,
  returns `-327.68`. Contrast: `CAST(32767.5 AS SMALLINT)` overflows
  (rounds away from zero to `32768`, out of range); `CAST(327.675 AS
  NUMERIC(4,2))` overflows likewise — both remain `outcomeDefinitelyInvalid`
  after this fix, unchanged.
- **How this composes with I1 (Fix round 1)'s rounding-loss rule:** I1
  established that a literal requiring rounding to fit the destination's
  scale is `outcomePossibleLoss`, never `outcomeSafe`, once it is known to
  be in range. N1 does not introduce a third outcome or contradict this —
  it only moves WHERE the range check happens (after rounding, not before).
  The composed order in `assignmentCompatibility`'s literal-value path is
  now: (1) round the literal to `destination.Scale` half-away-from-zero
  using exact `big.Rat`/`big.Int` arithmetic (`roundToScale`,
  `roundHalfAwayFromZero` — never `float64`); (2) compare the ROUNDED value
  against `StorageMin`/`StorageMax` — outside is
  `outcomeDefinitelyInvalid`; (3) if in range and rounding changed the
  value, `outcomePossibleLoss` (this is exactly I1's existing rule, now fed
  the correct in-range determination); (4) if in range and rounding did not
  change the value, `outcomeSafe`. No duplicate or conflicting
  fractional-digit check remains — the single `roundToScale` call now
  serves both what was previously two separate steps (the old
  `hasExtraFractionalDigits` helper was removed and folded into this one
  rounding call).
- **Dialect(s):** both (rounding behavior itself is not dialect-sensitive;
  it interacts with the already-dialect-sensitive storage range from rules
  2 and 9).
- **Authoritative source:** the reviewer's live `CAST(...)` probes against
  `interbase_reference`, cited above.
- **Test fixture:**
  `TestAssignmentCompatibilityRoundBeforeStorageRangeCheck` (all five
  live-verified round-into-range cases, plus the two round-out-of-range
  regression cases that must remain `outcomeDefinitelyInvalid`).
- **Live-probe status:** VERIFIED by the reviewer's own live `CAST(...)`
  probes against `interbase_reference`, cited above.

### 3. CHAR(n) / VARCHAR(n) / CSTRING(n) declared width — honest limitation corrected in Fix round 1 (I3)

- **Rule:** the parenthesized integer is the number this codebase's own
  catalog renderer wrote inside `CHAR(n)`/`VARCHAR(n)`/`CSTRING(n)`'s
  parentheses.
- **Corrected claim (was overclaimed in the original ledger):** this ledger
  previously stated the integer is definitely "a character count (not a
  byte length)." That is **not always true**: `interBaseCharacterLength`
  (`internal/database/interbase_catalog.go:121-132`) falls back to
  `domain.FieldLength` — a **byte** count from `RDB$FIELD_LENGTH` — whenever
  `domain.CharacterLength` (`RDB$CHARACTER_LENGTH`) is `NULL`, and that byte
  count is written into the identical `CHAR(n)`/`VARCHAR(n)` rendered
  position with no marker distinguishing it from a genuine character count.
  This parser cannot tell the two cases apart from the rendered string
  alone, so `sqlType.CharacterWidth` may overstate (for a multi-byte
  charset) or otherwise misrepresent a column's actual character capacity.
- **How `assignmentCompatibility` hedges around this:** a
  `familyCharacter`-into-`familyCharacter` assignment where the source's
  width fits the destination's width is still reported `outcomeSafe` (a
  narrower-or-equal count is safe regardless of which count it actually is).
  A source that appears *wider* than the destination is reported
  `outcomeUnknown`, not `outcomePossibleLoss` — asserting a narrowing
  finding here risks a false positive whenever either side's width is
  actually a byte count rather than a character count for a multi-byte
  charset column.
- **Dialect(s):** both.
- **Authoritative source:** `internal/database/interbase_catalog.go:89`,
  `:109`, `:111` (`fmt.Sprintf("CHAR(%d)", ...)`, `"VARCHAR(%d)"`,
  `"CSTRING(%d)"`, via `interBaseCharacterLength`, lines 121-132 — including
  its `FieldLength` byte-count fallback at those same lines).
- **Test fixture:** `TestParseDiagnosticTypeCharacterWidth`,
  `TestAssignmentCompatibilityCharacterWidth` (now also pins the
  outcomeUnknown hedge for a narrower destination).
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
  the ambiguity toward the wider interpretation (`familyDateTime`).
- **Corrected scope claim (Fix round 1, I4):** the original ledger and
  `sqlType`'s own doc comment stated this choice "can never fabricate a
  false safe verdict," which overstated what is actually guaranteed. The
  accurate, narrower claim is: field type 35 uniformly means date+time under
  Dialect 1, so a same-dialect Dialect-1-DATE-into-Dialect-1-DATE assignment
  is genuinely type-compatible on that basis, and this choice can only
  *suppress* a would-be narrowing finding within the narrowing checks this
  task enables (by treating a genuinely date-only column as if it also had a
  time component) — it never *asserts* a false narrowing finding. This is
  not a blanket guarantee that no rule in this file can ever produce a false
  "safe" outcome for any other reason; it describes only the specific,
  narrow effect of this one dialect-ambiguity choice.
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

### 9. Dialect 1 NUMERIC/DECIMAL precision 10-18 is DOUBLE PRECISION-backed (approximate), not exact (added in Fix round 1, I2)

- **Rule:** InterBase's precision→storage-type selection for NUMERIC/DECIMAL
  is itself dialect-sensitive for precision 10-18: Dialect 3 backs it with an
  exact BIGINT (see rule 2), but Dialect 1 backs it with DOUBLE PRECISION —
  an *approximate* binary floating-point type with no exact range at all.
  `parseDiagnosticType` is therefore dialect-aware for NUMERIC/DECIMAL
  parsing, not only for DATE/TIMESTAMP (rule 5): under a Dialect 1 variant
  with precision 10-18, it produces `familyApproximate` (no fabricated exact
  bounds) instead of `familyExactNumeric`.
- **Dialect(s):** Dialect 1 and Dialect 3 diverge only for precision 10-18;
  precision 1-9 selects the identical SMALLINT/INTEGER storage in both
  dialects.
- **Authoritative source:** `interbase-go/schema/ddl.go`'s
  `dialect1NumericStorageCompatible` (lines 476-487): `fieldTypeDouble`
  (DOUBLE PRECISION) is the only storage type the function accepts for
  precision 10-18 under Dialect 1, while `fieldTypeBigint`-backed precision
  10-18 is what Dialect 3 accepts elsewhere in the same file (the ordinary,
  non-dialect-1-specific precision switch in `sqlTypePartsWithRenderer`).
- **Test fixture:**
  `TestParseDiagnosticTypeNumericStorageWidthDialectSensitive` (the required
  Dialect 1/3 table-driven coverage — precision 4 and 9 identical across
  dialects, precision 12 and 18 diverge to `familyApproximate` under
  Dialect 1 only).
- **Live-probe status:** NOT VERIFIED — no live database available this
  session (grounded in `interbase-go/schema/ddl.go`'s source, not a live
  probe, unlike rule 2's C1 correction).

## Rules left disabled/unknown (inconclusive or out of scope)

- **FLOAT / DOUBLE PRECISION exact range or float-to-exact-numeric
  conversion.** No exact range is asserted for `familyApproximate`
  (`sqlType.StorageMin`/`StorageMax`/`DeclaredMin`/`DeclaredMax` are always
  `nil`, including for the Dialect 1 precision-10-18 NUMERIC case from rule
  9), and any conversion into or out of `familyApproximate` — including into
  an exact numeric or another approximate declaration — reports
  `outcomeUnknown` unconditionally (`assignmentCompatibility`'s `default`
  switch branch). This is deliberate: proving a specific FLOAT/DOUBLE
  PRECISION runtime value fits an exact numeric range requires knowing that
  specific value, which is usually unavailable for a non-literal expression,
  and this task found no narrow, citable InterBase rule that would let it
  assert more than that without guessing. **UNVERIFIED — treated as unknown,
  not enabled.**
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
- **NUMERIC(p, s) storage-width-based range — SUPERSEDED, this entry was
  itself wrong.** The original version of this ledger listed the
  storage-width-based range as a rule this task explicitly rejected,
  reasoning that "4 is SMALLINT's natural display precision" was an
  unverified assumption. Fix round 1's C1 correction (rule 2 above)
  live-verifies that the storage-width-based range is in fact exactly what
  InterBase enforces, and it is now the rule `assignmentCompatibility` uses
  for its safe/definitely-invalid boundary. This entry is retained only so a
  reader of this file's history understands why the wording changed: do not
  read rule 2 as reintroducing something this task once said was unsafe to
  assume — the live probes in Fix round 1 are what changed, not the
  reasoning standard.

## Dialect-sensitivity coverage note

Two rules have verified InterBase-specific dialect-sensitive grounding:
rule 5 (DATE/TIMESTAMP) and rule 9 (NUMERIC/DECIMAL precision 10-18 storage
selection, added in Fix round 1 / I2). No additional dialect-sensitive
numeric or string rule was found with a citation strong enough to enable;
in particular, this task did **not** invent a dialect-specific CHAR/VARCHAR
width rule or a dialect-specific plain-integer (SMALLINT/INTEGER/BIGINT)
range rule — neither is dialect-sensitive in this codebase's own rendering
logic, and neither is asserted to be so here. The NUMERIC/DECIMAL
storage-width selection (rule 2) itself IS dialect-sensitive for precision
10-18 specifically (rule 9), even though the exact-numeric range formula
that applies once a storage type is selected is not.
