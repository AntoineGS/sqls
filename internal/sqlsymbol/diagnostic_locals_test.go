package sqlsymbol

import (
	"strings"
	"testing"
)

func TestDiagnosticUnknownLocal(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE V_TOTAL INTEGER; BEGIN V_TOTLA = 1; END",
		nil, "interbase-unknown-variable", 1)
}

// Fix round 1 (C1): general expression-position reads (an IF/WHILE
// condition, the right-hand side of an assignment) are deliberately never
// scanned for unknown-variable purposes -- only a genuine assignment lvalue
// or an INTO target is. See diagnostic_locals.go's codeUnknownVariable doc
// comment. This replaces the pre-fix-round-1 TestDiagnosticUnknownLocalRead,
// which incorrectly asserted that a bare condition read was flagged.
func TestDiagnosticUnknownLocalConditionReadsNotFlagged(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE V_TOTAL INTEGER; BEGIN IF (V_TOTLA > 0) THEN V_TOTAL = 1; END",
		nil, codeUnknownVariable, 0)
}

func TestDiagnosticUnknownLocalKnownNameNotFlagged(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE V_TOTAL INTEGER; BEGIN V_TOTAL = 1; END",
		nil, codeUnknownVariable, 0)
}

func TestDiagnosticUnknownLocalColonPrefixedSQLInputNotFlagged(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS BEGIN SELECT ID FROM T WHERE ID = :EXTERNAL_ID; END",
		nil, codeUnknownVariable, 0)
}

func TestDiagnosticUnknownLocalTopLevelParametersNotFlagged(t *testing.T) {
	requireCodeCount(t, "SELECT :id FROM T", nil, codeUnknownVariable, 0)
	requireCodeCount(t, "SELECT ? FROM T", nil, codeUnknownVariable, 0)
}

func TestDiagnosticUnknownLocalIntoTargetFlaggedWhenUndeclared(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q RETURNS (RESULT INTEGER) AS BEGIN SELECT ID FROM T INTO MISSING_VAR; END",
		nil, codeUnknownVariable, 1)
}

func TestDiagnosticUnknownLocalIntoTargetKnownNotFlagged(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q RETURNS (RESULT INTEGER) AS BEGIN SELECT ID FROM T INTO RESULT; END",
		nil, codeUnknownVariable, 0)
	requireCodeCount(t,
		"CREATE PROCEDURE Q RETURNS (RESULT INTEGER) AS BEGIN SELECT ID FROM T INTO :RESULT; END",
		nil, codeUnknownVariable, 0)
}

func TestDiagnosticUnknownLocalIntoTargetColonPrefixedUnknownFlagged(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q RETURNS (RESULT INTEGER) AS BEGIN SELECT ID FROM T INTO :MISSING_VAR; END",
		nil, codeUnknownVariable, 1)
}

func TestDiagnosticUnknownLocalQuotedNameNoncollision(t *testing.T) {
	requireCodeCount(t,
		`CREATE PROCEDURE Q AS DECLARE VARIABLE V_TOTAL INTEGER; BEGIN "v_total" = 1; END`,
		nil, codeUnknownVariable, 1)
}

func TestDiagnosticDuplicateLocalLocalDeclaration(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE DUP INTEGER; DECLARE VARIABLE DUP INTEGER; BEGIN END",
		nil, codeDuplicateDeclaration, 1)
}

func TestDiagnosticDuplicateLocalParameterDeclaration(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q (DUP INTEGER) AS DECLARE VARIABLE DUP INTEGER; BEGIN END",
		nil, codeDuplicateDeclaration, 1)
}

func TestDiagnosticUnknownLocalDoesNotCrossProcedures(t *testing.T) {
	text := "CREATE PROCEDURE P1 AS DECLARE VARIABLE V_TOTAL INTEGER; BEGIN V_TOTAL = 1; END;" +
		"CREATE PROCEDURE P2 AS DECLARE VARIABLE V_TOTAL INTEGER; BEGIN V_TOTAL = 2; END"
	requireCodeCount(t, text, nil, codeUnknownVariable, 0)
}

func TestDiagnosticUnknownLocalIgnoresCommentsAndLiterals(t *testing.T) {
	text := "CREATE PROCEDURE Q AS DECLARE VARIABLE V_TOTAL INTEGER; " +
		"BEGIN /* V_TOTLA */ V_TOTAL = 1; -- V_TOTLA\nEND"
	requireCodeCount(t, text, nil, codeUnknownVariable, 0)
}

