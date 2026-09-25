package sqlsymbol

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/sqls-server/sqls/dialect"
)

// sqlTypeFamily classifies the broad kind of value an sqlType represents.
// The zero value, familyUnknown, is deliberate: it is what an sqlType looks
// like when parseDiagnosticType could not determine any family at all, and
// assignmentCompatibility treats it identically to "no destination type
// information available" rather than as a distinct ninth family.
type sqlTypeFamily uint8

const (
	familyUnknown sqlTypeFamily = iota
	// familyExactInteger is SMALLINT/INTEGER/BIGINT: exact whole-number
	// storage with a fixed two's-complement range (standard integer bounds,
	// not InterBase-specific).
	familyExactInteger
	// familyExactNumeric is NUMERIC(p, s)/DECIMAL(p, s): exact fixed-point
	// storage backed by a fixed-width integer type InterBase selects from
	// the declared precision (see numericStorageBackedRange, live-verified
	// against interbase_reference). StorageMin/StorageMax hold that
	// engine-enforced range; DeclaredMin/DeclaredMax separately hold the
	// range implied purely by the declared digit count. See sqlType's own
	// field docs for why both exist and which one assignmentCompatibility
	// actually uses for the safe/definitely-invalid boundary.
	familyExactNumeric
	// familyApproximate is FLOAT or bare DOUBLE PRECISION, or -- per the
	// live-verified correction in doc/interbase-diagnostic-semantics.md's
	// I2 entry -- a Dialect 1 NUMERIC(p, s)/DECIMAL(p, s) declaration whose
	// precision (10-18) InterBase actually backs with DOUBLE PRECISION
	// storage rather than a fixed-width exact integer. No exact range or
	// conversion rule is verified for this family; assignmentCompatibility
	// always reports it unknown.
	familyApproximate
	// familyDateOnly is a Dialect 3 DATE: a calendar date with no time
	// component.
	familyDateOnly
	// familyDateTime is a Dialect 1 DATE or a Dialect 3 TIMESTAMP: a
	// calendar date plus a time component. Dialect 1 has no separate
	// TIMESTAMP keyword at all (see interBaseTypeName's own dialect
	// branch), so every Dialect 1 "DATE" is modeled here rather than as
	// familyDateOnly -- see sqlType's package-level doc for the residual
	// ambiguity this simplification accepts.
	familyDateTime
	// familyTime is TIME: a time of day with no date component.
	familyTime
	// familyCharacter is CHAR/VARCHAR/CSTRING: a bounded character string.
	// CharacterWidth holds the declared maximum character count.
	familyCharacter
	// familyBoolean is BOOLEAN.
	familyBoolean
	// familyBlob is BLOB, BLOB_ID, or QUAD: opaque/large-object storage.
	// No assignment-compatibility rule is verified for this family.
	familyBlob
)

// String renders a human-readable family name, used only inside
// compatibility.Reason text.
func (f sqlTypeFamily) String() string {
	switch f {
	case familyExactInteger:
		return "exact integer"
	case familyExactNumeric:
		return "exact numeric"
	case familyApproximate:
		return "approximate floating point"
	case familyDateOnly:
		return "date-only"
	case familyDateTime:
		return "date and time"
	case familyTime:
		return "time-only"
	case familyCharacter:
		return "character"
	case familyBoolean:
		return "boolean"
	case familyBlob:
		return "blob"
	default:
		return "unknown"
	}
}

