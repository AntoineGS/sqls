package sqlsymbol

import "testing"

// --- brief's verbatim paired positive/negative fixtures ---

func TestDiagnosticSQLNames(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT NAEM FROM T;", c, "interbase-unknown-column", 1)
	requireCodeCount(t, "SELECT NAME FROM T;", c, "interbase-unknown-column", 0)
	requireCodeCount(t, "SELECT ID FROM T JOIN U ON T.ID=U.ID;", c, "interbase-ambiguous-column", 1)
	requireCodeCount(t, "SELECT T.ID FROM T JOIN U ON T.ID=U.ID;", c, "interbase-ambiguous-column", 0)
}

// --- unknown relation ---

func TestDiagnosticUnknownRelation(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT * FROM NOSUCH;", c, codeUnknownRelation, 1)
	requireCodeCount(t, "SELECT * FROM T;", c, codeUnknownRelation, 0)
	requireCodeCount(t, "UPDATE NOSUCH SET V = 1 WHERE ID = 1;", c, codeUnknownRelation, 1)
	requireCodeCount(t, "UPDATE T SET V = 1 WHERE ID = 1;", c, codeUnknownRelation, 0)
	requireCodeCount(t, "INSERT INTO NOSUCH (ID) VALUES (1);", c, codeUnknownRelation, 1)
	requireCodeCount(t, "INSERT INTO T (ID) VALUES (1);", c, codeUnknownRelation, 0)
	requireCodeCount(t, "DELETE FROM NOSUCH WHERE ID = 1;", c, codeUnknownRelation, 1)
	requireCodeCount(t, "DELETE FROM T WHERE ID = 1;", c, codeUnknownRelation, 0)
	requireCodeCount(t, "SELECT * FROM T JOIN NOSUCH ON T.ID = NOSUCH.ID;", c, codeUnknownRelation, 1)
}

// A known procedure name in a FROM position must never be reported as an
// unknown relation.
func TestDiagnosticUnknownRelationSelectableProcedure(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT * FROM P(1, 2);", c, codeUnknownRelation, 0)
}

// System catalog objects always report Unknown, never Missing: must never
// be flagged.
func TestDiagnosticUnknownRelationSystemName(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT * FROM RDB$RELATIONS;", c, codeUnknownRelation, 0)
}

// Loading/failing catalog categories: incomplete namespace means unknown
// relation cannot be proven, so no findings.
func TestDiagnosticUnknownRelationIncompleteCatalog(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	c.relationsKnown = false
	requireCodeCount(t, "SELECT * FROM NOSUCH;", c, codeUnknownRelation, 0)
	c2 := newDiagnosticFixtureCatalog()
	c2.proceduresKnown = false
	requireCodeCount(t, "SELECT * FROM NOSUCH;", c2, codeUnknownRelation, 0)
}

// A plain Catalog (not SemanticCatalog) cannot prove absence: no findings.
func TestDiagnosticUnknownRelationPlainCatalogWithheld(t *testing.T) {
	c := plainCatalog{}
	requireCodeCount(t, "SELECT * FROM NOSUCH;", c, codeUnknownRelation, 0)
}

type plainCatalog struct{}

func (plainCatalog) Columns(Name) ([]ColumnType, bool) { return nil, false }

// Script-local DDL: a table created earlier in the same buffer must not be
// flagged unknown, since DDLInvalidated makes later statements' knowledge of
// that name Unknown, not Missing, from that point on.
func TestDiagnosticUnknownRelationScriptLocalDDL(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "CREATE TABLE FRESH (ID INTEGER); SELECT * FROM FRESH;", c, codeUnknownRelation, 0)
}

// Quoted vs unquoted names are different identities per Name.Key().
func TestDiagnosticUnknownRelationQuotedCase(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	// The fixture's "T" key is uppercase; a quoted "t" is a distinct,
	// case-sensitive identity per Name.Key() and does not match it.
	requireCodeCount(t, `SELECT * FROM "t";`, c, codeUnknownRelation, 1)
	requireCodeCount(t, `SELECT * FROM "T";`, c, codeUnknownRelation, 0)
}

