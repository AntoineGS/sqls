package sqlsymbol

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

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

// TestDiagnosticsDefaultWrapperMatchesEmptyOptions compares full finding
// slices (every field, including Severity) rather than merely their
// lengths: a length-only comparison would not catch a bug that strips or
// mutates severity while leaving the finding count unchanged.
func TestDiagnosticsDefaultWrapperMatchesEmptyOptions(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		catalog Catalog
	}{
		{name: "null comparison only, no catalog", text: "SELECT ID FROM T WHERE V = NULL;"},
		{
			name:    "mixed rule families with a fixture catalog",
			text:    "CREATE PROCEDURE P AS DECLARE VARIABLE UNUSED VARCHAR(10); BEGIN SELECT MISSING_COLUMN FROM T WHERE V = NULL; END",
			catalog: newDiagnosticFixtureCatalog(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := AnalyzeDiagnostics(tt.text, interBaseVariant())
			if err != nil {
				t.Fatal(err)
			}
			want := a.Diagnostics(tt.catalog)
			got := a.DiagnosticsWithOptions(tt.catalog, DiagnosticOptions{})
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("Diagnostics(c) must behave exactly like DiagnosticsWithOptions(c, DiagnosticOptions{}) (-want +got):\n%s", diff)
			}
		})
	}
}

// --- severity() falls back to the finding's own severity rather than ever
// producing a blank/zero severity, whether because a code is not (yet)
// registered (future-rule registry drift) or because a level string somehow
// bypassed Validate ---

func TestDiagnosticOptionsSeverityFallsBackForUnregisteredCode(t *testing.T) {
	options := DiagnosticOptions{}
	if got := options.severity("interbase-not-registered-yet", 3); got != 3 {
		t.Fatalf("severity() = %d, want the finding's own fallback severity 3 for an unregistered code", got)
	}
}

func TestDiagnosticOptionsSeverityFallsBackForUnrecognizedLevel(t *testing.T) {
	// A level that Validate would reject (wrong case), modelling a policy
	// that somehow bypassed validation: severity must still never come back
	// blank/zero.
	options := DiagnosticOptions{Rules: map[string]string{codeNullComparison: "Error"}}
	if got := options.severity(codeNullComparison, 4); got != 4 {
		t.Fatalf("severity() = %d, want the finding's own fallback severity 4 for an unrecognized level", got)
	}
}

// TestDiagnosticRegistryCoversEveryCurrentCode enumerates every finding code
// any rule in the current codebase can actually emit and asserts each is
// present in diagnosticRegistry, catching registry drift as soon as a new
// code is introduced without also being registered.
func TestDiagnosticRegistryCoversEveryCurrentCode(t *testing.T) {
	codes := []string{
		codeUnused, codeStringTruncation, codeSingletonSelect, codeNullComparison,
		codeUnknownVariable, codeDuplicateDeclaration,
		codeUnknownRelation, codeUnknownColumn, codeUnknownQualifier, codeAmbiguousColumn,
		codeTargetCount, codeProcedureArity,
	}
	for _, code := range codes {
		if _, ok := diagnosticRegistry[code]; !ok {
			t.Errorf("diagnosticRegistry is missing code %q", code)
		}
	}
	if len(diagnosticRegistry) != len(codes) {
		t.Errorf("diagnosticRegistry has %d entries, want exactly %d known codes (found an extra or stale entry)", len(diagnosticRegistry), len(codes))
	}
}