// Fix round 1 (I1): renamed from TestDiagnosticUnknownLocalMalformedDeclarationNotFlagged,
// which asserted 1 finding despite its own name claiming nothing gets
// flagged -- misleading, since BROKEN and X are different names: this test
// only ever verified that an unrelated, genuinely-undeclared name (X) in
// the body still gets flagged despite an earlier, unrelated malformed
// declaration (BROKEN). See the two new tests below for the actual I1 bug
// (the SAME name that failed to declare must not be flagged later).
func TestDiagnosticUnknownLocalMalformedDeclarationDoesNotSuppressUnrelatedNames(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE BROKEN = 5; BEGIN X = 1; END",
		nil, codeUnknownVariable, 1)
}

// Fix round 1 (I1): a declaration missing its type never completes, so the
// name it would have declared must not be flagged as unknown later in the
// same procedure -- the malformed declaration is the reportable problem.
func TestDiagnosticUnknownLocalMalformedDeclarationSuppressesSameNameMissingType(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE X; BEGIN X = 1; END",
		nil, codeUnknownVariable, 0)
}

// Fix round 1 (I1): same, for a declaration missing its terminating ";".
func TestDiagnosticUnknownLocalMalformedDeclarationSuppressesSameNameMissingTerminator(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE X INTEGER BEGIN X = 1; END",
		nil, codeUnknownVariable, 0)
}

func TestDiagnosticUnknownLocalSuggestsNearestName(t *testing.T) {
	text := "CREATE PROCEDURE Q AS DECLARE VARIABLE V_TOTAL INTEGER; BEGIN V_TOTLA = 1; END"
	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	found := findByCode(a.Diagnostics(nil), codeUnknownVariable)
	if found == nil {
		t.Fatal("expected an unknown-variable finding")
	}
	if !strings.Contains(found.Message, "V_TOTAL") {
		t.Fatalf("message = %q, want a suggestion mentioning V_TOTAL", found.Message)
	}
}

func TestDiagnosticUnknownLocalSuggestionOmittedOnTie(t *testing.T) {
	text := "CREATE PROCEDURE Q AS DECLARE VARIABLE ABX INTEGER; DECLARE VARIABLE ABY INTEGER; BEGIN ABZ = 1; END"
	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	found := findByCode(a.Diagnostics(nil), codeUnknownVariable)
	if found == nil {
		t.Fatal("expected an unknown-variable finding")
	}
	if strings.Contains(found.Message, "did you mean") {
		t.Fatalf("message = %q, want no suggestion on a tie between equally-close candidates", found.Message)
	}
}

func TestDiagnosticUnknownLocalUnicodeSpanUsesByteOffsets(t *testing.T) {
	text := "/*\U0001F600*/ CREATE PROCEDURE Q AS DECLARE VARIABLE V_TOTAL INTEGER; BEGIN V_TOTLA = 1; END"
	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	wantStart := strings.Index(text, "V_TOTLA")
	found := findByCode(a.Diagnostics(nil), codeUnknownVariable)
	if found == nil {
		t.Fatal("expected an unknown-variable finding")
	}
	if found.Span.Start != wantStart || found.Span.End != wantStart+len("V_TOTLA") {
		t.Fatalf("span = %+v, want byte offsets [%d,%d)", found.Span, wantStart, wantStart+len("V_TOTLA"))
	}
}

// --- Fix round 1 (C1): reads on the right-hand side of an assignment, or
// inside an IF condition, that name a keyword, context value, type name, or
// database object other than a variable must never be flagged. Each of
// these is legal InterBase code the reviewer's probe found falsely flagged
// before this fix.