// Unsupported MERGE must remain silent: no attempted resolution inside it.
func TestDiagnosticUnknownRelationUnsupportedMerge(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "MERGE INTO NOSUCH USING T ON 1=1 WHEN NOT MATCHED THEN INSERT VALUES (1);", c, codeUnknownRelation, 0)
}

// Malformed SQL must not crash and must not produce speculative findings.
func TestDiagnosticUnknownRelationMalformedSQL(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT * FROM (((;", c, codeUnknownRelation, 0)
	requireCodeCount(t, "SELECT * FROM;", c, codeUnknownRelation, 0)
}

// --- unknown/ambiguous column: embedded + standalone parity ---

func TestDiagnosticColumnEmbeddedInProcedure(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	sql := `CREATE PROCEDURE PR1 AS
DECLARE VARIABLE X INTEGER;
BEGIN
  SELECT NAEM FROM T INTO :X;
END`
	requireCodeCount(t, sql, c, codeUnknownColumn, 1)
	ok := `CREATE PROCEDURE PR1 AS
DECLARE VARIABLE X INTEGER;
BEGIN
  SELECT NAME FROM T INTO :X;
END`
	requireCodeCount(t, ok, c, codeUnknownColumn, 0)
}

func TestDiagnosticColumnEmbeddedInTrigger(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	sql := `CREATE TRIGGER TR1 FOR T BEFORE INSERT AS
BEGIN
  IF (NEW.NAEM = 1) THEN
    EXIT;
END`
	requireCodeCount(t, sql, c, codeUnknownColumn, 1)
}

func TestDiagnosticColumnUpdateTarget(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "UPDATE T SET NAEM = 'x' WHERE ID = 1;", c, codeUnknownColumn, 1)
	requireCodeCount(t, "UPDATE T SET NAME = 'x' WHERE ID = 1;", c, codeUnknownColumn, 0)
}

func TestDiagnosticColumnInsertTarget(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "INSERT INTO T (NAEM) VALUES ('x');", c, codeUnknownColumn, 1)
	requireCodeCount(t, "INSERT INTO T (NAME) VALUES ('x');", c, codeUnknownColumn, 0)
}

func TestDiagnosticColumnDeletePredicate(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "DELETE FROM T WHERE NAEM = 'x';", c, codeUnknownColumn, 1)
	requireCodeCount(t, "DELETE FROM T WHERE NAME = 'x';", c, codeUnknownColumn, 0)
}

func TestDiagnosticColumnCTE(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "WITH C AS (SELECT ID, NAME FROM T) SELECT NAEM FROM C;", c, codeUnknownColumn, 1)
	requireCodeCount(t, "WITH C AS (SELECT ID, NAME FROM T) SELECT NAME FROM C;", c, codeUnknownColumn, 0)
}

func TestDiagnosticColumnDerivedTable(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT X.NAEM FROM (SELECT ID, NAME FROM T) X;", c, codeUnknownColumn, 1)
	requireCodeCount(t, "SELECT X.NAME FROM (SELECT ID, NAME FROM T) X;", c, codeUnknownColumn, 0)
}

func TestDiagnosticColumnSelectableProcedure(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT X.NOSUCH FROM P(1, 2) X;", c, codeUnknownColumn, 1)
	requireCodeCount(t, "SELECT X.OUT1 FROM P(1, 2) X;", c, codeUnknownColumn, 0)
}

func TestDiagnosticColumnCorrelatedSubquery(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	// U has no NAME column; the correlated reference to T.NAME (outer
	// query) must resolve fine via the scope chain, but a genuinely
	// unknown column inside the subquery must still be caught.
	requireCodeCount(t, "SELECT * FROM T WHERE EXISTS (SELECT * FROM U WHERE U.ID = T.ID AND U.NAEM = 1);", c, codeUnknownColumn, 1)
	requireCodeCount(t, "SELECT * FROM T WHERE EXISTS (SELECT * FROM U WHERE U.ID = T.ID);", c, codeUnknownColumn, 0)
}