// sqlType is a parsed InterBase type declaration. parseDiagnosticType
// retains family, storage/range, precision, scale, and character width
// independently, since a destination type is checked against each dimension
// separately by assignmentCompatibility.
//
// Dialect note (DATE/TIMESTAMP): a bare "DATE" string is genuinely ambiguous
// under Dialect 1, since both RDB$FIELD_TYPE 12 (a real date-only field) and
// RDB$FIELD_TYPE 35 (Dialect 1's own spelling for what Dialect 3 calls
// TIMESTAMP) render as "DATE" there
// (internal/database/interbase_catalog.go:29-39). This parser cannot
// recover which one produced a given string, so it resolves every Dialect 1
// "DATE" to familyDateTime (the wider, date+time interpretation). Field
// type 35 always means date+time under Dialect 1, so a same-dialect
// DATE-into-DATE assignment built on this rule is genuinely type-compatible
// on that basis. This is a safe-direction choice for the narrowing checks
// this task enables: it can cause a real date-only Dialect 1 column to be
// treated as if it also carried a time component, which can only suppress a
// would-be narrowing finding in those checks, never introduce one -- but
// that is not a blanket guarantee against every possible false "safe"
// verdict from every rule in this file. See doc/interbase-diagnostic-
// semantics.md's I4 entry for the precise scope of this guarantee.
type sqlType struct {
	// Family is the type's broad kind. familyUnknown means
	// parseDiagnosticType could not determine any family for the input;
	// assignmentCompatibility checks this first, before any other field.
	Family sqlTypeFamily

	// DeclaredMin and DeclaredMax are the value range implied purely by a
	// NUMERIC(p,s)/DECIMAL(p,s) declaration's own digit count (p decimal
	// digits, s of them fractional) -- e.g. NUMERIC(4,0) implies ±9999.
	// Populated only for familyExactNumeric; nil for every other family,
	// including familyExactInteger (a bare SMALLINT/INTEGER/BIGINT has no
	// separate "declared" concept distinct from its storage type, so only
	// StorageMin/StorageMax are populated for it).
	//
	// This range is informational only. assignmentCompatibility's
	// safe/definitely-invalid boundary never uses these fields -- see
	// StorageMin/StorageMax below and doc/interbase-diagnostic-semantics.md's
	// C1 entry, which live-verifies (via CAST probes against
	// interbase_reference) that InterBase does NOT enforce the declared
	// digit count at the storage layer: CAST(10000 AS NUMERIC(4,0))
	// succeeds even though 10000 exceeds ±9999. A future task may still
	// want DeclaredMin/DeclaredMax to phrase a message like "this value has
	// more digits than declared, though the engine will still accept it."
	DeclaredMin, DeclaredMax *big.Rat

	// StorageMin and StorageMax are the value range InterBase actually
	// enforces at the storage layer, as exact rationals (math/big.Rat,
	// never float64 -- a float64 bound would reintroduce the
	// precision-loss bugs this task exists to catch). Non-nil only for
	// familyExactInteger and familyExactNumeric; nil for every other
	// family, including familyApproximate, which has no verified exact
	// range at all.
	//
	// For familyExactInteger these are the type's fixed two's-complement
	// bounds (SMALLINT/INTEGER/BIGINT). For familyExactNumeric these are
	// the exact two's-complement bounds of whichever fixed-width integer
	// type InterBase selects to back the declaration, based on precision
	// alone (see numericStorageBackedRange), scaled by 10^-Scale --
	// live-verified: CAST(32768 AS NUMERIC(4,0)) overflows (SMALLINT's
	// ±32767 boundary) and CAST(327.67 AS NUMERIC(4,2)) succeeds (also
	// exactly SMALLINT's max, scaled). assignmentCompatibility's
	// safe/definitely-invalid judgment always uses these fields, never
	// DeclaredMin/DeclaredMax.
	StorageMin, StorageMax *big.Rat

	// Precision and Scale are the declared decimal digits and fractional
	// digits, populated only for familyExactNumeric (NUMERIC(p,s)/
	// DECIMAL(p,s)). Scale is implicitly 0 for familyExactInteger (the zero
	// value, left unset rather than explicitly assigned, since a bare
	// integer type has no fractional part) -- this lets
	// assignmentCompatibility compare Scale across an
	// exact-integer/exact-numeric pair uniformly, without a family special
	// case. Zero for every other family.
	Precision, Scale int

	// CharacterWidth is the maximum count this codebase's own catalog
	// renderer wrote inside CHAR(n)/VARCHAR(n)/CSTRING(n)'s parentheses;
	// zero for every other family (and zero is never a valid declared
	// width for a real familyCharacter type, so it safely doubles as "not
	// applicable").
	//
	// CAUTION: this number is not reliably a character count.
	// interBaseCharacterLength (the renderer this parser targets,
	// internal/database/interbase_catalog.go:121-132) falls back to
	// RDB$FIELD_LENGTH -- a byte count -- whenever RDB$CHARACTER_LENGTH is
	// NULL, and that byte count is written into the identical
	// CHAR(n)/VARCHAR(n) position with no marker distinguishing it from a
	// genuine character count. This parser cannot tell the two cases apart
	// from the rendered string alone. See doc/interbase-diagnostic-
	// semantics.md's I3 entry for how assignmentCompatibility hedges
	// around this honest limitation.
	CharacterWidth int
}

