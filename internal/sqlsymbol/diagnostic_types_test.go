package sqlsymbol

import (
	"math/big"
	"testing"

	"github.com/sqls-server/sqls/dialect"
)

func dialect1Variant() dialect.DriverVariant {
	return dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase1}
}

func dialect3Variant() dialect.DriverVariant {
	return dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}
}

func mustParseDiagnosticType(t *testing.T, declaration string, dv dialect.DriverVariant, c Catalog) sqlType {
	t.Helper()
	got, ok := parseDiagnosticType(declaration, dv, c)
	if !ok {
		t.Fatalf("parseDiagnosticType(%q) = not ok, want ok", declaration)
	}
	return got
}

func TestParseDiagnosticTypeExactIntegers(t *testing.T) {
	tests := []struct {
		declaration string
		min, max    int64
	}{
		{"SMALLINT", -32768, 32767},
		{"INTEGER", -2147483648, 2147483647},
		{"BIGINT", -9223372036854775808, 9223372036854775807},
	}
	for _, tt := range tests {
		t.Run(tt.declaration, func(t *testing.T) {
			got := mustParseDiagnosticType(t, tt.declaration, interBaseVariant(), nil)
			if got.Family != familyExactInteger {
				t.Fatalf("Family = %v, want familyExactInteger", got.Family)
			}
			if got.StorageMin == nil || got.StorageMin.Cmp(big.NewRat(tt.min, 1)) != 0 {
				t.Fatalf("StorageMin = %v, want %d", got.StorageMin, tt.min)
			}
			if got.StorageMax == nil || got.StorageMax.Cmp(big.NewRat(tt.max, 1)) != 0 {
				t.Fatalf("StorageMax = %v, want %d", got.StorageMax, tt.max)
			}
			if got.DeclaredMin != nil || got.DeclaredMax != nil {
				t.Fatalf("DeclaredMin/DeclaredMax = %v/%v, want nil/nil for familyExactInteger", got.DeclaredMin, got.DeclaredMax)
			}
		})
	}
}

// TestParseDiagnosticTypeNumericRoundTrip checks NUMERIC/DECIMAL comma-space
// tolerance and precision/scale round-tripping, and that DeclaredMin/
// DeclaredMax hold the pure digit-count-implied range (never the
// engine-enforced range -- see TestParseDiagnosticTypeNumericStorageWidth for
// StorageMin/StorageMax, the C1-corrected bounds).
func TestParseDiagnosticTypeNumericRoundTrip(t *testing.T) {
	tests := []struct {
		declaration      string
		precision, scale int
		minNum, minDen   int64
		maxNum, maxDen   int64
	}{
		{"NUMERIC(4, 0)", 4, 0, -9999, 1, 9999, 1},
		{"DECIMAL(9, 2)", 9, 2, -999999999, 100, 999999999, 100},
		// Tolerant of comma-space variation: the renderer's exact form is
		// ", " but the parser trims whitespace around the comma rather than
		// requiring an exact match.
		{"NUMERIC(3,1)", 3, 1, -999, 10, 999, 10},
	}
	for _, tt := range tests {
		t.Run(tt.declaration, func(t *testing.T) {
			got := mustParseDiagnosticType(t, tt.declaration, interBaseVariant(), nil)
			if got.Family != familyExactNumeric {
				t.Fatalf("Family = %v, want familyExactNumeric", got.Family)
			}
			if got.Precision != tt.precision || got.Scale != tt.scale {
				t.Fatalf("Precision/Scale = %d/%d, want %d/%d", got.Precision, got.Scale, tt.precision, tt.scale)
			}
			wantMin := big.NewRat(tt.minNum, tt.minDen)
			wantMax := big.NewRat(tt.maxNum, tt.maxDen)
			if got.DeclaredMin == nil || got.DeclaredMin.Cmp(wantMin) != 0 {
				t.Fatalf("DeclaredMin = %v, want %v", got.DeclaredMin, wantMin)
			}
			if got.DeclaredMax == nil || got.DeclaredMax.Cmp(wantMax) != 0 {
				t.Fatalf("DeclaredMax = %v, want %v", got.DeclaredMax, wantMax)
			}
		})
	}
}

