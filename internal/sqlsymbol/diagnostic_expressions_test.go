package sqlsymbol

import (
	"math/big"
	"strings"
	"testing"
)

// spanOfOccurrence returns the byte span of the occurrence-th (0-based)
// appearance of marker in text.
func spanOfOccurrence(t *testing.T, text, marker string, occurrence int) Span {
	t.Helper()
	from := 0
	offset := -1
	for i := 0; i <= occurrence; i++ {
		relative := strings.Index(text[from:], marker)
		if relative < 0 {
			t.Fatalf("occurrence %d of %q not found in %q", i, marker, text)
		}
		offset = from + relative
		from = offset + len(marker)
	}
	return Span{Start: offset, End: offset + len(marker)}
}

func spanOf(t *testing.T, text, marker string) Span {
	t.Helper()
	return spanOfOccurrence(t, text, marker, 0)
}

func requireRatEqual(t *testing.T, got *big.Rat, want *big.Rat) {
	t.Helper()
	if got == nil || want == nil {
		if got != want {
			t.Fatalf("Value = %v, want %v", got, want)
		}
		return
	}
	if got.Cmp(want) != 0 {
		t.Fatalf("Value = %s, want %s", got.RatString(), want.RatString())
	}
}

func TestDiagnosticExpressionIntegerLiteral(t *testing.T) {
	text := "SELECT 1 FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "1"))
	requireRatEqual(t, fact.Value, big.NewRat(1, 1))
	if fact.Type.Family != familyUnknown {
		t.Fatalf("Type.Family = %v, want familyUnknown (a bare literal's Value is the load-bearing fact, not a guessed Type)", fact.Type.Family)
	}
	if fact.Nullability != NotNullable {
		t.Fatalf("Nullability = %v, want NotNullable", fact.Nullability)
	}
}

func TestDiagnosticExpressionDecimalLiteral(t *testing.T) {
	text := "SELECT 1.5 FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "1.5"))
	requireRatEqual(t, fact.Value, big.NewRat(3, 2))
}

func TestDiagnosticExpressionNegativeConstant(t *testing.T) {
	text := "SELECT -5 FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "-5"))
	requireRatEqual(t, fact.Value, big.NewRat(-5, 1))
	if fact.Nullability != NotNullable {
		t.Fatalf("Nullability = %v, want NotNullable", fact.Nullability)
	}
}

func TestDiagnosticExpressionNegativeDecimalConstant(t *testing.T) {
	text := "SELECT -32768.4 FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "-32768.4"))
	requireRatEqual(t, fact.Value, big.NewRat(-327684, 10))
}

// TestDiagnosticExpressionHugeIntegerLiteral proves math/big.Rat is used
// throughout with no float64 conversion anywhere: a 40-digit literal must
// round-trip exactly.
func TestDiagnosticExpressionHugeIntegerLiteral(t *testing.T) {
	huge := "1234567890123456789012345678901234567890"
	text := "SELECT " + huge + " FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, huge))
	want, ok := new(big.Rat).SetString(huge)
	if !ok {
		t.Fatal("test setup: could not parse expected huge literal")
	}
	requireRatEqual(t, fact.Value, want)
}

// TestDiagnosticExpressionExponentLiteralIsUnknown is I3's required
// fixture: an exponent-form literal (1e5) is approximate/floating-point
// valued at the engine level, not an exact decimal value, even though its
// mathematical value happens to be a whole number. big.Rat.SetString
// itself accepts this syntax and would silently fold it to an exact
// rational; numberFact must refuse that rather than fabricate an exact
// Value for what InterBase treats as approximate.
func TestDiagnosticExpressionExponentLiteralIsUnknown(t *testing.T) {
	text := "SELECT 1e5 FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "1e5"))
	if fact.Value != nil {
		t.Fatalf("Value = %v, want nil (exponent literals are approximate, not exact)", fact.Value)
	}
}

