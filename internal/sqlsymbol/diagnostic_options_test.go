package sqlsymbol

import "testing"

func TestDiagnosticOptionsValidateAcceptsKnownCodesAndLevels(t *testing.T) {
	options := DiagnosticOptions{Rules: map[string]string{
		codeUnknownVariable: "error",
		codeNullComparison:  "warning",
		codeAmbiguousColumn: "default",
		codeUnused:          "off",
	}}
	if err := options.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for known codes/levels", err)
	}
}

func TestDiagnosticOptionsValidateRejectsUnknownCode(t *testing.T) {
	options := DiagnosticOptions{Rules: map[string]string{"interbase-not-a-real-code": "error"}}
	if err := options.Validate(); err == nil {
		t.Fatal("Validate() = nil, want an error for an unknown rule code")
	}
}

func TestDiagnosticOptionsValidateRejectsUnknownLevel(t *testing.T) {
	options := DiagnosticOptions{Rules: map[string]string{codeNullComparison: "critical"}}
	if err := options.Validate(); err == nil {
		t.Fatal("Validate() = nil, want an error for an unknown level")
	}
}

func TestDiagnosticOptionsValidateAcceptsEmptyRules(t *testing.T) {
	if err := (DiagnosticOptions{}).Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for empty options", err)
	}
}

// --- DiagnosticsWithOptions: off suppresses, everything else overrides severity ---

func TestDiagnosticsWithOptionsOffSuppressesFinding(t *testing.T) {
	requireCodeCountWithOptions(t, "SELECT ID FROM T WHERE V = NULL;", nil,
		DiagnosticOptions{Rules: map[string]string{codeNullComparison: "off"}}, codeNullComparison, 0)
}

func TestDiagnosticsWithOptionsDefaultKeepsRegistryLevel(t *testing.T) {
	a, err := AnalyzeDiagnostics("SELECT ID FROM T WHERE V = NULL;", interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	found := findByCode(a.DiagnosticsWithOptions(nil, DiagnosticOptions{}), codeNullComparison)
	if found == nil {
		t.Fatal("missing interbase-null-comparison finding")
	}
	if found.Severity != 2 {
		t.Fatalf("severity = %d, want 2 (warning, the registry default)", found.Severity)
	}
}

func TestDiagnosticsWithOptionsOverridesSeverity(t *testing.T) {
	a, err := AnalyzeDiagnostics("SELECT ID FROM T WHERE V = NULL;", interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	options := DiagnosticOptions{Rules: map[string]string{codeNullComparison: "error"}}
	found := findByCode(a.DiagnosticsWithOptions(nil, options), codeNullComparison)
	if found == nil {
		t.Fatal("missing interbase-null-comparison finding")
	}
	if found.Severity != 1 {
		t.Fatalf("severity = %d, want 1 (error, per rule override)", found.Severity)
	}
}

// --- DiagnosticsWithOptions must not compute an off rule, not merely filter it ---

type callCountingCatalog struct {
	*diagnosticFixtureCatalog
	columnsCalls    int
	uniqueKeysCalls int
}

func (c *callCountingCatalog) Columns(table Name) ([]ColumnType, bool) {
	c.columnsCalls++
	return c.diagnosticFixtureCatalog.Columns(table)
}

func (c *callCountingCatalog) UniqueKeys(table Name) ([][]string, bool) {
	c.uniqueKeysCalls++
	return c.diagnosticFixtureCatalog.UniqueKeys(table)
}

func TestDiagnosticsWithOptionsSkipsWidthAndSingletonComputationWhenOff(t *testing.T) {
	text := "CREATE PROCEDURE P AS BEGIN INSERT INTO T (NAME) SELECT NAME FROM U; SELECT ID FROM T WHERE V = 1; END"
	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	catalog := &callCountingCatalog{diagnosticFixtureCatalog: newDiagnosticFixtureCatalog()}
	options := DiagnosticOptions{Rules: map[string]string{
		codeStringTruncation: "off",
		codeSingletonSelect:  "off",
	}}
	a.DiagnosticsWithOptions(catalog, options)
	if catalog.columnsCalls != 0 {
		t.Errorf("Columns() called %d times, want 0 while interbase-string-truncation is off", catalog.columnsCalls)
	}
	if catalog.uniqueKeysCalls != 0 {
		t.Errorf("UniqueKeys() called %d times, want 0 while interbase-singleton-select is off", catalog.uniqueKeysCalls)
	}
}

// relationInfoCountingCatalog counts calls into RelationInfo, used by the
// name/shape rule group, to prove that group is skipped when every code it
// can produce is off.
type relationInfoCountingCatalog struct {
	*diagnosticFixtureCatalog
	relationInfoCalls int
}

func (c *relationInfoCountingCatalog) RelationInfo(table Name) (RelationFact, Knowledge) {
	c.relationInfoCalls++
	return c.diagnosticFixtureCatalog.RelationInfo(table)
}

func TestDiagnosticsWithOptionsSkipsNameAndShapeComputationWhenOff(t *testing.T) {
	text := "SELECT MISSING_COLUMN FROM T; INSERT INTO T VALUES (1, 2, 3, 4, 5);"
	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	catalog := &relationInfoCountingCatalog{diagnosticFixtureCatalog: newDiagnosticFixtureCatalog()}
	options := DiagnosticOptions{Rules: map[string]string{
		codeUnknownRelation:  "off",
		codeUnknownColumn:    "off",
		codeUnknownQualifier: "off",
		codeAmbiguousColumn:  "off",
		codeTargetCount:      "off",
		codeProcedureArity:   "off",
	}}
	got := a.DiagnosticsWithOptions(catalog, options)
	if catalog.relationInfoCalls != 0 {
		t.Errorf("RelationInfo() called %d times, want 0 while every name/shape code is off", catalog.relationInfoCalls)
	}
	if len(got) != 0 {
		t.Fatalf("findings = %+v, want none with every rule off", got)
	}
}

func TestDiagnosticsWithOptionsStillComputesNamesWhenOnlyOneGroupCodeIsOn(t *testing.T) {
	text := "SELECT MISSING_COLUMN FROM T;"
	catalog := newDiagnosticFixtureCatalog()
	options := DiagnosticOptions{Rules: map[string]string{
		codeUnknownRelation:  "off",
		codeUnknownQualifier: "off",
		codeAmbiguousColumn:  "off",
	}}
	requireCodeCountWithOptions(t, text, catalog, options, codeUnknownColumn, 1)
}

func TestDiagnosticsDefaultWrapperMatchesEmptyOptions(t *testing.T) {
	text := "SELECT ID FROM T WHERE V = NULL;"
	a, err := AnalyzeDiagnostics(text, interBaseVariant())
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Diagnostics(nil)) != len(a.DiagnosticsWithOptions(nil, DiagnosticOptions{})) {
		t.Fatal("Diagnostics(c) must behave exactly like DiagnosticsWithOptions(c, DiagnosticOptions{})")
	}
}
