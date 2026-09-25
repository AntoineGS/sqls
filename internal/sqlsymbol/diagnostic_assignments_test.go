package sqlsymbol

import (
	"strings"
	"testing"
)

// --- brief's required literal fixtures: UPDATE, INSERT VALUES, procedure local ---

func TestDiagnosticInvalidAssignmentUpdateOutOfRangeLiteral(t *testing.T) {
	requireCodeCount(t, "UPDATE T SET N = 99999 WHERE ID = 1;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 1)
}

func TestDiagnosticInvalidAssignmentInsertValuesOutOfRangeLiteral(t *testing.T) {
	requireCodeCount(t, "INSERT INTO T (ID, N) VALUES (1, 99999);",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 1)
}

func TestDiagnosticInvalidAssignmentProcedureLocalOutOfRangeLiteral(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE X AS DECLARE VARIABLE V SMALLINT; BEGIN V = 99999; END;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 1)
}

// --- safe coercion must never fire ---

func TestDiagnosticInvalidAssignmentSafeCoercionDoesNotFire(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE X (A SMALLINT) AS DECLARE VARIABLE B INTEGER; BEGIN B = A; END;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)
	requireCodeCount(t, "UPDATE T SET V = 100 WHERE ID = 1;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)
}

// --- outcomePossibleLoss must never produce this code, anywhere ---