func TestDiagnosticExpressionStringLiteral(t *testing.T) {
	text := "SELECT 'ABC' FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "'ABC'"))
	if fact.Value != nil {
		t.Fatalf("Value = %v, want nil for a string literal", fact.Value)
	}
	if fact.Type.Family != familyCharacter || fact.Type.CharacterWidth != 3 {
		t.Fatalf("Type = %+v, want familyCharacter width 3", fact.Type)
	}
	if fact.Nullability != NotNullable {
		t.Fatalf("Nullability = %v, want NotNullable", fact.Nullability)
	}
}

// TestDiagnosticExpressionNullLiteral is the brief's explicit rule: NULL is
// a null fact (Nullability: Nullable), never a fabricated universal
// concrete SQL type.
func TestDiagnosticExpressionNullLiteral(t *testing.T) {
	text := "SELECT NULL FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "NULL"))
	if fact.Nullability != Nullable {
		t.Fatalf("Nullability = %v, want Nullable", fact.Nullability)
	}
	if fact.Type.Family != familyUnknown {
		t.Fatalf("Type.Family = %v, want familyUnknown (no fabricated NULL type)", fact.Type.Family)
	}
	if fact.Value != nil {
		t.Fatalf("Value = %v, want nil", fact.Value)
	}
}

func TestDiagnosticExpressionLocalVariable(t *testing.T) {
	text := "CREATE PROCEDURE P AS DECLARE VARIABLE X SMALLINT; BEGIN SELECT ID FROM T WHERE ID = :X; END"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, ":X"))
	if fact.Type.Family != familyExactInteger {
		t.Fatalf("Type.Family = %v, want familyExactInteger (resolved SMALLINT local)", fact.Type.Family)
	}
	if fact.Type.StorageMax == nil || fact.Type.StorageMax.Cmp(big.NewRat(32767, 1)) != 0 {
		t.Fatalf("StorageMax = %v, want 32767", fact.Type.StorageMax)
	}
}

func TestDiagnosticExpressionColumnReference(t *testing.T) {
	text := "SELECT V FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "V"))
	if fact.Type.Family != familyExactInteger {
		t.Fatalf("Type.Family = %v, want familyExactInteger", fact.Type.Family)
	}
	if fact.Nullability != Nullable {
		t.Fatalf("Nullability = %v, want Nullable (V is declared Nullable in the fixture catalog)", fact.Nullability)
	}
}

func TestDiagnosticExpressionColumnReferenceNotNullable(t *testing.T) {
	text := "SELECT ID FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "ID"))
	if fact.Nullability != NotNullable {
		t.Fatalf("Nullability = %v, want NotNullable (ID is declared NOT NULL in the fixture catalog)", fact.Nullability)
	}
}

func TestDiagnosticExpressionParentheses(t *testing.T) {
	text := "SELECT (1) FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "(1)"))
	requireRatEqual(t, fact.Value, big.NewRat(1, 1))
}

func TestDiagnosticExpressionNestedParentheses(t *testing.T) {
	text := "SELECT ((1 + 2)) FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "((1 + 2))"))
	requireRatEqual(t, fact.Value, big.NewRat(3, 1))
}

func TestDiagnosticExpressionArithmeticAddition(t *testing.T) {
	text := "SELECT 2 + 3 FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "2 + 3"))
	requireRatEqual(t, fact.Value, big.NewRat(5, 1))
}

func TestDiagnosticExpressionArithmeticSubtraction(t *testing.T) {
	text := "SELECT 10 - 4 FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "10 - 4"))
	requireRatEqual(t, fact.Value, big.NewRat(6, 1))
}

func TestDiagnosticExpressionArithmeticSubtractionNegativeOperand(t *testing.T) {
	text := "SELECT 5 - -3 FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "5 - -3"))
	requireRatEqual(t, fact.Value, big.NewRat(8, 1))
}

func TestDiagnosticExpressionArithmeticMultiplication(t *testing.T) {
	text := "SELECT 4 * 5 FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "4 * 5"))
	requireRatEqual(t, fact.Value, big.NewRat(20, 1))
}

