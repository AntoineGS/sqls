package sqlsymbol

import "testing"

// --- brief's verbatim fixture ---

func TestDiagnosticNullComparison(t *testing.T) {
	requireCodeCount(t, "SELECT ID FROM T WHERE V = NULL;", nil, "interbase-null-comparison", 1)
	requireCodeCount(t, "SELECT ID FROM T WHERE V IS NULL;", nil, "interbase-null-comparison", 0)
	requireCodeCount(t, "UPDATE T SET V = NULL;", nil, "interbase-null-comparison", 0)
}

// --- clause coverage: WHERE, JOIN ON, HAVING, CASE WHEN, procedural IF ---

func TestDiagnosticNullComparisonJoinOn(t *testing.T) {
	requireCodeCount(t, "SELECT ID FROM T JOIN U ON T.ID = NULL;", nil, "interbase-null-comparison", 1)
	requireCodeCount(t, "SELECT ID FROM T JOIN U ON T.ID = U.ID;", nil, "interbase-null-comparison", 0)
}

func TestDiagnosticNullComparisonHaving(t *testing.T) {
	requireCodeCount(t, "SELECT ID FROM T GROUP BY ID HAVING ID = NULL;", nil, "interbase-null-comparison", 1)
	requireCodeCount(t, "SELECT ID FROM T GROUP BY ID HAVING COUNT(*) > 1;", nil, "interbase-null-comparison", 0)
}

func TestDiagnosticNullComparisonCaseWhen(t *testing.T) {
	requireCodeCount(t, "SELECT CASE WHEN V = NULL THEN 1 ELSE 0 END FROM T;", nil, "interbase-null-comparison", 1)
	requireCodeCount(t, "SELECT CASE WHEN V IS NULL THEN 1 ELSE 0 END FROM T;", nil, "interbase-null-comparison", 0)
}

func TestDiagnosticNullComparisonProceduralIf(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE V INTEGER; BEGIN IF (V = NULL) THEN V = 1; END",
		nil, "interbase-null-comparison", 1)
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE V INTEGER; BEGIN IF (V IS NULL) THEN V = 1; END",
		nil, "interbase-null-comparison", 0)
}

// --- assignments are never comparisons ---

func TestDiagnosticNullComparisonProceduralAssignmentNotFlagged(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE V INTEGER; BEGIN V = NULL; END",
		nil, "interbase-null-comparison", 0)
}

func TestDiagnosticNullComparisonUpdateSetNotFlaggedButWhereIs(t *testing.T) {
	requireCodeCount(t, "UPDATE T SET V = NULL WHERE ID = NULL;", nil, "interbase-null-comparison", 1)
}

// --- operator direction and variants ---

func TestDiagnosticNullComparisonReversedOperands(t *testing.T) {
	requireCodeCount(t, "SELECT ID FROM T WHERE NULL = V;", nil, "interbase-null-comparison", 1)
}

func TestDiagnosticNullComparisonNotEqualOperators(t *testing.T) {
	requireCodeCount(t, "SELECT ID FROM T WHERE V <> NULL;", nil, "interbase-null-comparison", 1)
	requireCodeCount(t, "SELECT ID FROM T WHERE V != NULL;", nil, "interbase-null-comparison", 1)
}

// --- parenthesized operands ---

func TestDiagnosticNullComparisonParenthesized(t *testing.T) {
	requireCodeCount(t, "SELECT ID FROM T WHERE (V) = NULL;", nil, "interbase-null-comparison", 1)
	requireCodeCount(t, "SELECT ID FROM T WHERE V = (NULL);", nil, "interbase-null-comparison", 1)
}

// --- incomplete/malformed expressions are withheld ---

func TestDiagnosticNullComparisonIncompleteExpressionWithheld(t *testing.T) {
	requireCodeCount(t, "SELECT ID FROM T WHERE V = NULL AND;", nil, "interbase-null-comparison", 0)
}

// --- comments and string literals never trigger a match ---

func TestDiagnosticNullComparisonCommentsAndStringsIgnored(t *testing.T) {
	requireCodeCount(t, "SELECT ID FROM T WHERE V = 'NULL = X';", nil, "interbase-null-comparison", 0)
	requireCodeCount(t, "SELECT ID FROM T WHERE /* V = NULL */ V = 1;", nil, "interbase-null-comparison", 0)
}

// --- NULL appearing inside a larger expression is never a bare operand ---

func TestDiagnosticNullComparisonFunctionCallArgumentNotFlagged(t *testing.T) {
	requireCodeCount(t, "SELECT ID FROM T WHERE COALESCE(V, NULL) = 1;", nil, "interbase-null-comparison", 0)
	requireCodeCount(t, "SELECT ID FROM T WHERE F(NULL) = V;", nil, "interbase-null-comparison", 0)
}

func TestDiagnosticNullComparisonNullifKeywordNotConfusedWithNull(t *testing.T) {
	requireCodeCount(t, "SELECT ID FROM T WHERE NULLIF(V, 1) = 2;", nil, "interbase-null-comparison", 0)
}

func TestDiagnosticNullComparisonArithmeticWithNullNotFlagged(t *testing.T) {
	requireCodeCount(t, "SELECT ID FROM T WHERE V = NULL + 1;", nil, "interbase-null-comparison", 0)
}