func TestDiagnosticInvalidAssignmentPossibleLossNeverFires(t *testing.T) {
	// INTEGER reference into a narrower SMALLINT destination is
	// outcomePossibleLoss (Task 11's concern), never outcomeDefinitelyInvalid.
	requireCodeCount(t,
		"CREATE PROCEDURE X (A INTEGER) AS DECLARE VARIABLE B SMALLINT; BEGIN B = A; END;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)
	// A literal that only requires rounding to fit the destination's scale
	// (but still lands in range) is outcomePossibleLoss, not invalid.
	requireCodeCount(t,
		"CREATE PROCEDURE X AS DECLARE VARIABLE V INTEGER; BEGIN V = 1.5; END;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)
}

// --- explicit CAST: valid stays silent, a cast that is valid on its own
// terms but overflows the OUTER destination fires once at the edge's own
// span (not duplicated), and a cast that is invalid on its OWN terms fires
// once at that same span with a CAST-local message, never evaluating the
// outer destination at all ---

func TestDiagnosticInvalidAssignmentExplicitCastValid(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE X AS DECLARE VARIABLE V INTEGER; BEGIN V = CAST(100 AS INTEGER); END;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)
}

// TestDiagnosticInvalidAssignmentValidCastFlowsToOuterCheck proves a CAST
// that is valid on its own terms (99999 fits INTEGER) still lets the outer
// assignment's own check run and fire, when the outer destination (a
// narrower SMALLINT local) cannot hold the CAST's resulting value -- and
// that this still produces exactly one finding, at the edge's own full
// source span, not a second one duplicated from the CAST itself.
func TestDiagnosticInvalidAssignmentValidCastFlowsToOuterCheck(t *testing.T) {
	text := "CREATE PROCEDURE X AS DECLARE VARIABLE V SMALLINT; BEGIN V = CAST(99999 AS INTEGER); END;"
	requireCodeCount(t, text, newDiagnosticFixtureCatalog(), codeInvalidAssignment, 1)

	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	var found *Finding
	for _, f := range a.Diagnostics(newDiagnosticFixtureCatalog()) {
		if f.Code == codeInvalidAssignment {
			f := f
			found = &f
		}
	}
	if found == nil {
		t.Fatal("expected exactly one interbase-invalid-assignment finding")
	}
	wantSpan := markerSpan(t, text, "CAST(99999 AS INTEGER)", 0)
	if found.Span != wantSpan {
		t.Fatalf("Span = %+v, want the edge's own full source span %+v (never the inner CAST span alone, never the outer local write span)", found.Span, wantSpan)
	}
}

// TestDiagnosticInvalidAssignmentInvalidCastReportedOnceAtCastSpan proves an
// invalid CAST (99999 overflows the CAST's own declared SMALLINT type) is
// reported once, at the edge's own span, with a message describing the
// CAST's own failure -- even though the OUTER destination (a wider INTEGER
// local, which would happily accept 99999) would otherwise stay silent.
// The outer destination check must never even run for this edge: reporting
// it too would be either evaluating a value the CAST never actually
// produces at runtime, or a redundant duplicate of the same finding.
func TestDiagnosticInvalidAssignmentInvalidCastReportedOnceAtCastSpan(t *testing.T) {
	text := "CREATE PROCEDURE X AS DECLARE VARIABLE V INTEGER; BEGIN V = CAST(99999 AS SMALLINT); END;"
	requireCodeCount(t, text, newDiagnosticFixtureCatalog(), codeInvalidAssignment, 1)

	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	var found *Finding
	for _, f := range a.Diagnostics(newDiagnosticFixtureCatalog()) {
		if f.Code == codeInvalidAssignment {
			f := f
			found = &f
		}
	}
	if found == nil {
		t.Fatal("expected exactly one interbase-invalid-assignment finding")
	}
	wantSpan := markerSpan(t, text, "CAST(99999 AS SMALLINT)", 0)
	if found.Span != wantSpan {
		t.Fatalf("Span = %+v, want the CAST's own span %+v, not the outer local write span", found.Span, wantSpan)
	}
	// The Reason cites the CAST's own declared range (SMALLINT's
	// [-32768, 32767]), never the outer INTEGER destination's much wider
	// range: a compatibility.Reason for an INTEGER destination would never
	// mention 32767.
	if !strings.Contains(found.Message, "32767") {
		t.Fatalf("message %q does not describe the CAST's own SMALLINT range, not the outer INTEGER destination", found.Message)
	}
}

// --- message must cite compatibility.Reason plus a destination label,
// never interpolate the full source SQL text ---

func TestDiagnosticInvalidAssignmentMessageOmitsFullSourceText(t *testing.T) {
	text := "CREATE PROCEDURE X AS DECLARE VARIABLE V SMALLINT; BEGIN V = CAST(99999 AS INTEGER); END;"
	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	var message string
	for _, f := range a.Diagnostics(newDiagnosticFixtureCatalog()) {
		if f.Code == codeInvalidAssignment {
			message = f.Message
		}
	}
	if message == "" {
		t.Fatal("expected an interbase-invalid-assignment finding with a message")
	}
	if !strings.Contains(message, "V") {
		t.Fatalf("message %q does not identify destination label V", message)
	}
	if strings.Contains(message, "CAST(99999 AS INTEGER)") {
		t.Fatalf("message %q must not interpolate the full source SQL text", message)
	}
}

// --- unknown/undomained destination type: withheld entirely ---

func TestDiagnosticInvalidAssignmentUnknownDestinationTypeWithheld(t *testing.T) {
	// Local variable typed by a domain name absent from a complete domain
	// namespace: parseDiagnosticType returns ok=false, so the edge is
	// skipped entirely -- no finding, not even a guessed one.
	requireCodeCount(t,
		"CREATE PROCEDURE X AS DECLARE VARIABLE V UNKNOWN_DOMAIN; BEGIN V = 99999; END;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)

	// A column whose rendered type is the catalog's own unrenderable
	// "TYPE(n)" fallback: same withholding rule, reached through
	// updateDestination/catalogColumnType instead of a local's own
	// DECLARE VARIABLE type.
	c := testCatalog{columns: map[string][]ColumnType{
		"Z": {{Name: "COL", Type: "TYPE(23)"}, {Name: "ID", Type: "INTEGER"}},
	}}
	requireCodeCount(t, "UPDATE Z SET COL = 99999 WHERE ID = 1;", c, codeInvalidAssignment, 0)
}

// --- qualified UPDATE target ---

func TestDiagnosticInvalidAssignmentQualifiedUpdateTarget(t *testing.T) {
	requireCodeCount(t, "UPDATE T SET T.N = 99999 WHERE T.ID = 1;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 1)
}

// --- INSERT SELECT with source from a different relation than the target ---

func TestDiagnosticInvalidAssignmentInsertSelectCrossRelation(t *testing.T) {
	requireCodeCount(t, "INSERT INTO T (N) SELECT 100 FROM U;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)
	requireCodeCount(t, "INSERT INTO T (N) SELECT 99999 FROM U;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 1)
}

// --- SELECT ... INTO (InterBase's INTO-before-FROM singleton-select form,
// the same order assignmentEdges' own selectIntoEdges recognizes) ---

func TestDiagnosticInvalidAssignmentSelectIntoOutOfRangeLiteral(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE X AS DECLARE VARIABLE V SMALLINT; BEGIN SELECT 99999 INTO :V FROM T; END;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 1)
	requireCodeCount(t,
		"CREATE PROCEDURE X AS DECLARE VARIABLE V SMALLINT; BEGIN SELECT 100 INTO :V FROM T; END;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)
}

// --- EXECUTE PROCEDURE input arguments ---

func TestDiagnosticInvalidAssignmentProcedureInputOutOfRangeLiteral(t *testing.T) {
	requireCodeCount(t, "EXECUTE PROCEDURE P(2147483648, 1);",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 1)
	requireCodeCount(t, "EXECUTE PROCEDURE P(1, 2);",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)
}

// --- EXECUTE PROCEDURE ... RETURNING_VALUES output targets: wired, but
// structurally unable to ever reach outcomeDefinitelyInvalid, since the
// procedure's declared OUTPUT type is always a type-only expressionFact (no
// Value) here -- assignmentCompatibility's literal-based invalid-range
// branch is only reachable when source.Value != nil. A range-narrowing
// RETURNING_VALUES pairing is therefore outcomePossibleLoss (or safe/
// unknown), never outcomeDefinitelyInvalid, under current Task 8 semantics.
func TestDiagnosticInvalidAssignmentReturningValuesNeverDefinitelyInvalid(t *testing.T) {
	requireCodeCount(t,
		"CREATE PROCEDURE X AS DECLARE VARIABLE V SMALLINT; BEGIN EXECUTE PROCEDURE P(1, 2) RETURNING_VALUES :V; END;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)
}

// --- trigger NEW.<col> = <expr> write-position assignments ---

func TestDiagnosticInvalidAssignmentTriggerNewFieldOutOfRangeLiteral(t *testing.T) {
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T BEFORE INSERT AS BEGIN NEW.N = 99999; END;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 1)
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T BEFORE INSERT AS BEGIN NEW.N = 100; END;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)
}

// OLD is always read-only: it is never a legal assignment target, so no
// trigger-body scan should ever treat "OLD.<col> = <expr>" as an edge.
func TestDiagnosticInvalidAssignmentTriggerOldNeverAnAssignmentTarget(t *testing.T) {
	requireCodeCount(t,
		"CREATE TRIGGER TRG1 FOR T BEFORE UPDATE AS BEGIN IF (OLD.N = 99999) THEN NEW.N = 1; END;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)
}

// --- stale-catalog withholding: a catalog-sourced destination (UPDATE/
// INSERT's relation-column target) must never be judged against metadata an
// earlier DDL statement in the same document has already invalidated ---

func TestDiagnosticInvalidAssignmentWithheldAfterAlterTableRedefinesColumn(t *testing.T) {
	// ALTER TABLE widens N to INTEGER; the pre-ALTER SMALLINT range must not
	// be used to judge the later UPDATE against it.
	requireCodeCount(t,
		"ALTER TABLE T ALTER COLUMN N TYPE INTEGER; UPDATE T SET N = 99999 WHERE ID = 1;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)
}

func TestDiagnosticInvalidAssignmentWithheldAfterDropCreateBeforeInsert(t *testing.T) {
	requireCodeCount(t,
		"DROP TABLE T; CREATE TABLE T (ID INTEGER, N INTEGER); INSERT INTO T (ID, N) VALUES (1, 99999);",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)
}

func TestDiagnosticInvalidAssignmentWithheldAfterDropCreateBeforeUpdate(t *testing.T) {
	requireCodeCount(t,
		"DROP TABLE T; CREATE TABLE T (ID INTEGER, N INTEGER); UPDATE T SET N = 99999;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)
}

// TestDiagnosticInvalidAssignmentDDLInvalidationDoesNotOverSuppress proves
// the withholding above is scoped to the actual invalidated relation: DDL
// against an unrelated table (U) must not suppress a genuine finding
// against T, and an UPDATE with no preceding DDL at all must still fire
// exactly as before.
func TestDiagnosticInvalidAssignmentDDLInvalidationDoesNotOverSuppress(t *testing.T) {
	requireCodeCount(t, "UPDATE T SET N = 99999 WHERE ID = 1;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 1)
	requireCodeCount(t,
		"ALTER TABLE U ALTER COLUMN V TYPE INTEGER; UPDATE T SET N = 99999 WHERE ID = 1;",
		newDiagnosticFixtureCatalog(), codeInvalidAssignment, 1)
}

// --- severity ---

func TestDiagnosticInvalidAssignmentSeverityIsError(t *testing.T) {
	text := "UPDATE T SET N = 99999 WHERE ID = 1;"
	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range a.Diagnostics(newDiagnosticFixtureCatalog()) {
		if f.Code == codeInvalidAssignment {
			if f.Severity != 1 {
				t.Fatalf("Severity = %d, want 1 (Error)", f.Severity)
			}
			return
		}
	}
	t.Fatal("expected an interbase-invalid-assignment finding")
}