func TestDiagnosticExpressionArithmeticChain(t *testing.T) {
	text := "SELECT 2 + 3 * 4 FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "2 + 3 * 4"))
	requireRatEqual(t, fact.Value, big.NewRat(14, 1))
}

// TestDiagnosticExpressionDivisionUnknown is the required division decision:
// no citable InterBase grounding for exact-integer division's truncation
// rule was found in this session's available material, so division always
// reports unknown rather than guessing (see report).
func TestDiagnosticExpressionDivisionUnknown(t *testing.T) {
	text := "SELECT 10 / 3 FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "10 / 3"))
	if fact.Value != nil {
		t.Fatalf("Value = %v, want nil (division has no verified exact-result rule)", fact.Value)
	}
}

// TestDiagnosticExpressionConcatenationMatchesExistingWidth cross-checks a
// concatenation width.go's own concatenationWidth already computes
// correctly, proving this task's independent implementation agrees.
func TestDiagnosticExpressionConcatenationMatchesExistingWidth(t *testing.T) {
	text := "SELECT NAME || 'X' FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "NAME || 'X'"))
	if fact.Type.Family != familyCharacter {
		t.Fatalf("Type.Family = %v, want familyCharacter", fact.Type.Family)
	}
	if fact.Type.CharacterWidth != 11 {
		t.Fatalf("CharacterWidth = %d, want 11 (VARCHAR(10) + 'X')", fact.Type.CharacterWidth)
	}
}

func TestDiagnosticExpressionCastLiteralOutOfRange(t *testing.T) {
	text := "SELECT CAST(40000 AS SMALLINT) FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "CAST(40000 AS SMALLINT)"))
	requireRatEqual(t, fact.Value, big.NewRat(40000, 1))
	if fact.Type.Family != familyExactInteger {
		t.Fatalf("Type.Family = %v, want familyExactInteger", fact.Type.Family)
	}
	if fact.Type.StorageMax == nil || fact.Type.StorageMax.Cmp(big.NewRat(32767, 1)) != 0 {
		t.Fatalf("StorageMax = %v, want 32767 (SMALLINT)", fact.Type.StorageMax)
	}
	if !fact.ExplicitCast {
		t.Fatal("ExplicitCast = false, want true")
	}
	// This function's own job is only to report the fact accurately;
	// judging that 40000 does not fit SMALLINT is assignmentCompatibility's
	// job when this fact is later used as a source, not this task's.
	verdict := assignmentCompatibility(fact, mustParseDiagnosticType(t, "INTEGER", interBaseVariant(), nil), interBaseVariant())
	if verdict.Outcome != outcomeSafe {
		t.Fatalf("sanity check: 40000 into INTEGER should be outcomeSafe, got %v", verdict.Outcome)
	}
}

// TestDiagnosticExpressionCastOrdinaryNarrowing is the brief's required
// contrast case: a CAST around a non-literal reference still reports the
// destination type and ExplicitCast, but no fabricated Value.
func TestDiagnosticExpressionCastOrdinaryNarrowing(t *testing.T) {
	text := "SELECT CAST(ID AS SMALLINT) FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "CAST(ID AS SMALLINT)"))
	if fact.Value != nil {
		t.Fatalf("Value = %v, want nil (source is a column reference, not a literal)", fact.Value)
	}
	if fact.Type.Family != familyExactInteger || fact.Type.StorageMax.Cmp(big.NewRat(32767, 1)) != 0 {
		t.Fatalf("Type = %+v, want SMALLINT", fact.Type)
	}
	if !fact.ExplicitCast {
		t.Fatal("ExplicitCast = false, want true")
	}
}

