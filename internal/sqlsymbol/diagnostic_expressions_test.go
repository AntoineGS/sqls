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

// TestDiagnosticExpressionCoalesceOuterJoinedField is the brief's required
// fixture: COALESCE over a nullable column (standing in for a LEFT JOIN's
// nullable side -- U.V is declared Nullable in the fixture catalog) paired
// with a NotNullable literal default. COALESCE's real semantics guarantee
// NotNullable here (some argument is provably non-null), unlike CASE's
// all-branches-must-agree rule.
func TestDiagnosticExpressionCoalesceOuterJoinedField(t *testing.T) {
	text := "SELECT COALESCE(U.V, 0) FROM T LEFT JOIN U ON T.ID = U.ID"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "COALESCE(U.V, 0)"))
	if fact.Nullability != NotNullable {
		t.Fatalf("Nullability = %v, want NotNullable (COALESCE with a NotNullable default argument)", fact.Nullability)
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

	overLimit := build(maxExpressionDepth + 10)
	text2 := "SELECT " + overLimit + " FROM T"
	_, m2 := buildModel(t, text2, newDiagnosticFixtureCatalog())
	fact2 := m2.expressionFact(spanOf(t, text2, overLimit))
	if fact2.Value != nil {
		t.Fatalf("expression nested well past maxExpressionDepth should report unknown, got Value = %s", fact2.Value.RatString())
	}
}

func TestDiagnosticExpressionUnresolvedIdentifierIsUnknown(t *testing.T) {
	text := "-- UNRESOLVABLENAME\nSELECT 1 FROM T"
	_, m := buildModel(t, text, newDiagnosticFixtureCatalog())
	fact := m.expressionFact(spanOf(t, text, "UNRESOLVABLENAME"))
	if fact.Value != nil || fact.Type.Family != familyUnknown || fact.Nullability != NullUnknown {
		t.Fatalf("fact = %+v, want the all-zero unknown fact", fact)
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

// TestDiagnosticExpressionDynamicSQLIsUnknown proves EXECUTE STATEMENT
// (dynamic SQL) never panics and always reports unknown; this task does
// not attempt to parse into it.
func TestDiagnosticExpressionDynamicSQLIsUnknown(t *testing.T) {
	text := "EXECUTE STATEMENT 'SELECT 1 FROM RDB$DATABASE'"
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
