package sqlsymbol

import (
	"fmt"
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
	// storage. Its Min/Max are the range implied by the declared precision
	// and scale alone -- see sqlType.Min's own doc for why this codebase
	// cannot go further and infer an underlying storage width.
	familyExactNumeric
	// familyApproximate is FLOAT or bare DOUBLE PRECISION: binary floating
	// point. No exact range or conversion rule is verified for this family;
	// assignmentCompatibility always reports it unknown.
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
// Dialect note: a bare "DATE" string is genuinely ambiguous under Dialect 1,
// since both RDB$FIELD_TYPE 12 (a real date-only field) and RDB$FIELD_TYPE
// 35 (Dialect 1's own spelling for what Dialect 3 calls TIMESTAMP) render as
// "DATE" there (internal/database/interbase_catalog.go:29-39). This parser
// cannot recover which one produced a given string, so it resolves every
// Dialect 1 "DATE" to familyDateTime (the wider, date+time interpretation).
// This is a safe-direction choice: it can only cause a real date-only
// Dialect 1 column to be treated as if it also carried a time component,
// which can suppress a would-be narrowing finding but can never fabricate
// one. See doc/interbase-diagnostic-semantics.md for the full citation.
type sqlType struct {
	// Family is the type's broad kind. familyUnknown means
	// parseDiagnosticType could not determine any family for the input;
	// assignmentCompatibility checks this first, before any other field.
	Family sqlTypeFamily

	// Min and Max are the type's exact provable value bounds, as exact
	// rationals (math/big.Rat, never float64 -- a float64 bound would
	// reintroduce the precision-loss bugs this task exists to catch).
	// They are non-nil only for familyExactInteger and familyExactNumeric;
	// nil for every other family, including familyApproximate, which has no
	// verified exact range at all.
	//
	// For familyExactInteger these are the type's fixed two's-complement
	// bounds. For familyExactNumeric these are the range implied by
	// Precision/Scale alone -- a p-digit decimal has a provable exact value
	// range as a matter of decimal arithmetic -- never a storage-width
	// assumption. This codebase's catalog rendering
	// (internal/database/interbase_catalog.go's interBaseNumericType) does
	// not expose which underlying fixed-width field backs a given
	// NUMERIC(p,s)/DECIMAL(p,s) (naturalPrecision is a display default, not
	// a storage signal), so no width-based bound is ever asserted here.
	Min, Max *big.Rat

	// Precision and Scale are the declared decimal digits and fractional
	// digits, populated only for familyExactNumeric (NUMERIC(p,s)/
	// DECIMAL(p,s), including the legacy Dialect-1 scaled DOUBLE PRECISION
	// form, which renders as the identical NUMERIC/DECIMAL string and so is
	// modeled identically -- see interBaseNumericType). Zero for every
	// other family.
	Precision, Scale int

	// CharacterWidth is the declared maximum character count for
	// familyCharacter (CHAR(n)/VARCHAR(n)/CSTRING(n)); zero for every other
	// family, and zero is never a valid declared width for a real
	// familyCharacter type (CHAR(0)/VARCHAR(0) cannot occur), so zero
	// safely doubles as "not applicable." It is a character count, not a
	// byte length: a CHARACTER SET suffix is parsed and discarded, never
	// folded into this count.
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
	return sqlType{Family: familyExactInteger, Min: big.NewRat(min, 1), Max: big.NewRat(max, 1)}
}

// exactNumericRange computes NUMERIC(precision, scale)'s exact value range
// directly from its declared digits: a precision-digit decimal magnitude
// (10^precision - 1) scaled by 10^-scale. This is sound independently of
// whatever storage width actually backs the field -- see sqlType.Min's doc.
func exactNumericRange(precision, scale int) sqlType {
	unscaledMax := new(big.Int).Sub(pow10(precision), big.NewInt(1))
	scaleDivisor := pow10(scale)
	max := new(big.Rat).SetFrac(unscaledMax, scaleDivisor)
	min := new(big.Rat).Neg(max)
	return sqlType{Family: familyExactNumeric, Precision: precision, Scale: scale, Min: min, Max: max}
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
// comma itself, and both operands being plain non-negative integers, are
// still required.
func parseNumericType(s string) (sqlType, bool) {
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
		if err1 != nil || err2 != nil || precision <= 0 || scale < 0 {
			return sqlType{}, false
		}
		return exactNumericRange(precision, scale), true
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
	if t, ok := parseNumericType(s); ok {
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

	if source.Value != nil && destination.Min != nil && destination.Max != nil {
		if source.Value.Cmp(destination.Min) < 0 || source.Value.Cmp(destination.Max) > 0 {
			return compatibility{
				Outcome: outcomeDefinitelyInvalid,
				Reason: fmt.Sprintf("literal %s is outside the %s destination's exact range [%s, %s]",
					source.Value.RatString(), destination.Family, destination.Min.RatString(), destination.Max.RatString()),
			}
		}
		return compatibility{
			Outcome: outcomeSafe,
			Reason: fmt.Sprintf("literal %s is within the %s destination's exact range [%s, %s]",
				source.Value.RatString(), destination.Family, destination.Min.RatString(), destination.Max.RatString()),
		}
	}

	if source.Type.Family == familyUnknown {
		return compatibility{Outcome: outcomeUnknown, Reason: "source expression's type could not be determined"}
	}
	src := source.Type

	if src.Min != nil && src.Max != nil && destination.Min != nil && destination.Max != nil {
		if src.Min.Cmp(destination.Min) >= 0 && src.Max.Cmp(destination.Max) <= 0 {
			return compatibility{
				Outcome: outcomeSafe,
				Reason: fmt.Sprintf("source's exact range [%s, %s] fits entirely within destination's exact range [%s, %s]",
					src.Min.RatString(), src.Max.RatString(), destination.Min.RatString(), destination.Max.RatString()),
			}
		}
		return compatibility{
			Outcome: outcomePossibleLoss,
			Reason: fmt.Sprintf("source's exact range [%s, %s] exceeds destination's exact range [%s, %s] for some values",
				src.Min.RatString(), src.Max.RatString(), destination.Min.RatString(), destination.Max.RatString()),
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
				Reason:  fmt.Sprintf("source's maximum width %d fits within destination's declared width %d", src.CharacterWidth, destination.CharacterWidth),
			}
		}
		return compatibility{
			Outcome: outcomePossibleLoss,
			Reason:  fmt.Sprintf("source's maximum width %d exceeds destination's declared width %d", src.CharacterWidth, destination.CharacterWidth),
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