// TestDiagnosticExpressionCastToCharacterDropsValue is C1's required
// fixture: CAST-ing a literal to a non-exact-numeric target family (here
// familyCharacter) must never propagate the source literal's Value, even
// though the literal's own numeric value is exactly known. Before this fix,
// Value was propagated regardless of the destination family, which could
// make a later assignmentCompatibility check treat the CAST's own
// character-typed result as if it were still a bare numeric literal (its
// Value-based fast path runs before the ordinary family-mismatch check),
// falsely reporting outcomeDefinitelyInvalid for a completely unrelated
// character-to-integer conversion this package has no verified rule for.
func TestDiagnosticExpressionCastToCharacterDropsValue(t *testing.T) {
	text := "SELECT CAST(32767.5 AS CHAR(7)) FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "CAST(32767.5 AS CHAR(7))"))
	if fact.Value != nil {
		t.Fatalf("Value = %v, want nil (CAST target is familyCharacter, not exact-numeric)", fact.Value)
	}
	if fact.Type.Family != familyCharacter {
		t.Fatalf("Type.Family = %v, want familyCharacter", fact.Type.Family)
	}
	verdict := assignmentCompatibility(fact, mustParseDiagnosticType(t, "SMALLINT", interBaseVariant(), nil), interBaseVariant())
	if verdict.Outcome == outcomeDefinitelyInvalid {
		t.Fatalf("verdict = %+v, want not outcomeDefinitelyInvalid (no verified character-to-integer rule, and the stale Value must not resurrect a numeric-range check)", verdict)
	}
}

// TestDiagnosticExpressionCastToApproximateDropsValue is C1's required
// fixture for familyApproximate: DOUBLE PRECISION has no verified exact
// range at all, so a CAST into it must never claim a known exact Value.
func TestDiagnosticExpressionCastToApproximateDropsValue(t *testing.T) {
	text := "SELECT CAST(1.5 AS DOUBLE PRECISION) FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "CAST(1.5 AS DOUBLE PRECISION)"))
	if fact.Value != nil {
		t.Fatalf("Value = %v, want nil (CAST target is familyApproximate)", fact.Value)
	}
	if fact.Type.Family != familyApproximate {
		t.Fatalf("Type.Family = %v, want familyApproximate", fact.Type.Family)
	}
}

// TestDiagnosticExpressionCastToNumericRoundsValue is C1's required
// fixture for the exact-numeric target family: N1's half-away-from-zero
// rounding to the destination's declared scale (reused from Task 8) must be
// applied to the CAST's own resulting Value, not just later at an
// assignment-compatibility check, since the CAST's result is itself the
// rounded value from that point on (e.g. if used as an operand of a further
// arithmetic expression).
func TestDiagnosticExpressionCastToNumericRoundsValue(t *testing.T) {
	text := "SELECT CAST(1.25 AS NUMERIC(4,1)) FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "CAST(1.25 AS NUMERIC(4,1))"))
	requireRatEqual(t, fact.Value, big.NewRat(13, 10))
	if fact.Type.Family != familyExactNumeric {
		t.Fatalf("Type.Family = %v, want familyExactNumeric", fact.Type.Family)
	}
}

func TestDiagnosticExpressionNestedCase(t *testing.T) {
	text := "SELECT CASE WHEN ID = 1 THEN CASE WHEN V = 1 THEN 1 ELSE 2 END ELSE 3 END FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "CASE WHEN ID = 1 THEN CASE WHEN V = 1 THEN 1 ELSE 2 END ELSE 3 END"))
	if fact.Nullability != NotNullable {
		t.Fatalf("Nullability = %v, want NotNullable (every branch, transitively, is a non-NULL literal)", fact.Nullability)
	}
}

func TestDiagnosticExpressionCaseWithoutElseIsNullable(t *testing.T) {
	text := "SELECT CASE WHEN ID = 1 THEN 1 END FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "CASE WHEN ID = 1 THEN 1 END"))
	if fact.Nullability != Nullable {
		t.Fatalf("Nullability = %v, want Nullable (no ELSE means an implicit ELSE NULL)", fact.Nullability)
	}
}