// TestParseDiagnosticTypeNumericStorageWidth is the C1 regression: InterBase
// selects a fixed storage width from precision alone and enforces THAT
// range, not the declared digit count. Live-verified against
// interbase_reference: CAST(10000 AS NUMERIC(4,0)) succeeds (SMALLINT
// storage, ±32767, not ±9999); CAST(327.67 AS NUMERIC(4,2)) succeeds
// (SMALLINT's max 32767 scaled by 10^-2); precision 9 selects INTEGER;
// precision 10-18 selects BIGINT under Dialect 3 (see the companion Dialect
// 1 test below for how 10-18 differs there -- I2).
func TestParseDiagnosticTypeNumericStorageWidth(t *testing.T) {
	tests := []struct {
		declaration   string
		storageMinNum int64
		storageMinDen int64
		storageMaxNum int64
		storageMaxDen int64
	}{
		{"NUMERIC(4, 0)", -32768, 1, 32767, 1},
		{"NUMERIC(4, 2)", -32768, 100, 32767, 100},
		{"NUMERIC(9, 0)", -2147483648, 1, 2147483647, 1},
		{"NUMERIC(18, 0)", -9223372036854775808, 1, 9223372036854775807, 1},
	}
	for _, tt := range tests {
		t.Run(tt.declaration, func(t *testing.T) {
			got := mustParseDiagnosticType(t, tt.declaration, dialect3Variant(), nil)
			if got.Family != familyExactNumeric {
				t.Fatalf("Family = %v, want familyExactNumeric", got.Family)
			}
			wantMin := big.NewRat(tt.storageMinNum, tt.storageMinDen)
			wantMax := big.NewRat(tt.storageMaxNum, tt.storageMaxDen)
			if got.StorageMin == nil || got.StorageMin.Cmp(wantMin) != 0 {
				t.Fatalf("StorageMin = %v, want %v", got.StorageMin, wantMin)
			}
			if got.StorageMax == nil || got.StorageMax.Cmp(wantMax) != 0 {
				t.Fatalf("StorageMax = %v, want %v", got.StorageMax, wantMax)
			}
		})
	}
}

// TestParseDiagnosticTypeNumericStorageWidthDialectSensitive is the required
// Dialect 1/3 table-driven coverage for I2: precision 1-9 selects the same
// SMALLINT/INTEGER storage in both dialects, but precision 10-18 diverges --
// Dialect 3 backs it with an exact BIGINT, Dialect 1 backs it with DOUBLE
// PRECISION (approximate), per interbase-go/schema/ddl.go's
// dialect1NumericStorageCompatible.
func TestParseDiagnosticTypeNumericStorageWidthDialectSensitive(t *testing.T) {
	tests := []struct {
		name        string
		declaration string
		dv          dialect.DriverVariant
		wantFamily  sqlTypeFamily
	}{
		{"dialect 1 precision 4 is exact (SMALLINT-backed)", "NUMERIC(4, 0)", dialect1Variant(), familyExactNumeric},
		{"dialect 3 precision 4 is exact (SMALLINT-backed)", "NUMERIC(4, 0)", dialect3Variant(), familyExactNumeric},
		{"dialect 1 precision 9 is exact (INTEGER-backed)", "NUMERIC(9, 0)", dialect1Variant(), familyExactNumeric},
		{"dialect 3 precision 9 is exact (INTEGER-backed)", "NUMERIC(9, 0)", dialect3Variant(), familyExactNumeric},
		{"dialect 1 precision 12 is approximate (DOUBLE PRECISION-backed)", "NUMERIC(12, 2)", dialect1Variant(), familyApproximate},
		{"dialect 3 precision 12 is exact (BIGINT-backed)", "NUMERIC(12, 2)", dialect3Variant(), familyExactNumeric},
		{"dialect 1 precision 18 is approximate (DOUBLE PRECISION-backed)", "NUMERIC(18, 0)", dialect1Variant(), familyApproximate},
		{"dialect 3 precision 18 is exact (BIGINT-backed)", "NUMERIC(18, 0)", dialect3Variant(), familyExactNumeric},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mustParseDiagnosticType(t, tt.declaration, tt.dv, nil)
			if got.Family != tt.wantFamily {
				t.Fatalf("Family = %v, want %v", got.Family, tt.wantFamily)
			}
			if tt.wantFamily == familyApproximate {
				if got.StorageMin != nil || got.StorageMax != nil {
					t.Fatalf("StorageMin/StorageMax = %v/%v, want nil/nil for an approximate-backed declaration (no fabricated exact bounds)", got.StorageMin, got.StorageMax)
				}
			}
		})
	}
}