// --- multiple conjuncts/disjuncts are each inspected independently ---

func TestDiagnosticNullComparisonWithinConjunction(t *testing.T) {
	requireCodeCount(t, "SELECT ID FROM T WHERE A = 1 AND B = NULL;", nil, "interbase-null-comparison", 1)
}

func TestDiagnosticNullComparisonWithinDisjunction(t *testing.T) {
	requireCodeCount(t, "SELECT ID FROM T WHERE A = NULL OR B = 1;", nil, "interbase-null-comparison", 1)
}

// --- a WHERE nested inside a subquery has no boundary keyword or semicolon
// of its own; its clause naturally ends at the enclosing parentheses. ---

func TestDiagnosticNullComparisonNestedSubqueryWhere(t *testing.T) {
	requireCodeCount(t,
		"SELECT ID FROM T WHERE EXISTS (SELECT 1 FROM U WHERE U.V = NULL);",
		nil, "interbase-null-comparison", 1)
}

// --- FOR SELECT ... INTO :var DO loops: INTO/DO must end a WHERE/HAVING/ON
// clause body, or the loop body's own assignment is misread as part of the
// clause's comparison (fix round 1, Critical). ---

func TestDiagnosticNullComparisonForSelectIntoDoNotSwallowedByWhere(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE R INTEGER; BEGIN FOR SELECT A FROM T WHERE ID > 0 INTO :R DO BEGIN R = NULL; SUSPEND; END END",
		nil, "interbase-null-comparison", 0)
}

func TestDiagnosticNullComparisonForSelectIntoDoIsNotNullNotSwallowed(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE X INTEGER; DECLARE VARIABLE V INTEGER; BEGIN FOR SELECT A FROM T WHERE ID IS NOT NULL INTO :X DO BEGIN V = NULL; END END",
		nil, "interbase-null-comparison", 0)
}

func TestDiagnosticNullComparisonForSelectIntoDoSingleStatementBodyNotSwallowed(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE X INTEGER; DECLARE VARIABLE V INTEGER; BEGIN FOR SELECT A FROM T WHERE ID = 1 AND B > 0 INTO :X DO V = NULL; END",
		nil, "interbase-null-comparison", 0)
}

func TestDiagnosticNullComparisonForSelectIntoDoOnClauseNotSwallowed(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE X INTEGER; DECLARE VARIABLE V INTEGER; BEGIN FOR SELECT A FROM T JOIN U ON T.ID > U.ID INTO :X DO V = NULL; END",
		nil, "interbase-null-comparison", 0)
}

func TestDiagnosticNullComparisonForSelectIntoDoHavingClauseNotSwallowed(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE X INTEGER; DECLARE VARIABLE V INTEGER; BEGIN FOR SELECT A FROM T GROUP BY A HAVING COUNT(*) > 1 INTO :X DO V = NULL; END",
		nil, "interbase-null-comparison", 0)
}

// --- missed detections from the same root cause: a clause body clipped too
// early by an unrecognized boundary (fix round 1, Important). ---

func TestDiagnosticNullComparisonJoinQualifierEndsOnClause(t *testing.T) {
	requireCodeCount(t,
		"SELECT ID FROM T JOIN U ON T.ID = NULL LEFT JOIN W ON U.ID = W.ID;",
		nil, "interbase-null-comparison", 1)
}

func TestDiagnosticNullComparisonForUpdateEndsWhereClause(t *testing.T) {
	requireCodeCount(t, "SELECT ID FROM T WHERE V = NULL FOR UPDATE;", nil, "interbase-null-comparison", 1)
}

// --- LEFT/RIGHT function calls must not be confused with join qualifiers ---

func TestDiagnosticNullComparisonLeftFunctionNotBoundary(t *testing.T) {
	requireCodeCount(t,
		"SELECT ID FROM T JOIN U ON T.NAME = LEFT(U.NAME, 3);",
		nil, "interbase-null-comparison", 0)
}

func TestDiagnosticNullComparisonRightFunctionNotBoundary(t *testing.T) {
	requireCodeCount(t,
		"SELECT ID FROM T JOIN U ON T.NAME = RIGHT(U.NAME, 3);",
		nil, "interbase-null-comparison", 0)
}

// --- span covers the whole comparison expression ---

func TestDiagnosticNullComparisonSpan(t *testing.T) {
	text := "SELECT ID FROM T WHERE V = NULL;"
	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	findings := a.Diagnostics(nil)
	var found *Finding
	for i, f := range findings {
		if f.Code == "interbase-null-comparison" {
			found = &findings[i]
		}
	}
	if found == nil {
		t.Fatal("expected a finding")
	}
	if got := text[found.Span.Start:found.Span.End]; got != "V = NULL" {
		t.Fatalf("span = %q, want %q", got, "V = NULL")
	}
	if found.Severity != 2 {
		t.Fatalf("severity = %d, want 2", found.Severity)
	}
	wantMessage := "comparison with NULL yields UNKNOWN; use IS NULL/IS NOT NULL if testing nullness"
	if found.Message != wantMessage {
		t.Fatalf("message = %q, want %q", found.Message, wantMessage)
	}
}