// TestDiagnosticExpressionCaseWithArithmeticInBranch is I2's required
// fixture: a CASE branch containing a unary-minus numeric literal ("-1")
// must not be misread by topLevelArithmeticSplits as if the whole CASE
// expression's own top-level operator were that "-". Before this fix, the
// "-" right after THEN (with no arithmetic operator immediately before it)
// looked exactly like a genuine top-level binary split point to
// topLevelArithmeticSplits, which had no notion of CASE...END nesting.
func TestDiagnosticExpressionCaseWithArithmeticInBranch(t *testing.T) {
	text := "SELECT CASE WHEN ID = 1 THEN -1 ELSE 1 END FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "CASE WHEN ID = 1 THEN -1 ELSE 1 END"))
	if fact.Nullability != NotNullable {
		t.Fatalf("Nullability = %v, want NotNullable (every branch is a non-NULL literal)", fact.Nullability)
	}
}

// TestDiagnosticExpressionCaseWithArithmeticInCondition is I2's other
// required shape: the arithmetic operator sits inside a WHEN condition
// instead of a THEN/ELSE branch, still nested inside the CASE...END that
// wraps the entire expression.
func TestDiagnosticExpressionCaseWithArithmeticInCondition(t *testing.T) {
	text := "SELECT CASE WHEN ID - 1 = 0 THEN 1 ELSE 2 END FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "CASE WHEN ID - 1 = 0 THEN 1 ELSE 2 END"))
	if fact.Nullability != NotNullable {
		t.Fatalf("Nullability = %v, want NotNullable (every branch is a non-NULL literal)", fact.Nullability)
	}
}

// TestDiagnosticExpressionCaseWithConcatenationInConditionIsNotCharacter is
// I2's concatenation-specific required fixture: a "||" inside a WHEN
// condition must not make concatenationFact treat the entire CASE
// expression as if it were itself a character-typed concatenation. Before
// this fix, concatenationFact's top-level "||" scan had no notion of
// CASE...END nesting either, so this would have falsely folded the whole
// CASE into a familyCharacter fact even though both of its actual branches
// (1 and 2) are numeric literals.
func TestDiagnosticExpressionCaseWithConcatenationInConditionIsNotCharacter(t *testing.T) {
	text := "SELECT CASE WHEN NAME || 'x' = 'ax' THEN 1 ELSE 2 END FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "CASE WHEN NAME || 'x' = 'ax' THEN 1 ELSE 2 END"))
	if fact.Type.Family == familyCharacter {
		t.Fatalf("Type.Family = %v, want anything but familyCharacter (both branches are numeric literals)", fact.Type.Family)
	}
}

// TestDiagnosticExpressionCoalesceOuterJoinedField is the brief's required
// fixture: COALESCE over a column that is declared NOT NULL in the catalog
// (U.ID) but is read through a LEFT JOIN's nullable side, paired with a
// NotNullable literal default. This exercises two rules at once: C3's
// outer-join downgrade must not stop COALESCE from still reaching
// NotNullable here, since COALESCE's real semantics guarantee NotNullable
// as soon as ANY argument is provably non-null (the literal 0), regardless
// of what U.ID's own nullability resolves to.
func TestDiagnosticExpressionCoalesceOuterJoinedField(t *testing.T) {
	text := "SELECT COALESCE(U.ID, 0) FROM T LEFT JOIN U ON T.ID = U.ID"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "COALESCE(U.ID, 0)"))
	if fact.Nullability != NotNullable {
		t.Fatalf("Nullability = %v, want NotNullable (COALESCE with a NotNullable default argument)", fact.Nullability)
	}
}

// TestDiagnosticExpressionOuterJoinedNotNullColumnIsDowngraded is C3's
// direct regression: U.ID is declared NOT NULL in the fixture catalog, but
// reading it through a LEFT JOIN's nullable side can genuinely observe NULL
// at runtime (no matching U row). This package tracks no per-relation
// join-side information, so it cannot tell whether THIS particular column
// sits on the nullable side -- the conservative fallback is to withhold
// NotNullable for every column fact in a statement that contains any outer
// join at all, rather than assert a fact it cannot actually prove.
func TestDiagnosticExpressionOuterJoinedNotNullColumnIsDowngraded(t *testing.T) {
	text := "SELECT U.ID FROM T LEFT JOIN U ON T.ID = U.ID"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "U.ID"))
	if fact.Nullability != NullUnknown {
		t.Fatalf("Nullability = %v, want NullUnknown (U.ID is NOT NULL in the catalog, but read through a LEFT JOIN's nullable side)", fact.Nullability)
	}
}