func TestParseDiagnosticTypeCharacterWidth(t *testing.T) {
	tests := []struct {
		declaration string
		width       int
	}{
		{"CHAR(10)", 10},
		{"VARCHAR(255)", 255},
		{"CSTRING(31)", 31},
	}
	for _, tt := range tests {
		t.Run(tt.declaration, func(t *testing.T) {
			got := mustParseDiagnosticType(t, tt.declaration, interBaseVariant(), nil)
			if got.Family != familyCharacter {
				t.Fatalf("Family = %v, want familyCharacter", got.Family)
			}
			if got.CharacterWidth != tt.width {
				t.Fatalf("CharacterWidth = %d, want %d", got.CharacterWidth, tt.width)
			}
		})
	}
}

// TestParseDiagnosticTypeCharacterSetCollateSuffixStripped reuses
// interBaseColumnTypeName's own stripping approach: cut at the first
// " CHARACTER SET " or " COLLATE ", whichever appears.
func TestParseDiagnosticTypeCharacterSetCollateSuffixStripped(t *testing.T) {
	tests := []struct {
		declaration string
		width       int
	}{
		{"VARCHAR(10) CHARACTER SET UTF8", 10},
		{"CHAR(5) CHARACTER SET UTF8 COLLATE UNICODE", 5},
		{"VARCHAR(20) COLLATE UNICODE", 20},
	}
	for _, tt := range tests {
		t.Run(tt.declaration, func(t *testing.T) {
			got := mustParseDiagnosticType(t, tt.declaration, interBaseVariant(), nil)
			if got.Family != familyCharacter || got.CharacterWidth != tt.width {
				t.Fatalf("got Family=%v CharacterWidth=%d, want familyCharacter/%d", got.Family, got.CharacterWidth, tt.width)
			}
		})
	}
}

func TestParseDiagnosticTypeApproximateAndMisc(t *testing.T) {
	tests := []struct {
		declaration string
		family      sqlTypeFamily
	}{
		{"FLOAT", familyApproximate},
		{"DOUBLE PRECISION", familyApproximate},
		{"BOOLEAN", familyBoolean},
		{"BLOB", familyBlob},
		{"BLOB_ID", familyBlob},
		{"QUAD", familyBlob},
		{"TIME", familyTime},
	}
	for _, tt := range tests {
		t.Run(tt.declaration, func(t *testing.T) {
			got := mustParseDiagnosticType(t, tt.declaration, interBaseVariant(), nil)
			if got.Family != tt.family {
				t.Fatalf("Family = %v, want %v", got.Family, tt.family)
			}
		})
	}
}