func TestDiagnosticUnknownLocalC1FalsePositives(t *testing.T) {
	tests := []struct {
		name, text string
	}{
		{"generator argument to GEN_ID", "CREATE PROCEDURE Q AS DECLARE VARIABLE V INTEGER; BEGIN V = GEN_ID(GEN_X, 1); END"},
		{"CURRENT_TIMESTAMP context value", "CREATE PROCEDURE Q AS DECLARE VARIABLE V TIMESTAMP; BEGIN V = CURRENT_TIMESTAMP; END"},
		{"USER context value", "CREATE PROCEDURE Q AS DECLARE VARIABLE V VARCHAR(20); BEGIN V = USER; END"},
		{"TRUE literal", "CREATE PROCEDURE Q AS DECLARE VARIABLE V BOOLEAN; BEGIN V = TRUE; END"},
		{"CAST type name", "CREATE PROCEDURE Q AS DECLARE VARIABLE V INTEGER; DECLARE VARIABLE S VARCHAR(10); BEGIN V = CAST(S AS INTEGER); END"},
		{"EXTRACT field keyword", "CREATE PROCEDURE Q AS DECLARE VARIABLE V INTEGER; DECLARE VARIABLE D DATE; BEGIN V = EXTRACT(YEAR FROM D); END"},
		{"BETWEEN operator", "CREATE PROCEDURE Q AS DECLARE VARIABLE V INTEGER; BEGIN IF (V BETWEEN 1 AND 2) THEN V = 1; END"},
		{"STARTING WITH operator", "CREATE PROCEDURE Q AS DECLARE VARIABLE V VARCHAR(10); BEGIN IF (V STARTING WITH 'a') THEN V = 'x'; END"},
		{"CONTAINING operator", "CREATE PROCEDURE Q AS DECLARE VARIABLE V VARCHAR(10); BEGIN IF (V CONTAINING 'a') THEN V = 'x'; END"},
		{"CURRENT_DATE context value", "CREATE PROCEDURE Q AS DECLARE VARIABLE D DATE; BEGIN IF (D < CURRENT_DATE) THEN D = CURRENT_DATE; END"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireCodeCount(t, tt.text, nil, codeUnknownVariable, 0)
		})
	}
}

// --- Fix round 1 (C1 regression coverage): scratch-verified false-positive
// risks from the original Task 3 round, now committed as permanent tests.

func TestDiagnosticUnknownLocalExceptionNameNotFlagged(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE V INTEGER; BEGIN WHEN EXCEPTION EX_SOMETHING DO V = 1; END",
		nil, codeUnknownVariable, 0)
}

func TestDiagnosticUnknownLocalLabelNotFlagged(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE V INTEGER; BEGIN <<LBL>> V = 1; LEAVE LBL; END",
		nil, codeUnknownVariable, 0)
}

func TestDiagnosticUnknownLocalProcedureCallNameNotFlagged(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE V INTEGER; BEGIN EXECUTE PROCEDURE SOMEPROC(V); END",
		nil, codeUnknownVariable, 0)
}

func TestDiagnosticUnknownLocalCursorNameNotFlagged(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE V INTEGER; BEGIN FOR SELECT ID FROM T INTO :V AS CURSOR C DO BEGIN V = V; END END",
		nil, codeUnknownVariable, 0)
}

// --- Triggers ---

func TestDiagnosticTriggerUnknownVariable(t *testing.T) {
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T BEFORE INSERT AS DECLARE VARIABLE V_TOTAL INTEGER; BEGIN V_TOTLA = 1; END",
		nil, codeUnknownVariable, 1)
}

func TestDiagnosticTriggerKnownLocalNotFlagged(t *testing.T) {
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T BEFORE INSERT AS DECLARE VARIABLE V_TOTAL INTEGER; BEGIN V_TOTAL = 1; END",
		nil, codeUnknownVariable, 0)
}

func TestDiagnosticTriggerDuplicateDeclaration(t *testing.T) {
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T BEFORE INSERT AS DECLARE VARIABLE DUP INTEGER; DECLARE VARIABLE DUP INTEGER; BEGIN END",
		nil, codeDuplicateDeclaration, 1)
}

func TestDiagnosticTriggerUnsupportedFormWithheldFromFindings(t *testing.T) {
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T INSTEAD OF INSERT AS BEGIN BADVAR = 1; END",
		nil, codeUnknownVariable, 0)
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T INSTEAD OF INSERT AS BEGIN BADVAR = 1; END",
		nil, codeDuplicateDeclaration, 0)
}

func TestDiagnosticTriggerOrEventListRecognized(t *testing.T) {
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T BEFORE INSERT OR UPDATE AS DECLARE VARIABLE V_TOTAL INTEGER; BEGIN V_TOTLA = 1; END",
		nil, codeUnknownVariable, 1)
}

// --- Fix round 1 (I2): NEW/OLD event-context coverage across all three
// trigger event kinds, plus lowercase new./old. binding.

func TestDiagnosticTriggerNewOldNotFlaggedAsUnknownBeforeUpdate(t *testing.T) {
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T BEFORE UPDATE AS BEGIN IF (NEW.ID = OLD.ID) THEN NEW.ID = OLD.ID; END",
		nil, codeUnknownVariable, 0)
}