// TestDiagnosticExpressionCaseOverOuterJoinIsNotNotNullable mirrors the
// above for a CASE branch: a CASE whose every branch is the same
// outer-joined NOT NULL column must not claim NotNullable either, since
// caseNullability's all-branches rule is fed by the same downgraded
// per-column fact.
func TestDiagnosticExpressionCaseOverOuterJoinIsNotNotNullable(t *testing.T) {
	text := "SELECT CASE WHEN T.ID = 1 THEN U.ID ELSE U.ID END FROM T LEFT JOIN U ON T.ID = U.ID"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "CASE WHEN T.ID = 1 THEN U.ID ELSE U.ID END"))
	if fact.Nullability == NotNullable {
		t.Fatalf("Nullability = %v, want not NotNullable (every branch is U.ID, outer-joined)", fact.Nullability)
	}
}

// TestDiagnosticExpressionInnerJoinedNotNullColumnStaysNotNullable proves
// C3's downgrade is specific to an outer join: an ordinary (INNER) JOIN
// never puts either side on a nullable side, so a NOT NULL column read
// through one must still report NotNullable.
func TestDiagnosticExpressionInnerJoinedNotNullColumnStaysNotNullable(t *testing.T) {
	text := "SELECT U.ID FROM T JOIN U ON T.ID = U.ID"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "U.ID"))
	if fact.Nullability != NotNullable {
		t.Fatalf("Nullability = %v, want NotNullable (an INNER JOIN puts neither side on a nullable side)", fact.Nullability)
	}
}

func TestDiagnosticExpressionCoalesceAllNullableIsNullable(t *testing.T) {
	text := "SELECT COALESCE(V, V) FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "COALESCE(V, V)"))
	if fact.Nullability != Nullable {
		t.Fatalf("Nullability = %v, want Nullable (every argument is individually Nullable)", fact.Nullability)
	}
}

// TestDiagnosticExpressionBudgetBoundary is the required "nested
// expressions at the budget boundary" fixture: an expression nested exactly
// maxExpressionDepth levels deep must still resolve, and one level past
// must report unknown rather than continuing to recurse.
func TestDiagnosticExpressionBudgetBoundary(t *testing.T) {
	build := func(n int) string {
		expr := "1"
		for i := 0; i < n; i++ {
			expr = "1+(" + expr + ")"
		}
		return expr
	}

	atLimit := build(maxExpressionDepth)
	text := "SELECT " + atLimit + " FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, atLimit))
	if fact.Value == nil {
		t.Fatalf("expression nested exactly at maxExpressionDepth (%d) should still resolve, got unknown", maxExpressionDepth)
	}
	want := big.NewRat(int64(maxExpressionDepth+1), 1)
	requireRatEqual(t, fact.Value, want)

	// One level past the limit is the actual boundary this budget is
	// supposed to enforce -- asserting only a much deeper case (as an
	// earlier version of this test did) would not catch an off-by-several
	// bug in the boundary check itself.
	oneOverLimit := build(maxExpressionDepth + 1)
	text2 := "SELECT " + oneOverLimit + " FROM T"
	_, m2 := buildModel(t, text2, newDiagnosticFixtureCatalog())
	fact2 := m2.expressionFact(spanOf(t, text2, oneOverLimit))
	if fact2.Value != nil {
		t.Fatalf("expression nested one level past maxExpressionDepth should report unknown, got Value = %s", fact2.Value.RatString())
	}

	overLimit := build(maxExpressionDepth + 10)
	text3 := "SELECT " + overLimit + " FROM T"
	_, m3 := buildModel(t, text3, newDiagnosticFixtureCatalog())
	fact3 := m3.expressionFact(spanOf(t, text3, overLimit))
	if fact3.Value != nil {
		t.Fatalf("expression nested well past maxExpressionDepth should report unknown, got Value = %s", fact3.Value.RatString())
	}
}

