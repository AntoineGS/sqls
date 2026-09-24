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

func TestDiagnosticUnknownLocalRead(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE V_TOTAL INTEGER; BEGIN IF (V_TOTLA > 0) THEN V_TOTAL = 1; END",
		nil, codeUnknownVariable, 1)
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

func TestDiagnosticUnknownLocalMalformedDeclarationNotFlagged(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE Q AS DECLARE VARIABLE BROKEN = 5; BEGIN X = 1; END",
		nil, codeUnknownVariable, 1)
}

func TestDiagnosticUnknownLocalSuggestsNearestName(t *testing.T) {
	text := "CREATE PROCEDURE Q AS DECLARE VARIABLE V_TOTAL INTEGER; BEGIN V_TOTLA = 1; END"
	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	var found *Finding
	for _, f := range a.Diagnostics(nil) {
		if f.Code == codeUnknownVariable {
			f := f
			found = &f
		}
	}
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
	var found *Finding
	for _, f := range a.Diagnostics(nil) {
		if f.Code == codeUnknownVariable {
			f := f
			found = &f
		}
	}
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
	var found *Finding
	for _, f := range a.Diagnostics(nil) {
		if f.Code == codeUnknownVariable {
			f := f
			found = &f
		}
	}
	if found == nil {
		t.Fatal("expected an unknown-variable finding")
	}
	if found.Span.Start != wantStart || found.Span.End != wantStart+len("V_TOTLA") {
		t.Fatalf("span = %+v, want byte offsets [%d,%d)", found.Span, wantStart, wantStart+len("V_TOTLA"))
	}
}

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

func TestDiagnosticTriggerNewOldNotFlaggedAsUnknown(t *testing.T) {
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T BEFORE UPDATE AS BEGIN IF (NEW.ID = OLD.ID) THEN NEW.ID = OLD.ID; END",
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
