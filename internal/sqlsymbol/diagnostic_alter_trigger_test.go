package sqlsymbol

import "testing"

var _ TriggerCatalog = (*triggerFixtureCatalog)(nil)

// triggerFixtureCatalog adds trigger relations to the complete fixture
// catalog, keyed by trigger Name.Key().
type triggerFixtureCatalog struct {
	*diagnosticFixtureCatalog
	triggers map[string]Name
}

func newTriggerFixtureCatalog(triggers map[string]Name) *triggerFixtureCatalog {
	return &triggerFixtureCatalog{diagnosticFixtureCatalog: newDiagnosticFixtureCatalog(), triggers: triggers}
}

func (c *triggerFixtureCatalog) TriggerRelation(name Name) (Name, bool) {
	relation, ok := c.triggers[name.Key()]
	return relation, ok
}

// Every supported ALTER TRIGGER header shape is recognized: its body's
// locals are checked with or without a catalog.
func TestDiagnosticAlterTriggerHeadersCheckLocals(t *testing.T) {
	for _, header := range []string{
		"ALTER TRIGGER TRG1 AS",
		"ALTER TRIGGER TRG1 ACTIVE AS",
		"ALTER TRIGGER TRG1 INACTIVE AS",
		"ALTER TRIGGER TRG1 BEFORE INSERT AS",
		"ALTER TRIGGER TRG1 AFTER UPDATE OR DELETE AS",
		"ALTER TRIGGER TRG1 ACTIVE BEFORE INSERT OR UPDATE OR DELETE POSITION 5 AS",
		"ALTER TRIGGER TRG1 INACTIVE POSITION 2 AS",
	} {
		t.Run(header, func(t *testing.T) {
			sql := header + " DECLARE VARIABLE V_TOTAL INTEGER; DECLARE VARIABLE DUP INTEGER; DECLARE VARIABLE DUP INTEGER; BEGIN V_TOTLA = 1; V_TOTAL = 2; END"
			for name, c := range map[string]Catalog{"no catalog": nil, "trigger catalog": newTriggerFixtureCatalog(map[string]Name{"TRG1": {Text: "T"}})} {
				t.Run(name, func(t *testing.T) {
					requireCodeCount(t, sql, c, codeUnknownVariable, 1)
					requireCodeCount(t, sql, c, codeDuplicateDeclaration, 1)
				})
			}
		})
	}
}

// Header shapes outside the supported grammar are withheld, never guessed at.
func TestDiagnosticAlterTriggerUnsupportedHeadersWithheld(t *testing.T) {
	for _, sql := range []string{
		"ALTER TRIGGER TRG1 INACTIVE;",
		"ALTER TRIGGER TRG1 BEFORE AS BEGIN BADVAR = 1; END",
		"ALTER TRIGGER TRG1 POSITION X AS BEGIN BADVAR = 1; END",
		"ALTER TRIGGER TRG1 FOR T BEFORE INSERT AS BEGIN BADVAR = 1; END",
		"ALTER TRIGGER AS BEGIN BADVAR = 1; END",
		"CREATE OR ALTER TRIGGER TRG1 FOR T BEFORE INSERT AS BEGIN BADVAR = 1; NEW.BOGUS = 1; END",
	} {
		t.Run(sql, func(t *testing.T) {
			requireCodeCount(t, sql, nil, codeUnknownVariable, 0)
			requireCodeCount(t, sql, newTriggerFixtureCatalog(map[string]Name{"TRG1": {Text: "T"}}), codeUnknownColumn, 0)
		})
	}
}

// NEW/OLD columns of an ALTER TRIGGER are checked only when the catalog
// names the trigger's relation; otherwise they are never reported.
func TestDiagnosticAlterTriggerRowColumnsNeedTriggerCatalog(t *testing.T) {
	sql := `ALTER TRIGGER TRG1 ACTIVE BEFORE UPDATE POSITION 1 AS
DECLARE VARIABLE X INTEGER;
BEGIN
  X = NEW.BOGUS;
  IF (OLD.NAEM = 'x') THEN NEW.V = X;
END`
	requireCodeCount(t, sql, nil, codeUnknownColumn, 0)
	requireCodeCount(t, sql, newDiagnosticFixtureCatalog(), codeUnknownColumn, 0)
	requireCodeCount(t, sql, newTriggerFixtureCatalog(map[string]Name{}), codeUnknownColumn, 0)
	requireCodeCount(t, sql, newTriggerFixtureCatalog(map[string]Name{"TRG1": {Text: "T"}}), codeUnknownColumn, 2)
	requireCodeCount(t, sql, newTriggerFixtureCatalog(map[string]Name{"TRG1": {Text: "NOSUCH"}}), codeUnknownColumn, 0)
}