// TestDiagnosticExpressionUnresolvedIdentifierIsUnknown proves an
// identifier qualified by something that is not a valid relation alias in
// scope (X, here, is not T's alias and not any other FROM-clause entry)
// reports unknown, rather than accidentally matching T's own columns
// through some unqualified fallback path. An earlier version of this test
// put the name inside a "--" comment, which the lexer strips entirely
// before this package ever sees it as a lexeme -- the assertion passed for
// the wrong reason (zero items in the span, not identifierFact/Resolve
// actually failing to resolve a real identifier).
func TestDiagnosticExpressionUnresolvedIdentifierIsUnknown(t *testing.T) {
	text := "SELECT X.NOSUCH FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "X.NOSUCH"))
	if fact.Value != nil || fact.Type.Family != familyUnknown || fact.Nullability != NullUnknown {
		t.Fatalf("fact = %+v, want the all-zero unknown fact", fact)
	}
}

// TestDiagnosticExpressionRelationColumnFactsRespectsDDLInvalidation is I1's
// required regression: relationColumnFacts must refuse stale catalog column
// facts the same way resolveRelationOutput already does, once an earlier
// CREATE/ALTER/DROP in the same document has invalidated the relation's
// identity as of this reference's position -- mirroring
// TestDiagnosticModelRelationOutputRespectsDDLInvalidation's own DDL setup.
func TestDiagnosticExpressionRelationColumnFactsRespectsDDLInvalidation(t *testing.T) {
	text := "CREATE TABLE T (ID INTEGER); SELECT ID FROM T;"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOfOccurrence(t, text, "ID", 1))
	if fact.Value != nil || fact.Type.Family != familyUnknown || fact.Nullability != NullUnknown {
		t.Fatalf("fact = %+v, want the all-zero unknown fact: T's catalog columns are stale after an earlier CREATE TABLE T in this same document", fact)
	}
}

func TestDiagnosticExpressionUnresolvedColumnIsUnknown(t *testing.T) {
	text := "SELECT NOSUCHCOLUMN FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "NOSUCHCOLUMN"))
	if fact.Value != nil || fact.Type.Family != familyUnknown {
		t.Fatalf("fact = %+v, want unknown for a column absent from the catalog", fact)
	}
}

// TestDiagnosticExpressionBindParameterNamedIsUnknown is the required
// fixture: a client bind parameter must always report unknown, regardless
// of its name or any value a user might currently have bound to it --
// never inferred from the parameter's name alone.
func TestDiagnosticExpressionBindParameterNamedIsUnknown(t *testing.T) {
	text := "SELECT ID FROM T WHERE ID = :SMALLINT_LOOKING_NAME"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, ":SMALLINT_LOOKING_NAME"))
	if fact.Value != nil || fact.Type.Family != familyUnknown || fact.Nullability != NullUnknown {
		t.Fatalf("fact = %+v, want the all-zero unknown fact for an unbound client parameter", fact)
	}
}

// TestDiagnosticExpressionBindParameterMatchingRealColumnNameIsUnknown is
// C2's required fixture: a client bind parameter whose name happens to
// match a real column in scope must still always report unknown. Before
// this fix, the colon-form path in identifierFact fell through to
// resolution.Role == Column whenever the binder resolved the name that way
// (which it can, since ":V" and ":ID" share a name with T's real columns),
// producing a fabricated fact for what is actually a client-supplied
// runtime value never provably related to that column.
func TestDiagnosticExpressionBindParameterMatchingRealColumnNameIsUnknown(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		marker string
	}{
		{"matches nullable column V", "SELECT ID FROM T WHERE V = :V", ":V"},
		{"matches not-null column ID", "SELECT ID FROM T WHERE ID = :ID", ":ID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, m := buildModel(t, tt.text, newDiagnosticFixtureCatalog())
			fact := m.expressionFact(spanOf(t, tt.text, tt.marker))
			if fact.Value != nil || fact.Type.Family != familyUnknown || fact.Nullability != NullUnknown {
				t.Fatalf("fact = %+v, want the all-zero unknown fact for a bind parameter matching a real column's name", fact)
			}
		})
	}
}