// pow10 computes 10^n as an exact big.Int, used to derive a NUMERIC/DECIMAL
// declaration's exact value range from its precision and scale.
func pow10(n int) *big.Int {
	if n < 0 {
		n = 0
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

func exactIntegerRange(min, max int64) sqlType {
	return sqlType{Family: familyExactInteger, StorageMin: big.NewRat(min, 1), StorageMax: big.NewRat(max, 1)}
}

// numericStorageBackedRange returns the exact two's-complement bounds of the
// fixed-width integer type InterBase actually selects to back a
// NUMERIC/DECIMAL declaration of the given precision. Live-verified against
// interbase_reference: CAST(10000 AS NUMERIC(4,0)) succeeds -- SMALLINT's
// ±32767 range, not the ±9999 that 4 decimal digits alone would imply --
// CAST(32768 AS NUMERIC(4,0)) overflows, and CAST(327.67 AS NUMERIC(4,2))
// succeeds, exactly SMALLINT's max scaled by 10^-2. Grounded in
// interbase-go/schema/ddl.go's dialect1NumericStorageCompatible and the
// precision switch inside sqlTypePartsWithRenderer.
//
// The precision buckets are the same for Dialect 1 and Dialect 3 for
// precision 1-9 (1-4 -> SMALLINT, 5-9 -> INTEGER); they diverge for
// precision 10-18: Dialect 3 backs it with an exact BIGINT, but Dialect 1
// backs it with DOUBLE PRECISION -- an approximate type with no exact range
// at all (dialect1NumericStorageCompatible's fieldTypeDouble case).
// approximate is true only in that Dialect 1, precision 10-18 case; callers
// (exactNumericRange) must not fabricate exact bounds when it is true -- see
// doc/interbase-diagnostic-semantics.md's I2 entry.
func numericStorageBackedRange(precision int, dv dialect.DriverVariant) (min, max *big.Int, approximate bool) {
	switch {
	case precision >= 1 && precision <= 4:
		return big.NewInt(-32768), big.NewInt(32767), false
	case precision >= 5 && precision <= 9:
		return big.NewInt(-2147483648), big.NewInt(2147483647), false
	case precision >= 10 && precision <= 18:
		if dv.Variant.InterBaseSQLDialect() == 1 {
			return nil, nil, true
		}
		return big.NewInt(math.MinInt64), big.NewInt(math.MaxInt64), false
	default:
		return nil, nil, false
	}
}

// exactNumericRange computes NUMERIC(precision, scale)'s two independent
// value ranges -- see sqlType's DeclaredMin/StorageMin field docs for what
// each represents and why both exist. When the storage InterBase actually
// selects for this precision is itself approximate (Dialect 1, precision
// 10-18 -- see numericStorageBackedRange), this returns familyApproximate
// with no fabricated exact bounds at all, rather than a familyExactNumeric
// carrying an invented storage range.
func exactNumericRange(precision, scale int, dv dialect.DriverVariant) sqlType {
	storageMinInt, storageMaxInt, approximate := numericStorageBackedRange(precision, dv)
	if approximate {
		return sqlType{Family: familyApproximate}
	}

	scaleDivisor := pow10(scale)
	declaredUnscaledMax := new(big.Int).Sub(pow10(precision), big.NewInt(1))
	declaredMax := new(big.Rat).SetFrac(declaredUnscaledMax, scaleDivisor)
	declaredMin := new(big.Rat).Neg(declaredMax)

	storageMin := new(big.Rat).SetFrac(storageMinInt, scaleDivisor)
	storageMax := new(big.Rat).SetFrac(storageMaxInt, scaleDivisor)

	return sqlType{
		Family:      familyExactNumeric,
		Precision:   precision,
		Scale:       scale,
		DeclaredMin: declaredMin,
		DeclaredMax: declaredMax,
		StorageMin:  storageMin,
		StorageMax:  storageMax,
	}
}

// roundHalfAwayFromZero rounds a *big.Rat to the nearest *big.Int, breaking
// an exact .5 tie away from zero, using only exact big.Int arithmetic (no
// float64 anywhere) -- this is InterBase's own literal-to-fixed-point
// rounding rule (N1): CAST(32767.5 AS SMALLINT) overflows (rounds away from
// zero to 32768), not CAST(32767.4 AS SMALLINT), which rounds toward zero
// to 32767 and is accepted.
func roundHalfAwayFromZero(value *big.Rat) *big.Int {
	num := value.Num()
	den := value.Denom() // big.Rat.Denom() is always > 0
	quotient, remainder := new(big.Int).QuoRem(num, den, new(big.Int))
	// QuoRem truncates toward zero; remainder has the same sign as num (or
	// is zero) and |remainder| < den. Round the truncated quotient away
	// from zero by one when the remainder is at least half of den.
	doubledRemainder := new(big.Int).Lsh(new(big.Int).Abs(remainder), 1)
	if doubledRemainder.Cmp(den) >= 0 {
		if num.Sign() >= 0 {
			quotient.Add(quotient, big.NewInt(1))
		} else {
			quotient.Sub(quotient, big.NewInt(1))
		}
	}
	return quotient
}

// roundToScale rounds value to scale fractional decimal digits, half away
// from zero, returning the result as an exact *big.Rat -- the same rounding
// InterBase itself applies to a literal being assigned into a scale-digit
// fixed-point destination (N1), performed before comparing against the
// destination's storage range rather than after.
func roundToScale(value *big.Rat, scale int) *big.Rat {
	divisor := pow10(scale)
	scaled := new(big.Rat).Mul(value, new(big.Rat).SetInt(divisor))
	roundedUnscaled := roundHalfAwayFromZero(scaled)
	return new(big.Rat).SetFrac(roundedUnscaled, divisor)
}

// stripTypeSuffixes cuts a rendered type string at the first CHARACTER SET
// or COLLATE clause, reusing interBaseColumnTypeName's own exact stripping
// approach (internal/database/interbase_catalog.go:46-55): a literal,
// case-sensitive search for " CHARACTER SET " / " COLLATE ", cutting at
// whichever appears first. No base type name produced by this codebase's
// renderer contains either keyword, so the cut is safe.
func stripTypeSuffixes(s string) string {
	if i := strings.Index(s, " CHARACTER SET "); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, " COLLATE "); i >= 0 {
		s = s[:i]
	}
	return s
}

func parseCharacterType(s string) (sqlType, bool) {
	upper := strings.ToUpper(s)
	for _, keyword := range [...]string{"CHAR", "VARCHAR", "CSTRING"} {
		prefix := keyword + "("
		if !strings.HasPrefix(upper, prefix) || !strings.HasSuffix(upper, ")") {
			continue
		}
		inner := strings.TrimSpace(s[len(prefix) : len(s)-1])
		width, err := strconv.Atoi(inner)
		if err != nil || width <= 0 {
			return sqlType{}, false
		}
		return sqlType{Family: familyCharacter, CharacterWidth: width}, true
	}
	return sqlType{}, false
}

// parseNumericType parses "NUMERIC(p, s)"/"DECIMAL(p, s)", the exact form
// interBaseNumericType renders (internal/database/interbase_catalog.go:158).
// The renderer always writes ", " with one space after the comma; this
// parser instead trims whitespace independently around each operand, which
// tolerates incidental spacing variation at no cost to correctness -- the
// comma itself, and both operands being plain integers with 1 <= precision
// <= 18 and 0 <= scale <= precision (interbase-go/schema/ddl.go's
// validNumericDeclaration bounds), are still required. dv resolves which
// fixed-width storage type this precision selects -- see
// numericStorageBackedRange -- since that selection is itself
// dialect-sensitive for precision 10-18 (I2).
func parseNumericType(s string, dv dialect.DriverVariant) (sqlType, bool) {
	upper := strings.ToUpper(s)
	for _, keyword := range [...]string{"NUMERIC", "DECIMAL"} {
		prefix := keyword + "("
		if !strings.HasPrefix(upper, prefix) || !strings.HasSuffix(upper, ")") {
			continue
		}
		inner := s[len(prefix) : len(s)-1]
		parts := strings.SplitN(inner, ",", 2)
		if len(parts) != 2 {
			return sqlType{}, false
		}
		precision, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		scale, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil || precision < 1 || precision > 18 || scale < 0 || scale > precision {
			return sqlType{}, false
		}
		return exactNumericRange(precision, scale, dv), true
	}
	return sqlType{}, false
}

// parseBaseSQLType parses declaration (already suffix-stripped and
// trimmed) against the exact base-type forms
// internal/database/interbase_catalog.go's interBaseColumnType/
// interBaseNumericType produce. It never guesses a family for text it does
// not recognize -- in particular the unrenderable "TYPE(n)" fallback
// (interbase_catalog.go:117) falls through to ok=false here, exactly as the
// brief requires.
func parseBaseSQLType(s string, dv dialect.DriverVariant) (sqlType, bool) {
	if s == "" {
		return sqlType{}, false
	}
	switch strings.ToUpper(s) {
	case "SMALLINT":
		return exactIntegerRange(-32768, 32767), true
	case "INTEGER":
		return exactIntegerRange(-2147483648, 2147483647), true
	case "BIGINT":
		return exactIntegerRange(-9223372036854775808, 9223372036854775807), true
	case "DOUBLE PRECISION", "FLOAT":
		return sqlType{Family: familyApproximate}, true
	case "DATE":
		if dv.Variant.InterBaseSQLDialect() == 1 {
			return sqlType{Family: familyDateTime}, true
		}
		return sqlType{Family: familyDateOnly}, true
	case "TIMESTAMP":
		return sqlType{Family: familyDateTime}, true
	case "TIME":
		return sqlType{Family: familyTime}, true
	case "BOOLEAN":
		return sqlType{Family: familyBoolean}, true
	case "BLOB", "BLOB_ID", "QUAD":
		return sqlType{Family: familyBlob}, true
	}
	if t, ok := parseCharacterType(s); ok {
		return t, true
	}
	if t, ok := parseNumericType(s, dv); ok {
		return t, true
	}
	return sqlType{}, false
}

// maxDomainResolutionDepth bounds a domain-of-domain chain independently of
// cycle detection, against a very long (but non-cyclic) chain. Cycle
// detection below is the primary defense; this is a secondary, arbitrary
// but safe bound -- ordinary schemas never nest domains this deep.
const maxDomainResolutionDepth = 8

// parseDiagnosticType parses declaration -- the exact rendered type-string
// form this codebase's own catalog adapter produces for ColumnDesc.Type,
// ProcedureParameterDesc.Type, and DomainDesc.Type
// (internal/database/interbase_catalog.go's interBaseColumnType and
// domain.SQLType()), or a bare domain name as it appears in PSQL source
// (for example DECLARE VARIABLE X MY_DOMAIN) -- into a structured sqlType.
//
// dv selects which dialect a bare "DATE" declaration is interpreted under;
// see sqlType's own doc for the DATE/TIMESTAMP dialect rule.
//
// When declaration does not match any recognized base-type grammar, it is
// tried as a domain name via c's SemanticCatalog.DomainInfo, but only when
// that domain's metadata is genuinely Present -- an Unknown or Missing
// domain never yields a type. A domain whose own Type names another domain
// is followed for up to maxDomainResolutionDepth levels, refusing to
// recurse through a cycle (a domain that, directly or through a chain,
// resolves back to a name already being resolved) by returning ok=false
// instead of looping or panicking.
//
// ok is false whenever no family could be determined with confidence: an
// unrenderable "TYPE(n)", an array declaration, an absent/unknown/cyclic
// domain, or any other unrecognized text. Callers must treat ok=false as
// "insufficient information," never as a guessed family.
func parseDiagnosticType(declaration string, dv dialect.DriverVariant, c Catalog) (sqlType, bool) {
	return resolveDiagnosticType(declaration, dv, c, nil)
}

func resolveDiagnosticType(declaration string, dv dialect.DriverVariant, c Catalog, visited map[string]bool) (sqlType, bool) {
	trimmed := stripTypeSuffixes(strings.TrimSpace(declaration))
	if t, ok := parseBaseSQLType(trimmed, dv); ok {
		return t, true
	}

	semantic, ok := c.(SemanticCatalog)
	if !ok {
		return sqlType{}, false
	}
	name := Name{Text: strings.TrimSpace(declaration)}
	key := name.Key()
	if key == "" || visited[key] || len(visited) >= maxDomainResolutionDepth {
		return sqlType{}, false
	}
	fact, knowledge := semantic.DomainInfo(name)
	if knowledge != Present {
		return sqlType{}, false
	}
	nextVisited := make(map[string]bool, len(visited)+1)
	for k := range visited {
		nextVisited[k] = true
	}
	nextVisited[key] = true
	return resolveDiagnosticType(fact.Type, dv, c, nextVisited)
}

// expressionFact is a source expression's known facts, gathered by a caller
// before calling assignmentCompatibility.
type expressionFact struct {
	// Value is the expression's exact constant numeric value when the
	// expression is a numeric literal (or a provably constant numeric
	// expression a later task chooses to fold); nil when no exact value is
	// known. math/big.Rat represents both integer and fixed-point decimal
	// literal text exactly -- using float64 here would reintroduce the
	// precision-loss bugs this task exists to catch.
	Value *big.Rat

	// Nullability is the expression's own provable nullability: a bare NULL
	// literal is Nullable, any other literal is NotNullable, and a
	// reference's nullability is provable only when the caller populates it
	// from catalog/local-variable metadata. NullUnknown (the zero value)
	// covers everything else.
	Nullability Nullability

	// Type is the expression's own declared type, typically produced by
	// parseDiagnosticType from a resolved column/variable/parameter
	// declaration, or synthesized directly for a literal's natural type.
	// Its zero value (Family == familyUnknown) is this fact's "type is
	// unknown" signal, mirroring how a destination sqlType signals the same
	// thing: assignmentCompatibility treats an unknown Type exactly like no
	// type information at all.
	Type sqlType

	// ExplicitCast is true when Type (and/or Value) was derived from an
	// explicit CAST(... AS <type>) around the expression rather than from
	// the expression's own natural type. It is provenance a later task can
	// use to decide whether an otherwise-lossy assignment was deliberately
	// acknowledged by the query's author; assignmentCompatibility itself
	// does not change its outcome based on this field.
	ExplicitCast bool
}

// compatibilityOutcome classifies assignmentCompatibility's verdict.
type compatibilityOutcome uint8

const (
	// outcomeUnknown means Task 8 could not prove any of the other three
	// outcomes: insufficient source/destination information, an unverified
	// conversion rule, or a family this task deliberately does not model
	// (arrays, BLOB/BLOB_ID/QUAD, an unrenderable TYPE(n)). This is the
	// default, and the brief's own instruction: "if a reference is
	// inconclusive, leave the rule disabled/unknown."
	outcomeUnknown compatibilityOutcome = iota
	// outcomeSafe means the source is proven to always fit the destination
	// without loss.
	outcomeSafe
	// outcomeDefinitelyInvalid means the source is proven to never fit the
	// destination -- for example a literal outside the destination's exact
	// numeric range.
	outcomeDefinitelyInvalid
	// outcomePossibleLoss means the assignment's source and destination
	// share a verified family, but the destination's range/width is
	// narrower than the source's declared range/width, so some (but not
	// necessarily this specific) value of the source's type would not fit.
	outcomePossibleLoss
)

// compatibility is assignmentCompatibility's own internal fact record: an
// outcome plus a human-readable reason citing the specific rule that
// produced it. It is not an LSP Finding -- Tasks 9-11 build the actual
// Finding value (code, message, severity, span) from a compatibility,
// choosing which outcomes are worth surfacing and at what severity.
type compatibility struct {
	Outcome compatibilityOutcome
	Reason  string
}

// assignmentCompatibility judges whether source can be assigned into a
// column/parameter/variable of type destination.
//
// dv is accepted for symmetry with parseDiagnosticType and reserved for a
// future dialect-sensitive comparison rule; no rule enabled by this task
// needs it beyond what parseDiagnosticType has already encoded into
// source.Type's and destination's Family (a Dialect 1 "DATE" and a Dialect 3
// "DATE" already parse to different families, so the mismatch surfaces
// through the ordinary family check below without dv being consulted again
// here).
func assignmentCompatibility(source expressionFact, destination sqlType, dv dialect.DriverVariant) compatibility {
	if destination.Family == familyUnknown {
		return compatibility{Outcome: outcomeUnknown, Reason: "destination type could not be determined"}
	}

	if source.Value != nil && destination.StorageMin != nil && destination.StorageMax != nil {
		// N1: InterBase rounds the literal to the destination's scale
		// half-away-from-zero BEFORE checking it against the storage
		// range, not after -- CAST(32767.4 AS SMALLINT) rounds to 32767
		// (in range, accepted) while CAST(32767.5 AS SMALLINT) rounds to
		// 32768 (out of range, overflow). Comparing the raw, unrounded
		// literal against the storage range (as an earlier version of
		// this function did) falsely reports the former as invalid.
		rounded := roundToScale(source.Value, destination.Scale)
		roundingOccurred := rounded.Cmp(source.Value) != 0
		if rounded.Cmp(destination.StorageMin) < 0 || rounded.Cmp(destination.StorageMax) > 0 {
			return compatibility{
				Outcome: outcomeDefinitelyInvalid,
				Reason: fmt.Sprintf("literal %s (rounds to %s at scale %d) is outside the %s destination's engine-enforced storage range [%s, %s]",
					source.Value.RatString(), rounded.RatString(), destination.Scale, destination.Family, destination.StorageMin.RatString(), destination.StorageMax.RatString()),
			}
		}
		if roundingOccurred {
			return compatibility{
				Outcome: outcomePossibleLoss,
				Reason: fmt.Sprintf("literal %s has more fractional digits than the destination's scale %d and will be rounded to %s",
					source.Value.RatString(), destination.Scale, rounded.RatString()),
			}
		}
		return compatibility{
			Outcome: outcomeSafe,
			Reason: fmt.Sprintf("literal %s is within the %s destination's engine-enforced storage range [%s, %s] with no rounding at scale %d",
				source.Value.RatString(), destination.Family, destination.StorageMin.RatString(), destination.StorageMax.RatString(), destination.Scale),
		}
	}

	if source.Type.Family == familyUnknown {
		return compatibility{Outcome: outcomeUnknown, Reason: "source expression's type could not be determined"}
	}
	src := source.Type

	if src.StorageMin != nil && src.StorageMax != nil && destination.StorageMin != nil && destination.StorageMax != nil {
		rangeFits := src.StorageMin.Cmp(destination.StorageMin) >= 0 && src.StorageMax.Cmp(destination.StorageMax) <= 0
		scaleFits := src.Scale <= destination.Scale
		if rangeFits && scaleFits {
			return compatibility{
				Outcome: outcomeSafe,
				Reason: fmt.Sprintf("source's storage range [%s, %s] at scale %d fits entirely within destination's storage range [%s, %s] at scale %d",
					src.StorageMin.RatString(), src.StorageMax.RatString(), src.Scale,
					destination.StorageMin.RatString(), destination.StorageMax.RatString(), destination.Scale),
			}
		}
		switch {
		case !rangeFits && !scaleFits:
			return compatibility{
				Outcome: outcomePossibleLoss,
				Reason: fmt.Sprintf("source's storage range [%s, %s] exceeds destination's [%s, %s], and source's scale %d exceeds destination's scale %d",
					src.StorageMin.RatString(), src.StorageMax.RatString(), destination.StorageMin.RatString(), destination.StorageMax.RatString(), src.Scale, destination.Scale),
			}
		case !rangeFits:
			return compatibility{
				Outcome: outcomePossibleLoss,
				Reason: fmt.Sprintf("source's storage range [%s, %s] exceeds destination's storage range [%s, %s] for some values",
					src.StorageMin.RatString(), src.StorageMax.RatString(), destination.StorageMin.RatString(), destination.StorageMax.RatString()),
			}
		default: // !scaleFits
			return compatibility{
				Outcome: outcomePossibleLoss,
				Reason:  fmt.Sprintf("source's scale %d exceeds destination's scale %d and will be rounded", src.Scale, destination.Scale),
			}
		}
	}

	if src.Family != destination.Family {
		return compatibility{
			Outcome: outcomeUnknown,
			Reason:  fmt.Sprintf("conversion from %s to %s is not a verified InterBase rule", src.Family, destination.Family),
		}
	}

	switch destination.Family {
	case familyCharacter:
		if src.CharacterWidth == 0 || destination.CharacterWidth == 0 {
			return compatibility{Outcome: outcomeUnknown, Reason: "character width is not known for source or destination"}
		}
		if src.CharacterWidth <= destination.CharacterWidth {
			return compatibility{
				Outcome: outcomeSafe,
				Reason:  fmt.Sprintf("source's maximum declared width %d fits within destination's declared width %d", src.CharacterWidth, destination.CharacterWidth),
			}
		}
		// I3: CharacterWidth is not reliably a character count -- it can be
		// an RDB$FIELD_LENGTH byte-count fallback (see sqlType's own doc).
		// This codebase cannot tell from the rendered string alone whether
		// a narrower destination genuinely means less room, so it does not
		// assert outcomePossibleLoss here: doing so risks a false positive
		// whenever either side's width is actually a byte count rather than
		// a character count.
		return compatibility{
			Outcome: outcomeUnknown,
			Reason:  fmt.Sprintf("source's declared width %d exceeds destination's declared width %d, but CharacterWidth may be a byte count rather than a character count for either side", src.CharacterWidth, destination.CharacterWidth),
		}
	case familyDateOnly, familyDateTime, familyTime, familyBoolean:
		return compatibility{
			Outcome: outcomeSafe,
			Reason:  fmt.Sprintf("source and destination are both the verified %s family", destination.Family),
		}
	default:
		// familyApproximate, familyBlob: no verified conversion rule, even
		// same-family (see sqlType's own family docs).
		return compatibility{
			Outcome: outcomeUnknown,
			Reason:  fmt.Sprintf("conversion within the %s family is not verified", destination.Family),
		}
	}
}