// TestParseDiagnosticTypeDialectSensitiveDate exercises the one verified
// dialect-sensitive rule: RDB$FIELD_TYPE 35 renders as "DATE" under Dialect 1
// (holding date+time) and "TIMESTAMP" under Dialect 3, while Dialect 1's own
// "DATE" string never denotes a date-only value in this model.
func TestParseDiagnosticTypeDialectSensitiveDate(t *testing.T) {
	tests := []struct {
		name        string
		declaration string
		dv          dialect.DriverVariant
		want        sqlTypeFamily
	}{
		{"dialect 1 DATE is date+time", "DATE", dialect1Variant(), familyDateTime},
		{"dialect 3 DATE is date-only", "DATE", dialect3Variant(), familyDateOnly},
		{"dialect 3 TIMESTAMP is date+time", "TIMESTAMP", dialect3Variant(), familyDateTime},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mustParseDiagnosticType(t, tt.declaration, tt.dv, nil)
			if got.Family != tt.want {
				t.Fatalf("Family = %v, want %v", got.Family, tt.want)
			}
		})
	}
}

// TestParseDiagnosticTypeUnrenderableFallback verifies TYPE(n) -- the
// catalog's own unrenderable fallback for an RDB$FIELD_TYPE code it does not
// know how to spell -- parses as unknown rather than a guessed family or a
// panic.
func TestParseDiagnosticTypeUnrenderableFallback(t *testing.T) {
	if _, ok := parseDiagnosticType("TYPE(23)", interBaseVariant(), nil); ok {
		t.Fatal("TYPE(23) should not parse to any known family")
	}
}

func TestParseDiagnosticTypeDomainResolution(t *testing.T) {
	c := &diagnosticFixtureCatalog{
		domainsKnown: true,
		domains: map[string]DomainFact{
			"D_AGE": {Type: "SMALLINT"},
		},
	}
	got := mustParseDiagnosticType(t, "D_AGE", interBaseVariant(), c)
	if got.Family != familyExactInteger {
		t.Fatalf("Family = %v, want familyExactInteger (resolved through domain D_AGE)", got.Family)
	}
	if got.StorageMax == nil || got.StorageMax.Cmp(big.NewRat(32767, 1)) != 0 {
		t.Fatalf("StorageMax = %v, want 32767", got.StorageMax)
	}
}

// TestParseDiagnosticTypeDomainOfDomain resolves one level of domain-on-domain.
func TestParseDiagnosticTypeDomainOfDomain(t *testing.T) {
	c := &diagnosticFixtureCatalog{
		domainsKnown: true,
		domains: map[string]DomainFact{
			"D_OUTER": {Type: "D_INNER"},
			"D_INNER": {Type: "INTEGER"},
		},
	}
	got := mustParseDiagnosticType(t, "D_OUTER", interBaseVariant(), c)
	if got.Family != familyExactInteger {
		t.Fatalf("Family = %v, want familyExactInteger (resolved through D_OUTER -> D_INNER -> INTEGER)", got.Family)
	}
	if got.StorageMax == nil || got.StorageMax.Cmp(big.NewRat(2147483647, 1)) != 0 {
		t.Fatalf("StorageMax = %v, want 2147483647", got.StorageMax)
	}
}

func TestParseDiagnosticTypeUnknownDomain(t *testing.T) {
	tests := []struct {
		name string
		c    Catalog
	}{
		{"domain namespace incomplete", &diagnosticFixtureCatalog{domainsKnown: false, domains: map[string]DomainFact{}}},
		{"domain namespace complete but name absent", &diagnosticFixtureCatalog{domainsKnown: true, domains: map[string]DomainFact{}}},
		{"catalog does not implement SemanticCatalog", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := parseDiagnosticType("UNKNOWN_DOMAIN", interBaseVariant(), tt.c); ok {
				t.Fatal("expected an unresolvable domain reference to fail to parse")
			}
		})
	}
}

