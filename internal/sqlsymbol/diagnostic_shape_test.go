package sqlsymbol

import "testing"

// fixtureWithProcedure returns a copy of the standard diagnostic fixture
// catalog with name additionally bound to fact, for tests needing a
// procedure shape the shared fixture (P: exactly two INTEGER inputs, one
// INTEGER output, both counts known) does not cover.
func fixtureWithProcedure(name string, fact ProcedureFact) *diagnosticFixtureCatalog {
	c := newDiagnosticFixtureCatalog()
	c.procedures[name] = fact
	return c
}

// fixtureProceduresLoading returns a catalog whose procedure namespace has
// not finished loading (ProcedureInfo reports Unknown, not Missing, for
// every name), simulating the state before metadata arrives.
func fixtureProceduresLoading() *diagnosticFixtureCatalog {
	c := newDiagnosticFixtureCatalog()
	c.procedures = map[string]ProcedureFact{}
	c.proceduresKnown = false
	return c
}

// --- brief's verbatim fixture ---

func TestDiagnosticTargetCount(t *testing.T) {
	requireCodeCount(t, "INSERT INTO T (ID, NAME) VALUES (1);",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 1)
	requireCodeCount(t, "INSERT INTO T (ID, NAME) VALUES (1, 'x');",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

// --- INSERT ... VALUES ---

func TestDiagnosticTargetCountInsertValuesCommaInsideFunctionCallCountsAsOneItem(t *testing.T) {
	// COALESCE(NAME, 'x') has an internal comma but is one VALUES element;
	// splitTopLevel must not miscount it as two.
	requireCodeCount(t, "INSERT INTO T (ID, NAME) VALUES (1, COALESCE(NAME, 'x'));",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

// --- Ruling I2: an omitted INSERT column list is always withheld ---
// Proving the insertable column order requires knowing whether computed
// columns are excluded from an implicit INSERT's target list, which the
// catalog adapter cannot signal; this must never be guessed at, regardless
// of value count or relation-column-list completeness.

func TestDiagnosticTargetCountInsertOmittedColumnListAlwaysWithheld(t *testing.T) {
	c := &diagnosticFixtureCatalog{
		relationsKnown:  false,
		proceduresKnown: true,
		domainsKnown:    true,
		relations:       map[string]RelationFact{},
		procedures:      map[string]ProcedureFact{},
		domains:         map[string]DomainFact{},
		keys:            map[string][][]string{},
	}
	requireCodeCount(t, "INSERT INTO T VALUES (1, 2);", c, "interbase-target-count", 0)
	// T has 4 known columns (ID, V, NAME, N); even so, neither a
	// clearly-short nor a matching-count implicit VALUES list is checked.
	requireCodeCount(t, "INSERT INTO T VALUES (1, 2);",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
	requireCodeCount(t, "INSERT INTO T VALUES (1, 2, 'x', 3);",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

// --- INSERT ... SELECT ---

func TestDiagnosticTargetCountInsertSelectMismatch(t *testing.T) {
	requireCodeCount(t, "INSERT INTO T (ID, NAME) SELECT ID, V, N FROM T;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 1)
	requireCodeCount(t, "INSERT INTO T (ID, NAME) SELECT ID, V FROM T;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

func TestDiagnosticTargetCountInsertSelectStarKnownShape(t *testing.T) {
	// T has 4 known columns, declared list has 2: a provable mismatch.
	requireCodeCount(t, "INSERT INTO T (ID, NAME) SELECT * FROM T;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 1)
}

func TestDiagnosticTargetCountInsertSelectStarUnknownShapeWithheld(t *testing.T) {
	// SELECT * over two relations is an unprovable projection: withhold
	// rather than guessing a mismatch.
	requireCodeCount(t, "INSERT INTO T (ID, NAME) SELECT * FROM T, U;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

// --- SELECT ... INTO ---

func TestDiagnosticTargetCountSelectIntoMismatch(t *testing.T) {
	requireCodeCount(t, "SELECT ID, V INTO :x FROM T;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 1)
	requireCodeCount(t, "SELECT ID, V INTO :x, :y FROM T;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

func TestDiagnosticTargetCountSelectIntoStarUnknownShapeWithheld(t *testing.T) {
	requireCodeCount(t, "SELECT * INTO :x FROM T, U;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

// --- UNION projection counts ---

func TestDiagnosticTargetCountUnionMismatch(t *testing.T) {
	requireCodeCount(t, "SELECT ID, V FROM T UNION SELECT ID FROM U;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 1)
	requireCodeCount(t, "SELECT ID FROM T UNION SELECT ID FROM U;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

func TestDiagnosticTargetCountUnionStarUnknownShapeWithheld(t *testing.T) {
	// The first arm's star is unprovable (two relations, no qualifier); a
	// real mismatch against the second arm must not be guessed at.
	requireCodeCount(t, "SELECT * FROM T, U UNION SELECT ID FROM U;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

func TestDiagnosticTargetCountUnionStarKnownShapeMismatch(t *testing.T) {
	requireCodeCount(t, "SELECT * FROM T UNION SELECT * FROM U;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 1)
}

// --- malformed statements: hard rule, zero findings ---

func TestDiagnosticTargetCountMalformedStatementsProduceNoFindings(t *testing.T) {
	requireCodeCount(t, "INSERT INTO T (ID, NAME VALUES (1);",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
	requireCodeCount(t, "UPDATE T SET V = (1;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
	requireCodeCount(t, "SELECT ID INTO :x FROM (T;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

func TestDiagnosticProcedureArityMalformedStatementProducesNoFindings(t *testing.T) {
	requireCodeCount(t, "EXECUTE PROCEDURE P(1;",
		newDiagnosticFixtureCatalog(), "interbase-procedure-arity", 0)
}

// --- EXECUTE PROCEDURE input arity ---

func TestDiagnosticProcedureArityExecuteTooFewInputs(t *testing.T) {
	requireCodeCount(t, "EXECUTE PROCEDURE P(1);",
		newDiagnosticFixtureCatalog(), "interbase-procedure-arity", 1)
}

func TestDiagnosticProcedureArityExecuteTooManyInputs(t *testing.T) {
	requireCodeCount(t, "EXECUTE PROCEDURE P(1, 2, 3);",
		newDiagnosticFixtureCatalog(), "interbase-procedure-arity", 1)
}

func TestDiagnosticProcedureArityExecuteExactMatch(t *testing.T) {
	requireCodeCount(t, "EXECUTE PROCEDURE P(1, 2);",
		newDiagnosticFixtureCatalog(), "interbase-procedure-arity", 0)
}

func TestDiagnosticProcedureArityArgumentFreeKnownProcedure(t *testing.T) {
	c := fixtureWithProcedure("Z", ProcedureFact{InputsKnown: true, OutputsKnown: true, MinInputs: 0, MinInputsKnown: true})
	requireCodeCount(t, "EXECUTE PROCEDURE Z;", c, "interbase-procedure-arity", 0)
	requireCodeCount(t, "EXECUTE PROCEDURE Z();", c, "interbase-procedure-arity", 0)
}

// --- minimum arity unknown: too few stays quiet, too many is diagnosed ---
// (This mirrors the real handler adapter: any catalog-sourced procedure with
// >0 parameters reports InputsKnown=true but MinInputsKnown=false, since the
// catalog cannot see trailing default parameters.)

func TestDiagnosticProcedureArityMinimumUnknownTooFewIsQuiet(t *testing.T) {
	c := fixtureWithProcedure("P2", ProcedureFact{
		Inputs:      []ColumnFact{{Name: "IN1", Type: "INTEGER"}, {Name: "IN2", Type: "INTEGER"}},
		InputsKnown: true,
	})
	requireCodeCount(t, "EXECUTE PROCEDURE P2(1);", c, "interbase-procedure-arity", 0)
}

func TestDiagnosticProcedureArityMinimumUnknownTooManyIsDiagnosed(t *testing.T) {
	c := fixtureWithProcedure("P2", ProcedureFact{
		Inputs:      []ColumnFact{{Name: "IN1", Type: "INTEGER"}, {Name: "IN2", Type: "INTEGER"}},
		InputsKnown: true,
	})
	requireCodeCount(t, "EXECUTE PROCEDURE P2(1, 2, 3);", c, "interbase-procedure-arity", 1)
}

// --- Fix round 1: C1 -- the InterBase-standard unparenthesized EXECUTE
// PROCEDURE syntax ("EXECUTE PROCEDURE name [param [, param ...]]
// [RETURNING_VALUES ...]") is an alternative to the parenthesized form,
// not a rarer variant, and must be counted correctly, not treated as a
// zero-argument call.

func TestDiagnosticProcedureArityUnparenthesizedArgs(t *testing.T) {
	requireCodeCount(t, "EXECUTE PROCEDURE P :a, :b;",
		newDiagnosticFixtureCatalog(), "interbase-procedure-arity", 0)
}

func TestDiagnosticProcedureArityUnparenthesizedArgsWithReturningValues(t *testing.T) {
	// Fixture P has exactly one known output (OUT1), so the
	// RETURNING_VALUES list here supplies one target to match.
	requireCodeCount(t, "EXECUTE PROCEDURE P 1, 2 RETURNING_VALUES :a;",
		newDiagnosticFixtureCatalog(), "interbase-procedure-arity", 0)
	requireCodeCount(t, "EXECUTE PROCEDURE P 1, 2 RETURNING_VALUES :a;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

func TestDiagnosticProcedureArityUnparenthesizedArgsInsideProcedureBody(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE X AS DECLARE VARIABLE A INTEGER; BEGIN EXECUTE PROCEDURE P :a, 2 RETURNING_VALUES :a; END;",
		newDiagnosticFixtureCatalog(), "interbase-procedure-arity", 0)
}

func TestDiagnosticProcedureArityLeadingParenthesizedExpressionArg(t *testing.T) {
	// "(1) + 1" is one bare argument (a parenthesized subexpression used
	// in a larger arithmetic expression), followed by a second bare
	// argument "2" -- this is the unparenthesized call form, not the
	// classic single-enclosing-parens form, even though it starts with "(".
	requireCodeCount(t, "EXECUTE PROCEDURE P (1) + 1, 2;",
		newDiagnosticFixtureCatalog(), "interbase-procedure-arity", 0)
}

func TestDiagnosticProcedureArityUnparenthesizedTooFewArgs(t *testing.T) {
	requireCodeCount(t, "EXECUTE PROCEDURE P :a;",
		newDiagnosticFixtureCatalog(), "interbase-procedure-arity", 1)
}

func TestDiagnosticProcedureArityUnparenthesizedTooManyArgs(t *testing.T) {
	requireCodeCount(t, "EXECUTE PROCEDURE P :a, :b, :c;",
		newDiagnosticFixtureCatalog(), "interbase-procedure-arity", 1)
}

// --- Fix round 1: I1 -- a trigger's SELECT ... INTO must produce exactly
// one target-count finding, not two, even when a DECLARE VARIABLE line
// before BEGIN causes buildContexts' generic per-token state machine to
// reset just before the trigger body (see diagnostic_model.go's
// excludeQueriesWithinTriggerBodies for the root cause).

func TestDiagnosticTargetCountTriggerSelectIntoNotDuplicated(t *testing.T) {
	requireCodeCount(t,
		"CREATE TRIGGER TR FOR T BEFORE INSERT AS DECLARE VARIABLE X INTEGER; BEGIN SELECT ID, V FROM T INTO :a; END;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 1)
}

// --- Fix round 1: I4 -- a parenthesized RETURNING_VALUES target group is
// withheld entirely: it is unverified whether InterBase accepts this form.

func TestDiagnosticTargetCountReturningValuesParenthesizedGroupWithheld(t *testing.T) {
	requireCodeCount(t, "EXECUTE PROCEDURE P(1, 2) RETURNING_VALUES (:a, :b);",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

// --- metadata-arrival transition ---

func TestDiagnosticProcedureArityMetadataArrivalTransition(t *testing.T) {
	requireCodeCount(t, "EXECUTE PROCEDURE P(1);",
		fixtureProceduresLoading(), "interbase-procedure-arity", 0)
	requireCodeCount(t, "EXECUTE PROCEDURE P(1);",
		newDiagnosticFixtureCatalog(), "interbase-procedure-arity", 1)
}

// --- selectable procedure in FROM: input arity ---

func TestDiagnosticProcedureAritySelectableProcedureInFrom(t *testing.T) {
	requireCodeCount(t, "SELECT X.OUT1 FROM P(1) X;",
		newDiagnosticFixtureCatalog(), "interbase-procedure-arity", 1)
	requireCodeCount(t, "SELECT X.OUT1 FROM P(1, 2) X;",
		newDiagnosticFixtureCatalog(), "interbase-procedure-arity", 0)
}

// --- EXECUTE PROCEDURE ... RETURNING_VALUES output count ---

func TestDiagnosticTargetCountExecuteProcedureReturningValuesMismatch(t *testing.T) {
	requireCodeCount(t, "EXECUTE PROCEDURE P(1, 2) RETURNING_VALUES :x, :y;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 1)
	requireCodeCount(t, "EXECUTE PROCEDURE P(1, 2) RETURNING_VALUES :x;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

func TestDiagnosticTargetCountStandaloneExecuteProcedureNeedsNoOutputs(t *testing.T) {
	// No RETURNING_VALUES/INTO: this syntax form does not require outputs
	// at all, so no target-count finding should ever fire for it.
	requireCodeCount(t, "EXECUTE PROCEDURE P(1, 2);",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

// --- false-positive resistance: nested commas must never be miscounted ---

func TestDiagnosticTargetCountInsertValuesCaseExpressionCommaNotMiscounted(t *testing.T) {
	// A CASE expression's internal commas (inside IN(...), or via a
	// function call in a branch) must not split a single VALUES element.
	requireCodeCount(t, "INSERT INTO T (ID, NAME) VALUES (1, CASE WHEN ID IN (1, 2) THEN 'a' ELSE 'b' END);",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

func TestDiagnosticTargetCountInsertSelectSubqueryCommaNotMiscounted(t *testing.T) {
	// A scalar subquery projection containing its own comma-separated
	// FROM list must not be mistaken for extra top-level projections.
	requireCodeCount(t, "INSERT INTO T (ID, NAME) SELECT ID, (SELECT NAME FROM T, U WHERE T.ID = U.ID) FROM T;",
		newDiagnosticFixtureCatalog(), "interbase-target-count", 0)
}

func TestDiagnosticProcedureArityNestedFunctionCallCommaNotMiscounted(t *testing.T) {
	// SUBSTRING's internal comma-free FROM/FOR syntax and a nested
	// function call's own commas must not inflate the argument count.
	requireCodeCount(t, "EXECUTE PROCEDURE P(COALESCE(1, 2), 2);",
		newDiagnosticFixtureCatalog(), "interbase-procedure-arity", 0)
}

func TestDiagnosticProcedureArityStringLiteralWithCommaNotMiscounted(t *testing.T) {
	// A string literal argument containing a comma character must not be
	// mistaken for an argument separator.
	requireCodeCount(t, "EXECUTE PROCEDURE P('a, b', 2);",
		newDiagnosticFixtureCatalog(), "interbase-procedure-arity", 0)
}