func TestDiagnosticExpressionBindParameterQuestionMarkIsUnknown(t *testing.T) {
	text := "SELECT ID FROM T WHERE ID = ?"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "?"))
	if fact.Value != nil || fact.Type.Family != familyUnknown || fact.Nullability != NullUnknown {
		t.Fatalf("fact = %+v, want the all-zero unknown fact for a ? placeholder", fact)
	}
}

func TestDiagnosticExpressionUnknownFunctionCallIsUnknown(t *testing.T) {
	text := "SELECT SOME_UDF(ID) FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "SOME_UDF(ID)"))
	if fact.Value != nil || fact.Type.Family != familyUnknown {
		t.Fatalf("fact = %+v, want unknown for an unrecognized call", fact)
	}
}

// TestDiagnosticExpressionDynamicSQLNoPanicReportsUnknown proves EXECUTE
// STATEMENT (dynamic SQL) never panics and reports unknown. This task does
// not attempt to parse into dynamic SQL at all -- there is no
// EXECUTE-STATEMENT-specific recognition anywhere in this file -- so this
// is honestly only exercising the same generic "shape not recognized by any
// of computeExpressionFact's dispatch branches" fallback every other
// unrecognized-shape case falls through to, not a dedicated dynamic-SQL
// rule. Renamed from the earlier "...IsUnknown" name, which implied a more
// specific guarantee than the code actually provides.
func TestDiagnosticExpressionDynamicSQLNoPanicReportsUnknown(t *testing.T) {
	text := "EXECUTE STATEMENT 'SELECT 1 FROM RDB$DATABASE'"
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "EXECUTE STATEMENT 'SELECT 1 FROM RDB$DATABASE'"))
	if fact.Value != nil || fact.Type.Family != familyUnknown {
		t.Fatalf("fact = %+v, want unknown for dynamic SQL", fact)
	}
}

// TestDiagnosticExpressionMalformedParenthesesNoPanic proves malformed/
// unbalanced parenthesization returns unknown gracefully instead of
// panicking.
func TestDiagnosticExpressionMalformedParenthesesNoPanic(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"unclosed paren", "SELECT (1 + 1 FROM T"},
		{"stray close paren", "SELECT 1 + 1) FROM T"},
		{"nested unclosed", "SELECT ((1 + 1) FROM T"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked: %v", r)
				}
			}()
			_, m := buildModel(t, tt.text, newDiagnosticFixtureCatalog())
			fact := m.expressionFact(Span{Start: 0, End: len(tt.text)})
			if fact.Value != nil {
				t.Fatalf("fact = %+v, want unknown for malformed parentheses", fact)
			}
		})
	}
}

// TestDiagnosticExpressionMemoization proves expressionFact is memoized:
// repeated calls for the same span return equal facts without recomputing
// (observed indirectly, since the cache field is unexported -- this test
// documents the contract by calling twice and requiring identical results,
// which would also catch a cache returning stale/wrong data on a hit).
func TestDiagnosticExpressionMemoization(t *testing.T) {
	text := "SELECT 2 + 3 FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	span := spanOf(t, text, "2 + 3")
	first := m.expressionFact(span)
	second := m.expressionFact(span)
	requireRatEqual(t, first.Value, second.Value)
	if len(m.exprFactCache) != 1 {
		t.Fatalf("exprFactCache has %d entries, want 1 after two calls for the same span", len(m.exprFactCache))
	}
}