// TestParseDiagnosticTypeDomainCycle proves a self-referencing domain chain
// terminates at unknown instead of recursing forever.
func TestParseDiagnosticTypeDomainCycle(t *testing.T) {
	c := &diagnosticFixtureCatalog{
		domainsKnown: true,
		domains: map[string]DomainFact{
			"D_A": {Type: "D_B"},
			"D_B": {Type: "D_A"},
		},
	}
	if _, ok := parseDiagnosticType("D_A", interBaseVariant(), c); ok {
		t.Fatal("a cyclic domain chain must not resolve to any type")
	}
}

// TestAssignmentCompatibilityRequiredFixtures is the exact fixture table
// from the task brief.
func TestAssignmentCompatibilityRequiredFixtures(t *testing.T) {
	dv := interBaseVariant()
	smallint := mustParseDiagnosticType(t, "SMALLINT", dv, nil)
	integer := mustParseDiagnosticType(t, "INTEGER", dv, nil)

	unknownDomainCatalog := &diagnosticFixtureCatalog{domainsKnown: false, domains: map[string]DomainFact{}}
	if _, ok := parseDiagnosticType("UNKNOWN_DOMAIN", dv, unknownDomainCatalog); ok {
		t.Fatal("UNKNOWN_DOMAIN must not parse")
	}
	var unknownDestination sqlType // zero value: Family == familyUnknown

	dialect1Date := mustParseDiagnosticType(t, "DATE", dialect1Variant(), nil)
	dialect3Date := mustParseDiagnosticType(t, "DATE", dialect3Variant(), nil)

	tests := []struct {
		name        string
		source      expressionFact
		destination sqlType
		want        compatibilityOutcome
	}{
		{"SMALLINT <- 32767: safe", expressionFact{Value: big.NewRat(32767, 1)}, smallint, outcomeSafe},
		{"SMALLINT <- 32768: definitely invalid", expressionFact{Value: big.NewRat(32768, 1)}, smallint, outcomeDefinitelyInvalid},
		{"SMALLINT <- -32768: safe", expressionFact{Value: big.NewRat(-32768, 1)}, smallint, outcomeSafe},
		{"SMALLINT <- -32769: definitely invalid", expressionFact{Value: big.NewRat(-32769, 1)}, smallint, outcomeDefinitelyInvalid},
		{"INTEGER <- SMALLINT reference: safe widening", expressionFact{Type: smallint}, integer, outcomeSafe},
		{"SMALLINT <- INTEGER reference: possible range loss", expressionFact{Type: integer}, smallint, outcomePossibleLoss},
		{"unknown domain <- any expression: unknown", expressionFact{Value: big.NewRat(1, 1)}, unknownDestination, outcomeUnknown},
		// Synthetic: source and destination are deliberately parsed under
		// different dialects to prove the parser's dialect sensitivity feeds
		// through to assignmentCompatibility as a genuine family mismatch,
		// not a realistic single-connection scenario (one connection has one
		// dialect).
		{"unverified dialect-specific conversion: unknown", expressionFact{Type: dialect1Date}, dialect3Date, outcomeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assignmentCompatibility(tt.source, tt.destination, dv)
			if got.Outcome != tt.want {
				t.Fatalf("Outcome = %v, want %v (reason: %q)", got.Outcome, tt.want, got.Reason)
			}
		})
	}
}