func TestDiagnosticTriggerNewNotFlaggedAsUnknownBeforeInsert(t *testing.T) {
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T BEFORE INSERT AS BEGIN NEW.ID = 1; END",
		nil, codeUnknownVariable, 0)
}

func TestDiagnosticTriggerOldNotFlaggedAsUnknownAfterDelete(t *testing.T) {
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T AFTER DELETE AS DECLARE VARIABLE V INTEGER; BEGIN V = OLD.ID; END",
		nil, codeUnknownVariable, 0)
}

func TestDiagnosticTriggerLowercaseNewOldNotFlaggedAsUnknown(t *testing.T) {
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T BEFORE UPDATE AS BEGIN IF (new.id = old.id) THEN new.id = old.id; END",
		nil, codeUnknownVariable, 0)
}

// --- Fix round 1 (C2): embedded SQL inside a trigger body must never be
// scanned for unknown-variable purposes. Each of these is legal InterBase
// trigger code the reviewer's probe found falsely flagged before this fix.

func TestDiagnosticTriggerC2FalsePositives(t *testing.T) {
	tests := []struct {
		name, text string
	}{
		{
			"UPDATE SET and WHERE targets",
			"CREATE TRIGGER TRG1 FOR T BEFORE UPDATE AS BEGIN UPDATE T2 SET COL = 1 WHERE ID = NEW.ID; END",
		},
		{
			"SELECT WHERE predicate with INTO",
			"CREATE TRIGGER TRG1 FOR T BEFORE UPDATE AS DECLARE VARIABLE V INTEGER; BEGIN SELECT ID FROM T2 WHERE CODE = 1 INTO :V; END",
		},
		{
			"FOR SELECT cursor WHERE predicate",
			"CREATE TRIGGER TRG1 FOR T BEFORE UPDATE AS DECLARE VARIABLE V INTEGER; BEGIN FOR SELECT ID FROM T2 WHERE CODE = NEW.ID INTO :V DO BEGIN V = V; END END",
		},
		{
			"DELETE WHERE predicate",
			"CREATE TRIGGER TRG1 FOR T AFTER DELETE AS BEGIN DELETE FROM T2 WHERE PARENT_ID = OLD.ID; END",
		},
		{
			"IF EXISTS subquery relation and predicate",
			"CREATE TRIGGER TRG1 FOR T BEFORE UPDATE AS BEGIN IF (EXISTS (SELECT 1 FROM T2 WHERE CODE = NEW.ID)) THEN NEW.ID = NEW.ID; END",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireCodeCount(t, tt.text, nil, codeUnknownVariable, 0)
		})
	}
}

// --- Fix round 1 (I3): trigger INTO targets must be validated the same way
// procedure INTO targets already are.

func TestDiagnosticTriggerIntoTargetFlaggedWhenUndeclared(t *testing.T) {
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T BEFORE UPDATE AS BEGIN SELECT ID FROM T2 WHERE CODE = 1 INTO :MISSING; END",
		nil, codeUnknownVariable, 1)
}

func TestDiagnosticTriggerIntoTargetKnownNotFlagged(t *testing.T) {
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T BEFORE UPDATE AS DECLARE VARIABLE V INTEGER; BEGIN SELECT ID FROM T2 WHERE CODE = 1 INTO :V; END",
		nil, codeUnknownVariable, 0)
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T BEFORE UPDATE AS DECLARE VARIABLE V INTEGER; BEGIN SELECT ID FROM T2 WHERE CODE = 1 INTO V; END",
		nil, codeUnknownVariable, 0)
}

// --- Fix round 1 (I2): nearest-name suggestion tie-omission on the trigger
// path specifically.

func TestDiagnosticTriggerSuggestionOmittedOnTie(t *testing.T) {
	text := "CREATE TRIGGER TRG1 FOR T BEFORE INSERT AS DECLARE VARIABLE ABX INTEGER; DECLARE VARIABLE ABY INTEGER; BEGIN ABZ = 1; END"
	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	found := findByCode(a.Diagnostics(nil), codeUnknownVariable)
	if found == nil {
		t.Fatal("expected an unknown-variable finding")
	}
	if strings.Contains(found.Message, "did you mean") {
		t.Fatalf("message = %q, want no suggestion on a tie between equally-close candidates", found.Message)
	}
}

func findByCode(findings []Finding, code string) *Finding {
	for _, f := range findings {
		if f.Code == code {
			f := f
			return &f
		}
	}
	return nil
}