func TestDiagnosticColumnUnionArms(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT NAEM FROM T UNION SELECT V FROM U;", c, codeUnknownColumn, 1)
	requireCodeCount(t, "SELECT ID FROM T UNION SELECT NAEM FROM U;", c, codeUnknownColumn, 1)
	requireCodeCount(t, "SELECT ID FROM T UNION SELECT ID FROM U;", c, codeUnknownColumn, 0)
}

// --- unknown qualifier ---

func TestDiagnosticUnknownQualifier(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT Z.ID FROM T;", c, codeUnknownQualifier, 1)
	requireCodeCount(t, "SELECT T.ID FROM T;", c, codeUnknownQualifier, 0)
}

// If FROM failed to parse cleanly (malformed statement), never claim a
// qualifier is unknown.
func TestDiagnosticUnknownQualifierMalformed(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT Z.ID FROM (((;", c, codeUnknownQualifier, 0)
}

// --- ambiguous column ---

func TestDiagnosticAmbiguousColumnRequiresAllKnown(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	c.relations["W"] = RelationFact{ColumnsKnown: false}
	requireCodeCount(t, "SELECT ID FROM T JOIN W ON T.ID = W.ID;", c, codeAmbiguousColumn, 0)
	requireCodeCount(t, "SELECT ID FROM T JOIN W ON T.ID = W.ID;", c, codeUnknownColumn, 0)
}

// JOIN USING merges the named column: must not be reported ambiguous.
func TestDiagnosticAmbiguousColumnJoinUsing(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT ID FROM T JOIN U USING (ID);", c, codeAmbiguousColumn, 0)
}

// NATURAL JOIN merges shared columns: must not be reported ambiguous.
func TestDiagnosticAmbiguousColumnNaturalJoin(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT ID FROM T NATURAL JOIN U;", c, codeAmbiguousColumn, 0)
}

// Duplicate aliases: two relations aliased the same name. Judgment call
// (documented in task-4-report.md): not its own diagnostic; qualifier
// resolution treats it as ambiguous and withholds rather than guessing.
func TestDiagnosticDuplicateAliasWithholds(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT X.ID FROM T X, U X;", c, codeUnknownColumn, 0)
	requireCodeCount(t, "SELECT X.ID FROM T X, U X;", c, codeUnknownQualifier, 0)
}

// --- select-list alias visibility per clause ---

func TestDiagnosticSelectListAliasNotVisibleInWhere(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	// ALIASED is not a real column of T and is not visible in WHERE per
	// InterBase's logical processing order (WHERE evaluates before the
	// SELECT list), so this must be flagged unknown.
	requireCodeCount(t, "SELECT NAME AS ALIASED FROM T WHERE ALIASED = 'x';", c, codeUnknownColumn, 1)
}

func TestDiagnosticSelectListAliasVisibleInGroupByHavingOrderBy(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT NAME AS ALIASED FROM T GROUP BY ALIASED;", c, codeUnknownColumn, 0)
	requireCodeCount(t, "SELECT NAME AS ALIASED FROM T GROUP BY ALIASED HAVING ALIASED = 'x';", c, codeUnknownColumn, 0)
	requireCodeCount(t, "SELECT NAME AS ALIASED FROM T ORDER BY ALIASED;", c, codeUnknownColumn, 0)
}

// --- repeat complete-metadata cases with incomplete metadata, to prove gating ---

func TestDiagnosticColumnWithheldWhenRelationColumnsUnknown(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	c.relations["T"] = RelationFact{ColumnsKnown: false}
	requireCodeCount(t, "SELECT NAEM FROM T;", c, codeUnknownColumn, 0)
}

// Job 3's "scope fully known" gate is about the FROM clause's own relation
// list being provably complete (a syntactic property TestDiagnosticUnknown
// QualifierMalformed already covers), not about catalog RelationInfo
// completeness: a qualifier that matches no relation in a cleanly parsed
// FROM clause is unknown regardless of whether the catalog itself has
// finished loading unrelated names.
func TestDiagnosticQualifierNotGatedByCatalogCompleteness(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	c.relationsKnown = false
	requireCodeCount(t, "SELECT NOSUCH.ID FROM T;", c, codeUnknownQualifier, 1)
}

// --- malformed SQL must not crash or speculate on columns ---

func TestDiagnosticColumnMalformedSQL(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT NAEM FROM (((;", c, codeUnknownColumn, 0)
	requireCodeCount(t, "SELECT FROM T WHERE;", c, codeUnknownColumn, 0)
}

// --- Fix round 1: C1(a) reserved words / clause syntax must never be
// flagged as unknown-column or unknown-relation ---

func TestDiagnosticFixRound1ReservedWordsNotFlaggedAsColumn(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	cases := []string{
		"SELECT DISTINCT NAME FROM T;",
		"SELECT FIRST 10 SKIP 5 NAME FROM T;",
		"SELECT NAME FROM T ORDER BY NAME DESC;",
		"SELECT NAME FROM T ORDER BY NAME DESCENDING NULLS LAST;",
		"SELECT NAME FROM T WHERE ID BETWEEN 1 AND 2;",
		"SELECT CURRENT_DATE FROM T;",
		"SELECT CURRENT_TIMESTAMP FROM T;",
		"SELECT CURRENT_USER FROM T;",
		"SELECT USER FROM T;",
		"SELECT GEN_ID(GEN_X, 1) FROM T;",
		"SELECT NEXT VALUE FOR GEN_X FROM T;",
		"SELECT CAST(ID AS VARCHAR(10) CHARACTER SET UTF8) FROM T;",
		"SELECT NAME COLLATE PXW_CSY FROM T;",
		"SELECT NAME FROM T WHERE NAME CONTAINING 'x';",
		"SELECT NAME FROM T WHERE NAME STARTING WITH 'x';",
		"SELECT NAME FROM T ROWS 1 TO 10;",
		"SELECT NAME FROM T PLAN (T NATURAL);",
	}
	for _, sql := range cases {
		requireCodeCount(t, sql, c, codeUnknownColumn, 0)
	}
}

func TestDiagnosticFixRound1ReservedWordsNotFlaggedInsideProcedure(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	sql := `CREATE PROCEDURE PR1 AS
DECLARE VARIABLE X INTEGER;
BEGIN
  SELECT DISTINCT NAME FROM T INTO :X;
END`
	requireCodeCount(t, sql, c, codeUnknownColumn, 0)
}

func TestDiagnosticFixRound1PlanClauseRelationNameNotFlaggedAsColumn(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT NAME FROM T PLAN (T NATURAL);", c, codeUnknownColumn, 0)
	requireCodeCount(t, "SELECT NAME FROM T PLAN JOIN (T NATURAL);", c, codeUnknownColumn, 0)
}

// --- Fix round 1: C1(b) FROM inside function-call syntax must not be
// mistaken for a query's own FROM clause ---

func TestDiagnosticFixRound1ExtractFromIsNotAQueryFromClause(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT EXTRACT(YEAR FROM ID) FROM T;", c, codeUnknownColumn, 0)
	requireCodeCount(t, "SELECT EXTRACT(YEAR FROM ID) FROM T;", c, codeUnknownRelation, 0)
}

func TestDiagnosticFixRound1TrimFromIsNotAQueryFromClause(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT TRIM(BOTH ' ' FROM NAME) FROM T;", c, codeUnknownColumn, 0)
	requireCodeCount(t, "SELECT TRIM(BOTH ' ' FROM NAME) FROM T;", c, codeUnknownRelation, 0)
}

func TestDiagnosticFixRound1UnknownRelationStillDetectedAlongsideExtract(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT EXTRACT(YEAR FROM ID) FROM NOSUCH;", c, codeUnknownRelation, 1)
}

// --- Fix round 1: I1 trigger UPDATE/DELETE bodies must be checked, not
// just SELECT/NEW/OLD ---

func TestDiagnosticFixRound1TriggerUpdateBodyChecked(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	sql := `CREATE TRIGGER TR1 FOR T BEFORE UPDATE AS
BEGIN
  UPDATE U SET NAEM = 1 WHERE ID = 1;
END`
	requireCodeCount(t, sql, c, codeUnknownColumn, 1)
}

func TestDiagnosticFixRound1TriggerDeleteBodyChecked(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	sql := `CREATE TRIGGER TR1 FOR T BEFORE DELETE AS
BEGIN
  DELETE FROM NOSUCH WHERE ID = 1;
END`
	requireCodeCount(t, sql, c, codeUnknownRelation, 1)
}

func TestDiagnosticFixRound1TriggerUpdateBodyKnownColumnNotFlagged(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	sql := `CREATE TRIGGER TR1 FOR T BEFORE UPDATE AS
BEGIN
  UPDATE U SET V = 1 WHERE ID = 1;
END`
	requireCodeCount(t, sql, c, codeUnknownColumn, 0)
}

// --- Fix round 1: I2 UNION alias visibility in a trailing ORDER BY must
// use the union's own merged output, not just the last arm's ---

func TestDiagnosticFixRound1UnionOrderByAliasFromEarlierArm(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT ID AS K FROM T UNION SELECT ID FROM U ORDER BY K;", c, codeUnknownColumn, 0)
}

// --- Fix round 1: I3 an unaliased FROM-clause procedure call is
// implicitly qualifiable by its own procedure name ---

func TestDiagnosticFixRound1UnaliasedProcedureCallQualifier(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	requireCodeCount(t, "SELECT P.OUT1 FROM P(1, 2);", c, codeUnknownQualifier, 0)
	requireCodeCount(t, "SELECT P.OUT1 FROM P(1, 2);", c, codeUnknownColumn, 0)
	requireCodeCount(t, "SELECT P.NOSUCH FROM P(1, 2);", c, codeUnknownColumn, 1)
}

// --- Fix round 1: I4 a malformed statement must not produce speculative
// missing-name errors, across all four finding types ---

func TestDiagnosticFixRound1MalformedProjectionSuppressesAllFindings(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	sql := "SELECT NAEM, FROM T WHERE;"
	requireCodeCount(t, sql, c, codeUnknownColumn, 0)
	requireCodeCount(t, sql, c, codeUnknownRelation, 0)
	requireCodeCount(t, sql, c, codeUnknownQualifier, 0)
	requireCodeCount(t, sql, c, codeAmbiguousColumn, 0)
}

// --- Fix round 1: I5 a bare name matching a declared local/parameter must
// withhold the unknown-column finding ---

func TestDiagnosticFixRound1BareNameMatchingLocalWithheld(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	sql := `CREATE PROCEDURE PR1 (ZZ INTEGER) AS
BEGIN
  SELECT ID FROM T WHERE ZZ = 1 INTO ZZ;
END`
	requireCodeCount(t, sql, c, codeUnknownColumn, 0)
}

// --- Fix round 1: I6 required coverage ---

func TestDiagnosticFixRound1UnknownRelationSuppressesColumnCascade(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	sql := "SELECT NOSUCH.ZZ, ZZ FROM NOSUCH;"
	requireCodeCount(t, sql, c, codeUnknownRelation, 1)
	requireCodeCount(t, sql, c, codeUnknownColumn, 0)
}

func TestDiagnosticFixRound1DDLAddedColumnNotFlaggedStale(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	sql := "ALTER TABLE T ADD NEWCOL INTEGER; SELECT NEWCOL FROM T;"
	requireCodeCount(t, sql, c, codeUnknownColumn, 0)
}

func TestDiagnosticFixRound1TriggerBodyBeyondNewOld(t *testing.T) {
	c := newDiagnosticFixtureCatalog()
	sql := `CREATE TRIGGER TR1 FOR T AFTER INSERT AS
DECLARE VARIABLE X INTEGER;
BEGIN
  SELECT NAEM FROM U INTO :X;
END`
	requireCodeCount(t, sql, c, codeUnknownColumn, 1)
}