func TestDiagnosticAlterTriggerNewFieldAssignmentNeedsTriggerCatalog(t *testing.T) {
	sql := "ALTER TRIGGER TRG1 AS BEGIN NEW.N = 99999; END;"
	requireCodeCount(t, sql, newDiagnosticFixtureCatalog(), codeInvalidAssignment, 0)
	requireCodeCount(t, sql, newTriggerFixtureCatalog(map[string]Name{"TRG1": {Text: "T"}}), codeInvalidAssignment, 1)
	requireCodeCount(t, "ALTER TRIGGER TRG1 AS BEGIN NEW.N = 100; END;", newTriggerFixtureCatalog(map[string]Name{"TRG1": {Text: "T"}}), codeInvalidAssignment, 0)
}

// An earlier DROP/CREATE of the trigger in the same document makes the
// catalog's relation stale, so its NEW/OLD columns are not checked.
func TestDiagnosticAlterTriggerCatalogRelationInvalidatedByEarlierDDL(t *testing.T) {
	c := newTriggerFixtureCatalog(map[string]Name{"TRG1": {Text: "T"}})
	body := " AS BEGIN NEW.BOGUS = 1; END"
	requireCodeCount(t, "ALTER TRIGGER TRG1"+body, c, codeUnknownColumn, 1)
	requireCodeCount(t, "DROP TRIGGER TRG1;\nALTER TRIGGER TRG1"+body, c, codeUnknownColumn, 0)
	requireCodeCount(t, "DROP TRIGGER TRG2;\nALTER TRIGGER TRG1"+body, c, codeUnknownColumn, 1)
}

// A CREATE TRIGGER keeps checking its own FOR relation, whatever the catalog
// says about a trigger of that name.
func TestDiagnosticCreateTriggerIgnoresTriggerCatalog(t *testing.T) {
	sql := "CREATE TRIGGER TRG1 FOR T BEFORE UPDATE AS BEGIN NEW.NAME = 'x'; END"
	requireCodeCount(t, sql, newTriggerFixtureCatalog(map[string]Name{"TRG1": {Text: "U"}}), codeUnknownColumn, 0)
}

func TestDiagnosticAlterTriggerFlow(t *testing.T) {
	for _, header := range []string{"CREATE TRIGGER TR_T FOR T BEFORE INSERT AS", "ALTER TRIGGER TR_T AS", "ALTER TRIGGER TR_T INACTIVE AFTER DELETE POSITION 3 AS"} {
		t.Run(header, func(t *testing.T) {
			source := header + " DECLARE VARIABLE V INTEGER; DECLARE VARIABLE W INTEGER; BEGIN W = V; V = 1; V = 2; NEW.ID = W + V; EXIT; W = 3; END"
			findings := flowDiagnostic(t, source, flowRulesOn)
			requireFlowCount(t, findings, "interbase-read-before-assignment", 1)
			requireFlowCount(t, findings, "interbase-dead-store", 1)
			requireFlowCount(t, findings, "interbase-unreachable", 1)
		})
	}
}

// Embedded SQL in an ALTER TRIGGER body is checked like a CREATE TRIGGER's.
func TestDiagnosticAlterTriggerEmbeddedSQLChecked(t *testing.T) {
	sql := `ALTER TRIGGER TR1 AS
DECLARE VARIABLE X INTEGER;
BEGIN
  SELECT NAEM FROM U INTO :X;
  DELETE FROM NOSUCH WHERE ID = :X;
END`
	requireCodeCount(t, sql, newDiagnosticFixtureCatalog(), codeUnknownColumn, 1)
	requireCodeCount(t, sql, newDiagnosticFixtureCatalog(), codeUnknownRelation, 1)
}