// TestAssignmentCompatibilityNumericStorageWidthLiveVerified is the C1
// regression, pinned to the reviewer's exact live-verified CAST probe
// results against interbase_reference.
func TestAssignmentCompatibilityNumericStorageWidthLiveVerified(t *testing.T) {
	dv := dialect3Variant()
	numeric40 := mustParseDiagnosticType(t, "NUMERIC(4, 0)", dv, nil)
	numeric42 := mustParseDiagnosticType(t, "NUMERIC(4, 2)", dv, nil)

	tests := []struct {
		name        string
		value       *big.Rat
		destination sqlType
		want        compatibilityOutcome
	}{
		// CAST(10000 AS NUMERIC(4,0)) succeeds live: SMALLINT storage
		// (±32767), not the ±9999 that 4 declared digits alone would imply.
		{"NUMERIC(4,0) <- 10000: safe (SMALLINT-backed, not ±9999)", big.NewRat(10000, 1), numeric40, outcomeSafe},
		// CAST(32768 AS NUMERIC(4,0)) overflows live: exceeds SMALLINT's
		// actual ±32767 storage range.
		{"NUMERIC(4,0) <- 32768: definitely invalid (exceeds SMALLINT storage)", big.NewRat(32768, 1), numeric40, outcomeDefinitelyInvalid},
		// CAST(327.67 AS NUMERIC(4,2)) succeeds live: 327.67 * 100 = 32767,
		// exactly SMALLINT's max.
		{"NUMERIC(4,2) <- 327.67: safe (SMALLINT max scaled by 10^-2)", big.NewRat(32767, 100), numeric42, outcomeSafe},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assignmentCompatibility(expressionFact{Value: tt.value}, tt.destination, dv)
			if got.Outcome != tt.want {
				t.Fatalf("Outcome = %v, want %v (reason: %q)", got.Outcome, tt.want, got.Reason)
			}
		})
	}
}

// TestAssignmentCompatibilityScaleLossLiteral is the I1 regression: a
// literal that would require rounding to fit the destination's declared
// scale is outcomePossibleLoss, never outcomeSafe, even when it is within
// the destination's storage range. Live-verified: CAST(1.5 AS INTEGER)
// returns 2 (rounds); CAST(1.235 AS NUMERIC(4,2)) returns 1.24 (rounds).
func TestAssignmentCompatibilityScaleLossLiteral(t *testing.T) {
	dv := dialect3Variant()
	integer := mustParseDiagnosticType(t, "INTEGER", dv, nil)
	numeric42 := mustParseDiagnosticType(t, "NUMERIC(4, 2)", dv, nil)

	tests := []struct {
		name        string
		value       *big.Rat
		destination sqlType
		want        compatibilityOutcome
	}{
		{"INTEGER <- 1.5: possible loss (rounds to 2)", big.NewRat(3, 2), integer, outcomePossibleLoss},
		{"INTEGER <- 2: safe (no fractional digits)", big.NewRat(2, 1), integer, outcomeSafe},
		{"NUMERIC(4,2) <- 1.234: possible loss (rounds to 1.23)", big.NewRat(1234, 1000), numeric42, outcomePossibleLoss},
		{"NUMERIC(4,2) <- 1.23: safe (matches scale exactly)", big.NewRat(123, 100), numeric42, outcomeSafe},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assignmentCompatibility(expressionFact{Value: tt.value}, tt.destination, dv)
			if got.Outcome != tt.want {
				t.Fatalf("Outcome = %v, want %v (reason: %q)", got.Outcome, tt.want, got.Reason)
			}
		})
	}
}

// TestAssignmentCompatibilityScaleLossReference is the I1 reference-source
// case: NUMERIC(18,2) <- NUMERIC(9,4) reference. The source's storage range
// fits entirely within the destination's, but the source's scale (4) exceeds
// the destination's declared scale (2), so a value with nonzero digits past
// the second decimal place would be rounded -- this must not be reported
// safe merely because the magnitude fits.
func TestAssignmentCompatibilityScaleLossReference(t *testing.T) {
	dv := dialect3Variant()
	numeric18_2 := mustParseDiagnosticType(t, "NUMERIC(18, 2)", dv, nil)
	numeric9_4 := mustParseDiagnosticType(t, "NUMERIC(9, 4)", dv, nil)

	got := assignmentCompatibility(expressionFact{Type: numeric9_4}, numeric18_2, dv)
	if got.Outcome != outcomePossibleLoss {
		t.Fatalf("NUMERIC(18,2) <- NUMERIC(9,4) reference: Outcome = %v, want outcomePossibleLoss (reason: %q)", got.Outcome, got.Reason)
	}
}

