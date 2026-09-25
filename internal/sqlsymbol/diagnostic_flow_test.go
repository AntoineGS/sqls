package sqlsymbol

import (
	"strings"
	"testing"
)

var flowRulesOn = DiagnosticOptions{Rules: map[string]string{
	"interbase-read-before-assignment": "warning",
	"interbase-output-not-assigned":    "warning",
	"interbase-dead-store":             "warning",
	"interbase-unreachable":            "warning",
}}

func flowDiagnostic(t *testing.T, source string, options DiagnosticOptions) []Finding {
	t.Helper()
	a, err := AnalyzeDiagnostics(source, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	return a.DiagnosticsWithOptions(nil, options)
}

func flowFindingsOf(findings []Finding, code string) []Finding {
	var out []Finding
	for _, finding := range findings {
		if finding.Code == code {
			out = append(out, finding)
		}
	}
	return out
}

func requireFlowCount(t *testing.T, findings []Finding, code string, want int) {
	t.Helper()
	if got := len(flowFindingsOf(findings, code)); got != want {
		t.Fatalf("%s findings = %d, want %d; all findings: %+v", code, got, want, findings)
	}
}

// Production change caught: marking a write before scanning the RHS would
// incorrectly suppress this finding. It also locks the required NULL wording.
func TestDiagnosticFlowReadBeforeAssignmentUsesInitialNullState(t *testing.T) {
	source := `CREATE PROCEDURE Q RETURNS (O INTEGER) AS
DECLARE VARIABLE V INTEGER;
BEGIN
  O = V;
  SUSPEND;
END`
	findings := flowDiagnostic(t, source, flowRulesOn)
	wantStart := strings.Index(source, "V;")
	wantStart = strings.Index(source[wantStart:], "V") + wantStart
	got := flowFindingsOf(findings, "interbase-read-before-assignment")
	if len(got) != 1 {
		t.Fatalf("read-before-assignment findings = %d, want 1: %+v", len(got), findings)
	}
	if got[0].Span != (Span{Start: wantStart, End: wantStart + 1}) {
		t.Fatalf("finding span = %+v, want V read at byte %d", got[0].Span, wantStart)
	}
	if !strings.Contains(strings.ToLower(got[0].Message), "null") || strings.Contains(strings.ToLower(got[0].Message), "uninitialized memory") {
		t.Fatalf("finding must describe a possible unintended initial NULL, not undefined memory: %q", got[0].Message)
	}
	requireFlowCount(t, findings, "interbase-output-not-assigned", 0)
}

// Production change caught: explicit NULL must establish assignment presence
// independently of the value's nullability.
func TestDiagnosticFlowExplicitNullAssignmentSuppressesReadBeforeAssignment(t *testing.T) {
	source := `CREATE PROCEDURE Q RETURNS (O INTEGER) AS
DECLARE VARIABLE V INTEGER;
BEGIN V = NULL; O = V; SUSPEND; END`
	requireFlowCount(t, flowDiagnostic(t, source, flowRulesOn), "interbase-read-before-assignment", 0)
}

// Production change caught: a recognized local declaration initializer starts
// with an explicitly assigned value, even though its value may be NULL.
func TestDiagnosticFlowVerifiedDeclarationDefaultStartsAssigned(t *testing.T) {
	for _, declaration := range []string{"DECLARE VARIABLE V INTEGER = 5;", "DECLARE VARIABLE V INTEGER DEFAULT 5;", "DECLARE VARIABLE V INTEGER DEFAULT NULL;"} {
		source := "CREATE PROCEDURE Q RETURNS (O INTEGER) AS " + declaration + " BEGIN O = V; SUSPEND; END"
		requireFlowCount(t, flowDiagnostic(t, source, flowRulesOn), "interbase-read-before-assignment", 0)
	}
}

// Production change caught: inputs are initialized on entry; output is
// warned if it reaches SUSPEND on the path that skips the one-arm IF.
func TestDiagnosticFlowOutputNotAssignedOnOneBranch(t *testing.T) {
	source := `CREATE PROCEDURE Q (FLAG INTEGER) RETURNS (O INTEGER) AS
BEGIN
  IF (FLAG = 1) THEN O = 1;
  SUSPEND;
END`
	findings := flowDiagnostic(t, source, flowRulesOn)
	requireFlowCount(t, findings, "interbase-read-before-assignment", 0)
	requireFlowCount(t, findings, "interbase-output-not-assigned", 1)
}

// Production change caught: branch join must intersect assignment facts, not
// union them, when both arms independently assign the output.
func TestDiagnosticFlowBothBranchesAssignOutput(t *testing.T) {
	source := `CREATE PROCEDURE Q (FLAG INTEGER) RETURNS (O INTEGER) AS
BEGIN IF (FLAG = 1) THEN O = 1; ELSE O = 2; SUSPEND; END`
	requireFlowCount(t, flowDiagnostic(t, source, flowRulesOn), "interbase-output-not-assigned", 0)
}

// Production change caught: a branch-local EXIT does not make later
// statements unreachable if another branch can continue to them.
func TestDiagnosticFlowConditionalExitLeavesOtherPathReachable(t *testing.T) {
	source := `CREATE PROCEDURE Q (FLAG INTEGER) RETURNS (O INTEGER) AS BEGIN IF (FLAG = 1) THEN EXIT; O = 2; SUSPEND; END`
	findings := flowDiagnostic(t, source, flowRulesOn)
	requireFlowCount(t, findings, "interbase-unreachable", 0)
	requireFlowCount(t, findings, "interbase-output-not-assigned", 1)
}

// Production change caught: first store is overwritten before observation;
// EXIT makes the next statement unreachable, while SUSPEND observes O=2.
func TestDiagnosticFlowDeadStoreAndUnreachableFixture(t *testing.T) {
	source := `CREATE PROCEDURE Q RETURNS (O INTEGER) AS
BEGIN
  O = 1;
  O = 2;
  SUSPEND;
  EXIT;
  O = 3;
END`
	findings := flowDiagnostic(t, source, flowRulesOn)
	requireFlowCount(t, findings, "interbase-dead-store", 1)
	requireFlowCount(t, findings, "interbase-unreachable", 1)
}

// Production change caught: output is observable at SUSPEND; a later write
// cannot make that already-emitted value dead.
func TestDiagnosticFlowSuspendObservesOutput(t *testing.T) {
	source := `CREATE PROCEDURE Q RETURNS (O INTEGER) AS
BEGIN O = 1; SUSPEND; O = 2; SUSPEND; END`
	requireFlowCount(t, flowDiagnostic(t, source, flowRulesOn), "interbase-dead-store", 0)
}

// Production change caught: procedure return observes outputs even without a
// SUSPEND, and retaining a previous output across SUSPEND is not NULL proof.
func TestDiagnosticFlowOutputEmissionAndContinuation(t *testing.T) {
	assigned := `CREATE PROCEDURE Q RETURNS (O INTEGER) AS BEGIN O = 1; END`
	requireFlowCount(t, flowDiagnostic(t, assigned, flowRulesOn), "interbase-output-not-assigned", 0)
	carried := `CREATE PROCEDURE Q (FLAG INTEGER) RETURNS (O INTEGER) AS
BEGIN O = 1; WHILE (FLAG = 1) DO BEGIN SUSPEND; EXIT; END SUSPEND; END`
	requireFlowCount(t, flowDiagnostic(t, carried, flowRulesOn), "interbase-output-not-assigned", 0)
}

// Production change caught: a zero-row SELECT INTO path must remain in the
// join, and a WHILE loop must retain its zero-iteration edge.
func TestDiagnosticFlowZeroRowSelectIntoAndZeroIterationLoop(t *testing.T) {
	for name, source := range map[string]string{
		"select-into": `CREATE PROCEDURE Q RETURNS (O INTEGER) AS BEGIN SELECT ID FROM T WHERE 1 = 0 INTO :O; SUSPEND; END`,
		"while":       `CREATE PROCEDURE Q (FLAG INTEGER) RETURNS (O INTEGER) AS BEGIN WHILE (FLAG = 1) DO O = 1; SUSPEND; END`,
		"for-select":  `CREATE PROCEDURE Q RETURNS (O INTEGER) AS BEGIN FOR SELECT ID FROM T INTO :O DO BEGIN O = ID; END SUSPEND; END`,
	} {
		t.Run(name, func(t *testing.T) {
			requireFlowCount(t, flowDiagnostic(t, source, flowRulesOn), "interbase-output-not-assigned", 1)
		})
	}
}

// Production change caught: flow conclusions must fail closed after an
// unsupported effect or an exception handler whose paths are not modeled.
func TestDiagnosticFlowUnsupportedEffectsAndExceptionRecoveryWithholdFacts(t *testing.T) {
	for name, source := range map[string]string{
		"merge":   `CREATE PROCEDURE Q RETURNS (O INTEGER) AS BEGIN MERGE INTO T USING U ON T.ID = U.ID WHEN MATCHED THEN UPDATE SET O = 2; SUSPEND; END`,
		"handler": `CREATE PROCEDURE Q RETURNS (O INTEGER) AS BEGIN WHEN ANY DO BEGIN O = 2; END SUSPEND; END`,
	} {
		t.Run(name, func(t *testing.T) {
			requireFlowCount(t, flowDiagnostic(t, source, flowRulesOn), "interbase-output-not-assigned", 0)
		})
	}
	independent := `CREATE PROCEDURE Q RETURNS (O INTEGER) AS DECLARE VARIABLE V INTEGER; BEGIN O = V; MERGE INTO T USING U ON T.ID = U.ID WHEN MATCHED THEN UPDATE SET O = 2; SUSPEND; END`
	findings := flowDiagnostic(t, independent, flowRulesOn)
	requireFlowCount(t, findings, "interbase-read-before-assignment", 1)
	requireFlowCount(t, findings, "interbase-output-not-assigned", 0)
}

// Production change caught: malformed/incomplete statements do not establish
// writes or support flow conclusions in the affected region.
func TestDiagnosticFlowIncompleteAssignmentWithholdsFacts(t *testing.T) {
	source := `CREATE PROCEDURE Q RETURNS (O INTEGER) AS BEGIN O = 1; O = ; SUSPEND; END`
	findings := flowDiagnostic(t, source, flowRulesOn)
	requireFlowCount(t, findings, "interbase-output-not-assigned", 0)
	requireFlowCount(t, findings, "interbase-dead-store", 0)
	incompleteSelect := `CREATE PROCEDURE Q RETURNS (O INTEGER) AS BEGIN SELECT ID INTO :O FROM; SUSPEND; END`
	requireFlowCount(t, flowDiagnostic(t, incompleteSelect, flowRulesOn), "interbase-output-not-assigned", 0)
	incompleteTerminator := `CREATE PROCEDURE Q RETURNS (O INTEGER) AS BEGIN O = 1; O = 2 END`
	findings = flowDiagnostic(t, incompleteTerminator, flowRulesOn)
	requireFlowCount(t, findings, "interbase-output-not-assigned", 0)
	requireFlowCount(t, findings, "interbase-dead-store", 0)
}

// Production change caught: a condition that invokes an unmodeled function
// cannot leave pre-call assignment facts definite.
func TestDiagnosticFlowUnknownConditionEffectsInvalidateFacts(t *testing.T) {
	source := `CREATE PROCEDURE Q RETURNS (O INTEGER) AS BEGIN IF (CHECK_STATE()) THEN O = 1; SUSPEND; END`
	requireFlowCount(t, flowDiagnostic(t, source, flowRulesOn), "interbase-output-not-assigned", 0)
}

// Production change caught: trigger event-context OLD/NEW values are inputs,
// not local values whose first read might be an unintended initial NULL.
func TestDiagnosticFlowTriggerContextStartsInitialized(t *testing.T) {
	source := `CREATE TRIGGER TR_T FOR T BEFORE UPDATE AS
DECLARE VARIABLE V INTEGER;
BEGIN V = NEW.ID; NEW.V = V; END`
	requireFlowCount(t, flowDiagnostic(t, source, flowRulesOn), "interbase-read-before-assignment", 0)
}

// Production change caught: trigger locals participate in flow, while NEW/OLD
// event-context columns remain initialized external values rather than locals.
func TestDiagnosticFlowTriggerLocalReadAndNewContext(t *testing.T) {
	source := `CREATE TRIGGER TR_T FOR T BEFORE UPDATE AS
DECLARE VARIABLE V INTEGER;
BEGIN
  IF (NEW.ID = 1) THEN V = 1;
  V = V + 1;
END`
	findings := flowDiagnostic(t, source, flowRulesOn)
	got := flowFindingsOf(findings, "interbase-read-before-assignment")
	if len(got) != 1 {
		t.Fatalf("trigger read-before-assignment findings = %d, want 1 for V on the skipped IF path: %+v", len(got), findings)
	}
	if got[0].Span.Start != strings.LastIndex(source, "V = V")+4 {
		t.Fatalf("trigger finding span = %+v, want read of V in V = V + 1", got[0].Span)
	}
}

// Production change caught: a loop back-edge must not make an output store
// before SUSPEND look dead merely because a later iteration can overwrite it.
func TestDiagnosticFlowLoopCarriedOutputIsObservable(t *testing.T) {
	source := `CREATE PROCEDURE Q (FLAG INTEGER) RETURNS (O INTEGER) AS
BEGIN O = 1; WHILE (FLAG = 1) DO BEGIN SUSPEND; O = O + 1; END SUSPEND; END`
	findings := flowDiagnostic(t, source, flowRulesOn)
	requireFlowCount(t, findings, "interbase-dead-store", 0)
	requireFlowCount(t, findings, "interbase-output-not-assigned", 0)
	carriedStore := `CREATE PROCEDURE Q (FLAG INTEGER) RETURNS (O INTEGER) AS BEGIN O = 1; WHILE (FLAG = 1) DO O = O + 1; SUSPEND; END`
	requireFlowCount(t, flowDiagnostic(t, carriedStore, flowRulesOn), "interbase-dead-store", 0)
}

// Production change caught: an unfinished CFG/worklist must be abandoned
// within the documented statement budget, not publish partial conclusions.
func TestDiagnosticFlowBudgetWithholdsUnfinishedProcedure(t *testing.T) {
	var source strings.Builder
	source.WriteString("CREATE PROCEDURE Q RETURNS (O INTEGER) AS DECLARE VARIABLE V INTEGER; BEGIN O = V;")
	for i := 0; i < 6000; i++ {
		source.WriteString(" V = 1;")
	}
	source.WriteString(" SUSPEND; END")
	requireFlowCount(t, flowDiagnostic(t, source.String(), flowRulesOn), "interbase-read-before-assignment", 0)
}

// Production change caught: exhausting the shared analysis budget must not
// retract findings from an earlier complete procedure or publish partial
// findings from the unfinished procedure.
func TestDiagnosticFlowBudgetRetainsEarlierCompleteProcedure(t *testing.T) {
	var source strings.Builder
	source.WriteString(`CREATE PROCEDURE FIRST_P RETURNS (O INTEGER) AS DECLARE VARIABLE V INTEGER; BEGIN O = V; SUSPEND; END;`)
	source.WriteString(`CREATE PROCEDURE SECOND_P RETURNS (O INTEGER) AS DECLARE VARIABLE V INTEGER; BEGIN O = V;`)
	for i := 0; i < 6000; i++ {
		source.WriteString(" V = 1;")
	}
	source.WriteString(" SUSPEND; END")
	requireFlowCount(t, flowDiagnostic(t, source.String(), flowRulesOn), "interbase-read-before-assignment", 1)
}

// Production change caught: absent policy and explicit default keep all flow
// diagnostics off, and all-off policy must not construct the CFG.
func TestDiagnosticFlowRulesDefaultOff(t *testing.T) {
	source := `CREATE PROCEDURE Q RETURNS (O INTEGER) AS DECLARE VARIABLE V INTEGER; BEGIN O = V; SUSPEND; END`
	for _, options := range []DiagnosticOptions{{}, {Rules: map[string]string{
		"interbase-read-before-assignment": "default", "interbase-output-not-assigned": "default",
		"interbase-dead-store": "default", "interbase-unreachable": "default",
	}}} {
		findings := flowDiagnostic(t, source, options)
		for _, code := range []string{"interbase-read-before-assignment", "interbase-output-not-assigned", "interbase-dead-store", "interbase-unreachable"} {
			requireFlowCount(t, findings, code, 0)
		}
	}
}

// Production change caught: every flow code must participate in registry
// validation and have a stable severity/default-off entry.
func TestDiagnosticFlowRuleRegistryAndSeverityOverrides(t *testing.T) {
	codes := []string{"interbase-read-before-assignment", "interbase-output-not-assigned", "interbase-dead-store", "interbase-unreachable"}
	for _, code := range codes {
		if _, ok := diagnosticRegistry[code]; !ok {
			t.Errorf("flow rule %q missing from diagnostic registry", code)
		}
		if !diagnosticDefaultOff[code] {
			t.Errorf("flow rule %q must default off", code)
		}
	}
	options := DiagnosticOptions{Rules: map[string]string{"interbase-read-before-assignment": "error"}}
	findings := flowDiagnostic(t, `CREATE PROCEDURE Q RETURNS (O INTEGER) AS DECLARE VARIABLE V INTEGER; BEGIN O = V; SUSPEND; END`, options)
	if got := flowFindingsOf(findings, "interbase-read-before-assignment"); len(got) != 1 || got[0].Severity != 1 {
		t.Fatalf("explicit error override findings = %+v, want one severity-1 finding", got)
	}
}