// TestAssignmentCompatibilityCharacterWidth also pins the I3 hedge: since
// CharacterWidth may be a byte-length fallback rather than a genuine
// character count (see sqlType's own doc), a narrower destination is
// reported outcomeUnknown, not outcomePossibleLoss -- this codebase cannot
// be reasonably confident a narrowing finding here is real.
func TestAssignmentCompatibilityCharacterWidth(t *testing.T) {
	dv := interBaseVariant()
	short := mustParseDiagnosticType(t, "VARCHAR(5)", dv, nil)
	long := mustParseDiagnosticType(t, "VARCHAR(20)", dv, nil)

	if got := assignmentCompatibility(expressionFact{Type: short}, long, dv); got.Outcome != outcomeSafe {
		t.Fatalf("short into long: Outcome = %v, want outcomeSafe (reason: %q)", got.Outcome, got.Reason)
	}
	if got := assignmentCompatibility(expressionFact{Type: long}, short, dv); got.Outcome != outcomeUnknown {
		t.Fatalf("long into short: Outcome = %v, want outcomeUnknown (I3: width may be a byte count, not flagged as possible loss) (reason: %q)", got.Outcome, got.Reason)
	}
}

// TestAssignmentCompatibilityDialectSensitiveDateTime is the table-driven
// Dialect 1 vs Dialect 3 coverage for the one verified dialect-sensitive
// rule this task enables (DATE/TIMESTAMP). A same-dialect DATE-into-DATE
// assignment is safe in both dialects; a DATE-into-TIMESTAMP or
// TIMESTAMP-into-DATE conversion has no verified rule in this task and stays
// unknown (see doc/interbase-diagnostic-semantics.md).
func TestAssignmentCompatibilityDialectSensitiveDateTime(t *testing.T) {
	dv := interBaseVariant()
	dialect1Date := mustParseDiagnosticType(t, "DATE", dialect1Variant(), nil)
	dialect3Date := mustParseDiagnosticType(t, "DATE", dialect3Variant(), nil)
	dialect3Timestamp := mustParseDiagnosticType(t, "TIMESTAMP", dialect3Variant(), nil)

	tests := []struct {
		name        string
		source      sqlType
		destination sqlType
		want        compatibilityOutcome
	}{
		{"dialect 1 DATE <- dialect 1 DATE: safe", dialect1Date, dialect1Date, outcomeSafe},
		{"dialect 3 DATE <- dialect 3 DATE: safe", dialect3Date, dialect3Date, outcomeSafe},
		{"dialect 3 TIMESTAMP <- dialect 3 DATE: unverified, unknown", dialect3Date, dialect3Timestamp, outcomeUnknown},
		{"dialect 3 DATE <- dialect 3 TIMESTAMP: unverified, unknown", dialect3Timestamp, dialect3Date, outcomeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assignmentCompatibility(expressionFact{Type: tt.source}, tt.destination, dv)
			if got.Outcome != tt.want {
				t.Fatalf("Outcome = %v, want %v (reason: %q)", got.Outcome, tt.want, got.Reason)
			}
		})
	}
}

func TestAssignmentCompatibilityUnknownWhenDestinationUnknown(t *testing.T) {
	got := assignmentCompatibility(expressionFact{Value: big.NewRat(1, 1)}, sqlType{}, interBaseVariant())
	if got.Outcome != outcomeUnknown {
		t.Fatalf("Outcome = %v, want outcomeUnknown", got.Outcome)
	}
}

func TestAssignmentCompatibilityUnknownWhenSourceUnknown(t *testing.T) {
	dv := interBaseVariant()
	smallint := mustParseDiagnosticType(t, "SMALLINT", dv, nil)
	got := assignmentCompatibility(expressionFact{}, smallint, dv)
	if got.Outcome != outcomeUnknown {
		t.Fatalf("Outcome = %v, want outcomeUnknown", got.Outcome)
	}
}
